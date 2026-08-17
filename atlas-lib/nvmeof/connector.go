package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
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
	// DisconnectController detaches a single controller — one path of a
	// multipath subsystem — leaving the subsystem's other paths, and every
	// namespace they serve, in place. It is what repairing one broken path
	// needs: Disconnect would take the working paths down with it, and on a
	// shared subsystem every co-tenant volume too.
	//
	// It must be idempotent: a controller that is already gone is not an
	// error, which matters because ctrl is a snapshot and the kernel may
	// have reaped it in the meantime.
	DisconnectController(ctx context.Context, ctrl nvme.Controller) error
	// IsConnected reports whether a live controller exists for nqn.
	IsConnected(ctx context.Context, nqn string) (bool, error)
}

// connector is everything about attaching that does not depend on how the
// attaching is done: priority order, per-path timeouts, waiting for a controller
// to reach live, leaving an existing path alone, ANA teardown order on release.
//
// Only two operations are mechanism-specific, and they are the two fields below.
// Both FabricsConnector and CLIConnector are this machinery with those two
// filled in — neither is built out of the other, because neither is a special
// case of the other; they are two ways of issuing the same two operations.
type connector struct {
	subs        nvme.SubsystemResolver
	hostNQN     string
	hostID      string
	poll        time.Duration
	pathTimeout time.Duration

	// attach establishes one path and returns whatever the mechanism reports.
	// It takes the Target rather than a rendered options line because not every
	// mechanism speaks the fabrics-device format — nvme-cli wants flags — and a
	// backend forced to parse our own rendering back into fields would be the
	// long way round.
	attach func(ctx context.Context, t Target) (string, error)
	// deleteCtrl tears one controller down: nvme-cli addresses a controller by
	// name where sysfs addresses it by path.
	deleteCtrl func(ctrl nvme.Controller) error
}

// Option configures a connector. Both constructors take the same options: they
// concern path handling, which is the half the two share.
type Option func(*connector)

// WithPathTimeout bounds how long a single path of a multipath connect may
// take to reach a live state before ConnectPaths records it as failed and
// moves on to the next path in priority order. Zero or less means no per-path
// bound — a path may then wait out the caller's whole context.
func WithPathTimeout(d time.Duration) Option {
	return func(c *connector) { c.pathTimeout = d }
}

// WithPollInterval sets how often controller state is re-read while waiting
// for a connect to go live.
func WithPollInterval(d time.Duration) Option {
	return func(c *connector) { c.poll = d }
}

// newConnector builds the shared half: the resolver, the host identity and the
// path-handling knobs. The mechanism-specific primitives are the caller's to
// supply, and every method here assumes they are set.
func newConnector(subs nvme.SubsystemResolver, opts ...Option) connector {
	if subs == nil {
		subs = nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
	}
	c := connector{
		subs:        subs,
		hostNQN:     readTrim("/etc/nvme/hostnqn"),
		hostID:      readTrim("/etc/nvme/hostid"),
		poll:        defaultPoll,
		pathTimeout: defaultPathTimeout,
	}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// Connect attaches the target subsystem over the single path t describes and
// returns once that path's controller is live. It is idempotent per path: a
// controller already fronting this endpoint is left alone (another controller
// for the same subsystem at a different address is not, since that is a
// different path). Multipath volumes go through ConnectPaths, which keeps the
// control plane's path priority.
func (c *connector) Connect(ctx context.Context, t Target) error {
	results, err := c.ConnectPaths(ctx, []Target{t})
	if err != nil {
		return err
	}
	return results[0].Err
}

// Disconnect removes every controller fronting nqn by writing its
// delete_controller attribute. It is idempotent: a subsystem that is already
// absent is not an error. For a multi-namespace subsystem this detaches the
// paths shared by every namespace on it.
//
// Controllers are released in ANA order — inaccessible and non-optimized
// paths first, the optimized path last (see disconnectOrder) — so I/O still
// in flight keeps the best path available until the end. The writes are
// sequential and each one tears its controller down before the next is
// issued, so the order is the order the kernel sees.
func (c *connector) Disconnect(ctx context.Context, nqn string) error {
	s, err := c.subs.ByNQN(ctx, nqn)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, ctrl := range disconnectOrder(s) {
		if ctrl.SysfsPath == "" {
			continue
		}
		// A path that fails to release does not stop the ones behind it: the
		// optimized path must still be torn down last, not left behind.
		if err := c.deleteController(ctrl); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("disconnect %s (%s): %w", nqn, ctrl.ID, err)
			}
		}
	}
	return firstErr
}

// DisconnectController tears down the single controller ctrl describes, leaving
// the subsystem's other paths — and every namespace they serve — attached.
//
// It is the narrow counterpart to Disconnect, and the difference is not
// cosmetic: on a subsystem shared by several volumes, Disconnect takes every
// co-tenant's block device with it, so repairing one broken path has to be able
// to say "this controller, and nothing else".
//
// A controller the kernel has already reaped is not an error: ctrl is a snapshot
// and the race is expected, so a vanished sysfs path counts as done.
func (c *connector) DisconnectController(_ context.Context, ctrl nvme.Controller) error {
	if ctrl.SysfsPath == "" {
		return fmt.Errorf("disconnect controller %s: no sysfs path: %w", ctrl.ID, errs.ErrUnsupported)
	}
	if err := c.deleteController(ctrl); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("disconnect controller %s: %w", ctrl.ID, err)
	}
	return nil
}

// deleteController tears down one controller through whichever mechanism this
// connector was built with.
func (c *connector) deleteController(ctrl nvme.Controller) error {
	return c.deleteCtrl(ctrl)
}

// IsConnected reports whether a live controller exists for nqn.
func (c *connector) IsConnected(ctx context.Context, nqn string) (bool, error) {
	s, err := c.subs.ByNQN(ctx, nqn)
	if errors.Is(err, errs.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, ctrl := range s.Controllers {
		if ctrl.IsLive() {
			return true, nil
		}
	}
	return false, nil
}

// pollInterval is how often controller state is re-read while waiting.
func (c *connector) pollInterval() time.Duration {
	if c.poll <= 0 {
		return defaultPoll
	}

	return c.poll
}

// Reading a Target is the same question whichever mechanism answers it, so
// these live with Target rather than with either connector.

// transport is t's transport, defaulting to TCP.
func transport(t Target) Transport {
	if t.Transport == "" {
		return TransportTCP
	}
	return t.Transport
}

// port is t's service port (trsvcid), defaulting to the NVMe-oF standard one.
func port(t Target) int {
	if t.Port == 0 {
		return defaultTrSvcID
	}
	return t.Port
}

// endpoint renders t's fabric endpoint for error messages.
func endpoint(t Target) string {
	return fmt.Sprintf("%s:%d", t.Address, port(t))
}

// readTrim reads a one-line file, e.g. /etc/nvme/hostnqn, and returns "" when
// it is absent — an unset host identity is a fallback, not a failure.
func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func orElse(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
