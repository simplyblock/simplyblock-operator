// Tests for a pass that reads the operation from a cache one write behind. The
// manager's client serves reads from an informer, and a pass that runs straight
// after another one can read the operation as it stood before that pass wrote
// its migration, or the instant it continued it. Neither call is idempotent: a
// second creation is a second migration of the subsystem, and a repeated
// continue is the shape that has lost writes.
//
// Regression: lblk_outage_matrix_k8s-20261002-111746, where the same read
// sent an Activate to the control plane twice, 32ms apart.

package volume

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// laggingReconciler is a reconciler over the test world behind a client that
// can answer from a copy one write behind.
func laggingReconciler(
	t *testing.T, api *fakeControlPlane,
) (*PersistentVolumeOpsReconciler, *testsupport.LaggingClient) {
	t.Helper()
	r := testReconciler(t, api, testWorld()...)
	cache := &testsupport.LaggingClient{Client: r.Client}
	r.Client = cache
	return r, cache
}

// passUntil runs passes until done reports true, and returns the operation as
// it stood before the pass that made it true.
func passUntil(
	t *testing.T, r *PersistentVolumeOpsReconciler, done func() bool,
) *simplyblockv1alpha2.PersistentVolumeOps {
	t.Helper()
	for range 10 {
		before := operationFrom(t, r)
		runPass(t, r)
		if done() {
			return before
		}
	}
	t.Fatal("ten passes did not get there")
	return nil
}

func TestAPassReadingTheOperationBeforeTheCreationDoesNotCreateAgain(t *testing.T) {
	api := idleSubsystem()
	r, cache := laggingReconciler(t, api)

	before := passUntil(t, r, func() bool { return api.creates > 0 })
	cache.Lag(before, 1)
	runPass(t, r)

	if api.creates != 1 {
		t.Errorf("the control plane was asked for %d migrations, want 1: the second pass "+
			"read the operation before the first one recorded its migration", api.creates)
	}
}

func TestAPassReadingTheOperationBeforeTheContinueDoesNotContinueAgain(t *testing.T) {
	api := idleSubsystem()
	r, cache := laggingReconciler(t, api)
	if err := atStep(r, stepMigrating); err != nil {
		t.Fatalf("place the operation at Migrating: %v", err)
	}

	before := passUntil(t, r, func() bool { return api.continues > 0 })
	cache.Lag(before, 1)
	runPass(t, r)

	if api.continues != 1 {
		t.Errorf("the copy was continued %d times, want 1: the second pass read the "+
			"operation before the first one recorded the continue", api.continues)
	}
}

// The pass that created the migration records it straight afterward, and its
// cache has not seen the claim either. If that write gave up, the creation would
// be recorded nowhere, and the next pass past the lease would create another.
func TestThePassThatCreatedTheMigrationRecordsItBeforeItsCacheSeesTheClaim(t *testing.T) {
	api := idleSubsystem()
	r, cache := laggingReconciler(t, api)

	for range 10 {
		before := operationFrom(t, r)
		if before.Status.Step.State == string(stepValidating) {
			cache.Lag(before, 20)
			break
		}
		runPass(t, r)
	}
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: testOpsName}})
	cache.Reads = 0
	if err != nil {
		t.Fatalf("the pass that created the migration failed to record it: %v", err)
	}

	if api.creates != 1 {
		t.Fatalf("the control plane was asked for %d migrations, want 1", api.creates)
	}
	if ops := operationFrom(t, r); ops.Status.Migration == nil ||
		ops.Status.Migration.MigrationUUID != testMigrationID {
		t.Errorf("migration = %+v, want the one that was created recorded", ops.Status.Migration)
	}
}
