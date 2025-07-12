package aws

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/controller/providers"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

//go:embed bootstrap/populate_secrets_from_ssm.sh
var awsPopulateSecretsScript string

type instanceReconciler struct {
	config config.GCPProvider
}
type instanceReconcileContext struct {
	providerConfigName  string
	location            *networkingv1alpha.Location
	workloadDeployment  *computev1alpha.WorkloadDeployment
	instance            *computev1alpha.Instance
	interfaceSubnets    []string
	instanceProfileName string
	userData            []byte
	downstreamClient    client.Client
	clusterName         string
}

type desiredInstanceResources struct {
	networkInterfaces []*awsec2v1beta1.NetworkInterface
	elasticIPs        []*awsec2v1beta1.EIP
	instance          *awsec2v1beta1.Instance
}

func NewInstanceReconciler(config config.GCPProvider) providers.InstanceReconciler {
	return &instanceReconciler{
		config: config,
	}
}

func (b *instanceReconciler) Reconcile(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	clusterName string,
	location *networkingv1alpha.Location,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	downstreamInstance *infrav1alpha1.ClusterDownstreamInstance,
	cloudConfig *cloudinit.CloudConfig,
	hasAggregatedSecret bool,
	programmedCondition *metav1.Condition,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	projectName := strings.TrimPrefix(clusterName, "/")

	cloudConfig.AWSRegion = location.Spec.Provider.AWS.Region
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

		if hasAggregatedSecret {
			// TODO(jreese) obtain this value from the ClusterDownstreamWorkloadDeployment
			cloudConfig.SecretsParameterName = fmt.Sprintf("workload-%s-%s", workload.UID, workloadDeployment.UID)

			cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
				Encoding:    "b64",
				Content:     base64.StdEncoding.EncodeToString([]byte(awsPopulateSecretsScript)),
				Owner:       "root:root",
				Path:        "/etc/secrets/populate_secrets_from_ssm.sh",
				Permissions: "0700",
			})
		}

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

	// Resolve subnets for each network interface
	interfaceSubnets := make([]string, len(instance.Spec.NetworkInterfaces))
	for interfaceIndex := range instance.Spec.NetworkInterfaces {
		// 1. Fetch the NetworkBinding for the WorkloadDeployment
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: instance.Namespace,
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

		// 3. Fetch the Subnet in the same namespace as the NetworkContext
		var subnet networkingv1alpha.Subnet
		subnetObjectKey := client.ObjectKey{
			Namespace: networkContext.Namespace,
			Name:      fmt.Sprintf("%s-0", networkContext.Name),
		}
		if err := upstreamClient.Get(ctx, subnetObjectKey, &subnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching subnet %s/%s: %w", subnetObjectKey.Namespace, subnetObjectKey.Name, err)
		}

		// 4. Use the subnet UID to create the downstream subnet reference
		interfaceSubnets[interfaceIndex] = fmt.Sprintf("subnet-%s", subnet.UID)
	}

	reconcileContext := &instanceReconcileContext{
		providerConfigName:  b.config.GetProviderConfigName("AWS", clusterName),
		location:            location,
		workloadDeployment:  workloadDeployment,
		instance:            instance,
		interfaceSubnets:    interfaceSubnets,
		instanceProfileName: fmt.Sprintf("workload-%s", workload.UID),
		userData:            userData,
		downstreamClient:    downstreamClient,
		clusterName:         clusterName,
	}

	desiredResources, err := b.collectDesiredResources(reconcileContext)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, networkInterface := range desiredResources.networkInterfaces {
		if controllerutil.SetControllerReference(downstreamInstance, networkInterface, downstreamClient.Scheme()); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set controller owner on network interface: %w", err)
		}
		_, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, networkInterface, clusterName, instance, nil)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or patch network interface %s: %w", networkInterface.Name, err)
		}
	}

	for _, elasticIP := range desiredResources.elasticIPs {
		if controllerutil.SetControllerReference(downstreamInstance, elasticIP, downstreamClient.Scheme()); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set controller owner on elastic IP: %w", err)
		}
		_, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, elasticIP, clusterName, instance, nil)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or patch elastic IP %s: %w", elasticIP.Name, err)
		}
	}

	if controllerutil.SetControllerReference(downstreamInstance, desiredResources.instance, downstreamClient.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set controller owner on instance: %w", err)
	}
	result, err := downstreamclient.CreateOrPatch(ctx, downstreamClient, desiredResources.instance, clusterName, instance, func(observed, desired *awsec2v1beta1.Instance) error {
		for interfaceIndex, networkInterface := range observed.Spec.ForProvider.NetworkInterface {
			desired.Spec.ForProvider.NetworkInterface[interfaceIndex].NetworkInterfaceID = networkInterface.NetworkInterfaceID
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or patch instance %s: %w", desiredResources.instance.Name, err)
	}

	logger.Info("processed AWS instance", "result", result)

	if desiredResources.instance.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
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

	if ptr.Deref(desiredResources.instance.Status.AtProvider.InstanceState, "") == "running" {
		runningCondition.Status = metav1.ConditionTrue
		runningCondition.Reason = computev1alpha.InstanceRunningReasonRunning
		runningCondition.Message = "Instance is running"
	}

	if apimeta.SetStatusCondition(&instance.Status.Conditions, runningCondition) || true {
		if err := upstreamClient.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update instance status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (b *instanceReconciler) Finalize(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamCluster cluster.Cluster,
	downstreamInstance *infrav1alpha1.ClusterDownstreamInstance,
) (providers.FinalizeResult, error) {
	logger := log.FromContext(ctx)

	if dt := downstreamInstance.DeletionTimestamp; !dt.IsZero() {
		// Not done cleaning up owned resources.
		return providers.FinalizeResultPending, nil
	}

	logger.Info("deleting downstream instance")

	if err := downstreamCluster.GetClient().Delete(ctx, downstreamInstance, &client.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
	}); err != nil {
		return providers.FinalizeResultError, fmt.Errorf("failed deleting downstream instance: %w", err)
	}

	return providers.FinalizeResultPending, nil
}

func (b *instanceReconciler) RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error {
	builder.
		WatchesRawSource(datumsource.MustNewClusterSource(downstreamCluster, &awsec2v1beta1.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*awsec2v1beta1.Instance, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, instance *awsec2v1beta1.Instance) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := instance.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := instance.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := instance.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("AWS instance is missing upstream ownership metadata")
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
		WatchesRawSource(datumsource.MustNewClusterSource(downstreamCluster, &infrav1alpha1.ClusterDownstreamInstance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*infrav1alpha1.ClusterDownstreamInstance, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj *infrav1alpha1.ClusterDownstreamInstance) []mcreconcile.Request {
				return []mcreconcile.Request{
					{
						ClusterName: obj.Spec.UpstreamInstanceRef.ClusterName,
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: obj.Spec.UpstreamInstanceRef.Namespace,
								Name:      obj.Spec.UpstreamInstanceRef.Name,
							},
						},
					},
				}
			})
		}))

	return nil
}

func (b *instanceReconciler) collectDesiredResources(
	reconcileContext *instanceReconcileContext,
) (*desiredInstanceResources, error) {
	desiredResources := &desiredInstanceResources{}

	if err := b.collectNetworkInterfaceResources(reconcileContext, desiredResources); err != nil {
		return nil, err
	}

	desiredResources.instance = &awsec2v1beta1.Instance{
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
		desiredResources.instance.Spec.ForProvider.NetworkInterface = append(desiredResources.instance.Spec.ForProvider.NetworkInterface, awsec2v1beta1.InstanceNetworkInterfaceParameters{
			NetworkCardIndex: ptr.To(float64(0)),
			DeviceIndex:      ptr.To(float64(interfaceIndex)),
			NetworkInterfaceIDRef: &crossplanecommonv1.Reference{
				Name: fmt.Sprintf("instance-%s-net-%d", reconcileContext.instance.UID, interfaceIndex),
			},
		})
	}

	return desiredResources, nil
}

func (b *instanceReconciler) collectNetworkInterfaceResources(
	reconcileContext *instanceReconcileContext,
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
