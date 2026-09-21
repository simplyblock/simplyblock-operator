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

// The node plugin never shares the host's PID namespace, with pNFS on or off.
//
// It does not need to: the mount reaches the node through bidirectional
// propagation on the export root, and exportfs reaches the host's nfsd through
// the two directories it keeps state in. Entering the host's PID namespace
// would let this pod see and signal every process on the node, which the Pod
// Security Standards refuse outright and an audit reads as an escape primitive.
func TestPNFSNeverSharesTheHostProcessNamespace(t *testing.T) {
	for name, pod := range map[string]corev1.PodSpec{
		"node, pNFS on":  nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec,
		"node, pNFS off": nodeDaemonSet(testDriver("simplyblock"), testImage).Spec.Template.Spec,
		"controller":     controllerStatefulSet(pnfsDriver(), testImage).Spec.Template.Spec,
	} {
		if pod.HostPID {
			t.Errorf("%s shares the host PID namespace", name)
		}
	}
}

// The state exportfs works through is the host's, mounted in. Against this
// container's own copies the export would report success and be invisible to
// every client, which is the worst failure available here.
func TestPNFSMountsTheHostNFSState(t *testing.T) {
	pod := nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec
	c := containerNamed(pod.Containers, "csi-node")

	for _, path := range []string{nfsStateDir, procFSDir} {
		if mountAtPath(c.VolumeMounts, path) == nil {
			t.Errorf("the node plugin does not mount %s, so exportfs would write a copy nothing reads", path)
		}
	}
}

// /proc/fs is the host's and always exists, so it is required rather than
// created. /proc/fs/nfsd itself is deliberately NOT mounted: it appears only
// once the nfsd module is loaded, and requiring it would stop the plugin
// starting on exactly the nodes it is there to bring up.
func TestPNFSRequiresProcFSRatherThanCreatingIt(t *testing.T) {
	volumes := nodeDaemonSet(pnfsDriver(), testImage).Spec.Template.Spec.Volumes

	for _, name := range []string{procFSVolumeName} {
		v := volumeNamed(volumes, name)
		if v == nil || v.HostPath == nil {
			t.Fatalf("%s is not a host path", name)
		}
		if v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathDirectory {
			t.Errorf("%s is %v, want Directory so a host without nfsd fails visibly",
				name, v.HostPath.Type)
		}
	}
}

func mountAtPath(mounts []corev1.VolumeMount, path string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].MountPath == path {
			return &mounts[i]
		}
	}
	return nil
}
