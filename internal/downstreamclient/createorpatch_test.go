package downstreamclient

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestCreateOrPatch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	tests := []struct {
		name              string
		existingObject    client.Object
		desiredObject     *corev1.ConfigMap
		clusterName       string
		parent            metav1.Object
		callback          func(*corev1.ConfigMap) error
		expectedResult    controllerutil.OperationResult
		expectedData      map[string]string
		expectedName      string
		expectedNamespace string
		wantErr           bool
	}{
		{
			name:           "create new object",
			existingObject: nil,
			desiredObject: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "test-namespace",
				},
			},
			clusterName: "test-cluster",
			parent: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "parent-pod",
					Namespace: "parent-namespace",
					UID:       uuid.NewUUID(),
				},
			},
			callback: func(obj *corev1.ConfigMap) error {
				obj.Data = map[string]string{
					"key1": "value1",
					"key2": "value2",
				}
				return nil
			},
			expectedResult:    controllerutil.OperationResultCreated,
			expectedData:      map[string]string{"key1": "value1", "key2": "value2"},
			expectedName:      "test-config",
			expectedNamespace: "test-namespace",
			wantErr:           false,
		},
		{
			name: "update existing object",
			existingObject: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "test-namespace",
				},
				Data: map[string]string{
					"old-key": "old-value",
				},
			},
			desiredObject: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "test-namespace",
				},
			},
			clusterName: "test-cluster",
			parent: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "parent-pod",
					Namespace: "parent-namespace",
					UID:       uuid.NewUUID(),
				},
			},
			callback: func(obj *corev1.ConfigMap) error {
				obj.Data = map[string]string{
					"key1": "updated-value1",
				}
				return nil
			},
			expectedResult:    controllerutil.OperationResultUpdated,
			expectedData:      map[string]string{"key1": "updated-value1"},
			expectedName:      "test-config",
			expectedNamespace: "test-namespace",
			wantErr:           false,
		},
		{
			name:           "no callback function",
			existingObject: nil,
			desiredObject: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "test-namespace",
				},
			},
			clusterName: "test-cluster",
			parent: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "parent-pod",
					Namespace: "parent-namespace",
					UID:       uuid.NewUUID(),
				},
			},
			callback:          nil,
			expectedResult:    controllerutil.OperationResultCreated,
			expectedData:      nil,
			expectedName:      "test-config",
			expectedNamespace: "test-namespace",
			wantErr:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fakeClient client.WithWatch
			if tt.existingObject != nil {
				fakeClient = fake.NewClientBuilder().
					WithScheme(scheme).
					WithObjects(tt.existingObject).
					Build()
			} else {
				fakeClient = fake.NewClientBuilder().
					WithScheme(scheme).
					Build()
			}

			result, err := CreateOrPatch(
				context.TODO(),
				fakeClient,
				tt.desiredObject,
				tt.clusterName,
				tt.parent,
				nil,
			)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedResult, result)

			// Verify the object was created/updated correctly
			var actualObj corev1.ConfigMap
			err = fakeClient.Get(context.TODO(), client.ObjectKey{
				Name:      tt.expectedName,
				Namespace: tt.expectedNamespace,
			}, &actualObj)
			require.NoError(t, err)

			// Check that upstream owner annotations were set
			annotations := actualObj.GetAnnotations()
			require.NotNil(t, annotations)
			assert.Equal(t, tt.parent.GetName(), annotations[UpstreamOwnerName])
			assert.Equal(t, tt.parent.GetNamespace(), annotations[UpstreamOwnerNamespace])
			assert.Equal(t, tt.clusterName, annotations[UpstreamOwnerClusterName])

			// Check that the expected data was set (if callback was provided)
			if tt.expectedData != nil {
				assert.Equal(t, tt.expectedData, actualObj.Data)
			}

			// Verify name and namespace
			assert.Equal(t, tt.expectedName, actualObj.GetName())
			assert.Equal(t, tt.expectedNamespace, actualObj.GetNamespace())
		})
	}
}

func TestCreateOrPatch_CallbackError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	desiredObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test-namespace",
		},
	}

	parent := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent-pod",
			Namespace: "parent-namespace",
			UID:       uuid.NewUUID(),
		},
	}

	// Test callback that returns an error
	_, err := CreateOrPatch(
		context.TODO(),
		fakeClient,
		desiredObject,
		"test-cluster",
		parent,
		nil,
	)

	assert.Error(t, err)
	assert.Equal(t, assert.AnError, err)

	// Verify the object was not created due to callback error
	var actualObj corev1.ConfigMap
	err = fakeClient.Get(context.TODO(), client.ObjectKey{
		Name:      "test-config",
		Namespace: "test-namespace",
	}, &actualObj)
	assert.Error(t, err) // Should not exist
}
