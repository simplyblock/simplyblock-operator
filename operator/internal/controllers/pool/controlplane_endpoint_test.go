// Whether StoragePoolReconciler reaches the control plane the ControlPlane
// object resolves to, and authenticates as its cluster once that cluster's
// secret is known -- rather than always the hardcoded in-cluster Service
// webapi.NewClient() defaults to. Before EndpointResolver existed on this
// reconciler, a pool on a ControlPlane.spec.source.managed deployment could
// never reach its control plane at all. See internal/controllers/cluster's
// identically-motivated fix (storagecluster_controller.go's clusterSecret and
// controlplane.go's clientFor).

package pool

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A pool's create reaches whatever endpoint the resolver publishes, not the
// package's built-in default. Without this, the request would try to dial
// the hardcoded in-cluster Service and never reach the stub at all -- the
// stub's own posts count is the proof, since the reconciler has no other way
// to answer testPoolUUID.
func TestAPoolReachesTheEndpointTheControlPlaneResolverPublishes(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	c := newClient(t, newCluster(testClusterUUID), newPool("tenant-a"))
	r := &StoragePoolReconciler{
		Client:           c,
		Scheme:           testScheme(t),
		Recorder:         rec,
		EndpointResolver: func(context.Context) string { return cp.server.URL },
	}

	p, _ := reconcileSettled(t, r, "tenant-a")

	if p.Status.UUID != testPoolUUID {
		t.Fatalf("status.uuid = %q, want %q -- the create never reached the resolved endpoint",
			p.Status.UUID, testPoolUUID)
	}
	if cp.posts != 1 {
		t.Errorf("the control plane was asked to create the pool %d times, want 1", cp.posts)
	}
}

// Once the pool's cluster has its own recorded secret, the create
// authenticates as that cluster instead of as this operator's own Kubernetes
// identity -- the only way to reach a control plane a different Kubernetes
// cluster runs, since a TokenReview can never cross that boundary.
func TestAPoolAuthenticatesAsItsClusterOnceTheSecretIsKnown(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + testCluster,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{"secret": []byte("cluster-own-secret")},
	}
	c := newClient(t, newCluster(testClusterUUID), newPool("tenant-a"), secret)
	r := &StoragePoolReconciler{
		Client:           c,
		Scheme:           testScheme(t),
		Recorder:         rec,
		EndpointResolver: func(context.Context) string { return cp.server.URL },
	}

	reconcileSettled(t, r, "tenant-a")

	if cp.lastAuth != "Bearer cluster-own-secret" {
		t.Errorf("authorization = %q, want the cluster's own secret", cp.lastAuth)
	}
}

// With no secret recorded yet (a cluster still being created), the call still
// goes out rather than being held -- the operator's own identity is what it
// falls back to, the same as before this fix.
func TestAPoolCallsWithNoClusterSecretDoesNotBlock(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	c := newClient(t, newCluster(testClusterUUID), newPool("tenant-a"))
	r := &StoragePoolReconciler{
		Client:           c,
		Scheme:           testScheme(t),
		Recorder:         rec,
		EndpointResolver: func(context.Context) string { return cp.server.URL },
	}

	p, _ := reconcileSettled(t, r, "tenant-a")

	if p.Status.UUID != testPoolUUID {
		t.Errorf("status.uuid = %q, want %q", p.Status.UUID, testPoolUUID)
	}
}
