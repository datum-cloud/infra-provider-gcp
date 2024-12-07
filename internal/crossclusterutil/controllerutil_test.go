package crossclusterutil

import (
	"context"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSetControllerReference(t *testing.T) {
	ctx := context.TODO()
	testScheme := scheme.Scheme
	require.NoError(t, corev1.AddToScheme(testScheme)) // Register corev1 types

	// Create fake client
	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.GenerateName != "" {
					cm.Name = names.SimpleNameGenerator.GenerateName(cm.GenerateName)
				}
				return client.Create(ctx, obj, opts...)
			},
		}).
		WithScheme(testScheme).
		Build()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upstream-owner",
			Namespace: "test-owner-namespace",
			UID:       uuid.NewUUID(),
		},
	}
	controlled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controlled",
			Namespace: "test-namespace",
			UID:       uuid.NewUUID(),
		},
	}

	err := SetControllerReference(ctx, fakeClient, owner, controlled, testScheme)
	require.NoError(t, err)

	spew.Config.DisableMethods = true
	spew.Dump(controlled)

	// Validate owner reference
	controlledOwnerReferences := controlled.GetOwnerReferences()
	require.Len(t, controlledOwnerReferences, 1)
	assert.Contains(t, controlledOwnerReferences[0].Name, owner.Name)
	assert.Equal(t, "", controlled.Labels[UpstreamOwnerGroupLabel])
	assert.Equal(t, "ConfigMap", controlled.Labels[UpstreamOwnerKindLabel])
	assert.Equal(t, owner.Name, controlled.Labels[UpstreamOwnerNameLabel])
	assert.Equal(t, owner.Namespace, controlled.Labels[UpstreamOwnerNamespaceLabel])
}
