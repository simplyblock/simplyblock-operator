package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A node's virtual machine is reachable through the QEMU monitor talosctl opens
// for it: `-monitor unix:<state>/<cluster>/<node>.monitor,server,nowait`. That is
// the only way to fault a host from outside the guest, which is the only way that
// works — a host outage takes the pod a test would otherwise drive it from.
//
// It is the human monitor (HMP), not QMP: a line protocol that greets with a
// banner and prompts with "(qemu)".
//
// The distinction this makes available is between a host that is gone and a host
// that has stopped answering. Deleting an nvmet subsystem is a deliberate
// teardown and the initiator sees the connection close; a frozen virtual machine
// answers nothing at all, and the initiator finds out when keep-alive expires.
// Only the second is what a failing host does.

const (
	// monitorPrompt is what HMP writes when it is ready for a command.
	monitorPrompt = "(qemu)"

	// monitorTimeout bounds one command. The monitor answers immediately or not
	// at all: a command that has not been acknowledged in this long means the
	// monitor is wedged, and a test blocking forever on it is worse than a
	// failure.
	monitorTimeout = 10 * time.Second

	// defaultNetdev is the id talosctl gives a node's network device.
	defaultNetdev = "net0"
)

// Monitor is a connection to one node's QEMU monitor.
//
// QEMU's socket chardev serves one client at a time, so hold it only as long as
// needed and close it. The prepared faults on *Cluster do that themselves.
type Monitor struct {
	conn net.Conn
	node string
	buf  bytes.Buffer
}

// MonitorPath is where talosctl's QEMU provisioner puts a node's monitor socket.
// Node names in Kubernetes are the names the provisioner used, so a node name is
// all this needs.
func (c *Cluster) MonitorPath(node string) string {
	return filepath.Join(stateDir(), c.cfg.Name, node+".monitor")
}

// Monitor connects to a node's QEMU monitor. The caller closes it.
func (c *Cluster) Monitor(ctx context.Context, node string) (*Monitor, error) {
	path := c.MonitorPath(node)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no monitor socket for %s at %s: %w", node, path, err)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		// Connecting to a unix socket needs write permission on it, and QEMU
		// created this one as root. Create hands the sockets over; a cluster
		// someone else started will not have been.
		return nil, fmt.Errorf("dial monitor for %s at %s: %w "+
			"(is the socket owned by this user? see Cluster.reclaimMonitors)", node, path, err)
	}

	m := &Monitor{conn: conn, node: node}
	// The banner ends at the first prompt. Reading it now means a command's
	// response is its own.
	if _, err := m.read(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read monitor banner for %s: %w", node, err)
	}
	return m, nil
}

// Close releases the monitor.
func (m *Monitor) Close() error {
	if m.conn == nil {
		return nil
	}
	return m.conn.Close()
}

