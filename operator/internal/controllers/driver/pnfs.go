// What spec.pnfs puts on the node plugin: the host directories an NFS metadata
// server cannot do without.
//
// All of them are about the container boundary, not about NFS. Assembly writes
// a drop-in the host's nfsd reads, runs exportfs against that nfsd, and mounts
// a filesystem nfsd then serves. Done against this container's own copies, the
// drop-in reaches nothing and the mount leaves exportfs publishing an empty
// directory -- the worse failure, because a client mounts it and sees a volume
// that merely looks empty.
//
// Deliberately no hostPID. The mount reaches the node through bidirectional
// propagation on the export root, which is how every CSI driver does it, and
// exportfs reaches the host's nfsd through the two directories it keeps state
// in. Entering the host's PID namespace would also let this pod see and signal
// every process on the node, which is a far larger grant than the four paths
// below and one the Pod Security Standards refuse outright.
//
// Nothing here checks the host can actually serve. nfsd and nfs-utils are the
// node OS's to provide, and a host without them fails visibly in the export's
// Assembling phase; a client without blkmapd is refused at stage time.
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

	// The parent, not /proc/fs/nfsd itself: that directory appears only once
	// the nfsd module is loaded, so requiring it would stop the plugin
	// starting on exactly the nodes it is there to bring up. Mounted
	// bidirectionally so the nfsd filesystem this plugin mounts is the host's.
	procFSDir        = "/proc/fs"
	procFSVolumeName = "host-proc-fs"
)

// pnfsEnabled applies the CRD's default.
func pnfsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.PNFS.EnablePNFS != nil && *d.Spec.PNFS.EnablePNFS
}

// pnfsVolumes are the two host directories, or none when pNFS is off.
func pnfsVolumes(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.Volume {
	if !pnfsEnabled(d) {
		return nil
	}
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	dir := corev1.HostPathDirectory
	return []corev1.Volume{
		// DirectoryOrCreate: a host that has never served an export has
		// neither.
		hostPathVolume(exportsVolumeName, exportsDir, &dirOrCreate),
		hostPathVolume(exportRootVolumeName, exportRoot, &dirOrCreate),
		// /proc/fs is Directory because it always exists and creating it would
		// mean something is very wrong; /var/lib/nfs is DirectoryOrCreate
		// because nfs-utils makes it on first use.
		hostPathVolume(nfsStateVolumeName, nfsStateDir, &dirOrCreate),
		hostPathVolume(procFSVolumeName, procFSDir, &dir),
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
		{Name: procFSVolumeName, MountPath: procFSDir, MountPropagation: &bidirectional},
	}
}
