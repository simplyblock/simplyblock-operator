//go:build !linux

// The architecture reading on platforms this product does not run on.
//
// The package still builds and its tests still run elsewhere, because every
// other reading is a path walk a developer's machine can be pointed at. This
// one is a system call, and a machine name read off a developer's laptop would
// be an answer about the laptop.

package inventory

import (
	"errors"
	"runtime"
)

// LocalMachine reports that this platform has no host architecture to read.
//
// It is an error rather than runtime.GOARCH so that nothing can be mistaken for
// a reading taken from a host: the two differ exactly when it matters, which is
// a binary built for one architecture running on another.
func LocalMachine() (string, error) {
	return "", errors.New("inventory: reading the machine's architecture is supported on Linux only, and this is " + runtime.GOOS)
}
