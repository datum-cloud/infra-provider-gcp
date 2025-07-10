package aws

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

func TestBuildWorkload(t *testing.T) {
	workload := &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-container-workload",
			UID:  uuid.NewUUID(),
		},
		Spec: computev1alpha.WorkloadSpec{
			Template: computev1alpha.InstanceTemplateSpec{
				Spec: computev1alpha.InstanceSpec{
					NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
						{
							Network: networkingv1alpha.NetworkRef{
								Name: "default",
							},
							NetworkPolicy: &computev1alpha.InstanceNetworkInterfaceNetworkPolicy{
								Ingress: []networkingv1alpha.NetworkPolicyIngressRule{
									{
										Ports: []networkingv1alpha.NetworkPolicyPort{
											{
												Port: ptr.To(intstr.FromInt(19999)),
											},
											{
												Port: ptr.To(intstr.FromInt(22)),
											},
											{
												Port:    ptr.To(intstr.FromInt(80)),
												EndPort: ptr.To(int32(81)),
											},
										},
										From: []networkingv1alpha.NetworkPolicyPeer{
											{
												IPBlock: &networkingv1alpha.IPBlock{
													CIDR: "0.0.0.0/0",
												},
											},
										},
									},
								},
							},
						},
						{
							Network: networkingv1alpha.NetworkRef{
								Name: "corp",
							},
						},
					},
				},
			},
		},
	}

	location := &networkingv1alpha.Location{
		Spec: networkingv1alpha.LocationSpec{
			Provider: networkingv1alpha.LocationProvider{
				AWS: &networkingv1alpha.AWSLocationProvider{
					Region: "us-east-1",
					Zone:   "us-east-1a",
				},
			},
		},
	}

	reconciler := NewWorkloadReconciler().(*workloadReconciler)

	reconcileContext := &workloadReconcileContext{
		providerConfigName: "test-provider",
		location:           location,
		workload:           workload,
	}

	result, err := reconciler.collectDesiredResources(reconcileContext)
	assert.NoError(t, err, "unexpected error in BuildWorkload")

	assert.Equal(t, fmt.Sprintf("workload-%s", workload.UID), result.instanceProfileIAMRole.Name)
	assert.Equal(t, fmt.Sprintf("workload-%s", workload.UID), result.instanceProfile.Name)

}
