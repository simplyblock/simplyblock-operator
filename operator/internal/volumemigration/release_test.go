package volumemigration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/nvme"
)

// fakeDetacher records what it was asked to tear down, and can be made to fail on
// one named controller so a partial cleanup can be asserted.
type fakeDetacher struct {
	seen   []string
	failOn string
}

func (f *fakeDetacher) DisconnectController(_ context.Context, ctrl nvme.Controller) error {
	if string(ctrl.ID) == f.failOn {
		return errors.New("delete_controller: device busy")
	}
	f.seen = append(f.seen, string(ctrl.ID))
	return nil
}

// released returns the controller IDs of rs, in the order reported.
func releasedIDs(rs []Released) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Controller)
	}
	return out
}

func assertSameSet(t *testing.T, got, want []string, what string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The leak this exists for: validation failed, so both target paths must go. They are
// parked (inaccessible on every namespace), which is exactly what makes them safe.
func TestReleaseMigrationPaths_ReleasesParkedTargets(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
		// The source path, carrying the volume. Not a migration target.
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme0", "nvme1"}, "released")
	assertSameSet(t, d.seen, []string{"nvme0", "nvme1"}, "disconnected")
}

// The safety rule. A migration target address that is already an HA path for the
// subsystem is serving, and must survive: tearing it down is the outage the whole
// package exists to avoid.
func TestReleaseMigrationPaths_KeepsServingPathAtTargetAddress(t *testing.T) {
	root := writeSysfs(t,
		// Same address as a migration target, but non-optimized => the kernel routes
		// I/O over it.
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "non-optimized", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme1"}, "released")
	if slices.Contains(d.seen, "nvme0") {
		t.Error("disconnected a path that was carrying I/O")
	}
}

// An optimized path at a target address is the post-cutover shape. Release must not be
// what notices — the caller is responsible for only calling this pre-cutover — but the
// rule protects it anyway, which is the point of keying on "carries I/O" rather than on
// a record of what we connected.
func TestReleaseMigrationPaths_KeepsOptimizedTargetAfterCutover(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "optimized", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "non-optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	if len(got) != 0 || len(d.seen) != 0 {
		t.Errorf("released %v, want nothing torn down after cutover", releasedIDs(got))
	}
}

// A target path that never came up is a controller with no namespace leg at all — the
// zero-namespace husk. It is still ours to release.
func TestReleaseMigrationPaths_ReleasesZeroNamespaceTarget(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", "connecting", "", nil},
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme0", "nvme1"}, "released")
}

// The cutover window: the control plane drives every path inaccessible for about two
// seconds while it moves the volume. Every controller then looks like it carries nothing,
// including the one about to serve the volume, so nothing may be released.
func TestReleaseMigrationPaths_DeclinesDuringTheCutoverWindow(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
		// The path that was serving, inaccessible for the length of the window.
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "inaccessible", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	if len(got) != 0 || len(d.seen) != 0 {
		t.Errorf("released %v during the cutover window, want nothing touched", releasedIDs(got))
	}
}

// The same fabric one sample later, with the volume served again: now the migration's own
// target paths are distinguishable from the serving one, and only they go.
func TestReleaseMigrationPaths_ReleasesOnceTheWindowHasPassed(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme0", "nvme1"}, "released")
}

// A path at an address the migration never asked for is none of release's business,
// however broken it looks. Reaping that is ReapDeadControllers' job.
func TestReleaseMigrationPaths_IgnoresUnrelatedAddresses(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.113:4426", "nvme5", stateLive, "", nil},
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("released %v, want nothing", releasedIDs(got))
	}
}

// A subsystem that is not attached at all is already released.
func TestReleaseMigrationPaths_SubsystemAbsent(t *testing.T) {
	root := writeSysfs(t, ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil})
	d := &fakeDetacher{}
	got, err := releaseMigrationPaths(context.Background(), root, "nqn.absent", nil, d)
	if err != nil {
		t.Fatalf("releaseMigrationPaths: %v", err)
	}
	if len(got) != 0 || len(d.seen) != 0 {
		t.Errorf("released %v for an absent subsystem", releasedIDs(got))
	}
}

func TestReleaseMigrationPaths_EmptyNQN(t *testing.T) {
	if _, err := releaseMigrationPaths(context.Background(), "", "", nil, &fakeDetacher{}); err == nil {
		t.Fatal("expected an error for an empty NQN")
	}
}

