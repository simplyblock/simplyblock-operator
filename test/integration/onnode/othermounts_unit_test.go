//go:build linux

// The mount-table reading behind shellFilesystem.OtherMounts, tested on a
// written table: it needs no node, unlike the rest of this suite.

package onnode

import (
	"slices"
	"testing"
)

// A pod's bind mount of the staged filesystem carries the staging mount's
// device number, a mount of another device does not, and an escaped space in a
// mount point is read back as the space it is.
func TestOtherMountsMatchesTheStagedFilesystemsDeviceNumber(t *testing.T) {
	table := "" +
		"25 1 253:0 / / rw - xfs /dev/mapper/root rw\n" +
		"300 25 253:7 / /staging/vol rw - ext4 /dev/mapper/sb-v rw\n" +
		"310 25 253:7 / /pods/a\\040b/mount rw - ext4 /dev/mapper/sb-v rw\n" +
		"320 25 253:8 / /pods/c/mount rw - ext4 /dev/mapper/sb-w rw\n"
	if got := otherMountsIn(table, "/staging/vol"); !slices.Equal(got, []string{"/pods/a b/mount"}) {
		t.Errorf("otherMountsIn = %v, want only the pod's bind mount", got)
	}
	if got := otherMountsIn(table, "/not/mounted"); got != nil {
		t.Errorf("otherMountsIn of an unmounted path = %v, want none", got)
	}
}
