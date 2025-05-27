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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
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

var errResourceIsDeleting = errors.New("resource is deleting")

// InstanceReconciler reconciles Instances and manages their intended state in
// GCP
type InstanceReconciler struct {
	mgr               mcmanager.Manager
	finalizers        finalizer.Finalizers
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

	_, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), *instance.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &instance)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(errResourceIsDeleting) {
			logger.Info("instance still has resources in GCP, waiting until removal")
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err = cl.GetClient().Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling instance")
	defer logger.Info("reconcile complete")

	if len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, waiting until they are removed", "scheduling_gates", instance.Spec.Controller.SchedulingGates)
		return ctrl.Result{}, nil
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
		return r.reconcileSandboxRuntimeInstance(ctx, req.ClusterName, cl.GetClient(), &workload, &workloadDeployment, &instance)
	} else if runtime.VirtualMachine != nil {
		return r.reconcileVMRuntimeInstance(ctx, req.ClusterName, cl.GetClient(), &workload, &workloadDeployment, &instance)
	}

	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileInstance(
	ctx context.Context,
	clusterName string,
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

	// TODO(jreese) sync power state

	defer func() {
		if err != nil {
			// Don't update the status if errors are encountered
			return
		}
		statusChanged := apimeta.SetStatusCondition(&instance.Status.Conditions, programmedCondition)

		if statusChanged {
			err = upstreamClient.Status().Update(ctx, instance)
		}
	}()

	// TODO
	// if err := r.reconcileNetworkInterfaceNetworkPolicies(ctx, upstreamClient, gcpProject, r.InfraClusterNamespaceName, deployment); err != nil {
	// 	return ctrl.Result{}, fmt.Errorf("failed reconciling network interface network policies: %w", err)
	// }

	// TODO(jreese) consider moving this out of the instance controller and into
	// one that handles workload scoped resources.

	// Service account names cannot exceed 30 characters
	// TODO(jreese) move to base36, as the underlying bytes won't be lost
	h := fnv.New32a()
	h.Write([]byte(workload.UID))

	var serviceAccount gcpcloudplatformv1beta1.ServiceAccount
	serviceAccountObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workload-%s", workload.UID),
	}
	if err := r.DownstreamCluster.GetClient().Get(ctx, serviceAccountObjectKey, &serviceAccount); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching workload's service account: %w", err)
	}

	if serviceAccount.CreationTimestamp.IsZero() {
		// TODO(jreese) have this owned by the workload
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
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: "project-test-fz3pr6", // TODO
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

	if serviceAccount.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("service account not ready yet")
		programmedCondition.Reason = "ProvisioningServiceAccount"
		programmedCondition.Message = "Service account is being provisioned for the workload"
		return ctrl.Result{}, nil
	}

	// TODO(jreese) add IAM Policy to the GCP service account to allow the service
	// account used by k8s-config-connector the `roles/iam.serviceAccountUser` role,
	// so that it can create instances with the service account without needing a
	// project level role binding. Probably just pass in the service account
	// email, otherwise we'd have to do some kind of discovery.

	if err := r.buildConfigMaps(ctx, upstreamClient, cloudConfig, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling configmaps: %w", err)
	}

	proceed, err := r.reconcileSecrets(
		ctx,
		clusterName,
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

	result, gcpInstance, err := r.reconcileGCPInstance(
		ctx,
		clusterName,
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

	// TODO(jreese) Reconcile power state

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

	aggregatedK8sSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      fmt.Sprintf("workload-%s", workload.UID),
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.DownstreamCluster.GetClient(), aggregatedK8sSecret, func() error {

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
	// TODO(jreese) handle updates
	var secret gcpsecretmanagerv1beta2.Secret

	secretObjectKey := client.ObjectKey{
		Name: fmt.Sprintf("workload-%s", workload.UID),
	}
	if err := r.DownstreamCluster.GetClient().Get(ctx, secretObjectKey, &secret); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching instance secret: %w", err)
	}

	if secret.CreationTimestamp.IsZero() {
		// TODO(jreese) have this owned by the workload
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
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: "project-test-fz3pr6", // TODO
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
		logger.Info("secret not ready yet")
		programmedCondition.Reason = "ProvisioningSecret"
		programmedCondition.Message = "Secret is being provisioned for the workload"
		return false, nil
	}

	var secretIAMPolicy gcpsecretmanagerv1beta2.SecretIAMMember
	if err := r.DownstreamCluster.GetClient().Get(ctx, client.ObjectKeyFromObject(&secret), &secretIAMPolicy); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching secret's IAM policy: %w", err)
	}

	if secretIAMPolicy.CreationTimestamp.IsZero() {
		secretIAMPolicy = gcpsecretmanagerv1beta2.SecretIAMMember{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: secret.Namespace,
				Name:      secret.Name,
			},
			Spec: gcpsecretmanagerv1beta2.SecretIAMMemberSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: "project-test-fz3pr6", // TODO
					},
				},
				ForProvider: gcpsecretmanagerv1beta2.SecretIAMMemberParameters{
					Role:   ptr.To("roles/secretmanager.secretAccessor"),
					Member: ptr.To(*serviceAccount.Status.AtProvider.Email),
				},
			},
		}

		if err := r.DownstreamCluster.GetClient().Create(ctx, &secretIAMPolicy); err != nil {
			return false, fmt.Errorf("failed setting IAM policy on secret: %w", err)
		}
	}

	// Store secret information in the secret version
	// TODO(jreese) handle updates to secrets - use Generation from aggregated
	// secret manifest?
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
				ProviderConfigReference: secret.Spec.ResourceSpec.ProviderConfigReference,
			},
			ForProvider: gcpsecretmanagerv1beta1.SecretVersionParameters{
				Enabled:        ptr.To(true),
				DeletionPolicy: ptr.To("DELETE"),
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
		logger.Info("secret version not ready yet")
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

	return true, nil
}

func (r *InstanceReconciler) reconcileGCPInstance(
	ctx context.Context,
	clusterName string,
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
			ProviderConfigReference: &crossplanecommonv1.Reference{
				Name: "project-test-fz3pr6", // TODO
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

func (r *InstanceReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {
	logger := log.FromContext(ctx)
	instance := obj.(*computev1alpha.Instance)

	gcpInstance := &gcpcomputev1beta2.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("instance-%s", instance.UID),
		},
	}

	if err := r.DownstreamCluster.GetClient().Delete(ctx, gcpInstance); client.IgnoreNotFound(err) != nil {
		return finalizer.Result{}, fmt.Errorf("failed to delete downstream instance: %w", err)
	}

	// TODO(jreese) wait for the instance to be deleted, need to move away from
	// the finalizer interface from controller-runtime to wait without passing
	// errors around.

	// Finalize secrets, service accounts, at a workload level.

	logger.Info("downstream instance deleted")

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpInfraFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

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
