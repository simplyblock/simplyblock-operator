// What spec.pnfs puts on the node plugin.
//
// pNFS makes the node plugin an NFS metadata server as well as an initiator: it
// makes a filesystem on the namespace, mounts it, and publishes it through the
// host's nfsd. Both of those cross the container boundary, and the tests here
// are about that crossing rather than about the export logic, which lives in
// atlas and is tested there.

package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func pnfsDriver() *simplyblockv1alpha2.SimplyblockDriver {
	d := testDriver("simplyblock")
	d.Spec.Link.EnableLink = ptr.To(true)
	d.Spec.PNFS.EnablePNFS = ptr.To(true)
	return d
}

func TestPNFSOffAddsNothing(t *testing.T) {
	pod := nodeDaemonSet(testDriver("simplyblock"), testImage).Spec.Template.Spec
	for _, name := range []string{exportsVolumeName, exportRootVolumeName} {
		if volumeNamed(pod.Volumes, name) != nil {
			t.Errorf("the node plugin carries %s with pNFS off", name)
		}
	}
}

// nfsd reads the export table from the host's own /etc/exports.d, and exportfs
// is what tells it to re-read. A drop-in written inside the container reaches
// nothing, and the export silently does not exist.
func TestPNFSMountsTheHostExportsDirectory(t *testing.T) {
	pod := nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec

	v := volumeNamed(pod.Volumes, exportsVolumeName)
	if v == nil || v.HostPath == nil {
		t.Fatalf("the exports volume is not a hostPath: %+v", v)
	}
	if v.HostPath.Path != exportsDir {
		t.Errorf("exports hostPath = %q, want %q", v.HostPath.Path, exportsDir)
	}
	if v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("exports hostPath type = %v; a host with no exports.d yet must still serve", v.HostPath.Type)
	}

	c := containerNamed(pod.Containers, "csi-node")
	m := mountNamed(c.VolumeMounts, exportsVolumeName)
	if m == nil {
		t.Fatal("the node plugin does not mount the exports directory")
	}
	if m.MountPath != exportsDir {
		t.Errorf("exports mount path = %q, want %q; the driver writes to a fixed path", m.MountPath, exportsDir)
	}
	if m.ReadOnly {
		t.Error("the exports directory is mounted read-only; the plugin writes drop-ins into it")
	}
}

// The export mount has to land in the host's mount namespace, because nfsd
// serves the host's view. Without bidirectional propagation the plugin mounts
// the filesystem into its own namespace, exportfs then exports an empty
// directory, and a client mounts something that looks like an empty volume.
func TestPNFSPropagatesTheExportMountToTheHost(t *testing.T) {
	pod := nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec

	v := volumeNamed(pod.Volumes, exportRootVolumeName)
	if v == nil || v.HostPath == nil || v.HostPath.Path != exportRoot {
		t.Fatalf("the export root is not the host's %s: %+v", exportRoot, v)
	}

	c := containerNamed(pod.Containers, "csi-node")
	m := mountNamed(c.VolumeMounts, exportRootVolumeName)
	if m == nil {
		t.Fatal("the node plugin does not mount the export root")
	}
	if m.MountPath != exportRoot {
		t.Errorf("export root mount path = %q, want %q", m.MountPath, exportRoot)
	}
	if m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationBidirectional {
		t.Errorf("export root propagation = %v, want Bidirectional; nfsd serves the host's mounts",
			m.MountPropagation)
	}
}

// pNFS is a node-side concern. The controller plugin creates the record and
// never assembles anything, so giving it the host's exports directory would be
// access it has no use for.
func TestPNFSDoesNotReachTheControllerPlugin(t *testing.T) {
	pod := controllerStatefulSet(pnfsDriver(), testImage).Spec.Template.Spec
	for _, name := range []string{exportsVolumeName, exportRootVolumeName} {
		if volumeNamed(pod.Volumes, name) != nil {
			t.Errorf("the controller plugin carries %s", name)
		}
	}
}

// exportfs has to run in the host's mount namespace, and the plugin reaches it
// through the host's init process, so the pod shares the host's PID namespace.
//
// It is not an extra privilege so much as a way of using one this pod already
// has: it is privileged, with SYS_ADMIN, the host's network, and /dev and /sys
// mounted, so what this adds is visibility of host processes rather than any
// new power over the machine. It is still gated, because a node plugin that is
// not serving exports has no use for it.
func TestPNFSSharesTheHostProcessNamespace(t *testing.T) {
	pod := nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec
	if !pod.HostPID {
		t.Error("the node plugin does not share the host PID namespace, so it cannot reach " +
			"the host's mount namespace and exportfs would edit a table nothing serves from")
	}
}

func TestPNFSOffLeavesTheProcessNamespaceAlone(t *testing.T) {
	pod := nodeDaemonSet(testDriver("simplyblock"), testImage).Spec.Template.Spec
	if pod.HostPID {
		t.Error("the node plugin shares the host PID namespace with pNFS off")
	}
}

// The controller plugin never runs exportfs.
func TestPNFSDoesNotGiveTheControllerTheHostProcessNamespace(t *testing.T) {
	pod := controllerStatefulSet(pnfsDriver(), testImage).Spec.Template.Spec
	if pod.HostPID {
		t.Error("the controller plugin shares the host PID namespace")
	}
}
