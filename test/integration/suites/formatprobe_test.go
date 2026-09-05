// The staging format probe against a real kernel: what atlas's blockdev prober
// reads from an nvmet-backed device that is healthy, blank, and — the reading
// the 2026-09-03 incident turned on — present with every path down. The unit
// tests in atlas script blkid's answers; this suite runs the real util-linux
// blkid against the real device states and pins that the scripted answers are
// the true ones.
package suites

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/blockdev"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

// probeShellImage carries util-linux and e2fsprogs. The default BusyBox shell
// cannot serve here: its blkid applet is not the binary whose exit-code
// contract is under test, and it has no mkfs.ext4 to put a real filesystem
// behind the fabric. Both packages are priority-required in Debian, so the
// slim image ships them without installing anything at test time.
const probeShellImage = "debian:bookworm-slim"

// The probed volume's identity, distinct from the defect suite's so the two
// can never be mistaken for each other in a shared snapshot.
const (
	probeVolumeUUID = "9d2e5a71-6c48-4f0b-8e37-1a5b9c4d2e60"
	probeBlankUUID  = "5f1a8c3d-2b7e-4a90-b6c4-8d0e7f3a1b52"
	probeNQN        = "nqn.2023-04.io.simplyblock:integration:" + probeVolumeUUID
	probePort       = 4420
)

