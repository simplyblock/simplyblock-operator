// What a reclaim gives back, and what it refuses to touch.
//
// The subject is the refusals. Handing an idle controller back to the kernel
// costs nothing, and handing back one something is driving takes a running
// application's disks out from under it mid-IO — and from sysfs the two look
// the same. Every case below is about that line.

package pci

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// reclaimableWorker is takenWorker with the parts a rebind actually writes.
//
// takenWorker describes a machine to be read; a reclaim changes one, so the
// attributes it opens have to exist the way they do in sysfs: a driver_override
// per device, an unbind for the driver holding them, and the bus's drivers_probe.
// The procfs is an empty directory, which is a machine where no process holds
// anything rather than one whose process table could not be read.
func reclaimableWorker() tree {
	t2 := takenWorker()
	for _, slot := range []string{"0000:00:02.0", "0000:00:03.0", "0000:00:04.0", "0000:00:05.0"} {
		t2.files["bus/pci/devices/"+slot+"/driver_override"] = ""
	}
	t2.files["bus/pci/drivers/uio_pci_generic/unbind"] = ""
	t2.files["bus/pci/drivers/nvme/unbind"] = ""
	t2.files["bus/pci/drivers_probe"] = ""
	t2.dirs = append(t2.dirs, "proc")
	return t2
}

// heldBy makes a process in the fixture's procfs hold one uio device open, so
// the reclaim reads a machine where something is driving a controller.
func heldBy(t2 tree, pid int, command, uio string) tree {
	dir := "proc/" + strconv.Itoa(pid)
	t2.files[dir+"/comm"] = command
	t2.links[dir+"/fd/3"] = "/dev/" + uio
	return t2
}

// overrideOf reads back a device's driver_override, which a reclaim clears so
// that the bus's probe is free to pick the kernel's own driver.
func overrideOf(t *testing.T, root, slot string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "bus", "pci", "devices", slot, "driver_override"))
	if err != nil {
		t.Fatalf("read %s driver_override: %v", slot, err)
	}
	return strings.TrimSpace(string(raw))
}

// reclaimedAddresses is the addresses a reclamation handed back.
func reclaimedAddresses(r Reclamation) string { return strings.Join(r.Reclaimed, ",") }

// An idle controller is handed back: its driver_override is cleared and the bus
// is asked to probe it again, which is what returns it to whatever driver the
// kernel prefers for it.
func TestReclaimHandsBackAnIdleController(t *testing.T) {
	root := reclaimableWorker().write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "proc")}

	devices, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}
	checked, err := CheckHolders(cfg, NVMeControllers(devices))
	if err != nil {
		t.Fatalf("check the holders: %v", err)
	}

	got, err := Reclaim(cfg, checked)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if len(got.Reclaimed) != 4 {
		t.Errorf("reclaimed %s, want all four idle controllers", reclaimedAddresses(got))
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused %v, and nothing is driving any of them", got.Refused)
	}

	// Every reclaimed device had its driver_override cleared, which is what
	// leaves the bus's probe free to pick the kernel's own driver instead of
	// sending the device straight back to the one it was taken from.
	for _, slot := range got.Reclaimed {
		if override := overrideOf(t, root, slot); override != "" {
			t.Errorf("%s still overrides its driver with %q", slot, override)
		}
	}
}

// A controller a process is driving is refused, and the refusal names what is
// driving it. This is the case that separates a leftover binding from a running
// storage node, and getting it wrong is data loss rather than an inconvenience.
func TestReclaimRefusesAControllerSomethingIsDriving(t *testing.T) {
	root := heldBy(reclaimableWorker(), 4242, "spdk_tgt", "uio2").write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "proc")}

	devices, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}
	checked, err := CheckHolders(cfg, NVMeControllers(devices))
	if err != nil {
		t.Fatalf("check the holders: %v", err)
	}

	got, err := Reclaim(cfg, checked)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if len(got.Reclaimed) != 3 {
		t.Errorf("reclaimed %s, want the three nothing is driving", reclaimedAddresses(got))
	}
	if len(got.Refused) != 1 {
		t.Fatalf("refused %v, want the one being driven", got.Refused)
	}
	refusal := got.Refused[0]
	if refusal.Address != "0000:00:02.0" {
		t.Errorf("refused %s, want the held controller 0000:00:02.0", refusal.Address)
	}
	if !strings.Contains(refusal.Reason, "spdk_tgt") {
		t.Errorf("the refusal reads %q, and the next question is always which process", refusal.Reason)
	}
}

// A controller whose holders could not be established is refused rather than
// reclaimed. Not knowing is not permission: a probe that cannot see the host's
// processes sees its own, and would conclude that nothing holds anything.
func TestReclaimRefusesWhatItCouldNotCheck(t *testing.T) {
	root := reclaimableWorker().write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "proc")}

	devices, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}

	// The scan's devices, with nobody having asked whether anything holds them:
	// InUse is nil, which is the unchecked state rather than the idle one.
	got, err := Reclaim(cfg, NVMeControllers(devices))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if len(got.Reclaimed) != 0 {
		t.Errorf("reclaimed %s without anyone having checked for holders",
			reclaimedAddresses(got))
	}
	if len(got.Refused) != 4 {
		t.Fatalf("refused %v, want all four unchecked controllers", got.Refused)
	}
}

// A controller the kernel already drives is not a reclaim's business. It has no
// userspace driver to take it from, so touching it would unbind a working
// device to bind it back again.
func TestReclaimLeavesAKernelDrivenControllerAlone(t *testing.T) {
	t2 := reclaimableWorker()
	kernel := "bus/pci/devices/0000:00:06.0"
	t2.files[kernel+"/class"] = "0x010802"
	t2.files[kernel+"/vendor"] = "0x1b36"
	t2.files[kernel+"/device"] = "0x0010"
	t2.files[kernel+"/numa_node"] = "0"
	t2.links[kernel+"/driver"] = "../../../bus/pci/drivers/nvme"
	root := t2.write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "proc")}

	devices, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the bus: %v", err)
	}
	checked, err := CheckHolders(cfg, NVMeControllers(devices))
	if err != nil {
		t.Fatalf("check the holders: %v", err)
	}

	got, err := Reclaim(cfg, checked)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	for _, address := range got.Reclaimed {
		if address == "0000:00:06.0" {
			t.Error("reclaimed a controller the kernel already drives")
		}
	}
	for _, refusal := range got.Refused {
		if refusal.Address == "0000:00:06.0" {
			t.Errorf("refused a controller the kernel already drives: %s", refusal.Reason)
		}
	}
}

// A machine with nothing on a userspace driver reclaims nothing and is not an
// error. It is the ordinary case on a fleet that was never prepared for SPDK.
func TestReclaimOnAMachineWithNothingBoundIsNotAFailure(t *testing.T) {
	root := tree{files: map[string]string{}, links: map[string]string{}}.write(t)
	cfg := Config{SysfsRoot: root, ProcRoot: filepath.Join(root, "proc")}

	got, err := Reclaim(cfg, nil)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(got.Reclaimed) != 0 || len(got.Refused) != 0 {
		t.Errorf("reclaimed %v and refused %v on a machine with no userspace binding",
			got.Reclaimed, got.Refused)
	}
}
