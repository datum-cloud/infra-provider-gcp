package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.datum.net/infra-provider-gcp/internal/crossclusterutil"
)

// InfraClusterNamespaceReconciler reconciles a Workload object and processes any
// gateways defined.
type InfraClusterNamespaceReconciler struct {
	client.Client
	InfraClient client.Client
	Scheme      *runtime.Scheme

	finalizers finalizer.Finalizers
}

var ignoreNamespaces = []string{
	"datum-system",
	"kube-public",
	"kube-system",
}

func (r *InfraClusterNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if slices.Contains(ignoreNamespaces, req.Name) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	// Work with the unstructured form of an instance group manager, as the generated
	// types are not aligned with the actual CRD. Particularly the `targetSize`
	// field.

	var namespace corev1.Namespace
	if err := r.Client.Get(ctx, req.NamespacedName, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
	}
	if finalizationResult.Updated {
		if err = r.Client.Update(ctx, &namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !namespace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling namespace")
	defer logger.Info("reconcile complete")

	var infraNamespace corev1.Namespace
	infraNamespaceObjectKey := client.ObjectKey{
		Name: crossclusterutil.InfraClusterNamespaceName(namespace),
	}
	if err := r.InfraClient.Get(ctx, infraNamespaceObjectKey, &infraNamespace); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching infra namespace: %w", err)
	}

	if infraNamespace.CreationTimestamp.IsZero() {
		logger.Info("creating infra namespace")
		infraNamespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: infraNamespaceObjectKey.Name,
				Labels: map[string]string{
					crossclusterutil.UpstreamOwnerNamespaceLabel: namespace.Name,
				},
			},
		}

		if err := r.InfraClient.Create(ctx, &infraNamespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating infra namespace: %w", err)
		}

	}

	return ctrl.Result{}, nil
}

func (r *InfraClusterNamespaceReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {
	namespace := obj.(*corev1.Namespace)

	var infraNamespace corev1.Namespace
	infraNamespaceObjectKey := client.ObjectKey{
		Namespace: crossclusterutil.InfraClusterNamespaceName(*namespace),
	}
	if err := r.InfraClient.Get(ctx, infraNamespaceObjectKey, &infraNamespace); client.IgnoreNotFound(err) != nil {
		return finalizer.Result{}, fmt.Errorf("failed fetching infra namespace: %w", err)
	}

	if err := r.InfraClient.Delete(ctx, &infraNamespace); err != nil {
		return finalizer.Result{}, fmt.Errorf("failed deleting infra namespace: %w", err)
	}

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InfraClusterNamespaceReconciler) SetupWithManager(mgr ctrl.Manager, infraCluster cluster.Cluster) error {

	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpWorkloadFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		// TODO(jreese) watch upstream ns. Need to adjust SetControllerReference to
		// support non namespaced entities. Need an anchor that's not namespaced.
		Named("infracluster-namespaces").
		Complete(r)
}
