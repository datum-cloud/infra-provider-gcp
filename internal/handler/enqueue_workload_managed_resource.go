package handler

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// EnqueueInstancesForWorkloadOwnedDownstreamResource produces a multicluster
// aware handler that expects the watched resource to be decorated with
// upstream workload metadata via labels and annotations. It will use this
// information to enqueue instances belonging to the workload which have not
// yet been programmed.
//
// Required labels:
// - compute.datumapis.com/workload-uid: the UID of the workload
//
// Required annotations:
// - meta.datumapis.com/upstream-owner-cluster-name: the name of the cluster that owns the workload
// - meta.datumapis.com/upstream-owner-name: the name of the workload
// - meta.datumapis.com/upstream-owner-namespace: the namespace of the workload
func EnqueueInstancesForWorkloadOwnedDownstreamResource[object client.Object](mgr mcmanager.Manager) mchandler.TypedEventHandlerFunc[object, mcreconcile.Request] {
	// Return a mchandler.TypedEventHandlerFunc. The clusterName and cluster are
	// not used.
	return func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[object, mcreconcile.Request] {
		// Return the handler that will actually enqueue the requests.
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj object) []mcreconcile.Request {
			logger := log.FromContext(ctx).WithValues("resource_kind", obj.GetObjectKind().GroupVersionKind().Kind, "resource_name", obj.GetName(), "resource_namespace", obj.GetNamespace())

			annotations := obj.GetAnnotations()
			if annotations == nil {
				logger.Info("resource is missing upstream ownership annotation metadata")
				return nil
			}

			labels := obj.GetLabels()
			if labels == nil {
				logger.V(1).Info("resource is missing workload ownership label metadata")
				return nil
			}

			upstreamClusterName := annotations[downstreamclient.UpstreamOwnerClusterName]
			upstreamName := annotations[downstreamclient.UpstreamOwnerName]
			upstreamNamespace := annotations[downstreamclient.UpstreamOwnerNamespace]

			workloadUID := labels[computev1alpha.WorkloadUIDLabel]

			if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" || workloadUID == "" {
				logger.V(1).Info(
					"resource is missing upstream ownership metadata",
					"upstream_cluster_name", upstreamClusterName,
					"upstream_name", upstreamName,
					"upstream_namespace", upstreamNamespace,
					"workload_uid", workloadUID,
				)
				return nil
			}

			var instanceList computev1alpha.InstanceList
			listOpts := []client.ListOption{
				client.InNamespace(upstreamNamespace),
				client.MatchingLabels{
					computev1alpha.WorkloadUIDLabel: workloadUID,
				},
			}

			upstreamCluster, err := mgr.GetCluster(ctx, upstreamClusterName)
			if err != nil {
				logger.Error(err, "failed to get upstream cluster")
				return nil
			}

			if err := upstreamCluster.GetClient().List(ctx, &instanceList, listOpts...); err != nil {
				logger.Error(err, "failed to list instances")
				return nil
			}

			var requests []mcreconcile.Request
			for _, instance := range instanceList.Items {
				requests = append(requests, mcreconcile.Request{
					Request: reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: instance.Namespace,
							Name:      instance.Name,
						},
					},
					ClusterName: upstreamClusterName,
				})
			}

			return requests
		})
	}
}
