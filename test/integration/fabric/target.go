package fabric

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// TargetSpec describes one nvmet subsystem and the port it listens on.
//
// A pair of targets sharing an NQN is what the host merges into a single
// multipath subsystem with two controllers. The NQN is the only key: the kernel
// looks a subsystem up by subsysnqn alone, and copies the model and serial from
// whichever controller got there first without comparing them again
// (nvme_init_subsystem). What it does reject is a second controller whose cntlid
// it has already seen, or one that does not advertise multi-controller support.
//
// So Model and Serial are not what makes the merge happen. They are set anyway
// because the detection reads them — a simplyblock volume's model is its master
// lvol UUID — and a pair that disagreed would be a fabric no cluster produces.
type TargetSpec struct {
	// NQN is the subsystem NQN, and also its configfs directory name.
	NQN string

	// Model and Serial are reported in Identify Controller. Two targets under
	// one NQN should agree on both — the kernel will not object if they do not,
	// but a real cluster's pair always does.
	Model  string
	Serial string

	// CntlIDMin and CntlIDMax bound the controller IDs this target hands out.
	// Two targets sharing an NQN need disjoint ranges: controller IDs are unique
	// per subsystem rather than per target, and the kernel rejects a controller
	// whose ID it has already seen ("Duplicate cntlid %u ... rejecting"). This is
	// the field the merge actually depends on; nvmet's default range is the whole
	// space, so leaving these at zero is what makes the second connect fail.
	CntlIDMin int
	CntlIDMax int

	// Addr and Port are the TCP listener. Addr must be an address the initiator
	// can reach, which for a node-hosted target is the node's internal IP.
	Addr string
	Port int

	// PortID names the configfs port directory. Ports are per node, so two
	// targets on two nodes may share an ID; two on one node may not.
	PortID int

	// ANAState is the port's ANA group 1 state: "optimized",
	// "non-optimized" or "inaccessible". Empty leaves nvmet's default.
	ANAState string
}

// Target is an nvmet subsystem on one node, with a TCP port pointing at it.
type Target struct {
	sh   *Shell
	spec TargetSpec
	root string

	// namespaces records what to tear down, nsid to loop device.
	namespaces map[int]string
}

// NewTarget creates the subsystem and its port. The subsystem exports nothing
// until AddNamespace: a target with no namespaces is a legitimate state and the
// one that produces a non-contributing controller.
func NewTarget(ctx context.Context, sh *Shell, spec TargetSpec) (*Target, error) {
	root, err := sh.EnsureConfigFS(ctx)
	if err != nil {
		return nil, err
	}
	t := &Target{sh: sh, spec: spec, root: root, namespaces: map[int]string{}}

	script := strings.Join([]string{
		"set -e",
		"S=" + quote(t.subsysDir()),
		"P=" + quote(t.portDir()),
		"mkdir -p \"$S\"",
		"echo 1 > \"$S\"/attr_allow_any_host",
		// The premise of a shared-NQN pair. Without these the two targets both
		// hand out cntlid 1 and the host rejects the second controller, which
		// fails later and far less legibly than here.
		"[ -f \"$S\"/attr_cntlid_min ] || { echo 'kernel lacks attr_cntlid_min' >&2; exit 1; }",
		fmt.Sprintf("printf %%s %s > \"$S\"/attr_model", quote(spec.Model)),
		fmt.Sprintf("printf %%s %s > \"$S\"/attr_serial", quote(spec.Serial)),
		// Before the port link, which is what opens the target to connects.
		fmt.Sprintf("echo %d > \"$S\"/attr_cntlid_min", spec.CntlIDMin),
		fmt.Sprintf("echo %d > \"$S\"/attr_cntlid_max", spec.CntlIDMax),
		"mkdir -p \"$P\"",
		"echo ipv4 > \"$P\"/addr_adrfam",
		"echo tcp > \"$P\"/addr_trtype",
		fmt.Sprintf("echo %s > \"$P\"/addr_traddr", quote(spec.Addr)),
		fmt.Sprintf("echo %d > \"$P\"/addr_trsvcid", spec.Port),
		anaLine(spec.ANAState),
		fmt.Sprintf("ln -sfn \"$S\" \"$P\"/subsystems/%s", quote(spec.NQN)),
	}, "\n")

	if out, err := sh.Run(ctx, script); err != nil {
		return nil, fmt.Errorf("create target %s on %s: %w\n%s", spec.NQN, sh.Node(), err, out)
	}
	return t, nil
}

func anaLine(state string) string {
	if state == "" {
		return ":"
	}
	return fmt.Sprintf("printf %%s %s > \"$P\"/ana_groups/1/ana_state", quote(state))
}

