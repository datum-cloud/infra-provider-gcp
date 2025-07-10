// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// WorkloadDeploymentReconciler reconciles a WorkloadDeployment with the primary function of cleaning
// up the downstream resources when the workload is deleted.
type WorkloadDeploymentReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
	Config            *config.GCPProvider

	awsWorkloadDeploymentReconciler providers.WorkloadDeploymentReconciler
}

func (r *WorkloadDeploymentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var workloadDeployment computev1alpha.WorkloadDeployment
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &workloadDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't do anything if a location isn't set
	if workloadDeployment.Status.Location == nil {
		return ctrl.Result{}, nil
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *workloadDeployment.Status.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling workload deployment")
	defer logger.Info("reconcile complete")

	if !workloadDeployment.DeletionTimestamp.IsZero() {
		// TODO(jreese): Clean up the downstream resources
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&workloadDeployment, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&workloadDeployment, gcpInfraFinalizer)
		if err := cl.GetClient().Update(ctx, &workloadDeployment); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
		req.ClusterName,
		cl.GetClient(),
		r.DownstreamCluster.GetClient(),
		r.Config.DownstreamResourceManagement.ManagedResourceLabels,
	)
	downstreamClient := downstreamStrategy.GetClient()

	// Ensure that the downstream workload deployment exists
	var downstreamWorkloadDeployment infrav1alpha1.ClusterDownstreamWorkloadDeployment
	if err := downstreamClient.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
	}, &downstreamWorkloadDeployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching downstream workload deployment: %w", err)
	}

	if downstreamWorkloadDeployment.CreationTimestamp.IsZero() {
		downstreamWorkloadDeployment := infrav1alpha1.ClusterDownstreamWorkloadDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
			},
			Spec: infrav1alpha1.ClusterDownstreamWorkloadDeploymentSpec{
				WorkloadDeploymentRef: infrav1alpha1.WorkloadDeploymentRef{
					UpstreamClusterName: req.ClusterName,
					Namespace:           workloadDeployment.Namespace,
					Name:                workloadDeployment.Name,
				},
			},
		}

		if err := downstreamClient.Create(ctx, &downstreamWorkloadDeployment); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating downstream workload deployment: %w", err)
		}
	}

	var workload computev1alpha.Workload
	workloadObjectKey := client.ObjectKey{
		Namespace: workloadDeployment.Namespace,
		Name:      workloadDeployment.Spec.WorkloadRef.Name,
	}
	if err := cl.GetClient().Get(ctx, workloadObjectKey, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching workload: %w", err)
	}

	// TODO(jreese)
	//
	// Ensure that a downstream workload exists for the provider
	// "scope". In the case of AWS, that will be the ARN in the Location. For GCP,
	// it will be the project name.
	//
	// We do this because a single workload may span multiple GCP projects or
	// multiple AWS accounts, which each need their own workload scoped resources.

	if location.Spec.Provider.AWS != nil {
		return r.awsWorkloadDeploymentReconciler.Reconcile(
			ctx,
			downstreamStrategy,
			downstreamClient,
			req.ClusterName,
			*location,
			workload,
			workloadDeployment,
			downstreamWorkloadDeployment,
			nil, // aggregatedK8sSecret,
		)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDeploymentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		Watches(&infrav1alpha1.ClusterDownstreamWorkloadDeployment{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				downstreamWorkloadDeployment := obj.(*infrav1alpha1.ClusterDownstreamWorkloadDeployment)
				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.Namespace,
								Name:      downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.Name,
							},
						},
						ClusterName: downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.UpstreamClusterName,
					},
				}
			})
		}).
		Named("workloaddeployment").
		Complete(r)
}
