// A volume moving from one host to another.
//
// Two nodes, because that is the only way to ask the question. On one host a
// second bring-up finds its own mount and its own volume group still there and
// can be right by accident. On another there is nothing but the bytes on the
// device, and a layer that reads them wrong reformats a volume that was serving
// a pod a moment earlier.
//
// The fabric is served from the first node and reached from both, which is also
// what a real cluster does: the volume lives where it lives, and whichever node
// the pod lands on connects to it.

package suites

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

const (
	handoffUUID       = "8c2a17be-4d59-4e0a-b3f7-91d6c0a25e"
	handoffNQN        = "nqn.2023-04.io.simplyblock:integration:" + handoffUUID
	handoffPort       = 4450
	handoffSizeMB     = 512
	handoffRemotePath = "/tmp/volstack-handoff.test"
)

// TestVolumeStackMovesBetweenHosts stages a volume on one node, releases it, and
// stages it on another, for each plan that has anything to recognize.
func TestVolumeStackMovesBetweenHosts(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{
		Name: clusterNameFor("handoff"),
		// A worker, because a volume cannot be handed anywhere on a single node
		// and the point of the case is the host that has never seen it.
		Workers: 1,
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if destroyErr := c.Destroy(context.WithoutCancel(ctx)); destroyErr != nil {
			t.Errorf("destroy cluster: %v", destroyErr)
		}
	})
	if err = c.WaitNodesReady(ctx, 2, 8*time.Minute); err != nil {
		t.Fatalf("nodes never became ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) < 2 {
		t.Fatalf("list nodes: %v (%v)", err, nodes)
	}
	serving, receiving := nodes[0], nodes[1]
	ip := internalIP(ctx, t, c, serving)

	// One shell per node. The first serves the fabric as well as consuming it,
	// which is what every case in this suite does; the second only consumes it,
	// which is what every node in a real cluster does.
	shells := map[string]*fabric.Shell{}
	for _, node := range []string{serving, receiving} {
		sh, shellErr := fabric.NewShell(ctx, c, node, fabric.WithImage(stackImage()))
		if shellErr != nil {
			t.Fatalf("start a shell on %s: %v", node, shellErr)
		}
		t.Cleanup(func() {
			if closeErr := sh.Close(context.WithoutCancel(ctx)); closeErr != nil {
				t.Errorf("close the shell on %s: %v", node, closeErr)
			}
		})
		shells[node] = sh
	}

	if !requireTools(ctx, t, shells[serving]) {
		t.Skipf("the shell image %s carries no LVM tooling", stackImage())
	}

	binary := buildOnNodeSuite(ctx, t)
	for node, sh := range shells {
		if copyErr := c.CopyTo(ctx, fabric.Namespace, sh.Pod(), binary, handoffRemotePath); copyErr != nil {
			t.Fatalf("carry the on-node suite to %s: %v", node, copyErr)
		}
	}

	for _, tc := range []struct {
		name   string
		uuid   string
		shape  string
		fsType string
	}{
		{name: "ext4 on a plain plan", uuid: handoffUUID + "01", shape: "plain", fsType: "ext4"},
		{name: "ext4 over LVM", uuid: handoffUUID + "02", shape: "lvm", fsType: "ext4"},
		{name: "xfs over LVM", uuid: handoffUUID + "03", shape: "lvm", fsType: "xfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := handoffTarget(ctx, t, shells[serving], ip, tc.uuid)

			// The two hosts differ in their own identity and agree on the volume's,
			// which is the arrangement a cluster has: one volume, and as many
			// initiators as there are nodes that might run the pod.
			write := handoffEnv(target, ip, tc.uuid, tc.shape, tc.fsType, "11111111-0000-0000-0000-00000000000a")
			read := handoffEnv(target, ip, tc.uuid, tc.shape, tc.fsType, "11111111-0000-0000-0000-00000000000b")

			runPhase(ctx, t, shells[serving], write, "TestHandoffWrite", handoffRemotePath)
			runPhase(ctx, t, shells[receiving], read, "TestHandoffRead", handoffRemotePath)
		})
	}
}

// handoffTarget publishes the volume's namespace on the serving node.
func handoffTarget(
	ctx context.Context, t *testing.T, sh *fabric.Shell, ip, uuid string,
) *fabric.Target {
	t.Helper()
	nqn := handoffNQN + ":" + uuid
	target, err := fabric.NewTarget(ctx, sh, fabric.TargetSpec{
		NQN:       nqn,
		Model:     "simplyblock-integration",
		Serial:    "handoff",
		CntlIDMin: 500,
		CntlIDMax: 599,
		Addr:      ip,
		Port:      handoffPort,
		PortID:    50,
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
	if err = target.AddNamespace(ctx, 1, handoffSizeMB, ""); err != nil {
		t.Fatalf("add a namespace to %s: %v", nqn, err)
	}
	return target
}

// handoffEnv is what one host is told. The volume's identity is the same on both
// and the host's own is not, because the volume is one thing and the initiators
// are two.
func handoffEnv(target *fabric.Target, ip, uuid, shape, fsType, hostID string) map[string]string {
	return map[string]string{
		"SB_ONNODE":       "1",
		"SB_VOLUME_UUID":  uuid,
		"SB_PHASED":       "1",
		"SB_HANDOFF_PLAN": shape,
		"SB_HANDOFF_FS":   fsType,
		"SB_HOST_ID":      hostID,
		"SB_STAGING_PATH": "/var/tmp/volstack-handoff/" + uuid + "/staging",
		"SB_RECORDS":      "/var/tmp/volstack-handoff/" + uuid + "/records",
		"SB_TARGET_NQN":   target.NQN(),
		"SB_TARGET_ADDR":  ip,
		"SB_TARGET_PORT":  strconv.Itoa(handoffPort),
		"SB_TARGET_NSID":  "1",
		"SB_HOST_NQN":     "nqn.2014-08.org.nvmexpress:uuid:" + hostID,
	}
}

// runPhase runs one phase on one host and fails the case if it did not pass.
func runPhase(
	ctx context.Context, t *testing.T, sh *fabric.Shell, env map[string]string, phase, binary string,
) {
	t.Helper()
	out, err := sh.Run(ctx, exportEnv(env)+binary+" -test.v -test.timeout=20m -test.run "+phase+"$")
	t.Logf("%s on %s:\n%s", phase, sh.Node(), out)
	if err != nil {
		t.Fatalf("%s failed on %s: %v", phase, sh.Node(), err)
	}
	if !strings.Contains(out, "PASS") || strings.Contains(out, "FAIL") {
		t.Fatalf("%s did not pass on %s", phase, sh.Node())
	}
}
