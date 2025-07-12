// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/controller/providers/aws"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// WorkloadDeploymentReconciler reconciles a WorkloadDeployment with the primary function of cleaning
// up the downstream resources when the workload is deleted.
type WorkloadDeploymentReconciler struct {
	mgr               mcmanager.Manager
	DownstreamCluster cluster.Cluster
	LocationClassName string
	Config            config.GCPProvider

	awsWorkloadDeploymentReconciler providers.WorkloadDeploymentReconciler
}

func (r *WorkloadDeploymentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var workloadDeployment computev1alpha.WorkloadDeployment
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &workloadDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't do anything if a location isn't set
	if workloadDeployment.Status.Location == nil {
		return ctrl.Result{}, nil
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *workloadDeployment.Status.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling workload deployment")
	defer logger.Info("reconcile complete")

	if !workloadDeployment.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&workloadDeployment, gcpInfraFinalizer) {
			return ctrl.Result{}, r.Finalize(ctx, cl.GetClient(), location, &workloadDeployment)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&workloadDeployment, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&workloadDeployment, gcpInfraFinalizer)
		if err := cl.GetClient().Update(ctx, &workloadDeployment); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
		req.ClusterName,
		cl.GetClient(),
		r.DownstreamCluster.GetClient(),
		r.Config.DownstreamResourceManagement.ManagedResourceLabels,
	)
	downstreamClient := downstreamStrategy.GetClient()

	var workload computev1alpha.Workload
	workloadObjectKey := client.ObjectKey{
		Namespace: workloadDeployment.Namespace,
		Name:      workloadDeployment.Spec.WorkloadRef.Name,
	}
	if err := cl.GetClient().Get(ctx, workloadObjectKey, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching workload: %w", err)
	}

	// Ensure that a downstream workload exists for the provider
	// "scope". In the case of AWS, that will be the ARN in the Location. For GCP,
	// it will be the project name.
	//
	// We do this because a single workload may span multiple GCP projects or
	// multiple AWS accounts, which each need their own workload scoped resources.
	shaString := generateProviderScopeHash(*location)
	downstreamWorkloadName := fmt.Sprintf("workload-%s-%s", workload.UID, shaString)

	var downstreamWorkload infrav1alpha1.ClusterDownstreamWorkload
	if err := downstreamClient.Get(ctx, client.ObjectKey{
		Name: downstreamWorkloadName,
	}, &downstreamWorkload); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching downstream workload: %w", err)
	}

	if downstreamWorkload.CreationTimestamp.IsZero() {
		downstreamWorkload = infrav1alpha1.ClusterDownstreamWorkload{
			ObjectMeta: metav1.ObjectMeta{
				Name: downstreamWorkloadName,
			},
			Spec: infrav1alpha1.ClusterDownstreamWorkloadSpec{
				UpstreamWorkloadRef: infrav1alpha1.UpstreamResourceRef{
					ClusterName: req.ClusterName,
					Namespace:   workload.Namespace,
					Name:        workload.Name,
				},
			},
		}

		if err := downstreamClient.Create(ctx, &downstreamWorkload); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating downstream workload: %w", err)
		}
	}

	// Ensure that the downstream workload deployment exists
	var downstreamWorkloadDeployment infrav1alpha1.ClusterDownstreamWorkloadDeployment
	if err := downstreamClient.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
	}, &downstreamWorkloadDeployment); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching downstream workload deployment: %w", err)
	}

	if downstreamWorkloadDeployment.CreationTimestamp.IsZero() {
		downstreamWorkloadDeployment = infrav1alpha1.ClusterDownstreamWorkloadDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
			},
			Spec: infrav1alpha1.ClusterDownstreamWorkloadDeploymentSpec{
				UpstreamWorkloadDeploymentRef: infrav1alpha1.UpstreamResourceRef{
					ClusterName: req.ClusterName,
					Namespace:   workloadDeployment.Namespace,
					Name:        workloadDeployment.Name,
				},
			},
		}

		if err := downstreamClient.Create(ctx, &downstreamWorkloadDeployment); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating downstream workload deployment: %w", err)
		}
	}

	aggregatedSecret, err := r.reconcileAggregatedSecret(ctx, cl.GetClient(), downstreamStrategy, &workloadDeployment, &downstreamWorkloadDeployment)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile aggregated secret: %w", err)
	}

	if location.Spec.Provider.AWS != nil {
		result, err := r.awsWorkloadDeploymentReconciler.Reconcile(
			ctx,
			cl.GetClient(),
			downstreamStrategy,
			downstreamClient,
			req.ClusterName,
			location,
			&workload,
			&workloadDeployment,
			&downstreamWorkloadDeployment,
			aggregatedSecret,
		)
		if err != nil {
			return result, err
		} else if result.RequeueAfter > 0 {
			return result, nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadDeploymentReconciler) Finalize(
	ctx context.Context,
	upstreamClient client.Client,
	location *networkingv1alpha.Location,
	workloadDeployment *computev1alpha.WorkloadDeployment,
) error {
	var downstreamWorkloadDeployment infrav1alpha1.ClusterDownstreamWorkloadDeployment
	if err := r.DownstreamCluster.GetClient().Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
	}, &downstreamWorkloadDeployment); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed fetching downstream workload deployment: %w", err)
	}

	if !downstreamWorkloadDeployment.CreationTimestamp.IsZero() {
		if location.Spec.Provider.AWS != nil {
			result, err := r.awsWorkloadDeploymentReconciler.Finalize(
				ctx,
				upstreamClient,
				r.DownstreamCluster,
				workloadDeployment,
				&downstreamWorkloadDeployment,
			)
			if err != nil {
				return fmt.Errorf("failed finalizing workload deployment: %w", err)
			} else if result == providers.FinalizeResultPending {
				return nil
			}
		}
	}

	if controllerutil.RemoveFinalizer(workloadDeployment, gcpInfraFinalizer) {
		if err := upstreamClient.Update(ctx, workloadDeployment); err != nil {
			return fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDeploymentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	r.awsWorkloadDeploymentReconciler = aws.NewWorkloadDeploymentReconciler(r.Config)

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.WorkloadDeployment{}).
		// TODO(jreese) this needs to watch the DownstreamCluster
		// Watches(&infrav1alpha1.ClusterDownstreamWorkloadDeployment{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		// 	return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
		// 		downstreamWorkloadDeployment := obj.(*infrav1alpha1.ClusterDownstreamWorkloadDeployment)
		// 		return []mcreconcile.Request{
		// 			{
		// 				Request: reconcile.Request{
		// 					NamespacedName: types.NamespacedName{
		// 						Namespace: downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.Namespace,
		// 						Name:      downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.Name,
		// 					},
		// 				},
		// 				ClusterName: downstreamWorkloadDeployment.Spec.WorkloadDeploymentRef.UpstreamClusterName,
		// 			},
		// 		}
		// 	})
		// }).
		Named("workloaddeployment").
		Complete(r)
}

