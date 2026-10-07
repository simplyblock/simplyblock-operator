// The kernel half of pNFS: whether a node's nfsd hands out SCSI layouts for an
// XFS filesystem on an NVMe-oF namespace, and whether a client given one writes
// to the namespace directly rather than through the server.
//
// It lives here rather than in the CSI e2e suite because the question is about
// the kernel the node boots, not about the driver. It needs no operator, no
// control plane, and no CSI: an nvmet target stands in for the storage, and the
// export is assembled by hand, the way the driver assembles it.

package suites

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

const (
	pnfsVolumeUUID = "9d3f1a52-6c0e-4b7a-8e21-5f4c3b2a1d09"
	pnfsNQN        = "nqn.2023-04.io.simplyblock:integration:pnfs:" + pnfsVolumeUUID
	// The NGUID is the identifier the layout carries, so it is spelled out
	// rather than left to nvmet, which sets none by default.
	pnfsNGUID  = "5c0a1d7e9b3f4e2a8d6c1b0f7e3a9d42"
	pnfsPort   = 4430
	pnfsPortID = 30

	// pnfsRoot is shared with the host, so the filesystem mounted under it is
	// the host's as much as the pod's (see fabric.WithSharedHostDir). It is
	// under /var/lib/kubelet because on Talos that is the one host directory
	// the kubelet both can write and mounts rshared, which a hostPath with
	// bidirectional propagation needs. /var/mnt is read-only to the kubelet.
	pnfsRoot   = "/var/lib/kubelet/sb-pnfs"
	pnfsExport = pnfsRoot + "/vol"

	pnfsClientMount = "/mnt/sb-pnfs"
	pnfsWriteMiB    = 64
)

