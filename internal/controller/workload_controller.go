// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	gcpcloudplatformv1beta1 "github.com/upbound/provider-gcp/apis/cloudplatform/v1beta1"
	gcpsecretmanagerv1beta2 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpInfraFinalizer = "compute.datumapis.com/infra-provider-gcp"

// WorkloadReconciler reconciles a Workload with the primary function of cleaning
// up the downstream resources when the workload is deleted.
type WorkloadReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
}

// +kubebuilder:rbac:groups=workload.datumapis.com,resources=workloads,verbs=get;list;watch
// +kubebuilder:rbac:groups=workload.datumapis.com,resources=workloads/finalizers,verbs=update

func (r *WorkloadReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var workload computev1alpha.Workload
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("reconciling workload")
	defer logger.Info("reconcile complete")

	if !workload.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&workload, gcpInfraFinalizer) {
			downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, cl.GetClient(), r.DownstreamCluster.GetClient())

			if err := downstreamStrategy.DeleteAnchorForObject(ctx, &workload); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete anchor for workload: %w", err)
			}

			if err := downstreamStrategy.GetClient().Delete(ctx, &gcpcloudplatformv1beta1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("workload-%s", workload.UID),
				},
			}); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete service account: %w", err)
			}

			if err := downstreamStrategy.GetClient().Delete(ctx, &gcpsecretmanagerv1beta2.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("workload-%s", workload.UID),
				},
			}); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete secret: %w", err)
			}

			controllerutil.RemoveFinalizer(&workload, gcpInfraFinalizer)
			if err := cl.GetClient().Update(ctx, &workload); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&workload, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&workload, gcpInfraFinalizer)
		if err := cl.GetClient().Update(ctx, &workload); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		// TODO(jreese) add predicate to only enqueue workloads with an instance in
		// the location class managed by this controller.
		For(&computev1alpha.Workload{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Named("workload").
		Complete(r)
}
