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

	// defaultConnectTimeout bounds a full Connect. A subsystem that never
	// exports a namespace device (stale controllers that came up "live" but
	// attached zero namespaces) would otherwise make Connect block forever;
	// with the timeout it fails with an explicit error instead. It is a
	// backstop for callers whose context carries no deadline of its own — a
	// caller with a shorter deadline (e.g. kubelet's CSI operation timeout)
	// still wins, since context.WithTimeout takes the earlier deadline.
	defaultConnectTimeout = 2 * time.Minute
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
	subs    nvme.SubsystemResolver
	hostNQN string
	hostID  string
	poll    time.Duration
	timeout time.Duration

	// connect writes an options line to the fabrics device and returns the
	// kernel's reply. A field so tests can stub the device write.
	connect func(ctx context.Context, options string) (string, error)
}

var _ Connector = (*FabricsConnector)(nil)

// NewFabricsConnector returns a connector that reads controller state through
// subs (defaulting to a local sysfs resolver) and defaults the host identity
// from /etc/nvme/hostnqn and /etc/nvme/hostid.
func NewFabricsConnector(subs nvme.SubsystemResolver) *FabricsConnector {
	if subs == nil {
		subs = nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
	}
	return &FabricsConnector{
		subs:    subs,
		hostNQN: readTrim("/etc/nvme/hostnqn"),
		hostID:  readTrim("/etc/nvme/hostid"),
		poll:    defaultPoll,
		timeout: defaultConnectTimeout,
		connect: writeFabricsDevice,
	}
}

// Connect attaches the target subsystem and returns once it has a live
// controller exporting at least one namespace device. It is idempotent: if the
// subsystem is already healthy it does nothing.
//
// A subsystem can be present with a live controller yet export no namespace
// device — stale controllers left by a half-completed or broken connection.
// Reusing that connection never yields a device, so Connect detects the
// condition, tears the stale controllers down, and reconnects fresh so the
// kernel re-enumerates namespaces. The whole operation is bounded by the
// connector's timeout so a subsystem that never produces a device fails with an
// explicit error instead of blocking forever.
func (c *FabricsConnector) Connect(ctx context.Context, t Target) error {
	if t.NQN == "" {
		return fmt.Errorf("connect: empty NQN")
	}

	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	poll := c.poll
	if poll <= 0 {
		poll = defaultPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	disconnectedStale := false
	for {
		s, err := c.subs.ByNQN(ctx, t.NQN)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			// Not connected — establish a fresh connection.
			if _, err := c.connect(ctx, c.options(t)); err != nil {
				return fmt.Errorf("connect %s: %w", t.NQN, err)
			}
		case err != nil:
			return err
		case hasLiveController(s) && len(s.Namespaces) > 0:
			// Live controller exporting a namespace device: usable.
			return nil
		case hasLiveController(s) && !disconnectedStale:
			// Live controller but no namespace device: stale. Tear it down so
			// the next iteration reconnects fresh. Do this at most once; a
			// freshly reconnected subsystem that still exports no device is
			// reported via the timeout below rather than churned repeatedly.
			if err := c.Disconnect(ctx, t.NQN); err != nil {
				return fmt.Errorf("disconnect stale subsystem %s: %w", t.NQN, err)
			}
			disconnectedStale = true
			continue
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to export a namespace device: %w", t.NQN, ctx.Err())
		case <-ticker.C:
		}
	}
}

// hasLiveController reports whether any controller fronting the subsystem is in
// the kernel "live" state.
func hasLiveController(s nvme.Subsystem) bool {
	for _, ctrl := range s.Controllers {
		if ctrl.IsLive() {
			return true
		}
	}
	return false
}

// Disconnect removes every controller fronting nqn by writing its
// delete_controller attribute. It is idempotent: a subsystem that is already
// absent is not an error. For a multi-namespace subsystem this detaches the
// paths shared by every namespace on it.
func (c *FabricsConnector) Disconnect(ctx context.Context, nqn string) error {
	s, err := c.subs.ByNQN(ctx, nqn)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, ctrl := range s.Controllers {
		if ctrl.SysfsPath == "" {
			continue
		}
		if err := writeSysfs(filepath.Join(ctrl.SysfsPath, deleteControllerAttr), "1"); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("disconnect %s (%s): %w", nqn, ctrl.ID, err)
			}
		}
	}
	return firstErr
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
	transport := t.Transport
	if transport == "" {
		transport = TransportTCP
	}
	port := t.Port
	if port == 0 {
		port = defaultTrSvcID
	}

	var b strings.Builder
	fmt.Fprintf(&b, "transport=%s,traddr=%s,trsvcid=%d,nqn=%s", transport, t.Address, port, t.NQN)

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
	return b.String()
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
