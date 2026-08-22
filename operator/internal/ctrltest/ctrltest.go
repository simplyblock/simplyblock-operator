// Package ctrltest provides the fake-client and fake-API scaffolding this operator's
// controller tests are built on.
//
// It is an ordinary package rather than a helper in each controller package's test
// files, because a helper declared in a _test.go file cannot be imported: every
// package that wanted one had to keep its own copy, and three copies of "build a fake
// client" drift. The naming follows the standard library's httptest, iotest and
// fstest — a normal package named for the thing it helps test.
//
// Importing [testing] from a non-test package is safe here because only _test.go files
// import ctrltest, so neither it nor the fake client it builds on reaches a production
// binary.
package ctrltest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// UnreachableAPI is a base URL that never connects. Point a client at it in a test
// that must not reach the storage API at all, so a call that should never happen fails
// loudly instead of quietly succeeding against a handler that was there anyway.
const UnreachableAPI = "http://127.0.0.1:1"

// NewScheme returns a scheme with the given types registered, failing the test if any
// registration does. Pass the AddToScheme of every API group the objects under test
// belong to: a fake client whose scheme is missing a type fails at the call rather
// than at construction, which is a long way from the cause.
func NewScheme(tb testing.TB, addToScheme ...func(*runtime.Scheme) error) *runtime.Scheme {
	tb.Helper()

	scheme := runtime.NewScheme()
	for _, add := range addToScheme {
		if err := add(scheme); err != nil {
			tb.Fatalf("failed to add scheme: %v", err)
		}
	}
	return scheme
}

// NewClient returns a fake client holding objects.
//
// statusSubresources names the kinds whose status is a real subresource, which a
// controller test almost always needs: without it the fake client folds a
// Status().Patch into the main object, so a reconciler that writes only status appears
// to write nothing and the test passes for the wrong reason.
func NewClient(
	tb testing.TB,
	scheme *runtime.Scheme,
	statusSubresources []client.Object,
	objects ...client.Object,
) client.Client {
	tb.Helper()

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(statusSubresources) > 0 {
		builder = builder.WithStatusSubresource(statusSubresources...)
	}
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	return builder.Build()
}

// NewAPIServer starts a fake storage API for the duration of the test and shuts it
// down through [testing.TB.Cleanup].
func NewAPIServer(tb testing.TB, h http.HandlerFunc) *httptest.Server {
	tb.Helper()
	srv := httptest.NewServer(h)
	tb.Cleanup(srv.Close)
	return srv
}
