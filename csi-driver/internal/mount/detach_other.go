//go:build !linux

// Detach on a system without lazy unmounts, where the driver never runs: the
// unit tests build here.

package mount

import "errors"

// Detach is unsupported off Linux.
func (m *Mounter) Detach(string) error {
	return errors.New("detaching a mount needs Linux")
}
