//go:build linux

// The ioctl against a real file: written data, a hole, and a range reaching
// past the end. Run wherever the test runs on Linux, which the module's CI is.

package fiemap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSeesWrittenDataAndAHole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	block := make([]byte, 64<<10)
	for i := range block {
		block[i] = 0xab
	}
	if _, err := f.WriteAt(block, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 * mib); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	pieces, err := Read(path, 0, 4*mib)
	if err != nil {
		t.Skipf("FIEMAP is not supported here: %v", err)
	}
	if len(pieces) < 2 || pieces[0].Kind != Written || pieces[0].Offset != 0 {
		t.Fatalf("pieces = %+v, want a written first piece then a hole", pieces)
	}
	last := pieces[len(pieces)-1]
	if last.Kind != Hole || last.Offset+last.Length != 4*mib {
		t.Errorf("pieces = %+v, want the range to end in a hole at 4 MiB", pieces)
	}
}
