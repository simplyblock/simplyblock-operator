// What spec.pnfs puts on the node plugin: the host directories an NFS metadata
// server cannot do without.
//
// All three are about the container boundary rather than about NFS. Against
// this container's own copies the drop-in reaches no nfsd and exportfs
// publishes an empty directory, which is the worse failure: a client mounts it
// and sees a volume that merely looks empty.
//
// nfsd's own control filesystem is not among them, and needs nothing here. It
// is keyed by network namespace rather than by mount namespace, and the plugin
// runs with hostNetwork, so the /proc/fs/nfsd it mounts for itself already
// drives the host's nfsd. A hostPath would not work anyway: runc refuses every
// bind mount whose target is inside the container's /proc.
//
// Deliberately no hostPID. The mount reaches the node through bidirectional
// propagation and exportfs through the state directories, so the PID namespace
// buys nothing, and the Pod Security Standards refuse it outright.
//
// Nothing here checks the host can serve. A missing nfsd fails visibly in the
// export's Assembling phase, but a client missing blkmapd is checked nowhere
// and degrades silently to metadata-server-routed I/O.
package driver

import (
	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// The drop-in directory nfsd reads. The same string on both sides of the
	// mount, because the driver has it as a constant: a path that could differ
	// would configure the plugin into writing where nothing reads.
	exportsDir        = "/etc/exports.d"
	exportsVolumeName = "host-exports"

	// Where an export's filesystem is mounted. Every export path is built under
	// it, so one volume covers every export the host serves.
	//
	// Under /var/lib/simplyblock rather than the host's /mnt: /mnt is a
	// general-purpose directory an administrator may be using for anything,
	// and a storage driver that makes directories there is one nobody can
	// reason about. This path is obviously the product's.
	exportRoot           = "/var/lib/simplyblock/exports"
	exportRootVolumeName = "host-export-root"

	// The state exportfs works through: nfs-utils keeps the export table here,
	// and rpc.mountd on the host reads the same file, so it is the host's
	// rather than this container's -- that is what makes an export published
	// here one the host actually serves.
	nfsStateDir        = "/var/lib/nfs"
	nfsStateVolumeName = "host-nfs-state"
)

// pnfsEnabled applies the CRD's default.
func pnfsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.PNFS.EnablePNFS != nil && *d.Spec.PNFS.EnablePNFS
}

// pnfsVolumes are the three host directories, or none when pNFS is off.
//
// All DirectoryOrCreate: a host that has never served an export has none of
// them, and nfs-utils makes its state directory on first use.
func pnfsVolumes(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.Volume {
	if !pnfsEnabled(d) {
		return nil
	}
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	return []corev1.Volume{
		hostPathVolume(exportsVolumeName, exportsDir, &dirOrCreate),
		hostPathVolume(exportRootVolumeName, exportRoot, &dirOrCreate),
		hostPathVolume(nfsStateVolumeName, nfsStateDir, &dirOrCreate),
	}
}

// pnfsVolumeMounts puts them on the node plugin. The export root propagates
// bidirectionally, so the mount the plugin makes is visible to nfsd.
func pnfsVolumeMounts(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.VolumeMount {
	if !pnfsEnabled(d) {
		return nil
	}
	bidirectional := corev1.MountPropagationBidirectional
	return []corev1.VolumeMount{
		{Name: exportsVolumeName, MountPath: exportsDir},
		{Name: exportRootVolumeName, MountPath: exportRoot, MountPropagation: &bidirectional},
		{Name: nfsStateVolumeName, MountPath: nfsStateDir},
	}
}
