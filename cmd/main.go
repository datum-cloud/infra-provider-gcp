// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"

	kcccomputev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/compute/v1beta1"
	kcciamv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/iam/v1beta1"
	kccsecretmanagerv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/secretmanager/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.datum.net/infra-provider-gcp/internal/controller"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))

	utilruntime.Must(kcccomputev1beta1.AddToScheme(scheme))
	utilruntime.Must(kcciamv1beta1.AddToScheme(scheme))
	utilruntime.Must(kcciamv1beta1.AddToScheme(scheme))
	utilruntime.Must(kccsecretmanagerv1beta1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var upstreamKubeconfig string
	var locationClassName string
	var infraNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// TODO(jreese) move to an approach that leverages a CRD to configure which
	// locations this deployment of infra-provider-gcp will consider. Should
	// include things like label or field selectors, anti-affinities for resources,
	// etc. When this is done, we should investigate leveraging the `ByObject`
	// setting of the informer cache to prevent populating the cache with entities
	// which the operator does not need to receive. We'll likely need to lean
	// into well known labels here, since a location class is defined on a location,
	// which entities only reference and do not embed.
	flag.StringVar(
		&locationClassName,
		"location-class",
		"self-managed",
		"Only consider resources attached to locations with the  specified location class.",
	)

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)

	flag.StringVar(&upstreamKubeconfig, "upstream-kubeconfig", "", "absolute path to the kubeconfig "+
		"file for the API server that is the source of truth for datum entities")

	flag.StringVar(&infraNamespace, "infra-namespace", "", "The namespace which resources for managing GCP entities "+
		"should be created in.")

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// TODO(user): TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
		// not provided, self-signed certificates will be generated by default. This option is not recommended for
		// production environments as self-signed certificates do not offer the same level of trust and security
		// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
		// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
		// to provide certificates, ensuring the server communicates using trusted and secure certificates.
		TLSOpts: tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(upstreamKubeconfig) == 0 {
		setupLog.Info("must provide --upstream-kubeconfig")
		os.Exit(1)
	}

	if len(infraNamespace) == 0 {
		setupLog.Info("must provide --infra-namespace")
		os.Exit(1)
	}

	upstreamClusterConfig, err := clientcmd.BuildConfigFromFlags("", upstreamKubeconfig)
	if err != nil {
		setupLog.Error(err, "unable to load control plane kubeconfig")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(upstreamClusterConfig, ctrl.Options{
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

	// We consider the cluster that the operator has been deployed in to be the
	// target cluster for infrastructure components.
	infraCluster, err := cluster.New(ctrl.GetConfigOrDie(), func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		setupLog.Error(err, "failed to construct cluster")
		os.Exit(1)
	}

	if err := mgr.Add(infraCluster); err != nil {
		setupLog.Error(err, "failed to add cluster to manager")
		os.Exit(1)
	}

	// TODO(jreese) rework the gateway controller when we have a higher level
	// orchestrator from network-services-operator that schedules "sub gateways"
	// onto clusters, similar to Workloads -> WorkloadDeployments and
	// Networks -> NetworkContexts
	//
	// if err = (&controller.WorkloadGatewayReconciler{
	// 	Client:      mgr.GetClient(),
	// 	InfraClient: infraCluster.GetClient(),
	// 	Scheme:      mgr.GetScheme(),
	// 	GCPProject:  gcpProject,
	// }).SetupWithManager(mgr, infraCluster); err != nil {
	// 	setupLog.Error(err, "unable to create controller", "controller", "WorkloadReconciler")
	// 	os.Exit(1)
	// }

	if err = (&controller.WorkloadDeploymentReconciler{
		Client:                    mgr.GetClient(),
		InfraClient:               infraCluster.GetClient(),
		Scheme:                    mgr.GetScheme(),
		LocationClassName:         locationClassName,
		InfraClusterNamespaceName: infraNamespace,
	}).SetupWithManager(mgr, infraCluster); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkloadDeploymentReconciler")
		os.Exit(1)
	}

	if err = (&controller.InstanceDiscoveryReconciler{
		Client:      mgr.GetClient(),
		InfraClient: infraCluster.GetClient(),
		Scheme:      mgr.GetScheme(),
	}).SetupWithManager(mgr, infraCluster); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InstanceDiscoveryReconciler")
		os.Exit(1)
	}

	if err = (&controller.NetworkContextReconciler{
		Client:                    mgr.GetClient(),
		InfraClient:               infraCluster.GetClient(),
		Scheme:                    mgr.GetScheme(),
		LocationClassName:         locationClassName,
		InfraClusterNamespaceName: infraNamespace,
	}).SetupWithManager(mgr, infraCluster); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkContextReconciler")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
