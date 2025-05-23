package controller

import (
	"context"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
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

// SubnetReconciler reconciles Instances and manages their intended state in
// GCP
type SubnetReconciler struct {
	mgr               mcmanager.Manager
	finalizers        finalizer.Finalizers
	LocationClassName string
	DownstreamCluster cluster.Cluster
}

func (r *SubnetReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var subnet networkingv1alpha.Subnet
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), subnet.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &subnet)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(errResourceIsDeleting) {
			logger.Info("subnet still has resources in GCP, waiting until removal")
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err = cl.GetClient().Update(ctx, &subnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !subnet.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling subnet")
	defer logger.Info("reconcile complete")

	if !apimeta.IsStatusConditionTrue(subnet.Status.Conditions, networkingv1alpha.SubnetAllocated) {
		logger.Info("subnet has not been allocated a prefix")
		return ctrl.Result{}, nil
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, cl.GetClient(), r.DownstreamCluster.GetClient())

	downstreamClient := downstreamStrategy.GetClient()
	downstreamSubnetObjectMeta, err := downstreamStrategy.ObjectMetaFromUpstreamObject(ctx, &subnet)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get downstream subnet object metadata: %w", err)
	}

	downstreamSubnet := &gcpcomputev1beta2.Subnetwork{
		ObjectMeta: downstreamSubnetObjectMeta,
	}

	result, err := controllerutil.CreateOrUpdate(ctx, downstreamClient, downstreamSubnet, func() error {
		if err := downstreamStrategy.SetControllerReference(ctx, &subnet, downstreamSubnet); err != nil {
			return fmt.Errorf("failed to set controller reference on downstream gateway: %w", err)
		}

		downstreamSubnet.Spec = gcpcomputev1beta2.SubnetworkSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: "project-test-fz3pr6", // TODO
				},
			},
			ForProvider: gcpcomputev1beta2.SubnetworkParameters_2{
				IPCidrRange: ptr.To(fmt.Sprintf("%s/%d", *subnet.Status.StartAddress, *subnet.Status.PrefixLength)),
				NetworkRef: &crossplanecommonv1.Reference{
					Name: "TODO",
				},
				Region:    ptr.To("TODO"),
				Purpose:   ptr.To("PRIVATE_RFC_1918"),
				StackType: ptr.To("IPV4_ONLY"),
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

	logger.Info("downstream subnet processed", "operation_result", result)

	return ctrl.Result{}, nil
}

func (r *SubnetReconciler) Finalize(
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

	_ = downstreamclient.NewMappedNamespaceResourceStrategy(clusterName, cl.GetClient(), r.DownstreamCluster.GetClient())

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubnetReconciler) SetupWithManager(mgr mcmanager.Manager, downstreamCluster cluster.Cluster) error {
	r.mgr = mgr
	r.finalizers = finalizer.NewFinalizers()
	r.DownstreamCluster = downstreamCluster

	return mcbuilder.ControllerManagedBy(mgr).
		For(&networkingv1alpha.Subnet{}).
		Named("subnet").
		Complete(r)
}
