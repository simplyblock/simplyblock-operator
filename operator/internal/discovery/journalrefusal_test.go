// What a logical block-device run does when the fleet does not say which disk
// the journal goes on.
//
// It drafts the journal split, whatever the disks look like. With
// enableJournalDevice unset, the journal of a block-device node is a partition
// carved from its smallest disk when the node is configured, so every disk
// keeps carrying data. That is also the layout for a worker with a single disk,
// such as one RAID-protected storage-array LUN. Only a run told to force it
// dedicates the smallest disk of every node to the journal.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// blockFleetWithDisks is a plan whose workers each hand over block devices of
// the given sizes.
func blockFleetWithDisks(sizes ...[]uint64) Plan {
	plan := Plan{Class: ClassBlock}
	for index, worker := range sizes {
		devices := make([]nodeprobe.Device, 0, len(worker))
		for slot, size := range worker {
			devices = append(devices, blockDisk("sd"+string(rune('b'+slot)), size))
		}
		plan.Workers = append(plan.Workers, Worker{
			Name:    "worker-" + string(rune('1'+index)),
			Class:   ClassBlock,
			Devices: devices,
		})
	}
	return plan
}

const (
	oneTerabyte  = uint64(1) << 40
	twoTerabytes = 2 * oneTerabyte
)

// A block fleet whose disks do not name a journal device is drafted with the
// journal split: the flag is left unset, and the note says where the journal
// goes and how to dedicate a disk instead.
func TestABlockFleetWithNoJournalDiskGetsTheJournalSplit(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes, twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err != nil {
		t.Fatalf("a block fleet of equal disks was refused: %v", err)
	}
	if template.Template.EnableJournalDevice != nil {
		t.Errorf("enableJournalDevice is %v, and the journal split leaves it unset",
			*template.Template.EnableJournalDevice)
	}
	notes := strings.Join(template.Notes, " ")
	for _, want := range []string{"partition carved from its smallest disk", "forceJournalDevice"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes read %q, and they have to name %q", notes, want)
		}
	}
}

// A worker with a single disk, such as one storage-array LUN, is drafted with
// the journal split as well, since there is no second disk to dedicate.
func TestABlockWorkerWithASingleDiskGetsTheJournalSplit(t *testing.T) {
	plan := blockFleetWithDisks([]uint64{twoTerabytes}, []uint64{twoTerabytes})

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err != nil {
		t.Fatalf("a fleet of single-disk workers was refused: %v", err)
	}
	if template.Template.EnableJournalDevice != nil {
		t.Errorf("enableJournalDevice is %v, and a single disk cannot be dedicated",
			*template.Template.EnableJournalDevice)
	}
}

// The same fleet is drafted when the run was told to force it, and the flag is
// proposed rather than left unset.
func TestAForcedRunDraftsTheFleetItWouldHaveRefused(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes, twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
	if err != nil {
		t.Fatalf("a forced run refused anyway: %v", err)
	}
	if template.Template.EnableJournalDevice == nil || !*template.Template.EnableJournalDevice {
		t.Error("a forced run left enableJournalDevice unset, which is the document that cannot deploy")
	}

	// The note has to say the disk was taken rather than offered, because the
	// capacity it spends is capacity the fleet did not set aside.
	if !strings.Contains(strings.Join(template.Notes, " "), "forceJournalDevice") {
		t.Errorf("the notes do not say the journal device was forced: %v", template.Notes)
	}
}

// A block fleet with a sole smallest disk per worker is drafted with the
// journal split too: the default does not depend on the disk sizes, and only a
// forced run dedicates that disk.
func TestABlockFleetWithASmallerDiskStillGetsTheJournalSplit(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{oneTerabyte, twoTerabytes},
		[]uint64{oneTerabyte, twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err != nil {
		t.Fatalf("a readable fleet was refused: %v", err)
	}
	if template.Template.EnableJournalDevice != nil {
		t.Errorf("enableJournalDevice is %v, and the journal split is the default for block devices",
			*template.Template.EnableJournalDevice)
	}

	forced, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
	if err != nil {
		t.Fatalf("a forced run refused: %v", err)
	}
	if forced.Template.EnableJournalDevice == nil || !*forced.Template.EnableJournalDevice {
		t.Error("a forced run over a fleet with a sole smallest disk did not set enableJournalDevice")
	}
}

// Forcing does not buy a worker that hands over one disk. That is impossible
// rather than ambiguous: dedicating the disk leaves the worker nothing to store
// on, and no flag should produce a node that stores nothing. The fleet gets the
// journal split instead, and the note names the worker with one disk.
func TestForcingDoesNotTakeAWorkersOnlyDisk(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
	if err != nil {
		t.Fatalf("a forced run over a worker with one disk was refused: %v", err)
	}
	if template.Template.EnableJournalDevice != nil {
		t.Fatal("a forced run took a worker's only disk, leaving it nothing to store on")
	}
	if !strings.Contains(strings.Join(template.Notes, " "), "worker-2") {
		t.Errorf("the notes %v have to name the worker with one disk", template.Notes)
	}
}

// An NVMe fleet with the same shape is not refused. The class has a fallback:
// with no device dedicated, the backend carves a journal partition out of every
// device, so an unset flag is a layout rather than a broken document.
func TestAnNVMeFleetWithNoJournalDiskIsNotRefused(t *testing.T) {
	plan := fleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes, twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err != nil {
		t.Fatalf("an NVMe fleet was refused, and its class has a journal layout for this: %v", err)
	}
	if template.Template.EnableJournalDevice != nil {
		t.Errorf("enableJournalDevice is %v, and an NVMe fleet of equal disks leaves it unset",
			*template.Template.EnableJournalDevice)
	}
}

// The disk a forced run takes is the same disk on every run over one fleet.
// Two runs proposing different documents for one unchanged fleet is a diff a
// reviewer cannot account for, and the addresses are what make it decidable.
func TestAForcedRunTakesTheSameDiskEveryTime(t *testing.T) {
	plan := blockFleetWithDisks([]uint64{twoTerabytes, twoTerabytes, twoTerabytes})

	first, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
	if err != nil {
		t.Fatalf("a forced run refused: %v", err)
	}
	for range 8 {
		again, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
		if err != nil {
			t.Fatalf("a forced run refused: %v", err)
		}
		if strings.Join(again.Notes, " ") != strings.Join(first.Notes, " ") {
			t.Fatalf("two runs over one fleet named different journal disks:\n %v\n %v",
				first.Notes, again.Notes)
		}
	}
}
