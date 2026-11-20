//go:build !linux

package fiemap

import (
	"errors"
	"os"
)

// Read is Linux-only: FIEMAP is a Linux ioctl.
func Read(path string, offset, length uint64) ([]Piece, error) {
	return nil, errors.ErrUnsupported
}

// ReadFile is Linux-only: FIEMAP is a Linux ioctl.
func ReadFile(f *os.File, offset, length uint64) ([]Piece, error) {
	return nil, errors.ErrUnsupported
}
