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

// VDOOverheadBytes is what VDO keeps of its pool for itself, for a volume of
// logicalBytes, estimated generously: a pool this much larger than the volume
// holds the volume's data even when none of it compresses or deduplicates,
// so the pool cannot fill before the volume does.
//
// Most of it does not depend on the volume's size: the deduplication index at
// its default memory setting, the recovery journal, and up to one slab (2GiB by
// default) that a pool not a whole number of slabs cannot use. The rest grows
// with the logical size: the block map, the reference counts, and the slab
// journals, together under 0.2% of it, which logicalBytes/256 covers twice.
// The runner kernel measured 4.02GiB kept of a 12GiB pool
// (test/integration/onnode/capacity_test.go), against which 5GiB is the fixed
// part.
func VDOOverheadBytes(logicalBytes int64) int64 {
	return 5<<30 + logicalBytes/256
}
