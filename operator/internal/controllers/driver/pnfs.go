// What spec.pnfs puts on the node plugin: the two host directories an NFS
// metadata server cannot do without.
//
// Both are about the container boundary rather than about NFS. The export logic
// is atlas/export, reached over the link, and it does three things that have to
// be true of the host and not of the pod: it writes a drop-in that the host's
// nfsd reads, it runs exportfs against that same nfsd, and it mounts a
// filesystem that nfsd then serves. A drop-in written inside the container
// reaches nothing, and a mount made inside it leaves exportfs publishing an
// empty directory -- which is the worse failure, because a client mounts it and
// sees a volume that merely looks empty.
//
// The pod also shares the host's PID namespace, which is how the plugin reaches
// the host's mount namespace: exportfs writes /var/lib/nfs/etab and pokes
// /proc/fs/nfsd, and rpc.mountd reads the same files, so run in the container's
// own namespace it would edit a table nothing serves from and the export would
// report success while being invisible to every client. It is less of an
// escalation than it reads as -- this pod is already privileged with SYS_ADMIN,
// the host's network, and /dev and /sys -- but it is still gated, because a node
// plugin not serving exports has no use for it.
//
// Nothing here is conditional on the host actually being able to serve. nfsd
// and nfs-utils are the node OS's to provide (design-pnfs-rwx.md §14.1, P0-10),
// and a host without them fails in the export record's Assembling phase, where
// the reason is visible, rather than here.

package driver

import (
	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// exportsDir is the drop-in directory nfsd reads, and the path the driver
	// writes to. It is the same string on both sides of the mount because the
	// driver has it as a constant (csi-driver/internal/nfsexport): a mount path
	// that could differ from it would be a way to configure the plugin into
	// writing somewhere nothing reads.
	exportsDir        = "/etc/exports.d"
	exportsVolumeName = "host-exports"

	// exportRoot is where an export's filesystem is mounted. The controller
	// builds every export path under it (csi-driver/internal/csi/controller's
	// exportPathFor), so one volume covers every export the host serves rather
	// than one per export, which a pod could not grow anyway.
	exportRoot           = "/mnt"
	exportRootVolumeName = "host-export-root"
)

// pnfsEnabled applies the CRD's default. The link it also requires is enforced
// at admission, so this does not check it again: a builder that silently
// dropped the volumes for an object the apiserver admitted would be harder to
// diagnose than the pod failing to serve.
func pnfsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.PNFS.EnablePNFS != nil && *d.Spec.PNFS.EnablePNFS
}

// pnfsVolumes are the two host directories, or none when pNFS is off.
func pnfsVolumes(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.Volume {
	if !pnfsEnabled(d) {
		return nil
	}
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	return []corev1.Volume{
		// DirectoryOrCreate for both: a host that has never served an export
		// has neither, and refusing to start until somebody makes them is a
		// worse first run than making them.
		hostPathVolume(exportsVolumeName, exportsDir, &dirOrCreate),
		hostPathVolume(exportRootVolumeName, exportRoot, &dirOrCreate),
	}
}

// pnfsVolumeMounts puts them on the node plugin. The export root propagates
// bidirectionally, which is what makes the mount the plugin creates visible to
// the nfsd serving it.
func pnfsVolumeMounts(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.VolumeMount {
	if !pnfsEnabled(d) {
		return nil
	}
	bidirectional := corev1.MountPropagationBidirectional
	return []corev1.VolumeMount{
		{Name: exportsVolumeName, MountPath: exportsDir},
		{Name: exportRootVolumeName, MountPath: exportRoot, MountPropagation: &bidirectional},
	}
}
