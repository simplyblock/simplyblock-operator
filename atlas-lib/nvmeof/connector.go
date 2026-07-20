package nvmeof

import "context"

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
	// request, deferring to the kernel default; CtrlLossTMOSec is a pointer
	// because 0 (fail I/O immediately on loss) is a meaningful value.
	HostIface         string // host_iface — bind to a source interface
	NrIOQueues        int    // nr_io_queues
	ReconnectDelaySec int    // reconnect_delay
	KeepAliveTMOSec   int    // keep_alive_tmo
	CtrlLossTMOSec    *int   // ctrl_loss_tmo
}

// Connector establishes and tears down fabric connections. It is an
// interface so the CSI driver's node service can be tested without a
// kernel or nvme-cli present.
type Connector interface {
	// Connect attaches the target subsystem, returning once the
	// controller reaches a live state. It must be idempotent.
	Connect(ctx context.Context, t Target) error
	// Disconnect detaches the subsystem identified by nqn. It must be
	// idempotent (no error if already disconnected).
	Disconnect(ctx context.Context, nqn string) error
	// IsConnected reports whether a live controller exists for nqn.
	IsConnected(ctx context.Context, nqn string) (bool, error)
}
