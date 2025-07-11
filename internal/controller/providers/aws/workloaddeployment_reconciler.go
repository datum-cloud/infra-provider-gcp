package aws

import (
	"context"
	"fmt"
	"strings"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	awsssmv1beta1 "github.com/upbound/provider-aws/apis/ssm/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

type workloadDeploymentReconciler struct {
	config config.GCPProvider
}
type workloadDeploymentReconcileContext struct {
	providerConfigName  string
	location            *networkingv1alpha.Location
	workload            *computev1alpha.Workload
	workloadDeployment  *computev1alpha.WorkloadDeployment
	aggregatedK8sSecret *corev1.Secret
	interfaceVPCs       []string
}

type desiredWorkloadDeploymentResources struct {
	secretsParameter   *awsssmv1beta1.Parameter
	securityGroups     []awsec2v1beta1.SecurityGroup
	securityGroupRules []awsec2v1beta1.SecurityGroupRule
}

func NewWorkloadDeploymentReconciler(config config.GCPProvider) providers.WorkloadDeploymentReconciler {
	return &workloadDeploymentReconciler{
		config: config,
	}
}

func (b *workloadDeploymentReconciler) Reconcile(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	clusterName string,
	location networkingv1alpha.Location,
	workload computev1alpha.Workload,
	workloadDeployment computev1alpha.WorkloadDeployment,
	downstreamWorkloadDeployment infrav1alpha1.ClusterDownstreamWorkloadDeployment,
	aggregatedK8sSecret *corev1.Secret,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Resolve VPCs for each network interface
	interfaceVPCs := make([]string, len(workloadDeployment.Spec.Template.Spec.NetworkInterfaces))
	for interfaceIndex := range workloadDeployment.Spec.Template.Spec.NetworkInterfaces {
		// 1. Fetch the NetworkBinding for the WorkloadDeployment
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: workloadDeployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", workloadDeployment.Name, interfaceIndex),
		}
		if err := upstreamClient.Get(ctx, networkBindingObjectKey, &networkBinding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching network binding %s: %w", networkBindingObjectKey.Name, err)
		}

		// 2. Fetch the NetworkContext referenced by the NetworkBinding
		var networkContext networkingv1alpha.NetworkContext
		networkContextObjectKey := client.ObjectKey{
			Namespace: networkBinding.Status.NetworkContextRef.Namespace,
			Name:      networkBinding.Status.NetworkContextRef.Name,
		}
		if err := upstreamClient.Get(ctx, networkContextObjectKey, &networkContext); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching network context %s/%s: %w", networkContextObjectKey.Namespace, networkContextObjectKey.Name, err)
		}

		// 4. Use the network context UID to create the downstream vpc reference
		interfaceVPCs[interfaceIndex] = fmt.Sprintf("networkcontext-%s", networkContext.UID)
	}

	reconcileContext := &workloadDeploymentReconcileContext{
		providerConfigName:  b.config.GetProviderConfigName("AWS", clusterName),
		location:            &location,
		workload:            &workload,
		workloadDeployment:  &workloadDeployment,
		aggregatedK8sSecret: aggregatedK8sSecret,
		interfaceVPCs:       interfaceVPCs,
	}

	desiredResources, err := b.collectDesiredResources(reconcileContext)
	if err != nil {
		return ctrl.Result{}, err
	}

	if desiredResources.secretsParameter != nil {
		result, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, desiredResources.secretsParameter, clusterName, &workloadDeployment, nil)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or patch ssm parameter %s: %w", desiredResources.secretsParameter.Name, err)
		}

		logger.Info("processed SSM Parameter", "result", result)
	}

	for _, securityGroup := range desiredResources.securityGroups {
		_, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, &securityGroup, clusterName, &workloadDeployment, nil)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or patch security group %s: %w", securityGroup.Name, err)
		}
	}

	for _, securityGroupRule := range desiredResources.securityGroupRules {
		_, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, &securityGroupRule, clusterName, &workloadDeployment, nil)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or patch security group rule %s: %w", securityGroupRule.Name, err)
		}
	}

	return ctrl.Result{}, nil
}

func (b *workloadDeploymentReconciler) RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error {
	return nil
}

func (b *workloadDeploymentReconciler) collectDesiredResources(
	reconcileContext *workloadDeploymentReconcileContext,
) (*desiredWorkloadDeploymentResources, error) {
	desiredResources := &desiredWorkloadDeploymentResources{}

	if reconcileContext.aggregatedK8sSecret != nil {
		desiredResources.secretsParameter = &awsssmv1beta1.Parameter{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("workload-%s-%s", reconcileContext.workload.UID, reconcileContext.workloadDeployment.UID),
			},
			Spec: awsssmv1beta1.ParameterSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: reconcileContext.providerConfigName,
					},
				},
				ForProvider: awsssmv1beta1.ParameterParameters_2{
					Region:    ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
					Overwrite: ptr.To(true),
					Type:      ptr.To("SecureString"),
					ValueSecretRef: &crossplanecommonv1.SecretKeySelector{
						SecretReference: crossplanecommonv1.SecretReference{
							Namespace: reconcileContext.aggregatedK8sSecret.Namespace,
							Name:      reconcileContext.aggregatedK8sSecret.Name,
						},
						Key: "secretData",
					},
				},
			},
		}
	}

	if err := b.collectSecurityGroupResources(reconcileContext, desiredResources); err != nil {
		return nil, err
	}

	return desiredResources, nil
}

