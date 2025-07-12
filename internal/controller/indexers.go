package controller

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	networkContextControllerNetworkUIDIndex = "networkContextControllerNetworkUIDIndex"

	clusterDownstreamWorkloadIdentifierIndex = "clusterDownstreamWorkloadIdentifierIndex"
)

func AddIndexers(ctx context.Context, mgr mcmanager.Manager) error {
	return errors.Join(
		addNetworkContextControllerIndexers(ctx, mgr),
	)
}

func addNetworkContextControllerIndexers(ctx context.Context, mgr mcmanager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &networkingv1alpha.NetworkContext{}, networkContextControllerNetworkUIDIndex, networkContextControllerNetworkUIDIndexFunc); err != nil {
		return fmt.Errorf("failed to add network context controller indexer %q: %w", networkContextControllerNetworkUIDIndex, err)
	}

	return nil
}

func networkContextControllerNetworkUIDIndexFunc(o client.Object) []string {

	if networkRef := metav1.GetControllerOf(o); networkRef != nil {
		return []string{
			fmt.Sprintf("network-%s", networkRef.UID),
		}
	}

	return nil
}

func AddClusterDownstreamWorkloadIndexers(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &infrav1alpha1.ClusterDownstreamWorkload{}, clusterDownstreamWorkloadIdentifierIndex, clusterDownstreamWorkloadIdentifierIndexIndexFunc); err != nil {
		return fmt.Errorf("failed to add cluster downstream workload indexer %q: %w", clusterDownstreamWorkloadIdentifierIndex, err)
	}

	return nil
}

func clusterDownstreamWorkloadIdentifierIndexIndexFunc(o client.Object) []string {
	obj := o.(*infrav1alpha1.ClusterDownstreamWorkload)
	return []string{
		fmt.Sprintf(
			"%s/%s/%s",
			obj.Spec.UpstreamWorkloadRef.ClusterName,
			obj.Spec.UpstreamWorkloadRef.Namespace,
			obj.Spec.UpstreamWorkloadRef.Name,
		),
	}
}
