package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// CLIConnector attaches and detaches through the nvme-cli binary instead of
// talking to the kernel directly.
//
// It exists for callers that already drive the fabric with nvme-cli and cannot
// switch to sysfs writes in one step, such as the simplyblock CSI node plugin, whose
// connect path is nvme-cli today. Sharing this type is what lets such a caller
// move onto atlas's path handling incrementally rather than keeping a second
// implementation of it.
//
// Only two operations are nvme-cli's: establishing a path, and tearing a
// controller down. Everything that decides *what* happens (priority order on
// attach, per-path timeouts, waiting for a controller to reach live, skipping a
// path that already exists, and ANA teardown order on release) comes from the
// shared connector, the same machinery FabricsConnector runs on. Those are the
// parts with the subtle behavior, and a second copy of them would be the
// duplication this type exists to avoid.
//
// It is Linux-only in practice and needs nvme-cli on PATH and the privileges to
// run it.
type CLIConnector struct {
	connector

	// run executes nvme-cli. A field so tests can observe the command line
	// without a binary present.
	run CommandRunner
}

// CommandRunner executes nvme-cli with args and returns its combined output,
// which is where nvme-cli puts the reason for a failure. The error is the
// process's, so a non-zero exit must come back as one: connect reads both, and
// tells "already connected" from a real failure by the output alone.
//
// It exists so a caller can wrap the invocation without reimplementing any of
// the path handling around it. The case it was added for is privilege: an image
// that runs nvme-cli as a non-root user, as a Red Hat certified image must,
// reaches the fabric through a setuid helper, and which helper that is, is the
// image's business rather than this package's.
type CommandRunner func(ctx context.Context, args ...string) ([]byte, error)

var _ Connector = (*CLIConnector)(nil)

// cliTimeout is a backstop on a single nvme-cli invocation, not the normal
// bound. Ordinarily connectPath's per-path deadline is much tighter and wins,
// since context.WithTimeout keeps the earlier of the two. This exists for the
// case where there is no tighter one: WithPathTimeout(0) and a caller context
// carrying no deadline of its own. Without it, nvme-cli against an unreachable
// target would block for as long as it likes, which is what the per-path budget
// exists to prevent.
const cliTimeout = 40 * time.Second

// NewCLIConnector returns a connector that drives nvme-cli, reading controller
// state through subs (defaulting to a local sysfs resolver) and defaulting the
// host identity from /etc/nvme/hostnqn and /etc/nvme/hostid.
//
// The options are FabricsConnector's, and mean the same here.
func NewCLIConnector(subs nvme.SubsystemResolver, opts ...Option) *CLIConnector {
	return NewCLIConnectorWithRunner(subs, runCommand, opts...)
}

// NewCLIConnectorWithRunner is NewCLIConnector with the nvme-cli invocation
// supplied by the caller, for an image that cannot exec the binary directly.
// See CommandRunner. A nil runner means the plain one NewCLIConnector uses.
func NewCLIConnectorWithRunner(
	subs nvme.SubsystemResolver,
	run CommandRunner,
	opts ...Option,
) *CLIConnector {
	if run == nil {
		run = runCommand
	}
	c := &CLIConnector{connector: newConnector(subs, opts...), run: run}
	c.attach = c.connect
	c.deleteCtrl = c.disconnectController
	return c
}

// SudoRunner runs nvme-cli through sudo, for an image whose process is not root
// and reaches the fabric through a sudoers rule for the `nvme` binary.
//
// The privilege is needed even in a privileged container: the capabilities a
// privileged container grants are dropped when the entrypoint runs as an
// unprivileged uid, so writing /dev/nvme-fabrics fails on permissions with
// nothing in the container's configuration to suggest why.
func SudoRunner(ctx context.Context, args ...string) ([]byte, error) {
	//nolint:gosec // fixed binaries, structured args
	return exec.CommandContext(ctx, "sudo", append([]string{"nvme"}, args...)...).CombinedOutput()
}

// connect establishes one path with `nvme connect`.
//
// "already connected" is treated as success, and that needs saying because the
// same message is at the heart of the bug this package exists for. At the
// controller level it is the truth: a controller for this endpoint exists, which
// is exactly what establishing the path was for, so failing here would turn a
// satisfied request into an error. What it does *not* mean is that the path
// works, since the controller can be live and still serve no namespace, and
// that is a different question, asked by Inspect and answered by a repair. Conflating
// the two is what made the original incident unreadable: the connect looked
// fine, so nothing looked wrong.
func (c *CLIConnector) connect(ctx context.Context, t Target) (string, error) {
	// The caller's deadline still wins when it is the earlier one. This only
	// ensures there is one at all.
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	args, err := c.connectArgs(t)
	if err != nil {
		return "", fmt.Errorf("nvme connect %s via %s: %w", t.NQN, endpoint(t), err)
	}
	out, err := c.run(ctx, args...)
	if err == nil {
		return string(out), nil
	}
	if isAlreadyConnected(out, err) {
		return string(out), nil
	}
	return "", fmt.Errorf("nvme connect %s via %s: %w: %s",
		t.NQN, endpoint(t), err, strings.TrimSpace(string(out)))
}

