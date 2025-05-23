// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	gcpcomputev1beta1 "github.com/upbound/provider-gcp/apis/compute/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkContextReconciler reconciles a NetworkContext and ensures that a GCP
// ComputeNetwork is created to represent the context within GCP.
type NetworkContextReconciler struct {
	mgr               mcmanager.Manager
	finalizers        finalizer.Finalizers
	DownstreamCluster cluster.Cluster
	LocationClassName string
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts/finalizers,verbs=update

// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computenetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computenetworks/status,verbs=get

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

	_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), networkContext.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling network context")
	defer logger.Info("reconcile complete")

	finalizationResult, err := r.finalizers.Finalize(ctx, &networkContext)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
	}
	if finalizationResult.Updated {
		if err = cl.GetClient().Update(ctx, &networkContext); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

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
		Message:            "Network is not ready",
	}

	defer func() {
		if err != nil {
			// Don't update the status if errors are encountered
			return
		}
		statusChanged := apimeta.SetStatusCondition(&networkContext.Status.Conditions, programmedCondition)

		if statusChanged {
			err = cl.GetClient().Status().Update(ctx, &networkContext)
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

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, cl.GetClient(), r.DownstreamCluster.GetClient())

	downstreamClient := downstreamStrategy.GetClient()
	downstreamNetworkObjectMeta, err := downstreamStrategy.ObjectMetaFromUpstreamObject(ctx, &network)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get downstream network object metadata: %w", err)
	}

	downstreamNetworkObjectMeta.Name = fmt.Sprintf("network-%s", network.UID)

	downstreamNetwork := &gcpcomputev1beta1.Network{
		ObjectMeta: downstreamNetworkObjectMeta,
	}

	result, err := controllerutil.CreateOrUpdate(ctx, downstreamClient, downstreamNetwork, func() error {
		if err := downstreamStrategy.SetControllerReference(ctx, &network, downstreamNetwork); err != nil {
			return fmt.Errorf("failed to set controller reference on downstream network: %w", err)
		}

		downstreamNetwork.Spec = gcpcomputev1beta1.NetworkSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: "project-test-fz3pr6", // TODO
				},
			},
			ForProvider: gcpcomputev1beta1.NetworkParameters{
				Mtu:                   ptr.To(float64(network.Spec.MTU)),
				AutoCreateSubnetworks: ptr.To(false),
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

	if downstreamNetwork.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("GCP network not ready yet")
		return ctrl.Result{}, nil
	}

	programmedCondition.Status = metav1.ConditionTrue
	programmedCondition.Reason = networkingv1alpha.NetworkContextProgrammed
	programmedCondition.Message = "Network is ready."

	return ctrl.Result{}, nil
}

func (r *NetworkContextReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {

	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("cluster name not found in context")
	}

	cl, err := r.mgr.GetCluster(ctx, clusterName)
	if err != nil {
		return finalizer.Result{}, err
	}

	_ = cl
	// TODO

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkContextReconciler) SetupWithManager(mgr mcmanager.Manager, downstreamCluster cluster.Cluster) error {
	r.mgr = mgr
	r.DownstreamCluster = downstreamCluster
	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpInfraFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	return mcbuilder.ControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkContext{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Owns(&gcpcomputev1beta1.Network{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Complete(r)
}
