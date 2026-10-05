// The subsystem lock under the conditions a controller actually meets: a cache
// that lags its own writes, a write that conflicts with an unrelated update,
// and a write that fails outright halfway through an acquisition.
//
// The lock is only a lock if none of those can leave it half taken or strip it
// from the operation holding it. Each case drives one of them through the
// cached client, while the uncached reader sees what the API server holds.

package volume

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// withCachedClient rewires the reconciler's cached client through funcs, while
// the uncached reader keeps reading the store directly.
func withCachedClient(r *PersistentVolumeOpsReconciler, funcs interceptor.Funcs) {
	r.Client = interceptor.NewClient(r.Reader.(client.WithWatch), funcs)
}

// lockedBy returns both volumes of the shared subsystem annotated with holder.
func lockedBy(holder string) (*corev1.PersistentVolume, *corev1.PersistentVolume) {
	named, sibling := testVolumeObject(), siblingVolumeObject()
	for _, pv := range []*corev1.PersistentVolume{named, sibling} {
		pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: holder}
	}
	return named, sibling
}

// Regression: 2026-10-06-pvops-lock-rollback-on-own-lock — a running operation
// whose own lock had not reached the cache missed the fast path, patched its
// volume against the stale copy, conflicted with its own earlier write, and
// rolled back by stripping the locks it held while its migration ran.
func TestARunningOperationReadingAStaleCopyKeepsItsLocks(t *testing.T) {
	ops := testOperation()
	ops.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{State: string(stepMigrating)}
	named, sibling := lockedBy(testOpsName)
	r := testReconciler(t, sharedSubsystem(), ops, named, sibling)

	stale := testVolumeObject()
	stale.ResourceVersion = "1"
	lock, err := r.acquireLock(context.Background(), ops, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !lock.acquired {
		t.Error("the running operation lost its claim to a subsystem it holds")
	}
	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != testOpsName {
			t.Errorf("volume %s is locked by %q, want the running operation %q", name, got, testOpsName)
		}
	}
}

// Regression: 2026-10-06-pvops-release-misses-uncached-lock — the release found
// its volumes through the cache, so a release right after an acquisition missed
// the annotations it had just written and left them on the volumes.
func TestReleaseFindsLocksTheCacheHasNotSeen(t *testing.T) {
	ops := testOperation()
	named, sibling := lockedBy(testOpsName)
	r := testReconciler(t, sharedSubsystem(), ops, named, sibling)
	withCachedClient(r, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if pvs, ok := list.(*corev1.PersistentVolumeList); ok {
				for i := range pvs.Items {
					delete(pvs.Items[i].Annotations, simplyblockv1alpha2.PersistentVolumeOpsLock)
				}
			}
			return nil
		},
	})

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != "" {
			t.Errorf("volume %s is still locked by %q", name, got)
		}
	}
}

// Regression: 2026-10-06-pvops-release-conflict-read-as-done — a release whose
// patch met a conflict from an unrelated update of the volume reported success
// with the annotation still naming the operation.
func TestAReleaseThatConflictsIsRetried(t *testing.T) {
	ops := testOperation()
	named, sibling := lockedBy(testOpsName)
	r := testReconciler(t, sharedSubsystem(), ops, named, sibling)
	conflicted := false
	withCachedClient(r, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !conflicted {
				conflicted = true
				return apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumes"}, obj.GetName(),
					errors.New("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != "" {
			t.Errorf("volume %s is still locked by %q after a conflicting release", name, got)
		}
	}
}

// Regression: 2026-10-06-pvops-partial-lock-on-error — an acquisition whose
// second volume failed to patch returned the error with the first volume still
// locked, so a queued operation held part of a subsystem.
func TestAnAcquisitionThatFailsHalfwayHoldsNothing(t *testing.T) {
	ops := testOperation()
	r := testReconciler(t, sharedSubsystem(), ops, testVolumeObject(), siblingVolumeObject())
	patches := 0
	withCachedClient(r, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			if patches == 2 {
				return apierrors.NewInternalError(errors.New("etcd is unhappy"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	var pv corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPVName}, &pv); err != nil {
		t.Fatal(err)
	}
	if _, err := r.acquireLock(context.Background(), ops, &pv); err == nil {
		t.Fatal("an acquisition whose patch failed reported no error")
	}
	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != "" {
			t.Errorf("volume %s is locked by %q after a failed acquisition, want nothing held", name, got)
		}
	}
}
