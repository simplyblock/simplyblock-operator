package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T, addToScheme ...func(*runtime.Scheme) error) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range addToScheme {
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
