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
// and does not survive a checkout on a filesystem without symlinks. The capture
// is a JSON document of three maps -- directories, file contents, symlink
// targets -- materialized into a temporary tree per test.
//
// Capturing another host: run the walker in hack/inventory/capture-host.py on it
// and drop the result beside this file. The trees are small; the OKD worker below
// is 16k files and 128 KiB compressed.

package inventory

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// hostTranscript is the captured shape of one machine's sysfs.
type hostTranscript struct {
	Dirs  []string          `json:"dirs"`
	Files map[string]string `json:"files"`
	Links map[string]string `json:"links"`
}

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

	handle, err := os.Open(filepath.Join("testdata", "hosts", name+".json.gz"))
	if err != nil {
		t.Fatalf("open the host transcript: %v", err)
	}
	defer func() { _ = handle.Close() }()

	reader, err := gzip.NewReader(handle)
	if err != nil {
		t.Fatalf("read the host transcript: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var host hostTranscript
	if err := json.NewDecoder(reader).Decode(&host); err != nil {
		t.Fatalf("decode the host transcript: %v", err)
	}

	// Not t.TempDir: the tree outlives the test that first asked for it, and Go
	// removes the process's temporary directories when it exits.
	root, err := os.MkdirTemp("", "inventory-host-")
	if err != nil {
		t.Fatalf("create the host root: %v", err)
	}
	for _, dir := range host.Dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	for path, content := range host.Files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create the parent of %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for path, target := range host.Links {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create the parent of %s: %v", path, err)
		}
		_ = os.Symlink(target, full)
	}
	materialized.Store(name, root)
	return root
}

// TestTheCapturedHostAnswersEveryReader is what makes the transcript worth
// keeping: one machine, read by all of them.
//
// A reader added later gets a real host to run against for free, and a reader
// that starts disagreeing with a machine says so here rather than on a cluster.
func TestTheCapturedHostAnswersEveryReader(t *testing.T) {
	root := hostFixture(t, "okd-worker")
	host := syntheticHost(root)

	cpu, err := ReadCPU(host)
	if err != nil {
		t.Fatalf("ReadCPU: %v", err)
	}
	if cpu.OnlineCount == 0 || cpu.PhysicalCores == 0 {
		t.Errorf("the captured host reads as having no CPUs: online=%d cores=%d",
			cpu.OnlineCount, cpu.PhysicalCores)
	}
	if cpu.Sockets == 0 {
		t.Error("the captured host reads as having no sockets")
	}

	pages, err := ReadHugePages(host)
	if err != nil {
		t.Fatalf("ReadHugePages: %v", err)
	}
	if len(pages.Pools) == 0 {
		t.Error("the captured host reads as having no huge-page pools")
	}

	ifaces, err := ReadInterfaces(host)
	if err != nil {
		t.Fatalf("ReadInterfaces: %v", err)
	}
	if len(ifaces) == 0 {
		t.Error("the captured host reads as having no interfaces")
	}
}