// Command runs one monitor command and returns what it printed, without the
// prompt.
//
// HMP reports a bad command by printing an error and prompting again, so an
// unknown command is not a transport failure; the text is returned and checked.
func (m *Monitor) Command(ctx context.Context, cmd string) (string, error) {
	deadline := time.Now().Add(monitorTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := m.conn.SetDeadline(deadline); err != nil {
		return "", err
	}

	if _, err := m.conn.Write([]byte(cmd + "\n")); err != nil {
		return "", fmt.Errorf("write %q to %s's monitor: %w", cmd, m.node, err)
	}
	out, err := m.read(ctx)
	if err != nil {
		return out, fmt.Errorf("read reply to %q from %s's monitor: %w", cmd, m.node, err)
	}

	// The monitor echoes nothing, but it does repeat the command on some builds
	// when the chardev is in line mode; drop it so callers see only output.
	out = strings.TrimPrefix(strings.TrimSpace(out), cmd)
	if strings.Contains(out, "unknown command") || strings.Contains(out, "Error:") {
		return out, fmt.Errorf("monitor rejected %q on %s: %s", cmd, m.node, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// read consumes bytes up to and including the next prompt.
func (m *Monitor) read(ctx context.Context) (string, error) {
	chunk := make([]byte, 4096)
	for {
		if s := m.buf.String(); strings.Contains(s, monitorPrompt) {
			i := strings.LastIndex(s, monitorPrompt)
			m.buf.Reset()
			m.buf.WriteString(s[i+len(monitorPrompt):])
			return s[:i], nil
		}
		if err := ctx.Err(); err != nil {
			return m.buf.String(), err
		}
		n, err := m.conn.Read(chunk)
		m.buf.Write(chunk[:n])
		if err != nil {
			// The prompt may have arrived in the same read as the error.
			if s := m.buf.String(); strings.Contains(s, monitorPrompt) {
				continue
			}
			return m.buf.String(), err
		}
	}
}

// Freeze stops the node's virtual CPUs.
//
// The host stops answering without closing anything: no FIN and no RST, so an
// initiator connected to it learns nothing until keep-alive expires. This is what
// a wedged host looks like, and no amount of configfs on a live host reproduces
// it. Thaw resumes.
func (c *Cluster) Freeze(ctx context.Context, node string) error {
	return c.monitorCommand(ctx, node, "stop")
}

// Thaw resumes a frozen node.
func (c *Cluster) Thaw(ctx context.Context, node string) error {
	return c.monitorCommand(ctx, node, "cont")
}

// LinkDown detaches the node's virtual network cable, which is a partition rather
// than an outage: the host keeps running and keeps its state, and its peers see
// their connections fail. Distinguishable from Freeze, where the host is not
// running either.
func (c *Cluster) LinkDown(ctx context.Context, node string) error {
	return c.monitorCommand(ctx, node, "set_link "+defaultNetdev+" off")
}

// LinkUp reattaches it.
func (c *Cluster) LinkUp(ctx context.Context, node string) error {
	return c.monitorCommand(ctx, node, "set_link "+defaultNetdev+" on")
}

// Shutdown asks the guest to power off, as an ACPI request it can act on. The
// node leaves the cluster the way a planned reboot does.
func (c *Cluster) Shutdown(ctx context.Context, node string) error {
	return c.monitorCommand(ctx, node, "system_powerdown")
}

// PowerOff cuts the power: the virtual machine exits without telling the guest.
// Nothing is flushed, which is the point.
//
// There is no matching power-on. talosctl's provisioner owns the QEMU processes
// and has no way to restart one, so a node powered off this way is gone for the
// rest of the cluster's life.
func (c *Cluster) PowerOff(ctx context.Context, node string) error {
	// `quit` makes QEMU exit, so the monitor connection dies with it and the
	// reply never arrives. That is success, not a transport failure.
	err := c.monitorCommand(ctx, node, "quit")
	if err == nil || isClosed(err) {
		return nil
	}
	return err
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) ||
		strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "connection reset")
}

// monitorCommand opens a monitor, runs one command and closes it, because QEMU's
// socket chardev serves one client at a time.
func (c *Cluster) monitorCommand(ctx context.Context, node, cmd string) error {
	m, err := c.Monitor(ctx, node)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	_, err = m.Command(ctx, cmd)
	return err
}

// reclaimMonitors hands the monitor sockets to the current user.
//
// QEMU created them as root, and connecting to a unix socket needs write
// permission on it, so without this the monitors are reachable only by another
// root process. Ownership rather than mode: the sockets live under the user's own
// home directory and widening them to everyone on the machine buys nothing.
func (c *Cluster) reclaimMonitors(ctx context.Context) error {
	if !*c.cfg.Sudo {
		return nil
	}
	dir := filepath.Join(stateDir(), c.cfg.Name)
	sockets, err := filepath.Glob(filepath.Join(dir, "*.monitor"))
	if err != nil || len(sockets) == 0 {
		// A provisioner that stops opening monitors is a change worth noticing,
		// but not one that should fail a cluster nothing has asked to fault yet.
		return nil //nolint:nilerr // best effort by design
	}

	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	args := append([]string{"chown", owner}, sockets...)
	cmd := sudoCommand(ctx, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("take ownership of %s's monitor sockets: %w\n%s",
			c.cfg.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
