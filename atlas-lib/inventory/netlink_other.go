//go:build !linux

// The link-identity reading on platforms this product does not run on.
//
// The package still builds and its tests still run elsewhere, because every
// other interface reading is a path walk a developer's machine can be pointed
// at. This one is a netlink request, which only Linux answers.

package inventory

import (
	"errors"
	"runtime"
)

// LocalLinks reports that this platform has no netlink to read the tags from.
// ReadInterfaces treats the error as an absent answer, so the interfaces are
// still read, without their tags.
func LocalLinks() (map[string]LinkIdentity, error) {
	return nil, errors.New("inventory: reading VLAN and VXLAN identifiers is supported on Linux only, and this is " + runtime.GOOS)
}
