package controller

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"
	"strconv"
	"strings"
	"time"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	crossplanemeta "github.com/crossplane/crossplane-runtime/pkg/meta"
	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	awsiamv1beta1 "github.com/upbound/provider-aws/apis/iam/v1beta1"
	awsssmv1beta1 "github.com/upbound/provider-aws/apis/ssm/v1beta1"
	gcpcloudplatformv1beta1 "github.com/upbound/provider-gcp/apis/cloudplatform/v1beta1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	gcpsecretmanagerv1beta1 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta1"
	gcpsecretmanagerv1beta2 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
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

	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	datumhandler "go.datum.net/infra-provider-gcp/internal/handler"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	"go.datum.net/infra-provider-gcp/internal/util/text"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const crossplaneFinalizer = "finalizer.managedresource.crossplane.io"

var errResourceIsDeleting = errors.New("resource is deleting")

//go:embed cloudinit/populate_secrets.py
var gcpPopulateSecretsScript string

//go:embed cloudinit/populate_secrets_from_ssm.sh
var awsPopulateSecretsScript string

// InstanceReconciler reconciles Instances and manages their intended state in
// GCP
type InstanceReconciler struct {
	mgr               mcmanager.Manager
	Config            config.GCPProvider
	LocationClassName string
	DownstreamCluster cluster.Cluster
}

func (r *InstanceReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var instance computev1alpha.Instance
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Don't do anything if a location isn't set
	if instance.Spec.Location == nil {
		return ctrl.Result{}, nil
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *instance.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling instance")
	defer logger.Info("reconcile complete")

	if !instance.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&instance, gcpInfraFinalizer) {
			return ctrl.Result{}, r.Finalize(ctx, cl.GetClient(), &instance)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&instance, gcpInfraFinalizer) {
		controllerutil.AddFinalizer(&instance, gcpInfraFinalizer)
		if err := cl.GetClient().Update(ctx, &instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, waiting until they are removed", "scheduling_gates", instance.Spec.Controller.SchedulingGates)
		return ctrl.Result{}, nil
	}

	workloadDeploymentRef := metav1.GetControllerOf(&instance)
	if workloadDeploymentRef == nil {
		return ctrl.Result{}, fmt.Errorf("instance is not owned by a workload deployment")
	}

	// Load the WorkloadDeployment and Workload for the instance
	var workloadDeployment computev1alpha.WorkloadDeployment
	workloadDeploymentObjectKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      workloadDeploymentRef.Name,
	}
	if err := cl.GetClient().Get(ctx, workloadDeploymentObjectKey, &workloadDeployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching workload deployment: %w", err)
	}

	workloadRef := metav1.GetControllerOf(&workloadDeployment)
	if workloadRef == nil {
		return ctrl.Result{}, fmt.Errorf("workload deployment is not owned by a workload")
	}

	var workload computev1alpha.Workload
	workloadObjectKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      workloadRef.Name,
	}
	if err := cl.GetClient().Get(ctx, workloadObjectKey, &workload); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching workload: %w", err)
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
		req.ClusterName,
		cl.GetClient(),
		r.DownstreamCluster.GetClient(),
		r.Config.DownstreamResourceManagement.ManagedResourceLabels,
	)
	downstreamClient := downstreamStrategy.GetClient()

	runtime := instance.Spec.Runtime
	if runtime.Sandbox != nil {
		return r.reconcileSandboxRuntimeInstance(
			ctx,
			req.ClusterName,
			*location,
			cl.GetClient(),
			downstreamStrategy,
			downstreamClient,
			&workload,
			&workloadDeployment,
			&instance)
	} else if runtime.VirtualMachine != nil {
		return r.reconcileVMRuntimeInstance(
			ctx,
			req.ClusterName,
			*location,
			cl.GetClient(),
			downstreamStrategy,
			downstreamClient,
			&workload,
			&workloadDeployment,
			&instance,
		)
	}

	return ctrl.Result{}, nil
}

