// Small polling and value helpers every test in this package reaches for: none
// of this package's fakes are watches, so a test proving one of them has to
// poll for the effect rather than block on a channel, the same shape
// atlas-lib/link's own tests use for the same reason (an in-process fake
// standing in for something that would otherwise need a live cluster).

package objectstore

import (
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ctrlRequest is the reconcile.Request naming obj, for a reconciler called
// directly rather than through a manager's work queue.
func ctrlRequest(obj client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

// ctrlRequestKey is ctrlRequest for a caller that already has the key rather
// than the object.
func ctrlRequestKey(key client.ObjectKey) ctrl.Request {
	return ctrl.Request{NamespacedName: key}
}

// apierrorsIsNotFound names apierrors.IsNotFound locally so a test asserting
// a delete took effect reads without its own import line.
func apierrorsIsNotFound(err error) bool { return apierrors.IsNotFound(err) }

// newTestScheme builds a scheme carrying core Kubernetes (PersistentVolumeClaim,
// PersistentVolume, Event), NFSExport, and S3Bucket/ObjectStore, which is
// every kind the phase unit tests construct.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
		objectstorev1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}

// newTestClient is a fake client over newTestScheme, with the status
// subresource registered for every kind these tests write status on.
func newTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithStatusSubresource(
			&objectstorev1alpha1.S3Bucket{},
			&simplyblockv1alpha2.NFSExport{},
		).
		WithObjects(objects...).
		Build()
}

// waitFor polls cond until it holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// neverWithin fails if cond becomes true inside d. It is how a test asserts a
// negative: nothing happening is the expected outcome, and only time
// distinguishes that from something that has not happened yet.
func neverWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("expected %s never to happen, but it did", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mustQuantity parses a resource.Quantity literal, panicking on a malformed
// one: these are fixed test fixtures, never user input, so a parse failure is
// a typo in the test itself.
func mustQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic("mustQuantity: " + s + ": " + err.Error())
	}
	return q
}
