// The volume stack on a node, driven from here.
//
// The assertions are not in this file. They are in test/integration/onnode,
// compiled for the node and run there, because the layers call the kernel: an
// O_DIRECT read with its own alignment rules, an ioctl for the block size, a
// mount syscall, nvme-cli, LVM. Driving those from the test host would mean
// substituting an adapter for every one of them, and the adapters would then be
// what the suite proved correct.
//
// So this file is a driver: it raises a cluster, publishes the namespaces the
// on-node suite expects, carries the binary across, runs it, and reports what it
// said.

package suites

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

// stackShellImage has to carry every tool the layers shell out to: nvme-cli for
// the fabric layer, lvm2 for the two LVM layers, and mkfs plus mount for the
// filesystem layer. The driver's own base image is the faithful choice, since
// running the stack against the binaries that ship is the point, and a missing
// tool here is a missing tool in production too.
//
// Overridable because the published image and the Dockerfile in this repository
// are built separately, and a run should be able to name an image that is known
// to carry the set rather than wait on that being reconciled.
const stackShellImage = "simplyblock/spdkcsi:base_image"

// stackTools are the binaries the on-node suite cannot run without. They are
// checked before anything is built or copied, so a missing one reads as a
// missing one rather than as a layer that failed.
var stackTools = []string{"nvme", "pvcreate", "vgcreate", "lvcreate", "mkfs.ext4", "mount", "blockdev"}

// The stack volume's identity, distinct from the other suites' so that a shared
// snapshot can never confuse them.
const (
	stackVolumeUUID = "3f6c1d84-95ab-4e27-b0d3-7c8e2a4f6b19"
	stackNQN        = "nqn.2023-04.io.simplyblock:integration:" + stackVolumeUUID
	stackNQN2       = "nqn.2023-04.io.simplyblock:integration:" + stackVolumeUUID + ":m2"
	stackPort       = 4430
	stackPort2      = 4431
	stackRemotePath = "/tmp/volstack.test"
)

// TestVolumeStackOnNode brings every plan shape up against a real kernel.
//
// One cluster and one run for all of them: raising a Talos cluster is minutes,
// and the plans are independent of each other in a way that does not need a
// fresh machine between them. What they are not independent of is the fabric, so
// each gets its own namespace.
func TestVolumeStackOnNode(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{Name: clusterNameFor("volstack")})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if destroyErr := c.Destroy(context.WithoutCancel(ctx)); destroyErr != nil {
			t.Errorf("destroy cluster: %v", destroyErr)
		}
	})

	if err = c.WaitNodesReady(ctx, 1, 5*time.Minute); err != nil {
		t.Fatalf("nodes never became ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("list nodes: %v (%v)", err, nodes)
	}
	node := nodes[0]
	ip := internalIP(ctx, t, c, node)

	sh, err := fabric.NewShell(ctx, c, node, fabric.WithImage(stackImage()))
	if err != nil {
		t.Fatalf("start a shell on %s: %v", node, err)
	}
	t.Cleanup(func() {
		if closeErr := sh.Close(context.WithoutCancel(ctx)); closeErr != nil {
			t.Errorf("close the shell on %s: %v", node, closeErr)
		}
	})

	requireTools(ctx, t, sh)

	// Two namespaces, because the striped plan is the one shape whose bottom
	// layer takes more than one and the composite's ordering is what the record
	// has to preserve.
	first := publishNamespace(ctx, t, sh, stackNQN, ip, stackPort, 1)
	second := publishNamespace(ctx, t, sh, stackNQN2, ip, stackPort2, 2)

	binary := buildOnNodeSuite(ctx, t)
	if copyErr := c.CopyTo(ctx, fabric.Namespace, sh.Pod(), binary, stackRemotePath); copyErr != nil {
		t.Fatalf("carry the on-node suite to %s: %v", node, copyErr)
	}

	env := map[string]string{
		"SB_ONNODE":       "1",
		"SB_VOLUME_UUID":  stackVolumeUUID,
		"SB_TARGET_NQN":   first.NQN(),
		"SB_TARGET_ADDR":  ip,
		"SB_TARGET_PORT":  strconv.Itoa(stackPort),
		"SB_TARGET_NSID":  "1",
		"SB_TARGET2_NQN":  second.NQN(),
		"SB_TARGET2_ADDR": ip,
		"SB_TARGET2_PORT": strconv.Itoa(stackPort2),
		"SB_TARGET2_NSID": "1",
	}

	out, err := sh.Run(ctx, exportEnv(env)+stackRemotePath+" -test.v -test.timeout=30m")
	t.Logf("on-node suite output:\n%s", out)
	if err != nil {
		t.Fatalf("the on-node suite failed on %s: %v", node, err)
	}
	if !strings.Contains(out, "PASS") || strings.Contains(out, "FAIL") {
		t.Fatalf("the on-node suite did not pass on %s", node)
	}
}

