package qemu

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	export "github.com/simplyblock/atlas/nfsexport"
)

func validGuest(arch Arch) Guest {
	g := Guest{
		Arch:          arch,
		KernelPath:    "/mds/kernel/vmlinuz",
		RootDiskPath:  "/mds/disk.qcow2",
		StateDiskPath: "/dev/mds-state",
		CPUs:          2,
		MemoryMiB:     2048,
		TapName:       "mds-tap0",
		MAC:           net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x02},
		Address:       netip.MustParsePrefix("169.254.100.2/30"),
		Gateway:       netip.MustParseAddr("169.254.100.1"),
		Hostname:      "pnfs-mds-0",
		QMPSocket:     "/run/mds/qmp.sock",
	}
	if arch == ArchARM64 {
		g.FirmwarePath = "/mds/firmware/QEMU_EFI.fd"
	}
	return g
}

func mustArgs(t *testing.T, g Guest) []string {
	t.Helper()
	args, err := g.Args()
	if err != nil {
		t.Fatalf("Args() for a valid %s guest: %v", g.Arch, err)
	}
	return args
}

// values returns every value given to flag, in command-line order.
func values(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

// The design refuses software emulation: a guest too slow to serve an export
// is worse than a pod that fails to start and says why.
func TestGuestAlwaysRunsUnderKVM(t *testing.T) {
	for _, arch := range []Arch{ArchAMD64, ArchARM64} {
		if args := mustArgs(t, validGuest(arch)); !slices.Contains(args, "-enable-kvm") {
			t.Errorf("%s: args lack -enable-kvm: %v", arch, args)
		}
	}
}

func TestArchitectureSelectsBinaryMachineAndFirmware(t *testing.T) {
	cases := []struct {
		arch     Arch
		binary   string
		machine  string
		firmware []string
	}{
		{ArchAMD64, "qemu-system-x86_64", "q35", nil},
		{ArchARM64, "qemu-system-aarch64", "virt", []string{"/mds/firmware/QEMU_EFI.fd"}},
	}
	for _, c := range cases {
		g := validGuest(c.arch)
		binary, err := g.Binary()
		if err != nil || binary != c.binary {
			t.Errorf("%s: Binary() = %q, %v, want %q", c.arch, binary, err, c.binary)
		}
		args := mustArgs(t, g)
		if got := values(args, "-machine"); !slices.Equal(got, []string{c.machine}) {
			t.Errorf("%s: -machine = %v, want [%s]", c.arch, got, c.machine)
		}
		if got := values(args, "-bios"); !slices.Equal(got, c.firmware) {
			t.Errorf("%s: -bios = %v, want %v", c.arch, got, c.firmware)
		}
	}
}

// ARM64's virt machine only has ACPI, which the guest needs for the power
// button and CPU topology, when UEFI firmware brings it up.
func TestARM64WithoutFirmwareIsRefused(t *testing.T) {
	g := validGuest(ArchARM64)
	g.FirmwarePath = ""
	if _, err := g.Args(); err == nil {
		t.Fatal("Args() accepted an arm64 guest without firmware")
	}
}

func TestAMD64WithFirmwareIsRefused(t *testing.T) {
	g := validGuest(ArchAMD64)
	g.FirmwarePath = "/mds/firmware/QEMU_EFI.fd"
	if _, err := g.Args(); err == nil {
		t.Fatal("Args() accepted firmware for an amd64 guest, which boots without it")
	}
}

func TestUnknownArchitectureIsRefused(t *testing.T) {
	g := validGuest(ArchAMD64)
	g.Arch = "riscv64"
	if _, err := g.Binary(); err == nil {
		t.Error("Binary() accepted riscv64")
	}
	if _, err := g.Args(); err == nil {
		t.Error("Args() accepted riscv64")
	}
}

// The guest's fstab mounts / from /dev/vda and the state disk by its serial.
// PCI enumeration follows command-line order, so the root disk has to be the
// first block device, and only the state disk is identified by serial.
func TestRootDiskIsFirstAndReadOnlyStateDiskCarriesItsSerial(t *testing.T) {
	for _, arch := range []Arch{ArchAMD64, ArchARM64} {
		args := mustArgs(t, validGuest(arch))
		drives := values(args, "-drive")
		if len(drives) != 2 {
			t.Fatalf("%s: want 2 drives, got %v", arch, drives)
		}
		root, state := drives[0], drives[1]
		if !strings.Contains(root, "file=/mds/disk.qcow2") || !strings.Contains(root, "format=qcow2") ||
			!strings.Contains(root, "readonly=on") {
			t.Errorf("%s: first drive is not the read-only qcow2 root: %q", arch, root)
		}
		if !strings.Contains(state, "file=/dev/mds-state") || !strings.Contains(state, "format=raw") ||
			strings.Contains(state, "readonly=on") {
			t.Errorf("%s: second drive is not the writable raw state disk: %q", arch, state)
		}

		var blk []string
		for _, d := range values(args, "-device") {
			if strings.HasPrefix(d, "virtio-blk-pci,") {
				blk = append(blk, d)
			}
		}
		if len(blk) != 2 {
			t.Fatalf("%s: want 2 virtio-blk devices, got %v", arch, blk)
		}
		rootID, stateID := driveID(root), driveID(state)
		if !strings.Contains(blk[0], "drive="+rootID) || !strings.Contains(blk[1], "drive="+stateID) {
			t.Errorf("%s: virtio-blk order %v does not attach root (%s) before state (%s)", arch, blk, rootID, stateID)
		}
		if !strings.Contains(blk[1], "serial="+StateDiskSerial) || strings.Contains(blk[0], "serial=") {
			t.Errorf("%s: only the state disk carries serial=%s: %v", arch, StateDiskSerial, blk)
		}
	}
}

func driveID(drive string) string {
	for _, opt := range strings.Split(drive, ",") {
		if id, ok := strings.CutPrefix(opt, "id="); ok {
			return id
		}
	}
	return ""
}

func TestKernelCmdlineCarriesTheStaticAddress(t *testing.T) {
	want := []string{
		"root=/dev/vda", "ro", "console=hvc0", "panic=-1",
		"ip=169.254.100.2::169.254.100.1:255.255.255.252:pnfs-mds-0:eth0:off",
		"nfsd.pnfs_probe_name=" + export.LayoutProbeName, "nfsd.pnfs_layout_hold=60",
		"xfs.pnfs_zeroed_layouts=0",
	}
	cmdline := strings.Fields(validGuest(ArchAMD64).KernelCmdline())
	if !slices.Equal(cmdline, want) {
		t.Errorf("amd64 cmdline = %q, want %q", cmdline, want)
	}

	arm := strings.Fields(validGuest(ArchARM64).KernelCmdline())
	if !slices.Equal(arm, append(slices.Clone(want), "acpi=on")) {
		t.Errorf("arm64 cmdline = %q, want amd64's plus acpi=on", arm)
	}
}

// Regression: 2026-10-10-pnfs-mds-restart-lost-layoutcommit (PR #714), the
// command-line half only. F-20 in test-plan-pnfs-rwx.md is the live half.
//
// Zeroed layouts are the guest kernel's opt-in (pnfs-os patch 0005): blocks a
// client writes through a layout are durable without its LAYOUTCOMMIT, which a
// restarted server never receives. The command line states the choice either
// way, so a guest's dmesg shows which one it booted with.
func TestKernelCmdlineEnablesZeroedLayoutsOnRequest(t *testing.T) {
	g := validGuest(ArchAMD64)
	g.ZeroedLayouts = true
	cmdline := strings.Fields(g.KernelCmdline())
	if !slices.Contains(cmdline, "xfs.pnfs_zeroed_layouts=1") {
		t.Errorf("cmdline = %q, want xfs.pnfs_zeroed_layouts=1", cmdline)
	}
	if slices.Contains(cmdline, "xfs.pnfs_zeroed_layouts=0") {
		t.Errorf("cmdline = %q carries both settings", cmdline)
	}
}

func TestArgsBootTheKernelDirectlyWithTheCmdline(t *testing.T) {
	g := validGuest(ArchAMD64)
	args := mustArgs(t, g)
	if got := values(args, "-kernel"); !slices.Equal(got, []string{g.KernelPath}) {
		t.Errorf("-kernel = %v", got)
	}
	if got := values(args, "-append"); !slices.Equal(got, []string{g.KernelCmdline()}) {
		t.Errorf("-append = %v, want [%s]", got, g.KernelCmdline())
	}
}

// panic=-1 reboots the guest at once, and -no-reboot turns that reboot into
// QEMU exiting, so a guest panic ends the runner and kubelet restarts the pod.
func TestGuestPanicEndsQEMU(t *testing.T) {
	if args := mustArgs(t, validGuest(ArchAMD64)); !slices.Contains(args, "-no-reboot") {
		t.Errorf("args lack -no-reboot: %v", args)
	}
}

func TestConsoleIsAVirtioConsoleOnStdio(t *testing.T) {
	args := mustArgs(t, validGuest(ArchARM64))
	chardevs := values(args, "-chardev")
	if !slices.ContainsFunc(chardevs, func(c string) bool { return strings.HasPrefix(c, "stdio,id=console") }) {
		t.Errorf("no stdio chardev named console: %v", chardevs)
	}
	if !slices.Contains(values(args, "-device"), "virtconsole,chardev=console") {
		t.Errorf("no virtconsole on the console chardev: %v", values(args, "-device"))
	}
}

func TestNICIsTheTapWithTheGivenMAC(t *testing.T) {
	g := validGuest(ArchAMD64)
	args := mustArgs(t, g)
	netdevs := values(args, "-netdev")
	if len(netdevs) != 1 || !strings.HasPrefix(netdevs[0], "tap,id=net0,ifname=mds-tap0,script=no,downscript=no") {
		t.Fatalf("-netdev = %v", netdevs)
	}
	if strings.Contains(netdevs[0], "vhost=on") {
		t.Errorf("vhost requested without VhostNet: %q", netdevs[0])
	}
	if !slices.Contains(values(args, "-device"), "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:02") {
		t.Errorf("no virtio-net-pci with the MAC: %v", values(args, "-device"))
	}

	g.VhostNet = true
	if netdev := values(mustArgs(t, g), "-netdev")[0]; !strings.HasSuffix(netdev, ",vhost=on") {
		t.Errorf("VhostNet did not request vhost: %q", netdev)
	}
}

func TestResourcesAndQMPSocket(t *testing.T) {
	args := mustArgs(t, validGuest(ArchAMD64))
	if got := values(args, "-smp"); !slices.Equal(got, []string{"2"}) {
		t.Errorf("-smp = %v", got)
	}
	if got := values(args, "-m"); !slices.Equal(got, []string{"2048M"}) {
		t.Errorf("-m = %v", got)
	}
	if got := values(args, "-qmp"); !slices.Equal(got, []string{"unix:/run/mds/qmp.sock,server=on,wait=off"}) {
		t.Errorf("-qmp = %v", got)
	}
}

func TestInvalidInputIsRefused(t *testing.T) {
	cases := map[string]func(*Guest){
		"no kernel":             func(g *Guest) { g.KernelPath = "" },
		"no root disk":          func(g *Guest) { g.RootDiskPath = "" },
		"no state disk":         func(g *Guest) { g.StateDiskPath = "" },
		"no tap":                func(g *Guest) { g.TapName = "" },
		"no QMP socket":         func(g *Guest) { g.QMPSocket = "" },
		"no CPUs":               func(g *Guest) { g.CPUs = 0 },
		"too little memory":     func(g *Guest) { g.MemoryMiB = 64 },
		"no MAC":                func(g *Guest) { g.MAC = nil },
		"IPv6 address":          func(g *Guest) { g.Address = netip.MustParsePrefix("fd00::2/64") },
		"gateway outside":       func(g *Guest) { g.Gateway = netip.MustParseAddr("10.0.0.1") },
		"gateway is the guest":  func(g *Guest) { g.Gateway = netip.MustParseAddr("169.254.100.2") },
		"no hostname":           func(g *Guest) { g.Hostname = "" },
		"hostname with a colon": func(g *Guest) { g.Hostname = "pnfs:mds" },
		"hostname with a space": func(g *Guest) { g.Hostname = "pnfs mds" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := validGuest(ArchAMD64)
			mutate(&g)
			if args, err := g.Args(); err == nil {
				t.Errorf("Args() accepted it: %v", args)
			}
		})
	}
}
