// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	kcccomputev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/compute/v1beta1"
	kcccomputev1alpha1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/k8s/v1alpha1"
	"google.golang.org/protobuf/proto"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"go.datum.net/infra-provider-gcp/internal/controller/k8sconfigconnector"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkContextReconciler reconciles a NetworkContext and ensures that a GCP
// ComputeNetwork is created to represent the context within GCP.
type NetworkContextReconciler struct {
	client.Client
	InfraClient client.Client
	Scheme      *runtime.Scheme
	GCPProject  string
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=networkcontexts/finalizers,verbs=update

// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computenetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computenetworks/status,verbs=get

func (r *NetworkContextReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	var networkContext networkingv1alpha.NetworkContext
	if err := r.Client.Get(ctx, req.NamespacedName, &networkContext); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !networkContext.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling network context")
	defer logger.Info("reconcile complete")

	readyCondition := metav1.Condition{
		Type:               networkingv1alpha.NetworkBindingReady,
		Status:             metav1.ConditionFalse,
		Reason:             "Unknown",
		ObservedGeneration: networkContext.Generation,
		Message:            "Unknown state",
	}

	defer func() {
		if err != nil {
			// Don't update the status if errors are encountered
			return
		}
		statusChanged := apimeta.SetStatusCondition(&networkContext.Status.Conditions, readyCondition)

		if statusChanged {
			err = r.Client.Status().Update(ctx, &networkContext)
		}
	}()

	var network networkingv1alpha.Network
	networkObjectKey := client.ObjectKey{
		Namespace: networkContext.Namespace,
		Name:      networkContext.Spec.Network.Name,
	}
	if err := r.Client.Get(ctx, networkObjectKey, &network); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching network: %w", err)
	}

	kccNetworkName := fmt.Sprintf("network-%s", networkContext.UID)

	var kccNetwork kcccomputev1beta1.ComputeNetwork
	kccNetworkObjectKey := client.ObjectKey{
		Namespace: networkContext.Namespace,
		Name:      kccNetworkName,
	}
	if err := r.InfraClient.Get(ctx, kccNetworkObjectKey, &kccNetwork); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching gcp network: %w", err)
	}

	if kccNetwork.CreationTimestamp.IsZero() {
		logger.Info("creating GCP network")

		kccNetwork = kcccomputev1beta1.ComputeNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: networkContext.Namespace,
				Name:      kccNetworkName,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kcccomputev1beta1.ComputeNetworkSpec{
				Mtu: proto.Int64(int64(network.Spec.MTU)),
			},
		}

		kccNetwork.Spec.AutoCreateSubnetworks = proto.Bool(false)

		if err := controllerutil.SetControllerReference(&networkContext, &kccNetwork, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set controller on firewall: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &kccNetwork); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating gcp network: %w", err)
		}
	}

	if !k8sconfigconnector.IsStatusConditionTrue(kccNetwork.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("GCP network not ready yet")
		readyCondition.Reason = "ProviderNetworkNotReady"
		readyCondition.Message = "Network is not ready."
		return ctrl.Result{}, nil
	}

	readyCondition.Status = metav1.ConditionTrue
	readyCondition.Reason = "NetworkReady"
	readyCondition.Message = "Network is ready."

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkContextReconciler) SetupWithManager(mgr ctrl.Manager, infraCluster cluster.Cluster) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkContext{}).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeNetwork{},
			handler.TypedEnqueueRequestForOwner[*kcccomputev1beta1.ComputeNetwork](mgr.GetScheme(), mgr.GetRESTMapper(), &networkingv1alpha.NetworkContext{}),
		)).
		Complete(r)
}