// TestPNFS_SCSILayoutBypassesTheServer writes through a pNFS mount and asserts
// the data never crossed NFS: the client fetched a layout, and its NFS WRITE
// count did not move.
//
// A kernel whose nfsd cannot issue SCSI layouts serves the same export, mounts
// the same way, and accepts the same writes, all as plain NFS. The WRITE
// counter is the only thing that tells the two apart, which is why it is the
// assertion rather than a check on the plumbing.
func TestPNFS_SCSILayoutBypassesTheServer(t *testing.T) {
	requireIntegration(t)
	requirePNFS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Two nodes: a layout only means something to a client that is not the
	// server.
	c, nodes := leaseNodes(ctx, t, 2)
	mds, client := nodes[0], nodes[1]
	mdsIP := internalIP(ctx, t, c, mds)
	t.Logf("metadata server and target on %s (%s), client on %s", mds, mdsIP, client)

	// The server's shell is its own pod rather than a leased one: it carries
	// the shared directory, and a pod's volumes cannot be added to later.
	shMDS, err := fabric.NewShell(ctx, c, mds,
		fabric.WithImage(stackImage()), fabric.WithSharedHostDir(pnfsRoot))
	if err != nil {
		t.Fatalf("start the server shell: %v", err)
	}
	// Its pod only. Shell.Close deletes the whole manifest, and that carries
	// the namespace every leased shell lives in.
	t.Cleanup(func() {
		_, _ = c.Kubectl(context.WithoutCancel(ctx), "delete", "pod", "-n", fabric.Namespace,
			shMDS.Pod(), "--wait=false")
	})
	shClient := leaseShell(ctx, t, client, stackImage())

	requirePNFSTools(ctx, t, shMDS, "mkfs.xfs", "exportfs", "rpc.mountd", "rpc.nfsd")
	requirePNFSTools(ctx, t, shClient, "mount.nfs4")

	// Read, not loaded: Talos refuses a pod's modprobe, so the machine config
	// has to have loaded nfsd at boot.
	if out, err := shMDS.Run(ctx, "grep -q '^nfsd ' /proc/modules && echo loaded || echo absent"); err != nil ||
		strings.TrimSpace(out) != "loaded" {
		t.Fatalf("nfsd is not loaded on %s, so it cannot serve NFS at all; boot an image carrying "+
			"the nfsd extension with SB_TALOS_MODULES=nfsd: %v\n%s", mds, err, out)
	}

	tgt, err := fabric.NewTarget(ctx, shMDS, fabric.TargetSpec{
		NQN:       pnfsNQN,
		Model:     pnfsVolumeUUID,
		Serial:    "pnfs",
		CntlIDMin: 1, CntlIDMax: 999,
		Addr: mdsIP, Port: pnfsPort, PortID: pnfsPortID,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	t.Cleanup(func() { _ = tgt.Close(context.WithoutCancel(ctx)) })
	if err := tgt.AddNamespaceWith(ctx, 1, 1024, fabric.NamespaceOptions{
		UUID: pnfsVolumeUUID, NGUID: pnfsNGUID, Reservations: true,
	}); err != nil {
		t.Fatalf("add namespace: %v", err)
	}

	// The server reaches the namespace over the fabric like any client, as it
	// does in the product: nfsd identifies the device through the NVMe host
	// driver, which a loop device under nvmet does not go through.
	mdsDev := connectNamespace(ctx, t, shMDS, mdsIP)
	if out, err := shMDS.Run(ctx, strings.Join([]string{
		"set -e",
		"mkfs.xfs -q -f " + mdsDev,
		"mkdir -p " + pnfsExport,
		"mountpoint -q " + pnfsExport + " || mount " + mdsDev + " " + pnfsExport,
	}, "\n")); err != nil {
		t.Fatalf("make and mount the exported filesystem: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = shMDS.Run(context.WithoutCancel(ctx), "umount "+pnfsExport) })

	startNFSServer(ctx, t, shMDS)

	clientDev := connectNamespace(ctx, t, shClient, mdsIP)
	linkLayoutDevice(ctx, t, shClient, clientDev)

	if out, err := shClient.Run(ctx, fmt.Sprintf(
		"mkdir -p %[1]s && mount -t nfs4 -o vers=4.1,proto=tcp %[2]s:/ %[1]s",
		pnfsClientMount, mdsIP)); err != nil {
		t.Fatalf("mount the export over NFSv4.1: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = shClient.Run(context.WithoutCancel(ctx), "umount -f "+pnfsClientMount) })

	before := mountOps(ctx, t, shClient)
	if out, err := shClient.Run(ctx, fmt.Sprintf(
		"dd if=/dev/zero of=%s/data bs=1M count=%d conv=fsync 2>&1", pnfsClientMount, pnfsWriteMiB)); err != nil {
		t.Fatalf("write %d MiB through the mount: %v\n%s", pnfsWriteMiB, err, out)
	}
	after := mountOps(ctx, t, shClient)

	layouts := after["LAYOUTGET"] - before["LAYOUTGET"]
	writes := after["WRITE"] - before["WRITE"]
	t.Logf("%d MiB written: LAYOUTGET %+d, LAYOUTCOMMIT %+d, WRITE %+d",
		pnfsWriteMiB, layouts, after["LAYOUTCOMMIT"]-before["LAYOUTCOMMIT"], writes)

	if layouts == 0 {
		t.Errorf("the client fetched no layout, so the server issued none: an nfsd built without "+
			"CONFIG_NFSD_SCSILAYOUT, or a device without reservations or an NGUID\n%s",
			serverDiagnostics(ctx, shMDS))
	}
	if writes != 0 {
		t.Errorf("%d NFS WRITEs for %d MiB: the data went through the server, not to the namespace",
			writes, pnfsWriteMiB)
	}
}

// requirePNFS skips unless the run says its nodes boot a kernel with nfsd.
// The stock Image Factory image has no nfsd module at all, so this cannot be
// part of every run.
func requirePNFS(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_PNFS") == "" {
		t.Skip("set SB_PNFS=1 on a cluster booted from an image with the nfsd extension " +
			"(SB_TALOS_DISK_IMAGE, SB_TALOS_MODULES=nfsd); the factory image has no NFS server")
	}
}

// requirePNFSTools fails rather than skips: SB_PNFS asked for this spec, so a
// shell image without the tools is a misconfigured run, not an absent feature.
func requirePNFSTools(ctx context.Context, t *testing.T, sh *fabric.Shell, tools ...string) {
	t.Helper()
	if missing := absentTools(ctx, t, sh, tools); len(missing) > 0 {
		t.Fatalf("the shell image %s on %s is missing %v; set SB_STACK_IMAGE to a CSI image "+
			"built with nfs-utils and xfsprogs", stackImage(), sh.Node(), missing)
	}
}

// connectNamespace connects sh's node to the target and returns the namespace's
// head device.
func connectNamespace(ctx context.Context, t *testing.T, sh *fabric.Shell, addr string) string {
	t.Helper()
	init, err := fabric.NewInitiator(ctx, sh)
	if err != nil {
		t.Fatalf("prepare initiator on %s: %v", sh.Node(), err)
	}
	if err := init.Connect(ctx, pnfsNQN, addr, pnfsPort); err != nil {
		t.Fatalf("connect %s: %v", sh.Node(), err)
	}
	t.Cleanup(func() { _ = init.Disconnect(context.WithoutCancel(ctx), pnfsNQN) })
	return waitForHeadDevice(ctx, t, sh, pnfsNQN, 1)
}

// linkLayoutDevice makes the name a client resolves a SCSI layout through.
//
// The kernel opens /dev/disk/by-id/nvme-eui.<identifier> itself, with no
// userspace helper. The identifier is the NGUID nfsd put in the layout. udev
// creates the link on most hosts, but whether it does is the host's business,
// and a missing link fails the layout silently into plain NFS. So it is made
// here when absent, as the CSI driver makes it.
func linkLayoutDevice(ctx context.Context, t *testing.T, sh *fabric.Shell, dev string) {
	t.Helper()
	link := "/dev/disk/by-id/nvme-eui." + pnfsNGUID
	if out, err := sh.Run(ctx, fmt.Sprintf(
		"mkdir -p /dev/disk/by-id && { [ -e %[1]s ] || ln -s %[2]s %[1]s; } && readlink -f %[1]s",
		link, dev)); err != nil {
		t.Fatalf("link %s to %s: %v\n%s", link, dev, err, out)
	}
	t.Cleanup(func() {
		_, _ = sh.Run(context.WithoutCancel(ctx), fmt.Sprintf("[ -L %[1]s ] && rm -f %[1]s", link))
	})
}

// startNFSServer brings up nfsd for the one export, from the shell.
//
// Everything runs in the pod: Talos has no nfs-utils, and the kernel half only
// needs its control files written. Version 4 only, because 2 and 3 register
// with rpcbind, which Talos does not run, while 4 tolerates its absence.
//
// The export is the NFSv4 root (fsid=0), and the client mounts server:/.
// Under any other root nfsd walks a pseudo filesystem down from /, asking
// rpc.mountd about each directory on the way, and mountd answers from its own
// mount namespace: the pod's /, not the host's. Every answer misses, and the
// mount fails with ENOENT for a path that exists. As the root, the export is
// the only path mountd resolves, and the shared directory makes it the same
// filesystem in both namespaces.
//
// The lease and grace times are cut to ten seconds first. A freshly started
// server refuses new opens for its grace period, ninety seconds by default,
// and the client waits it out without saying so. They can only be set while no
// threads are running, so before rpc.nfsd.
func startNFSServer(ctx context.Context, t *testing.T, sh *fabric.Shell) {
	t.Helper()
	script := strings.Join([]string{
		"set -e",
		"mountpoint -q /proc/fs/nfsd || mount -t nfsd nfsd /proc/fs/nfsd",
		"mkdir -p /var/lib/nfs/rpc_pipefs",
		"mountpoint -q /var/lib/nfs/rpc_pipefs || mount -t rpc_pipefs sunrpc /var/lib/nfs/rpc_pipefs",
		"echo 10 > /proc/fs/nfsd/nfsv4leasetime",
		"echo 10 > /proc/fs/nfsd/nfsv4gracetime",
		fmt.Sprintf("printf '%%s\\n' '%s *(rw,sync,no_root_squash,no_subtree_check,insecure,pnfs,fsid=0)' > /etc/exports",
			pnfsExport),
		"exportfs -ra",
		"pgrep -x rpc.mountd >/dev/null || rpc.mountd --no-nfs-version 2 --no-nfs-version 3",
		// Client tracking. Without it nfsd still serves, and only reclaim after a
		// server restart is lost, which this spec never does.
		"if command -v nfsdcld >/dev/null; then pgrep -x nfsdcld >/dev/null || nfsdcld; fi",
		"rpc.nfsd --no-nfs-version 3 8",
		"cat /proc/fs/nfsd/threads",
	}, "\n")
	out, err := sh.Run(ctx, script)
	if err != nil {
		t.Fatalf("start the NFS server on %s: %v\n%s", sh.Node(), err, out)
	}
	if n, _ := strconv.Atoi(lastLine(out)); n == 0 {
		t.Fatalf("nfsd on %s reports no threads after rpc.nfsd:\n%s", sh.Node(), out)
	}
	t.Cleanup(func() {
		_, _ = sh.Run(context.WithoutCancel(ctx), strings.Join([]string{
			"rpc.nfsd 0",
			"exportfs -ua",
			"pkill -x rpc.mountd",
			"pkill -x nfsdcld",
			": > /etc/exports",
		}, "; "))
	})
}

// mountOps reads the client mount's per-operation counts from mountstats.
func mountOps(ctx context.Context, t *testing.T, sh *fabric.Shell) map[string]int {
	t.Helper()
	out, err := sh.Run(ctx, "cat /proc/self/mountstats")
	if err != nil {
		t.Fatalf("read mountstats on %s: %v\n%s", sh.Node(), err, out)
	}
	ops, err := parseMountOps(out, pnfsClientMount)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return ops
}

// parseMountOps returns the operation counts of the NFS mount at mountpoint:
// the first number on each line of its per-op statistics.
//
// mountstats lists every mount on the system, one "device ... mounted on ..."
// header each, and a count belongs to the header above it. Reading the
// counts without the header would add up every NFS mount on the node.
func parseMountOps(stats, mountpoint string) (map[string]int, error) {
	ops := map[string]int{}
	in, perOp, found := false, false, false
	for _, line := range strings.Split(stats, "\n") {
		if strings.HasPrefix(line, "device ") {
			in = strings.Contains(line, " mounted on "+mountpoint+" ")
			found = found || in
			perOp = false
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == "per-op statistics" {
			perOp = true
			continue
		}
		if !perOp {
			continue
		}
		name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("mountstats op %s: count %q is not a number", name, fields[0])
		}
		ops[name] = n
	}
	if !found {
		return nil, fmt.Errorf("mountstats has no mount on %s", mountpoint)
	}
	return ops, nil
}

// serverDiagnostics is what the server can say about why it issued no layout.
func serverDiagnostics(ctx context.Context, sh *fabric.Shell) string {
	out, _ := sh.Run(context.WithoutCancel(ctx), strings.Join([]string{
		"echo '--- exports (look for pnfs)'; cat /proc/fs/nfsd/exports 2>&1",
		"echo '--- kernel config'; (zcat /proc/config.gz 2>/dev/null | grep -E 'NFSD_(SCSI|BLOCK)LAYOUT') || echo 'no /proc/config.gz'",
		"echo '--- dmesg'; dmesg 2>/dev/null | grep -iE 'nfsd|pnfs|reservation' | tail -20",
	}, "; "))
	return out
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
