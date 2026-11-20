// Support bundles for tests: a fixture directory under testdata packed the way
// kubectl-simplyblock-bundle packs a collection, so every package's tests read
// the same archives the tool is given.

package bundletest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// Archive packs a fixture directory as kubectl-simplyblock-bundle does: one top
// directory, gzip-compressed. edit may change or drop a file by its path below
// the top directory, and junk adds entries of binary metadata at the paths
// given, the way tar on macOS adds AppleDouble files.
func Archive(t *testing.T, dir string, edit func(name string, data []byte) ([]byte, bool), junk ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	const top = "simplyblock-bundle-discover-fleet-02-20261024T090000Z"
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if edit != nil {
			var keep bool
			if data, keep = edit(filepath.ToSlash(rel), data); !keep {
				return nil
			}
		}
		hdr := &tar.Header{Name: top + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range junk {
		data := []byte{0, 5, 22, 7, 0, 2, 0, 0}
		hdr := &tar.Header{Name: top + "/" + name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Fixture is the path of a fixture bundle directory, from any package of this
// module.
func Fixture(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
