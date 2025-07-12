// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ClusterDownstreamWorkloadSpec struct {
	// WorkloadRef is a reference to the workload that this
	// downstream workload is associated with.
	// +kubebuilder:validation:Required
	UpstreamWorkloadRef UpstreamResourceRef `json:"upstreamWorkloadRef"`
}

type ClusterDownstreamWorkloadStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	AggregatedSecretRef *SecretReference `json:"aggregatedSecret,omitempty"`
}

type SecretReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ClusterDownstreamWorkload is used to track and own downstream resources
// for a given workload. These are cluster scoped resources so that they can
// own the cluster scoped crossplane resources until Crossplane v2 comes out.
type ClusterDownstreamWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterDownstreamWorkloadSpec `json:"spec,omitempty"`

	// +kubebuilder:default={conditions:{{type:"Ready",status:"Unknown",reason:"Pending", message:"Waiting for controller", lastTransitionTime: "1970-01-01T00:00:00Z"}}}
	Status ClusterDownstreamWorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDownstreamWorkloadList contains a list of ClusterDownstreamWorkload.
type ClusterDownstreamWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDownstreamWorkload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterDownstreamWorkload{}, &ClusterDownstreamWorkloadList{})
}
