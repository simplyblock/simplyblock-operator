// Growing a volume from the side that owns it.
//
// An expand is two parties: whoever serves the volume makes it bigger, and the
// node takes the space. The suite has to be both, and it has to be them in that
// order, so the stack is brought up in one run of the on-node binary, the
// namespaces are grown here, and a second run takes what appeared. A single run
// growing its own backing store would be proving that a test can resize a loop
// device.

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

// The grown volume's identity, and the sizes it moves between. Small, because
// what is under test is whether each layer takes the space rather than how much
// of it there is, and a sparse file costs nothing until it is written to.
const (
	// Completed by each case with two more digits, so every case has a volume
	// identity of its own that is still a UUID.
	growVolumeUUID = "6b41f0e7-25d8-4a3c-9f16-2ec7b5a03d"
	growNQN        = "nqn.2023-04.io.simplyblock:integration:" + growVolumeUUID
	growPort       = 4440
	growFromMB     = 512
	growToMB       = 1024
	growRemotePath = "/tmp/volstack-grow.test"
)

// TestVolumeStackGrows walks an expand up the whole stack, for each filesystem
// and for a volume spread across two namespaces.
func TestVolumeStackGrows(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{Name: clusterNameFor("grow")})
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
		t.Skipf("the shell image %s carries no LVM tooling, and every plan here has an LVM layer",
			stackImage())
	}

	binary := buildOnNodeSuite(ctx, t)
	if copyErr := c.CopyTo(ctx, fabric.Namespace, sh.Pod(), binary, growRemotePath); copyErr != nil {
		t.Fatalf("carry the on-node suite to %s: %v", node, copyErr)
	}

	for _, tc := range []struct {
		name    string
		uuid    string
		shape   string
		fsType  string
		members int
	}{
		{
			name: "ext4 on one namespace", uuid: growVolumeUUID + "01",
			shape: "lvm", fsType: "ext4", members: 1,
		},
		{
			name: "xfs on one namespace", uuid: growVolumeUUID + "02",
			shape: "lvm", fsType: "xfs", members: 1,
		},
		{
			name: "ext4 striped across two", uuid: growVolumeUUID + "03",
			shape: "striped", fsType: "ext4", members: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A namespace of its own per case, so a case never inherits what the
			// one before it left, and a fresh subsystem so the initiator has
			// nothing of the previous case still attached.
			targets := growTargets(ctx, t, sh, ip, tc.name, tc.members)
			env := growEnv(targets, ip, tc.name, tc.uuid, tc.shape, tc.fsType)

			runOnNode(ctx, t, sh, env, "TestGrowStage")

			for i, target := range targets {
				if err := target.GrowNamespace(ctx, 1, growToMB); err != nil {
					t.Fatalf("grow namespace %d: %v", i, err)
				}
			}

			runOnNode(ctx, t, sh, env, "TestGrowExtend")
		})
	}

	// A stripe cannot be extended onto one leg. LVM places the new extents across
	// as many members as the volume already uses and fails when it cannot, and the
	// alternative would be a volume half spread and half not, with nothing said.
	t.Run("a stripe with only one member grown", func(t *testing.T) {
		targets := growTargets(ctx, t, sh, ip, "partial", 2)
		env := growEnv(targets, ip, "partial", growVolumeUUID+"04", "striped", "ext4")

		runOnNode(ctx, t, sh, env, "TestGrowStage")

		if err := targets[0].GrowNamespace(ctx, 1, growToMB); err != nil {
			t.Fatalf("grow the first namespace: %v", err)
		}
		// Only the first grew, so only the first is waited for. Waiting for the
		// other would time out, and this case would then pass on the timeout
		// rather than on the refusal it exists to observe.
		env["SB_GROW_MEMBERS"] = "1"

		out, err := onNode(ctx, sh, env, "TestGrowExtend")
		if err == nil {
			t.Fatalf("a stripe extended onto one grown member:\n%s", out)
		}
		// The refusal has to come from the extension, not from anything on the way
		// to it. A case that accepted any failure would pass whether the stripe
		// held or the fabric fell over.
		if !strings.Contains(out, "grow the stack") {
			t.Fatalf("the extension failed for some reason other than refusing to spread onto one member:\n%s",
				tail(out, 20))
		}
		t.Logf("the stripe refused to extend onto one member, as it must:\n%s", tail(out, 12))
	})
}

