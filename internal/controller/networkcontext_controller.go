// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	gcpcomputev1beta1 "github.com/upbound/provider-gcp/apis/compute/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkContextReconciler reconciles a NetworkContext and ensures that a GCP
// ComputeNetwork is created to represent the context within GCP.
type NetworkContextReconciler struct {
	mgr               mcmanager.Manager
	Config            config.GCPProvider
	DownstreamCluster cluster.Cluster
	LocationClassName string
}

func (r *NetworkContextReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var networkContext networkingv1alpha.NetworkContext
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &networkContext); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), networkContext.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling network context")
	defer logger.Info("reconcile complete")

	if !networkContext.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if apimeta.IsStatusConditionTrue(networkContext.Status.Conditions, networkingv1alpha.NetworkContextProgrammed) {
		logger.Info("network context is already programmed")
		return ctrl.Result{}, nil
	}

	programmedCondition := metav1.Condition{
		Type:               networkingv1alpha.NetworkContextProgrammed,
		Status:             metav1.ConditionFalse,
		Reason:             networkingv1alpha.NetworkContextProgrammedReasonNotProgrammed,
		ObservedGeneration: networkContext.Generation,
		Message:            "Network has not been programmed",
	}

	defer func() {
		if apimeta.SetStatusCondition(&networkContext.Status.Conditions, programmedCondition) {
			err = errors.Join(err, cl.GetClient().Status().Update(ctx, &networkContext))
		}
	}()

	var network networkingv1alpha.Network
	networkObjectKey := client.ObjectKey{
		Namespace: networkContext.Namespace,
		Name:      networkContext.Spec.Network.Name,
	}
	if err := cl.GetClient().Get(ctx, networkObjectKey, &network); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching network: %w", err)
	}

	// Note!!!
	//
	// Crossplane v1 managed resources are cluster scoped! We don't want
	// to deal with the creation of Composite Resource Definitions, Composite
	// Resources, or Claims. As a result, the resource name needs to be globally
	// unique.
	//
	// In Crossplane v2, which is currently in preview, managed resources will
	// also be available as namespaced resources. Unfortunately the preview does
	// not include modifications to the GCP provider.
	//
	// If we want the name to be used for the resource at the provider to differ
	// from this resource name, use the following annotation:
	// crossplanemeta.AnnotationKeyExternalName (crossplane.io/external-name)
	downstreamNetwork := &gcpcomputev1beta1.Network{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("network-%s", network.UID),
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.DownstreamCluster.GetClient(), downstreamNetwork, func() error {
		if downstreamNetwork.Annotations == nil {
			downstreamNetwork.Annotations = make(map[string]string)
		}

		downstreamNetwork.Annotations[downstreamclient.UpstreamOwnerClusterName] = req.ClusterName

		downstreamNetwork.Spec = gcpcomputev1beta1.NetworkSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: r.Config.GetProviderConfigName(req.ClusterName),
				},
			},
			ForProvider: gcpcomputev1beta1.NetworkParameters{
				Mtu:                                   ptr.To(float64(network.Spec.MTU)),
				AutoCreateSubnetworks:                 ptr.To(false),
				NetworkFirewallPolicyEnforcementOrder: ptr.To("AFTER_CLASSIC_FIREWALL"),
				RoutingMode:                           ptr.To("REGIONAL"),
				Project:                               ptr.To(location.Spec.Provider.GCP.ProjectID),
			},
		}

		return nil
	})

	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("downstream network processed", "operation_result", result)

	// See if there's been a failure to create the network.
	//
	// NOTE(jreese): Odd observation - the ServiceAccount failure info is in a different
	// condition. Need to look into that.
	if condition := downstreamNetwork.Status.GetCondition(crossplanecommonv1.TypeSynced); condition.Reason == "ReconcileError" {
		logger.Info("network failed to create")
		programmedCondition.Reason = "NetworkFailedToCreate"
		programmedCondition.Message = fmt.Sprintf("Network failed to create: %s", condition.Message)
		return ctrl.Result{}, fmt.Errorf(programmedCondition.Message)
	}

	if downstreamNetwork.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("GCP network not ready yet")

		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = networkingv1alpha.NetworkContextProgrammedReasonProgrammingInProgress
		programmedCondition.Message = "Network is being programmed."

		return ctrl.Result{}, nil
	}

	programmedCondition.Status = metav1.ConditionTrue
	programmedCondition.Reason = networkingv1alpha.NetworkContextProgrammed
	programmedCondition.Message = "Network is ready."

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkContextReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkContext{}).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpcomputev1beta1.Network{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*gcpcomputev1beta1.Network, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, network *gcpcomputev1beta1.Network) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := network.Annotations[downstreamclient.UpstreamOwnerClusterName]

				if upstreamClusterName == "" {
					logger.Info("GCP network is missing upstream ownership metadata")
					return nil
				}

				cluster, err := mgr.GetCluster(ctx, upstreamClusterName)
				if err != nil {
					logger.Error(err, "failed to get upstream cluster")
					return nil
				}
				clusterClient := cluster.GetClient()

				listOpts := client.MatchingFields{
					networkContextControllerNetworkUIDIndex: network.GetName(),
				}

				var networkContexts networkingv1alpha.NetworkContextList
				if err := clusterClient.List(ctx, &networkContexts, listOpts); err != nil {
					logger.Error(err, "failed to list network contexts")
					return nil
				}

				var requests []mcreconcile.Request
				for _, networkContext := range networkContexts.Items {
					requests = append(requests, mcreconcile.Request{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: networkContext.Namespace,
								Name:      networkContext.Name,
							},
						},
						ClusterName: upstreamClusterName,
					})
				}

				return requests
			})
		})).
		Complete(r)
}
