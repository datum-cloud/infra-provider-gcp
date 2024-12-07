package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gcpcomputev1 "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	kcccomputev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/compute/v1beta1"
	kcccomputev1alpha1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/k8s/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"go.datum.net/infra-provider-gcp/internal/controller/k8sconfigconnector"
	"go.datum.net/infra-provider-gcp/internal/crossclusterutil"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// InstanceDiscoveryReconciler reconciles a Workload object and processes any
// gateways defined.
type InstanceDiscoveryReconciler struct {
	client.Client
	InfraClient client.Client
	Scheme      *runtime.Scheme

	finalizers              finalizer.Finalizers
	instancesClient         *gcpcomputev1.InstancesClient
	instanceTemplatesClient *gcpcomputev1.InstanceTemplatesClient
	migClient               *gcpcomputev1.InstanceGroupManagersClient
}

// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancegroupmanager,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeinstancegroupmanager/status,verbs=get

func (r *InstanceDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Work with the unstructured form of an instance group manager, as the generated
	// types are not aligned with the actual CRD. Particularly the `targetSize`
	// field.

	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)

	if err := r.InfraClient.Get(ctx, req.NamespacedName, &instanceGroupManager); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &instanceGroupManager)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(resourceIsDeleting) {
			logger.Info("resources are still deleting, requeuing")
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err = r.InfraClient.Update(ctx, &instanceGroupManager); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	var reconcileResult ctrl.Result

	var isDeleting bool
	if t := instanceGroupManager.GetDeletionTimestamp(); !t.IsZero() {
		isDeleting = true
		reconcileResult.RequeueAfter = 10 * time.Second
	}

	// Very ugly workaround for not being able to use the typed instance group
	// manager.
	conditions, err := extractUnstructuredConditions(instanceGroupManager.Object)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed extracting instance group manager conditions: %w", err)
	}

	if !isDeleting && !k8sconfigconnector.IsStatusConditionTrue(conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("instance group manager not ready yet")
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling instance group manager")
	defer logger.Info("reconcile complete")

	var workloadDeployment computev1alpha.WorkloadDeployment
	if !isDeleting {
		w, err := r.getWorkloadDeploymentForInstanceGroupManager(ctx, instanceGroupManager)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching workload deployment: %w", err)
		}
		workloadDeployment = *w
	}

	gcpProject, ok, err := unstructured.NestedString(instanceGroupManager.Object, "spec", "projectRef", "external")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reading project from instance group manager: %w", err)
	} else if !ok {
		return ctrl.Result{}, fmt.Errorf("empty project found on instance group manager")
	}

	gcpZone, ok, err := unstructured.NestedString(instanceGroupManager.Object, "spec", "location")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reading zone from instance group manager: %w", err)
	} else if !ok {
		return ctrl.Result{}, fmt.Errorf("empty location found on instance group manager")
	}

	// TODO(jreese) shortcut reconciliation based on stability and last observed
	// info, status.isStable, etc.
	//
	// TODO(jreese) see if we can use the resource export functionality to obtain
	// a yaml manifest that can be applied to create a ComputeInstance that will
	// acquire the managed instance. This way, this reconciler can be responsible
	// only for ensuring the ComputeInstances exist, and another reconciler can
	// watch those in order to reconcile the Datum Instance representation. If
	// we do that, we'll want to make sure to set the `abandon` annotation value.

	listRequest := &computepb.ListManagedInstancesInstanceGroupManagersRequest{
		Project:              gcpProject,
		Zone:                 gcpZone,
		InstanceGroupManager: req.Name,
	}

	// TODO(jreese) delete instances that no longer show up in the managed list
	for managedInstance, err := range r.migClient.ListManagedInstances(ctx, listRequest).All() {
		if err != nil {
			if e, ok := err.(*apierror.APIError); ok && e.HTTPCode() == http.StatusNotFound {
				break
			}
			return ctrl.Result{}, fmt.Errorf("failed listing managed instances: %w", err)
		}

		result, err := r.reconcileDatumInstance(
			ctx,
			logger,
			gcpProject,
			gcpZone,
			isDeleting,
			&workloadDeployment,
			managedInstance,
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed reconciling datum instance: %w", err)
		}

		if result.RequeueAfter > 0 {
			return result, nil
		}
	}

	// TODO(jreese) enable periodic reconcile
	// TODO(jreese) 30 seconds is aggressive to do all the time, consider having
	// this configurable similar to what KCC supports, and probably context based
	// requeue after based on condition status transition times.
	// return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	return reconcileResult, nil
}

