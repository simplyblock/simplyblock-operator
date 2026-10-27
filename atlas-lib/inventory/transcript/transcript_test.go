// What deriving class/block leaves of a transcript: everything it does not
// derive.

package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: 2026-10-27-withclassblock-drops-root. WithClassBlock rebuilt the
// Host from its sysfs maps alone, so a capture that seeds block/ lost its
// os-release and machine name on the way to a fixture, and every host replayed
// through it read as an unknown distribution.
func TestWithClassBlockKeepsTheRootFilesAndTheMachine(t *testing.T) {
	host := Host{
		Dirs:    []string{"devices/virtual/block/vda"},
		Links:   map[string]string{"block/vda": "../devices/virtual/block/vda"},
		Root:    map[string]string{"etc/os-release": "ID=amzn\n"},
		Machine: "x86_64",
	}

	derived := host.WithClassBlock()

	if derived.Links["class/block/vda"] == "" {
		t.Fatalf("no class/block entry was derived: %v", derived.Links)
	}
	if derived.Machine != "x86_64" {
		t.Errorf("machine = %q, want x86_64", derived.Machine)
	}
	root := t.TempDir()
	if err := derived.Materialize(root); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(HostRootOf(root), "etc", "os-release")); err != nil || string(got) != "ID=amzn\n" {
		t.Errorf("os-release = %q, %v, want the captured one", got, err)
	}
}
