package providers

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/infra-provider-gcp/internal/controller/cloudinit"
	"go.datum.net/infra-provider-gcp/internal/downstreamclient"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

type Reconciler interface {
	Reconcile(
		ctx context.Context,
		downstreamStrategy downstreamclient.ResourceStrategy,
		downstreamClient client.Client,
		projectName string,
		location networkingv1alpha.Location,
		workload computev1alpha.Workload,
		workloadDeployment computev1alpha.WorkloadDeployment,
		instance computev1alpha.Instance,
		cloudConfig *cloudinit.CloudConfig,
	) (ctrl.Result, error)

	RegisterWatches(*mcbuilder.TypedBuilder[mcreconcile.Request]) error
}