// growTargets publishes one subsystem per member, each with a single namespace.
func growTargets(
	ctx context.Context, t *testing.T, sh *fabric.Shell, ip, name string, members int,
) []*fabric.Target {
	t.Helper()
	suffix := sanitizeForNQN(name)

	targets := make([]*fabric.Target, 0, members)
	for i := range members {
		nqn := growNQN + ":" + suffix + ":" + strconv.Itoa(i)
		port := growPort + i
		target, err := fabric.NewTarget(ctx, sh, fabric.TargetSpec{
			NQN:       nqn,
			Model:     "simplyblock-integration",
			Serial:    "grow" + strconv.Itoa(i),
			CntlIDMin: 300 + i*100,
			CntlIDMax: 300 + i*100 + 99,
			Addr:      ip,
			Port:      port,
			PortID:    30 + i,
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
		if err = target.AddNamespace(ctx, 1, growFromMB, ""); err != nil {
			t.Fatalf("add a namespace to %s: %v", nqn, err)
		}
		targets = append(targets, target)
	}
	return targets
}

// growEnv is what both phases of one case are told, and it has to be the same
// for both or the second addresses a different stack than the first raised.
func growEnv(targets []*fabric.Target, ip, name, uuid, shape, fsType string) map[string]string {
	// Fixed across the two phases of one case, because the phase that extends has
	// to find the stack the phase before it raised and a directory made by a
	// process that has exited is not where it is. Distinct between cases, because
	// a case that fails leaves its stack mounted and its volume group behind, and
	// the next one would then be handed the wreckage of the last one and fail for
	// a reason that has nothing to do with it.
	scope := sanitizeForNQN(name)
	env := map[string]string{
		"SB_ONNODE": "1",
		// The volume's identity is the case's, because the volume group and the
		// logical volume are named after it and two cases sharing one would have
		// the second find the first's. A UUID rather than a name, because the
		// initiator's own identity used to be derived from this one and a connect
		// carrying a hostid that is not a UUID is refused by the kernel.
		"SB_VOLUME_UUID":  uuid,
		"SB_GROW_PLAN":    shape,
		"SB_GROW_FS":      fsType,
		"SB_STAGING_PATH": "/var/tmp/volstack-grow/" + scope + "/staging",
		"SB_RECORDS":      "/var/tmp/volstack-grow/" + scope + "/records",
	}
	for i, target := range targets {
		prefix := "SB_TARGET"
		if i > 0 {
			prefix = "SB_TARGET" + strconv.Itoa(i+1)
		}
		env[prefix+"_NQN"] = target.NQN()
		env[prefix+"_ADDR"] = ip
		env[prefix+"_PORT"] = strconv.Itoa(growPort + i)
		env[prefix+"_NSID"] = "1"
	}
	return env
}

// runOnNode runs one phase and fails the case if it did not pass.
func runOnNode(ctx context.Context, t *testing.T, sh *fabric.Shell, env map[string]string, phase string) {
	t.Helper()
	out, err := onNode(ctx, sh, env, phase)
	t.Logf("%s:\n%s", phase, out)
	if err != nil {
		t.Fatalf("%s failed on %s: %v", phase, sh.Node(), err)
	}
	if !strings.Contains(out, "PASS") || strings.Contains(out, "FAIL") {
		t.Fatalf("%s did not pass on %s", phase, sh.Node())
	}
}

// onNode runs one phase and hands back what it said, for a caller that expects
// it to fail.
func onNode(
	ctx context.Context, sh *fabric.Shell, env map[string]string, phase string,
) (string, error) {
	return sh.Run(ctx, exportEnv(env)+growRemotePath+
		" -test.v -test.timeout=20m -test.run "+phase+"$")
}

// sanitizeForNQN reduces a case name to something an NQN can hold.
func sanitizeForNQN(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, name)
}

// tail is the last few lines of an output, for a log line that should not carry
// the whole of a test run.
func tail(out string, lines int) string {
	split := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(split) <= lines {
		return out
	}
	return strings.Join(split[len(split)-lines:], "\n")
}