// TestProbe_PathlessDeviceReadsAsBlank reproduces the device state the
// 2026-09-03 mkfs incident turned on and asserts what the format probe reads
// from it.
//
// Regression: 2026-09-03-blkid-exit2-conflation — the CSI node plugin probed a
// volume whose NVMe-oF paths were all down: the block device was still there,
// every read on it failed, and blkid folded that into the same exit code 2 a
// blank device produces. The driver read "no filesystem" and ran mkfs.ext4 -F
// over 1.1 TiB of data. The probe cannot repair blkid's conflation; what this
// suite pins, against a real kernel and the real util-linux blkid, is the
// conflation itself: a pathless device with a filesystem on it and a blank
// device are byte-identical readings, which is why the caller must settle ""
// from another source of truth before formatting, and why a kernel or blkid
// that ever starts distinguishing the two shows up here first.
func TestProbe_PathlessDeviceReadsAsBlank(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{Name: clusterNameFor("probe")})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Destroy(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("destroy cluster: %v", err)
		}
	})

	if err := c.WaitNodesReady(ctx, 1, 5*time.Minute); err != nil {
		t.Fatalf("nodes never became ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("list nodes: %v (%v)", err, nodes)
	}
	node := nodes[0]
	ip := internalIP(ctx, t, c, node)

	sh, err := fabric.NewShell(ctx, c, node, fabric.WithImage(probeShellImage))
	if err != nil {
		t.Fatalf("node shell: %v", err)
	}
	t.Cleanup(func() { _ = sh.Close(context.WithoutCancel(ctx)) })

	tgt, err := fabric.NewTarget(ctx, sh, fabric.TargetSpec{
		NQN:       probeNQN,
		Model:     probeVolumeUUID,
		Serial:    "single",
		CntlIDMin: 1, CntlIDMax: 999,
		Addr: ip, Port: probePort, PortID: 1,
		ANAState: "optimized",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	t.Cleanup(func() { _ = tgt.Close(context.WithoutCancel(ctx)) })

	// Namespace 1 carries a real ext4 filesystem; namespace 2 stays blank, so
	// the pathless reading can be compared against the genuinely empty one.
	if err := tgt.AddNamespace(ctx, 1, 64, probeVolumeUUID); err != nil {
		t.Fatalf("add data namespace: %v", err)
	}
	if err := tgt.AddNamespace(ctx, 2, 64, probeBlankUUID); err != nil {
		t.Fatalf("add blank namespace: %v", err)
	}

	backing := tgt.NamespaceDevice(1)
	if out, err := sh.Run(ctx, "mkfs.ext4 -q -F "+backing); err != nil {
		t.Fatalf("mkfs.ext4 on the target's backing device: %v\n%s", err, out)
	}
	wantUUID := filesystemUUID(ctx, t, sh, backing)
	if wantUUID == "" {
		t.Fatalf("the freshly made filesystem on %s has no UUID; the fixture is broken", backing)
	}
	t.Logf("namespace 1 backed by %s, ext4 UUID %s", backing, wantUUID)

	init, err := fabric.NewInitiator(ctx, sh)
	if err != nil {
		t.Fatalf("prepare initiator: %v", err)
	}
	t.Cleanup(func() { _ = init.Disconnect(context.WithoutCancel(ctx), probeNQN) })

	// ctrl_loss_tmo keeps the device node alive across the whole outage, and
	// fast_io_fail_tmo turns blocked reads into prompt failures — the same two
	// knobs the incident cluster ran with (3600) and the e2e spec sets, scaled
	// to a test's patience.
	if err := init.Connect(ctx, probeNQN, ip, probePort,
		"ctrl_loss_tmo=600", "fast_io_fail_tmo=2"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	dataDev := waitForHeadDevice(ctx, t, sh, probeNQN, 1)
	blankDev := waitForHeadDevice(ctx, t, sh, probeNQN, 2)
	prober := blockdev.NewBlkidProberWithRunner(shellRunner(sh))

	t.Run("a healthy device answers with its filesystem", func(t *testing.T) {
		waitFor(ctx, t, time.Minute, func() (string, bool) {
			fs, err := prober.Format(ctx, dataDev)
			return fmt.Sprintf("fs=%q err=%v", fs, err), err == nil && fs == "ext4"
		})
		fs, err := prober.Format(ctx, dataDev)
		if err != nil || fs != "ext4" {
			t.Fatalf("Format(%s) = %q, %v, want ext4 over the fabric\n%s",
				dataDev, fs, err, init.Describe(ctx))
		}
	})

	t.Run("a blank device answers with nothing", func(t *testing.T) {
		fs, err := prober.Format(ctx, blankDev)
		if err != nil || fs != "" {
			t.Fatalf("Format(%s) = %q, %v, want \"\" for a never-formatted namespace", blankDev, fs, err)
		}
	})

	if err := tgt.UnlinkPort(ctx); err != nil {
		t.Fatalf("withdraw the subsystem from its port: %v", err)
	}

	t.Run("a device with no serving path is byte-identical to the blank one", func(t *testing.T) {
		// The premise first: the block device must still exist. A vanished
		// device also probes blank, but for a reason staging can see (the open
		// fails), so it would prove nothing about the conflation.
		waitFor(ctx, t, 2*time.Minute, func() (string, bool) {
			present := devicePresent(ctx, sh, dataDev)
			fs, err := prober.Format(ctx, dataDev)
			return fmt.Sprintf("present=%v fs=%q err=%v", present, fs, err),
				present && err == nil && fs == ""
		})
		if !devicePresent(ctx, sh, dataDev) {
			t.Fatalf("the device node went away; the kernel no longer holds the state under test\n%s",
				init.Describe(ctx))
		}
		fs, err := prober.Format(ctx, dataDev)
		if err != nil || fs != "" {
			t.Fatalf("Format(%s) with every path down = %q, %v, want the blank reading (\"\", nil): "+
				"this is the conflation that formatted a data-bearing volume on 2026-09-03, "+
				"and the reason a \"\" probe must never be settled by mkfs alone\n%s",
				dataDev, fs, err, init.Describe(ctx))
		}
	})

	if err := tgt.RelinkPort(ctx); err != nil {
		t.Fatalf("restore the subsystem on its port: %v", err)
	}

	t.Run("the filesystem was there all along", func(t *testing.T) {
		waitFor(ctx, t, 3*time.Minute, func() (string, bool) {
			fs, err := prober.Format(ctx, dataDev)
			return fmt.Sprintf("fs=%q err=%v", fs, err), err == nil && fs == "ext4"
		})
		fs, err := prober.Format(ctx, dataDev)
		if err != nil || fs != "ext4" {
			t.Fatalf("Format(%s) after the path returned = %q, %v, want ext4 again\n%s",
				dataDev, fs, err, init.Describe(ctx))
		}
		if got := filesystemUUID(ctx, t, sh, dataDev); got != wantUUID {
			t.Fatalf("filesystem UUID after the outage = %q, want %q: something wrote to the device",
				got, wantUUID)
		}
	})
}

// shellRunner adapts a node shell to blockdev's Runner: the probe's command
// runs inside the shell pod on the node that holds the device, with the remote
// exit code carried back on a marker line so kubectl's own exit stays zero and
// the code survives the transport.
//
// This drives the blkid probe rather than the content reading, which is what
// this suite is about: what it pins is blkid's own answer on a device with no
// serving path, the reading the driver used to act on. Driving the content
// reading here needs a shell-backed Reader doing bounded reads rather than a
// command runner, and it belongs with the suite that asserts the new readings.
func shellRunner(sh *fabric.Shell) blockdev.Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
		words := make([]string, 0, len(args)+1)
		for _, w := range append([]string{name}, args...) {
			words = append(words, shellQuote(w))
		}
		out, err := sh.Run(ctx, strings.Join(words, " ")+`; echo "__rc=$?"`)
		if err != nil {
			return []byte(out), 0, err
		}
		idx := strings.LastIndex(out, "__rc=")
		if idx < 0 {
			return []byte(out), 0, fmt.Errorf("no exit-code marker in shell output: %q", out)
		}
		rc, convErr := strconv.Atoi(strings.TrimSpace(out[idx+len("__rc="):]))
		if convErr != nil {
			return []byte(out), 0, fmt.Errorf("unparsable exit-code marker in shell output: %q", out)
		}
		return []byte(out[:idx]), rc, nil
	}
}

// shellQuote makes one value safe as a single shell word, the same way the
// fabric package quotes what it interpolates into scripts.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// devicePresent reports whether path is a block device on the shell's node.
func devicePresent(ctx context.Context, sh *fabric.Shell, path string) bool {
	out, err := sh.Run(ctx, fmt.Sprintf("[ -b %s ] && echo present || echo absent", shellQuote(path)))
	return err == nil && strings.Contains(out, "present")
}

// waitForHeadDevice resolves the multipath head device of one namespace of the
// subsystem, straight from sysfs, and fails the test when it never appears.
func waitForHeadDevice(ctx context.Context, t *testing.T, sh *fabric.Shell, nqn string, nsid int) string {
	t.Helper()
	script := strings.Join([]string{
		"NQN=" + shellQuote(nqn),
		`for s in /sys/class/nvme-subsystem/nvme-subsys*; do`,
		`	[ -f "$s"/subsysnqn ] || continue`,
		`	[ "$(cat "$s"/subsysnqn)" = "$NQN" ] || continue`,
		`	for n in "$s"/nvme*n*; do`,
		`		[ -f "$n"/nsid ] || continue`,
		// A subsystem directory holds the head namespace and also one entry per
		// controller path, and both carry the namespace id. The per-path ones are
		// named nvmeXcYnZ, so the c is what tells them apart, and taking them all
		// would return several names for one namespace.
		`		case "$(basename "$n")" in *c*n*) continue ;; esac`,
		fmt.Sprintf(`		if [ "$(cat "$n"/nsid)" = %d ]; then basename "$n"; fi`, nsid),
		`	done`,
		`done`,
		// Whatever the last namespace examined turned out to be, the scan itself
		// succeeded. Without this the exit status is the last comparison's, so a
		// subsystem whose final entry is a different namespace reports failure
		// while having printed the right answer, and the transport folds its own
		// complaint into the output.
		`exit 0`,
	}, "\n")

	var device, last string
	waitFor(ctx, t, time.Minute, func() (string, bool) {
		out, err := sh.Run(ctx, script)
		last = out
		if err != nil {
			return out, false
		}
		name := strings.TrimSpace(out)
		if name == "" || strings.ContainsAny(name, " \n") {
			return out, false
		}
		device = "/dev/" + name
		return out, true
	})
	if device == "" {
		t.Fatalf("namespace %d of %s never produced a head device; the scan last saw %q",
			nsid, nqn, strings.TrimSpace(last))
	}
	return device
}

// filesystemUUID is the UUID of the filesystem on device, or "" when there is
// none to read — the identity a reformat cannot preserve.
func filesystemUUID(ctx context.Context, t *testing.T, sh *fabric.Shell, device string) string {
	t.Helper()
	out, err := sh.Run(ctx, "blkid -p -s UUID -o export "+shellQuote(device)+" || true")
	if err != nil {
		t.Fatalf("read filesystem UUID of %s: %v\n%s", device, err, out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "UUID="); ok {
			return v
		}
	}
	return ""
}

// internalIP is the node's InternalIP: what a target advertises and an
// initiator dials. The defect suite's nodeIPs insists on two nodes; this
// resolves one.
func internalIP(ctx context.Context, t *testing.T, c *cluster.Cluster, node string) string {
	t.Helper()
	out, err := c.Kubectl(ctx, "get", "node", node, "-o",
		`jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("internal IP of %s: %v (%q)", node, err, out)
	}
	return strings.TrimSpace(out)
}
