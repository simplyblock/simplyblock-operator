package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// statusSubresource is the name a client passes to a SubResourceUpdate
// interceptor for a status write, which is how a test makes one fail.
const statusSubresource = "status"

// newTestScheme builds a scheme carrying both simplyblock API versions, plus
// whatever else the caller adds.
//
// Both versions are unconditional because the group has two of them: a kind with
// a renamed property is read as v1alpha2 by its controller and may still be
// written as v1alpha1 by a controller that has not moved yet, and a fake client
// that knows only one of them panics on the other. Callers still pass
// simplyblockv1alpha1.AddToScheme, which is a harmless second registration.
func newTestScheme(t *testing.T, addToScheme ...func(*runtime.Scheme) error) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range append(
		[]func(*runtime.Scheme) error{
			simplyblockv1alpha1.AddToScheme,
			simplyblockv1alpha2.AddToScheme,
		},
		addToScheme...,
	) {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to add scheme: %v", err)
		}
	}

	return scheme
}

func newTestClient(
	t *testing.T,
	scheme *runtime.Scheme,
	statusSubresources []client.Object,
	objects ...client.Object,
) client.Client {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(statusSubresources) > 0 {
		builder = builder.WithStatusSubresource(statusSubresources...)
	}
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}

	return builder.Build()
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func testCluster(namespace, clusterName, uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: uuid,
		},
	}
}
