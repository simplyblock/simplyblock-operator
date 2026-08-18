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
	connector
}

var _ Connector = (*FabricsConnector)(nil)

// NewFabricsConnector returns a connector that reads controller state through
// subs (defaulting to a local sysfs resolver) and defaults the host identity
// from /etc/nvme/hostnqn and /etc/nvme/hostid.
func NewFabricsConnector(subs nvme.SubsystemResolver, opts ...Option) *FabricsConnector {
	c := &FabricsConnector{connector: newConnector(subs, opts...)}
	c.attach = func(ctx context.Context, t Target) (string, error) {
		opts, err := fabricsOptions(t, c.hostNQN, c.hostID)
		if err != nil {
			return "", err
		}
		return writeFabricsDevice(ctx, opts)
	}
	c.deleteCtrl = func(ctrl nvme.Controller) error {
		return writeSysfs(filepath.Join(ctrl.SysfsPath, deleteControllerAttr), "1")
	}
	return c
}

// fabricsOptions renders the comma-separated NVMe-oF connect string the kernel
// fabrics device expects. Empty/zero fields are omitted. It errors only on a
// host identity that cannot be made self-consistent (see hostIdentity).
func fabricsOptions(t Target, cHostNQN, cHostID string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "transport=%s,traddr=%s,trsvcid=%d,nqn=%s", transport(t), t.Address, port(t), t.NQN)

	hostNQN, hostID, err := hostIdentity(t, cHostNQN, cHostID)
	if err != nil {
		return "", err
	}
	if hostNQN != "" {
		fmt.Fprintf(&b, ",hostnqn=%s", hostNQN)
	}
	if hostID != "" {
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
	// The kernel takes the secrets in the options line itself, so this string
	// carries credentials from here on: it goes to the fabrics device and
	// nowhere else, and no error renders it.
	if t.DHCHAPSecret != "" {
		fmt.Fprintf(&b, ",dhchap_secret=%s", t.DHCHAPSecret)
	}
	if t.DHCHAPCtrlSecret != "" {
		fmt.Fprintf(&b, ",dhchap_ctrl_secret=%s", t.DHCHAPCtrlSecret)
	}
	return b.String(), nil
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
