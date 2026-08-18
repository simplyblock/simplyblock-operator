package fabric

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

// The integration suite's assertions rest on two things that need no cluster to
// check: that ReconstructSysfs rebuilds a tree atlas's resolver can read, and
// that the expectations the suite asserts are the ones real kernel state
// produces.
//
// So this replays a snapshot captured from a live two-target fabric — the same
// fixture nvmeof's own tests use — through the same code path the integration
// test takes. When the suite fails, this says whether the fabric was wrong or
// the harness was.
func TestReconstructSysfs_ReplaysACapturedFabric(t *testing.T) {
	const (
		fixture = "../../../atlas-lib/nvmeof/testdata/sysfs/controller-not-contributing.tsv"
		nqn     = "nqn.2023-02.io.simplyblock:nvmetlab:lvol:00000001-0000-4000-8000-000000000001"
	)

	snapshot, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}

	root := filepath.Join(t.TempDir(), "sys")
	n, err := ReconstructSysfs(string(snapshot), root)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	t.Logf("reconstructed %d attributes", n)

	ctx := context.Background()
	cfg := nvme.SysfsConfig{SysRoot: root}

	sub, err := nvme.NewSysfsSubsystemResolver(cfg).ByNQN(ctx, nqn)
	if err != nil {
		t.Fatalf("resolve %s from the reconstructed tree: %v", nqn, err)
	}
	if len(sub.Controllers) != 2 {
		t.Fatalf("want 2 controllers, got %d", len(sub.Controllers))
	}
	// The captured subsystem is multi-namespace, which is the harder case: the
	// defect concerns namespace 1 and the others belong to other volumes.
	if len(sub.Namespaces) < 1 {
		t.Fatalf("want at least 1 namespace, got %d", len(sub.Namespaces))
	}
	t.Logf("subsystem %s: %d controllers, %d namespaces",
		sub.ID, len(sub.Controllers), len(sub.Namespaces))

	defects, err := nvmeof.Inspect(ctx,
		nvme.NewSysfsSubsystemResolver(cfg),
		nvme.NewSysfsDeviceResolver(cfg),
		nvme.DeviceSelector{NQN: nqn, NSID: 1}, nil)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(defects) != 1 {
		t.Fatalf("want exactly 1 defect, got %d: %+v", len(defects), defects)
	}
	if defects[0].Kind != nvmeof.DefectControllerNotContributing {
		t.Errorf("kind = %s, want %s", defects[0].Kind, nvmeof.DefectControllerNotContributing)
	}
	if defects[0].Scope != nvmeof.ScopeController {
		t.Errorf("scope = %v, want ScopeController", defects[0].Scope)
	}
	if len(defects[0].Controllers) != 1 {
		t.Fatalf("want 1 controller to tear down, got %d", len(defects[0].Controllers))
	}
	t.Logf("defect names %s at %s: %s",
		defects[0].Controllers[0].ID,
		defects[0].Controllers[0].Address.TrAddr,
		defects[0].Detail)
}

// A snapshot with nothing in it is a capture that silently failed, and the tests
// downstream would read the absence of a subsystem as the absence of a defect.
func TestReconstructSysfs_RejectsAnEmptySnapshot(t *testing.T) {
	for _, snapshot := range []string{"", "\n\n", "not a sysfs path\tvalue\n"} {
		if _, err := ReconstructSysfs(snapshot, t.TempDir()); err == nil {
			t.Errorf("ReconstructSysfs(%q) = nil error, want a failure", snapshot)
		}
	}
}