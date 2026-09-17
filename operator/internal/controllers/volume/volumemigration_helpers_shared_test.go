// The scaffolding the registered kind's tests brought with them when the
// controller moved into this package.
//
// It is a copy of what internal/controller's tests share, and the duplication
// is deliberate rather than overlooked: a test helper cannot be imported across
// packages without being exported into the build, and the six other test files
// that still use the original are reason enough to leave it where it is. Both
// copies go when the registered kind does.
//
// Everything here is the registered kind's. The redesigned kind's own
// scaffolding is in helpers_test.go, and the two are kept apart so that
// deleting one is a file rather than an excavation.

package volume

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
// Both versions are unconditional because the group has two of them: a kind
// with a renamed property is read as v1alpha2 by its controller and may still
// be written as v1alpha1 by a controller that has not moved yet, and a fake
// client that knows only one of them panics on the other.
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

// testCluster is the StorageCluster the realignment counter is written on. Only
// the backend UUID varies between the cases: what they are about is whether the
// UUID a move reports matches the one a cluster claims.
func testCluster(uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realignClusterName,
			Namespace: realignNamespace,
		},
		Status: simplyblockv1alpha2.StorageClusterStatus{UUID: uuid},
	}
}

// unreachableAPI is a base URL that always fails to connect, for tests that
// must never reach the control plane.
const unreachableAPI = "http://127.0.0.1:1"

// newAPIServer starts a test server that is closed at test end.
func newAPIServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
