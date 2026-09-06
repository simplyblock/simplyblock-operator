// A volume given more members.
//
// The other way capacity arrives, and the one a striped export depends on: its
// members cannot grow, so more are attached and the volume spreads onto them.
// The driver publishes every namespace up front and then tells the two phases
// about different numbers of them, which is what a volume gaining members looks
// like from the node: the same volume, and more underneath it than last time.

package suites

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

const (
	extendUUID       = "2f7d4c19-8ba6-4e35-97c1-5d0e8a6b3f42"
	extendNQN        = "nqn.2023-04.io.simplyblock:integration:" + extendUUID
	extendPort       = 4460
	extendSizeMB     = 512
	extendStripes    = 2
	extendStarting   = 2
	extendTotal      = 4
	extendRemotePath = "/tmp/volstack-extend.test"
)

// TestVolumeStackExtendsOntoNewMembers stages a striped volume over two
// namespaces and then gives it two more.
func TestVolumeStackExtendsOntoNewMembers(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{Name: clusterNameFor("extend")})
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

	if !requireTools(ctx, t, sh) {
		t.Skipf("the shell image %s carries no LVM tooling", stackImage())
	}

	binary := buildOnNodeSuite(ctx, t)
	if copyErr := c.CopyTo(ctx, fabric.Namespace, sh.Pod(), binary, extendRemotePath); copyErr != nil {
		t.Fatalf("carry the on-node suite to %s: %v", node, copyErr)
	}

	// Every namespace exists from the start. What changes between the phases is
	// how many of them the node is told about, which is the same thing as far as
	// the node can tell: a volume it staged over two now has four underneath it.
	targets := extendTargets(ctx, t, sh, ip, extendTotal)

	first := extendEnv(targets[:extendStarting], ip)
	all := extendEnv(targets, ip)

	runPhase(ctx, t, sh, first, "TestExtendStage", extendRemotePath)
	runPhase(ctx, t, sh, all, "TestExtendMembers", extendRemotePath)
}

// extendTargets publishes one subsystem per member.
func extendTargets(
	ctx context.Context, t *testing.T, sh *fabric.Shell, ip string, members int,
) []*fabric.Target {
	t.Helper()
	targets := make([]*fabric.Target, 0, members)
	for i := range members {
		nqn := extendNQN + ":" + strconv.Itoa(i)
		target, err := fabric.NewTarget(ctx, sh, fabric.TargetSpec{
			NQN:       nqn,
			Model:     "simplyblock-integration",
			Serial:    "extend" + strconv.Itoa(i),
			CntlIDMin: 700 + i*100,
			CntlIDMax: 700 + i*100 + 99,
			Addr:      ip,
			Port:      extendPort + i,
			PortID:    70 + i,
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
		if err = target.AddNamespace(ctx, 1, extendSizeMB, ""); err != nil {
			t.Fatalf("add a namespace to %s: %v", nqn, err)
		}
		targets = append(targets, target)
	}
	return targets
}

// extendEnv names the members one phase is to build over. The volume's identity
// and where it is staged are the same for both, because it is the same volume.
func extendEnv(targets []*fabric.Target, ip string) map[string]string {
	env := map[string]string{
		"SB_ONNODE":         "1",
		"SB_PHASED":         "1",
		"SB_VOLUME_UUID":    extendUUID,
		"SB_EXTEND_MEMBERS": strconv.Itoa(len(targets)),
		"SB_EXTEND_STRIPES": strconv.Itoa(extendStripes),
		"SB_EXTEND_FS":      "ext4",
		"SB_STAGING_PATH":   "/var/tmp/volstack-extend/staging",
		"SB_RECORDS":        "/var/tmp/volstack-extend/records",
	}
	for i, target := range targets {
		prefix := "SB_TARGET"
		if i > 0 {
			prefix = "SB_TARGET" + strconv.Itoa(i+1)
		}
		env[prefix+"_NQN"] = target.NQN()
		env[prefix+"_ADDR"] = ip
		env[prefix+"_PORT"] = strconv.Itoa(extendPort + i)
		env[prefix+"_NSID"] = "1"
	}
	return env
}
