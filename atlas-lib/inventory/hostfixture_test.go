// Whole-host transcripts: a real machine's sysfs, captured and replayed.
//
// The fixtures beside this one are written by hand, one attribute at a time,
// which is what a test of a single reading wants. This is the other kind: every
// path under the trees the readers walk, taken off a machine a storage node runs
// on, so a reading can be checked against what a host actually exports rather
// than against what the fixture's author remembered to include.
//
// It is one file per host rather than a directory of thousands, because sysfs is
// mostly empty files and symlinks and a checked-in tree of those is unreviewable
// and does not survive a checkout on a filesystem without symlinks. The format
// and the materializing are inventory/transcript's, which the operator's
// discovery fixtures replay the same captures through; what is here is the
// caching and the t.Fatalf that only a test wants.
//
// Capturing another host: run the walker in hack/inventory/capture-host.py on it
// and drop the result beside this file. The trees are small; the OKD worker below
// is 16k files and 128 KiB compressed.

package inventory

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/simplyblock/atlas/inventory/transcript"
)

// materialized caches one transcript per process. A host is sixteen thousand
// files and writing them takes seconds, which is worth paying once and not once
// per test: nothing reading a transcript writes to it.
var materialized sync.Map

// hostFixture materializes a captured host under a temporary root and returns
// it, ready to be handed to a Config as SysfsRoot. The tree is written once per
// process and shared by every test that asks for that host.
//
// A symlink whose target cannot be created is written anyway: sysfs is full of
// links that point outside the captured subtree, and a reader that follows one
// gets the same "not found" it would get on a host where the target is a device
// that has since gone. Failing the fixture instead would make the capture depend
// on how much of the machine was walked.
func hostFixture(t *testing.T, name string) string {
	t.Helper()

	if root, ok := materialized.Load(name); ok {
		return root.(string)
	}

	host, err := transcript.Load(filepath.Join("testdata", "hosts", name+".json.gz"))
	if err != nil {
		t.Fatalf("read the host transcript: %v", err)
	}

	// Not t.TempDir: the tree outlives the test that first asked for it, and Go
	// removes the process's temporary directories when it exits.
	root, err := os.MkdirTemp("", "inventory-host-")
	if err != nil {
		t.Fatalf("create the host root: %v", err)
	}
	if err := host.Materialize(root); err != nil {
		t.Fatalf("materialize the host transcript: %v", err)
	}
	materialized.Store(name, root)
	return root
}

// devRootOf is the device-node directory of a materialized fixture, which is
// what a reader takes as DevRoot.
func devRootOf(root string) string {
	return transcript.DevRootOf(root)
}
