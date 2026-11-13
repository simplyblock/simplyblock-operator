// The nvme-tcp-module init container on the node plugin, which
// spec.enableNVMeTCPModuleInstall adds: it loads the NVMe/TCP initiator on a
// worker whose kernel does not load it by itself, and on Ubuntu installs the
// package that carries it. It lives beside the DaemonSet builder because it is
// part of the node plugin's pod and of nothing else.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1.

package driver

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	nvmeTCPModuleContainerName = "nvme-tcp-module"

	// hostRootVolumeName is the host's / as the init container mounts it.
	// Entering it with chroot needs no host PID namespace, which on a pod
	// would stay shared with the plugin containers for the pod's whole life.
	hostRootVolumeName = "host-root"
	hostRootMountPath  = "/host"
)

// nvmeTCPGuardScript runs in the driver image and decides whether to enter the
// host at all. Only an Ubuntu host is entered: the install below needs its
// /bin/sh and apt. Any other host, including one without a shell such as
// Talos, is left alone and the container exits 0, so a mixed fleet does not
// hold its other workers in Init. The node plugin's own start-up check then
// reports a module that is still missing. HOST_ROOT exists for the unit test.
const nvmeTCPGuardScript = `set -eu
host="${HOST_ROOT:-/host}"
id=""
if [ -r "$host/etc/os-release" ]; then
  id=$(. "$host/etc/os-release" && printf '%s' "${ID:-}") || id=""
fi
if [ "$id" = ubuntu ]; then
  exec chroot "$host" /bin/sh -c "$1"
fi
if [ -d /sys/module/nvme_tcp ]; then
  echo "[INFO] nvme-tcp is loaded on this ${id:-unidentified} host"
else
  echo "[WARN] nvme-tcp is not loaded, and it is installed automatically only on Ubuntu, not on this ${id:-unidentified} host"
fi
`

// nvmeTCPUbuntuScript runs inside the host under its own /bin/sh, which is
// dash, so it is POSIX sh. Ubuntu's cloud kernels ship nvme-tcp in
// linux-modules-extra, and some (the AWS kernels) in linux-modules, so the
// module is tried before any package. The install is tried before an apt
// update, and waits for the dpkg lock that cloud-init or unattended-upgrades
// often holds on a fresh node. modprobe's own error reaches the log.
//
// While the kernel is unchanged, a restart finds the module and never reaches
// apt. After a kernel upgrade and a reboot the new kernel's package is
// missing again, and the node plugin's start waits on the package mirror once.
const nvmeTCPUbuntuScript = `set -eu
export DEBIAN_FRONTEND=noninteractive
pkg="linux-modules-extra-$(uname -r)"
if ! modprobe nvme-tcp; then
  apt-get -o DPkg::Lock::Timeout=120 install -y "$pkg" ||
    { apt-get -o DPkg::Lock::Timeout=120 update && apt-get -o DPkg::Lock::Timeout=120 install -y "$pkg"; }
  modprobe nvme-tcp
fi
if [ ! -f /etc/modules-load.d/nvme-tcp.conf ]; then
  echo nvme-tcp > /etc/modules-load.d/nvme-tcp.conf
fi
echo "[INFO] nvme-tcp is loaded and listed in /etc/modules-load.d/nvme-tcp.conf"
`

func nvmeTCPModuleInstall(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.EnableNVMeTCPModuleInstall != nil && *d.Spec.EnableNVMeTCPModuleInstall
}

// nvmeTCPModuleContainer runs in the driver's own image, which carries sh and
// chroot, under the driver's pull policy, so it pulls from wherever the plugin
// does and a mirrored registry needs nothing more.
func nvmeTCPModuleContainer(image string, pull corev1.PullPolicy) corev1.Container {
	return corev1.Container{
		Name:            nvmeTCPModuleContainerName,
		Image:           image,
		ImagePullPolicy: pull,
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Command: []string{
			"/bin/sh", "-c", nvmeTCPGuardScript, nvmeTCPModuleContainerName, nvmeTCPUbuntuScript,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: hostRootVolumeName, MountPath: hostRootMountPath},
		},
	}
}

func hostRootVolume() corev1.Volume {
	return corev1.Volume{
		Name: hostRootVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/"},
		},
	}
}
