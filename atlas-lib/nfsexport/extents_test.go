// The diagnostic reader of a file's extents may only look at a regular file
// directly inside an export: every way out of that is refused.

package nfsexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/fiemap"
)

// extentsUnderRoot builds a reader over a temporary exports root holding one
// export with one file, and records what the filesystem reader was asked.
func extentsUnderRoot(t *testing.T) (r *ExtentReader, root, export string, asked *[]string) {
	t.Helper()
	root = t.TempDir()
	export = filepath.Join(root, "default-claim-1234")
	if err := os.Mkdir(export, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(export, "f.r1"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	asked = &[]string{}
	r = &ExtentReader{Root: root, read: func(f *os.File, offset, length uint64) ([]fiemap.Piece, error) {
		*asked = append(*asked, f.Name())
		return []fiemap.Piece{{Offset: offset, Length: length, Kind: fiemap.Written}}, nil
	}}
	return r, root, export, asked
}

func TestAFileInsideAnExportIsRead(t *testing.T) {
	r, _, export, asked := extentsUnderRoot(t)

	got, err := r.FileExtents(context.Background(), export, "f.r1", 0, 4096)
	if err != nil {
		t.Fatalf("FileExtents: %v", err)
	}
	if got.Size != 8192 || len(got.Pieces) != 1 {
		t.Errorf("FileExtents = %+v, want size 8192 and the reader's piece", got)
	}
	if len(*asked) != 1 || (*asked)[0] != filepath.Join(export, "f.r1") {
		t.Errorf("reader asked %v", *asked)
	}
}

func TestEveryPathOutOfAnExportIsRefused(t *testing.T) {
	r, root, export, asked := extentsUnderRoot(t)
	if err := os.Symlink("/etc/hostname", filepath.Join(export, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(export, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"an export outside the root":   {"/etc", "hostname"},
		"the root itself":              {root, "default-claim-1234"},
		"an export nested in another":  {filepath.Join(export, "dir"), "x"},
		"a traversing export path":     {filepath.Join(root, "..", "x"), "f"},
		"a file name with a separator": {export, "dir/x"},
		"a parent file name":           {export, ".."},
		"the export itself":            {export, "."},
		"a symlink":                    {export, "link"},
		"a directory":                  {export, "dir"},
	}
	for name, c := range cases {
		if _, err := r.FileExtents(context.Background(), c[0], c[1], 0, 4096); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s (%s, %s): err = %v, want ErrInvalidSpec", name, c[0], c[1], err)
		}
	}
	if len(*asked) != 0 {
		t.Errorf("the filesystem was read for a refused path: %v", *asked)
	}
}
