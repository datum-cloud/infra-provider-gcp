package aws

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

func TestBuildWorkloadDeployment(t *testing.T) {
	workloadDeployment := &computev1alpha.WorkloadDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-container-workload-us-dfw",
			UID:  uuid.NewUUID(),
		},
		Spec: computev1alpha.WorkloadDeploymentSpec{
			WorkloadRef: computev1alpha.WorkloadReference{
				Name: "my-container-workload",
			},
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

	reconciler := NewWorkloadDeploymentReconciler().(*workloadDeploymentReconciler)

	reconcileContext := &workloadDeploymentReconcileContext{
		providerConfigName: "test-provider",
		location:           location,
		workloadDeployment: workloadDeployment,
		aggregatedK8sSecret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "my-container-workload-us-dfw-secret",
			},
		},
		interfaceVPCs: []string{
			"networkcontext-4aab3b2e-3ef8-4459-8f76-ff26448770fe",
			"networkcontext-fefa1e10-8ea4-4804-8f1b-e66adf98a432",
		},
	}

	result, err := reconciler.collectDesiredResources(reconcileContext)
	assert.NoError(t, err, "unexpected error in BuildWorkloadDeployment")

	assert.NotNil(t, result.secretsParameter)

	assert.Len(t, result.securityGroups, 2)
	assert.Len(t, result.securityGroupRules, 5)

}