// TODO(jreese) Audit for workload-scoped but regional resources, make sure
// the k8s entity names are unique across regions.
func (r *InstanceReconciler) reconcileInstance(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata map[string]*string,
) (res ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	programmedCondition := metav1.Condition{
		Type:               computev1alpha.InstanceProgrammed,
		Status:             metav1.ConditionFalse,
		Reason:             computev1alpha.InstanceProgrammedReasonPendingProgramming,
		ObservedGeneration: instance.Generation,
		Message:            "Instance resources are not ready",
	}

	defer func() {
		if apimeta.SetStatusCondition(&instance.Status.Conditions, programmedCondition) {
			err = errors.Join(err, upstreamClient.Status().Update(ctx, instance))
		}
	}()

	if err := r.reconcileNetworkInterfaces(
		ctx,
		clusterName,
		location,
		upstreamClient,
		downstreamClient,
		workload,
		workloadDeployment,
		instance,
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling network interfaces: %w", err)
	}

	if err := r.buildConfigMaps(ctx, upstreamClient, cloudConfig, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling configmaps: %w", err)
	}

	aggregatedK8sSecret, hasAggregatedSecret, err := r.reconcileAggregatedSecret(ctx, upstreamClient, downstreamStrategy, downstreamClient, instance, workload)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling aggregated secret: %w", err)
	}

	if location.Spec.Provider.GCP != nil {
		// Service account names cannot exceed 30 characters
		// TODO(jreese) move to base36, as the underlying bytes won't be lost
		h := fnv.New32a()
		h.Write([]byte(workload.UID))

		// NOTE: This is garbage collected in the workload controller.
		var serviceAccount gcpcloudplatformv1beta1.ServiceAccount
		serviceAccountObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("workload-%s", workload.UID),
		}
		if err := downstreamStrategy.GetClient().Get(ctx, serviceAccountObjectKey, &serviceAccount); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching workload's service account: %w", err)
		}

		// NOTE: We do not wait for the service account to be ready, as the underlying
		// Terraform provider has a `time.Sleep(10 * time.Second)` in the create path.
		// See: https://github.com/hashicorp/terraform-provider-google/blob/092c36f3857a8ca0291dd3992f72357cabd45dc7/google/services/resourcemanager/resource_google_service_account.go#L181-L184
		//
		// In testing, the service account is created and ready in less than a second.
		// In the case that it's not ready, the system will retry until it is.
		//
		// We do, however, wait for the Crossplane `AnnotationKeyExternalCreateSucceeded`
		// annotation to show up, to ensure the action has been issued prior to trying
		// to create the instance.
		if serviceAccount.CreationTimestamp.IsZero() {
			serviceAccount = gcpcloudplatformv1beta1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name: serviceAccountObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        workload.Name,
						downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
						downstreamclient.UpstreamOwnerClusterName: clusterName,
						crossplanemeta.AnnotationKeyExternalName:  fmt.Sprintf("workload-%d", h.Sum32()),
					},
					Labels: map[string]string{
						computev1alpha.WorkloadUIDLabel: string(workload.UID),
					},
				},
				Spec: gcpcloudplatformv1beta1.ServiceAccountSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ManagementPolicies: crossplanecommonv1.ManagementPolicies{
							crossplanecommonv1.ManagementActionAll,
						},
						DeletionPolicy: crossplanecommonv1.DeletionDelete,
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("GCP", clusterName),
						},
					},
					ForProvider: gcpcloudplatformv1beta1.ServiceAccountParameters{
						Project:     ptr.To(location.Spec.Provider.GCP.ProjectID),
						Description: ptr.To(fmt.Sprintf("service account for workload %s", workload.UID)),
					},
				},
			}

			if err := downstreamClient.Create(ctx, &serviceAccount); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create workload's service account: %w", err)
			}
		}

		// TODO(jreese) use "Observe" mode resources to check on service account
		// status. I've ran into a condition in testing where the service account
		// was not found by the instance, which resulted in the create operation
		// being rejected.

		// See if there's been a failure to create the service account.
		if condition := serviceAccount.Status.GetCondition("LastAsyncOperation"); condition.Reason == "AsyncCreateFailure" {
			logger.Info("service account failed to create")
			programmedCondition.Reason = "ServiceAccountFailedToCreate"
			// TODO(jreese) should we only pass this through for locations in the same
			// datum project (cluster for multicluster-runtime) as the instance?
			programmedCondition.Message = fmt.Sprintf("Service account failed to create: %s", condition.Message)
			return ctrl.Result{}, fmt.Errorf(programmedCondition.Message)
		}

		// TODO(jreese) add IAM Policy to the GCP service account to allow the service
		// account used by Crossplane the `roles/iam.serviceAccountUser` role,
		// so that it can create instances with the service account without needing a
		// project level role binding. Probably just pass in the service account
		// email, otherwise we'd have to do some kind of discovery.

		if hasAggregatedSecret {
			proceed, err := r.reconcileGCPSecrets(
				ctx,
				clusterName,
				location,
				downstreamClient,
				&programmedCondition,
				cloudConfig,
				workload,
				aggregatedK8sSecret,
				serviceAccount,
			)
			if !proceed || err != nil {
				return ctrl.Result{}, err
			}
		}

		// It's unlikely that we will hit this condition, as we wait for secrets
		// above to be ready and return early if they are not. However, it's good to
		// check.
		if _, ok := serviceAccount.Annotations[crossplanemeta.AnnotationKeyExternalCreateSucceeded]; !ok {
			logger.Info("service account not created yet", "service_account", serviceAccount.Name)
			programmedCondition.Reason = "ProvisioningServiceAccount"
			programmedCondition.Message = "Service account is being provisioned for the workload"
			return ctrl.Result{}, nil
		}

		gcpInstance, err := r.reconcileGCPInstance(
			ctx,
			clusterName,
			upstreamClient,
			downstreamClient,
			location,
			workload,
			workloadDeployment,
			instance,
			cloudConfig,
			instanceMetadata,
			serviceAccount,
		)
		if err != nil {
			return ctrl.Result{}, err
		}

		if gcpInstance.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
			logger.Info("GCP instance not ready yet")
			programmedCondition.Reason = "ProvisioningProviderInstance"
			programmedCondition.Message = "GCP instance is being provisioned"

			return ctrl.Result{}, nil
		}

		programmedCondition.Status = metav1.ConditionTrue
		programmedCondition.Reason = computev1alpha.InstanceProgrammedReasonProgrammed
		programmedCondition.Message = "Instance has been programmed"
		instance.Status.Controller = &computev1alpha.InstanceControllerStatus{
			ObservedTemplateHash: instance.Spec.Controller.TemplateHash,
		}

		// TODO(jreese) remove when we have workload-operator define these
		if len(instance.Status.NetworkInterfaces) == 0 {
			instance.Status.NetworkInterfaces = make([]computev1alpha.InstanceNetworkInterfaceStatus, len(gcpInstance.Status.AtProvider.NetworkInterface))
		}

		for i, gcpNetworkInterface := range gcpInstance.Status.AtProvider.NetworkInterface {
			interfaceStatus := computev1alpha.InstanceNetworkInterfaceStatus{}
			if gcpNetworkInterface.NetworkIP != nil {
				interfaceStatus.Assignments.NetworkIP = gcpNetworkInterface.NetworkIP
			}

			for _, accessConfig := range gcpNetworkInterface.AccessConfig {
				if accessConfig.NATIP != nil {
					interfaceStatus.Assignments.ExternalIP = accessConfig.NATIP
				}
			}

			instance.Status.NetworkInterfaces[i] = interfaceStatus
		}
	} else if location.Spec.Provider.AWS != nil {

		var secretsParameter *awsssmv1beta1.Parameter
		if hasAggregatedSecret {
			param, proceed, err := r.reconcileAWSSecrets(
				ctx,
				clusterName,
				location,
				downstreamClient,
				&programmedCondition,
				cloudConfig,
				workloadDeployment,
				aggregatedK8sSecret,
			)
			if !proceed || err != nil {
				return ctrl.Result{}, err
			}

			secretsParameter = param

			cloudConfig.AWSRegion = location.Spec.Provider.AWS.Region
			cloudConfig.SecretsParameterName = *secretsParameter.Status.AtProvider.ID
		}

		var instanceProfileRole awsiamv1beta1.Role
		instanceProfileRoleObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("workload-%s", workload.UID),
		}
		if err := downstreamClient.Get(ctx, instanceProfileRoleObjectKey, &instanceProfileRole); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching instance profile role: %w", err)
		}

		if instanceProfileRole.CreationTimestamp.IsZero() {
			instanceProfileRole = awsiamv1beta1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceProfileRoleObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        workload.Name,
						downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
						downstreamclient.UpstreamOwnerClusterName: clusterName,
					},
				},
				Spec: awsiamv1beta1.RoleSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", clusterName),
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
					},
				},
			}

			if secretsParameter != nil {
				instanceProfileRole.Spec.ForProvider.InlinePolicy = append(instanceProfileRole.Spec.ForProvider.InlinePolicy, awsiamv1beta1.InlinePolicyParameters{
					Name: ptr.To("ssm-read"),
					Policy: ptr.To(fmt.Sprintf(text.Dedent(`
						{
							"Version": "2012-10-17",
							"Statement": [
								{
									"Effect": "Allow",
									"Action": "ssm:GetParameter",
									"Resource": "%s"
								}
							]
						}
					`),
						*secretsParameter.Status.AtProvider.Arn,
					)),
				})
			}

			if err := downstreamClient.Create(ctx, &instanceProfileRole); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create instance profile role: %w", err)
			}
		}

		var instanceProfile awsiamv1beta1.InstanceProfile
		instanceProfileObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("workload-%s", workload.UID),
		}
		if err := downstreamClient.Get(ctx, instanceProfileObjectKey, &instanceProfile); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching instance profile: %w", err)
		}

		if instanceProfile.CreationTimestamp.IsZero() {
			instanceProfile = awsiamv1beta1.InstanceProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceProfileObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        workload.Name,
						downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
						downstreamclient.UpstreamOwnerClusterName: clusterName,
					},
				},
				Spec: awsiamv1beta1.InstanceProfileSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", clusterName),
						},
					},
					ForProvider: awsiamv1beta1.InstanceProfileParameters{
						RoleRef: &crossplanecommonv1.Reference{
							Name: instanceProfileRole.Name,
						},
					},
				},
			}

			if err := downstreamClient.Create(ctx, &instanceProfile); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create instance profile: %w", err)
			}
		}

		var awsInstance awsec2v1beta1.Instance
		instanceObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("instance-%s", instance.UID),
		}
		if err := downstreamClient.Get(ctx, instanceObjectKey, &awsInstance); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching instance: %w", err)
		}

		if awsInstance.CreationTimestamp.IsZero() {
			cloudConfig.Hostname = fmt.Sprintf("%s.%s.%s.cloud.datum-dns.net", instance.Name, instance.Namespace, strings.TrimPrefix(clusterName, "/"))
			butaneConfig, err := cloudConfig.ToButane()
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to convert cloud config to ignition: %w", err)
			}

			ignitionConfigBytes, err := butaneConfig.ToIgnitionJSON()
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to generate ignition config: %w", err)
			}

			logger.Info("creating instance", "instance_name", instanceObjectKey.Name)
			awsInstance = awsec2v1beta1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        instance.Name,
						downstreamclient.UpstreamOwnerNamespace:   instance.Namespace,
						downstreamclient.UpstreamOwnerClusterName: clusterName,
					},
				},
				Spec: awsec2v1beta1.InstanceSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", clusterName),
						},
					},
					ForProvider: awsec2v1beta1.InstanceParameters{
						Region:           ptr.To(location.Spec.Provider.AWS.Region),
						AvailabilityZone: ptr.To(location.Spec.Provider.AWS.Zone),
						AMI:              ptr.To("ami-003496bc8140c472c"),
						InstanceType:     ptr.To("t3a.micro"),
						// TODO(jreese) remove
						KeyName:            ptr.To("jreese@datum.net"),
						IAMInstanceProfile: ptr.To(instanceProfile.Name),
						UserDataBase64:     ptr.To(base64.StdEncoding.EncodeToString(ignitionConfigBytes)),
					},
				},
			}

			for interfaceIndex := range instance.Spec.NetworkInterfaces {
				awsInstance.Spec.ForProvider.NetworkInterface = append(awsInstance.Spec.ForProvider.NetworkInterface, awsec2v1beta1.InstanceNetworkInterfaceParameters{
					NetworkCardIndex: ptr.To(float64(0)),
					DeviceIndex:      ptr.To(float64(interfaceIndex)),
					NetworkInterfaceIDRef: &crossplanecommonv1.Reference{
						Name: fmt.Sprintf("instance-%s-net-%d", instance.UID, interfaceIndex),
					},
				})
			}

			if err := downstreamClient.Create(ctx, &awsInstance); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create instance: %w", err)
			}
		}

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
	}

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileSandboxRuntimeInstance(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("processing sandbox based instance")

	runtimeSpec := instance.Spec.Runtime

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "instance",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			HostNetwork: true,
		},
	}

	volumeMap := map[string]computev1alpha.VolumeSource{}
	for _, v := range instance.Spec.Volumes {
		volumeMap[v.Name] = v.VolumeSource
	}

	for _, c := range runtimeSpec.Sandbox.Containers {
		// TODO(jreese) handle env vars that use `valueFrom`
		container := corev1.Container{
			Name:  c.Name,
			Image: c.Image,
			Env:   c.Env,
		}

		for _, attachment := range c.VolumeAttachments {
			if attachment.MountPath != nil {
				volume := volumeMap[attachment.Name]

				if volume.Disk != nil {
					populator := volume.Disk.Template.Spec.Populator
					if populator == nil || populator.Filesystem == nil {
						return ctrl.Result{}, fmt.Errorf("cannot mount volume with unknown filesystem")
					}

					container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
						Name:      fmt.Sprintf("disk-%s", attachment.Name),
						MountPath: *attachment.MountPath,
					})
				}

				if volume.ConfigMap != nil {
					// Cloud-init will place files at /etc/configmaps/<name>/<key>

					if len(volume.ConfigMap.Items) > 0 {
						// TODO(jreese) implement this
						logger.Info("attaching specific configmap items is not currently supported")
						return ctrl.Result{}, nil
					}

					container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
						Name:      fmt.Sprintf("configmap-%s", volume.ConfigMap.Name),
						MountPath: *attachment.MountPath,
					})
				}

				if volume.Secret != nil {
					if len(volume.Secret.Items) > 0 {
						// TODO(jreese) implement this
						logger.Info("attaching specific secret items is not currently supported")
						return ctrl.Result{}, nil
					}

					container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
						Name:      fmt.Sprintf("secret-%s", volume.Secret.SecretName),
						MountPath: *attachment.MountPath,
					})
				}
			}
		}

		pod.Spec.Containers = append(pod.Spec.Containers, container)
	}

	cloudConfig := &cloudinit.CloudConfig{}
	cloudConfig.RunCmd = []string{
		// Rely on network policies
		"iptables -I INPUT 1 -j ACCEPT",
		"systemctl enable kubelet --now",
	}

	hostPathType := corev1.HostPathDirectory

	// Add pod volumes
	for _, volume := range instance.Spec.Volumes {
		if volume.Disk != nil {
			populator := volume.Disk.Template.Spec.Populator
			if populator == nil || populator.Filesystem == nil {
				continue
			}

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: fmt.Sprintf("disk-%s", volume.Name),
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: fmt.Sprintf("/mnt/disk-%s", volume.Name),
						Type: &hostPathType,
					},
				},
			})
		}

		if volume.ConfigMap != nil {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: fmt.Sprintf("configmap-%s", volume.ConfigMap.Name),
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: fmt.Sprintf("/etc/configmaps/%s", volume.ConfigMap.Name),
						Type: &hostPathType,
					},
				},
			})
		}

		if volume.Secret != nil {
			// This content is populated by the populate_secrets.sh script.
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: fmt.Sprintf("secret-%s", volume.Secret.SecretName),
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: fmt.Sprintf("/etc/secrets/content/%s", volume.Secret.SecretName),
						Type: &hostPathType,
					},
				},
			})
		}
	}

	serializer := k8sjson.NewSerializerWithOptions(
		k8sjson.DefaultMetaFactory,
		upstreamClient.Scheme(),
		upstreamClient.Scheme(),
		k8sjson.SerializerOptions{Yaml: true, Pretty: true},
	)

	podSpecBytes, err := k8sruntime.Encode(serializer, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to marshal pod spec: %w", err)
	}

	cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
		Encoding:    "b64",
		Content:     base64.StdEncoding.EncodeToString(podSpecBytes),
		Owner:       "root:root",
		Path:        "/etc/kubernetes/manifests/instance.yaml",
		Permissions: "0644",
	})

	// Inject a boot volume
	instance = instance.DeepCopy()
	instance.Spec.Volumes = append([]computev1alpha.InstanceVolume{
		{
			Name: "datum-boot",
			VolumeSource: computev1alpha.VolumeSource{
				Disk: &computev1alpha.DiskTemplateVolumeSource{
					Template: &computev1alpha.DiskTemplateVolumeSourceTemplate{
						Spec: computev1alpha.DiskSpec{
							Type: "pd-standard",
							Populator: &computev1alpha.DiskPopulator{
								Image: &computev1alpha.ImageDiskPopulator{
									Name: "datumcloud/cos-stable-117-18613-0-79",
								},
							},
						},
					},
				},
			},
		},
	}, instance.Spec.Volumes...)

	return r.reconcileInstance(
		ctx,
		clusterName,
		location,
		upstreamClient,
		downstreamStrategy,
		downstreamClient,
		workload,
		workloadDeployment,
		instance,
		cloudConfig,
		nil,
	)
}

