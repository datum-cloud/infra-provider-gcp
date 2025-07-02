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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	logger := log.FromContext(ctx)

	cloudConfig.AWSRegion = location.Spec.Provider.AWS.Region
	cloudConfig.SecretsParameterName = "TODO" // Get from workload deployment scoped resource

	cloudConfig.Hostname = fmt.Sprintf("%s.%s.%s.cloud.datum-dns.net", instance.Name, instance.Namespace, projectName)

	var userData []byte

	if instance.Spec.Runtime.VirtualMachine != nil {
		// TODO(jreese) Test VM based instances!
		cloudConfigBytes, err := cloudConfig.Generate()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed generating cloud init user data: %w", err)
		}
		userData = append([]byte("#cloud-config\n\n"), cloudConfigBytes...)
	} else if instance.Spec.Runtime.Sandbox != nil {
		butaneConfig, err := cloudConfig.ToButane()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to convert cloud config to ignition: %w", err)
		}

		ignitionConfigBytes, err := butaneConfig.ToIgnitionJSON()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to generate ignition config: %w", err)
		}
		userData = ignitionConfigBytes
	}

	reconcileContext := &reconcileContext{
		userData: userData,
	}

	desiredResources, err := b.collectDesiredResources(reconcileContext)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create Network interfaces
	for _, desiredInterface := range desiredResources.networkInterfaces {
		networkInterface := desiredInterface.DeepCopy()
		result, err := controllerutil.CreateOrUpdate(ctx, downstreamClient, networkInterface, func() error {

			// TODO(jreese) add any necessary annotations/labels for use in watches
			// desiredInterface.Annotations = networkInterface.Annotations
			// desiredInterface.Labels = networkInterface.Labels

			// TODO(jreese) make sure we don't clobber spec values that crossplane
			// may be updating.
			networkInterface.Spec = desiredInterface.Spec

			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed creating ENI: %w", err)
		}

		logger.Info("processed ENI", "name", networkInterface.Name, "result", result)
	}

	/*
		if awsInstance.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
					logger.Info("AWS instance not ready yet")
					programmedCondition.Reason = "ProvisioningProviderInstance"
					programmedCondition.Message = "AWS instance is being provisioned"

					return ctrl.Result{}, nil
				}

				programmedCondition.Status = metav1.ConditionTrue
				programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammed
				programmedCondition.Message = "Instance has been programmed"
				instance.Status.Controller = &computev1alpha.InstanceControllerStatus{
					ObservedTemplateHash: instance.Spec.Controller.TemplateHash,
				}

				if len(instance.Status.NetworkInterfaces) == 0 {
					instance.Status.NetworkInterfaces = make([]computev1alpha.InstanceNetworkInterfaceStatus, len(instance.Spec.NetworkInterfaces))
				}

				for interfaceIndex := range instance.Spec.NetworkInterfaces {
					var networkInterface awsec2v1beta1.NetworkInterface
					networkInterfaceObjectKey := client.ObjectKey{
						Name: fmt.Sprintf("instance-%s-net-%d", instance.UID, interfaceIndex),
					}
					if err := downstreamClient.Get(ctx, networkInterfaceObjectKey, &networkInterface); client.IgnoreNotFound(err) != nil {
						return ctrl.Result{}, fmt.Errorf("failed fetching network interface: %w", err)
					}

					interfaceStatus := computev1alpha.InstanceNetworkInterfaceStatus{}
					interfaceStatus.Assignments.NetworkIP = networkInterface.Status.AtProvider.PrivateIP

					var publicIP awsec2v1beta1.EIP
					publicIPObjectKey := client.ObjectKey{
						Name: fmt.Sprintf("instance-%s-net-%d-public-ip", instance.UID, interfaceIndex),
					}
					if err := downstreamClient.Get(ctx, publicIPObjectKey, &publicIP); client.IgnoreNotFound(err) != nil {
						return ctrl.Result{}, fmt.Errorf("failed fetching public IP: %w", err)
					}

					if !publicIP.CreationTimestamp.IsZero() {
						interfaceStatus.Assignments.ExternalIP = publicIP.Status.AtProvider.PublicIP
					}

					instance.Status.NetworkInterfaces[interfaceIndex] = interfaceStatus
				}

				runningCondition := metav1.Condition{
					Type:               computev1alpha.InstanceRunning,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: instance.Generation,
					Reason:             computev1alpha.InstanceRunningReasonStopped,
					Message:            "Instance is not running",
				}

				if ptr.Deref(awsInstance.Status.AtProvider.InstanceState, "") == "running" {
					runningCondition.Status = metav1.ConditionTrue
					runningCondition.Reason = computev1alpha.InstanceRunningReasonRunning
					runningCondition.Message = "Instance is running"
				}

				if apimeta.SetStatusCondition(&instance.Status.Conditions, runningCondition) {
					if err := upstreamClient.Status().Update(ctx, instance); err != nil {
						return ctrl.Result{}, fmt.Errorf("failed to update instance status: %w", err)
					}
				}
	*/

	return ctrl.Result{}, nil
}

func (b *instanceReconciler) RegisterWatches(builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error {
	// TODO(jreese) add watches for at least the AWS Instance, consider it for
	// other resources if necessary to avoid backoffs.
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
