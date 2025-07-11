package downstreamclient

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CreateOrPatch creates or updates a downstream resource with upstream owner annotations.
// T must be a client.Object type. The function automatically sets the required
// upstream owner annotations based on the provided parent object and cluster name.
// The obj parameter should be the desired object.
// The callback function is available to set fields which cannot be naturally merged,
// such as lists.
func CreateOrPatch[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	clusterName string,
	parent metav1.Object,
	callback func(observed, desired T) error,
) (controllerutil.OperationResult, error) {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	obj.GetObjectKind().SetGroupVersionKind(gvk)
	desired := obj.DeepCopyObject().(client.Object)

	// Set upstream owner annotations
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	annotations[UpstreamOwnerName] = parent.GetName()
	annotations[UpstreamOwnerNamespace] = parent.GetNamespace()
	annotations[UpstreamOwnerClusterName] = clusterName

	desired.SetAnnotations(annotations)

	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		if err := c.Create(ctx, desired); err != nil {
			return controllerutil.OperationResultNone, err
		}
		return controllerutil.OperationResultCreated, nil
	}

	if callback != nil {
		if err := callback(obj, desired.(T)); err != nil {
			return controllerutil.OperationResultNone, err
		}
	}

	// TODO(jreese) optimize for avoiding patches

	if err := c.Patch(
		ctx,
		desired,
		client.Apply,
		client.ForceOwnership,
		client.FieldOwner("infra-provider-crossplane"),
	); err != nil {
		return controllerutil.OperationResultNone, err
	}

	return controllerutil.OperationResultUpdated, nil
}