func (r *InstanceReconciler) reconcileVMRuntimeInstance(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("processing VM based workload")

	runtimeSpec := instance.Spec.Runtime

	instanceMetadata := map[string]*string{
		"ssh-keys": ptr.To(instance.Annotations[computev1alpha.SSHKeysAnnotation]),
	}

	volumeMap := map[string]computev1alpha.VolumeSource{}
	for _, v := range instance.Spec.Volumes {
		volumeMap[v.Name] = v.VolumeSource
	}

	cloudConfig := &cloudinit.CloudConfig{}

	mountParentDirs := sets.Set[string]{}
	for _, attachment := range runtimeSpec.VirtualMachine.VolumeAttachments {
		if attachment.MountPath != nil {
			volume := volumeMap[attachment.Name]

			// Disk backed volumes are currently handed inside `buildInstanceTemplateVolumes`

			if volume.ConfigMap != nil {
				// Cloud-init will place files at /etc/configmaps/<name>/<key>

				if len(volume.ConfigMap.Items) > 0 {
					// TODO(jreese) implement this
					logger.Info("attaching specific configmap items is not currently supported")
					return ctrl.Result{}, nil
				}

				mountParentDirs.Insert(fmt.Sprintf("mkdir -p %s", path.Dir(*attachment.MountPath)))

				cloudConfig.RunCmd = append(
					cloudConfig.RunCmd,
					fmt.Sprintf("ln -s /etc/configmaps/%s %s", volume.ConfigMap.Name, *attachment.MountPath),
				)
			}

			if volume.Secret != nil {
				if len(volume.Secret.Items) > 0 {
					// TODO(jreese) implement this
					logger.Info("attaching specific secret items is not currently supported")
					return ctrl.Result{}, nil
				}

				mountParentDirs.Insert(fmt.Sprintf("mkdir -p %s", path.Dir(*attachment.MountPath)))

				cloudConfig.RunCmd = append(
					cloudConfig.RunCmd,
					fmt.Sprintf("ln -s /etc/secrets/content/%s %s", volume.Secret.SecretName, *attachment.MountPath),
				)
			}
		}
	}

	cloudConfig.RunCmd = append(mountParentDirs.UnsortedList(), cloudConfig.RunCmd...)

	return r.reconcileInstance(
		ctx,
		clusterName,
		location,
		upstreamClient,
		downstreamStrategy,
		downstreamClient,
		workload,
		workloadDeployment,
		instance,
		cloudConfig,
		instanceMetadata,
	)
}

func (r *InstanceReconciler) buildConfigMaps(
	ctx context.Context,
	upstreamClient client.Client,
	cloudConfig *cloudinit.CloudConfig,
	instance *computev1alpha.Instance,
) error {
	var objectKeys []client.ObjectKey
	for _, volume := range instance.Spec.Volumes {
		if volume.ConfigMap != nil {
			objectKeys = append(objectKeys, client.ObjectKey{
				Namespace: instance.Namespace,
				Name:      volume.ConfigMap.Name,
			})
		}
	}

	if len(objectKeys) == 0 {
		return nil
	}

	for _, configMapObjectKey := range objectKeys {
		var configMap corev1.ConfigMap
		if err := upstreamClient.Get(ctx, configMapObjectKey, &configMap); err != nil {
			return fmt.Errorf("failed to get configmap: %w", err)
		}

		for k, v := range configMap.Data {
			cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
				Encoding:    "b64",
				Content:     base64.StdEncoding.EncodeToString([]byte(v)),
				Owner:       "root:root",
				Path:        fmt.Sprintf("/etc/configmaps/%s/%s", configMap.Name, k),
				Permissions: "0644",
			})
		}
	}

	return nil
}

// reconcileAggregatedSecret aggregates secret data into a single secret which
// can be used to populate provider secret stores.
//
// The second return value indicates whether or not secret material exists for
// the workload.
func (r *InstanceReconciler) reconcileAggregatedSecret(
	ctx context.Context,
	upstreamClient client.Client,
	downstreamStrategy downstreamclient.ResourceStrategy,
	downstreamClient client.Client,
	instance *computev1alpha.Instance,
	workload *computev1alpha.Workload,
) (*corev1.Secret, bool, error) {
	var objectKeys []client.ObjectKey
	for _, volume := range instance.Spec.Volumes {
		if volume.Secret != nil {
			objectKeys = append(objectKeys, client.ObjectKey{
				Namespace: instance.Namespace,
				Name:      volume.Secret.SecretName,
			})
		}
	}

	if len(objectKeys) == 0 {
		return nil, false, nil
	}

	// Aggregate secret data into one value by creating a map of secret names
	// to content. This will allow for mounting of keys into volumes or secrets
	// as expected.
	secretData := map[string]map[string][]byte{}
	for _, objectKey := range objectKeys {
		var k8ssecret corev1.Secret
		if err := upstreamClient.Get(ctx, objectKey, &k8ssecret); err != nil {
			return nil, false, fmt.Errorf("failed fetching secret: %w", err)
		}

		secretData[k8ssecret.Name] = k8ssecret.Data
	}

	secretBytes, err := json.Marshal(secretData)
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal secret data")
	}

	downstreamNamespaceName, err := downstreamStrategy.GetDownstreamNamespaceName(ctx, instance)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get downstream namespace name: %w", err)
	}

	// NOTE: This is garbage collected in the workload controller.
	aggregatedK8sSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: downstreamNamespaceName,
			Name:      fmt.Sprintf("workload-%s", workload.UID),
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, downstreamClient, aggregatedK8sSecret, func() error {
		if err := downstreamStrategy.SetControllerReference(ctx, workload, aggregatedK8sSecret); err != nil {
			return fmt.Errorf("failed to set owner reference on aggregated k8s secret: %w", err)
		}

		aggregatedK8sSecret.Data = map[string][]byte{
			"secretData": secretBytes,
		}

		return nil
	})

	if err != nil {
		return nil, false, fmt.Errorf("failed to reconcile aggregated k8s secret: %w", err)
	}

	return aggregatedK8sSecret, true, nil
}

