// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path"
	"strconv"
	"strings"
	"time"

	_ "embed"

	kcccomputev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/compute/v1beta1"
	kcciamv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/iam/v1beta1"
	kcccomputev1alpha1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/k8s/v1alpha1"
	kccsecretmanagerv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/secretmanager/v1beta1"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/controller/k8sconfigconnector"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

var imageMap = map[string]string{
	"datumcloud/ubuntu-2204-lts":           "projects/ubuntu-os-cloud/global/images/ubuntu-2204-jammy-v20240927",
	"datumcloud/cos-stable-117-18613-0-79": "projects/cos-cloud/global/images/cos-stable-117-18613-0-79",
}

var machineTypeMap = map[string]string{
	"datumcloud/d1-standard-2": "n2-standard-2",
}

const gcpInfraFinalizer = "compute.datumapis.com/infra-provider-gcp-deployment-controller"
const deploymentNameLabel = "compute.datumapis.com/deployment-name"

//go:embed cloudinit/populate_secrets.py
var populateSecretsScript string

// WorkloadDeploymentReconciler reconciles a WorkloadDeployment object
type WorkloadDeploymentReconciler struct {
	client.Client
	InfraClient client.Client
	Scheme      *runtime.Scheme
	GCPProject  string

	finalizers finalizer.Finalizers
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments/finalizers,verbs=update

// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computefirewalls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computefirewalls/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancegroupmanagers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancegroupmanagers/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancetemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancetemplates/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computesubnetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computesubnetworks/status,verbs=get

// +kubebuilder:rbac:groups=iam.cnrm.cloud.google.com,resources=iampolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=iam.cnrm.cloud.google.com,resources=iampolicies/status,verbs=get
// +kubebuilder:rbac:groups=iam.cnrm.cloud.google.com,resources=iamserviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=iam.cnrm.cloud.google.com,resources=iamserviceaccounts/status,verbs=get

// +kubebuilder:rbac:groups=secretmanager.cnrm.cloud.google.com,resources=secretmanagersecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secretmanager.cnrm.cloud.google.com,resources=secretmanagersecrets/status,verbs=get
// +kubebuilder:rbac:groups=secretmanager.cnrm.cloud.google.com,resources=secretmanagersecretversions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secretmanager.cnrm.cloud.google.com,resources=secretmanagersecretversions/status,verbs=get

func (r *WorkloadDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var deployment computev1alpha.WorkloadDeployment
	if err := r.Client.Get(ctx, req.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	logger.Info("reconciling deployment")
	defer logger.Info("reconcile complete")

	finalizationResult, err := r.finalizers.Finalize(ctx, &deployment)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
	}
	if finalizationResult.Updated {
		if err = r.Client.Update(ctx, &deployment); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !deployment.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// TODO(jreese) for both this reconciler and the gateway one, handle updates
	// appropriately.

	// Don't do anything if a cluster isn't set
	if deployment.Status.ClusterRef == nil {
		return ctrl.Result{}, nil
	}

	runtime := deployment.Spec.Template.Spec.Runtime
	if runtime.Sandbox != nil {
		return r.reconcileSandboxRuntimeDeployment(ctx, logger, &deployment)
	} else if runtime.VirtualMachine != nil {
		return r.reconcileVMRuntimeDeployment(ctx, logger, &deployment)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDeploymentReconciler) SetupWithManager(mgr ctrl.Manager, infraCluster cluster.Cluster) error {
	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpInfraFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	// Watch the unstructured form of an instance group manager, as the generated
	// types are not aligned with the actual CRD.
	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha.WorkloadDeployment{}).
		Owns(&networkingv1alpha.NetworkBinding{}).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeFirewall{},
			handler.TypedEnqueueRequestForOwner[*kcccomputev1beta1.ComputeFirewall](mgr.GetScheme(), mgr.GetRESTMapper(), &computev1alpha.WorkloadDeployment{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeInstanceTemplate{},
			handler.TypedEnqueueRequestForOwner[*kcccomputev1beta1.ComputeInstanceTemplate](mgr.GetScheme(), mgr.GetRESTMapper(), &computev1alpha.WorkloadDeployment{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcciamv1beta1.IAMServiceAccount{},
			handler.TypedEnqueueRequestForOwner[*kcciamv1beta1.IAMServiceAccount](mgr.GetScheme(), mgr.GetRESTMapper(), &computev1alpha.WorkloadDeployment{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&instanceGroupManager,
			handler.TypedEnqueueRequestForOwner[*unstructured.Unstructured](mgr.GetScheme(), mgr.GetRESTMapper(), &computev1alpha.WorkloadDeployment{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kccsecretmanagerv1beta1.SecretManagerSecret{},
			handler.TypedEnqueueRequestForOwner[*kccsecretmanagerv1beta1.SecretManagerSecret](mgr.GetScheme(), mgr.GetRESTMapper(), &computev1alpha.WorkloadDeployment{}),
		)).
		Complete(r)
}

func (r *WorkloadDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	logger logr.Logger,
	deployment *computev1alpha.WorkloadDeployment,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata []kcccomputev1beta1.InstancetemplateMetadata,
) (res ctrl.Result, err error) {

	var cluster networkingv1alpha.DatumCluster
	clusterObjectKey := client.ObjectKey{
		Namespace: deployment.Status.ClusterRef.Namespace,
		Name:      deployment.Status.ClusterRef.Name,
	}
	if err := r.Client.Get(ctx, clusterObjectKey, &cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching cluster: %w", err)
	}

	if cluster.Spec.Provider.GCP == nil {
		return ctrl.Result{}, fmt.Errorf("attached cluster is not for the GCP provider")
	}

	// var gcpProject string
	gcpRegion := cluster.Spec.Provider.GCP.Region
	gcpZone := cluster.Spec.Provider.GCP.Zone

	// if len(gcpProject) == 0 {
	// 	return ctrl.Result{}, fmt.Errorf("failed to locate value for cluster property %s", ClusterPropertyProject)
	// }

	if len(gcpRegion) == 0 {
		return ctrl.Result{}, fmt.Errorf("failed to locate value for cluster property %s", ClusterPropertyRegion)
	}

	if len(gcpZone) == 0 {
		return ctrl.Result{}, fmt.Errorf("failed to locate value for cluster property %s", ClusterPropertyZone)
	}

	availableCondition := metav1.Condition{
		Type:               computev1alpha.WorkloadDeploymentAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             "DeploymentResourcesNotReady",
		ObservedGeneration: deployment.Generation,
		Message:            "Deployment resources are not ready",
	}

	defer func() {
		if err != nil {
			// Don't update the status if errors are encountered
			return
		}
		statusChanged := apimeta.SetStatusCondition(&deployment.Status.Conditions, availableCondition)

		if statusChanged {
			err = r.Client.Status().Update(ctx, deployment)
		}
	}()

	if err := r.reconcileNetworkInterfaceNetworkPolicies(ctx, logger, deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling network interface network policies: %w", err)
	}

	// Service account names cannot exceed 30 characters
	h := fnv.New32a()
	h.Write([]byte(deployment.Spec.WorkloadRef.UID))

	var serviceAccount kcciamv1beta1.IAMServiceAccount
	serviceAccountObjectKey := client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      fmt.Sprintf("workload-%d", h.Sum32()),
	}
	if err := r.InfraClient.Get(ctx, serviceAccountObjectKey, &serviceAccount); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching deployment's service account: %w", err)
	}

	if serviceAccount.CreationTimestamp.IsZero() {
		serviceAccount = kcciamv1beta1.IAMServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: serviceAccountObjectKey.Namespace,
				Name:      serviceAccountObjectKey.Name,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kcciamv1beta1.IAMServiceAccountSpec{
				Description: proto.String(fmt.Sprintf("service account for workload %s", deployment.Spec.WorkloadRef.UID)),
			},
		}

		if err := controllerutil.SetControllerReference(deployment, &serviceAccount, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set controller on service account: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &serviceAccount); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create deployment's service account: %w", err)
		}
	}

	if !k8sconfigconnector.IsStatusConditionTrue(serviceAccount.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("service account not ready yet")
		availableCondition.Reason = "ServiceAccountNotReady"
		return ctrl.Result{}, nil
	}

	if err := r.reconcileConfigMaps(ctx, cloudConfig, deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reconciling configmaps: %w", err)
	}

	proceed, err := r.reconcileSecrets(ctx, logger, &availableCondition, cloudConfig, deployment, serviceAccount)
	if !proceed || err != nil {
		return ctrl.Result{}, err
	}

	result, instanceTemplate, oldInstanceTemplate, err := r.reconcileInstanceTemplate(ctx, logger, gcpRegion, &availableCondition, deployment, cloudConfig, instanceMetadata, &serviceAccount)
	if !result.IsZero() || err != nil {
		return result, err
	}

	if !k8sconfigconnector.IsStatusConditionTrue(instanceTemplate.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("instance template not ready yet")
		availableCondition.Reason = "InstanceTemplateNotReady"
		return ctrl.Result{}, nil
	}

	instanceGroupManager, err := r.reconcileInstanceGroupManager(ctx, logger, gcpZone, &availableCondition, deployment, instanceTemplate)
	if err != nil {
		return ctrl.Result{}, err
	}

	proceed, err = r.checkInstanceGroupManagerReadiness(logger, &availableCondition, instanceGroupManager)
	if !proceed || err != nil {
		return ctrl.Result{}, err
	}

	result, err = r.updateDeploymentStatus(ctx, logger, &availableCondition, deployment, instanceGroupManager)
	if !result.IsZero() || err != nil {
		return result, err
	}

	if !oldInstanceTemplate.CreationTimestamp.IsZero() {
		logger.Info("deleting old instance template")
		if err := r.InfraClient.Delete(ctx, oldInstanceTemplate); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete instance instance template: %w", err)
		}

		logger.Info("old instance template deleted")
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadDeploymentReconciler) reconcileSandboxRuntimeDeployment(
	ctx context.Context,
	logger logr.Logger,
	deployment *computev1alpha.WorkloadDeployment,
) (ctrl.Result, error) {
	logger.Info("processing sandbox based workload")

	runtimeSpec := deployment.Spec.Template.Spec.Runtime

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
	for _, v := range deployment.Spec.Template.Spec.Volumes {
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
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
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
		r.Scheme,
		r.Scheme,
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
	deployment = deployment.DeepCopy()
	deployment.Spec.Template.Spec.Volumes = append([]computev1alpha.InstanceVolume{
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
	}, deployment.Spec.Template.Spec.Volumes...)

	return r.reconcileDeployment(
		ctx,
		logger,
		deployment,
		cloudConfig,
		nil,
	)
}

func (r *WorkloadDeploymentReconciler) reconcileVMRuntimeDeployment(
	ctx context.Context,
	logger logr.Logger,
	deployment *computev1alpha.WorkloadDeployment,
) (ctrl.Result, error) {

	logger.Info("processing VM based workload")

	runtimeSpec := deployment.Spec.Template.Spec.Runtime

	instanceMetadata := []kcccomputev1beta1.InstancetemplateMetadata{
		{
			Key:   "ssh-keys",
			Value: deployment.Spec.Template.Annotations[computev1alpha.SSHKeysAnnotation],
		},
	}

	volumeMap := map[string]computev1alpha.VolumeSource{}
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		volumeMap[v.Name] = v.VolumeSource
	}

	cloudConfig := &cloudinit.CloudConfig{}

	mountParentDirs := sets.Set[string]{}
	for _, attachment := range runtimeSpec.VirtualMachine.VolumeAttachments {
		if attachment.MountPath != nil {
			volume := volumeMap[attachment.Name]

			if volume.Disk != nil {
				// Currently handed inside `buildInstanceTemplateVolumes`
			}

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

	return r.reconcileDeployment(
		ctx,
		logger,
		deployment,
		cloudConfig,
		instanceMetadata,
	)
}

func (r *WorkloadDeploymentReconciler) reconcileNetworkInterfaceNetworkPolicies(
	ctx context.Context,
	logger logr.Logger,
	deployment *computev1alpha.WorkloadDeployment,
) error {
	for interfaceIndex, networkInterface := range deployment.Spec.Template.Spec.NetworkInterfaces {
		interfacePolicy := networkInterface.NetworkPolicy
		if interfacePolicy == nil {
			continue
		}

		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: deployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", deployment.Name, interfaceIndex),
		}

		if err := r.Client.Get(ctx, networkBindingObjectKey, &networkBinding); err != nil {
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
		if err := r.Client.Get(ctx, networkContextObjectKey, &networkContext); err != nil {
			return fmt.Errorf("failed fetching network context: %w", err)
		}

		// TODO(jreese) change this to where a higher level datum controller makes a
		// network policy in the network service as a result of reacting to a
		// workload being created that has an interface policy

		for ruleIndex, ingressRule := range interfacePolicy.Ingress {

			firewallName := fmt.Sprintf("%s-net-%d-%d", deployment.Name, interfaceIndex, ruleIndex)

			var firewall kcccomputev1beta1.ComputeFirewall
			firewallObjectKey := client.ObjectKey{
				Namespace: deployment.Namespace,
				Name:      firewallName,
			}

			if err := r.InfraClient.Get(ctx, firewallObjectKey, &firewall); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to read firewall from k8s API: %w", err)
			}

			if firewall.CreationTimestamp.IsZero() {
				logger.Info("creating firewall for interface policy rule")
				firewall = kcccomputev1beta1.ComputeFirewall{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: deployment.Namespace,
						Name:      firewallName,
						Annotations: map[string]string{
							GCPProjectAnnotation: r.GCPProject,
						},
					},
					Spec: kcccomputev1beta1.ComputeFirewallSpec{
						Description: proto.String(fmt.Sprintf(
							"instance interface policy for %s: interfaceIndex:%d, ruleIndex:%d",
							deployment.Name,
							interfaceIndex,
							ruleIndex,
						)),
						Direction: proto.String("INGRESS"),
						NetworkRef: kcccomputev1alpha1.ResourceRef{
							Namespace: deployment.Namespace,
							Name:      fmt.Sprintf("network-%s", networkContext.UID),
						},
						Priority: proto.Int64(65534),
						TargetTags: []string{
							fmt.Sprintf("deployment-%s", deployment.UID),
						},
					},
				}

				if err := controllerutil.SetControllerReference(deployment, &firewall, r.Scheme); err != nil {
					return fmt.Errorf("failed to set controller on firewall: %w", err)
				}

				for _, port := range ingressRule.Ports {
					ipProtocol := "tcp"
					if port.Protocol != nil {
						ipProtocol = strings.ToLower(string(*port.Protocol))
					}

					var gcpPorts []string
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

						gcpPorts = append(gcpPorts, gcpPort)
					}

					firewall.Spec.Allow = append(firewall.Spec.Allow, kcccomputev1beta1.FirewallAllow{
						Protocol: ipProtocol,
						Ports:    gcpPorts,
					})
				}

				for _, peer := range ingressRule.From {
					if peer.IPBlock != nil {
						firewall.Spec.SourceRanges = append(firewall.Spec.SourceRanges, peer.IPBlock.CIDR)
						// TODO(jreese) implement IPBlock.Except as a separate rule of one higher priority
					}
				}

				if err := r.InfraClient.Create(ctx, &firewall); err != nil {
					return fmt.Errorf("failed to create firewall: %w", err)
				}
			}
		}
	}
	return nil
}

func (r *WorkloadDeploymentReconciler) reconcileConfigMaps(
	ctx context.Context,
	cloudConfig *cloudinit.CloudConfig,
	deployment *computev1alpha.WorkloadDeployment,
) error {
	var objectKeys []client.ObjectKey
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.ConfigMap != nil {
			objectKeys = append(objectKeys, client.ObjectKey{
				Namespace: deployment.Namespace,
				Name:      volume.ConfigMap.Name,
			})
		}
	}

	if len(objectKeys) == 0 {
		return nil
	}

	for _, configMapObjectKey := range objectKeys {
		var configMap corev1.ConfigMap
		if err := r.Client.Get(ctx, configMapObjectKey, &configMap); err != nil {
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

func (r *WorkloadDeploymentReconciler) reconcileSecrets(
	ctx context.Context,
	logger logr.Logger,
	availableCondition *metav1.Condition,
	cloudConfig *cloudinit.CloudConfig,
	deployment *computev1alpha.WorkloadDeployment,
	serviceAccount kcciamv1beta1.IAMServiceAccount,
) (bool, error) {
	var objectKeys []client.ObjectKey
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			objectKeys = append(objectKeys, client.ObjectKey{
				Namespace: deployment.Namespace,
				Name:      volume.Secret.SecretName,
			})
		}
	}

	if len(objectKeys) == 0 {
		return true, nil
	}

	var secret kccsecretmanagerv1beta1.SecretManagerSecret

	// Aggregate secret data into one value by creating a map of secret names
	// to content. This will allow for mounting of keys into volumes or secrets
	// as expected.
	secretData := map[string]map[string][]byte{}
	for _, objectKey := range objectKeys {
		var k8ssecret corev1.Secret
		if err := r.Client.Get(ctx, objectKey, &k8ssecret); err != nil {
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
			Namespace: deployment.Namespace,
			Name:      fmt.Sprintf("deployment-%s", deployment.UID),
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.InfraClient, aggregatedK8sSecret, func() error {
		if aggregatedK8sSecret.CreationTimestamp.IsZero() {
			if err := controllerutil.SetControllerReference(deployment, aggregatedK8sSecret, r.Scheme); err != nil {
				return fmt.Errorf("failed to set controller on aggregated deployment secret: %w", err)
			}
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

	secretObjectKey := client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      fmt.Sprintf("deployment-%s", deployment.UID),
	}
	if err := r.InfraClient.Get(ctx, secretObjectKey, &secret); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching deployment secret: %w", err)
	}

	if secret.CreationTimestamp.IsZero() {
		secret = kccsecretmanagerv1beta1.SecretManagerSecret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: secretObjectKey.Namespace,
				Name:      secretObjectKey.Name,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kccsecretmanagerv1beta1.SecretManagerSecretSpec{
				Replication: &kccsecretmanagerv1beta1.SecretReplication{
					Automatic: proto.Bool(true),
				},
			},
		}

		if err := controllerutil.SetControllerReference(deployment, &secret, r.Scheme); err != nil {
			return false, fmt.Errorf("failed to set controller on deployment secret manager secret: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &secret); err != nil {
			return false, fmt.Errorf("failed to create deployment secret: %w", err)
		}
	}

	if !k8sconfigconnector.IsStatusConditionTrue(secret.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("secret not ready yet")
		availableCondition.Reason = "SecretNotReady"
		return false, nil
	}

	var secretIAMPolicy kcciamv1beta1.IAMPolicy
	if err := r.InfraClient.Get(ctx, client.ObjectKeyFromObject(&secret), &secretIAMPolicy); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching secret's IAM policy: %w", err)
	}

	if secretIAMPolicy.CreationTimestamp.IsZero() {
		secretIAMPolicy = kcciamv1beta1.IAMPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: secret.Namespace,
				Name:      secret.Name,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kcciamv1beta1.IAMPolicySpec{
				ResourceRef: kcccomputev1alpha1.IAMResourceRef{
					Kind:      "SecretManagerSecret",
					Namespace: secret.Namespace,
					Name:      secret.Name,
				},
				Bindings: []kcciamv1beta1.PolicyBindings{
					{
						Role: "roles/secretmanager.secretAccessor",
						Members: []string{
							*serviceAccount.Status.Member,
						},
					},
				},
			},
		}

		if err := controllerutil.SetControllerReference(deployment, &secretIAMPolicy, r.Scheme); err != nil {
			return false, fmt.Errorf("failed to set controller on deployment secret IAM policy: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &secretIAMPolicy); err != nil {
			return false, fmt.Errorf("failed setting IAM policy on secret: %w", err)
		}
	}

	// Store secret information in the secret version
	// TODO(jreese) handle updates to secrets - use Generation from aggregated
	// secret manifest?
	var secretVersion kccsecretmanagerv1beta1.SecretManagerSecretVersion
	if err := r.InfraClient.Get(ctx, client.ObjectKeyFromObject(&secret), &secretVersion); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("failed fetching secret manager version: %w", err)
	}

	if secretVersion.CreationTimestamp.IsZero() {
		// TODO(jreese) create new versions on updates
		secretVersion = kccsecretmanagerv1beta1.SecretManagerSecretVersion{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: secret.Namespace,
				Name:      secret.Name,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kccsecretmanagerv1beta1.SecretManagerSecretVersionSpec{
				Enabled: proto.Bool(true),
				SecretData: kccsecretmanagerv1beta1.SecretversionSecretData{
					ValueFrom: &kccsecretmanagerv1beta1.SecretversionValueFrom{
						SecretKeyRef: &kcccomputev1alpha1.SecretKeyRef{
							Key:  "secretData",
							Name: aggregatedK8sSecret.Name,
						},
					},
				},
				SecretRef: kcccomputev1alpha1.ResourceRef{
					Namespace: secret.Namespace,
					Name:      secret.Name,
				},
			},
		}

		if err := controllerutil.SetControllerReference(deployment, &secretVersion, r.Scheme); err != nil {
			return false, fmt.Errorf("failed to set controller on secret version: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &secretVersion); err != nil {
			return false, fmt.Errorf("failed to create secret version: %w", err)
		}
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
		fmt.Sprintf("/etc/secrets/populate_secrets.py https://secretmanager.googleapis.com/v1/%s/versions/latest:access", *secret.Status.Name),
	)

	return true, nil
}

func (r *WorkloadDeploymentReconciler) reconcileInstanceTemplate(
	ctx context.Context,
	logger logr.Logger,
	gcpRegion string,
	availableCondition *metav1.Condition,
	deployment *computev1alpha.WorkloadDeployment,
	cloudConfig *cloudinit.CloudConfig,
	instanceMetadata []kcccomputev1beta1.InstancetemplateMetadata,
	serviceAccount *kcciamv1beta1.IAMServiceAccount,
) (ctrl.Result, *kcccomputev1beta1.ComputeInstanceTemplate, *kcccomputev1beta1.ComputeInstanceTemplate, error) {

	var instanceTemplate kcccomputev1beta1.ComputeInstanceTemplate
	var oldInstanceTemplate kcccomputev1beta1.ComputeInstanceTemplate

	var instanceTemplates kcccomputev1beta1.ComputeInstanceTemplateList
	if err := r.InfraClient.List(
		ctx,
		&instanceTemplates,
		client.MatchingLabels{
			deploymentNameLabel: deployment.Name,
		},
	); err != nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("unable to list instance templates: %w", err)
	}

	instanceTemplateName := fmt.Sprintf("deployment-%s-gen%d", deployment.UID, deployment.Generation)
	if len(instanceTemplates.Items) > 0 {
		for _, t := range instanceTemplates.Items {
			if t.Name == instanceTemplateName {
				instanceTemplate = t
			} else {
				oldInstanceTemplate = t
			}
		}
	}

	runtimeSpec := deployment.Spec.Template.Spec.Runtime

	if instanceTemplate.CreationTimestamp.IsZero() {
		availableCondition.Reason = "InstanceTemplateDoesNotExist"
		logger.Info("instance template does not exist")
		machineType, ok := machineTypeMap[runtimeSpec.Resources.InstanceType]
		if !ok {
			return ctrl.Result{}, nil, nil, fmt.Errorf("unable to map datum instance type: %s", runtimeSpec.Resources.InstanceType)
		}

		userData, err := cloudConfig.Generate()
		if err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("failed generating cloud init user data: %w", err)
		}

		instanceMetadata = append(instanceMetadata, kcccomputev1beta1.InstancetemplateMetadata{
			Key:   "user-data",
			Value: fmt.Sprintf("#cloud-config\n\n%s", string(userData)),
		})

		instanceTemplate = kcccomputev1beta1.ComputeInstanceTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: deployment.Namespace,
				Name:      instanceTemplateName,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
				Labels: map[string]string{
					deploymentNameLabel: deployment.Name,
				},
			},
			Spec: kcccomputev1beta1.ComputeInstanceTemplateSpec{
				MachineType:  machineType,
				CanIpForward: proto.Bool(true),
				Metadata:     instanceMetadata,
				ServiceAccount: &kcccomputev1beta1.InstancetemplateServiceAccount{
					Scopes: []string{"cloud-platform"},
					ServiceAccountRef: &kcccomputev1alpha1.ResourceRef{
						Namespace: serviceAccount.Namespace,
						Name:      serviceAccount.Name,
					},
				},
				Tags: []string{
					fmt.Sprintf("workload-%s", deployment.Spec.WorkloadRef.UID),
					fmt.Sprintf("deployment-%s", deployment.UID),
				},
			},
		}

		if err := r.buildInstanceTemplateVolumes(logger, cloudConfig, deployment, &instanceTemplate); err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("failed to build instance template volumes: %w", err)
		}

		result, err := r.buildInstanceTemplateNetworkInterfaces(ctx, logger, gcpRegion, availableCondition, deployment, &instanceTemplate)
		if err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("failed to build instance template network interfaces: %w", err)
		} else if !result.IsZero() {
			logger.Info("network environment is not ready to attach")
			return result, nil, nil, nil
		}

		if err := controllerutil.SetControllerReference(deployment, &instanceTemplate, r.Scheme); err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("failed to set controller on firewall: %w", err)
		}

		logger.Info("creating instance template for workload")
		if err := r.InfraClient.Create(ctx, &instanceTemplate); err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("failed to create instance template: %w", err)
		}

		return ctrl.Result{}, nil, nil, nil
	}
	return ctrl.Result{}, &instanceTemplate, &oldInstanceTemplate, nil
}

