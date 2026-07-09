package nvmeof

import "context"

// Transport is an NVMe-oF transport type.
type Transport string

const TransportTCP Transport = "tcp"

// Target describes a remote NVMe-oF subsystem to connect to.
type Target struct {
	NQN       string
	Transport Transport
	Address   string // host or IP of the storage node
	Port      int    // service port, typically 4420
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
