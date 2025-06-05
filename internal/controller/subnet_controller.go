package controller

import (
	"context"
	"errors"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// SubnetReconciler reconciles Instances and manages their intended state in
// GCP
type SubnetReconciler struct {
	mgr               mcmanager.Manager
	finalizers        finalizer.Finalizers
	Config            config.GCPProvider
	LocationClassName string
	DownstreamCluster cluster.Cluster
}

func (r *SubnetReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var subnet networkingv1alpha.Subnet
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), subnet.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &subnet)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(errResourceIsDeleting) {
			logger.Info("subnet still has resources in GCP, waiting until removal")
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err = cl.GetClient().Update(ctx, &subnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !subnet.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if apimeta.IsStatusConditionTrue(subnet.Status.Conditions, networkingv1alpha.SubnetProgrammed) {
		logger.Info("subnet is already programmed")
		return ctrl.Result{}, nil
	}

	programmedCondition := metav1.Condition{
		Type:               networkingv1alpha.SubnetProgrammed,
		Status:             metav1.ConditionFalse,
		Reason:             networkingv1alpha.SubnetProgrammedReasonNotProgrammed,
		ObservedGeneration: subnet.Generation,
		Message:            "Subnet has not been programmed",
	}

	defer func() {
		if apimeta.SetStatusCondition(&subnet.Status.Conditions, programmedCondition) {
			err = errors.Join(err, cl.GetClient().Status().Update(ctx, &subnet))
		}
	}()

	logger.Info("reconciling subnet")
	defer logger.Info("reconcile complete")

	if !apimeta.IsStatusConditionTrue(subnet.Status.Conditions, networkingv1alpha.SubnetAllocated) {
		logger.Info("subnet has not been allocated a prefix")
		return ctrl.Result{}, nil
	}

	// Get the context the subnet is in so we can obtain the network
	var networkContext networkingv1alpha.NetworkContext
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Namespace: subnet.Namespace, Name: subnet.Spec.NetworkContext.Name}, &networkContext); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get network context: %w", err)
	}

	var network networkingv1alpha.Network
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Namespace: networkContext.Namespace, Name: networkContext.Spec.Network.Name}, &network); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get network: %w", err)
	}

	downstreamSubnet := &gcpcomputev1beta2.Subnetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("subnet-%s", subnet.UID),
		},
	}

	if err := r.DownstreamCluster.GetClient().Get(ctx, client.ObjectKey{Name: downstreamSubnet.Name}, downstreamSubnet); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get downstream subnet: %w", err)
	}

	if downstreamSubnet.CreationTimestamp.IsZero() {
		downstreamSubnet = &gcpcomputev1beta2.Subnetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("subnet-%s", subnet.UID),
				Annotations: map[string]string{
					downstreamclient.UpstreamOwnerName:        subnet.Name,
					downstreamclient.UpstreamOwnerNamespace:   subnet.Namespace,
					downstreamclient.UpstreamOwnerClusterName: req.ClusterName,
				},
			},
			Spec: gcpcomputev1beta2.SubnetworkSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: r.Config.GetProviderConfigName(req.ClusterName),
					},
				},
				ForProvider: gcpcomputev1beta2.SubnetworkParameters_2{
					IPCidrRange: ptr.To(fmt.Sprintf("%s/%d", *subnet.Status.StartAddress, *subnet.Status.PrefixLength)),
					NetworkRef: &crossplanecommonv1.Reference{
						Name: fmt.Sprintf("network-%s", network.UID),
					},
					Project:   ptr.To(location.Spec.Provider.GCP.ProjectID),
					Region:    ptr.To(location.Spec.Provider.GCP.Region),
					Purpose:   ptr.To("PRIVATE"),
					StackType: ptr.To("IPV4_ONLY"),
				},
			},
		}

		if err := r.DownstreamCluster.GetClient().Create(ctx, downstreamSubnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create downstream subnet: %w", err)
		}
	}

	if downstreamSubnet.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("GCP subnet not ready yet")

		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = networkingv1alpha.SubnetProgrammedReasonProgrammingInProgress
		programmedCondition.Message = "Subnet is being programmed."

		return ctrl.Result{}, nil
	}

	programmedCondition.Status = metav1.ConditionTrue
	programmedCondition.Reason = networkingv1alpha.SubnetProgrammedReasonProgrammed
	programmedCondition.Message = "Subnet is ready."

	return ctrl.Result{}, nil
}

func (r *SubnetReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {
	subnet := obj.(*networkingv1alpha.Subnet)

	downstreamSubnet := &gcpcomputev1beta2.Subnetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("subnet-%s", subnet.UID),
		},
	}

	if err := r.DownstreamCluster.GetClient().Delete(ctx, downstreamSubnet); client.IgnoreNotFound(err) != nil {
		return finalizer.Result{}, fmt.Errorf("failed to delete downstream subnet: %w", err)
	}

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubnetReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpInfraFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	return mcbuilder.ControllerManagedBy(mgr).
		For(&networkingv1alpha.Subnet{}).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpcomputev1beta2.Subnetwork{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*gcpcomputev1beta2.Subnetwork, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, subnet *gcpcomputev1beta2.Subnetwork) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := subnet.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := subnet.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := subnet.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("GCP subnet is missing upstream ownership metadata")
					return nil
				}

				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: upstreamNamespace,
								Name:      upstreamName,
							},
						},
						ClusterName: upstreamClusterName,
					},
				}
			})
		})).
		Named("subnet").
		Complete(r)
}
