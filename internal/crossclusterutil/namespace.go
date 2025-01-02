package crossclusterutil

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func InfraClusterNamespaceName(ns corev1.Namespace) string {
	return fmt.Sprintf("ns-%s", ns.UID)
}

func InfraClusterNamespaceNameFromUpstream(ctx context.Context, c client.Client, name string) (string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		return "", fmt.Errorf("failed fetching upstream namespace: %w", err)
	}

	return InfraClusterNamespaceName(ns), nil
}
