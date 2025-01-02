// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	kcccomputev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/compute/v1beta1"
	kcccomputev1alpha1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/k8s/v1alpha1"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.datum.net/infra-provider-gcp/internal/controller/k8sconfigconnector"
	"go.datum.net/infra-provider-gcp/internal/crossclusterutil"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

const gcpWorkloadFinalizer = "compute.datumapis.com/gcp-workload-controller"

// TODO(jreese) move to indexer package in workload-operator that can be used
const deploymentWorkloadUID = "spec.workloadRef.uid"

// WorkloadGatewayReconciler reconciles a Workload object and processes any
// gateways defined.
type WorkloadGatewayReconciler struct {
	client.Client
	InfraClient               client.Client
	Scheme                    *runtime.Scheme
	GCPProject                string
	InfraClusterNamespaceName string

	finalizers finalizer.Finalizers
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloads/finalizers,verbs=update

// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeaddresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeaddresses/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computefirewalls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computefirewalls/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computehealthchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computehealthchecks/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computebackendservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computebackendservices/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computetargettcpproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computetargettcpproxies/status,verbs=get
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeforwardingrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.cnrm.cloud.google.com,resources=computeforwardingrules/status,verbs=get

func (r *WorkloadGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var workload computev1alpha.Workload
	if err := r.Client.Get(ctx, req.NamespacedName, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	finalizationResult, err := r.finalizers.Finalize(ctx, &workload)
	if err != nil {
		if v, ok := err.(kerrors.Aggregate); ok && v.Is(resourceIsDeleting) {
			logger.Info("resources are still deleting, requeuing")
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to finalize: %w", err)
		}
	}
	if finalizationResult.Updated {
		if err := r.Client.Update(ctx, &workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update based on finalization result: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !workload.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	logger.Info("reconciling workload")
	defer logger.Info("reconcile complete")

	return ctrl.Result{}, r.reconcileWorkloadGateway(ctx, logger, &workload)
}

func (r *WorkloadGatewayReconciler) reconcileWorkloadGateway(
	ctx context.Context,
	logger logr.Logger,
	workload *computev1alpha.Workload,
) error {
	if gateway := workload.Spec.Gateway; gateway == nil {
		return nil
	}

	logger.Info("gateway definition found")

	// TODO(jreese) have different provisioners for each gateway class, move this
	// code out.

	// TODO(jreese) break the gateway out into a WorkloadGateway resource that can
	// be individually reconciled, so that we don't have multiple controllers
	// trying to update the workload spec. A separate reconciler should be
	// responsible for observing relevant dependent resources to determine the
	// workload status.

	// TODO(jreese) handle multiple listeners

	if len(workload.Spec.Gateway.Template.Spec.Listeners) == 0 {
		return fmt.Errorf("no listeners found on gateway")
	}

	listener := workload.Spec.Gateway.Template.Spec.Listeners[0]

	backendPorts := getGatewayBackendPorts(workload)

	// 1. Get an IP address for the load balancer
	// TODO(jreese) ipv6
	address, err := r.reconcileGatewayAddress(ctx, logger, workload)
	if err != nil {
		return err
	}

	if workload.Status.Gateway == nil {
		workload.Status.Gateway = &computev1alpha.WorkloadGatewayStatus{}
	}

	if !k8sconfigconnector.IsStatusConditionTrue(address.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
		logger.Info("address not ready yet")
		_ = apimeta.SetStatusCondition(&workload.Status.Gateway.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ListenerAddressNotReady",
			ObservedGeneration: workload.Generation,
			Message:            "Listener address is not Ready",
		})

		if err := r.Client.Status().Update(ctx, workload); err != nil {
			return fmt.Errorf("failed to update workload gateway status: %w", err)
		}

		return nil
	}

	if len(workload.Status.Gateway.Addresses) == 0 {
		addressType := gatewayv1.AddressType("IPAddress")
		workload.Status.Gateway.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  &addressType,
				Value: *address.Status.ObservedState.Address,
			},
		}
		if err := r.Client.Status().Update(ctx, workload); err != nil {
			return fmt.Errorf("failed to update workload gateway status: %w", err)
		}

		return nil
	}

	// 2. Create firewall rule to allow the load balancer to reach the backends.
	//
	// In the current configuration, load balancing will only direct traffic to
	// the primary interface. A more complex move to network endpoint groups
	// would be required to support load balancing into other interfaces. Given
	// this, we'll only enable the firewall rule on the network that the first
	// interface is attached to.

	if _, err := r.reconcileGatewayLBFirewall(ctx, logger, workload, backendPorts); err != nil {
		return err
	}

	// 3. Create external load balancers for the backend ports
	// TODO(jreese) make sure that multiple backend services can reuse the same
	// address on different ports.

	if err := r.reconcileGatewayBackendServices(ctx, logger, workload, backendPorts, address, int32(listener.Port)); err != nil {
		return err
	}

	return nil
}

func (r *WorkloadGatewayReconciler) reconcileGatewayAddress(
	ctx context.Context,
	logger logr.Logger,
	workload *computev1alpha.Workload,
) (kcccomputev1beta1.ComputeAddress, error) {

	addressName := fmt.Sprintf("workload-gw-%s", workload.UID)
	var address kcccomputev1beta1.ComputeAddress
	addressObjectKey := client.ObjectKey{
		Namespace: workload.Namespace,
		Name:      addressName,
	}
	if err := r.InfraClient.Get(ctx, addressObjectKey, &address); client.IgnoreNotFound(err) != nil {
		return address, fmt.Errorf("failed fetching IP address: %w", err)
	}

	if address.CreationTimestamp.IsZero() {
		logger.Info("creating global IP address")
		address := kcccomputev1beta1.ComputeAddress{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: workload.Namespace,
				Name:      addressName,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kcccomputev1beta1.ComputeAddressSpec{
				Location: "global",
				// TODO(jreese) support internal load balancers too - would need to
				// define multiple gateways on the workload.
				AddressType: proto.String("EXTERNAL"),
				IpVersion:   proto.String("IPV4"),
				// Required for global load balancers
				NetworkTier: proto.String("PREMIUM"),
			},
		}

		if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, &address, r.Scheme); err != nil {
			return address, fmt.Errorf("failed failed to set owner on IP address: %w", err)
		}

		if err := r.InfraClient.Create(ctx, &address); err != nil {
			return address, fmt.Errorf("failed to create IP address: %w", err)
		}
	}

	return address, nil
}