func (r *WorkloadDeploymentReconciler) buildInstanceTemplateVolumes(
	logger logr.Logger,
	cloudConfig *cloudinit.CloudConfig,
	deployment *computev1alpha.WorkloadDeployment,
	instanceTemplate *kcccomputev1beta1.ComputeInstanceTemplate,
) error {
	for volumeIndex, volume := range deployment.Spec.Template.Spec.Volumes {
		disk := kcccomputev1beta1.InstancetemplateDisk{
			AutoDelete: proto.Bool(true),
			Labels: map[string]string{
				"volume_name": volume.Name,
			},
		}

		if volume.Disk != nil {
			if volume.Disk.Template != nil {
				diskTemplate := volume.Disk.Template

				// TODO(jreese) we'll need to have our images have different udev rules
				// so that device names are enumerated at `/dev/disk/by-id/datumcloud-*`
				// instead of `/dev/disk/by-id/google-*`
				disk.DiskType = proto.String(diskTemplate.Spec.Type)

				if volume.Disk.DeviceName == nil {
					disk.DeviceName = proto.String(fmt.Sprintf("volume-%d", volumeIndex))
				} else {
					disk.DeviceName = proto.String(*volume.Disk.DeviceName)
				}

				if populator := diskTemplate.Spec.Populator; populator != nil {
					if populator.Image != nil {
						// TODO(jreese) Should we only allow one volume to be populated by
						// an image per instance?
						disk.Boot = proto.Bool(true)
						// Should be prevented by validation, but be safe
						sourceImage, ok := imageMap[populator.Image.Name]
						if !ok {
							return fmt.Errorf("unable to map datum image name: %s", populator.Image.Name)
						}

						disk.SourceImageRef = &kcccomputev1alpha1.ResourceRef{
							External: sourceImage,
						}
					}

					if populator.Filesystem != nil {
						// Filesystem based populator, add cloud-init data to format the disk
						// and make the volume available to mount into containers.

						// TODO(jreese) we'll need to have our images have different udev rules
						// so that device names are enumerated at `/dev/disk/by-id/datumcloud-*`
						// instead of `/dev/disk/by-id/google-*`

						devicePath := fmt.Sprintf("/dev/disk/by-id/google-%s", *disk.DeviceName)

						cloudConfig.FSSetup = append(cloudConfig.FSSetup, cloudinit.FSSetup{
							Label:      fmt.Sprintf("disk-%s", volume.Name),
							Filesystem: populator.Filesystem.Type,
							Device:     devicePath,
						})

						runtime := deployment.Spec.Template.Spec.Runtime

						if runtime.Sandbox != nil {
							cloudConfig.Mounts = append(cloudConfig.Mounts,
								fmt.Sprintf("[%s, %s]", devicePath, fmt.Sprintf("/mnt/disk-%s", volume.Name)),
							)
						}

						if runtime.VirtualMachine != nil {
							for _, attachment := range runtime.VirtualMachine.VolumeAttachments {
								if attachment.Name != volume.Name {
									continue
								}

								if attachment.MountPath == nil {
									logger.Info("unexpected VM attachment with no mount path for filesystem populated volume", "attachment_name", attachment.Name)
									continue
								}

								cloudConfig.Mounts = append(cloudConfig.Mounts,
									fmt.Sprintf("[%s, %s]", devicePath, *attachment.MountPath),
								)
							}
						}
					}
				}

				if diskTemplate.Spec.Resources != nil {
					if storage, ok := diskTemplate.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
						return fmt.Errorf("unable to locate storage resource request for volume: %s", volume.Name)
					} else {
						disk.DiskSizeGb = proto.Int64(storage.Value() / (1024 * 1024 * 1024))
					}
				}

				instanceTemplate.Spec.Disk = append(instanceTemplate.Spec.Disk, disk)
			}
		}
	}

	return nil
}

