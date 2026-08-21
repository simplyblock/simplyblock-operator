package volumemigration

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/ptr"
)

// Connection describes one NVMe-oF target path to connect and validate. It is the
// wire format between the operator and the validation Job (VMIG_CONNECTIONS), so
// its JSON tags are part of that contract and outlive any one connect mechanism.
type Connection struct {
	NQN            string `json:"nqn"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	Transport      string `json:"transport"`
	NrIoQueues     int    `json:"nrIoQueues,omitempty"`
	ReconnectDelay int    `json:"reconnectDelay,omitempty"`
	CtrlLossTmo    int    `json:"ctrlLossTmo,omitempty"`
	FastIOFailTmo  int    `json:"fastIOFailTmo,omitempty"`
	KeepAliveTmo   int    `json:"keepAliveTmo,omitempty"`
}

// Address is the "<ip>:<port>" form the host reports a controller at, which is how
// an expected path is matched against an attached one.
func (c Connection) Address() string {
	return fmt.Sprintf("%s:%d", c.IP, c.Port)
}

// CtrlLossTmoSec is how long the kernel keeps retrying a migration target path whose
// controller it has lost, before deleting it.
//
// It must match the CSI driver's DefaultCtrlLossTmo (csi-driver's initiator.go), which
// is the value every volume path in this system is connected with: the driver hardcodes
// it and ignores the ctrl-loss-tmo the control plane returns. A migration target path
// becomes the volume's data path at cutover, so connecting it with anything else would
// leave the same volume's paths on two different loss timeouts depending on which one
// last moved.
//
// The control plane's value is deliberately not used, and the migration is where that
// matters most. It answers CreateMigration with an hour, which for a path that may
// never be cut over to means an abandoned controller retries a target that has stopped
// answering for it until the node reboots — and because the kernel resets its reconnect
// counter whenever a reconnect gets far enough, "an hour" is a floor rather than a
// bound. At a minute, a target path nothing cleans up expires on its own.
//
// The cleanups are still needed and are not a fallback for this: ReleaseMigrationPaths
// removes the path when validation fails, ReapDeadControllers removes the husk a lost
// one leaves behind. This only bounds how long a path that escapes both can survive.
// It is applied where the control plane's answer is first recorded, so that the
// VolumeMigration's status.connections is a truthful record of the connect that will be
// made, and so there is one place that decides it rather than two that can disagree.
const CtrlLossTmoSec = 60

// target renders one connection as an atlas connect target.
//
// A zero tunable is left off rather than sent as 0, matching what the operator
// asked for: the control plane only fills the fields it has an opinion about, and
// the kernel default is the right answer for the rest. The two timeouts are
// pointers in atlas because 0 is meaningful for them (fail I/O immediately), so
// "unset" has to be expressible as something other than zero.
func (c Connection) target() nvmeof.Target {
	t := nvmeof.Target{
		NQN:               c.NQN,
		Transport:         nvmeof.Transport(c.Transport),
		Address:           c.IP,
		Port:              c.Port,
		NrIOQueues:        c.NrIoQueues,
		ReconnectDelaySec: c.ReconnectDelay,
		KeepAliveTMOSec:   c.KeepAliveTmo,
	}
	// The two timeouts are sent whenever they are set to anything at all, including
	// the negative "retry forever" and the zero "fail I/O at once" — atlas makes them
	// pointers precisely because both are meaningful (see nvmeof.WithCtrlLossTMOSec),
	// so a `> 0` guard here would silently turn either into the kernel default.
	// Unset is the only thing that may be dropped, and for these fields the CRD's
	// omitempty makes unset indistinguishable from zero; zero is the safer reading of
	// the two, since sending it is what the caller asked for either way.
	if c.CtrlLossTmo != 0 {
		t.CtrlLossTMOSec = ptr.To(c.CtrlLossTmo)
	}
	if c.FastIOFailTmo != 0 {
		t.FastIOFailTMOSec = ptr.To(c.FastIOFailTmo)
	}
	return t
}

// connector builds the nvme-cli connector the validation Job attaches through,
// reading controller state under sysRoot — the host's /sys, mounted into the Job,
// rather than the container's own.
//
// nvme-cli runs through sudo because the rebalancer image runs as an unprivileged
// uid to satisfy the OpenShift SCC and Red Hat certification requirements, and its
// sudoers rule is what gets the fabrics device open. The container being privileged
// is not enough on its own: those capabilities are dropped on the way to a non-root
// uid.
func connector(sysRoot string) *nvmeof.CLIConnector {
	return nvmeof.NewCLIConnectorWithRunner(
		nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{SysRoot: sysRoot}),
		nvmeof.SudoRunner,
	)
}

// EnsureMigrationPaths connects each NVMe-oF path and returns what became of it.
//
// Attaching goes through atlas rather than a `nvme connect` per connection, and the
// difference that matters here is idempotence: a path whose controller already
// exists is left alone instead of connected again. Re-issuing the connect — which
// is what the retry loop above this does — otherwise adds a second controller for
// the same endpoint each time round, and a subsystem carrying duplicate controllers
// for one address is precisely the state the validation is meant to rule out.
// Paths are attached one at a time, in the order the control plane returned them,
// and each is given a bounded wait to reach a live state.
//
// The connect's own success is still not proof that the path is usable: a
// controller can be live and serve no namespace. VerifyMigrationPaths establishes
// that, by reading the host's own view afterwards rather than trusting this report.
//
// Connections are grouped by NQN because atlas attaches one subsystem at a time. A
// migration moves a single subsystem, so in practice there is one group.
func EnsureMigrationPaths(ctx context.Context, sysRoot string, conns []Connection) error {
	c := connector(sysRoot)
	for _, nqn := range subsystemOrder(conns) {
		targets := make([]nvmeof.Target, 0, len(conns))
		for _, conn := range conns {
			if conn.NQN == nqn {
				targets = append(targets, conn.target())
			}
		}
		if _, err := c.ConnectPaths(ctx, targets); err != nil {
			return err
		}
	}
	return nil
}

// subsystemOrder returns the distinct NQNs of conns, in first-seen order.
func subsystemOrder(conns []Connection) []string {
	seen := make(map[string]bool, len(conns))
	order := make([]string, 0, 1)
	for _, c := range conns {
		if !seen[c.NQN] {
			seen[c.NQN] = true
			order = append(order, c.NQN)
		}
	}
	return order
}
