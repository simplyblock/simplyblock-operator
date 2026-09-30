// What the probe records about the controllers it took back.
//
// The record is the point. A reclaim changes the machine before the machine is
// read, so a report that carried only the result would describe a worker whose
// disks appeared for reasons nothing in the report explains — and the next
// person to read it would have no way to tell a fleet that always had kernel
// disks from one this run rebound.

package nodeprobe

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/pci"
)

// boundWorker writes a machine whose two NVMe controllers are on a userspace
// driver with nothing holding them, plus the attributes a rebind writes.
func boundWorker(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, slot := range []string{"0000:01:00.0", "0000:0b:00.0"} {
		dir := "bus/pci/devices/" + slot
		write(dir+"/class", "0x010802\n")
		write(dir+"/vendor", "0x144d\n")
		write(dir+"/device", "0xa80a\n")
		write(dir+"/numa_node", "0\n")
		write(dir+"/driver_override", "")
		if err := os.MkdirAll(filepath.Join(root, dir, "uio", "uio0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../bus/pci/drivers/vfio-pci",
			filepath.Join(root, dir, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	write("bus/pci/drivers/vfio-pci/unbind", "")
	write("bus/pci/drivers_probe", "")
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReclaimUserspaceRecordsWhatItHandedBack(t *testing.T) {
	root := boundWorker(t)

	got, err := ReclaimUserspace(pci.Config{
		SysfsRoot: root, ProcRoot: filepath.Join(root, "proc"),
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if got == nil {
		t.Fatal("the reclaim reported nothing at all, and a run that reclaimed has to say so")
	}
	if len(got.Reclaimed) != 2 {
		t.Errorf("recorded %v, want both controllers", got.Reclaimed)
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused %v, and nothing is driving either of them", got.Refused)
	}
}

// A machine with nothing to reclaim still records that the reclaim ran. The
// empty result and the absent one are different findings: one is a fleet whose
// disks were already the kernel's, and the other is a run that never looked.
func TestReclaimUserspaceRecordsAnEmptyPass(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ReclaimUserspace(pci.Config{
		SysfsRoot: root, ProcRoot: filepath.Join(root, "proc"),
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if got == nil {
		t.Fatal("a pass that found nothing to reclaim reported nothing at all")
	}
	if len(got.Reclaimed) != 0 || len(got.Refused) != 0 {
		t.Errorf("recorded %v and %v on a machine with no userspace binding",
			got.Reclaimed, got.Refused)
	}
}

// The report carries it, and the wire form keeps it: the operator reads the
// report and nothing else, so a field the encoding dropped is a reclaim nobody
// downstream can account for.
func TestTheReportCarriesTheReclaim(t *testing.T) {
	report := Report{
		Version: ReportVersion,
		Node:    testNode,
		Reclaim: &Reclaim{
			Reclaimed: []string{"0000:01:00.0"},
			Refused: []ReclaimRefusal{{
				Address: "0000:0b:00.0",
				Reason:  "pid 4242 (spdk_tgt) holds /dev/uio0",
			}},
		},
	}

	encoded, err := Encode(report)
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	back, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode the report: %v", err)
	}

	if back.Reclaim == nil {
		t.Fatal("the reclaim did not survive the round trip")
	}
	if len(back.Reclaim.Reclaimed) != 1 || back.Reclaim.Reclaimed[0] != "0000:01:00.0" {
		t.Errorf("read back %v, want the one reclaimed controller", back.Reclaim.Reclaimed)
	}
	if len(back.Reclaim.Refused) != 1 {
		t.Fatalf("read back %v, want the one refusal", back.Reclaim.Refused)
	}
	if back.Reclaim.Refused[0].Reason == "" {
		t.Error("the refusal survived without its reason, which is the whole of what it says")
	}
}

// A report from a run that did not reclaim carries nothing, rather than an
// empty reclaim that reads as a pass which found nothing.
func TestAReportFromAnOrdinaryRunCarriesNoReclaim(t *testing.T) {
	encoded, err := Encode(Report{Version: ReportVersion, Node: testNode})
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	back, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if back.Reclaim != nil {
		t.Errorf("a run that did not reclaim reported %+v", back.Reclaim)
	}
}
