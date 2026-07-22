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
		connect: writeFabricsDevice,
	}
}

// Connect attaches the target subsystem and returns once a controller for it
// is live. It is idempotent: if a live controller already exists it does
// nothing.
func (c *FabricsConnector) Connect(ctx context.Context, t Target) error {
	if t.NQN == "" {
		return fmt.Errorf("connect: empty NQN")
	}
	if ok, err := c.IsConnected(ctx, t.NQN); err != nil {
		return err
	} else if ok {
		return nil
	}
	if _, err := c.connect(ctx, c.options(t)); err != nil {
		return fmt.Errorf("connect %s: %w", t.NQN, err)
	}
	return c.waitLive(ctx, t.NQN)
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

// waitLive polls until a controller for nqn is live or ctx is done.
func (c *FabricsConnector) waitLive(ctx context.Context, nqn string) error {
	poll := c.poll
	if poll <= 0 {
		poll = defaultPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		ok, err := c.IsConnected(ctx, nqn)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to become live: %w", nqn, ctx.Err())
		case <-ticker.C:
		}
	}
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
