// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	gcpcomputev1beta1 "github.com/upbound/provider-gcp/apis/compute/v1beta1"
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

	"go.datum.net/infra-provider-gcp/internal/locationutil"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkReconciler reconciles a Network with the primary function of cleaning
// up the downstream resources when the network is deleted.
type NetworkReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
}

func (r *NetworkReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var network networkingv1alpha.Network
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &network); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("reconciling network")
	defer logger.Info("reconcile complete")

	if !network.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&network, gcpInfraFinalizer) {

			downstreamNetwork := &gcpcomputev1beta1.Network{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("network-%s", network.UID),
				},
			}

			if err := r.DownstreamCluster.GetClient().Delete(ctx, downstreamNetwork); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete downstream network: %w", err)
			}

			controllerutil.RemoveFinalizer(&network, gcpInfraFinalizer)
			if err := cl.GetClient().Update(ctx, &network); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// NOTE(jreese) consider an approach that doesn't use a finalizer, but instead
	// creates a binding to the network that can be owned by the network and
	// subsequently GCd.

	if !controllerutil.ContainsFinalizer(&network, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&network, gcpInfraFinalizer)
		if err := cl.GetClient().Update(ctx, &network); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		// Watch NetworkContexts and enqueue requests for the Network
		Watches(&networkingv1alpha.NetworkContext{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				logger := log.FromContext(ctx)
				networkContext := obj.(*networkingv1alpha.NetworkContext)

				_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), networkContext.Spec.Location, r.LocationClassName)
				if err != nil {
					logger.Error(err, "failed to get location")
					return nil
				} else if !shouldProcess {
					return nil
				}

				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: networkContext.Namespace,
								Name:      networkContext.Spec.Network.Name,
							},
						},
						ClusterName: clusterName,
					},
				}
			})
		}).
		Named("network").
		Complete(r)
}