// AddNamespace backs namespace nsid with a sparse file of sizeMB, attached to a
// loop device.
//
// The file lives in the pod and the loop device is the node's — loop is not
// namespaced, and it holds the file open, so nvmet resolves a stable block
// device rather than a path that means something different in another mount
// namespace.
//
// uuid, when set, becomes the namespace's device_uuid, which is what the host
// publishes as the namespace UUID and what a device selector matches on.
func (t *Target) AddNamespace(ctx context.Context, nsid, sizeMB int, uuid string) error {
	img := fmt.Sprintf("/var/tmp/nvmet/%s-ns%d.img", sanitize(t.spec.NQN), nsid)
	nsDir := fmt.Sprintf("%s/namespaces/%d", t.subsysDir(), nsid)

	uuidLine := ":"
	if uuid != "" {
		uuidLine = fmt.Sprintf("printf %%s %s > \"$N\"/device_uuid", quote(uuid))
	}

	script := strings.Join([]string{
		"set -e",
		"IMG=" + quote(img),
		"N=" + quote(nsDir),
		"mkdir -p \"$(dirname \"$IMG\")\"",
		fmt.Sprintf("[ -f \"$IMG\" ] || truncate -s %dM \"$IMG\" 2>/dev/null || "+
			"dd if=/dev/zero of=\"$IMG\" bs=1M count=0 seek=%d 2>/dev/null", sizeMB, sizeMB),
		// BusyBox losetup syntax only: -a to list, -f for the next free device,
		// and the two-argument form to associate. It has no -j and no --show.
		// The list is checked first so a retry reuses the existing device
		// instead of stacking a second one on the same file.
		"DEV=$(losetup -a 2>/dev/null | grep -F \"$IMG\" | cut -d: -f1 | head -1)",
		"if [ -z \"$DEV\" ]; then DEV=$(losetup -f) && losetup \"$DEV\" \"$IMG\"; fi",
		"[ -n \"$DEV\" ] || { echo 'no loop device available' >&2; exit 1; }",
		"mkdir -p \"$N\"",
		"printf %s \"$DEV\" > \"$N\"/device_path",
		uuidLine,
		"echo 1 > \"$N\"/enable",
		"echo \"$DEV\"",
	}, "\n")

	out, err := t.sh.Run(ctx, script)
	if err != nil {
		return fmt.Errorf("add namespace %d to %s on %s: %w\n%s",
			nsid, t.spec.NQN, t.sh.Node(), err, out)
	}
	t.namespaces[nsid] = strings.TrimSpace(out)
	return nil
}

// DisableNamespace takes a namespace offline without removing it. The host keeps
// the controller and loses the path, which is the difference between a
// namespace that never existed and one that went away.
func (t *Target) DisableNamespace(ctx context.Context, nsid int) error {
	out, err := t.sh.Run(ctx, fmt.Sprintf("echo 0 > %s/namespaces/%d/enable",
		quote(t.subsysDir()), nsid))
	if err != nil {
		return fmt.Errorf("disable namespace %d on %s: %w\n%s", nsid, t.spec.NQN, err, out)
	}
	return nil
}

// NQN is the subsystem NQN.
func (t *Target) NQN() string { return t.spec.NQN }

// Endpoint is the address:port an initiator connects to.
func (t *Target) Endpoint() (string, int) { return t.spec.Addr, t.spec.Port }

// Close removes the port, the namespaces and the subsystem, and detaches the
// loop devices.
//
// configfs enforces the order — a subsystem directory will not rmdir while a
// port still links it, and a namespace will not while enabled — so this runs
// as one script and reports the first failure rather than unwinding in Go.
func (t *Target) Close(ctx context.Context) error {
	lines := []string{
		"S=" + quote(t.subsysDir()),
		"P=" + quote(t.portDir()),
		fmt.Sprintf("rm -f \"$P\"/subsystems/%s || true", quote(t.spec.NQN)),
		"rmdir \"$P\" 2>/dev/null || true",
	}
	for nsid, dev := range t.namespaces {
		lines = append(lines,
			fmt.Sprintf("echo 0 > \"$S\"/namespaces/%d/enable 2>/dev/null || true", nsid),
			fmt.Sprintf("rmdir \"$S\"/namespaces/%d 2>/dev/null || true", nsid),
			fmt.Sprintf("losetup -d %s 2>/dev/null || true", quote(dev)),
		)
	}
	lines = append(lines, "rmdir \"$S\" 2>/dev/null || true")

	if out, err := t.sh.Run(ctx, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("close target %s on %s: %w\n%s", t.spec.NQN, t.sh.Node(), err, out)
	}
	return nil
}

func (t *Target) subsysDir() string { return t.root + "/subsystems/" + t.spec.NQN }
func (t *Target) portDir() string {
	return t.root + "/ports/" + strconv.Itoa(t.spec.PortID)
}

// quote makes a value safe as a single shell word. NQNs carry colons and dots
// and the scripts here are assembled as text, so nothing may be interpolated
// bare.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