func (r *WorkloadGatewayReconciler) reconcileGatewayLBFirewall(
	ctx context.Context,
	logger logr.Logger,
	workload *computev1alpha.Workload,
	backendPorts sets.Set[computev1alpha.NamedPort],
) (kcccomputev1beta1.ComputeFirewall, error) {
	firewallName := fmt.Sprintf("workload-gw-hc-%s", workload.UID)

	var firewall kcccomputev1beta1.ComputeFirewall
	firewallObjectKey := client.ObjectKey{
		Namespace: workload.Namespace,
		Name:      firewallName,
	}

	if err := r.InfraClient.Get(ctx, firewallObjectKey, &firewall); client.IgnoreNotFound(err) != nil {
		return firewall, fmt.Errorf("failed fetching firewall rule for LB backends: %w", err)
	}

	if firewall.CreationTimestamp.IsZero() {
		logger.Info("creating firewall rule for LB access", "firewall_rule", firewallName)
		primaryNetworkInterface := workload.Spec.Template.Spec.NetworkInterfaces[0]

		var primaryNetwork networkingv1alpha.Network
		primaryNetworkObjectKey := client.ObjectKey{
			Namespace: workload.Namespace,
			Name:      primaryNetworkInterface.Network.Name,
		}
		if err := r.InfraClient.Get(ctx, primaryNetworkObjectKey, &primaryNetwork); err != nil {
			return firewall, fmt.Errorf("failed fetching network for primary network interface: %w", err)
		}

		firewall := kcccomputev1beta1.ComputeFirewall{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: workload.Namespace,
				Name:      firewallName,
				Annotations: map[string]string{
					GCPProjectAnnotation: r.GCPProject,
				},
			},
			Spec: kcccomputev1beta1.ComputeFirewallSpec{
				Description: proto.String(fmt.Sprintf("gateway policy for workload-%s", workload.UID)),
				Direction:   proto.String("INGRESS"),
				NetworkRef: kcccomputev1alpha1.ResourceRef{
					Namespace: workload.Namespace,
					Name:      fmt.Sprintf("network-%s", primaryNetwork.UID),
				},
				Priority: proto.Int64(1000), // Default priority, but might want to adjust
				SourceRanges: []string{
					// See https://cloud.google.com/load-balancing/docs/https#firewall-rules
					// TODO(jreese) add ipv6
					"130.211.0.0/22",
					"35.191.0.0/16",
				},
				TargetTags: []string{
					fmt.Sprintf("workload-%s", workload.UID),
				},
			},
		}

		if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, &firewall, r.Scheme); err != nil {
			return firewall, fmt.Errorf("failed failed to set owner on firewall: %w", err)
		}

		for _, namedPort := range backendPorts.UnsortedList() {
			ipProtocol := "tcp"
			if namedPort.Protocol != nil {
				ipProtocol = strings.ToLower(string(*namedPort.Protocol))
			}

			firewall.Spec.Allow = append(firewall.Spec.Allow, kcccomputev1beta1.FirewallAllow{
				Protocol: ipProtocol,
				Ports:    []string{strconv.Itoa(int(namedPort.Port))},
			})
		}

		if err := r.InfraClient.Create(ctx, &firewall); err != nil {
			return firewall, fmt.Errorf("failed to create gateway firewall rule: %w", err)
		}
	}

	return firewall, nil
}

