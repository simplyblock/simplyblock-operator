// Asking a subsystem's controllers to look for a namespace that was just added
// to it, which is what a resume does when the kernel has not noticed yet.

package swaprpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/nvme"
)

type fakeSubsystems struct {
	sub nvme.Subsystem
	err error
}

func (f fakeSubsystems) List(context.Context) ([]nvme.Subsystem, error) { return nil, nil }
func (f fakeSubsystems) ByNQN(context.Context, string) (nvme.Subsystem, error) {
	return f.sub, f.err
}

func controllerDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rescan_controller"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRescanAsksEveryLiveControllerOfTheSubsystem(t *testing.T) {
	root := t.TempDir()
	live1, live2, dead := controllerDir(t, root, "nvme1"), controllerDir(t, root, "nvme2"), controllerDir(t, root, "nvme3")
	rescan := SysfsRescan(fakeSubsystems{sub: nvme.Subsystem{NQN: targetNQN, Controllers: []nvme.Controller{
		{ID: "nvme1", SysfsPath: live1, State: "live"},
		{ID: "nvme2", SysfsPath: live2, State: "live"},
		{ID: "nvme3", SysfsPath: dead, State: "connecting"},
	}}})

	if err := rescan(ctx, targetNQN); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{live1: "1", live2: "1", dead: ""} {
		got, _ := os.ReadFile(filepath.Join(dir, "rescan_controller"))
		if string(got) != want {
			t.Errorf("%s: wrote %q, want %q", filepath.Base(dir), got, want)
		}
	}
}

func TestRescanOfASubsystemWithNoLiveControllerFails(t *testing.T) {
	rescan := SysfsRescan(fakeSubsystems{sub: nvme.Subsystem{NQN: targetNQN}})
	if err := rescan(ctx, targetNQN); err == nil {
		t.Fatal("a rescan reached no controller and reported success")
	}
	rescan = SysfsRescan(fakeSubsystems{err: errors.New("not attached")})
	if err := rescan(ctx, targetNQN); err == nil {
		t.Fatal("a subsystem that is not attached was rescanned")
	}
}

// Part of Regression: 2026-11-02-cg-flip-stale-source-path. A controller that is
// not live is disconnected through its delete_controller attribute; one that is
// live, or already being deleted, is left alone.
func TestDropStaleDisconnectsOnlyTheControllersThatAreNotLive(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"nvme1", "nvme2", "nvme3", "nvme4"} {
		dirs[name] = controllerDir(t, root, name)
		if err := os.WriteFile(filepath.Join(dirs[name], "delete_controller"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	drop := SysfsDropStale(fakeSubsystems{sub: nvme.Subsystem{NQN: sourceNQN, Controllers: []nvme.Controller{
		{ID: "nvme1", SysfsPath: dirs["nvme1"], State: "live"},
		{ID: "nvme2", SysfsPath: dirs["nvme2"], State: "connecting"},
		{ID: "nvme3", SysfsPath: dirs["nvme3"], State: "resetting"},
		{ID: "nvme4", SysfsPath: dirs["nvme4"], State: "deleting"},
	}}})

	n, err := drop(ctx, sourceNQN)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("dropped %d controller(s), want the 2 that are connecting or resetting", n)
	}
	for name, want := range map[string]string{"nvme1": "", "nvme2": "1", "nvme3": "1", "nvme4": ""} {
		got, _ := os.ReadFile(filepath.Join(dirs[name], "delete_controller"))
		if string(got) != want {
			t.Errorf("%s: wrote %q to delete_controller, want %q", name, got, want)
		}
	}
}

func TestDropStaleOfASubsystemNotAttachedDropsNothing(t *testing.T) {
	drop := SysfsDropStale(fakeSubsystems{err: errors.New("not attached")})
	n, err := drop(ctx, sourceNQN)
	if err != nil || n != 0 {
		t.Fatalf("dropped %d, err %v; want nothing dropped and no error for a subsystem the host does not hold", n, err)
	}
}
