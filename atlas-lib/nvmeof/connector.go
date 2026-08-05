package nvmeof

import (
	"context"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
)

// Transport is an NVMe-oF transport type.
type Transport string

const TransportTCP Transport = "tcp"

// Target describes a remote NVMe-oF subsystem to connect to.
type Target struct {
	NQN       string
	Transport Transport
	Address   string // host or IP of the storage node (traddr)
	Port      int    // service port (trsvcid), typically 4420

	// Optional host identity. When empty the connector falls back to the
	// node identity in /etc/nvme/hostnqn and /etc/nvme/hostid.
	HostNQN string // hostnqn
	HostID  string // hostid

	// Optional connection tunables. A zero value is omitted from the connect
	// request, deferring to the kernel default; the timeouts are pointers
	// because 0 (fail I/O immediately) is a meaningful value.
	HostIface         string // host_iface — bind to a source interface
	NrIOQueues        int    // nr_io_queues — number of I/O queue pairs
	ReconnectDelaySec int    // reconnect_delay
	KeepAliveTMOSec   int    // keep_alive_tmo
	CtrlLossTMOSec    *int   // ctrl_loss_tmo
	FastIOFailTMOSec  *int   // fast_io_fail_tmo

	// TLS requests an encrypted connection. The kernel needs a pre-shared
	// key for the host/subsystem NQN pair in its keyring; the connect fails
	// if none is installed.
	TLS bool // tls
}

// TargetOption overrides one connect parameter on every target Targets
// builds. Endpoint identity — NQN, transport, address, port — is not
// overridable: that is what identifies the path, and rewriting it would attach
// something other than what the control plane answered.
type TargetOption func(*Target)

// WithHostNQN sets the initiator NQN presented to the target. Unset, the
// connector falls back to /etc/nvme/hostnqn.
func WithHostNQN(nqn string) TargetOption {
	return func(t *Target) { t.HostNQN = nqn }
}

// WithHostID sets the initiator host id. Unset, the connector falls back to
// /etc/nvme/hostid.
func WithHostID(id string) TargetOption {
	return func(t *Target) { t.HostID = id }
}

// WithHostIface binds every path to a source interface (host_iface),
// overriding the interface the control plane suggested.
func WithHostIface(iface string) TargetOption {
	return func(t *Target) { t.HostIface = iface }
}

// WithNrIOQueues sets the number of I/O queue pairs per path
// (nr_io_queues). Zero omits the option, leaving the kernel default.
func WithNrIOQueues(n int) TargetOption {
	return func(t *Target) { t.NrIOQueues = n }
}

// WithReconnectDelaySec sets the delay between reconnect attempts
// (reconnect_delay). Zero omits the option, leaving the kernel default.
func WithReconnectDelaySec(sec int) TargetOption {
	return func(t *Target) { t.ReconnectDelaySec = sec }
}

// WithKeepAliveTMOSec sets the keep-alive timeout (keep_alive_tmo). Zero omits
// the option, leaving the kernel default.
func WithKeepAliveTMOSec(sec int) TargetOption {
	return func(t *Target) { t.KeepAliveTMOSec = sec }
}

// WithCtrlLossTMOSec sets how long the kernel keeps retrying a lost controller
// before giving up (ctrl_loss_tmo). 0 fails I/O immediately, -1 retries
// forever; both are meaningful, which is why this is an explicit option rather
// than a zero value.
func WithCtrlLossTMOSec(sec int) TargetOption {
	return func(t *Target) { t.CtrlLossTMOSec = ptr.To(sec) }
}

// WithFastIOFailTMOSec sets how long I/O waits on a controller that is down
// before failing fast (fast_io_fail_tmo). 0 and -1 ("off") are both
// meaningful.
func WithFastIOFailTMOSec(sec int) TargetOption {
	return func(t *Target) { t.FastIOFailTMOSec = ptr.To(sec) }
}

// WithTLS forces transport encryption on or off, overriding what the control
// plane asked for. Enabling it requires a pre-shared key for the
// host/subsystem NQN pair in the kernel keyring; disabling it when the control
// plane asked for TLS attaches the volume in the clear.
func WithTLS(enabled bool) TargetOption {
	return func(t *Target) { t.TLS = enabled }
}

// Targets turns a control-plane connection into the ordered target list
// ConnectPaths expects: one target per endpoint, in the order the control
// plane returned them, so the primary path stays first. Endpoints are mapped
// one to one — none are dropped, merged or reordered — so what the control
// plane answered is what gets attached, in its order.
//
// Each target's identity (NQN, transport, address, port) and its connect
// tunables come from the connection, since the control plane picks them per
// path. The options then override the tunables and supply what the control
// plane cannot know — this node's host identity, or a local policy such as a
// fixed ctrl_loss_tmo. With no options the control plane's parameters are used
// as they are, and the connector falls back to the node's /etc/nvme identity.
func Targets(conn lvol.Connection, opts ...TargetOption) []Target {
	out := make([]Target, 0, len(conn.Endpoints))
	for _, e := range conn.Endpoints {
		t := Target{
			NQN:               conn.NQN,
			Transport:         Transport(e.Transport),
			Address:           e.Address,
			Port:              e.Port,
			HostIface:         e.HostIface,
			NrIOQueues:        e.NrIOQueues,
			ReconnectDelaySec: e.ReconnectDelaySec,
			KeepAliveTMOSec:   e.KeepAliveTMOSec,
			CtrlLossTMOSec:    e.CtrlLossTMOSec,
			FastIOFailTMOSec:  e.FastIOFailTMOSec,
			TLS:               e.TLS,
		}
		for _, opt := range opts {
			opt(&t)
		}
		out = append(out, t)
	}
	return out
}

// Connector establishes and tears down fabric connections. It is an
// interface so the CSI driver's node service can be tested without a
// kernel or nvme-cli present.
type Connector interface {
	// Connect attaches the target subsystem over a single path, returning
	// once the controller reaches a live state. It must be idempotent.
	Connect(ctx context.Context, t Target) error
	// ConnectPaths attaches the subsystem over several paths in descending
	// priority order (primary first), which it must preserve. It must be
	// idempotent and must attach the paths it can even when a
	// higher-priority one is unavailable.
	ConnectPaths(ctx context.Context, targets []Target) ([]PathResult, error)
	// Disconnect detaches the subsystem identified by nqn, releasing paths
	// that cannot serve I/O before the optimized one. It must be
	// idempotent (no error if already disconnected).
	Disconnect(ctx context.Context, nqn string) error
	// IsConnected reports whether a live controller exists for nqn.
	IsConnected(ctx context.Context, nqn string) (bool, error)
}
