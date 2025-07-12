// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterDownstreamInstanceSpec defines the desired state of ClusterDownstreamInstance.
type ClusterDownstreamInstanceSpec struct {
	// InstanceRef is a reference to the instance that this downstream instance is
	// associated with.
	// +kubebuilder:validation:Required
	UpstreamInstanceRef UpstreamResourceRef `json:"upstreamInstanceRef"`
}

// ClusterDownstreamInstanceStatus defines the observed state of ClusterDownstreamInstance.
type ClusterDownstreamInstanceStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ClusterDownstreamInstance is the Schema for the clusterdownstreaminstances API.
type ClusterDownstreamInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDownstreamInstanceSpec   `json:"spec,omitempty"`
	Status ClusterDownstreamInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDownstreamInstanceList contains a list of ClusterDownstreamInstance.
type ClusterDownstreamInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDownstreamInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterDownstreamInstance{}, &ClusterDownstreamInstanceList{})
}
