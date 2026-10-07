// The QEMU command line that starts the pNFS metadata server's guest.
//
// The guest is the pnfs-os image (vela-os, prototype/boards/pnfs-common), and
// the devices, their order, and the kernel command line built here are the
// contract that image boots against: the read-only root disk is the first
// virtio disk, the state disk carries the serial its fstab mounts by, and the
// guest's address arrives as an `ip=` argument because the guest runs no DHCP
// client. A change on either side is a change to both.
//
// It lives apart from the runner so the command line is a pure function of its
// inputs and can be tested without KVM, a tap device, or a guest. The
// technique follows neonvm-runner in the autoscaling repository (direct kernel
// boot, virtio devices, a virtio console on stdio), without its API.

package qemu

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Arch is a guest architecture, named as Go names it, so a runner passes
// runtime.GOARCH: the guest always matches the node it runs on, because KVM
// cannot run a foreign architecture.
type Arch string

const (
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
)

// StateDiskSerial is the virtio serial of the state disk. The guest mounts
// /dev/disk/by-id/virtio-pnfs-state at /var/lib/nfs.
const StateDiskSerial = "pnfs-state"

// MinMemoryMiB is the least memory the guest boots with: systemd, nfsd, and the
// agent need it.
const MinMemoryMiB = 256

// Guest is everything the command line is built from.
type Guest struct {
	Arch Arch

	// KernelPath is the guest kernel, booted directly: bzImage on amd64, the
	// uncompressed `Image` on ARM64.
	KernelPath string
	// FirmwarePath is the UEFI firmware an ARM64 guest needs for ACPI. It must
	// be empty on amd64.
	FirmwarePath string

	// RootDiskPath is the image's qcow2 root filesystem, attached read-only.
	RootDiskPath string
	// StateDiskPath is the state PVC's raw block device.
	StateDiskPath string

	CPUs      int
	MemoryMiB int

	// TapName is the host tap device the guest's only NIC is attached to.
	TapName string
	// VhostNet moves the NIC's data path into the host kernel. It needs
	// /dev/vhost-net in the pod, which is optional.
	VhostNet bool
	MAC      net.HardwareAddr

	// Address is the guest's IPv4 address and prefix on the runner's bridge,
	// and Gateway the bridge's own address.
	Address netip.Prefix
	Gateway netip.Addr
	// Hostname must be stable across restarts: nfsd derives its server owner
	// and scope from it, and NFSv4.1 clients only reclaim their state from a
	// server whose identity did not change.
	Hostname string

	// QMPSocket is the Unix socket the runner drives QEMU through.
	QMPSocket string
}

// Binary returns the QEMU system emulator for the guest's architecture.
func (g Guest) Binary() (string, error) {
	switch g.Arch {
	case ArchAMD64:
		return "qemu-system-x86_64", nil
	case ArchARM64:
		return "qemu-system-aarch64", nil
	default:
		return "", fmt.Errorf("unsupported guest architecture %q", g.Arch)
	}
}

// Args returns QEMU's arguments, or an error naming the first input that
// cannot start a guest.
func (g Guest) Args() ([]string, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}

	args := []string{
		"-machine", machineType(g.Arch),
		"-enable-kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(g.CPUs),
		"-m", strconv.Itoa(g.MemoryMiB) + "M",
		"-nodefaults",
		"-no-user-config",
		"-nographic",
		"-no-reboot",
		"-msg", "timestamp=on",
		"-qmp", "unix:" + g.QMPSocket + ",server=on,wait=off",
	}
	if g.Arch == ArchARM64 {
		args = append(args, "-bios", g.FirmwarePath)
	}

	args = append(args,
		// The console is the guest's only output, and the runner's stdout is
		// the pod log. signal=off keeps a stray ^C on stdin from killing QEMU:
		// stopping the guest is the runner's decision, made over QMP.
		"-chardev", "stdio,id=console,signal=off",
		"-device", "virtio-serial-pci,id=virtio-serial0",
		"-device", "virtconsole,chardev=console",

		// Order is the contract: the root disk is the first virtio-blk device
		// and enumerates as /dev/vda. The root image sits on the container's
		// filesystem, where O_DIRECT is not guaranteed, so it keeps QEMU's
		// default cache. The state disk is a block device and bypasses it.
		"-drive", "id=root,file="+g.RootDiskPath+",format=qcow2,if=none,readonly=on",
		"-device", "virtio-blk-pci,drive=root",
		"-drive", "id=state,file="+g.StateDiskPath+",format=raw,if=none,cache=none",
		"-device", "virtio-blk-pci,drive=state,serial="+StateDiskSerial,

		"-netdev", g.netdev(),
		"-device", "virtio-net-pci,netdev=net0,mac="+g.MAC.String(),

		"-kernel", g.KernelPath,
		"-append", g.KernelCmdline(),
	)
	return args, nil
}

