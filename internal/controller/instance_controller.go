package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	crossplanemeta "github.com/crossplane/crossplane-runtime/pkg/meta"
	gcpcloudplatformv1beta1 "github.com/upbound/provider-gcp/apis/cloudplatform/v1beta1"
	gcpcomputev1beta2 "github.com/upbound/provider-gcp/apis/compute/v1beta2"
	gcpsecretmanagerv1beta1 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta1"
	gcpsecretmanagerv1beta2 "github.com/upbound/provider-gcp/apis/secretmanager/v1beta2"
	gcpv1beta1 "github.com/upbound/provider-gcp/apis/v1beta1"
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

	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	datumhandler "go.datum.net/infra-provider-gcp/internal/handler"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpWorkloadFinalizer = "compute.datumapis.com/gcp-workload-controller"
const crossplaneFinalizer = "finalizer.managedresource.crossplane.io"

var errResourceIsDeleting = errors.New("resource is deleting")

// InstanceReconciler reconciles Instances and manages their intended state in
// GCP
type InstanceReconciler struct {
	mgr               mcmanager.Manager
	LocationClassName string
	DownstreamCluster cluster.Cluster
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/finalizers,verbs=update
// TODO(jreese) Additional RBAC for referenced resources

// TODO(jreese) RBAC for Crossplane resources

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

	_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *instance.Spec.Location, r.LocationClassName)
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

	// Fetch the ProviderConfig that will be used for Crossplane GCP resources
	var providerConfig gcpv1beta1.ProviderConfig
	if err := cl.GetClient().Get(ctx, client.ObjectKey{
		Name: "project-test-fz3pr6", // TODO
	}, &providerConfig); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching provider config: %w", err)
	}

	if apimeta.IsStatusConditionTrue(instance.Status.Conditions, computev1alpha.InstanceProgrammed) {
		if instance.Status.Controller != nil && instance.Spec.Controller.TemplateHash == instance.Status.Controller.ObservedTemplateHash {
			logger.Info("instance is already programmed")
			return ctrl.Result{}, nil
		}
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

	runtime := instance.Spec.Runtime
	if runtime.Sandbox != nil {
		return r.reconcileSandboxRuntimeInstance(ctx, req.ClusterName, providerConfig, cl.GetClient(), &workload, &workloadDeployment, &instance)
	} else if runtime.VirtualMachine != nil {
		return r.reconcileVMRuntimeInstance(ctx, req.ClusterName, providerConfig, cl.GetClient(), &workload, &workloadDeployment, &instance)
	}

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileInstance(
	ctx context.Context,
	clusterName string,
	providerConfig gcpv1beta1.ProviderConfig,
	upstreamClient client.Client,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata map[string]*string,
) (res ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	var location networkingv1alpha.Location
	locationObjectKey := client.ObjectKey{
		Namespace: instance.Spec.Location.Namespace,
		Name:      instance.Spec.Location.Name,
	}
	if err := upstreamClient.Get(ctx, locationObjectKey, &location); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching location: %w", err)
	}

	if location.Spec.Provider.GCP == nil {
		return ctrl.Result{}, fmt.Errorf("attached location is not for the GCP provider")
	}

	gcpZone := location.Spec.Provider.GCP.Zone

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

	// TODO
	// if err := r.reconcileNetworkInterfaceNetworkPolicies(ctx, upstreamClient, gcpProject, r.InfraClusterNamespaceName, deployment); err != nil {
	// 	return ctrl.Result{}, fmt.Errorf("failed reconciling network interface network policies: %w", err)
	// }

	// Service account names cannot exceed 30 characters
	// TODO(jreese) move to base36, as the underlying bytes won't be lost
	h := fnv.New32a()
	h.Write([]byte(workload.UID))

	// NOTE: This is garbage collected in the workload controller.
	var serviceAccount gcpcloudplatformv1beta1.ServiceAccount
	serviceAccountObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workload-%s", workload.UID),
	}
	if err := r.DownstreamCluster.GetClient().Get(ctx, serviceAccountObjectKey, &serviceAccount); client.IgnoreNotFound(err) != nil {
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
						Name: providerConfig.Name,
					},
				},
				ForProvider: gcpcloudplatformv1beta1.ServiceAccountParameters{
					Description: ptr.To(fmt.Sprintf("service account for workload %s", workload.UID)),
				},
			},
		}

		if err := r.DownstreamCluster.GetClient().Create(ctx, &serviceAccount); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create workload's service account: %w", err)
		}
	}

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

	if err := r.buildConfigMaps(ctx, upstreamClient, cloudConfig, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling configmaps: %w", err)
	}

	proceed, err := r.reconcileSecrets(
		ctx,
		clusterName,
		providerConfig,
		upstreamClient,
		&programmedCondition,
		cloudConfig,
		workload,
		instance,
		serviceAccount,
	)
	if !proceed || err != nil {
		return ctrl.Result{}, err
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

	result, gcpInstance, err := r.reconcileGCPInstance(
		ctx,
		clusterName,
		providerConfig,
		upstreamClient,
		gcpZone,
		workload,
		workloadDeployment,
		instance,
		cloudConfig,
		instanceMetadata,
		serviceAccount,
	)
	if !result.IsZero() || err != nil {
		return result, err
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

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileSandboxRuntimeInstance(
	ctx context.Context,
	clusterName string,
	providerConfig gcpv1beta1.ProviderConfig,
	client client.Client,
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
		client.Scheme(),
		client.Scheme(),
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
		providerConfig,
		client,
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
	providerConfig gcpv1beta1.ProviderConfig,
	client client.Client,
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
		providerConfig,
		client,
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

func (r *InstanceReconciler) reconcileSecrets(
	ctx context.Context,
	clusterName string,
	providerConfig gcpv1beta1.ProviderConfig,
	upstreamClient client.Client,
	programmedCondition *metav1.Condition,
	cloudConfig *cloudinit.CloudConfig,
	workload *computev1alpha.Workload,
	instance *computev1alpha.Instance,
	serviceAccount gcpcloudplatformv1beta1.ServiceAccount,
) (bool, error) {
	logger := log.FromContext(ctx)
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
		return true, nil
	}

	// Aggregate secret data into one value by creating a map of secret names
	// to content. This will allow for mounting of keys into volumes or secrets
	// as expected.
	secretData := map[string]map[string][]byte{}
	for _, objectKey := range objectKeys {
		var k8ssecret corev1.Secret
		if err := upstreamClient.Get(ctx, objectKey, &k8ssecret); err != nil {
			return false, fmt.Errorf("failed fetching secret: %w", err)
		}

		secretData[k8ssecret.Name] = k8ssecret.Data
	}

	secretBytes, err := json.Marshal(secretData)
	if err != nil {
		return false, fmt.Errorf("failed to marshal secret data")
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(clusterName, upstreamClient, r.DownstreamCluster.GetClient())
	downstreamClient := downstreamStrategy.GetClient()

	downstreamNamespaceName, err := downstreamStrategy.GetDownstreamNamespaceName(ctx, instance)
	if err != nil {
		return false, fmt.Errorf("failed to get downstream namespace name: %w", err)
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
		return false, fmt.Errorf("failed to reconcile aggregated k8s secret: %w", err)
	}

	// Create a secret in the secret manager service, grant access to the service
	// account specific to the deployment.

	// NOTE: This is garbage collected in the workload controller.
	var secret gcpsecretmanagerv1beta2.Secret
	secretObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workload-%s", workload.UID),
	}
	if err := r.DownstreamCluster.GetClient().Get(ctx, secretObjectKey, &secret); client.IgnoreNotFound(err) != nil {
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
						Name: providerConfig.Name,
					},
				},
				ForProvider: gcpsecretmanagerv1beta2.SecretParameters{
					Replication: &gcpsecretmanagerv1beta2.ReplicationParameters{
						Auto: &gcpsecretmanagerv1beta2.AutoParameters{},
					},
				},
			},
		}

		if err := r.DownstreamCluster.GetClient().Create(ctx, &secret); err != nil {
			return false, fmt.Errorf("failed to create instance secret: %w", err)
		}
	}

	if secret.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("secret not ready yet", "secret", secret.Name)
		programmedCondition.Reason = "ProvisioningSecret"
		programmedCondition.Message = "Secret is being provisioned for the workload"
		return false, nil
	}

	var secretIAMPolicy gcpsecretmanagerv1beta2.SecretIAMMember
	if err := r.DownstreamCluster.GetClient().Get(ctx, client.ObjectKeyFromObject(&secret), &secretIAMPolicy); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching secret's IAM policy: %w", err)
	}

	// Store secret information in the secret version
	// TODO(jreese) handle updates to secrets - this needs to be done by creating
	// a new secret version.
	secretVersion := &gcpsecretmanagerv1beta1.SecretVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: secret.Name,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.DownstreamCluster.GetClient(), secretVersion, func() error {
		if controllerutil.SetOwnerReference(&secret, secretVersion, r.DownstreamCluster.GetClient().Scheme()) != nil {
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
				ProviderConfigReference: secret.Spec.ResourceSpec.ProviderConfigReference,
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

	if secretVersion.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("secret version not ready yet", "secret", secret.Name, "secret_version", secretVersion.Name)
		programmedCondition.Reason = "ProvisioningSecretVersion"
		programmedCondition.Message = "Secret version is being provisioned for the workload"
		return false, nil
	}

	cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, cloudinit.WriteFile{
		Encoding:    "b64",
		Content:     base64.StdEncoding.EncodeToString([]byte(populateSecretsScript)),
		Owner:       "root:root",
		Path:        "/etc/secrets/populate_secrets.py",
		Permissions: "0755",
	})

	cloudConfig.RunCmd = append(
		cloudConfig.RunCmd,
		fmt.Sprintf("/etc/secrets/populate_secrets.py https://secretmanager.googleapis.com/v1/%s/versions/latest:access", *secret.Status.AtProvider.Name),
	)

	if secretIAMPolicy.CreationTimestamp.IsZero() {
		secretIAMPolicy = gcpsecretmanagerv1beta2.SecretIAMMember{
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
						Name: providerConfig.Name,
					},
				},
				ForProvider: gcpsecretmanagerv1beta2.SecretIAMMemberParameters{
					Role:   ptr.To("roles/secretmanager.secretAccessor"),
					Member: ptr.To(fmt.Sprintf("%s@%s.iam.gserviceaccount.com", serviceAccount.Annotations[crossplanemeta.AnnotationKeyExternalName], providerConfig.Spec.ProjectID)),
				},
			},
		}

		if err := controllerutil.SetOwnerReference(&secret, &secretIAMPolicy, r.DownstreamCluster.GetClient().Scheme()); err != nil {
			return false, fmt.Errorf("failed to set owner reference on secret IAM policy: %w", err)
		}

		if err := r.DownstreamCluster.GetClient().Create(ctx, &secretIAMPolicy); err != nil {
			return false, fmt.Errorf("failed setting IAM policy on secret: %w", err)
		}
	}

	return true, nil
}

