package lvol

import "context"

// Endpoint is one NVMe-oF path through which a logical volume is reachable.
// A volume may expose several (multipath, or HA across storage nodes).
type Endpoint struct {
	Transport string // e.g. "tcp"
	Address   string // storage-node host or IP
	Port      int    // service port, typically 4420
}

// Connection is the control-plane's answer to "how do I attach this
// volume over the fabric": the subsystem NQN plus the paths to it.
type Connection struct {
	NQN       string
	Endpoints []Endpoint
}

// Resolver looks up logical volumes and their fabric connection details,
// typically from the simplyblock control plane. It is an interface so
// callers (e.g. the CSI node service) depend on the behavior, not on the
// controlplane client; controlplane.Client implements it.
//
// It is the control-plane counterpart to Mapper: Resolver answers "where
// does this volume live and how do I reach it" from the control plane,
// while Mapper answers "which local NVMe device is it" once attached.
type Resolver interface {
	// Volume returns the identity and metadata of a logical volume.
	Volume(ctx context.Context, h VolumeHandle) (Volume, error)
	// Connection returns how to reach the volume over NVMe-oF.
	Connection(ctx context.Context, h VolumeHandle) (Connection, error)
}
