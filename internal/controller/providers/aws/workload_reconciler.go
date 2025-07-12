package aws

import (
	"context"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	awsiamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/util/text"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

type workloadReconciler struct {
	config config.GCPProvider
}
type workloadReconcileContext struct {
	providerConfigName string
	workload           *computev1alpha.Workload
}

type desiredWorkloadResources struct {
	instanceProfileIAMRole awsiamv1beta1.Role
	instanceProfile        awsiamv1beta1.InstanceProfile
}

func NewWorkloadReconciler(config config.GCPProvider) providers.WorkloadReconciler {
	return &workloadReconciler{
		config: config,
	}
}

func (b *workloadReconciler) Reconcile(
	ctx context.Context,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	clusterName string,
	workload *computev1alpha.Workload,
	downstreamWorkload *infrav1alpha1.ClusterDownstreamWorkload,
) (ctrl.Result, error) {

	reconcileContext := &workloadReconcileContext{
		// TODO(jreese) pass GCP project / AWS Role ARN info into the ClusterDownstreamWorkload
		// So they can be used to locate the correct provider config.
		providerConfigName: b.config.GetProviderConfigName("AWS", clusterName),
		workload:           workload,
	}

	desiredResources, err := b.collectDesiredResources(reconcileContext)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update the IAM Role for the Instance Profile
	if controllerutil.SetControllerReference(downstreamWorkload, &desiredResources.instanceProfileIAMRole, downstreamClient.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set controller owner on IAM Role for instance profile: %w", err)
	}
	_, err = downstreamclient.CreateOrPatch(ctx, downstreamClient, &desiredResources.instanceProfileIAMRole, "__downstream__", workload, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or patch IAM role for instance profile %s: %w", desiredResources.instanceProfileIAMRole.Name, err)
	}

	// Update the Instance Profile
	if controllerutil.SetControllerReference(downstreamWorkload, &desiredResources.instanceProfile, downstreamClient.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set controller owner for instance profile: %w", err)
	}
	_, err = downstreamclient.CreateOrPatch(ctx, downstreamClient, &desiredResources.instanceProfile, "__downstream__", workload, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or patch instance profile %s: %w", desiredResources.instanceProfileIAMRole.Name, err)
	}

	return ctrl.Result{}, nil
}

func (b *workloadReconciler) Finalize(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamCluster cluster.Cluster,
	workload *computev1alpha.Workload,
	downstreamWorkload *infrav1alpha1.ClusterDownstreamWorkload,
) (providers.FinalizeResult, error) {
	logger := log.FromContext(ctx)

	if dt := downstreamWorkload.DeletionTimestamp; !dt.IsZero() {
		// Not done cleaning up owned resources.
		return providers.FinalizeResultPending, nil
	}

	logger.Info("deleting downstream workload")

	if err := downstreamCluster.GetClient().Delete(ctx, downstreamWorkload, &client.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
	}); err != nil {
		return providers.FinalizeResultError, fmt.Errorf("failed deleting downstream workload: %w", err)
	}

	return providers.FinalizeResultPending, nil
}

func (b *workloadReconciler) RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error {
	return nil
}

func (b *workloadReconciler) collectDesiredResources(
	reconcileContext *workloadReconcileContext,
) (*desiredWorkloadResources, error) {
	desiredResources := &desiredWorkloadResources{}

	desiredResources.instanceProfileIAMRole = awsiamv1beta1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("workload-%s", reconcileContext.workload.UID),
		},
		Spec: awsiamv1beta1.RoleSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: reconcileContext.providerConfigName,
				},
			},
			ForProvider: awsiamv1beta1.RoleParameters{
				AssumeRolePolicy: ptr.To(text.Dedent(`
					{
						"Version": "2012-10-17",
						"Statement": [
							{
								"Effect": "Allow",
								"Principal": {
									"Service": "ec2.amazonaws.com"
								},
								"Action": "sts:AssumeRole"
							}
						]
					}
				`)),
				InlinePolicy: []awsiamv1beta1.InlinePolicyParameters{
					{
						Name: ptr.To("ssm-read"),
						Policy: ptr.To(fmt.Sprintf(text.Dedent(`
								{
									"Version": "2012-10-17",
									"Statement": [
										{
											"Effect": "Allow",
											"Action": "ssm:GetParameter",
											"Resource": "arn:aws:ssm:*:*:parameter/workload-%s-*"
										}
									]
								}
							`),
							reconcileContext.workload.UID,
						)),
					},
				},
			},
		},
	}

	desiredResources.instanceProfile = awsiamv1beta1.InstanceProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("workload-%s", reconcileContext.workload.UID),
		},
		Spec: awsiamv1beta1.InstanceProfileSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: reconcileContext.providerConfigName,
				},
			},
			ForProvider: awsiamv1beta1.InstanceProfileParameters{
				RoleRef: &crossplanecommonv1.Reference{
					Name: desiredResources.instanceProfileIAMRole.Name,
				},
			},
		},
	}

	return desiredResources, nil
}