func (r *InstanceReconciler) reconcileAWSSecrets(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	downstreamClient client.Client,
	programmedCondition *metav1.Condition,
	cloudConfig *cloudinit.CloudConfig,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	aggregatedK8sSecret *corev1.Secret,
) (*awsssmv1beta1.Parameter, bool, error) {
	logger := log.FromContext(ctx)

	var secretsParameter awsssmv1beta1.Parameter
	secretsParameterObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workloaddeployment-%s", workloadDeployment.UID),
	}
	if err := downstreamClient.Get(ctx, secretsParameterObjectKey, &secretsParameter); client.IgnoreNotFound(err) != nil {
		return nil, false, fmt.Errorf("failed fetching secrets parameter: %w", err)
	}

	if secretsParameter.CreationTimestamp.IsZero() {
		secretsParameter = awsssmv1beta1.Parameter{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretsParameterObjectKey.Name,
				Annotations: map[string]string{
					downstreamclient.UpstreamOwnerName:        workloadDeployment.Name,
					downstreamclient.UpstreamOwnerNamespace:   workloadDeployment.Namespace,
					downstreamclient.UpstreamOwnerClusterName: clusterName,
				},
			},
			Spec: awsssmv1beta1.ParameterSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: r.Config.GetProviderConfigName("AWS", clusterName),
					},
				},
				ForProvider: awsssmv1beta1.ParameterParameters_2{
					Region:    ptr.To(location.Spec.Provider.AWS.Region),
					Overwrite: ptr.To(true),
					Type:      ptr.To("SecureString"),
					ValueSecretRef: &crossplanecommonv1.SecretKeySelector{
						SecretReference: crossplanecommonv1.SecretReference{
							Namespace: aggregatedK8sSecret.Namespace,
							Name:      aggregatedK8sSecret.Name,
						},
						Key: "secretData",
					},
				},
			},
		}

		if err := downstreamClient.Create(ctx, &secretsParameter); err != nil {
			return nil, false, fmt.Errorf("failed to create secrets parameter: %w", err)
		}
	}

	if secretsParameter.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("secret not ready yet", "secret", secretsParameter.Name)
		programmedCondition.Reason = "ProvisioningSecret"
		programmedCondition.Message = "Secret is being provisioned for the workload"
		return nil, false, nil
	}

	cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
		Encoding:    "b64",
		Content:     base64.StdEncoding.EncodeToString([]byte(awsPopulateSecretsScript)),
		Owner:       "root:root",
		Path:        "/etc/secrets/populate_secrets_from_ssm.sh",
		Permissions: "0700",
	})

	// TODO(jreese) updates? Do we need to hash the secret data and put an
	// annotation on the parameter?

	return &secretsParameter, true, nil
}

func (r *InstanceReconciler) reconcileGCPSecrets(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	downstreamClient client.Client,
	programmedCondition *metav1.Condition,
	cloudConfig *cloudinit.CloudConfig,
	workload *computev1alpha.Workload,
	aggregatedK8sSecret *corev1.Secret,
	serviceAccount gcpcloudplatformv1beta1.ServiceAccount,
) (bool, error) {
	logger := log.FromContext(ctx)

	// Create a secret in the secret manager service, grant access to the service
	// account specific to the deployment.

	// NOTE: This is garbage collected in the workload controller.
	var secret gcpsecretmanagerv1beta2.Secret
	secretObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workload-%s", workload.UID),
	}
	if err := downstreamClient.Get(ctx, secretObjectKey, &secret); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching instance secret: %w", err)
	}

	if secret.CreationTimestamp.IsZero() {
		secret = gcpsecretmanagerv1beta2.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretObjectKey.Name,
				Annotations: map[string]string{
					downstreamclient.UpstreamOwnerName:        workload.Name,
					downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
					downstreamclient.UpstreamOwnerClusterName: clusterName,
				},
				Labels: map[string]string{
					computev1alpha.WorkloadUIDLabel: string(workload.UID),
				},
			},
			Spec: gcpsecretmanagerv1beta2.SecretSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ManagementPolicies: crossplanecommonv1.ManagementPolicies{
						crossplanecommonv1.ManagementActionAll,
					},
					DeletionPolicy: crossplanecommonv1.DeletionDelete,
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: r.Config.GetProviderConfigName("GCP", clusterName),
					},
				},
				ForProvider: gcpsecretmanagerv1beta2.SecretParameters{
					Project: ptr.To(location.Spec.Provider.GCP.ProjectID),
					Replication: &gcpsecretmanagerv1beta2.ReplicationParameters{
						Auto: &gcpsecretmanagerv1beta2.AutoParameters{},
					},
				},
			},
		}

		if err := downstreamClient.Create(ctx, &secret); err != nil {
			return false, fmt.Errorf("failed to create instance secret: %w", err)
		}
	}

	if secret.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("secret not ready yet", "secret", secret.Name)
		programmedCondition.Reason = "ProvisioningSecret"
		programmedCondition.Message = "Secret is being provisioned for the workload"
		return false, nil
	}

	// Store secret information in the secret version
	// TODO(jreese) handle updates to secrets - this needs to be done by creating
	// a new secret version.
	secretVersion := &gcpsecretmanagerv1beta1.SecretVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: secret.Name,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, secretVersion, func() error {
		if err := controllerutil.SetOwnerReference(&secret, secretVersion, downstreamClient.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference on secret version: %w", err)
		}

		if secretVersion.Annotations == nil {
			secretVersion.Annotations = make(map[string]string)
		}

		if secretVersion.Labels == nil {
			secretVersion.Labels = make(map[string]string)
		}

		secretVersion.Annotations[downstreamclient.UpstreamOwnerName] = workload.Name
		secretVersion.Annotations[downstreamclient.UpstreamOwnerNamespace] = workload.Namespace
		secretVersion.Annotations[downstreamclient.UpstreamOwnerClusterName] = clusterName

		secretVersion.Labels[computev1alpha.WorkloadUIDLabel] = string(workload.UID)

		secretVersion.Spec = gcpsecretmanagerv1beta1.SecretVersionSpec{
			ResourceSpec: crossplanecommonv1.ResourceSpec{
				ManagementPolicies: crossplanecommonv1.ManagementPolicies{
					crossplanecommonv1.ManagementActionAll,
				},
				DeletionPolicy:          crossplanecommonv1.DeletionDelete,
				ProviderConfigReference: secret.Spec.ProviderConfigReference,
			},
			ForProvider: gcpsecretmanagerv1beta1.SecretVersionParameters{
				Enabled:        ptr.To(true),
				DeletionPolicy: ptr.To("DELETE"),
				// This is self referencing toward the value in the spec prior to
				// assignment of our desired state, as Crossplane mutates this value
				// when resolving the SecretRef value.
				Secret: secretVersion.Spec.ForProvider.Secret,
				SecretDataSecretRef: crossplanecommonv1.SecretKeySelector{
					Key: "secretData",
					SecretReference: crossplanecommonv1.SecretReference{
						Name:      aggregatedK8sSecret.Name,
						Namespace: aggregatedK8sSecret.Namespace,
					},
				},
				SecretRef: &crossplanecommonv1.Reference{
					Name: secret.Name,
				},
			},
		}
		return nil
	})

	if err != nil {
		return false, fmt.Errorf("failed to create secret version: %w", err)
	}

	logger.Info("downstream secret version processed", "operation_result", result)

	cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
		Encoding:    "b64",
		Content:     base64.StdEncoding.EncodeToString([]byte(gcpPopulateSecretsScript)),
		Owner:       "root:root",
		Path:        "/etc/secrets/populate_secrets.py",
		Permissions: "0755",
	})

	cloudConfig.RunCmd = append(
		cloudConfig.RunCmd,
		fmt.Sprintf("/etc/secrets/populate_secrets.py https://secretmanager.googleapis.com/v1/%s/versions/latest:access", *secret.Status.AtProvider.Name),
	)

	var secretIAMMember gcpsecretmanagerv1beta2.SecretIAMMember
	if err := downstreamClient.Get(ctx, client.ObjectKeyFromObject(&secret), &secretIAMMember); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching secret's IAM policy: %w", err)
	}

	if secretIAMMember.CreationTimestamp.IsZero() {
		secretIAMMember = gcpsecretmanagerv1beta2.SecretIAMMember{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: secret.Namespace,
				Name:      secret.Name,
			},
			Spec: gcpsecretmanagerv1beta2.SecretIAMMemberSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ManagementPolicies: crossplanecommonv1.ManagementPolicies{
						crossplanecommonv1.ManagementActionAll,
					},
					DeletionPolicy: crossplanecommonv1.DeletionDelete,
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: r.Config.GetProviderConfigName("GCP", clusterName),
					},
				},
				ForProvider: gcpsecretmanagerv1beta2.SecretIAMMemberParameters{
					Project: ptr.To(location.Spec.Provider.GCP.ProjectID),
					SecretIDRef: &crossplanecommonv1.Reference{
						Name: secret.Name,
					},
					Role:   ptr.To("roles/secretmanager.secretAccessor"),
					Member: ptr.To(fmt.Sprintf("serviceAccount:%s@%s.iam.gserviceaccount.com", serviceAccount.Annotations[crossplanemeta.AnnotationKeyExternalName], location.Spec.Provider.GCP.ProjectID)),
				},
			},
		}

		if err := controllerutil.SetOwnerReference(&secret, &secretIAMMember, downstreamClient.Scheme()); err != nil {
			return false, fmt.Errorf("failed to set owner reference on secret IAM policy: %w", err)
		}

		if err := downstreamClient.Create(ctx, &secretIAMMember); err != nil {
			return false, fmt.Errorf("failed setting IAM policy on secret: %w", err)
		}

		// Wait for these together so they can be processed in parallel.

		if secretVersion.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
			logger.Info("secret version not ready yet", "secret", secret.Name, "secret_version", secretVersion.Name)
			programmedCondition.Reason = "ProvisioningSecretVersion"
			programmedCondition.Message = "Secret version is being provisioned for the workload"
			return false, nil
		}

		if secretIAMMember.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
			logger.Info("secret IAM member not ready yet", "secret", secret.Name, "secret_version", secretVersion.Name)
			programmedCondition.Reason = "ProvisioningSecretIAMMember"
			programmedCondition.Message = "Secret IAM member is being provisioned"
			return false, nil
		}
	}

	return true, nil
}