func (r *WorkloadGatewayReconciler) reconcileGatewayBackendServices(
	ctx context.Context,
	logger logr.Logger,
	workload *computev1alpha.Workload,
	backendPorts sets.Set[computev1alpha.NamedPort],
	address kcccomputev1beta1.ComputeAddress,
	listenerPort int32,
) (err error) {
	readyCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "GatewayResourcesNotReady",
		ObservedGeneration: workload.Generation,
		Message:            "Gateway resources are not ready",
	}

	defer func() {
		if err != nil {
			// Don't update the status if errors are encountered
			return
		}
		statusChanged := apimeta.SetStatusCondition(&workload.Status.Gateway.Conditions, readyCondition)

		if statusChanged {
			err = r.Client.Status().Update(ctx, workload)
		}
	}()

	readyBackendServices := 0
	for _, namedPort := range backendPorts.UnsortedList() {
		healthCheckName := fmt.Sprintf("workload-gw-hc-%s-%d", workload.UID, namedPort.Port)

		var healthCheck kcccomputev1beta1.ComputeHealthCheck
		healthCheckObjectKey := client.ObjectKey{
			Namespace: workload.Namespace,
			Name:      healthCheckName,
		}
		if err := r.InfraClient.Get(ctx, healthCheckObjectKey, &healthCheck); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed fetching health check: %w", err)
		}

		if healthCheck.CreationTimestamp.IsZero() {
			logger.Info("creating health check", "health_check", healthCheckName)
			healthCheck = kcccomputev1beta1.ComputeHealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: workload.Namespace,
					Name:      healthCheckName,
					Annotations: map[string]string{
						GCPProjectAnnotation: r.GCPProject,
					},
				},
				Spec: kcccomputev1beta1.ComputeHealthCheckSpec{
					Location: "global",
					TcpHealthCheck: &kcccomputev1beta1.HealthcheckTcpHealthCheck{
						Port: proto.Int64(int64(namedPort.Port)),
					},
				},
			}

			if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, &healthCheck, r.Scheme); err != nil {
				return fmt.Errorf("failed failed to set owner on health check: %w", err)
			}

			if err := r.InfraClient.Create(ctx, &healthCheck); err != nil {
				return fmt.Errorf("failed to create health check: %w", err)
			}

			return nil
		}

		if !k8sconfigconnector.IsStatusConditionTrue(healthCheck.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
			readyCondition.Reason = "HealthCheckNotReady"
			return nil
		}

		// 4. Reconcile `backend-service` load balancer for each named port.
		backendServiceName := fmt.Sprintf("workload-%s-%s", workload.UID, namedPort.Name)

		backendService := &kcccomputev1beta1.ComputeBackendService{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: workload.Namespace,
				Name:      backendServiceName,
			},
		}

		backendServiceResult, err := r.reconcileBackendService(ctx, logger, workload, namedPort, &healthCheck, backendService)
		if err != nil {
			return fmt.Errorf("failed to create or update backend service: %w", err)
		}

		if backendServiceResult != controllerutil.OperationResultNone {
			logger.Info("backend service mutated", "result", backendServiceResult)
		}

		if !k8sconfigconnector.IsStatusConditionTrue(backendService.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
			logger.Info("backend service not ready yet")
			readyCondition.Reason = "BackendServiceNotReady"
			return nil
		}

		// 5. Create a "target tcp proxy" for the backend service
		// TODO(jreese) probably need to use a hash
		targetTCPProxyName := fmt.Sprintf("workload-gw-%s-%d", workload.UID, namedPort.Port)

		var targetTCPProxy kcccomputev1beta1.ComputeTargetTCPProxy
		targetTCPProxyObjectKey := client.ObjectKey{
			Namespace: workload.Namespace,
			Name:      targetTCPProxyName,
		}

		if err := r.InfraClient.Get(ctx, targetTCPProxyObjectKey, &targetTCPProxy); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed fetching target TCP proxy: %w", err)
		}

		if targetTCPProxy.CreationTimestamp.IsZero() {
			logger.Info("creating target TCP proxy")
			targetTCPProxy = kcccomputev1beta1.ComputeTargetTCPProxy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: workload.Namespace,
					Name:      targetTCPProxyName,
					Annotations: map[string]string{
						GCPProjectAnnotation: r.GCPProject,
					},
				},
				Spec: kcccomputev1beta1.ComputeTargetTCPProxySpec{
					BackendServiceRef: kcccomputev1alpha1.ResourceRef{
						Namespace: backendService.Namespace,
						Name:      backendService.Name,
					},
					ProxyHeader: proto.String("NONE"),
				},
			}

			if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, &targetTCPProxy, r.Scheme); err != nil {
				return fmt.Errorf("failed failed to set owner on target TCP proxy: %w", err)
			}

			if err := r.InfraClient.Create(ctx, &targetTCPProxy); err != nil {
				return fmt.Errorf("failed to create target TCP proxy: %w", err)
			}

			return nil
		}

		if !k8sconfigconnector.IsStatusConditionTrue(targetTCPProxy.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
			logger.Info("target TCP proxy not ready yet")
			readyCondition.Reason = "TargetTCPProxyNotReady"
			return nil
		}

		// 6. Create a forwarding rule for the address and port toward the TCP LB
		forwardingRuleName := fmt.Sprintf("workload-gw-%s-%d", workload.UID, namedPort.Port)

		var forwardingRule kcccomputev1beta1.ComputeForwardingRule
		forwardingRuleObjectKey := client.ObjectKey{
			Namespace: workload.Namespace,
			Name:      forwardingRuleName,
		}

		if err := r.InfraClient.Get(ctx, forwardingRuleObjectKey, &forwardingRule); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed fetching forwarding rule for TCP proxy: %w", err)
		}

		if forwardingRule.CreationTimestamp.IsZero() {
			logger.Info("creating forwarding rule", "forwarding_rule", forwardingRuleName)

			forwardingRule := kcccomputev1beta1.ComputeForwardingRule{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: workload.Namespace,
					Name:      forwardingRuleName,
					Annotations: map[string]string{
						GCPProjectAnnotation: r.GCPProject,
					},
				},
				Spec: kcccomputev1beta1.ComputeForwardingRuleSpec{
					Location:            "global",
					LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
					Target: &kcccomputev1beta1.ForwardingruleTarget{
						TargetTCPProxyRef: &kcccomputev1alpha1.ResourceRef{
							Namespace: targetTCPProxy.Namespace,
							Name:      targetTCPProxy.Name,
						},
					},
					NetworkTier: proto.String("PREMIUM"),
					IpProtocol:  proto.String("TCP"),
					IpAddress: &kcccomputev1beta1.ForwardingruleIpAddress{
						AddressRef: &kcccomputev1alpha1.ResourceRef{
							Namespace: address.Namespace,
							Name:      address.Name,
						},
					},
					PortRange: proto.String(fmt.Sprintf("%d-%d", listenerPort, listenerPort)),
				},
			}

			if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, &forwardingRule, r.Scheme); err != nil {
				return fmt.Errorf("failed failed to set owner on forwarding rule: %w", err)
			}

			if err := r.InfraClient.Create(ctx, &forwardingRule); err != nil {
				return fmt.Errorf("failed to create forwarding rule for TCP proxy: %w", err)
			}
		}

		if !k8sconfigconnector.IsStatusConditionTrue(forwardingRule.Status.Conditions, kcccomputev1alpha1.ReadyConditionType) {
			logger.Info("forwarding rule not ready yet")
			readyCondition.Reason = "ForwardingRuleNotReady"
			return nil
		}

		logger.Info("forwarding rule is ready")
		readyBackendServices++
	}

	if readyBackendServices == len(backendPorts) {
		readyCondition.Reason = "GatewayResourcesReady"
		readyCondition.Message = "All gateway resources ready"
		readyCondition.Status = metav1.ConditionTrue
	}

	return nil
}