func (r *InstanceDiscoveryReconciler) getWorkloadDeploymentForInstanceGroupManager(
	ctx context.Context,
	instanceGroupManager unstructured.Unstructured,
) (*computev1alpha.WorkloadDeployment, error) {
	labels := instanceGroupManager.GetLabels()
	var workloadDeployment computev1alpha.WorkloadDeployment
	if labels[crossclusterutil.UpstreamOwnerKindLabel] != "WorkloadDeployment" {
		return nil, fmt.Errorf("failed to find WorkloadDeployment owner for ComputeInstanceGroupManager")
	}

	workloadDeploymentObjectKey := client.ObjectKey{
		Namespace: labels[crossclusterutil.UpstreamOwnerNamespaceLabel],
		Name:      labels[crossclusterutil.UpstreamOwnerNameLabel],
	}
	if err := r.Client.Get(ctx, workloadDeploymentObjectKey, &workloadDeployment); err != nil {
		return nil, fmt.Errorf("failed to get workload deployment: %w", err)
	}

	return &workloadDeployment, nil
}

func (r *InstanceDiscoveryReconciler) reconcileDatumInstance(
	ctx context.Context,
	logger logr.Logger,
	gcpProject string,
	gcpZone string,
	isDeleting bool,
	workloadDeployment *computev1alpha.WorkloadDeployment,
	managedInstance *computepb.ManagedInstance,
) (ctrl.Result, error) {

	getInstanceReq := &computepb.GetInstanceRequest{
		Project:  gcpProject,
		Zone:     gcpZone,
		Instance: *managedInstance.Name,
	}

	instance, err := r.instancesClient.Get(ctx, getInstanceReq)

	if err != nil {
		if e, ok := err.(*apierror.APIError); ok && e.HTTPCode() == http.StatusNotFound {
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed fetching gcp instance for managed instance: %w", err)
	}

	datumInstance := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workloadDeployment.Namespace,
			Name:      *managedInstance.Name,
		},
	}

	if !isDeleting {
		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, datumInstance, func() error {
			if datumInstance.CreationTimestamp.IsZero() {
				logger.Info("creating datum instance")
			} else {
				logger.Info("updating datum instance")
			}

			if err := controllerutil.SetControllerReference(workloadDeployment, datumInstance, r.Scheme); err != nil {
				return fmt.Errorf("failed to set controller on instance: %w", err)
			}

			// TODO(jreese) track a workload deployment revision that aligns with the
			// instance template 1:1, and have a controller on that be responsible for
			// creating the instance template. The deployment controller would then
			// point the MIG to the latest version.
			//
			// TODO(jreese) this will be required for updates to instances by a new
			// template to be communicated correctly.

			datumInstance.Spec = workloadDeployment.Spec.Template.Spec
			return nil
		})

		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed reconciling datum instance: %w", err)
		}

		if result != controllerutil.OperationResultNone {
			logger.Info("datum instance mutated", "result", result)
		}
	} else {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(datumInstance), datumInstance); err != nil {
			if apierrors.IsNotFound(err) {
				// This would occur during deletion at the moment.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed fetching datum instance: %w", err)
		}
	}

	var instanceStatus string
	if instance.Status != nil {
		instanceStatus = *instance.Status
	}

	var statusUpdated bool

	datumNetworkInterfaces := make([]computev1alpha.InstanceNetworkInterfaceStatus, 0, len(instance.NetworkInterfaces))

	for _, networkInterface := range instance.NetworkInterfaces {
		datumNetworkInterfaceStatus := computev1alpha.InstanceNetworkInterfaceStatus{}

		if networkInterface.NetworkIP != nil {
			datumNetworkInterfaceStatus.Assignments.NetworkIP = proto.String(*networkInterface.NetworkIP)
		}

		for _, accessConfig := range networkInterface.AccessConfigs {
			if *accessConfig.Type == "ONE_TO_ONE_NAT" && accessConfig.NatIP != nil {
				datumNetworkInterfaceStatus.Assignments.ExternalIP = proto.String(*accessConfig.NatIP)
			}
		}

		datumNetworkInterfaces = append(datumNetworkInterfaces, datumNetworkInterfaceStatus)
	}

	if !equality.Semantic.DeepEqual(datumInstance.Status.NetworkInterfaces, datumNetworkInterfaces) {
		statusUpdated = true
		datumInstance.Status.NetworkInterfaces = datumNetworkInterfaces
	}

	var reconcileResult ctrl.Result
	switch instanceStatus {
	case "RUNNING":
		changed := apimeta.SetStatusCondition(&datumInstance.Status.Conditions, metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionTrue,
			Reason:             "InstanceIsRunning",
			ObservedGeneration: datumInstance.Generation,
			Message:            "GCP Instance status is RUNNING",
		})
		if changed {
			statusUpdated = true
		}
	default:
		reconcileResult.RequeueAfter = 10 * time.Second

		changed := apimeta.SetStatusCondition(&datumInstance.Status.Conditions, metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			Reason:             fmt.Sprintf("InstanceIs%s%s", string(instanceStatus[0]), strings.ToLower(instanceStatus[1:])),
			ObservedGeneration: datumInstance.Generation,
			Message:            fmt.Sprintf("GCP Instance status is %s", instanceStatus),
		})
		if changed {
			statusUpdated = true
		}
	}

	if statusUpdated {
		if err := r.Client.Status().Update(ctx, datumInstance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update datum instance status: %w", err)
		}
	}
	return reconcileResult, nil
}

