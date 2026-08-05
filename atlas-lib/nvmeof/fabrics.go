package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

const (
	// fabricsDevice is the kernel NVMe-oF connect interface: writing a
	// comma-separated options line to it creates a controller and reading
	// back yields "instance=N,cntlid=M".
	fabricsDevice = "/dev/nvme-fabrics"
	// deleteControllerAttr, under a controller's sysfs dir, tears the
	// controller down when "1" is written to it.
	deleteControllerAttr = "delete_controller"

	defaultTrSvcID = 4420
	defaultPoll    = 100 * time.Millisecond
	// defaultPathTimeout bounds the wait for one path of a multipath connect
	// to reach a live state. It is per path, not per connect: a storage node
	// that is restarting must not consume the caller's whole context before
	// the remaining paths get their turn.
	defaultPathTimeout = 10 * time.Second
)

// FabricsConnector establishes and tears down NVMe-oF connections by talking
// to the kernel directly: it writes a connect options line to
// /dev/nvme-fabrics and removes a controller through its delete_controller
// sysfs attribute. It requires no nvme-cli binary. Controller state is read
// back through a nvme.SubsystemResolver (for IsConnected, for waiting until a
// fresh controller is live, and to locate the controllers to disconnect).
//
// It is Linux-only in practice — the fabrics device and sysfs attributes
// exist only there — and surfaces the underlying file error elsewhere.
type FabricsConnector struct {
	subs        nvme.SubsystemResolver
	hostNQN     string
	hostID      string
	poll        time.Duration
	pathTimeout time.Duration

	// connect writes an options line to the fabrics device and returns the
	// kernel's reply. A field so tests can stub the device write.
	connect func(ctx context.Context, options string) (string, error)
	// deleteCtrl tears down the controller at a sysfs path. A field so tests
	// can observe teardown order; nil uses the delete_controller attribute.
	deleteCtrl func(sysfsPath string) error
}

var _ Connector = (*FabricsConnector)(nil)

// Option configures a FabricsConnector.
type Option func(*FabricsConnector)

// WithPathTimeout bounds how long a single path of a multipath connect may
// take to reach a live state before ConnectPaths records it as failed and
// moves on to the next path in priority order. Zero or less means no per-path
// bound — a path may then wait out the caller's whole context.
func WithPathTimeout(d time.Duration) Option {
	return func(c *FabricsConnector) { c.pathTimeout = d }
}

// WithPollInterval sets how often controller state is re-read while waiting
// for a connect to go live.
func WithPollInterval(d time.Duration) Option {
	return func(c *FabricsConnector) { c.poll = d }
}

// NewFabricsConnector returns a connector that reads controller state through
// subs (defaulting to a local sysfs resolver) and defaults the host identity
// from /etc/nvme/hostnqn and /etc/nvme/hostid.
func NewFabricsConnector(subs nvme.SubsystemResolver, opts ...Option) *FabricsConnector {
	if subs == nil {
		subs = nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
	}
	c := &FabricsConnector{
		subs:        subs,
		hostNQN:     readTrim("/etc/nvme/hostnqn"),
		hostID:      readTrim("/etc/nvme/hostid"),
		poll:        defaultPoll,
		pathTimeout: defaultPathTimeout,
		connect:     writeFabricsDevice,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect attaches the target subsystem over the single path t describes and
// returns once that path's controller is live. It is idempotent per path: a
// controller already fronting this endpoint is left alone (another controller
// for the same subsystem at a different address is not, since that is a
// different path). Multipath volumes go through ConnectPaths, which keeps the
// control plane's path priority.
func (c *FabricsConnector) Connect(ctx context.Context, t Target) error {
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
func (c *FabricsConnector) Disconnect(ctx context.Context, nqn string) error {
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
		if err := c.deleteController(ctrl.SysfsPath); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("disconnect %s (%s): %w", nqn, ctrl.ID, err)
			}
		}
	}
	return firstErr
}

// deleteController tears down one controller through its delete_controller
// sysfs attribute.
func (c *FabricsConnector) deleteController(sysfsPath string) error {
	if c.deleteCtrl != nil {
		return c.deleteCtrl(sysfsPath)
	}
	return writeSysfs(filepath.Join(sysfsPath, deleteControllerAttr), "1")
}

// IsConnected reports whether a live controller exists for nqn.
func (c *FabricsConnector) IsConnected(ctx context.Context, nqn string) (bool, error) {
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

// options renders the comma-separated NVMe-oF connect string the kernel
// fabrics device expects. Empty/zero fields are omitted.
func (c *FabricsConnector) options(t Target) string {
	var b strings.Builder
	fmt.Fprintf(&b, "transport=%s,traddr=%s,trsvcid=%d,nqn=%s", transport(t), t.Address, port(t), t.NQN)

	if hostNQN := orElse(t.HostNQN, c.hostNQN); hostNQN != "" {
		fmt.Fprintf(&b, ",hostnqn=%s", hostNQN)
	}
	if hostID := orElse(t.HostID, c.hostID); hostID != "" {
		fmt.Fprintf(&b, ",hostid=%s", hostID)
	}
	if t.HostIface != "" {
		fmt.Fprintf(&b, ",host_iface=%s", t.HostIface)
	}
	if t.NrIOQueues > 0 {
		fmt.Fprintf(&b, ",nr_io_queues=%d", t.NrIOQueues)
	}
	if t.ReconnectDelaySec > 0 {
		fmt.Fprintf(&b, ",reconnect_delay=%d", t.ReconnectDelaySec)
	}
	if t.KeepAliveTMOSec > 0 {
		fmt.Fprintf(&b, ",keep_alive_tmo=%d", t.KeepAliveTMOSec)
	}
	if t.CtrlLossTMOSec != nil {
		fmt.Fprintf(&b, ",ctrl_loss_tmo=%d", *t.CtrlLossTMOSec)
	}
	if t.FastIOFailTMOSec != nil {
		fmt.Fprintf(&b, ",fast_io_fail_tmo=%d", *t.FastIOFailTMOSec)
	}
	// tls is a bare boolean token, like nvme-cli's --tls.
	if t.TLS {
		b.WriteString(",tls")
	}
	return b.String()
}

// pollInterval is how often controller state is re-read while waiting.
func (c *FabricsConnector) pollInterval() time.Duration {
	if c.poll <= 0 {
		return defaultPoll
	}
	return c.poll
}

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

// writeFabricsDevice opens /dev/nvme-fabrics, writes the connect options, and
// returns the kernel's "instance=N,cntlid=M" reply. A rejected connect (bad
// options, unreachable or duplicate target) surfaces as the write error.
func writeFabricsDevice(_ context.Context, options string) (string, error) {
	f, err := os.OpenFile(fabricsDevice, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(options); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// writeSysfs writes val to an existing sysfs attribute (no create, no
// truncate — the canonical way to poke a kernel attribute).
func writeSysfs(path, val string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(val)
	return err
}

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
