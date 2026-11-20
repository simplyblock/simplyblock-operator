// The keys of a storage cluster's metadata server record: the annotations and
// the finalizer on its StatefulSet. They live in one package of no other
// dependencies because three writers use them (the workload's template update,
// the state volume, and the takeover), and a key spelled in each of them would
// let the spellings drift.

package mdsstate

const (
	// ActiveAnnotation names the pod that is the active guest.
	ActiveAnnotation = "storage.simplyblock.io/mds-active"
	// ActiveInstanceAnnotation is the active's pod UID and runner container ID.
	ActiveInstanceAnnotation = "storage.simplyblock.io/mds-active-instance"
	// EpochAnnotation is the current promotion epoch, a decimal integer.
	EpochAnnotation = "storage.simplyblock.io/mds-epoch"
	// StateVolumeAnnotation is the handle of the state volume.
	StateVolumeAnnotation = "storage.simplyblock.io/mds-state-volume"
	// HostOrdinalsAnnotation is how many ordinals' host NQNs the volumes allow,
	// so a scale-down knows which hosts to remove.
	HostOrdinalsAnnotation = "storage.simplyblock.io/mds-host-ordinals"

	// StateVolumeFinalizer deletes the state volume with the StatefulSet.
	StateVolumeFinalizer = "storage.simplyblock.io/mds-state-volume"
)
