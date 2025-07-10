package downstreamclient

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CreateOrPatch creates or patches a downstream resource with upstream owner annotations.
// T must be a client.Object type. The function automatically sets the required
// upstream owner annotations based on the provided parent object and cluster name.
// The obj parameter should be an object with just namespace and name set.
// The callback function is required to set the spec and other desired fields.
func CreateOrPatch[T client.Object](
	ctx context.Context,
	downstreamClient client.Client,
	obj T,
	clusterName string,
	parent metav1.Object,
	callback func(T) error,
) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrPatch(ctx, downstreamClient, obj, func() error {
		// Set upstream owner annotations
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}

		annotations[UpstreamOwnerName] = parent.GetName()
		annotations[UpstreamOwnerNamespace] = parent.GetNamespace()
		annotations[UpstreamOwnerClusterName] = clusterName

		obj.SetAnnotations(annotations)

		// Call the callback function to set the spec
		if callback != nil {
			if err := callback(obj); err != nil {
				return err
			}
		}

		return nil
	})
}
