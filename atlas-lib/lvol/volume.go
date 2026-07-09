package lvol

// VolumeHandle is the stable, cluster-wide identifier of a logical volume —
// the value used as the CSI volume_id.
type VolumeHandle string

// Volume is the identity of a simplyblock logical volume, independent of
// where (or whether) it is currently attached to a node.
type Volume struct {
	ID        VolumeHandle
	Name      string
	Pool      string
	SizeBytes uint64
	NQN       string // subsystem NQN this volume is published under
}
