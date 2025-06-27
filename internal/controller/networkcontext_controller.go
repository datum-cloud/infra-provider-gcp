// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"

	crossplanecommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	awsec2v1beta1 "github.com/upbound/provider-aws/apis/ec2/v1beta1"
	gcpcomputev1beta1 "github.com/upbound/provider-gcp/apis/compute/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/config"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	"go.datum.net/infra-provider-gcp/internal/locationutil"
	datumsource "go.datum.net/infra-provider-gcp/internal/source"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkContextReconciler reconciles a NetworkContext and ensures that a GCP
// ComputeNetwork is created to represent the context within GCP.
type NetworkContextReconciler struct {
	mgr               mcmanager.Manager
	Config            config.GCPProvider
	DownstreamCluster cluster.Cluster
	LocationClassName string
}

func (r *NetworkContextReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var networkContext networkingv1alpha.NetworkContext
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &networkContext); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	location, shouldProcess, err := locationutil.GetLocation(ctx, cl.GetClient(), networkContext.Spec.Location, r.LocationClassName)
	if err != nil {
		return ctrl.Result{}, err
	} else if !shouldProcess {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling network context")
	defer logger.Info("reconcile complete")

	if !networkContext.DeletionTimestamp.IsZero() {
		// TODO(jreese): Finalizer logic for AWS
		return ctrl.Result{}, nil
	}

	programmedCondition := metav1.Condition{
		Type:               networkingv1alpha.NetworkContextProgrammed,
		Status:             metav1.ConditionFalse,
		Reason:             networkingv1alpha.NetworkContextProgrammedReasonNotProgrammed,
		ObservedGeneration: networkContext.Generation,
		Message:            "Network has not been programmed",
	}

	defer func() {
		if apimeta.SetStatusCondition(&networkContext.Status.Conditions, programmedCondition) {
			err = errors.Join(err, cl.GetClient().Status().Update(ctx, &networkContext))
		}
	}()

	var network networkingv1alpha.Network
	networkObjectKey := client.ObjectKey{
		Namespace: networkContext.Namespace,
		Name:      networkContext.Spec.Network.Name,
	}
	if err := cl.GetClient().Get(ctx, networkObjectKey, &network); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed fetching network: %w", err)
	}

	downstreamStrategy := downstreamclient.NewMappedNamespaceResourceStrategy(
		req.ClusterName,
		cl.GetClient(),
		r.DownstreamCluster.GetClient(),
		r.Config.DownstreamResourceManagement.ManagedResourceLabels,
	)
	downstreamClient := downstreamStrategy.GetClient()

	var downstreamResourceStatus crossplanecommonv1.ResourceStatus
	if location.Spec.Provider.GCP != nil {
		downstreamNetwork := &gcpcomputev1beta1.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("network-%s", network.UID),
			},
		}

		result, err := controllerutil.CreateOrPatch(ctx, downstreamClient, downstreamNetwork, func() error {
			metav1.SetMetaDataAnnotation(&downstreamNetwork.ObjectMeta, downstreamclient.UpstreamOwnerClusterName, req.ClusterName)

			downstreamNetwork.Spec = gcpcomputev1beta1.NetworkSpec{
				ResourceSpec: crossplanecommonv1.ResourceSpec{
					ProviderConfigReference: &crossplanecommonv1.Reference{
						Name: r.Config.GetProviderConfigName("GCP", req.ClusterName),
					},
				},
				ForProvider: gcpcomputev1beta1.NetworkParameters{
					Mtu:                                   ptr.To(float64(network.Spec.MTU)),
					AutoCreateSubnetworks:                 ptr.To(false),
					NetworkFirewallPolicyEnforcementOrder: ptr.To("AFTER_CLASSIC_FIREWALL"),
					RoutingMode:                           ptr.To("REGIONAL"),
					Project:                               ptr.To(location.Spec.Provider.GCP.ProjectID),
				},
			}

			return nil
		})

		if err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}

		downstreamResourceStatus = downstreamNetwork.Status.ResourceStatus
		logger.Info("downstream gcp network processed", "operation_result", result, "name", downstreamNetwork.Name)
	} else if location.Spec.Provider.AWS != nil {
		// In AWS, we need to create a VPC with a CIDR. Subnets in the VPC must
		// be allocated from a CIDR that's associated with the VPC. Additionally,
		// Subnets in AWS are zonal, not regional. A quirk is that we issue subnets
		// to Locations, which are zonal.
		//
		// To do this, we'll use an existing SubnetClaim, or create one.

		var subnetClaims networkingv1alpha.SubnetClaimList
		listOpts := []client.ListOption{
			client.InNamespace(networkContext.Namespace),
		}

		if err := cl.GetClient().List(ctx, &subnetClaims, listOpts...); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed listing subnet claims: %w", err)
		}

		var subnetClaim networkingv1alpha.SubnetClaim
		for _, claim := range subnetClaims.Items {
			// If it's not the same subnet class, don't consider the subnet claim.
			if claim.Spec.SubnetClass != "private" {
				continue
			}

			// If it's not ipv4, don't consider the subnet claim.
			if claim.Spec.IPFamily != networkingv1alpha.IPv4Protocol {
				continue
			}

			// TODO(jreese): Aggregate CIDRs for network contexts in the network
			// that are in the same AWS region.

			// If it's not the same network context, don't consider the subnet claim.
			if claim.Spec.NetworkContext.Name != networkContext.Name {
				continue
			}

			subnetClaim = claim
			break
		}

		if subnetClaim.CreationTimestamp.IsZero() {
			subnetClaim = networkingv1alpha.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: networkContext.Namespace,
					// In the future, subnets will be created with an ordinal that increases.
					// This ensures that we don't create duplicate subnet claims when the
					// cache is not up to date.
					Name: fmt.Sprintf("%s-0", networkContext.Name),
				},
				Spec: networkingv1alpha.SubnetClaimSpec{
					SubnetClass: "private",
					IPFamily:    networkingv1alpha.IPv4Protocol,
					NetworkContext: networkingv1alpha.LocalNetworkContextRef{
						Name: networkContext.Name,
					},
					Location: networkingv1alpha.LocationReference{
						Namespace: networkContext.Namespace,
						Name:      networkContext.Spec.Location.Name,
					},
				},
			}

			if err := controllerutil.SetOwnerReference(&networkContext, &subnetClaim, cl.GetScheme()); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to set controller on subnet claim: %w", err)
			}

			if err := cl.GetClient().Create(ctx, &subnetClaim); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating subnet claim: %w", err)
			}
		}

		logger.Info("found subnet claim", "subnetClaim", subnetClaim.Name)

		if !apimeta.IsStatusConditionTrue(subnetClaim.Status.Conditions, networkingv1alpha.SubnetClaimAllocated) {
			logger.Info("waiting for subnet claim to be allocated", "subnetClaim", subnetClaim.Name)
			return ctrl.Result{}, nil
		} else if subnetClaim.Status.StartAddress == nil || subnetClaim.Status.PrefixLength == nil {
			logger.Info(
				"subnet claim is allocated but is invalid",
				"has_start_address", subnetClaim.Status.StartAddress != nil,
				"has_prefix_length", subnetClaim.Status.PrefixLength != nil,
			)
			return ctrl.Result{}, nil
		}

		var downstreamVPC awsec2v1beta1.VPC
		downstreamVPCObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("networkcontext-%s", networkContext.UID),
		}
		if err := downstreamClient.Get(ctx, downstreamVPCObjectKey, &downstreamVPC); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching downstream network: %w", err)
		}

		if downstreamVPC.CreationTimestamp.IsZero() {
			downstreamVPC = awsec2v1beta1.VPC{
				ObjectMeta: metav1.ObjectMeta{
					Name: downstreamVPCObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        networkContext.Name,
						downstreamclient.UpstreamOwnerNamespace:   networkContext.Namespace,
						downstreamclient.UpstreamOwnerClusterName: req.ClusterName,
					},
				},
				Spec: awsec2v1beta1.VPCSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", req.ClusterName),
						},
					},
					ForProvider: awsec2v1beta1.VPCParameters_2{
						Region:    ptr.To(location.Spec.Provider.AWS.Region),
						CidrBlock: ptr.To(fmt.Sprintf("%s/%d", *subnetClaim.Status.StartAddress, *subnetClaim.Status.PrefixLength)),
					},
				},
			}

			logger.Info("creating downstream network", "downstream_vpc", downstreamVPC.Name)

			if err := downstreamClient.Create(ctx, &downstreamVPC); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating downstream network: %w", err)
			}
		} else {
			// TODO(jreese): Make sure the VPC has the correct CIDR blocks.
			// There are limitations to how many can be added, but once we refactor
			// IPAM this _shouldn't_ be a concern. Default is 5 IPv4 blocks, up to 50.
			//
			// https://docs.aws.amazon.com/vpc/latest/userguide/amazon-vpc-limits.html
		}

		if downstreamVPC.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
			logger.Info("VPC is not ready yet")
			return ctrl.Result{}, nil
		}

		var internetGateway awsec2v1beta1.InternetGateway
		internetGatewayObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("networkcontext-%s", networkContext.UID),
		}
		if err := downstreamClient.Get(ctx, internetGatewayObjectKey, &internetGateway); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching internet gateway: %w", err)
		}

		if internetGateway.CreationTimestamp.IsZero() {
			internetGateway = awsec2v1beta1.InternetGateway{
				ObjectMeta: metav1.ObjectMeta{
					Name: internetGatewayObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        networkContext.Name,
						downstreamclient.UpstreamOwnerNamespace:   networkContext.Namespace,
						downstreamclient.UpstreamOwnerClusterName: req.ClusterName,
					},
				},
				Spec: awsec2v1beta1.InternetGatewaySpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", req.ClusterName),
						},
					},
					ForProvider: awsec2v1beta1.InternetGatewayParameters_2{
						Region: ptr.To(location.Spec.Provider.AWS.Region),
						VPCIDRef: &crossplanecommonv1.Reference{
							Name: downstreamVPCObjectKey.Name,
						},
					},
				},
			}

			logger.Info("creating downstream internet gateway", "downstream_internet_gateway", internetGateway.Name)

			if err := downstreamClient.Create(ctx, &internetGateway); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating downstream internet gateway: %w", err)
			}
		}

		if internetGateway.Status.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
			logger.Info("internet gateway is not ready yet")
			return ctrl.Result{}, nil
		}

		var internetEgressRoute awsec2v1beta1.Route
		internetEgressRouteObjectKey := client.ObjectKey{
			Name: fmt.Sprintf("networkcontext-%s-internet-egress", networkContext.UID),
		}
		if err := downstreamClient.Get(ctx, internetEgressRouteObjectKey, &internetEgressRoute); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed fetching internet egress route: %w", err)
		}

		if internetEgressRoute.CreationTimestamp.IsZero() {
			internetEgressRoute = awsec2v1beta1.Route{
				ObjectMeta: metav1.ObjectMeta{
					Name: internetEgressRouteObjectKey.Name,
					Annotations: map[string]string{
						downstreamclient.UpstreamOwnerName:        networkContext.Name,
						downstreamclient.UpstreamOwnerNamespace:   networkContext.Namespace,
						downstreamclient.UpstreamOwnerClusterName: req.ClusterName,
					},
				},
				Spec: awsec2v1beta1.RouteSpec{
					ResourceSpec: crossplanecommonv1.ResourceSpec{
						ProviderConfigReference: &crossplanecommonv1.Reference{
							Name: r.Config.GetProviderConfigName("AWS", req.ClusterName),
						},
					},
					ForProvider: awsec2v1beta1.RouteParameters_2{
						Region:               ptr.To(location.Spec.Provider.AWS.Region),
						RouteTableID:         ptr.To(*downstreamVPC.Status.AtProvider.MainRouteTableID),
						DestinationCidrBlock: ptr.To("0.0.0.0/0"),
						GatewayIDRef: &crossplanecommonv1.Reference{
							Name: internetGatewayObjectKey.Name,
						},
					},
				},
			}

			logger.Info("creating downstream internet egress route", "downstream_internet_egress_route", internetEgressRoute.Name)

			if err := downstreamClient.Create(ctx, &internetEgressRoute); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating downstream internet egress route: %w", err)
			}
		}

		downstreamResourceStatus = internetEgressRoute.Status.ResourceStatus
	}

	// See if there's been a failure to create the network.
	//
	// NOTE(jreese): Odd observation - the ServiceAccount failure info is in a different
	// condition. Need to look into that.
	if condition := downstreamResourceStatus.GetCondition(crossplanecommonv1.TypeSynced); condition.Reason == "ReconcileError" {
		logger.Info("network failed to create")
		programmedCondition.Reason = "NetworkFailedToCreate"
		programmedCondition.Message = fmt.Sprintf("Network failed to create: %s", condition.Message)
		return ctrl.Result{}, fmt.Errorf(programmedCondition.Message)
	}

	if downstreamResourceStatus.GetCondition(crossplanecommonv1.TypeReady).Status != corev1.ConditionTrue {
		logger.Info("provider network not ready yet")

		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = networkingv1alpha.NetworkContextProgrammedReasonProgrammingInProgress
		programmedCondition.Message = "Network is being programmed."

		return ctrl.Result{}, nil
	}

	logger.Info("network context is ready", "name", networkContext.Name)

	programmedCondition.Status = metav1.ConditionTrue
	programmedCondition.Reason = networkingv1alpha.NetworkContextProgrammed
	programmedCondition.Message = "Network is ready."

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkContextReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	return mcbuilder.ControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkContext{}).
		Watches(&networkingv1alpha.SubnetClaim{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				subnetClaim := obj.(*networkingv1alpha.SubnetClaim)

				return []mcreconcile.Request{
					{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: subnetClaim.Namespace,
								Name:      subnetClaim.Spec.NetworkContext.Name,
							},
						},
						ClusterName: clusterName,
					},
				}
			})
		}).
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &awsec2v1beta1.VPC{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*awsec2v1beta1.VPC, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, vpc *awsec2v1beta1.VPC) []mcreconcile.Request {
				// TODO(jreese): Use a function for these simple handlers - probably already have one in downstreamclient.
				logger := log.FromContext(ctx)

				upstreamClusterName := vpc.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := vpc.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := vpc.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("AWS VPC is missing upstream ownership metadata")
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
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &awsec2v1beta1.InternetGateway{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*awsec2v1beta1.InternetGateway, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, internetGateway *awsec2v1beta1.InternetGateway) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := internetGateway.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := internetGateway.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := internetGateway.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("AWS internet gateway is missing upstream ownership metadata", "name", internetGateway.Name)
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
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &awsec2v1beta1.Route{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*awsec2v1beta1.Route, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, route *awsec2v1beta1.Route) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := route.Annotations[downstreamclient.UpstreamOwnerClusterName]
				upstreamName := route.Annotations[downstreamclient.UpstreamOwnerName]
				upstreamNamespace := route.Annotations[downstreamclient.UpstreamOwnerNamespace]

				if upstreamClusterName == "" || upstreamName == "" || upstreamNamespace == "" {
					logger.Info("AWS route is missing upstream ownership metadata", "name", route.Name)
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
		WatchesRawSource(datumsource.MustNewClusterSource(r.DownstreamCluster, &gcpcomputev1beta1.Network{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[*gcpcomputev1beta1.Network, mcreconcile.Request] {
			return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, network *gcpcomputev1beta1.Network) []mcreconcile.Request {
				logger := log.FromContext(ctx)

				upstreamClusterName := network.Annotations[downstreamclient.UpstreamOwnerClusterName]

				if upstreamClusterName == "" {
					logger.Info("GCP network is missing upstream ownership metadata")
					return nil
				}

				cluster, err := mgr.GetCluster(ctx, upstreamClusterName)
				if err != nil {
					logger.Error(err, "failed to get upstream cluster")
					return nil
				}
				clusterClient := cluster.GetClient()

				listOpts := client.MatchingFields{
					networkContextControllerNetworkUIDIndex: network.GetName(),
				}

				var networkContexts networkingv1alpha.NetworkContextList
				if err := clusterClient.List(ctx, &networkContexts, listOpts); err != nil {
					logger.Error(err, "failed to list network contexts")
					return nil
				}

				var requests []mcreconcile.Request
				for _, networkContext := range networkContexts.Items {
					requests = append(requests, mcreconcile.Request{
						Request: reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: networkContext.Namespace,
								Name:      networkContext.Name,
							},
						},
						ClusterName: upstreamClusterName,
					})
				}

				return requests
			})
		})).
		Complete(r)
}
