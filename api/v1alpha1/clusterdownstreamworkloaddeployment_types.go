// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ClusterDownstreamWorkloadDeploymentSpec struct {
	// WorkloadDeploymentRef is a reference to the workload deployment that this
	// downstream workload deployment is associated with.
	// +kubebuilder:validation:Required
	WorkloadDeploymentRef WorkloadDeploymentRef `json:"workloadDeploymentRef"`
}

type WorkloadDeploymentRef struct {
	UpstreamClusterName string `json:"upstreamClusterName"`
	Namespace           string `json:"namespace"`
	Name                string `json:"name"`
}

type ClusterDownstreamWorkloadDeploymentStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ResourceRefs is a list of references to downstream resources managed by
	// the workload deployment.
	ResourceRefs []corev1.ObjectReference `json:"resourceRefs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ClusterDownstreamWorkloadDeployment is used to track and own downstream resources
// for a given workload deployment. These are cluster scoped resources so that
// they can own the cluster scoped crossplane resources until Crossplane v2
// comes out.
type ClusterDownstreamWorkloadDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDownstreamWorkloadDeploymentSpec   `json:"spec,omitempty"`
	Status ClusterDownstreamWorkloadDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDownstreamWorkloadDeploymentList contains a list of ClusterDownstreamWorkloadDeployment.
type ClusterDownstreamWorkloadDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDownstreamWorkloadDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterDownstreamWorkloadDeployment{}, &ClusterDownstreamWorkloadDeploymentList{})
}
