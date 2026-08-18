package lvol

import "context"

// Endpoint is one NVMe-oF path through which a logical volume is reachable,
// with the connect parameters the control plane chose for it. A volume may
// expose several endpoints (multipath, or HA across storage nodes).
//
// The control plane also returns a prebuilt "connect" command line and the
// subsystem's allowed-hosts list. Neither is carried here as such — the
// connector renders its own connect options, and host authorization is not the
// initiator's business — with one exception: the DHCHAP secrets below, which
// the control plane publishes nowhere else (see controlplane.Client.Connection).
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

	// DHCHAPSecret and DHCHAPCtrlSecret are the DHCHAP key material the target
	// expects for the host this connection was resolved for — the host named by
	// ForHost, since the secret is per (host, subsystem) and there is no such
	// thing as "the" secret for a volume. Empty when the pool is not
	// DHCHAP-gated, or when no host was named.
	//
	// DHCHAPSecret authenticates the host to the target; DHCHAPCtrlSecret
	// authenticates the target back to the host (bidirectional DHCHAP), and is
	// empty when the control plane asked only for one-way authentication.
	//
	// These are credentials. They must not be logged, and nothing in this
	// module renders them into an error message.
	DHCHAPSecret     string
	DHCHAPCtrlSecret string
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

// ConnectionParams are the inputs a Resolver may take into account when
// answering Connection. They are set through ConnectionOption rather than
// passed positionally, since every one of them is optional and a resolver that
// does not authorize hosts may ignore them all.
type ConnectionParams struct {
	// HostNQN is the initiator NQN the connection is being resolved for.
	HostNQN string
}

// ConnectionOption narrows who a Connection is being resolved for.
type ConnectionOption func(*ConnectionParams)

// ForHost names the initiator NQN the connection is resolved for.
//
// It matters for two reasons on an access-controlled pool, and for neither on
// an open one. First, authorization: the control plane answers only for a host
// on the subsystem's allowed-hosts list, so an unnamed host is either refused
// or answered for whichever identity the control plane assumes — never for
// this one. Second, credentials: the DHCHAP secret is per (host, subsystem),
// so the returned Endpoint.DHCHAPSecret is only the right one when the host it
// belongs to was named here.
//
// The NQN must be the same one the connect then presents (nvmeof.WithHostNQN);
// a connection resolved for one identity and attached under another
// authenticates with the wrong secret, or is refused outright.
func ForHost(nqn string) ConnectionOption {
	return func(p *ConnectionParams) {
		p.HostNQN = nqn
	}
}

// ConnectionOptions folds opts into a ConnectionParams. It is for Resolver
// implementations, so each does not re-implement the fold.
func ConnectionOptions(opts ...ConnectionOption) ConnectionParams {
	var p ConnectionParams
	for _, opt := range opts {
		opt(&p)
	}
	return p
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
	Connection(ctx context.Context, h VolumeHandle, opts ...ConnectionOption) (Connection, error)
}
