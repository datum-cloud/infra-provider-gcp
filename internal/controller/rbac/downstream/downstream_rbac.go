package downstream

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Crossplane GCP resources (downstream control plane)
// +kubebuilder:rbac:groups=cloudplatform.gcp.upbound.io,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudplatform.gcp.upbound.io,resources=serviceaccounts/status,verbs=get
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=firewalls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=firewalls/status,verbs=get
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=instances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=instances/status,verbs=get
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=networks/status,verbs=get
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=subnetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.gcp.upbound.io,resources=subnetworks/status,verbs=get
// +kubebuilder:rbac:groups=gcp.upbound.io,resources=providerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secretiammembers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secretiammembers/status,verbs=get
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secrets/status,verbs=get
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secretversions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secretmanager.gcp.upbound.io,resources=secretversions/status,verbs=get