func (r *InstanceReconciler) reconcileGCPInstance(
	ctx context.Context,
	clusterName string,
	upstreamClient client.Client,
	downstreamClient client.Client,
	location networkingv1alpha.Location,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata map[string]*string,
	serviceAccount gcpcloudplatformv1beta1.ServiceAccount,
) (*gcpcomputev1beta2.Instance, error) {
	logger := log.FromContext(ctx)

	runtimeSpec := instance.Spec.Runtime
	gcpProject := location.Spec.Provider.GCP.ProjectID
	gcpZone := location.Spec.Provider.GCP.Zone

	gcpInstance := &gcpcomputev1beta2.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("instance-%s", instance.UID),
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, gcpInstance, func() error {

		if gcpInstance.Annotations == nil {
			gcpInstance.Annotations = make(map[string]string)
		}

		if !controllerutil.ContainsFinalizer(gcpInstance, gcpInfraFinalizer) {
			controllerutil.AddFinalizer(gcpInstance, gcpInfraFinalizer)
		}

		gcpInstance.Annotations[downstreamclient.UpstreamOwnerName] = instance.Name
		gcpInstance.Annotations[downstreamclient.UpstreamOwnerNamespace] = instance.Namespace
		gcpInstance.Annotations[downstreamclient.UpstreamOwnerClusterName] = clusterName

		machineType, ok := r.Config.MachineTypeMap[runtimeSpec.Resources.InstanceType]
		if !ok {
			return fmt.Errorf("unable to map datum instance type: %s", runtimeSpec.Resources.InstanceType)
		}

		userData, err := cloudConfig.Generate()
		if err != nil {
			return fmt.Errorf("failed generating cloud init user data: %w", err)
		}

		if instanceMetadata == nil {
			instanceMetadata = map[string]*string{}
		}

		instanceMetadata["user-data"] = ptr.To(fmt.Sprintf("## template: jinja\n#cloud-config\n\n%s", string(userData)))

		gcpInstance.Spec.ResourceSpec = crossplanecommonv1.ResourceSpec{
			ManagementPolicies: crossplanecommonv1.ManagementPolicies{
				crossplanecommonv1.ManagementActionAll,
			},
			DeletionPolicy: crossplanecommonv1.DeletionDelete,
			ProviderConfigReference: &crossplanecommonv1.Reference{
				Name: r.Config.GetProviderConfigName("GCP", clusterName),
			},
		}

		gcpInstance.Spec.ForProvider.MachineType = ptr.To(machineType)
		gcpInstance.Spec.ForProvider.CanIPForward = ptr.To(true)
		gcpInstance.Spec.ForProvider.Metadata = instanceMetadata
		gcpInstance.Spec.ForProvider.Hostname = ptr.To(fmt.Sprintf("%s.cloud.datum-dns.net", instance.Name))
		gcpInstance.Spec.ForProvider.Project = ptr.To(gcpProject)
		gcpInstance.Spec.ForProvider.Zone = ptr.To(gcpZone)

		if gcpInstance.Spec.ForProvider.ServiceAccount == nil {
			gcpInstance.Spec.ForProvider.ServiceAccount = &gcpcomputev1beta2.ServiceAccountParameters{
				Scopes: []*string{
					ptr.To("cloud-platform"),
				},
				Email: ptr.To(fmt.Sprintf("%s@%s.iam.gserviceaccount.com", serviceAccount.Annotations[crossplanemeta.AnnotationKeyExternalName], gcpProject)),
			}
		}
		gcpInstance.Spec.ForProvider.Tags = []*string{
			ptr.To(fmt.Sprintf("workload-%s", workload.UID)),
			ptr.To(fmt.Sprintf("deployment-%s", workloadDeployment.UID)),
		}

		if err := r.buildGCPInstanceNetworkInterfaces(ctx, upstreamClient, workloadDeployment, instance, gcpInstance); err != nil {
			return fmt.Errorf("failed to build instance network interfaces: %w", err)
		}

		if err := r.buildGCPInstanceVolumes(ctx, cloudConfig, instance, gcpInstance); err != nil {
			return fmt.Errorf("failed to build instance volumes: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create gcp instance: %w", err)
	}

	logger.Info("downstream instance processed", "operation_result", result)

	// There's a race condition where the instance is created, but the service
	// account is not yet ready, which results in Crossplane thinking the instance
	// exists, but GCP has torn it down. We're not waiting for the service account
	// to be marked as ready because of a hard coded 10 second sleep inside the
	// underlying terraform provider.
	//
	// We can tell this happens if the Ready condition is True, but the currentStatus
	// is STOPPING.
	//
	// An alternative approach would be to make another Crossplane resource
	// which is in Observe mode, and periodically probe it by bumping an
	// annotation. However, since this is a race condition and so far not seen
	// as all that common, we just deal with the issue if we run into it.
	if gcpInstance.Status.GetCondition(crossplanecommonv1.TypeReady).Status == corev1.ConditionTrue &&
		ptr.Deref(gcpInstance.Status.AtProvider.CurrentStatus, "") == "STOPPING" {
		logger.Info("instance is in STOPPING state, retrying create", "instance", gcpInstance.Name)
		// Add an annotation to the GCP instance to have crossplane process it again.
		gcpInstance.Annotations["compute.datumapis.com/infra-provider-gcp-retry-create"] = time.Now().Format(time.RFC3339)
		if err := downstreamClient.Update(ctx, gcpInstance); err != nil {
			return nil, fmt.Errorf("failed to update instance with retry annotation: %w", err)
		}
		return nil, fmt.Errorf("instance created in STOPPING state, retrying create")
	}

	if err := r.syncGCPInstancePowerState(ctx, upstreamClient, instance, gcpInstance); err != nil {
		return nil, fmt.Errorf("failed to sync instance power state: %w", err)
	}

	return gcpInstance, nil
}

func (r *InstanceReconciler) buildGCPInstanceVolumes(
	ctx context.Context,
	cloudConfig *cloudinit.CloudConfig,
	instance *computev1alpha.Instance,
	gcpInstance *gcpcomputev1beta2.Instance,
) error {
	// Temporary to address linter violations for unused variables. Will be
	// removed once we implement additional volume support below.
	_ = ctx
	_ = cloudConfig
	for volumeIndex, volume := range instance.Spec.Volumes {
		if volume.Disk == nil {
			continue
		}

		labels := map[string]string{
			"volume_name": volume.Name,
		}

		var deviceName *string

		if volume.Disk.DeviceName == nil {
			deviceName = ptr.To(fmt.Sprintf("volume-%d", volumeIndex))
		} else {
			deviceName = volume.Disk.DeviceName
		}

		if volume.Disk != nil {
			if volume.Disk.Template != nil {
				diskTemplate := volume.Disk.Template

				// TODO(jreese) we'll need to have our images have different udev rules
				// so that device names are enumerated at `/dev/disk/by-id/datumcloud-*`
				// instead of `/dev/disk/by-id/google-*`

				if populator := diskTemplate.Spec.Populator; populator != nil {
					if populator.Image != nil {
						// Should be prevented by validation, but be safe
						sourceImage, ok := r.Config.ImageMap[populator.Image.Name]
						if !ok {
							return fmt.Errorf("unable to map datum image name: %s", populator.Image.Name)
						}

						gcpInstance.Spec.ForProvider.BootDisk = &gcpcomputev1beta2.BootDiskParameters{
							AutoDelete: ptr.To(true),
							DeviceName: deviceName,
							InitializeParams: &gcpcomputev1beta2.InitializeParamsParameters{
								Image:  ptr.To(sourceImage),
								Type:   ptr.To(diskTemplate.Spec.Type),
								Labels: labels,
							},
						}

						if diskTemplate.Spec.Resources != nil {
							if storage, ok := diskTemplate.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
								return fmt.Errorf("unable to locate storage resource request for volume: %s", volume.Name)
							} else {
								gcpInstance.Spec.ForProvider.BootDisk.InitializeParams.Size = ptr.To(storage.AsFloat64Slow() / (1024 * 1024 * 1024))
							}
						}

						continue
					}

					// TODO(jreese) need to provision disks directly, and then attach them.
					continue

					// if populator.Filesystem != nil {
					// 	// Filesystem based populator, add cloud-init data to format the disk
					// 	// and make the volume available to mount into containers.

					// 	// TODO(jreese) we'll need to have our images have different udev rules
					// 	// so that device names are enumerated at `/dev/disk/by-id/datumcloud-*`
					// 	// instead of `/dev/disk/by-id/google-*`

					// 	devicePath := fmt.Sprintf("/dev/disk/by-id/google-%s", *deviceName)

					// 	cloudConfig.FSSetup = append(cloudConfig.FSSetup, cloudinit.FSSetup{
					// 		Label:      fmt.Sprintf("disk-%s", volume.Name),
					// 		Filesystem: populator.Filesystem.Type,
					// 		Device:     devicePath,
					// 	})

					// 	runtime := instance.Spec.Runtime

					// 	if runtime.Sandbox != nil {
					// 		cloudConfig.Mounts = append(cloudConfig.Mounts,
					// 			fmt.Sprintf("[%s, %s]", devicePath, fmt.Sprintf("/mnt/disk-%s", volume.Name)),
					// 		)
					// 	}

					// 	if runtime.VirtualMachine != nil {
					// 		for _, attachment := range runtime.VirtualMachine.VolumeAttachments {
					// 			if attachment.Name != volume.Name {
					// 				continue
					// 			}

					// 			if attachment.MountPath == nil {
					// 				logger.Info("unexpected VM attachment with no mount path for filesystem populated volume", "attachment_name", attachment.Name)
					// 				continue
					// 			}

					// 			cloudConfig.Mounts = append(cloudConfig.Mounts,
					// 				fmt.Sprintf("[%s, %s]", devicePath, *attachment.MountPath),
					// 			)
					// 		}
					// 	}
				}
			}

			// if diskTemplate.Spec.Resources != nil {
			// 	if storage, ok := diskTemplate.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
			// 		return fmt.Errorf("unable to locate storage resource request for volume: %s", volume.Name)
			// 	} else {
			// 		disk.DiskSizeGb = proto.Int64(storage.Value() / (1024 * 1024 * 1024))
			// 	}
			// }

		}
	}
	return nil
}

