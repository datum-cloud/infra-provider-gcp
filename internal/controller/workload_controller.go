// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	gcpcloudplatformv1beta1 "github.com/upbound/provider-gcp/apis/cloudplatform/v1beta1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	gcpsecretmanagerv1beta2 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta2"

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

	"go.datum.net/infra-provider-gcp/internal/config"

	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpInfraFinalizer = "compute.datumapis.com/infra-provider-gcp"

// WorkloadReconciler reconciles a Workload with the primary function of cleaning
// up the downstream resources when the workload is deleted.
type WorkloadReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
	Config            *config.GCPProvider
}

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
			downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
				req.ClusterName,
				cl.GetClient(),
				r.DownstreamCluster.GetClient(),
				r.Config.DownstreamResourceManagement.ManagedResourceLabels,
			)

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

			// Locate firewall rules and delete them
			var firewallRules gcpcomputev1beta2.FirewallList
			listOpts := []client.ListOption{
				client.MatchingLabels{
					computev1alpha.WorkloadUIDLabel: string(workload.UID),
				},
			}

			if err := downstreamStrategy.GetClient().List(ctx, &firewallRules, listOpts...); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to list firewall rules: %w", err)
			}

			for _, firewallRule := range firewallRules.Items {
				if err := downstreamStrategy.GetClient().Delete(ctx, &firewallRule); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, fmt.Errorf("failed to delete firewall rule: %w", err)
				}
			}

			controllerutil.RemoveFinalizer(&workload, gcpInfraFinalizer)
			if err := cl.GetClient().Update(ctx, &workload); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// NOTE(jreese) consider an approach that doesn't use a finalizer, but instead
	// creates a binding to the workload that can be owned by the workload and
	// subsequently GCd.

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
		Watches(&computev1alpha.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				logger := log.FromContext(ctx)
				instance := obj.(*computev1alpha.Instance)

				// Don't do anything if a location isn't set
				if instance.Spec.Location == nil {
					return nil
				}

				_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *instance.Spec.Location, r.LocationClassName)
				if err != nil {
					logger.Error(err, "failed to get location")
					return nil
				} else if !shouldProcess {
					return nil
				}

				workloadDeploymentRef := metav1.GetControllerOf(instance)
				if workloadDeploymentRef == nil {
					return nil
				}

				// Load the WorkloadDeployment for the instance
				var workloadDeployment computev1alpha.WorkloadDeployment
				workloadDeploymentObjectKey := client.ObjectKey{
					Namespace: instance.Namespace,
					Name:      workloadDeploymentRef.Name,
				}
				if err := cl.GetClient().Get(ctx, workloadDeploymentObjectKey, &workloadDeployment); err != nil {
					logger.Error(err, "failed fetching workload deployment")
					return nil
				}

				workloadRef := metav1.GetControllerOf(&workloadDeployment)
				if workloadRef == nil {
					logger.Info("workload deployment is not owned by a workload")
					return nil
				}

				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: workloadDeployment.Namespace,
								Name:      workloadRef.Name,
							},
						},
						ClusterName: clusterName,
					},
				}
			})
		}).
		Named("workload").
		Complete(r)
}
