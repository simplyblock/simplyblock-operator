// whitebox test of the guardian's publish/unpublish registry — the
// authoritative "should be connected" set reconnectSubsystems checks host
// state against.
package util

import (
	"testing"
	"time"
)

func newTestGuardian() *Guardian {
	return &Guardian{
		lvols:              map[string]*LvolState{},
		lastRestart:        map[string]time.Time{},
		clusterWasInactive: map[string]bool{},
	}
}

func TestTrackedLvolsReflectsPublish(t *testing.T) {
	g := newTestGuardian()

	const (
		clusterID = "c1"
		lvolID    = "lvol-1"
		nqn       = "nqn.2023-02.io.simplyblock:c1:lvol:master-lvol"
		podUID    = "pod-uid-1"
	)
	targetPath := "/var/lib/kubelet/pods/" + podUID + "/volumes/kubernetes.io~csi/pvc-x/mount"

	g.RegisterPublish(clusterID, lvolID, nqn, targetPath)

	tracked := g.TrackedLvols()
	tl, ok := tracked[lvolID]
	if !ok {
		t.Fatalf("TrackedLvols() missing lvol %q after RegisterPublish", lvolID)
	}
	if tl.ClusterID != clusterID || tl.NQN != nqn {
		t.Fatalf("TrackedLvols()[%q] = %+v, want clusterID=%q nqn=%q", lvolID, tl, clusterID, nqn)
	}
	if tl.AlreadyBroken {
		t.Fatalf("TrackedLvols()[%q].AlreadyBroken = true, want false before any MarkBrokenLvol", lvolID)
	}

	g.RegisterUnpublishByTargetPath(targetPath)
	if _, ok := g.TrackedLvols()[lvolID]; ok {
		t.Fatalf("TrackedLvols() still contains %q after RegisterUnpublishByTargetPath", lvolID)
	}
}

// A lvol with no NQN (e.g. state loaded from an older on-disk format, or a
// publish call that raced RegisterPublish before the NQN field landed) must
// never surface from TrackedLvols: reconnectSubsystems has nothing correct
// to check it against.
func TestTrackedLvolsSkipsMissingNQN(t *testing.T) {
	g := newTestGuardian()
	g.RegisterPublish("c1", "lvol-1", "", "/var/lib/kubelet/pods/pod-uid-1/volumes/kubernetes.io~csi/pvc-x/mount")

	if _, ok := g.TrackedLvols()["lvol-1"]; ok {
		t.Fatal("TrackedLvols() should skip a tracked lvol with an empty NQN")
	}
}

func TestTrackedLvolsMarksAlreadyBroken(t *testing.T) {
	g := newTestGuardian()
	const lvolID = "lvol-1"
	g.RegisterPublish("c1", lvolID, "nqn.2023-02.io.simplyblock:c1:lvol:master-lvol",
		"/var/lib/kubelet/pods/pod-uid-1/volumes/kubernetes.io~csi/pvc-x/mount")

	g.MarkBrokenLvol(lvolID)

	tl, ok := g.TrackedLvols()[lvolID]
	if !ok {
		t.Fatalf("TrackedLvols() missing lvol %q after MarkBrokenLvol", lvolID)
	}
	if !tl.AlreadyBroken {
		t.Fatalf("TrackedLvols()[%q].AlreadyBroken = false, want true after MarkBrokenLvol", lvolID)
	}
}
