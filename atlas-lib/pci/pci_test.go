// What the PCI scan concludes, and what the rebind refuses.
//
// The fixture is a transcript of a worker whose NVMe controllers are bound to
// uio_pci_generic: four of them, the class code they really report, and the uio
// numbering the machine really has — where uio0 is the controller in slot 05.0
// and uio2 is the one in slot 02.0. That last detail is the reason the mapping
// follows the device's own uio directory instead of counting, and a fixture
// that numbered them in order would not have caught a reader that counted.

package pci

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree builds a sysfs and procfs under a temporary root.
type tree struct {
	files map[string]string
	links map[string]string
	dirs  []string
}

func (t2 tree) write(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range t2.dirs {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range t2.files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, target := range t2.links {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// takenWorker is the fleet this was written against: four NVMe controllers,
// every one of them on a userspace driver, plus the AHCI controller the machine
// boots from.
func takenWorker() tree {
	t2 := tree{files: map[string]string{}, links: map[string]string{}}

	// The uio index deliberately does not follow the PCI order, because on the
	// real machine it does not.
	for _, c := range []struct{ slot, uio string }{
		{"0000:00:02.0", "uio2"},
		{"0000:00:03.0", "uio3"},
		{"0000:00:04.0", "uio1"},
		{"0000:00:05.0", "uio0"},
	} {
		dir := "bus/pci/devices/" + c.slot
		t2.files[dir+"/class"] = "0x010802"
		t2.files[dir+"/vendor"] = "0x1b36"
		t2.files[dir+"/device"] = "0x0010"
		t2.files[dir+"/numa_node"] = "-1"
		t2.links[dir+"/driver"] = "../../../bus/pci/drivers/uio_pci_generic"
		t2.dirs = append(t2.dirs, dir+"/uio/"+c.uio)
	}

	// The boot controller, which is not NVMe and is on a kernel driver.
	ahci := "bus/pci/devices/0000:00:1f.2"
	t2.files[ahci+"/class"] = "0x010601"
	t2.files[ahci+"/vendor"] = "0x8086"
	t2.files[ahci+"/device"] = "0x2922"
	t2.files[ahci+"/numa_node"] = "0"
	t2.links[ahci+"/driver"] = "../../../bus/pci/drivers/ahci"

	t2.dirs = append(t2.dirs, "bus/pci/drivers/uio_pci_generic", "bus/pci/drivers/nvme")
	return t2
}

// nvmeSlot returns one scanned device by address.
func nvmeSlot(t *testing.T, devices []Device, address string) Device {
	t.Helper()
	for _, device := range devices {
		if device.Address == address {
			return device
		}
	}
	t.Fatalf("%s is missing from the scan", address)
	return Device{}
}

func TestScanFindsNVMeControllersNoBlockDeviceWouldShow(t *testing.T) {
	// This is the whole point of the package. Every one of these controllers is
	// an NVMe disk, and a scan of class/block on this machine finds none of
	// them.
	root := takenWorker().write(t)

	devices, err := Scan(Config{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}

	nvme := NVMeControllers(devices)
	if len(nvme) != 4 {
		t.Fatalf("found %d NVMe controllers, want 4: %+v", len(nvme), nvme)
	}
	for _, device := range nvme {
		if !device.BoundToUserspace() {
			t.Errorf("%s is on %q, want a userspace driver", device.Address, device.Driver)
		}
		if device.HasKernelDriver() != true {
			t.Errorf("%s reports no driver at all", device.Address)
		}
	}

	// The boot controller is mass storage and is not NVMe: matching the class
	// alone would have taken it.
	ahci := nvmeSlot(t, devices, "0000:00:1f.2")
	if ahci.IsNVMe() {
		t.Errorf("the AHCI controller (class %s) was read as NVMe", ahci.Class)
	}
}

func TestScanMapsAUIODeviceByItsOwnDirectoryAndNotByCounting(t *testing.T) {
	// uio0 belongs to the controller in slot 05.0 and uio2 to the one in 02.0.
	// A reader that numbered them in PCI order would hand a caller the wrong
	// character device, and the caller would then ask whether the wrong device
	// is busy.
	root := takenWorker().write(t)

	devices, err := Scan(Config{SysfsRoot: root, DevRoot: "/dev"})
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}

	for slot, want := range map[string]string{
		"0000:00:02.0": "/dev/uio2",
		"0000:00:03.0": "/dev/uio3",
		"0000:00:04.0": "/dev/uio1",
		"0000:00:05.0": "/dev/uio0",
	} {
		device := nvmeSlot(t, devices, slot)
		if !slices.Equal(device.UIODevices, []string{want}) {
			t.Errorf("%s maps to %v, want [%s]", slot, device.UIODevices, want)
		}
	}
}

func TestScanReportsADeviceNoDriverOwns(t *testing.T) {
	// A controller rebound away from one driver and not to another. It has no
	// block device and no userspace driver either, so its content cannot be
	// read at all, which is a different answer from being free.
	h := takenWorker()
	delete(h.links, "bus/pci/devices/0000:00:02.0/driver")
	h.dirs = slices.DeleteFunc(h.dirs, func(d string) bool {
		return d == "bus/pci/devices/0000:00:02.0/uio/uio2"
	})

	devices, err := Scan(Config{SysfsRoot: h.write(t)})
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}

	orphan := nvmeSlot(t, devices, "0000:00:02.0")
	if orphan.HasKernelDriver() {
		t.Errorf("%s reports driver %q, want none", orphan.Address, orphan.Driver)
	}
	if orphan.BoundToUserspace() {
		t.Error("a device with no driver was reported as taken by a userspace one")
	}
	if !orphan.IsNVMe() {
		t.Error("a device with no driver stopped being an NVMe controller")
	}
}

func TestScanIsOrderedAndSurvivesAHostWithNoBus(t *testing.T) {
	root := takenWorker().write(t)
	devices, err := Scan(Config{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}
	if !slices.IsSortedFunc(devices, func(a, b Device) int { return strings.Compare(a.Address, b.Address) }) {
		t.Errorf("the scan is not ordered: %+v", devices)
	}

	empty, err := Scan(Config{SysfsRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("scan a tree with no PCI bus: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("found %d devices on a tree with no bus", len(empty))
	}
}

func TestHeldByFindsTheProcessDrivingADevice(t *testing.T) {
	// The only evidence there is: sysfs exports nothing that changes while a
	// process holds the character device, so the answer is a walk of the
	// process table.
	h := takenWorker()
	h.links["1234/fd/3"] = "/dev/uio2"
	h.files["1234/comm"] = "spdk_tgt"
	h.links["9/fd/0"] = "/dev/null"
	h.files["9/comm"] = "sh"

	root := h.write(t)
	devices, err := Scan(Config{SysfsRoot: root, DevRoot: "/dev"})
	if err != nil {
		t.Fatal(err)
	}

	holders, err := HeldBy(Config{SysfsRoot: root, ProcRoot: root, DevRoot: "/dev"},
		nvmeSlot(t, devices, "0000:00:02.0"))
	if err != nil {
		t.Fatalf("read the holders: %v", err)
	}
	if len(holders) != 1 {
		t.Fatalf("found %d holders, want 1: %+v", len(holders), holders)
	}
	if holders[0].PID != 1234 || holders[0].Command != "spdk_tgt" {
		t.Errorf("the holder is %+v, want pid 1234 (spdk_tgt)", holders[0])
	}

	// A controller nothing holds is bound and idle.
	idle, err := HeldBy(Config{SysfsRoot: root, ProcRoot: root, DevRoot: "/dev"},
		nvmeSlot(t, devices, "0000:00:03.0"))
	if err != nil {
		t.Fatalf("read the holders: %v", err)
	}
	if len(idle) != 0 {
		t.Errorf("found %+v holding an idle controller", idle)
	}
}

func TestBindToRefusesADeviceSomethingIsDriving(t *testing.T) {
	// The guard that matters. Handing a controller back to the kernel while
	// SPDK is driving it takes a storage node's disks out from under it.
	h := takenWorker()
	h.links["1234/fd/3"] = "/dev/uio2"
	h.files["1234/comm"] = "spdk_tgt"
	root := h.write(t)

	cfg := Config{SysfsRoot: root, ProcRoot: root, DevRoot: "/dev"}
	err := BindTo(cfg, "0000:00:02.0", DriverNVMe)

	if err == nil {
		t.Fatal("rebound a controller a process is driving")
	}
	if !errors.Is(err, ErrDeviceHeld) {
		t.Errorf("the error is %v, and a caller cannot tell it from a broken sysfs", err)
	}
	if !strings.Contains(err.Error(), "spdk_tgt") {
		t.Errorf("the error is %q, and it does not name what is holding it", err)
	}
}

func TestBindToRefusesWhatItCannotCheck(t *testing.T) {
	// Not knowing is not permission. A caller that cannot see the host's
	// processes gets a refusal rather than a rebind on an unchecked assumption.
	root := takenWorker().write(t)

	err := BindTo(Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "nonexistent")},
		"0000:00:02.0", DriverNVMe)

	if err == nil {
		t.Fatal("rebound a controller whose holders could not be read")
	}
	if errors.Is(err, ErrDeviceHeld) {
		t.Error("an unreadable process table was reported as the device being held")
	}
}

func TestBindToRefusesAnAddressThatIsNotOne(t *testing.T) {
	root := takenWorker().write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: root}

	for _, bad := range []string{"", "nvme0n1", "0000:5e:00", "../../etc/passwd"} {
		if err := BindTo(cfg, bad, DriverNVMe); err == nil {
			t.Errorf("accepted %q as a PCI address", bad)
		}
	}
	if err := BindTo(cfg, "0000:99:00.0", DriverNVMe); err == nil {
		t.Error("rebound a device that is not on the bus")
	}
}

func TestParseAddressNormalizesAndRefuses(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0000:5e:00.0", "0000:5e:00.0"},
		{"0000:5E:00.0", "0000:5e:00.0"},
		{"  0000:5e:00.0  ", "0000:5e:00.0"},
	} {
		got, err := ParseAddress(tc.in)
		if err != nil {
			t.Errorf("parse %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parse %q: read %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "0000:5e:00", "5e:00.0", "0000:5e:00.gg", "/dev/nvme0n1"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("parsed %q as a PCI address", bad)
		}
	}
}

func TestUnbindOnADeviceWithNoDriverIsNotAFailure(t *testing.T) {
	// Unbinding is for reaching the state of having no driver, so arriving
	// there early is success rather than an error a caller has to special-case.
	h := takenWorker()
	delete(h.links, "bus/pci/devices/0000:00:02.0/driver")

	if err := Unbind(Config{SysfsRoot: h.write(t)}, "0000:00:02.0"); err != nil {
		t.Errorf("unbinding a device with no driver failed: %v", err)
	}
}
