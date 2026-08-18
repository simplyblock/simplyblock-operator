package fabric

import (
	"context"
	"fmt"
	"strings"
)

// Initiator connects a node to targets by writing to the kernel directly: a
// connect options line to /dev/nvme-fabrics, and delete_controller or
// rescan_controller under a controller's sysfs directory.
//
// This is the mechanism atlas's own FabricsConnector uses, so the fabric these
// tests build is established the way the code under test establishes one. It also
// needs no nvme-cli in the pod.
//
// The controllers belong to the node's kernel, not to the pod: the nvme-tcp host
// is global. So the sysfs the tests read back is the node's real state, and a
// test that leaves a controller behind leaves it behind for the next one.
type Initiator struct {
	sh      *Shell
	hostNQN string
	hostID  string
}

// NewInitiator prepares a node to connect.
//
// The host identity is derived from the node name and passed on every connect
// rather than left to /etc/nvme/hostnqn: two controllers with different host NQNs
// are two hosts as far as the target is concerned, which is not the topology
// under test.
func NewInitiator(_ context.Context, sh *Shell) (*Initiator, error) {
	id := hostUUID(sh.Node())
	return &Initiator{
		sh:      sh,
		hostNQN: "nqn.2014-08.org.nvmexpress:uuid:" + id,
		hostID:  id,
	}, nil
}

// Connect attaches one controller to one target endpoint. Connecting the same
// NQN twice over different endpoints is how a subsystem ends up with two
// controllers.
func (i *Initiator) Connect(ctx context.Context, nqn, addr string, port int) error {
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%d,nqn=%s,hostnqn=%s,hostid=%s",
		addr, port, nqn, i.hostNQN, i.hostID)

	// Opened read-write, as nvme-cli and atlas both do: the kernel replies on the
	// same descriptor with the instance and cntlid it assigned. The reply is not
	// read here, but a write-only open is not what the fabrics device expects.
	out, err := i.sh.Run(ctx, fmt.Sprintf(
		"exec 3<>/dev/nvme-fabrics && printf %%s %s >&3 && exec 3>&-", quote(opts)))
	if err != nil {
		return fmt.Errorf("connect %s at %s:%d from %s: %w\n%s\noptions: %s",
			nqn, addr, port, i.sh.Node(), err, out, opts)
	}
	return nil
}

// Disconnect tears down every controller of nqn, the way atlas repairs one: a "1"
// into delete_controller.
func (i *Initiator) Disconnect(ctx context.Context, nqn string) error {
	out, err := i.sh.Run(ctx, forEachController(nqn, `echo 1 > "$c"/delete_controller`))
	if err != nil {
		return fmt.Errorf("disconnect %s on %s: %w\n%s", nqn, i.sh.Node(), err, out)
	}
	return nil
}

// Rescan asks each of the subsystem's controllers to re-enumerate its
// namespaces. The target sends an AEN when its namespace set changes and the
// host acts on it by itself; this is the nudge for one that has not yet.
func (i *Initiator) Rescan(ctx context.Context, nqn string) error {
	out, err := i.sh.Run(ctx, forEachController(nqn, `echo 1 > "$c"/rescan_controller`))
	if err != nil {
		return fmt.Errorf("rescan %s on %s: %w\n%s", nqn, i.sh.Node(), err, out)
	}
	return nil
}

// Describe is the host's NVMe state read straight from sysfs, for failure
// messages. Deliberately not the resolver's view: when an assertion about the
// resolver fails, its own output is not evidence.
func (i *Initiator) Describe(ctx context.Context) string {
	out, err := i.sh.Run(ctx, `for c in /sys/class/nvme/nvme*; do
	[ -d "$c" ] || continue
	printf '%s state=%s cntlid=%s addr=%s nqn=%s\n' "$(basename "$c")" \
		"$(cat "$c"/state 2>/dev/null)" "$(cat "$c"/cntlid 2>/dev/null)" \
		"$(cat "$c"/address 2>/dev/null)" "$(cat "$c"/subsysnqn 2>/dev/null)"
	for n in "$c"/nvme*n*; do
		[ -e "$n" ] && printf '  path %s\n' "$(basename "$n")"
	done
done
for s in /sys/class/nvme-subsystem/nvme-subsys*; do
	[ -d "$s" ] || continue
	printf '%s nqn=%s model=%s serial=%s\n' "$(basename "$s")" \
		"$(cat "$s"/subsysnqn 2>/dev/null)" "$(cat "$s"/model 2>/dev/null)" \
		"$(cat "$s"/serial 2>/dev/null)"
done`)
	if err != nil {
		return fmt.Sprintf("(reading sysfs failed: %v)\n%s", err, out)
	}
	return out
}

// forEachController runs body for every controller whose subsysnqn is nqn, with
// the controller's sysfs directory in $c.
//
// Matching on subsysnqn rather than on a controller list the caller passes keeps
// this independent of the resolver under test, and it is what makes Disconnect
// safe to call from a cleanup that does not know what came up.
func forEachController(nqn, body string) string {
	return strings.Join([]string{
		"NQN=" + quote(nqn),
		`for c in /sys/class/nvme/nvme*; do`,
		`	[ -f "$c"/subsysnqn ] || continue`,
		`	[ "$(cat "$c"/subsysnqn)" = "$NQN" ] || continue`,
		"	" + body,
		`done`,
	}, "\n")
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
