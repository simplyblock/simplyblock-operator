// Package-local test scheme builder: the consistencygroup package's copy of
// the flat controller package's helper, carried along when the ops controller
// moved into the domain-package layout, because Go test helpers do not cross
// package boundaries.
package consistencygroup

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// newTestScheme builds a scheme carrying both simplyblock API versions, plus
// whatever else the caller adds. Both versions are unconditional because the
// group has two of them, and a fake client that knows only one panics on the
// other.
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
