// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	"go.datum.net/infra-provider-gcp/internal/controller/providers/aws"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpInfraFinalizer = "compute.datumapis.com/infra-provider-gcp"

type ClusterDownstreamWorkloadReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
	Config            *config.GCPProvider

	awsWorkloadReconciler providers.WorkloadReconciler
}

func (r *ClusterDownstreamWorkloadReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	// Note: This reconciler bypasses the multicluster lib as the downstream
	// cluster is not registered with it. This is because there's currently no
	// way to control which cluster a watch is registered with aside from the
	// "local" cluster, and "provider" clusters.

	downstreamClient := r.DownstreamCluster.GetClient()

	var downstreamWorkload infrav1alpha1.ClusterDownstreamWorkload
	if err := downstreamClient.Get(ctx, req.NamespacedName, &downstreamWorkload); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("reconciling workload")
	defer logger.Info("reconcile complete")

	if !downstreamWorkload.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&downstreamWorkload, gcpInfraFinalizer) {
			// downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
			// 	req.ClusterName,
			// 	cl.GetClient(),
			// 	r.DownstreamCluster.GetClient(),
			// 	r.Config.DownstreamResourceManagement.ManagedResourceLabels,
			// )

			// if err := downstreamStrategy.DeleteAnchorForObject(ctx, &workload); err != nil {
			// 	return ctrl.Result{}, fmt.Errorf("failed to delete anchor for workload: %w", err)
			// }

			// if err := downstreamStrategy.GetClient().Delete(ctx, &gcpcloudplatformv1beta1.ServiceAccount{
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name: fmt.Sprintf("workload-%s", workload.UID),
			// 	},
			// }); client.IgnoreNotFound(err) != nil {
			// 	return ctrl.Result{}, fmt.Errorf("failed to delete service account: %w", err)
			// }

			// if err := downstreamStrategy.GetClient().Delete(ctx, &gcpsecretmanagerv1beta2.Secret{
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name: fmt.Sprintf("workload-%s", workload.UID),
			// 	},
			// }); client.IgnoreNotFound(err) != nil {
			// 	return ctrl.Result{}, fmt.Errorf("failed to delete secret: %w", err)
			// }

			// // Locate firewall rules and delete them
			// var firewallRules gcpcomputev1beta2.FirewallList
			// listOpts := []client.ListOption{
			// 	client.MatchingLabels{
			// 		computev1alpha.WorkloadUIDLabel: string(workload.UID),
			// 	},
			// }

			// if err := downstreamStrategy.GetClient().List(ctx, &firewallRules, listOpts...); err != nil {
			// 	return ctrl.Result{}, fmt.Errorf("failed to list firewall rules: %w", err)
			// }

			// for _, firewallRule := range firewallRules.Items {
			// 	if err := downstreamStrategy.GetClient().Delete(ctx, &firewallRule); client.IgnoreNotFound(err) != nil {
			// 		return ctrl.Result{}, fmt.Errorf("failed to delete firewall rule: %w", err)
			// 	}
			// }

			// controllerutil.RemoveFinalizer(&workload, gcpInfraFinalizer)
			// if err := cl.GetClient().Update(ctx, &workload); err != nil {
			// 	return ctrl.Result{}, err
			// }
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&downstreamWorkload, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&downstreamWorkload, gcpInfraFinalizer)
		if err := downstreamClient.Update(ctx, &downstreamWorkload); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	cl, err := r.mgr.GetCluster(ctx, downstreamWorkload.Spec.WorkloadRef.UpstreamClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var workload computev1alpha.Workload
	workloadObjectKey := client.ObjectKey{
		Namespace: downstreamWorkload.Spec.WorkloadRef.Namespace,
		Name:      downstreamWorkload.Spec.WorkloadRef.Name,
	}
	if err := cl.GetClient().Get(ctx, workloadObjectKey, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching upstream workload: %w", err)
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
		downstreamWorkload.Spec.WorkloadRef.UpstreamClusterName,
		cl.GetClient(),
		downstreamClient,
		r.Config.DownstreamResourceManagement.ManagedResourceLabels,
	)

	// TODO(jreese) add provider info into the ClusterDownstreamWorkload

	result, err := r.awsWorkloadReconciler.Reconcile(ctx, downstreamStrategy, downstreamStrategy.GetClient(), downstreamWorkload.Spec.WorkloadRef.UpstreamClusterName, workload)
	if err != nil {
		return result, err
	} else if result.RequeueAfter > 0 {
		return result, nil
	}

	if apimeta.SetStatusCondition(&downstreamWorkload.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: downstreamWorkload.Generation,
		Reason:             "Ready",
		Message:            "Downstream resources have been reconciled",
	}) {

		if err := downstreamClient.Status().Update(ctx, &downstreamWorkload); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed updating downstream workload status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterDownstreamWorkloadReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	r.awsWorkloadReconciler = aws.NewWorkloadReconciler(*r.Config)

	return mcbuilder.ControllerManagedBy(mgr).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &infrav1alpha1.ClusterDownstreamWorkload{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*infrav1alpha1.ClusterDownstreamWorkload, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj *infrav1alpha1.ClusterDownstreamWorkload) []mcreconcile.Request {
				return []mcreconcile.Request{
					{
						// TODO(jreese) contribute more flexible watch registration for clusters
						// so we can use the standard multicluster lib for these watches.
						ClusterName: "__downstream__",
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: obj.Namespace,
								Name:      obj.Name,
							},
						},
					},
				}
			})
		})).
		Named("workload").
		Complete(r)
}