func (r *WorkloadDeploymentReconciler) buildInstanceTemplateNetworkInterfaces(
	ctx context.Context,
	logger logr.Logger,
	gcpRegion string,
	availableCondition *metav1.Condition,
	deployment *computev1alpha.WorkloadDeployment,
	instanceTemplate *kcccomputev1beta1.ComputeInstanceTemplate,
) (ctrl.Result, error) {
	for interfaceIndex := range deployment.Spec.Template.Spec.NetworkInterfaces {
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: deployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", deployment.Name, interfaceIndex),
		}

		if err := r.Client.Get(ctx, networkBindingObjectKey, &networkBinding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching network binding for interface: %w", err)
		}

		if networkBinding.Status.NetworkContextRef == nil {
			return ctrl.Result{}, fmt.Errorf("network binding not associated with network context")
		}

		var networkContext networkingv1alpha.NetworkContext
		networkContextObjectKey := client.ObjectKey{
			Namespace: networkBinding.Status.NetworkContextRef.Namespace,
			Name:      networkBinding.Status.NetworkContextRef.Name,
		}
		if err := r.Client.Get(ctx, networkContextObjectKey, &networkContext); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching network context: %w", err)
		}

		// Get subnet that should be used for instances
		// TODO(jreese) filter on subnet class
		var subnetClaims networkingv1alpha.SubnetClaimList
		listOpts := []client.ListOption{
			client.InNamespace(networkContext.Namespace),
			client.MatchingLabels{
				"cloud.datum.net/network-context": networkContext.Name,
				"gcp.topology.datum.net/region":   gcpRegion,
				"gcp.topology.datum.net/project":  r.GCPProject,
			},
		}

		if err := r.Client.List(ctx, &subnetClaims, listOpts...); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching subnet claims: %w", err)
		}

		if len(subnetClaims.Items) == 0 {
			logger.Info("creating subnet claim")
			// TODO(jreese) This is not the best long term location for subnet claims
			// to be created. Need to review this. Note how we list subnet claims, but
			// create one with a specific name. This won't work out in the current
			// logic if another subnet is required. This really should be done
			// elsewhere. Perhaps take a SchedulingGate approach, and have a separate
			// controller deal with subnet needs for deployments in a cluster, and
			// remove the gate when things are ready.

			subnetClaim := networkingv1alpha.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: networkContext.Namespace,
					Name:      fmt.Sprintf("gcp-%s", gcpRegion),
					Labels: map[string]string{
						"cloud.datum.net/network-context": networkContext.Name,
						"gcp.topology.datum.net/region":   gcpRegion,
						"gcp.topology.datum.net/project":  r.GCPProject,
					},
				},
				Spec: networkingv1alpha.SubnetClaimSpec{
					SubnetClass: "private",
					IPFamily:    networkingv1alpha.IPv4Protocol,
					NetworkContext: networkingv1alpha.LocalNetworkContextRef{
						Name: networkContext.Name,
					},
					Topology: map[string]string{
						"gcp.topology.datum.net/region":  gcpRegion,
						"gcp.topology.datum.net/project": r.GCPProject,
					},
				},
			}

			if err := r.Client.Create(ctx, &subnetClaim); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating subnet claim: %w", err)
			}

			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}

		subnetClaim := subnetClaims.Items[0]

		if !apimeta.IsStatusConditionTrue(subnetClaim.Status.Conditions, "Ready") {
			availableCondition.Reason = "SubnetClaimNotReady"
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}

		var subnet networkingv1alpha.Subnet
		subnetObjectKey := client.ObjectKey{
			Namespace: subnetClaim.Namespace,
			Name:      subnetClaim.Status.SubnetRef.Name,
		}
		if err := r.Client.Get(ctx, subnetObjectKey, &subnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching subnet: %w", err)
		}

		if !apimeta.IsStatusConditionTrue(subnet.Status.Conditions, "Ready") {
			availableCondition.Reason = "SubnetNotReady"
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}

		var kccSubnet kcccomputev1beta1.ComputeSubnetwork
		kccSubnetObjectKey := client.ObjectKey{
			Namespace: subnetClaim.Namespace,
			Name:      fmt.Sprintf("%s-%s", networkContext.Name, subnetClaim.Status.SubnetRef.Name),
		}
		if err := r.InfraClient.Get(ctx, kccSubnetObjectKey, &kccSubnet); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching GCP subnetwork: %w", err)
		}

		if kccSubnet.CreationTimestamp.IsZero() {
			kccSubnet = kcccomputev1beta1.ComputeSubnetwork{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: kccSubnetObjectKey.Namespace,
					Name:      kccSubnetObjectKey.Name,
					Annotations: map[string]string{
						GCPProjectAnnotation: r.GCPProject,
					},
				},
				Spec: kcccomputev1beta1.ComputeSubnetworkSpec{
					IpCidrRange: fmt.Sprintf("%s/%d", *subnet.Status.StartAddress, *subnet.Status.PrefixLength),
					NetworkRef: kcccomputev1alpha1.ResourceRef{
						Namespace: deployment.Namespace,
						Name:      fmt.Sprintf("network-%s", networkContext.UID),
					},
					Purpose: proto.String("PRIVATE"),
					Region:  gcpRegion,
					// TODO(jreese) ipv6
					StackType: proto.String("IPV4_ONLY"),
				},
			}

			if err := controllerutil.SetControllerReference(&subnet, &kccSubnet, r.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to set controller on GCP subnetwork: %w", err)
			}

			if err := r.InfraClient.Create(ctx, &kccSubnet); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating GCP subnetwork: %w", err)
			}
		}

		if !k8sconfigconnector.IsStatusConditionTrue(kccSubnet.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
			availableCondition.Reason = "SubnetNotReady"
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		gcpInterface := kcccomputev1beta1.InstancetemplateNetworkInterface{
			NetworkRef: &kcccomputev1alpha1.ResourceRef{
				Namespace: deployment.Namespace,
				Name:      fmt.Sprintf("network-%s", networkContext.UID),
			},
			AccessConfig: []kcccomputev1beta1.InstancetemplateAccessConfig{
				{
					// TODO(jreese) only enable this if instructed by workload spec
					// TODO(jreese) bleh: https://github.com/GoogleCloudPlatform/k8s-config-connector/issues/329

					// ONE_TO_ONE_NAT is enabled by default. We'll need the above fixed
					// if we want to be able to omit the NAT ip

					// NatIpRef: &kcccomputev1alpha1.ResourceRef{},
				},
			},
			SubnetworkRef: &kcccomputev1alpha1.ResourceRef{
				Namespace: kccSubnet.Namespace,
				Name:      kccSubnet.Name,
			},
		}
		instanceTemplate.Spec.NetworkInterface = append(instanceTemplate.Spec.NetworkInterface, gcpInterface)
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadDeploymentReconciler) reconcileInstanceGroupManager(
	ctx context.Context,
	logger logr.Logger,
	gcpZone string,
	availableCondition *metav1.Condition,
	deployment *computev1alpha.WorkloadDeployment,
	instanceTemplate *kcccomputev1beta1.ComputeInstanceTemplate,
) (*unstructured.Unstructured, error) {
	instanceGroupManagerName := fmt.Sprintf("deployment-%s", deployment.UID)

	// Unstructured is used here due to bugs in type generation. We'll likely
	// completely move away from this to our own per-instance control though.
	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)
	instanceGroupManagerObjectKey := client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      instanceGroupManagerName,
	}
	if err := r.InfraClient.Get(ctx, instanceGroupManagerObjectKey, &instanceGroupManager); client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("failed fetching instance group manager: %w", err)
	}

	if t := instanceGroupManager.GetCreationTimestamp(); t.IsZero() {
		availableCondition.Reason = "InstanceGroupManagerDoesNotExist"
		var namedPorts []kcccomputev1beta1.InstancegroupmanagerNamedPorts
		if sb := deployment.Spec.Template.Spec.Runtime.Sandbox; sb != nil {
			for _, c := range sb.Containers {
				for _, p := range c.Ports {
					namedPorts = append(namedPorts, kcccomputev1beta1.InstancegroupmanagerNamedPorts{
						Name: proto.String(p.Name),
						Port: proto.Int64(int64(p.Port)),
					})
				}
			}
		}

		if vm := deployment.Spec.Template.Spec.Runtime.VirtualMachine; vm != nil {
			for _, p := range vm.Ports {
				namedPorts = append(namedPorts, kcccomputev1beta1.InstancegroupmanagerNamedPorts{
					Name: proto.String(p.Name),
					Port: proto.Int64(int64(p.Port)),
				})
			}
		}

		instanceGroupManager := &kcccomputev1beta1.ComputeInstanceGroupManager{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: deployment.Namespace,
				Name:      instanceGroupManagerName,
			},
			Spec: kcccomputev1beta1.ComputeInstanceGroupManagerSpec{
				ProjectRef: kcccomputev1alpha1.ResourceRef{
					External: r.GCPProject,
				},
				Location:         proto.String(gcpZone),
				BaseInstanceName: proto.String(fmt.Sprintf("%s-#", deployment.Name)),
				InstanceTemplateRef: &kcccomputev1alpha1.ResourceRef{
					Namespace: instanceTemplate.Namespace,
					Name:      instanceTemplate.Name,
				},

				NamedPorts: namedPorts,
				UpdatePolicy: &kcccomputev1beta1.InstancegroupmanagerUpdatePolicy{
					Type:                        proto.String("PROACTIVE"),
					MinimalAction:               proto.String("RESTART"),
					MostDisruptiveAllowedAction: proto.String("RESTART"),
				},
			},
		}

		logger.Info("creating instance group manager", "name", instanceGroupManager.Name)
		if err := controllerutil.SetControllerReference(deployment, instanceGroupManager, r.Scheme); err != nil {
			return nil, fmt.Errorf("failed to set controller on firewall: %w", err)
		}

		// Work around bug in generated struct having the wrong type for TargetSize
		unstructuredInstanceGroupManager, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(instanceGroupManager)
		if err != nil {
			return nil, fmt.Errorf("failed to convert instance group manager to unstructured type: %w", err)
		}

		// Have to set maxReplicas as an int64 due to DeepCopy logic not handling
		// int32s correctly.
		maxReplicas := int64(deployment.Spec.ScaleSettings.MinReplicas)
		if err := unstructured.SetNestedField(unstructuredInstanceGroupManager, maxReplicas, "spec", "targetSize"); err != nil {
			return nil, fmt.Errorf("failed to set target size: %w", err)
		}

		logger.Info("creating instance group manager for workload")
		u := &unstructured.Unstructured{
			Object: unstructuredInstanceGroupManager,
		}
		u.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)
		if err := r.InfraClient.Create(ctx, u); err != nil {
			return nil, fmt.Errorf("failed to create instance group manager: %w", err)
		}

		logger.Info(
			"instance group manager created",
		)

		return u, nil
	} else {
		instanceTemplateName, ok, err := unstructured.NestedString(instanceGroupManager.Object, "spec", "instanceTemplateRef", "name")
		if !ok || err != nil {
			return nil, fmt.Errorf("failed to get instance template ref from instance group manager")
		}

		if instanceTemplateName != instanceTemplate.Name {
			logger.Info("updating instance group manager template", "template_name", instanceTemplate.Name)
			if err := unstructured.SetNestedField(instanceGroupManager.Object, instanceTemplate.Name, "spec", "instanceTemplateRef", "name"); err != nil {
				return nil, fmt.Errorf("failed setting instance template ref name: %w", err)
			}

			if err := r.InfraClient.Update(ctx, &instanceGroupManager); err != nil {
				return nil, fmt.Errorf("failed updating instance template for instance group manager: %w", err)
			}
		}
		return &instanceGroupManager, nil
	}
}