// One controller refusing to release must not strand the others: the reported set is
// what actually went, and the error names the one that did not.
func TestReleaseMigrationPaths_PartialFailure(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{failOn: "nvme0"}
	got, err := releaseMigrationPaths(context.Background(), root, pathsNQN, targetConns, d)
	if err == nil {
		t.Fatal("expected the failed disconnect to be reported")
	}
	if !strings.Contains(err.Error(), "nvme0") {
		t.Errorf("error = %q, want it to name nvme0", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme1"}, "released")
	assertSameSet(t, d.seen, []string{"nvme1"}, "disconnected")
}

// The state that permanently blocked validation on the test cluster: a live controller
// carrying no namespace, alongside a working path.
func TestReapDeadControllers_ReapsLiveZeroNamespace(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.113:4430", "nvme12", stateLive, "", nil},
		ctrlSpec{"10.0.0.114:4430", "nvme13", stateLive, "non-optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme12"}, "reaped")
}

// A controller mid-reconnect is indistinguishable from a leaked one in a snapshot, so
// it is left for ctrl_loss_tmo rather than raced.
func TestReapDeadControllers_LeavesConnecting(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.112:4426", "nvme6", "connecting", "", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme9", "connecting", "inaccessible", nil},
		ctrlSpec{"10.0.0.114:4430", "nvme13", stateLive, "non-optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reaped %v, want connecting controllers left alone", releasedIDs(got))
	}
}

// A path that is merely parked is not dead — that is every validated migration target
// pre-cutover, and reaping those would break the migration this runs in front of.
func TestReapDeadControllers_LeavesParkedTarget(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reaped %v, want parked paths left alone", releasedIDs(got))
	}
}

// Enumeration in progress: legs for some namespaces and not others. Ambiguous, so left.
func TestReapDeadControllers_LeavesPartiallyServing(t *testing.T) {
	root := writeSysfsNS(t, 3,
		ctrlSpec{"10.0.0.113:4430", "nvme2", stateLive, "optimized", []int{1}},
		ctrlSpec{"10.0.0.114:4430", "nvme3", stateLive, "non-optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reaped %v, want a partially-serving controller left alone", releasedIDs(got))
	}
}

// Reaping the last controller would drop the host's connection to the subsystem, which
// is the signal the validation Job gates on. Nothing is served either way.
func TestReapDeadControllers_NeverReapsTheLastController(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.113:4430", "nvme12", stateLive, "", nil},
		ctrlSpec{"10.0.0.114:4430", "nvme13", stateLive, "", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	if len(got) != 0 || len(d.seen) != 0 {
		t.Errorf("reaped %v, want the host connection preserved", releasedIDs(got))
	}
}

// Inspect reports a non-contributing controller once per namespace it fails to serve, so
// on a 5-namespace subsystem — the shape the test cluster failed on — one husk yields five
// defects naming it. It must be torn down once and reported once.
func TestReapDeadControllers_ReportsEachControllerOnce(t *testing.T) {
	root := writeSysfsNS(t, 5,
		ctrlSpec{"10.0.0.113:4430", "nvme12", stateLive, "", nil},
		ctrlSpec{"10.0.0.114:4430", "nvme13", stateLive, "non-optimized", nil},
	)
	d := &fakeDetacher{}
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, d)
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	assertSameSet(t, releasedIDs(got), []string{"nvme12"}, "reaped")
	assertSameSet(t, d.seen, []string{"nvme12"}, "disconnected")
}

// The reason a reap reports is the atlas defect kind, so the operator log names the
// controller with the same words VerifyMigrationPaths rejected the migration over. That
// alignment is the reason the selection goes through Inspect at all.
func TestReapDeadControllers_ReasonNamesTheDefect(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.113:4430", "nvme12", stateLive, "", nil},
		ctrlSpec{"10.0.0.114:4430", "nvme13", stateLive, "optimized", nil},
	)
	got, err := reapDeadControllers(context.Background(), root, pathsNQN, &fakeDetacher{})
	if err != nil {
		t.Fatalf("reapDeadControllers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reaped %v, want exactly nvme12", releasedIDs(got))
	}
	if got[0].Reason != string(reapableKind) {
		t.Errorf("reason = %q, want the defect kind %q", got[0].Reason, reapableKind)
	}
}

func TestReapDeadControllers_EmptyNQN(t *testing.T) {
	if _, err := reapDeadControllers(context.Background(), "", "", &fakeDetacher{}); err == nil {
		t.Fatal("expected an error for an empty NQN")
	}
}

func TestFormatReleased(t *testing.T) {
	if got := FormatReleased(nil); got != "none" {
		t.Errorf("FormatReleased(nil) = %q, want %q", got, "none")
	}
	got := FormatReleased([]Released{
		{Controller: "nvme9", Address: "10.0.0.112:4428", Reason: "b"},
		{Controller: "nvme0", Address: "10.0.0.114:4428", Reason: "a"},
	})
	want := "nvme0 at 10.0.0.114:4428 (a); nvme9 at 10.0.0.112:4428 (b)"
	if got != want {
		t.Errorf("FormatReleased = %q, want %q", got, want)
	}
}