func (r *InstanceReconciler) buildGCPInstanceNetworkInterfaces(
	ctx context.Context,
	upstreamClient client.Client,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	gcpInstance *gcpcomputev1beta2.Instance,
) error {
	gcpNetworkInterfaces := gcpInstance.Spec.ForProvider.NetworkInterface

	if len(gcpNetworkInterfaces) == 0 {
		gcpNetworkInterfaces = make([]gcpcomputev1beta2.NetworkInterfaceParameters, len(instance.Spec.NetworkInterfaces))
	}

	for networkInterfaceIndex := range instance.Spec.NetworkInterfaces {
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", workloadDeployment.Name, networkInterfaceIndex),
		}

		if err := upstreamClient.Get(ctx, networkBindingObjectKey, &networkBinding); err != nil {
			return fmt.Errorf("failed fetching network binding for interface: %w", err)
		}

		if networkBinding.Status.NetworkContextRef == nil {
			return fmt.Errorf("network binding not associated with network context")
		}

		var networkContext networkingv1alpha.NetworkContext
		networkContextObjectKey := client.ObjectKey{
			Namespace: networkBinding.Status.NetworkContextRef.Namespace,
			Name:      networkBinding.Status.NetworkContextRef.Name,
		}
		if err := upstreamClient.Get(ctx, networkContextObjectKey, &networkContext); err != nil {
			return fmt.Errorf("failed fetching network context: %w", err)
		}

		var network networkingv1alpha.Network
		networkObjectKey := client.ObjectKey{
			Namespace: networkContext.Namespace,
			Name:      networkContext.Spec.Network.Name,
		}
		if err := upstreamClient.Get(ctx, networkObjectKey, &network); err != nil {
			return fmt.Errorf("failed fetching network: %w", err)
		}

		// Fetch the first subnet in the network context
		// TODO(jreese) have the workload-operator specify the subnet to use
		// in the instance status before the Network gate is removed.
		var subnet networkingv1alpha.Subnet
		subnetObjectKey := client.ObjectKey{
			Namespace: networkContext.Namespace,
			Name:      fmt.Sprintf("%s-0", networkContext.Name),
		}
		if err := upstreamClient.Get(ctx, subnetObjectKey, &subnet); err != nil {
			return fmt.Errorf("failed fetching subnet: %w", err)
		}

		gcpNetworkInterfaces[networkInterfaceIndex].Network = ptr.To(fmt.Sprintf("network-%s", network.UID))
		gcpNetworkInterfaces[networkInterfaceIndex].Subnetwork = ptr.To(fmt.Sprintf("subnet-%s", subnet.UID))
		gcpNetworkInterfaces[networkInterfaceIndex].AccessConfig = []gcpcomputev1beta2.AccessConfigParameters{
			{
				// TODO(jreese) wire this up to our own settings
				NATIP:               ptr.To(""),
				NetworkTier:         ptr.To("PREMIUM"),
				PublicPtrDomainName: ptr.To(""),
			},
		}

	}
	gcpInstance.Spec.ForProvider.NetworkInterface = gcpNetworkInterfaces
	return nil
}