func (r *WorkloadGatewayReconciler) reconcileBackendService(
	ctx context.Context,
	logger logr.Logger,
	workload *computev1alpha.Workload,
	namedPort computev1alpha.NamedPort,
	healthCheck *kcccomputev1beta1.ComputeHealthCheck,
	backendService *kcccomputev1beta1.ComputeBackendService,
) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrUpdate(ctx, r.InfraClient, backendService, func() error {
		if backendService.CreationTimestamp.IsZero() {
			logger.Info("creating backend service")
		} else {
			logger.Info("updating backend service")
		}

		// Add a backend to the backend service for each workload deployment found
		listOpts := client.MatchingFields{
			deploymentWorkloadUID: string(workload.UID),
		}
		var deployments computev1alpha.WorkloadDeploymentList
		if err := r.Client.List(ctx, &deployments, listOpts); err != nil {
			return fmt.Errorf("failed to list worklaod deployments: %w", err)
		}

		if len(deployments.Items) == 0 {
			logger.Info("no workload deployments found")
			return nil
		}

		var backends []kcccomputev1beta1.BackendserviceBackend

		for _, deployment := range deployments.Items {

			// KCC can't point to a ComputeInstanceGroupManager, even with the Kind
			// field being set in the InstanceGroupRef, so we need to look them up.

			var instanceGroupManager unstructured.Unstructured
			instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)
			instanceGroupManagerObjectKey := client.ObjectKey{
				Namespace: workload.Namespace,
				Name:      fmt.Sprintf("deployment-%s", deployment.UID),
			}
			if err := r.InfraClient.Get(ctx, instanceGroupManagerObjectKey, &instanceGroupManager); err != nil {
				return fmt.Errorf("failed fetching instance group manager for deployment: %w", err)
			}

			instanceGroup, ok, err := unstructured.NestedString(instanceGroupManager.Object, "status", "instanceGroup")
			if err != nil {
				return fmt.Errorf("failed reading instance group from instance group manager: %w", err)
			} else if !ok {
				return fmt.Errorf("did not find instance group in instance group manager status")
			}

			backend := kcccomputev1beta1.BackendserviceBackend{
				BalancingMode:  proto.String("UTILIZATION"),
				MaxUtilization: proto.Float64(.8),

				Group: kcccomputev1beta1.BackendserviceGroup{
					InstanceGroupRef: &kcccomputev1alpha1.ResourceRef{
						External: instanceGroup,
					},
				},
			}

			backends = append(backends, backend)
		}

		if backendService.Annotations == nil {
			backendService.Annotations = map[string]string{
				GCPProjectAnnotation: r.GCPProject,
			}
		}

		backendService.Spec.Location = "global"
		backendService.Spec.LoadBalancingScheme = proto.String("EXTERNAL_MANAGED")
		backendService.Spec.Protocol = proto.String("TCP")
		// TODO(jreese) ipv6 support
		// TODO(jreese) the following field doesn't exist in the struct, do we need
		// it?
		// IpAddressSelectionPolicy: "IPV4_ONLY",
		// TODO(jreese) allow tweaking this. Possibly from readiness probe definitions?
		backendService.Spec.TimeoutSec = proto.Int64(300)
		backendService.Spec.PortName = proto.String(namedPort.Name)
		backendService.Spec.HealthChecks = []kcccomputev1beta1.BackendserviceHealthChecks{
			{
				HealthCheckRef: &kcccomputev1alpha1.ResourceRef{
					Namespace: workload.Namespace,
					Name:      healthCheck.Name,
				},
			},
		}

		backendService.Spec.Backend = backends

		if err := crossclusterutil.SetControllerReference(ctx, r.InfraClient, workload, backendService, r.Scheme); err != nil {
			return fmt.Errorf("failed failed to set owner on backend service: %w", err)
		}

		return nil
	})
}