func (r *InstanceReconciler) reconcileGCPInstance(
	ctx context.Context,
	clusterName string,
	providerConfig gcpv1beta1.ProviderConfig,
	upstreamClient client.Client,
	gcpZone string,
	workload *computev1alpha.Workload,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	instance *computev1alpha.Instance,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata map[string]*string,
	serviceAccount gcpcloudplatformv1beta1.ServiceAccount,
) (ctrl.Result, *gcpcomputev1beta2.Instance, error) {
	logger := log.FromContext(ctx)

	runtimeSpec := instance.Spec.Runtime

	gcpInstance := &gcpcomputev1beta2.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("instance-%s", instance.UID),
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.DownstreamCluster.GetClient(), gcpInstance, func() error {

		if gcpInstance.Annotations == nil {
			gcpInstance.Annotations = make(map[string]string)
		}

		if !controllerutil.ContainsFinalizer(gcpInstance, gcpInfraFinalizer) {
			controllerutil.AddFinalizer(gcpInstance, gcpInfraFinalizer)
		}

		gcpInstance.Annotations[downstreamclient.UpstreamOwnerName] = instance.Name
		gcpInstance.Annotations[downstreamclient.UpstreamOwnerNamespace] = instance.Namespace
		gcpInstance.Annotations[downstreamclient.UpstreamOwnerClusterName] = clusterName

		machineType, ok := machineTypeMap[runtimeSpec.Resources.InstanceType]
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
				Name: providerConfig.Name,
			},
		}

		gcpInstance.Spec.ForProvider.MachineType = ptr.To(machineType)
		gcpInstance.Spec.ForProvider.CanIPForward = ptr.To(true)
		gcpInstance.Spec.ForProvider.Metadata = instanceMetadata
		gcpInstance.Spec.ForProvider.Hostname = ptr.To(fmt.Sprintf("%s.cloud.datum-dns.net", instance.Name))
		gcpInstance.Spec.ForProvider.Zone = ptr.To(gcpZone)

		if gcpInstance.Spec.ForProvider.ServiceAccount == nil {
			gcpInstance.Spec.ForProvider.ServiceAccount = &gcpcomputev1beta2.ServiceAccountParameters{
				Scopes: []*string{
					ptr.To("cloud-platform"),
				},
				Email: serviceAccount.Status.AtProvider.Email,
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
		return ctrl.Result{}, nil, fmt.Errorf("failed to create gcp instance: %w", err)
	}

	logger.Info("downstream instance processed", "operation_result", result)

	if err := r.syncInstancePowerState(ctx, upstreamClient, instance, gcpInstance); err != nil {
		return ctrl.Result{}, nil, fmt.Errorf("failed to sync instance power state: %w", err)
	}

	return ctrl.Result{}, gcpInstance, nil
}

