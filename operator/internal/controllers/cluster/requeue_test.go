// The requeue intervals these two reconcilers back off by, asserted as values
// rather than as the bare fact that some requeue happened.
//
// Every one of them is a backstop: a queued operation is normally woken by its
// cluster, and a claim that lost its race is normally woken by the write that
// beat it. That is exactly why they are worth pinning. A backstop nothing
// reaches in a passing test is a constant free to drift, and
// design-storagecluster.md §6.1 states these numbers, so the document and the
// code are held to the same ones here.

package cluster

import (
	"context"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A queued operation waits the ordinary retry, which §6.1 calls the backstop for
// a lock-release event that was missed.
func TestAQueuedOperationRequeuesAtTheOrdinaryRetry(t *testing.T) {
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = otherOpsName
	})
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{},
		held, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown))

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testOpsName},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != opsRetry {
		t.Errorf("RequeueAfter = %v, want the ordinary retry %v", result.RequeueAfter, opsRetry)
	}
	if opsRetry != 10*time.Second {
		t.Errorf("opsRetry = %v, want 10s", opsRetry)
	}
}

// A 409 on the lock patch is a different situation from a lock somebody visibly
// holds: the object moved under this pass and the answer is one read away, so it
// backs off by the shorter contention interval rather than by the full retry. It
// is not immediate, because an immediate requeue against a contended object is a
// spin.
func TestTheContentionIntervalIsShorterThanTheRetryAndNotZero(t *testing.T) {
	if opsContended == 0 {
		t.Fatal("opsContended is zero, which makes a contended requeue a spin")
	}
	if opsContended >= opsRetry {
		t.Errorf("opsContended = %v, which is not shorter than opsRetry = %v",
			opsContended, opsRetry)
	}
	if opsContended != 5*time.Second {
		t.Errorf("opsContended = %v, want 5s", opsContended)
	}
}

// The creation claim backs off by the same interval and for the same reason: its
// optimistic-lock patch 409'd, so another reconciler is mid-claim and this pass
// re-reads rather than retrying instantly.
func TestALostCreationClaimBacksOffByTheContentionInterval(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, newUncreatedCluster())
	ctx := context.Background()

	// A reconciler holding a copy nobody has written since, against an object
	// somebody else has moved: the patch carries a resourceVersion the API
	// server will refuse.
	var stale simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: testNamespace, Name: testClusterName,
	}, &stale); err != nil {
		t.Fatalf("read the cluster: %v", err)
	}
	stale.ResourceVersion = "1"

	if got := r.claim(ctx, &stale); got.RequeueAfter != claimBackoff {
		t.Errorf("RequeueAfter = %v, want the contention interval %v",
			got.RequeueAfter, claimBackoff)
	}
	if claimBackoff != 5*time.Second {
		t.Errorf("claimBackoff = %v, want 5s", claimBackoff)
	}
}
