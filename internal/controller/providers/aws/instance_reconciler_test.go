package aws

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

func TestBuildInstance(t *testing.T) {
	workloadDeployment := &computev1alpha.WorkloadDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-container-workload-us-dfw",
			UID:  uuid.NewUUID(),
		},
	}
	instance := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-container-workload-us-dfw-0",
			UID:  uuid.NewUUID(),
		},
		Spec: computev1alpha.InstanceSpec{
			NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
				{
					Network: networkingv1alpha.NetworkRef{
						Name: "default",
					},
				},
				{
					Network: networkingv1alpha.NetworkRef{
						Name: "corp",
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

	reconciler := NewInstanceReconciler().(*instanceReconciler)

	reconcileContext := &instanceReconcileContext{
		providerConfigName: "test-provider",
		location:           location,
		workloadDeployment: workloadDeployment,
		instance:           instance,
		interfaceSubnets: []string{
			"test",
			"test2",
		},
		instanceProfileName: "instanceprofile",
		userData:            []byte("userdata"),
	}

	result, err := reconciler.collectDesiredResources(reconcileContext)
	assert.NoError(t, err, "unexpected error in BuildInstance")

	assert.Len(t, result.networkInterfaces, len(instance.Spec.NetworkInterfaces))

	for interfaceIndex, awsNetworkInterface := range result.networkInterfaces {
		assert.Equal(t, reconcileContext.providerConfigName, awsNetworkInterface.Spec.ProviderConfigReference.Name)
		assert.Equal(t, location.Spec.Provider.AWS.Region, ptr.Deref(awsNetworkInterface.Spec.ForProvider.Region, ""))
		assert.Equal(t, reconcileContext.interfaceSubnets[interfaceIndex], awsNetworkInterface.Spec.ForProvider.SubnetIDRef.Name)
		assert.Equal(t, fmt.Sprintf("workloaddeployment-%s-net-%d", reconcileContext.workloadDeployment.UID, interfaceIndex), awsNetworkInterface.Spec.ForProvider.SecurityGroupRefs[0].Name)

		interfaceAttachment := result.instance.Spec.ForProvider.NetworkInterface[interfaceIndex]

		assert.NotNil(t, interfaceAttachment.NetworkInterfaceIDRef)
		assert.Equal(t, awsNetworkInterface.Name, interfaceAttachment.NetworkInterfaceIDRef.Name)
	}

	assert.Len(t, result.elasticIPs, 1)
	elasticIp := result.elasticIPs[0]
	assert.Equal(t, reconcileContext.providerConfigName, elasticIp.Spec.ProviderConfigReference.Name)
	assert.Equal(t, location.Spec.Provider.AWS.Region, ptr.Deref(elasticIp.Spec.ForProvider.Region, ""))
	assert.Equal(t, result.networkInterfaces[0].Name, elasticIp.Spec.ForProvider.NetworkInterfaceRef.Name)

	assert.NotNil(t, result.instance)
	assert.Len(t, result.instance.Spec.ForProvider.NetworkInterface, len(instance.Spec.NetworkInterfaces))
	assert.Equal(t, reconcileContext.providerConfigName, result.instance.Spec.ProviderConfigReference.Name)
	assert.Equal(t, reconcileContext.instanceProfileName, ptr.Deref(result.instance.Spec.ForProvider.IAMInstanceProfile, ""))
	assert.NotEmpty(t, ptr.Deref(result.instance.Spec.ForProvider.UserDataBase64, ""))

}
