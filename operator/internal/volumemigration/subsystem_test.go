package volumemigration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds a sysfs tree with one nvme-subsystem directory per NQN, which is
// what the resolver reads (class/nvme-subsystem, not class/nvme).
func fakeSysfs(t *testing.T, nqns ...string) string {
	t.Helper()
	root := t.TempDir()
	for i, nqn := range nqns {
		dir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys"+string(rune('0'+i)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "subsysnqn"), []byte(nqn+"\n"), 0o644); err != nil {
			t.Fatalf("write subsysnqn: %v", err)
		}
	}
	return root
}

func TestHostHasSubsystem(t *testing.T) {
	const want = "nqn.2023-02.io.simplyblock:cluster:lvol:vol-1"
	other := "nqn.2023-02.io.simplyblock:cluster:lvol:vol-2"

	t.Run("connected", func(t *testing.T) {
		present, err := HostHasSubsystem(context.Background(), fakeSysfs(t, other, want), want)
		if err != nil {
			t.Fatalf("HostHasSubsystem: %v", err)
		}
		if !present {
			t.Errorf("present = false, want true — the host holds this subsystem")
		}
	})

	t.Run("not connected", func(t *testing.T) {
		present, err := HostHasSubsystem(context.Background(), fakeSysfs(t, other), want)
		if err != nil {
			t.Fatalf("HostHasSubsystem: %v", err)
		}
		if present {
			t.Errorf("present = true, want false")
		}
	})

	// The dangerous direction: reporting "not connected" for a host that is in fact
	// connected would let the migration cut over without switching its paths. An
	// unreadable or empty sysfs must therefore be an error, never a clean "absent".
	t.Run("sysfs not visible is an error, not absence", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			sysRoot string
		}{
			{"no subsystems at all", fakeSysfs(t)},
			{"sysfs missing entirely", filepath.Join(t.TempDir(), "nonexistent")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				present, err := HostHasSubsystem(context.Background(), tc.sysRoot, want)
				if err == nil {
					t.Fatalf("expected an error, got present=%v", present)
				}
				if present {
					t.Errorf("present = true alongside an error")
				}
			})
		}
	})

	t.Run("empty NQN is rejected", func(t *testing.T) {
		if _, err := HostHasSubsystem(context.Background(), fakeSysfs(t, want), ""); err == nil {
			t.Errorf("expected an error for an empty NQN")
		} else if !strings.Contains(err.Error(), "empty NQN") {
			t.Errorf("error = %v, want it to mention the empty NQN", err)
		}
	})
}