// reconcileAggregatedSecret aggregates secret data into a single secret which
// can be used to populate provider secret stores. A secret is created per
// WorkloadDeployment instead of per Workload for ease of use.
func (r *WorkloadDeploymentReconciler) reconcileAggregatedSecret(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	downstreamWorklaodDeployment *infrav1alpha1.ClusterDownstreamWorkloadDeployment,
) (*corev1.Secret, error) {
	var objectKeys []client.ObjectKey
	for _, volume := range workloadDeployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			objectKeys = append(objectKeys, client.ObjectKey{
				Namespace: workloadDeployment.Namespace,
				Name:      volume.Secret.SecretName,
			})
		}
	}

	if len(objectKeys) == 0 {
		return nil, nil
	}

	// Aggregate secret data into one value by creating a map of secret names
	// to content. This will allow for mounting of keys into volumes or secrets
	// as expected.
	secretData := map[string]map[string][]byte{}
	for _, objectKey := range objectKeys {
		var k8ssecret corev1.Secret
		if err := upstreamClient.Get(ctx, objectKey, &k8ssecret); err != nil {
			return nil, fmt.Errorf("failed fetching secret: %w", err)
		}

		secretData[k8ssecret.Name] = k8ssecret.Data
	}

	secretBytes, err := json.Marshal(secretData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal secret data")
	}

	downstreamNamespaceName, err := downstreamStrategy.GetDownstreamNamespaceName(ctx, workloadDeployment)
	if err != nil {
		return nil, fmt.Errorf("failed to get downstream namespace name: %w", err)
	}

	aggregatedK8sSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: downstreamNamespaceName,
			Name:      fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, downstreamStrategy.GetClient(), aggregatedK8sSecret, func() error {
		if err := controllerutil.SetControllerReference(downstreamWorklaodDeployment, aggregatedK8sSecret, r.DownstreamCluster.GetScheme()); err != nil {
			return fmt.Errorf("failed to set controller reference on aggregated k8s secret: %w", err)
		}

		aggregatedK8sSecret.Data = map[string][]byte{
			"secretData": secretBytes,
		}

		return nil
	})

	return aggregatedK8sSecret, err
}

// generateProviderScopeHash generates a SHA256 hash based on the provider scope
// For AWS, it uses the ARN; for GCP, it uses the project ID
func generateProviderScopeHash(location networkingv1alpha.Location) string {
	var scopeValue string

	if location.Spec.Provider.AWS != nil {
		scopeValue = location.Spec.Provider.AWS.RoleARN
	} else if location.Spec.Provider.GCP != nil {
		scopeValue = location.Spec.Provider.GCP.ProjectID
	}

	hash := sha256.Sum256([]byte(scopeValue))
	return fmt.Sprintf("%x", hash)
}
