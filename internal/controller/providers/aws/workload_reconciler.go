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
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/util/text"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

type workloadReconciler struct{}
type workloadReconcileContext struct {
	providerConfigName string
	location           *networkingv1alpha.Location
	workload           *computev1alpha.Workload
}

type desiredWorkloadResources struct {
	instanceProfileIAMRole awsiamv1beta1.Role
	instanceProfile        awsiamv1beta1.InstanceProfile
}

func NewWorkloadReconciler() providers.WorkloadReconciler {
	return &workloadReconciler{}
}

func (b *workloadReconciler) Reconcile(
	ctx context.Context,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	clusterName string,
	location networkingv1alpha.Location,
	workload computev1alpha.Workload,

) (ctrl.Result, error) {
	return ctrl.Result{}, nil
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
											"Resource": "arn:aws:ssm:*:*:parameter/workload-%s"
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
