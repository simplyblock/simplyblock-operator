// Tests for a restore pass that reads the operation from a cache one write
// behind. The restore is guarded by the volume the operation recorded and, when
// none is recorded, by a lookup of the volume by name. The control plane lists a
// restored volume some time after it accepted the restore, so a pass reading the
// operation before the previous one recorded the volume finds nothing either way
// and asks for a second restore: a second volume nobody references.
//
// Regression: lblk_outage_matrix_k8s-20261002-111746, where the same read
// sent an Activate to the control plane twice, 32ms apart.

package backup

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func TestAPassReadingTheOperationBeforeTheRestoreDoesNotRestoreAgain(t *testing.T) {
	ctx := context.Background()
	// The pool lists nothing: the control plane has not published the
	// restored volume yet.
	api := &fakeControlPlane{restoredID: testRestoreID}
	ops := restoringOps()
	deadline := metav1.NewTime(time.Now().Add(time.Hour))
	ops.Status.Step.Deadline = &deadline
	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), heldBackup(), ops)
	cache := &testsupport.LaggingClient{Client: r.Client}
	r.Client = cache

	var before simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(ctx, opsRequest(testOpsName).NamespacedName, &before); err != nil {
		t.Fatalf("read the operation: %v", err)
	}
	if _, err := r.Reconcile(ctx, opsRequest(testOpsName)); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if api.restores != 1 {
		t.Fatalf("the first pass asked for %d restores, want 1", api.restores)
	}

	cache.Lag(&before, 1)
	if _, err := r.Reconcile(ctx, opsRequest(testOpsName)); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	cache.CatchUp()

	if api.restores != 1 {
		t.Errorf("the control plane was asked for %d restores, want 1: the second pass "+
			"read the operation before the first one recorded its volume", api.restores)
	}
}