func (r *InstanceReconciler) reconcileNetworkInterfaces(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	upstreamClient client.Client,
	downstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
) error {
	logger := log.FromContext(ctx)

	for interfaceIndex, networkInterface := range workloadDeployment.Spec.Template.Spec.NetworkInterfaces {
		interfacePolicy := networkInterface.NetworkPolicy
		if interfacePolicy == nil {
			continue
		}

		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: workloadDeployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", workloadDeployment.Name, interfaceIndex),
		}

		if err := upstreamClient.Get(ctx, networkBindingObjectKey, &networkBinding); err != nil {
			return fmt.Errorf("failed fetching network binding for interface: %w", err)
		}

		if networkBinding.Status.NetworkContextRef == nil {
			return fmt.Errorf("network binding not associated with network context")
		}

		var networkContext networkingv1alpha.NetworkContext
		networkContextObjectKey := client.ObjectKey{
			Namespace: networkBinding.Status.NetworkContextRef.Namespace,
			Name:      networkBinding.Status.NetworkContextRef.Name,
		}
		if err := upstreamClient.Get(ctx, networkContextObjectKey, &networkContext); err != nil {
			return fmt.Errorf("failed fetching network context: %w", err)
		}

		var network networkingv1alpha.Network
		if err := upstreamClient.Get(ctx, client.ObjectKey{Namespace: networkContext.Namespace, Name: networkContext.Spec.Network.Name}, &network); err != nil {
			return fmt.Errorf("failed to get network: %w", err)
		}

		if location.Spec.Provider.AWS != nil {
			var securityGroup awsec2v1beta1.SecurityGroup
			securityGroupObjectKey := client.ObjectKey{
				Name: fmt.Sprintf("workload-%s-net-%d", workload.UID, interfaceIndex),
			}
			if err := downstreamClient.Get(ctx, securityGroupObjectKey, &securityGroup); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed fetching security group: %w", err)
			}

			if securityGroup.CreationTimestamp.IsZero() {
				logger.Info("creating security group for interface policy", "security_group_name", securityGroupObjectKey.Name)
				securityGroup = awsec2v1beta1.SecurityGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name: securityGroupObjectKey.Name,
						Annotations: map[string]string{
							downstreamclient.UpstreamOwnerName:        workload.Name,
							downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
							downstreamclient.UpstreamOwnerClusterName: clusterName,
						},
					},
					Spec: awsec2v1beta1.SecurityGroupSpec{
						ResourceSpec: crossplanecommonv1.ResourceSpec{
							ProviderConfigReference: &crossplanecommonv1.Reference{
								Name: r.Config.GetProviderConfigName("AWS", clusterName),
							},
						},
						ForProvider: awsec2v1beta1.SecurityGroupParameters_2{
							Region: ptr.To(location.Spec.Provider.AWS.Region),
							VPCIDRef: &crossplanecommonv1.Reference{
								Name: fmt.Sprintf("networkcontext-%s", networkContext.UID),
							},
						},
					},
				}

				if err := downstreamClient.Create(ctx, &securityGroup); err != nil {
					return fmt.Errorf("failed to create security group: %w", err)
				}
			}

			var securityGroupEgressRule awsec2v1beta1.SecurityGroupRule
			securityGroupEgressRuleObjectKey := client.ObjectKey{
				Name: fmt.Sprintf("workload-%s-net-%d-egress", workload.UID, interfaceIndex),
			}
			if err := downstreamClient.Get(ctx, securityGroupEgressRuleObjectKey, &securityGroupEgressRule); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed fetching security group egress rule: %w", err)
			}

			if securityGroupEgressRule.CreationTimestamp.IsZero() {
				logger.Info("creating security group egress rule", "security_group_rule_name", securityGroupEgressRuleObjectKey.Name)
				securityGroupEgressRule = awsec2v1beta1.SecurityGroupRule{
					ObjectMeta: metav1.ObjectMeta{
						Name: securityGroupEgressRuleObjectKey.Name,
						Annotations: map[string]string{
							downstreamclient.UpstreamOwnerName:        workload.Name,
							downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
							downstreamclient.UpstreamOwnerClusterName: clusterName,
						},
					},
					Spec: awsec2v1beta1.SecurityGroupRuleSpec{
						ResourceSpec: crossplanecommonv1.ResourceSpec{
							ProviderConfigReference: &crossplanecommonv1.Reference{
								Name: r.Config.GetProviderConfigName("AWS", clusterName),
							},
						},
						ForProvider: awsec2v1beta1.SecurityGroupRuleParameters_2{
							Region: ptr.To(location.Spec.Provider.AWS.Region),
							SecurityGroupIDRef: &crossplanecommonv1.Reference{
								Name: fmt.Sprintf("workload-%s-net-%d", workload.UID, interfaceIndex),
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

				if err := downstreamClient.Create(ctx, &securityGroupEgressRule); err != nil {
					return fmt.Errorf("failed to create security group egress rule: %w", err)
				}
			}

			var networkInterface awsec2v1beta1.NetworkInterface
			networkInterfaceObjectKey := client.ObjectKey{
				Name: fmt.Sprintf("instance-%s-net-%d", instance.UID, interfaceIndex),
			}
			if err := downstreamClient.Get(ctx, networkInterfaceObjectKey, &networkInterface); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed fetching network interface: %w", err)
			}

			if networkInterface.CreationTimestamp.IsZero() {
				// Fetch the first subnet in the network context
				// TODO(jreese) have the workload-operator specify the subnet to use
				// in the instance status before the Network gate is removed.
				var subnet networkingv1alpha.Subnet
				subnetObjectKey := client.ObjectKey{
					Namespace: networkContext.Namespace,
					Name:      fmt.Sprintf("%s-0", networkContext.Name),
				}
				if err := upstreamClient.Get(ctx, subnetObjectKey, &subnet); err != nil {
					return fmt.Errorf("failed fetching subnet: %w", err)
				}

				logger.Info("creating network interface", "network_interface_name", networkInterfaceObjectKey.Name)
				networkInterface = awsec2v1beta1.NetworkInterface{
					ObjectMeta: metav1.ObjectMeta{
						Name: networkInterfaceObjectKey.Name,
						Annotations: map[string]string{
							downstreamclient.UpstreamOwnerName:        instance.Name,
							downstreamclient.UpstreamOwnerNamespace:   instance.Namespace,
							downstreamclient.UpstreamOwnerClusterName: clusterName,
						},
					},
					Spec: awsec2v1beta1.NetworkInterfaceSpec{
						ResourceSpec: crossplanecommonv1.ResourceSpec{
							ProviderConfigReference: &crossplanecommonv1.Reference{
								Name: r.Config.GetProviderConfigName("AWS", clusterName),
							},
						},
						ForProvider: awsec2v1beta1.NetworkInterfaceParameters_2{
							Region: ptr.To(location.Spec.Provider.AWS.Region),
							SubnetIDRef: &crossplanecommonv1.Reference{
								Name: fmt.Sprintf("subnet-%s", subnet.UID),
							},
							SecurityGroupRefs: []crossplanecommonv1.Reference{
								{
									Name: securityGroupObjectKey.Name,
								},
							},
						},
					},
				}

				if err := downstreamClient.Create(ctx, &networkInterface); err != nil {
					return fmt.Errorf("failed to create network interface: %w", err)
				}

				if interfaceIndex == 0 {
					var publicIP awsec2v1beta1.EIP
					publicIPObjectKey := client.ObjectKey{
						Name: fmt.Sprintf("instance-%s-net-%d-public-ip", instance.UID, interfaceIndex),
					}
					if err := downstreamClient.Get(ctx, publicIPObjectKey, &publicIP); client.IgnoreNotFound(err) != nil {
						return fmt.Errorf("failed fetching public IP: %w", err)
					}

					if publicIP.CreationTimestamp.IsZero() {
						logger.Info("creating public IP", "public_ip_name", publicIPObjectKey.Name)
						publicIP = awsec2v1beta1.EIP{
							ObjectMeta: metav1.ObjectMeta{
								Name: publicIPObjectKey.Name,
								Annotations: map[string]string{
									downstreamclient.UpstreamOwnerName:        instance.Name,
									downstreamclient.UpstreamOwnerNamespace:   instance.Namespace,
									downstreamclient.UpstreamOwnerClusterName: clusterName,
								},
							},
							Spec: awsec2v1beta1.EIPSpec{
								ResourceSpec: crossplanecommonv1.ResourceSpec{
									ProviderConfigReference: &crossplanecommonv1.Reference{
										Name: r.Config.GetProviderConfigName("AWS", clusterName),
									},
								},
								ForProvider: awsec2v1beta1.EIPParameters{
									Region: ptr.To(location.Spec.Provider.AWS.Region),
									Domain: ptr.To("vpc"),
									NetworkInterfaceRef: &crossplanecommonv1.Reference{
										Name: networkInterfaceObjectKey.Name,
									},
								},
							},
						}

						if err := downstreamClient.Create(ctx, &publicIP); err != nil {
							return fmt.Errorf("failed to create public IP: %w", err)
						}
					}
				}
			}

		}

		if err := r.reconcileNetworkInterfaceNetworkPolicies(
			ctx,
			clusterName,
			location,
			downstreamClient,
			workload,
			workloadDeployment,
			network,
			interfaceIndex,
			interfacePolicy,
		); err != nil {
			return fmt.Errorf("failed reconciling network interface network policies: %w", err)
		}
	}
	return nil
}

