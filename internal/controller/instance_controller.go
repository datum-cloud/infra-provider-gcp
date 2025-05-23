package controller

import (
	"context"
	"errors"
	"fmt"

	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/locationutil"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpWorkloadFinalizer = "compute.datumapis.com/gcp-workload-controller"

var errResourceIsDeleting = errors.New("resource is deleting")

// InstanceReconciler reconciles Instances and manages their intended state in
// GCP
type InstanceReconciler struct {
	mgr               mcmanager.Manager
	finalizers        finalizer.Finalizers
	LocationClassName string
}

func (r *InstanceReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var instance computev1alpha.Instance
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't do anything if a location isn't set
	if instance.Spec.Location == nil {
		return ctrl.Result{}, nil
	}

	_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *instance.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &instance)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(errResourceIsDeleting) {
			logger.Info("instance still has resources in GCP, waiting until removal")
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err = cl.GetClient().Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling instance")
	defer logger.Info("reconcile complete")

	if len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, waiting until they are removed", "scheduling_gates", instance.Spec.Controller.SchedulingGates)
		return ctrl.Result{}, nil
	}

	var gcpInstance gcpcomputev1beta2.Instance

	_ = gcpInstance

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager, downstreamCluster cluster.Cluster) error {
	r.mgr = mgr
	r.finalizers = finalizer.NewFinalizers()

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		Named("instance").
		Complete(r)
}