func (r *WorkloadDeploymentReconciler) updateDeploymentStatus(
	ctx context.Context,
	logger logr.Logger,
	availableCondition *metav1.Condition,
	deployment *computev1alpha.WorkloadDeployment,
	instanceGroupManager *unstructured.Unstructured,
) (ctrl.Result, error) {
	var requeueAfter time.Duration

	currentActions, ok, err := unstructured.NestedMap(instanceGroupManager.Object, "status", "currentActions")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get instance group manager current actions: %w", err)
	} else if !ok {
		// Status has not been populated yet
		return ctrl.Result{}, nil
	} else {
		totalInstances := int64(0)
		stableInstances := int64(0)
		for action, v := range currentActions {
			i, ok := v.(int64)
			if !ok {
				return ctrl.Result{}, fmt.Errorf("unexpected type for action %s: %T", action, v)
			}
			totalInstances += i
			if action == "none" {
				stableInstances = i
			}
		}

		deployment.Status.Replicas = int32(totalInstances)

		deployment.Status.CurrentReplicas = int32(totalInstances)

		deployment.Status.DesiredReplicas = deployment.Spec.ScaleSettings.MinReplicas

		// TODO(jreese) derive a Ready condition if we can based on instances with
		// a Ready condition. We'd need some way to drive that value from instance
		// observations, though.

		if stableInstances < 1 {
			logger.Info("no stable instances found")
			availableCondition.Reason = "NoStableInstanceFound"
			availableCondition.Message = "No stable instances found"

			// Manipulate a label on the ComputeInstanceGroupManager so that the
			// KCC controller reconciles the entity. We could alternatively set the
			// `cnrm.cloud.google.com/reconcile-interval-in-seconds` annotation, but
			// this approach allows for more fine grained control of forced
			// reconciliation, and avoids problems with multiple controllers wanting
			// to influence reconciliation of a KCC resource.
			//
			// An annotation was originally attempted, but it did not result in a
			// refresh of the instance group resource

			const timestampLabel = "compute.datumapis.com/deployment-reconciler-ts"

			groupManagerLabels := instanceGroupManager.GetLabels()
			if groupManagerLabels == nil {
				groupManagerLabels = map[string]string{}
			}

			groupManagerTimestampUpdateRequired := false
			if lastTime, ok := groupManagerLabels[timestampLabel]; ok {
				t, err := strconv.ParseInt(lastTime, 10, 64)
				if err != nil || time.Since(time.Unix(t, 0)) > 10*time.Second {
					// If we get an error, it's likely a result of an outside influence,
					// so override the value.
					groupManagerTimestampUpdateRequired = true
				}
			} else {
				groupManagerTimestampUpdateRequired = true
			}

			if groupManagerTimestampUpdateRequired {
				groupManagerLabels[timestampLabel] = strconv.FormatInt(metav1.Now().Unix(), 10)
				logger.Info("updating reconciler timestamp label on instance group manager")
				instanceGroupManager.SetLabels(groupManagerLabels)
				if err := r.InfraClient.Update(ctx, instanceGroupManager); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed updating instance group manager to update label: %w", err)
				}
			}
			requeueAfter = 10 * time.Second

		} else {
			availableCondition.Status = metav1.ConditionTrue
			availableCondition.Reason = "StableInstanceFound"
			availableCondition.Message = "At least one stable instances was found"
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *WorkloadDeploymentReconciler) checkInstanceGroupManagerReadiness(
	logger logr.Logger,
	availableCondition *metav1.Condition,
	instanceGroupManager *unstructured.Unstructured,
) (bool, error) {
	conditions, ok, err := unstructured.NestedSlice(instanceGroupManager.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Errorf("failed to get instance group manager status conditions: %w", err)
	} else if !ok {
		logger.Info("instance group manager not ready yet")
		availableCondition.Reason = "InstanceGroupManagerNotReady"
		return false, nil
	} else {
		for _, c := range conditions {
			cond := c.(map[string]interface{})
			if cond["type"].(string) == kcccomputev1alpha1.ReadyConditionType &&
				cond["status"].(string) != "True" {
				logger.Info("instance group manager not ready yet")

				availableCondition.Reason = "InstanceGroupManagerNotReady"
				return false, nil
			}
		}
	}
	return true, nil
}