func (b *workloadDeploymentReconciler) collectSecurityGroupResources(
	reconcileContext *workloadDeploymentReconcileContext,
	desiredResources *desiredWorkloadDeploymentResources,
) error {
	networkInterfaceCount := len(reconcileContext.workloadDeployment.Spec.Template.Spec.NetworkInterfaces)
	interfaceVPCCount := len(reconcileContext.interfaceVPCs)
	if networkInterfaceCount != interfaceVPCCount {
		return fmt.Errorf("incorrect network interface target vpc length. got %d expected %d", interfaceVPCCount, networkInterfaceCount)
	}

	for interfaceIndex, networkInterface := range reconcileContext.workloadDeployment.Spec.Template.Spec.NetworkInterfaces {

		vpcName := reconcileContext.interfaceVPCs[interfaceIndex]

		instanceSecurityGroup := awsec2v1beta1.SecurityGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("workloaddeployment-%s-net-%d", reconcileContext.workloadDeployment.UID, interfaceIndex),
			},
			Spec: awsec2v1beta1.SecurityGroupSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: reconcileContext.providerConfigName,
					},
				},
				ForProvider: awsec2v1beta1.SecurityGroupParameters_2{
					Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
					VPCIDRef: &crossplanecommonv1.Reference{
						Name: vpcName,
					},
				},
			},
		}

		desiredResources.securityGroups = append(desiredResources.securityGroups, instanceSecurityGroup)

		// TODO(jreese) This should probably move into the VPC level, and have a
		// default security group that instances can be added to.
		securityGroupEgressRule := awsec2v1beta1.SecurityGroupRule{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("workloaddeployment-%s-net-%d-egress", reconcileContext.workloadDeployment.UID, interfaceIndex),
			},
			Spec: awsec2v1beta1.SecurityGroupRuleSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: reconcileContext.providerConfigName,
					},
				},
				ForProvider: awsec2v1beta1.SecurityGroupRuleParameters_2{
					Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
					SecurityGroupIDRef: &crossplanecommonv1.Reference{
						Name: fmt.Sprintf("workloaddeployment-%s-net-%d", reconcileContext.workloadDeployment.UID, interfaceIndex),
					},
					Type:     ptr.To("egress"),
					Protocol: ptr.To("all"),
					FromPort: ptr.To(float64(0)),
					ToPort:   ptr.To(float64(65535)),
					CidrBlocks: []*string{
						ptr.To("0.0.0.0/0"),
					},
				},
			},
		}

		desiredResources.securityGroupRules = append(desiredResources.securityGroupRules, securityGroupEgressRule)

		interfacePolicy := networkInterface.NetworkPolicy
		if interfacePolicy == nil {
			continue
		}

		for ruleIndex, ingressRule := range interfacePolicy.Ingress {
			for portIndex, port := range ingressRule.Ports {
				if port.Port == nil {
					continue
				}

				ipProtocol := "tcp"
				if port.Protocol != nil {
					ipProtocol = strings.ToLower(string(*port.Protocol))
				}

				securityGroupRule := awsec2v1beta1.SecurityGroupRule{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("workloaddeployment-%s-net-%d-%d-%d", reconcileContext.workloadDeployment.UID, interfaceIndex, ruleIndex, portIndex),
					},
					Spec: awsec2v1beta1.SecurityGroupRuleSpec{
						ResourceSpec: crossplanecommonv1.ResourceSpec{
							ProviderConfigReference: &crossplanecommonv1.Reference{
								Name: reconcileContext.providerConfigName,
							},
						},
						ForProvider: awsec2v1beta1.SecurityGroupRuleParameters_2{
							Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
							SecurityGroupIDRef: &crossplanecommonv1.Reference{
								Name: fmt.Sprintf("workloaddeployment-%s-net-%d", reconcileContext.workloadDeployment.UID, interfaceIndex),
							},
							Type:     ptr.To("ingress"),
							Protocol: ptr.To(ipProtocol),
							FromPort: ptr.To(float64(port.Port.IntValue())),
							ToPort:   ptr.To(float64(port.Port.IntValue())),
						},
					},
				}

				if port.EndPort != nil {
					securityGroupRule.Spec.ForProvider.ToPort = ptr.To(float64(*port.EndPort))
				}

				for _, peer := range ingressRule.From {
					if peer.IPBlock != nil {
						securityGroupRule.Spec.ForProvider.CidrBlocks = append(securityGroupRule.Spec.ForProvider.CidrBlocks, ptr.To(peer.IPBlock.CIDR))
					}
				}

				desiredResources.securityGroupRules = append(desiredResources.securityGroupRules, securityGroupRule)

			}
		}
	}

	return nil
}
