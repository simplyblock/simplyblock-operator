// The sysfs and procfs trees the inventory tests read.
//
// A fixture is a real tree on disk rather than an injected interface, because
// every reader in this package is a path walk: what breaks is a missing
// attribute, an unexpected symlink, or a directory that is present but empty,
// and none of those three can be expressed by a map of stubbed answers. The
// trees below are transcripts of hosts a storage node has run on.

package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture builds a tree under a temporary root from a map of relative path to
// file contents, plus a map of relative path to symlink target. The kernel
// terminates every attribute with a newline, so the writer adds one: a reader
// that forgot to trim would pass against a fixture that did not.
type fixture struct {
	files map[string]string
	links map[string]string
	dirs  []string
}

// write materializes the fixture and returns its root.
func (f fixture) write(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range f.dirs {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range f.files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, target := range f.links {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