func (r *WorkloadDeploymentReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {
	deployment := obj.(*computev1alpha.WorkloadDeployment)

	// Delete child entities in a sequence that does not result in exponential
	// backoffs of deletion attempts that occurs when they're all deleted by GC.
	instanceGroupManagerName := fmt.Sprintf("deployment-%s", deployment.UID)

	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)
	instanceGroupManagerObjectKey := client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      instanceGroupManagerName,
	}
	if err := r.InfraClient.Get(ctx, instanceGroupManagerObjectKey, &instanceGroupManager); client.IgnoreNotFound(err) != nil {
		return finalizer.Result{}, fmt.Errorf("failed fetching  instance group manager: %w", err)
	}

	if t := instanceGroupManager.GetCreationTimestamp(); !t.IsZero() {
		if dt := instanceGroupManager.GetDeletionTimestamp(); dt.IsZero() {
			if err := r.InfraClient.Delete(ctx, &instanceGroupManager); err != nil {
				return finalizer.Result{}, fmt.Errorf("failed deleting instance group manager: %w", err)
			}
		}
	}

	var instanceTemplates kcccomputev1beta1.ComputeInstanceTemplateList
	if err := r.InfraClient.List(
		ctx,
		&instanceTemplates,
		client.MatchingLabels{
			deploymentNameLabel: deployment.Name,
		},
	); err != nil {
		return finalizer.Result{}, fmt.Errorf("unable to list instance templates: %w", err)
	}

	for _, instanceTemplate := range instanceTemplates.Items {
		if err := r.InfraClient.Delete(ctx, &instanceTemplate); err != nil {
			return finalizer.Result{}, fmt.Errorf("failed to delete instance template: %w", err)
		}
	}

	// Allow GC to remove the following:
	//
	// - Deployment specific service account
	// - Deployment specific secret related entities
	// - Interface specific firewall rules

	return finalizer.Result{}, nil
}
