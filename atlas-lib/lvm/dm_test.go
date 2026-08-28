package lvm

import (
	"context"
	"errors"
	"testing"
)

func TestEscapeDMName(t *testing.T) {
	tests := []struct{ name, want string }{
		{"vdo-abc123", "vdo--abc123"},
		{"plain", "plain"},
		{"a-b-c", "a--b--c"},
	}
	for _, tt := range tests {
		if got := escapeDMName(tt.name); got != tt.want {
			t.Errorf("escapeDMName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestManager_RemoveOrphanedDMNodes(t *testing.T) {
	t.Run("no matching nodes", func(t *testing.T) {
		fake := &fakeRunner{
			out: map[string]string{joinKey([]string{"dmsetup", "ls"}): "No devices found"},
			err: map[string]error{},
		}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.RemoveOrphanedDMNodes(context.Background(), VolumeGroup{Name: "vdo-abc123"}); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}
	})

	t.Run("matches escaped names and removes them", func(t *testing.T) {
		lsOut := "vdo--abc123-vdopool-vpool\t(253:3)\n" +
			"vdo--abc123-vdopool_vdata\t(253:2)\n" +
			"rl-root\t(253:0)\n"
		fake := &fakeRunner{
			out: map[string]string{joinKey([]string{"dmsetup", "ls"}): lsOut},
			err: map[string]error{},
		}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.RemoveOrphanedDMNodes(context.Background(), VolumeGroup{Name: "vdo-abc123"}); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}

		removed := map[string]bool{}
		for _, call := range fake.calls {
			if len(call) == 3 && call[0] == "dmsetup" && call[1] == "remove" {
				removed[call[2]] = true
			}
		}
		if !removed["vdo--abc123-vdopool-vpool"] || !removed["vdo--abc123-vdopool_vdata"] {
			t.Errorf("expected both orphaned nodes removed, got calls %v", fake.calls)
		}
		if removed["rl-root"] {
			t.Error("unrelated dm node rl-root must not be removed")
		}
	})

	t.Run("a node stuck on pass one clears once its dependent is gone", func(t *testing.T) {
		removePool := joinKey([]string{"dmsetup", "remove", "vdo--abc123-vdopool-vpool"})
		poolAttempts := 0
		run := func(_ context.Context, args ...string) (string, error) {
			switch joinKey(args) {
			case joinKey([]string{"dmsetup", "ls"}):
				return "vdo--abc123-vdopool-vpool\t(253:3)\nvdo--abc123-vdopool_vdata\t(253:2)\n", nil
			case removePool:
				poolAttempts++
				if poolAttempts == 1 {
					// The pool depends on vdata: removing it first fails, then succeeds
					// on a later pass once vdata is already gone.
					return "", errors.New("device is in use")
				}
				return "", nil
			default:
				return "", nil
			}
		}
		mgr := NewManagerWithRunner(run)
		if err := mgr.RemoveOrphanedDMNodes(context.Background(), VolumeGroup{Name: "vdo-abc123"}); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}
		if poolAttempts < 2 {
			t.Errorf("expected the pool node to be retried on a later pass, got %d attempt(s)", poolAttempts)
		}
	})

	t.Run("dmsetup ls itself fails", func(t *testing.T) {
		wantErr := errors.New("dmsetup: command not found")
		fake := &fakeRunner{
			out: map[string]string{},
			err: map[string]error{joinKey([]string{"dmsetup", "ls"}): wantErr},
		}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.RemoveOrphanedDMNodes(context.Background(), VolumeGroup{Name: "vdo-abc123"}); !errors.Is(err, wantErr) {
			t.Errorf("RemoveOrphanedDMNodes() error = %v, want wrapping %v", err, wantErr)
		}
	})
}