// KernelCmdline returns the guest kernel's command line.
//
// The address is configured by the kernel itself (`ip=`, `CONFIG_IP_PNP`) before
// init runs, on eth0, which the guest keeps by not renaming interfaces. panic=-1
// turns a guest panic into an immediate reboot, which -no-reboot turns into
// QEMU exiting.
func (g Guest) KernelCmdline() string {
	parts := []string{
		"root=/dev/vda", "ro", "console=hvc0", "panic=-1",
		fmt.Sprintf("ip=%s::%s:%s:%s:eth0:off",
			g.Address.Addr(), g.Gateway, netmask(g.Address), g.Hostname),
	}
	if g.Arch == ArchARM64 {
		parts = append(parts, "acpi=on")
	}
	return strings.Join(parts, " ")
}

func (g Guest) netdev() string {
	netdev := "tap,id=net0,ifname=" + g.TapName + ",script=no,downscript=no"
	if g.VhostNet {
		netdev += ",vhost=on"
	}
	return netdev
}

func (g Guest) validate() error {
	if _, err := g.Binary(); err != nil {
		return err
	}
	switch {
	case g.Arch == ArchARM64 && g.FirmwarePath == "":
		return errors.New("an arm64 guest needs UEFI firmware for ACPI")
	case g.Arch == ArchAMD64 && g.FirmwarePath != "":
		return errors.New("an amd64 guest boots without firmware, got " + g.FirmwarePath)
	case g.KernelPath == "":
		return errors.New("no guest kernel")
	case g.RootDiskPath == "":
		return errors.New("no root disk")
	case g.StateDiskPath == "":
		return errors.New("no state disk")
	case g.TapName == "":
		return errors.New("no tap device")
	case g.QMPSocket == "":
		return errors.New("no QMP socket")
	case g.CPUs < 1:
		return fmt.Errorf("guest needs at least one CPU, got %d", g.CPUs)
	case g.MemoryMiB < MinMemoryMiB:
		return fmt.Errorf("guest needs at least %d MiB of memory, got %d", MinMemoryMiB, g.MemoryMiB)
	case len(g.MAC) != 6:
		return fmt.Errorf("guest MAC %q is not a 48-bit address", g.MAC)
	}
	return g.validateAddressing()
}

// validateAddressing checks what the kernel's `ip=` parser can take: IPv4 only,
// and fields separated by colons, so none may contain one.
func (g Guest) validateAddressing() error {
	if !g.Address.IsValid() || !g.Address.Addr().Is4() {
		return fmt.Errorf("guest address %s is not IPv4, which ip= requires", g.Address)
	}
	if !g.Gateway.Is4() || !g.Address.Masked().Contains(g.Gateway) || g.Gateway == g.Address.Addr() {
		return fmt.Errorf("gateway %s is not another address in %s", g.Gateway, g.Address.Masked())
	}
	// A pod name is a DNS-1123 subdomain, which carries no colon to break `ip=`.
	if errs := validation.IsDNS1123Subdomain(g.Hostname); len(errs) > 0 {
		return fmt.Errorf("hostname %q: %s", g.Hostname, strings.Join(errs, ", "))
	}
	return nil
}

func machineType(arch Arch) string {
	if arch == ArchARM64 {
		return "virt"
	}
	return "q35"
}

func netmask(prefix netip.Prefix) string {
	mask := net.CIDRMask(prefix.Bits(), 32)
	return net.IP(mask).String()
}
