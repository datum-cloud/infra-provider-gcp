package aws

import (
	"context"
	"encoding/base64"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/providers"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

type instanceReconciler struct{}
type reconcileContext struct {
	workloadDeployment  *computev1alpha.WorkloadDeployment
	instance            *computev1alpha.Instance
	location            *networkingv1alpha.Location
	providerConfigName  string
	interfaceSubnets    []string
	instanceProfileName string
	userData            []byte
}

type desiredInstanceResources struct {
	networkInterfaces []*awsec2v1beta1.NetworkInterface
	elasticIPs        []*awsec2v1beta1.EIP
	instance          *awsec2v1beta1.Instance
}

func NewInstanceReconciler() providers.Reconciler {
	return &instanceReconciler{}
}

func (b *instanceReconciler) Reconcile(
	ctx context.Context,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	projectName string,
	location networkingv1alpha.Location,
	workload computev1alpha.Workload,
	workloadDeployment computev1alpha.WorkloadDeployment,
	instance computev1alpha.Instance,
	cloudConfig *cloudinit.CloudConfig,
) (ctrl.Result, error) {

	cloudConfig.AWSRegion = location.Spec.Provider.AWS.Region
	cloudConfig.SecretsParameterName = "TODO" // Get from workload deployment scoped resource

	cloudConfig.Hostname = fmt.Sprintf("%s.%s.%s.cloud.datum-dns.net", instance.Name, instance.Namespace, projectName)
	butaneConfig, err := cloudConfig.ToButane()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to convert cloud config to ignition: %w", err)
	}

	ignitionConfigBytes, err := butaneConfig.ToIgnitionJSON()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to generate ignition config: %w", err)
	}

	reconcileContext := &reconcileContext{
		userData: ignitionConfigBytes,
	}

	desiredResources, err := b.collectDesiredResources(reconcileContext)
	if err != nil {
		return ctrl.Result{}, err
	}

	_ = desiredResources

	return ctrl.Result{}, nil
}

func (b *instanceReconciler) RegisterWatches(builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error {
	return nil
}

func (b *instanceReconciler) collectDesiredResources(
	reconcileContext *reconcileContext,
) (*desiredInstanceResources, error) {
	desiredResources := &desiredInstanceResources{}

	if err := b.collectNetworkInterfaceResources(reconcileContext, desiredResources); err != nil {
		return nil, err
	}

	awsInstance := &awsec2v1beta1.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("instance-%s", reconcileContext.instance.UID),
		},
		Spec: awsec2v1beta1.InstanceSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ProviderConfigReference: &crossplanecommonv1.Reference{
					Name: reconcileContext.providerConfigName,
				},
			},
			ForProvider: awsec2v1beta1.InstanceParameters{
				Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
				// TODO(jreese) wire in images and instance types
				AMI:          ptr.To("ami-003496bc8140c472c"),
				InstanceType: ptr.To("t3a.micro"),
				// TODO(jreese) remove
				KeyName:            ptr.To("jreese@datum.net"),
				IAMInstanceProfile: ptr.To(reconcileContext.instanceProfileName),
				UserDataBase64:     ptr.To(base64.StdEncoding.EncodeToString(reconcileContext.userData)),
			},
		},
	}

	for interfaceIndex := range reconcileContext.instance.Spec.NetworkInterfaces {
		awsInstance.Spec.ForProvider.NetworkInterface = append(awsInstance.Spec.ForProvider.NetworkInterface, awsec2v1beta1.InstanceNetworkInterfaceParameters{
			NetworkCardIndex: ptr.To(float64(0)),
			DeviceIndex:      ptr.To(float64(interfaceIndex)),
			NetworkInterfaceIDRef: &crossplanecommonv1.Reference{
				Name: fmt.Sprintf("instance-%s-net-%d", reconcileContext.instance.UID, interfaceIndex),
			},
		})
	}

	desiredResources.instance = awsInstance

	return desiredResources, nil
}

func (b *instanceReconciler) collectNetworkInterfaceResources(
	reconcileContext *reconcileContext,
	desiredResources *desiredInstanceResources,
) error {
	networkInterfaceCount := len(reconcileContext.instance.Spec.NetworkInterfaces)
	interfaceSubnetsCount := len(reconcileContext.interfaceSubnets)
	if networkInterfaceCount != interfaceSubnetsCount {
		return fmt.Errorf("incorrect network interface target subnet length. got %d expected %d", interfaceSubnetsCount, networkInterfaceCount)
	}

	for interfaceIndex := range reconcileContext.instance.Spec.NetworkInterfaces {
		subnetName := reconcileContext.interfaceSubnets[interfaceIndex]
		networkInterface := &awsec2v1beta1.NetworkInterface{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("instance-%s-net-%d", reconcileContext.instance.UID, interfaceIndex),
			},
			Spec: awsec2v1beta1.NetworkInterfaceSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: reconcileContext.providerConfigName,
					},
				},
				ForProvider: awsec2v1beta1.NetworkInterfaceParameters_2{
					Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
					SubnetIDRef: &crossplanecommonv1.Reference{
						Name: subnetName,
					},
					SecurityGroupRefs: []crossplanecommonv1.Reference{
						{
							Name: fmt.Sprintf("workloaddeployment-%s-net-%d", reconcileContext.workloadDeployment.UID, interfaceIndex),
						},
					},
				},
			},
		}

		desiredResources.networkInterfaces = append(desiredResources.networkInterfaces, networkInterface)

		if interfaceIndex == 0 {
			// TODO(jreese) Instead of going off of interfaceIndex, go off of settings
			// on the network interface to indicate if a public address should be allocated.
			publicIP := &awsec2v1beta1.EIP{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("instance-%s-net-%d-public-ip", reconcileContext.instance.UID, interfaceIndex),
				},
				Spec: awsec2v1beta1.EIPSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: reconcileContext.providerConfigName,
						},
					},
					ForProvider: awsec2v1beta1.EIPParameters{
						Region: ptr.To(reconcileContext.location.Spec.Provider.AWS.Region),
						Domain: ptr.To("vpc"),
						NetworkInterfaceRef: &crossplanecommonv1.Reference{
							Name: networkInterface.Name,
						},
					},
				},
			}

			desiredResources.elasticIPs = append(desiredResources.elasticIPs, publicIP)
		}
	}

	return nil
}