func (r *InstanceDiscoveryReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {

	return finalizer.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager, infraCluster cluster.Cluster) error {

	instancesClient, err := gcpcomputev1.NewInstancesRESTClient(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create instance group managers client: %w", err)
	}
	r.instancesClient = instancesClient

	instanceTemplatesClient, err := gcpcomputev1.NewInstanceTemplatesRESTClient(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create instance group managers client: %w", err)
	}
	r.instanceTemplatesClient = instanceTemplatesClient

	instanceGroupManagersClient, err := gcpcomputev1.NewInstanceGroupManagersRESTClient(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create instance group managers client: %w", err)
	}
	r.migClient = instanceGroupManagersClient

	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpWorkloadFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	// Watch the unstructured form of an instance group manager, as the generated
	// types are not aligned with the actual CRD.
	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)

	return ctrl.NewControllerManagedBy(mgr).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&instanceGroupManager,
			&handler.TypedEnqueueRequestForObject[*unstructured.Unstructured]{},
		)).
		Named("instancediscovery").
		Complete(r)
}

func extractUnstructuredConditions(
	obj map[string]interface{},
) ([]kcccomputev1alpha1.Condition, error) {
	conditions, ok, _ := unstructured.NestedSlice(obj, "status", "conditions")
	if !ok {
		return nil, nil
	}

	wrappedConditions := map[string]interface{}{
		"conditions": conditions,
	}

	var typedConditions struct {
		Conditions []kcccomputev1alpha1.Condition `json:"conditions"`
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(wrappedConditions, &typedConditions); err != nil {
		return nil, fmt.Errorf("failed converting unstructured conditions: %w", err)
	}

	return typedConditions.Conditions, nil
}
