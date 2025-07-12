// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"golang.org/x/sync/errgroup"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	awsiamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	awsssmv1beta1 "github.com/upbound/provider-aws/apis/ssm/v1beta1"
	gcpcloudplatformv1beta1 "github.com/upbound/provider-gcp/apis/cloudplatform/v1beta1"
	gcpcomputev1beta1 "github.com/upbound/provider-gcp/apis/compute/v1beta1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	gcpsecretmanagerv1beta1 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta1"
	gcpsecretmanagerv1beta2 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta2"
	gcpv1beta1 "github.com/upbound/provider-gcp/apis/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcsingle "sigs.k8s.io/multicluster-runtime/providers/single"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller"
	"go.datum.net/infra-provider-gcp/internal/providers"
	mcdatum "go.datum.net/infra-provider-gcp/internal/providers/datum"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))

	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))

	utilruntime.Must(awsec2v1beta1.AddToScheme(scheme))
	utilruntime.Must(awsiamv1beta1.AddToScheme(scheme))
	utilruntime.Must(awsssmv1beta1.AddToScheme(scheme))

	utilruntime.Must(gcpv1beta1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(gcpcomputev1beta1.AddToScheme(scheme))
	utilruntime.Must(gcpcomputev1beta2.AddToScheme(scheme))
	utilruntime.Must(gcpcloudplatformv1beta1.AddToScheme(scheme))
	utilruntime.Must(gcpsecretmanagerv1beta1.AddToScheme(scheme))
	utilruntime.Must(gcpsecretmanagerv1beta2.AddToScheme(scheme))

	utilruntime.Must(infrav1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var serverConfigFile string

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")

	opts := zap.Options{
		Development: true,
	}

	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")

	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var serverConfig config.GCPProvider
	var configData []byte
	if len(serverConfigFile) > 0 {
		var err error
		configData, err = os.ReadFile(serverConfigFile)
		if err != nil {
			setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
			os.Exit(1)
		}
	}

	if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
		setupLog.Error(err, "unable to decode server config")
		os.Exit(1)
	}

	setupLog.Info("server config", "config", serverConfig)

	cfg := ctrl.GetConfigOrDie()

	deploymentCluster, err := cluster.New(cfg, func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		setupLog.Error(err, "failed creating local cluster")
		os.Exit(1)
	}

	runnables, provider, err := initializeClusterDiscovery(serverConfig, deploymentCluster, scheme)
	if err != nil {
		setupLog.Error(err, "unable to initialize cluster discovery")
		os.Exit(1)
	}

	setupLog.Info("cluster discovery mode", "mode", serverConfig.Discovery.Mode)

	ctx := ctrl.SetupSignalHandler()

	deploymentClusterClient := deploymentCluster.GetClient()

	metricsServerOptions := serverConfig.MetricsServer.Options(ctx, deploymentClusterClient)

	webhookServer := webhook.NewServer(
		serverConfig.WebhookServer.Options(ctx, deploymentClusterClient),
	)

	mgr, err := mcmanager.New(cfg, provider, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsServerOptions,
		WebhookServer:           webhookServer,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "fddf20f1.datumapis.com",
		LeaderElectionNamespace: leaderElectionNamespace,

		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	downstreamRestConfig, err := serverConfig.DownstreamResourceManagement.RestConfig()
	if err != nil {
		setupLog.Error(err, "unable to load control plane kubeconfig")
		os.Exit(1)
	}

	downstreamCluster, err := cluster.New(downstreamRestConfig, func(o *cluster.Options) {
		o.Scheme = scheme
		o.Cache = cache.Options{
			DefaultLabelSelector: labels.SelectorFromSet(serverConfig.DownstreamResourceManagement.ManagedResourceLabels),
		}
	})
	if err != nil {
		setupLog.Error(err, "failed to construct cluster")
		os.Exit(1)
	}

	if err = (&controller.ClusterDownstreamWorkloadReconciler{
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
		Config:            &serverConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterDownstreamWorkloadReconciler")
		os.Exit(1)
	}

	if err = (&controller.WorkloadDeploymentReconciler{
		Config:            serverConfig,
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkloadDeploymentReconciler")
		os.Exit(1)
	}

	if err = (&controller.InstanceReconciler{
		Config:            serverConfig,
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InstanceReconciler")
		os.Exit(1)
	}

	if err = (&controller.NetworkReconciler{
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkReconciler")
		os.Exit(1)
	}

	if err = (&controller.NetworkContextReconciler{
		Config:            serverConfig,
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkContextReconciler")
		os.Exit(1)
	}

	if err = (&controller.SubnetReconciler{
		Config:            serverConfig,
		LocationClassName: serverConfig.LocationClassName,
		DownstreamCluster: downstreamCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SubnetReconciler")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := controller.AddClusterDownstreamWorkloadIndexers(ctx, downstreamCluster.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "unable to add indexers to downstream cluster")
		os.Exit(1)
	}

	if err = controller.AddIndexers(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to add indexers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, runnable := range runnables {
		g.Go(func() error {
			return ignoreCanceled(runnable.Start(ctx))
		})
	}

	setupLog.Info("starting cluster discovery provider")
	g.Go(func() error {
		return ignoreCanceled(provider.Run(ctx, mgr))
	})

	g.Go(func() error {
		return ignoreCanceled(downstreamCluster.Start(ctx))
	})

	setupLog.Info("starting multicluster manager")
	g.Go(func() error {
		return ignoreCanceled(mgr.Start(ctx))
	})

	if err := g.Wait(); err != nil {
		setupLog.Error(err, "unable to start")
		os.Exit(1)
	}
}

type runnableProvider interface {
	multicluster.Provider
	Run(context.Context, mcmanager.Manager) error
}

// Needed until we contribute the patch in the following PR again (need to sign CLA):
//
//	See: https://github.com/kubernetes-sigs/multicluster-runtime/pull/18
type wrappedSingleClusterProvider struct {
	multicluster.Provider
	cluster cluster.Cluster
}

func (p *wrappedSingleClusterProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	if err := mgr.Engage(ctx, "single", p.cluster); err != nil {
		return err
	}
	return p.Provider.(runnableProvider).Run(ctx, mgr)
}

func initializeClusterDiscovery(
	serverConfig config.GCPProvider,
	deploymentCluster cluster.Cluster,
	scheme *runtime.Scheme,
) (runnables []manager.Runnable, provider runnableProvider, err error) {
	runnables = append(runnables, deploymentCluster)
	switch serverConfig.Discovery.Mode {
	case providers.ProviderSingle:
		provider = &wrappedSingleClusterProvider{
			Provider: mcsingle.New("single", deploymentCluster),
			cluster:  deploymentCluster,
		}

	case providers.ProviderDatum:
		discoveryRestConfig, err := serverConfig.Discovery.DiscoveryRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get discovery rest config: %w", err)
		}

		projectRestConfig, err := serverConfig.Discovery.ProjectRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get project rest config: %w", err)
		}

		discoveryManager, err := manager.New(discoveryRestConfig, manager.Options{
			Client: client.Options{
				Cache: &client.CacheOptions{
					Unstructured: true,
				},
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to set up overall controller manager: %w", err)
		}

		provider, err = mcdatum.New(discoveryManager, mcdatum.Options{
			ClusterOptions: []cluster.Option{
				func(o *cluster.Options) {
					o.Scheme = scheme
				},
			},
			InternalServiceDiscovery: serverConfig.Discovery.InternalServiceDiscovery,
			ProjectRestConfig:        projectRestConfig,
			LabelSelector:            serverConfig.Discovery.LabelSelector,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create datum project provider: %w", err)
		}

		runnables = append(runnables, discoveryManager)

	// case providers.ProviderKind:
	// 	provider = mckind.New(mckind.Options{
	// 		ClusterOptions: []cluster.Option{
	// 			func(o *cluster.Options) {
	// 				o.Scheme = scheme
	// 			},
	// 		},
	// 	})

	default:
		return nil, nil, fmt.Errorf(
			"unsupported cluster discovery mode %s",
			serverConfig.Discovery.Mode,
		)
	}

	return runnables, provider, nil
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
