// The read-only diagnostic of what a file in an export is made of on disk:
// data, unwritten, delalloc, or hole. It answers the question a client's read
// of zeros raises, and it is confined to regular files directly inside an
// export, because it reports device offsets of whatever it is pointed at.

package nfsexport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/simplyblock/atlas/fiemap"
)

// FileExtents is what a byte range of one file in an export is made of.
type FileExtents struct {
	// Size is the file's size as the server knows it.
	Size   uint64
	Pieces []fiemap.Piece
}

// ExtentReader reads a file's extents within the exports under Root.
type ExtentReader struct {
	// Root is the directory every export is mounted directly under.
	Root string

	// read is fiemap.ReadFile, replaced in tests.
	read func(f *os.File, offset, length uint64) ([]fiemap.Piece, error)
}

// FileExtents reports what [offset, offset+length) of file in the export at
// exportPath is made of.
func (r *ExtentReader) FileExtents(
	ctx context.Context, exportPath, file string, offset, length uint64,
) (FileExtents, error) {
	path, err := r.confine(exportPath, file)
	if err != nil {
		return FileExtents{}, err
	}
	// O_NOFOLLOW and a check of what was opened, not of the name: nothing can
	// swap the file for a link between the two.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return FileExtents{}, fmt.Errorf("%w: opening %s: %v", ErrInvalidSpec, path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return FileExtents{}, err
	}
	if !info.Mode().IsRegular() {
		return FileExtents{}, fmt.Errorf("%w: %s is not a regular file", ErrInvalidSpec, path)
	}
	read := r.read
	if read == nil {
		read = fiemap.ReadFile
	}
	pieces, err := read(f, offset, length)
	if err != nil {
		return FileExtents{}, err
	}
	return FileExtents{Size: uint64(info.Size()), Pieces: pieces}, nil
}

// confine returns the file's path when the export is a directory directly
// under Root and the file a plain name in it.
func (r *ExtentReader) confine(exportPath, file string) (string, error) {
	root := filepath.Clean(r.Root)
	if exportPath != filepath.Clean(exportPath) || filepath.Dir(exportPath) != root {
		return "", fmt.Errorf("%w: %s is not an export under %s", ErrInvalidSpec, exportPath, root)
	}
	if info, err := os.Lstat(exportPath); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not an export directory", ErrInvalidSpec, exportPath)
	}
	if file == "" || file == "." || file == ".." || strings.ContainsRune(file, filepath.Separator) {
		return "", fmt.Errorf("%w: %q is not a file name", ErrInvalidSpec, file)
	}
	return filepath.Join(exportPath, file), nil
}
