// The on-disk format an export's filesystem is made with.
//
// This image is newer than the hosts it runs on, so mkfs.xfs here defaults
// features an older host kernel cannot mount: the filesystem formats cleanly
// and then fails to mount, naming a feature flag rather than the skew behind
// it. XFS features can only be added, never removed, so there is no repair
// path and prevention is the only option.

package nfsexport

import (
	"github.com/simplyblock/atlas/export"

	csimount "github.com/simplyblock/csi-driver/internal/mount"
)

// exportFormatOptions are the block path's, unchanged.
//
// Pinning a baseline is mkfs.xfs's own job, through the config file that names
// the whole feature set rather than the flags somebody remembered. A list
// written out here was missing parent pointers, which 6.18 defaults on and
// which a host on 5.14 refuses as an unknown incompatible feature, 0x80. One
// pinning for every XFS volume on the node, so the two
// cannot drift and neither is touched when xfsprogs defaults another feature
// on.
func exportFormatOptions() []string {
	return csimount.FormatOptions(export.FSType, nil, false)
}
