package providers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	infrav1alpha1 "go.datum.net/infra-provider-gcp/api/v1alpha1"
	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// TODO(jreese) make a layer on top of this so that controllers don't need to
// look at whether they're reconciling gcp vs aws vs other

type InstanceReconciler interface {
	Reconcile(
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
	) (ctrl.Result, error)

	RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error

	Finalize(
		ctx context.Context,
		upstreamClient client.Client,
		downstreamCluster cluster.Cluster,
		downstreamInstance *infrav1alpha1.ClusterDownstreamInstance,
	) (FinalizeResult, error)
}

type WorkloadDeploymentReconciler interface {
	Reconcile(
		ctx context.Context,
		upstreamClient client.Client,
		downstreamStrategy downstreamclient.ResourceStrategy,
		downstreamClient client.Client,
		clusterName string,
		location *networkingv1alpha.Location,
		workload *computev1alpha.Workload,
		workloadDeployment *computev1alpha.WorkloadDeployment,
		downstreamWorkloadDeployment *infrav1alpha1.ClusterDownstreamWorkloadDeployment,
		aggregatedK8sSecret *corev1.Secret,
	) (ctrl.Result, error)

	RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error

	Finalize(
		ctx context.Context,
		upstreamClient client.Client,
		downstreamCluster cluster.Cluster,
		workloadDeployment *computev1alpha.WorkloadDeployment,
		downstreamWorkloadDeployment *infrav1alpha1.ClusterDownstreamWorkloadDeployment,
	) (FinalizeResult, error)
}

type WorkloadReconciler interface {
	Reconcile(
		ctx context.Context,
		downstreamStrategy downstreamclient.ResourceStrategy,
		downstreamClient client.Client,
		clusterName string,
		workload *computev1alpha.Workload,
		downstreamWorkload *infrav1alpha1.ClusterDownstreamWorkload,
	) (ctrl.Result, error)

	RegisterWatches(downstreamCluster cluster.Cluster, builder *mcbuilder.TypedBuilder[mcreconcile.Request]) error

	Finalize(
		ctx context.Context,
		upstreamClient client.Client,
		downstreamCluster cluster.Cluster,
		workload *computev1alpha.Workload,
		downstreamWorkload *infrav1alpha1.ClusterDownstreamWorkload,
	) (FinalizeResult, error)
}

// FinalizeResult is the action result of a Finalize call.
type FinalizeResult string

const (
	// FinalizeResultError means that an error was encountered during
	FinalizeResultError FinalizeResult = "error"

	// FinalizeResultComplete means that the resource has completed finalizing
	FinalizeResultComplete FinalizeResult = "complete"

	// FinalizeResultPending means that the resource is not done finalizing and
	// the finalizer should not be removed.
	FinalizeResultPending FinalizeResult = "pending"
)
