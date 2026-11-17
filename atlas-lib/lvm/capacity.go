// How much of a device LVM keeps for itself, for the callers that size the
// devices a volume group is made of: a volume that has to hold a given number
// of bytes through LVM needs devices that much larger.

package lvm

// PhysicalVolumeOverheadBytes is what LVM keeps of each device it makes a
// physical volume of, with the defaults this package creates them under: a
// 1MiB area before the first extent for the label and the metadata, and 4MiB
// extents. A device whose size is a whole number of extents, which a
// control-plane volume of whole GiB is, loses exactly one extent to the two
// together, so a logical volume over n such devices is n extents short of
// their sum.
//
// The on-node suite measures it against the LVM the driver ships
// (test/integration/onnode/capacity_test.go), since it is LVM's defaults and
// not this package that decide it.
const PhysicalVolumeOverheadBytes = int64(4 << 20)
