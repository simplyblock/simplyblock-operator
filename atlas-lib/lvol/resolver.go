package lvol

import "context"

// Endpoint is one NVMe-oF path through which a logical volume is reachable,
// with the connect parameters the control plane chose for it. A volume may
// expose several endpoints (multipath, or HA across storage nodes).
//
// The control plane also returns a prebuilt "connect" command line and the
// subsystem's allowed-hosts list; neither is carried here — the connector
// renders its own connect options, and host authorization is not the
// initiator's business.
type Endpoint struct {
	Transport string // e.g. "tcp"
	Address   string // storage-node host or IP
	Port      int    // service port, typically 4420

	// Per-path connect tunables as the control plane chose them. The
	// timeouts are pointers because 0 is a meaningful value (fail I/O
	// immediately) distinct from "not specified".
	NrIOQueues        int  // number of I/O queue pairs
	ReconnectDelaySec int  // delay between reconnect attempts
	KeepAliveTMOSec   int  // keep-alive timeout
	CtrlLossTMOSec    *int // give up on a lost controller after this long
	FastIOFailTMOSec  *int // fail fast on I/O while a controller is down

	HostIface string // bind the path to this source interface
	// TLS reports that the control plane expects this path to be encrypted.
	// Establishing it needs a pre-shared key in the host keyring.
	TLS bool
}

// Connection is the control-plane's answer to "how do I attach this
// volume over the fabric": the subsystem NQN plus the paths to it.
type Connection struct {
	NQN string
	// NSID is the namespace id the volume occupies within the subsystem, for
	// subsystems that export several. Zero when the control plane does not
	// report one.
	NSID uint32
	// Endpoints are in the control plane's priority order — primary,
	// secondary, tertiary — and that order is preserved as received.
	// nvmeof.ConnectPaths relies on it to attach the primary path first.
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
