package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

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

func testCluster(namespace, clusterName, uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha2.StorageClusterSpec{},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID: uuid,
		},
	}
}

// testClusterUUID is the backend cluster every test in this package that needs
// one reports. It was the volume migration tests' constant and stayed when they
// left, because the replication tests read it too.
const testClusterUUID = "cluster-uuid"

// unreachableAPI is a base URL that always fails to connect; use it for tests
// that must never reach the storage API.
const unreachableAPI = "http://127.0.0.1:1"

// newAPIServer starts an httptest server that is closed at test end.
func newAPIServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
