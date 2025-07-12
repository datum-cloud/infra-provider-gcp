package v1alpha1

// UpstreamResourceRef can be used to provide a reference to a resource in a
// separate cluster. The target type of the upstream resource is left to the
// owner of the type which this is included in.
type UpstreamResourceRef struct {
	// The name of the cluster which the resource exists in.
	//
	// +kubebuilder:validation:Required
	ClusterName string `json:"clusterName,omitempty"`

	// The namespace in the referenced cluster which the resource exists in
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// The name of the referenced resource
	//
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
}