func (r *InstanceReconciler) reconcileNetworkInterfaceNetworkPolicies(
	ctx context.Context,
	clusterName string,
	location networkingv1alpha.Location,
	downstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	network networkingv1alpha.Network,
	interfaceIndex int,
	interfacePolicy *computev1alpha.InstanceNetworkInterfaceNetworkPolicy,
) error {
	logger := log.FromContext(ctx)

	for ruleIndex, ingressRule := range interfacePolicy.Ingress {
		if location.Spec.Provider.GCP != nil {
			// This could result in duplicate firewall rules if a workload that spans
			// multiple contexts of the same network is created. This is considered
			// ok for the time being, as the move to effective network policies for
			// interfaces will address this.
			//
			// In addition, these will not be GCd until the workload is deleted,
			firewallName := fmt.Sprintf("deployment-%s-net-%d-%d", workloadDeployment.UID, interfaceIndex, ruleIndex)

			var firewall gcpcomputev1beta2.Firewall
			firewallObjectKey := client.ObjectKey{
				Name: firewallName,
			}

			if err := downstreamClient.Get(ctx, firewallObjectKey, &firewall); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to read firewall from k8s API: %w", err)
			}

			if firewall.CreationTimestamp.IsZero() {
				logger.Info("creating firewall for interface policy rule", "firewall_name", firewallName)
				firewall = gcpcomputev1beta2.Firewall{
					ObjectMeta: metav1.ObjectMeta{
						Name: firewallObjectKey.Name,
						Labels: map[string]string{
							computev1alpha.WorkloadUIDLabel:           string(workload.UID),
							computev1alpha.WorkloadDeploymentUIDLabel: string(workloadDeployment.UID),
						},
					},
					Spec: gcpcomputev1beta2.FirewallSpec{
						ResourceSpec: crossplanecommonv1.ResourceSpec{
							ManagementPolicies: crossplanecommonv1.ManagementPolicies{
								crossplanecommonv1.ManagementActionAll,
							},
							DeletionPolicy: crossplanecommonv1.DeletionDelete,
							ProviderConfigReference: &crossplanecommonv1.Reference{
								Name: r.Config.GetProviderConfigName("GCP", clusterName),
							},
						},
						ForProvider: gcpcomputev1beta2.FirewallParameters{
							Project: ptr.To(location.Spec.Provider.GCP.ProjectID),
							Description: ptr.To(fmt.Sprintf(
								"instance interface policy for %s: interfaceIndex:%d, ruleIndex:%d",
								workloadDeployment.Name,
								interfaceIndex,
								ruleIndex,
							)),
							Direction: ptr.To("INGRESS"),
							NetworkRef: &crossplanecommonv1.Reference{
								Name: fmt.Sprintf("network-%s", network.UID),
							},
							Priority: ptr.To(float64(65534)),
							TargetTags: []*string{
								ptr.To(fmt.Sprintf("workload-%s", workload.UID)),
								ptr.To(fmt.Sprintf("deployment-%s", workloadDeployment.UID)),
							},
						},
					},
				}

				for _, port := range ingressRule.Ports {
					ipProtocol := "tcp"
					if port.Protocol != nil {
						ipProtocol = strings.ToLower(string(*port.Protocol))
					}

					var gcpPorts []*string
					if port.Port != nil {
						var gcpPort string

						gcpPort = strconv.Itoa(port.Port.IntValue())
						if gcpPort == "0" {
							// TODO(jreese) look up named port
							logger.Info("named port lookup not implemented")
							return nil
						}

						if port.EndPort != nil {
							gcpPort = fmt.Sprintf("%s-%d", gcpPort, *port.EndPort)
						}

						gcpPorts = append(gcpPorts, ptr.To(gcpPort))
					}

					firewall.Spec.ForProvider.Allow = append(firewall.Spec.ForProvider.Allow, gcpcomputev1beta2.AllowParameters{
						Protocol: ptr.To(ipProtocol),
						Ports:    gcpPorts,
					})
				}

				for _, peer := range ingressRule.From {
					if peer.IPBlock != nil {
						firewall.Spec.ForProvider.SourceRanges = append(firewall.Spec.ForProvider.SourceRanges, ptr.To(peer.IPBlock.CIDR))
						// TODO(jreese) implement IPBlock.Except as a separate rule of one higher priority
					}
				}

				if err := downstreamClient.Create(ctx, &firewall); err != nil {
					return fmt.Errorf("failed to create firewall: %w", err)
				}
			}
		} else if location.Spec.Provider.AWS != nil {
			for portIndex, port := range ingressRule.Ports {
				if port.Port == nil {
					logger.Info("skipping interface policy rule with no port", "rule_index", ruleIndex, "port_index", portIndex)
					continue
				}

				ipProtocol := "tcp"
				if port.Protocol != nil {
					ipProtocol = strings.ToLower(string(*port.Protocol))
				}

				var securityGroupRule awsec2v1beta1.SecurityGroupRule
				securityGroupRuleObjectKey := client.ObjectKey{
					Name: fmt.Sprintf("workload-%s-net-%d-%d-%d", workload.UID, interfaceIndex, ruleIndex, portIndex),
				}
				if err := downstreamClient.Get(ctx, securityGroupRuleObjectKey, &securityGroupRule); client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("failed fetching security group rule: %w", err)
				}

				if securityGroupRule.CreationTimestamp.IsZero() {
					logger.Info("creating security group rule for interface policy rule", "security_group_rule_name", securityGroupRuleObjectKey.Name)
					securityGroupRule = awsec2v1beta1.SecurityGroupRule{
						ObjectMeta: metav1.ObjectMeta{
							Name: securityGroupRuleObjectKey.Name,
							Annotations: map[string]string{
								downstreamclient.UpstreamOwnerName:        workload.Name,
								downstreamclient.UpstreamOwnerNamespace:   workload.Namespace,
								downstreamclient.UpstreamOwnerClusterName: clusterName,
							},
						},
						Spec: awsec2v1beta1.SecurityGroupRuleSpec{
							ResourceSpec: crossplanecommonv1.ResourceSpec{
								ProviderConfigReference: &crossplanecommonv1.Reference{
									Name: r.Config.GetProviderConfigName("AWS", clusterName),
								},
							},
							ForProvider: awsec2v1beta1.SecurityGroupRuleParameters_2{
								Region: ptr.To(location.Spec.Provider.AWS.Region),
								SecurityGroupIDRef: &crossplanecommonv1.Reference{
									Name: fmt.Sprintf("workload-%s-net-%d", workload.UID, interfaceIndex),
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

					if err := downstreamClient.Create(ctx, &securityGroupRule); err != nil {
						return fmt.Errorf("failed to create security group rule: %w", err)
					}
				}
			}
		}
	}
	return nil
}

func (r *InstanceReconciler) syncGCPInstancePowerState(
	ctx context.Context,
	upstreamClient client.Client,
	instance *computev1alpha.Instance,
	gcpInstance *gcpcomputev1beta2.Instance,
) error {
	runningCondition := metav1.Condition{
		Type:               computev1alpha.InstanceRunning,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: instance.Generation,
		Reason:             computev1alpha.InstanceRunningReasonStopped,
		Message:            "Instance is not running",
	}

	// In testing, no observation has been made of Crossplane updating these
	// values as instances are created or deleted, but we go through the logic
	// either way. In our case, we'll say the instance is stopping if there's
	// a deletion timestamp. The provisioning path does get handled correctly in
	// that CurrentStatus is nil until it's RUNNING.
	//
	// TODO(jreese) we may be able to have an Observe mode resource that we can
	// force refreshes on when the main resource is being processed. This seems
	// to work in testing, but it seems that at least one modification is needed
	// to get the first refresh to happen.
	// https://docs.crossplane.io/latest/guides/import-existing-resources/#import-resources-automatically

	if !instance.DeletionTimestamp.IsZero() {
		runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
		runningCondition.Message = "Instance is being deleted"
	} else {

		// CurrentStatus can be one of:
		//
		//  - PROVISIONING
		//  - STAGING
		//  - RUNNING
		//  - STOPPING
		//  - SUSPENDING
		//  - SUSPENDED
		//  - REPAIRING
		//  - TERMINATED
		//
		// See: https://cloud.google.com/compute/docs/instances/instance-lifecycle
		switch ptr.Deref(gcpInstance.Status.AtProvider.CurrentStatus, "PROVISIONING") {
		case "PROVISIONING":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStarting
			runningCondition.Message = "Instance is starting"
		case "STAGING":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStarting
			runningCondition.Message = "Instance is staging"
		case "RUNNING":
			runningCondition.Status = metav1.ConditionTrue
			runningCondition.Reason = computev1alpha.InstanceRunningReasonRunning
			runningCondition.Message = "Instance is running"
		case "STOPPING":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopping
			runningCondition.Message = "Instance is stopping"
		case "SUSPENDING":
		case "SUSPENDED":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopped
			runningCondition.Message = "Instance is suspended or suspending at provider"
		case "REPAIRING":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopped
			runningCondition.Message = "Instance is being repaired at provider"
		case "TERMINATED":
			runningCondition.Reason = computev1alpha.InstanceRunningReasonStopped
			runningCondition.Message = "Instance is not running"
		}
	}

	if apimeta.SetStatusCondition(&instance.Status.Conditions, runningCondition) {
		if err := upstreamClient.Status().Update(ctx, instance); err != nil {
			return fmt.Errorf("failed to update instance status: %w", err)
		}
	}

	return nil
}

func (r *InstanceReconciler) Finalize(
	ctx context.Context,
	upstreamClient client.Client,
	instance *computev1alpha.Instance,
) error {
	logger := log.FromContext(ctx)
	logger.Info("finalizing instance", "instance_name", instance.Name, "finalizers", instance.Finalizers)

	var gcpInstance gcpcomputev1beta2.Instance
	gcpInstanceObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("instance-%s", instance.UID),
	}
	downstreamClient := r.DownstreamCluster.GetClient()

	if err := downstreamClient.Get(ctx, gcpInstanceObjectKey, &gcpInstance); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed fetching downstream instance: %w", err)
	}

	if dt := gcpInstance.DeletionTimestamp; !gcpInstance.CreationTimestamp.IsZero() && dt.IsZero() {
		if err := downstreamClient.Delete(ctx, &gcpInstance); err != nil {
			return fmt.Errorf("failed to delete downstream instance: %w", err)
		}
	}

	logger.Info("gcp instance finalizers", "finalizers", gcpInstance.Finalizers)

	// Wait for the instance to be deleted - crossplane doesn't update the
	// atProvider information or any status conditions when it's successfully
	// deleted a resource. Observing that the crossplane finalizer has been
	// removed is the best we've got.
	if !gcpInstance.CreationTimestamp.IsZero() && controllerutil.ContainsFinalizer(&gcpInstance, crossplaneFinalizer) {
		logger.Info("downstream instance is being deleted")
		return r.syncGCPInstancePowerState(ctx, upstreamClient, instance, &gcpInstance)
	}

	logger.Info("downstream instance deleted")

	if controllerutil.RemoveFinalizer(&gcpInstance, gcpInfraFinalizer) {
		if err := downstreamClient.Update(ctx, &gcpInstance); err != nil {
			return fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	if controllerutil.RemoveFinalizer(instance, gcpInfraFinalizer) {
		if err := upstreamClient.Update(ctx, instance); err != nil {
			return fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpcloudplatformv1beta1.ServiceAccount{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource[*gcpcloudplatformv1beta1.ServiceAccount](mgr))).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpsecretmanagerv1beta2.Secret{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource[*gcpsecretmanagerv1beta2.Secret](mgr))).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpsecretmanagerv1beta1.SecretVersion{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource[*gcpsecretmanagerv1beta1.SecretVersion](mgr))).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpcomputev1beta2.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*gcpcomputev1beta2.Instance, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, instance *gcpcomputev1beta2.Instance) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := instance.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := instance.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := instance.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("GCP instance is missing upstream ownership metadata")
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
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &awsec2v1beta1.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*awsec2v1beta1.Instance, mcreconcile.Request] {
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
		Named("instance").
		Complete(r)
}
