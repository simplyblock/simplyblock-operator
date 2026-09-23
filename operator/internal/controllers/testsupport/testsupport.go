// The helpers every controller suite in this repository uses, as a real package
// rather than a _test.go file.
//
// The distinction is not stylistic. A _test.go file is compiled only into its own
// package's test binary, so a helper declared in one cannot be imported by
// another, and the controllers are one package per domain
// (design-crd-model.md §7.10). Keeping them here is what stops a third copy of
// the same scheme builder appearing with each domain that moves.
//
// There is no shared *controller* package beside it, and that is deliberate: what
// once looked like one held only the auto-rebalancer's Job scaffolding, which
// belongs to the volume domain.

package testsupport

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// StatusSubresource is the name a client passes to a SubResourceUpdate
// interceptor for a status write, which is how a test makes one fail.
const StatusSubresource = "status"

// NewScheme builds a scheme carrying both simplyblock API versions, plus whatever
// else the caller adds.
//
// Both versions are unconditional because the group has two of them: a kind with a
// renamed property is read as v1alpha2 by its controller and may still be written
// as v1alpha1 by a controller that has not moved yet, and a fake client that knows
// only one of them panics on the other.
func NewScheme(t *testing.T, addToScheme ...func(*runtime.Scheme) error) *runtime.Scheme {
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

// NewClient builds a fake client over the scheme, with the given kinds served
// through a status subresource.
//
// The subresource list is not optional decoration: without it a fake client
// applies a status write to the whole object, so a test asserting that a spec is
// left alone passes against a controller that overwrites it.
func NewClient(
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

// Contains reports whether a slice holds a value, which is what an assertion over
// a set of names or reasons asks.
func Contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// Cluster is a StorageCluster the control plane has already created, which is the
// precondition almost every controller in this repository holds on.
func Cluster(namespace, name, uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: uuid},
	}
}
