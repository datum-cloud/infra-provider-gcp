package controller

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	networkContextControllerNetworkUIDIndex = "networkContextControllerNetworkUIDIndex"
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
	networkContext := o.(*networkingv1alpha.NetworkContext)

	ownerRefs := networkContext.GetOwnerReferences()

	for _, ref := range ownerRefs {
		if ref.Kind == "Network" && ptr.Deref(ref.Controller, false) {
			return []string{
				fmt.Sprintf("network-%s", ref.UID),
			}
		}
	}

	return []string{}
}