var resourceIsDeleting = errors.New("resource is deleting")

func (r *WorkloadGatewayReconciler) Finalize(
	ctx context.Context,
	obj client.Object,
) (finalizer.Result, error) {
	// workload := obj.(*computev1alpha.Workload)

	// TODO(jreese) Delete child entities in a sequence that does not result in
	// exponential backoffs of deletion attempts that occurs when they're all
	// deleted by GC.
	//
	// Make sure to update the status conditions

	if err := crossclusterutil.DeleteAnchorForObject(ctx, r.Client, r.InfraClient, obj, r.InfraClusterNamespaceName); err != nil {
		return finalizer.Result{}, fmt.Errorf("failed deleting instance group manager anchor: %w", err)
	}

	return finalizer.Result{}, nil
}

func getGatewayBackendPorts(workload *computev1alpha.Workload) sets.Set[computev1alpha.NamedPort] {
	runtime := workload.Spec.Template.Spec.Runtime
	namedPorts := map[string]computev1alpha.NamedPort{}
	if runtime.Sandbox != nil {
		for _, c := range runtime.Sandbox.Containers {
			for _, namedPort := range c.Ports {
				namedPorts[namedPort.Name] = namedPort
			}

		}
	}

	if runtime.VirtualMachine != nil {
		for _, namedPort := range runtime.VirtualMachine.Ports {
			namedPorts[namedPort.Name] = namedPort
		}
	}

	backendPorts := sets.Set[computev1alpha.NamedPort]{}

	for _, tcpRoute := range workload.Spec.Gateway.TCPRoutes {
		for _, rule := range tcpRoute.Rules {
			for _, backendRef := range rule.BackendRefs {
				// Consider looking to see if backendRef.Port is set, if we end up
				// not forcing users to leverage a named port.
				if namedPort, ok := namedPorts[string(backendRef.Name)]; !ok {
					panic("did not find named port for backend ref")
				} else {
					backendPorts.Insert(namedPort)
				}
			}
		}
	}
	return backendPorts
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadGatewayReconciler) SetupWithManager(mgr ctrl.Manager, infraCluster cluster.Cluster) error {

	r.finalizers = finalizer.NewFinalizers()
	if err := r.finalizers.Register(gcpWorkloadFinalizer, r); err != nil {
		return fmt.Errorf("failed to register finalizer: %w", err)
	}

	// TODO(jreese) move to indexer package

	err := mgr.GetFieldIndexer().IndexField(context.Background(), &computev1alpha.WorkloadDeployment{}, deploymentWorkloadUID, func(o client.Object) []string {
		return []string{
			string(o.(*computev1alpha.WorkloadDeployment).Spec.WorkloadRef.UID),
		}
	})
	if err != nil {
		return fmt.Errorf("failed to add workload deployment field indexer: %w", err)
	}

	// Watch the unstructured form of an instance group manager, as the generated
	// types are not aligned with the actual CRD.
	var instanceGroupManager unstructured.Unstructured
	instanceGroupManager.SetGroupVersionKind(kcccomputev1beta1.ComputeInstanceGroupManagerGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha.Workload{}).
		Owns(&computev1alpha.WorkloadDeployment{}).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeAddress{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeAddress](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeFirewall{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeFirewall](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeHealthCheck{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeHealthCheck](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeBackendService{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeBackendService](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeTargetTCPProxy{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeTargetTCPProxy](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&kcccomputev1beta1.ComputeForwardingRule{},
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*kcccomputev1beta1.ComputeForwardingRule](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		WatchesRawSource(source.TypedKind(
			infraCluster.GetCache(),
			&instanceGroupManager,
			crossclusterutil.TypedEnqueueRequestForUpstreamOwner[*unstructured.Unstructured](mgr.GetScheme(), &computev1alpha.Workload{}),
		)).
		Complete(r)
}