// connectArgs renders the flags for one target. It mirrors fabricsOptions field
// for field, so the two mechanisms ask the kernel for the same thing. A value
// the fabrics-device line omits is omitted here too, leaving the kernel
// default.
func (c *CLIConnector) connectArgs(t Target) ([]string, error) {
	args := []string{
		"connect",
		"-t", string(transport(t)),
		"-a", t.Address,
		"-s", strconv.Itoa(port(t)),
		"-n", t.NQN,
	}
	hostNQN, hostID, err := hostIdentity(t, c.hostNQN, c.hostID)
	if err != nil {
		return nil, err
	}
	if hostNQN != "" {
		args = append(args, "--hostnqn", hostNQN)
	}
	if hostID != "" {
		args = append(args, "--hostid", hostID)
	}
	if t.HostIface != "" {
		args = append(args, "--host-iface", t.HostIface)
	}
	if t.NrIOQueues > 0 {
		args = append(args, "--nr-io-queues", strconv.Itoa(t.NrIOQueues))
	}
	if t.ReconnectDelaySec > 0 {
		args = append(args, "--reconnect-delay", strconv.Itoa(t.ReconnectDelaySec))
	}
	if t.KeepAliveTMOSec > 0 {
		args = append(args, "--keep-alive-tmo", strconv.Itoa(t.KeepAliveTMOSec))
	}
	if t.CtrlLossTMOSec != nil {
		args = append(args, "--ctrl-loss-tmo", strconv.Itoa(*t.CtrlLossTMOSec))
	}
	if t.FastIOFailTMOSec != nil {
		args = append(args, "--fast_io_fail_tmo", strconv.Itoa(*t.FastIOFailTMOSec))
	}
	if t.TLS {
		args = append(args, "--tls")
	}
	// nvme-cli reads a secret straight from the argument, so it stands in this
	// command line, which is why nothing here logs one.
	if t.DHCHAPSecret != "" {
		args = append(args, "--dhchap-secret", t.DHCHAPSecret)
	}
	if t.DHCHAPCtrlSecret != "" {
		args = append(args, "--dhchap-ctrl-secret", t.DHCHAPCtrlSecret)
	}
	return args, nil
}

// disconnectController releases one controller with `nvme disconnect -d`, which
// addresses it by kernel name, the last path component of its sysfs directory.
//
// `nvme disconnect -n <nqn>` is the wrong tool here and worth naming: it takes
// the whole subsystem, and on a subsystem shared by several volumes that is
// every co-tenant's block device, not just this path.
//
// A controller already gone is not an error: the caller holds a snapshot and the
// kernel may reap the controller before this runs. The check is on the sysfs
// entry rather than on nvme-cli's exit code so the guarantee is ours, not a
// property of whichever nvme-cli is installed.
func (c *CLIConnector) disconnectController(ctrl nvme.Controller) error {
	name := controllerName(ctrl)
	if name == "" {
		return fmt.Errorf("disconnect controller: no name or sysfs path: %w", errs.ErrUnsupported)
	}
	if ctrl.SysfsPath != "" {
		if _, err := os.Stat(ctrl.SysfsPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	// The teardown has no caller context to inherit: deleteCtrl is reached from
	// Disconnect's ordered walk, which must finish releasing the paths behind
	// this one even when the caller has given up.
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	out, err := c.run(ctx, "disconnect", "-d", name)
	if err != nil {
		return fmt.Errorf("nvme disconnect -d %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// controllerName is the kernel name nvme-cli addresses a controller by,
// e.g., `nvme3`, taken from the id or from the sysfs path it was found at.
func controllerName(ctrl nvme.Controller) string {
	if ctrl.ID != "" {
		return string(ctrl.ID)
	}
	if ctrl.SysfsPath != "" {
		return filepath.Base(ctrl.SysfsPath)
	}
	return ""
}

// isAlreadyConnected reports whether a failed `nvme connect` failed only because
// a controller for that NQN and address exists. nvme-cli says so in its output
// rather than through an exit code, so the text is what there is to go on.
func isAlreadyConnected(out []byte, err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "already connected")
}

// runCommand executes nvme-cli and returns its combined output, which is where
// nvme-cli puts the reason for a failure.
func runCommand(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "nvme", args...).CombinedOutput() //nolint:gosec // fixed binary, structured args
}
