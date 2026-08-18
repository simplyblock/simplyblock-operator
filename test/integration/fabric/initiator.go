package fabric

import (
	"context"
	"fmt"
	"strings"
)

// Initiator runs nvme-cli on a node, connecting it to targets.
//
// The controllers it creates belong to the node's kernel, not to the pod: the
// nvme-tcp host is global. That is why the sysfs the tests read back is the
// node's real state, and why a test that leaves a controller behind leaves it
// behind for the next one.
type Initiator struct {
	sh *Shell
}

// NewInitiator prepares a node to connect. It writes a host NQN derived from the
// node name: nvme-cli invents one per invocation when /etc/nvme/hostnqn is
// missing, and two controllers with different host NQNs are two hosts as far as
// the target is concerned, which is not the topology under test.
func NewInitiator(ctx context.Context, sh *Shell) (*Initiator, error) {
	nqn := "nqn.2014-08.org.nvmexpress:uuid:" + hostUUID(sh.Node())
	script := strings.Join([]string{
		"set -e",
		"mkdir -p /etc/nvme",
		fmt.Sprintf("printf '%%s\\n' %s > /etc/nvme/hostnqn", quote(nqn)),
		fmt.Sprintf("printf '%%s\\n' %s > /etc/nvme/hostid", quote(hostUUID(sh.Node()))),
		"modprobe nvme_tcp 2>/dev/null || true",
	}, "\n")
	if out, err := sh.Run(ctx, script); err != nil {
		return nil, fmt.Errorf("prepare initiator on %s: %w\n%s", sh.Node(), err, out)
	}
	return &Initiator{sh: sh}, nil
}

// Connect attaches one controller to one target endpoint. Connecting the same
// NQN twice over different endpoints is how a subsystem ends up with two
// controllers.
func (i *Initiator) Connect(ctx context.Context, nqn, addr string, port int) error {
	out, err := i.sh.Run(ctx, fmt.Sprintf(
		"nvme connect -t tcp -a %s -s %d -n %s", quote(addr), port, quote(nqn)))
	if err != nil {
		return fmt.Errorf("connect %s at %s:%d from %s: %w\n%s",
			nqn, addr, port, i.sh.Node(), err, out)
	}
	return nil
}

// Disconnect tears down every controller of nqn.
func (i *Initiator) Disconnect(ctx context.Context, nqn string) error {
	out, err := i.sh.Run(ctx, fmt.Sprintf("nvme disconnect -n %s", quote(nqn)))
	if err != nil {
		return fmt.Errorf("disconnect %s on %s: %w\n%s", nqn, i.sh.Node(), err, out)
	}
	return nil
}

// ListSubsys is `nvme list-subsys` output, for failure messages. The tests
// assert on sysfs, not on this — nvme-cli's JSON shape moves between releases —
// but a failed assertion is unreadable without seeing what the host saw.
func (i *Initiator) ListSubsys(ctx context.Context) string {
	out, err := i.sh.Run(ctx, "nvme list-subsys 2>&1 || true")
	if err != nil {
		return fmt.Sprintf("(list-subsys failed: %v)", err)
	}
	return out
}

// hostUUID derives a stable UUID-shaped string from a node name, so a rerun on
// the same node presents the same host identity.
func hostUUID(node string) string {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(node) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x",
		uint32(h), uint16(h>>32), uint16(h>>16)&0xfff, uint16(h>>4)&0xfff, h&0xffffffffffff)
}