func (r *InstanceReconciler) buildGCPInstanceVolumes(
	ctx context.Context,
	cloudConfig *cloudinit.CloudConfig,
	instance *computev1alpha.Instance,
	gcpInstance *gcpcomputev1beta2.Instance,
) error {
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
						sourceImage, ok := imageMap[populator.Image.Name]
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

func (r *InstanceReconciler) syncInstancePowerState(
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

	var gcpInstance gcpcomputev1beta2.Instance
	gcpInstanceObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("instance-%s", instance.UID),
	}

	if err := r.DownstreamCluster.GetClient().Get(ctx, gcpInstanceObjectKey, &gcpInstance); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed fetching downstream instance: %w", err)
	}

	if dt := gcpInstance.DeletionTimestamp; !gcpInstance.CreationTimestamp.IsZero() && dt.IsZero() {
		if err := r.DownstreamCluster.GetClient().Delete(ctx, &gcpInstance); err != nil {
			return fmt.Errorf("failed to delete downstream instance: %w", err)
		}
	}

	// Wait for the instance to be deleted - crossplane doesn't update the
	// atProvider information or any status conditions when it's successfully
	// deleted a resource. Observing that the crossplane finalizer has been
	// removed is the best we've got.
	if !gcpInstance.CreationTimestamp.IsZero() && controllerutil.ContainsFinalizer(&gcpInstance, crossplaneFinalizer) {
		logger.Info("downstream instance is being deleted")
		return r.syncInstancePowerState(ctx, upstreamClient, instance, &gcpInstance)
	}

	logger.Info("downstream instance deleted")

	if controllerutil.RemoveFinalizer(&gcpInstance, gcpInfraFinalizer) {
		if err := r.DownstreamCluster.GetClient().Update(ctx, &gcpInstance); err != nil {
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
		Watches(&gcpcloudplatformv1beta1.ServiceAccount{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource(mgr)).
		Watches(&gcpsecretmanagerv1beta2.Secret{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource(mgr)).
		Watches(&gcpsecretmanagerv1beta1.SecretVersion{}, datumhandler.EnqueueInstancesForWorkloadOwnedDownstreamResource(mgr)).
		Watches(&gcpcomputev1beta2.Instance{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				instance := obj.(*gcpcomputev1beta2.Instance)
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
		}).
		Named("instance").
		Complete(r)
}