// stackImage is the image the shell runs, overridable for a run that knows a
// better one than the default.
func stackImage() string {
	if v := os.Getenv("SB_STACK_IMAGE"); v != "" {
		return v
	}
	return stackShellImage
}

// requireTools fails early when the image is missing something the layers shell
// out to. Without it the first missing binary surfaces as a layer error deep in
// the on-node run, which reads as a defect in the code under test.
func requireTools(ctx context.Context, t *testing.T, sh *fabric.Shell) {
	t.Helper()
	var missing []string
	for _, tool := range stackTools {
		out, err := sh.Run(ctx, "command -v "+shellValue(tool)+" || true")
		if err != nil {
			t.Fatalf("look for %s on %s: %v", tool, sh.Node(), err)
		}
		if strings.TrimSpace(out) == "" {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		t.Skipf("the shell image %s is missing %v, and the stack shells out to every one of them; "+
			"set SB_STACK_IMAGE to an image that carries them", stackImage(), missing)
	}
}

// publishNamespace creates one nvmet subsystem with a single namespace behind
// it, and returns the target so the caller can name it to the on-node suite.
func publishNamespace(
	ctx context.Context, t *testing.T, sh *fabric.Shell, nqn, ip string, port, portID int,
) *fabric.Target {
	t.Helper()
	target, err := fabric.NewTarget(ctx, sh, fabric.TargetSpec{
		NQN:       nqn,
		Model:     "simplyblock-integration",
		Serial:    "volstack" + strconv.Itoa(portID),
		CntlIDMin: portID * 100,
		CntlIDMax: portID*100 + 99,
		Addr:      ip,
		Port:      port,
		PortID:    portID,
		ANAState:  "optimized",
	})
	if err != nil {
		t.Fatalf("publish %s: %v", nqn, err)
	}
	t.Cleanup(func() {
		if closeErr := target.Close(context.WithoutCancel(ctx)); closeErr != nil {
			t.Errorf("withdraw %s: %v", nqn, closeErr)
		}
	})

	// Large enough that LVM has room for its metadata and an extent or two of
	// data, which a few megabytes does not leave.
	if err = target.AddNamespace(ctx, 1, 512, ""); err != nil {
		t.Fatalf("add a namespace to %s: %v", nqn, err)
	}
	return target
}

// buildOnNodeSuite compiles the on-node package for the node's architecture and
// returns the path to the binary.
//
// The node's architecture is the host's: the harness runs QEMU with the host's
// own architecture so the machines boot at native speed, which is the only
// reason a Talos cluster in a test is minutes rather than an afternoon.
func buildOnNodeSuite(ctx context.Context, t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "volstack.test")

	//nolint:gosec // the toolchain, with arguments this function composed
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-o", binary, "./onnode/")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the on-node suite for linux/%s: %v\n%s", runtime.GOARCH, err, out)
	}
	return binary
}

// moduleRoot is this module's directory, which is the parent of the suites
// package the test binary runs from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	return filepath.Dir(wd)
}

// exportEnv renders the on-node suite's parameters as a shell prefix.
func exportEnv(env map[string]string) string {
	var b strings.Builder
	for k, v := range env {
		fmt.Fprintf(&b, "%s=%s ", k, shellValue(v))
	}
	return b.String()
}

// shellValue makes one value safe as a single shell word.
func shellValue(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
