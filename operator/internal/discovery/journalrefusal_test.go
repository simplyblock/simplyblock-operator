// What a logical block-device run does when the fleet does not say which disk
// the journal goes on.
//
// It refuses, where an NVMe run stays quiet. The two classes differ because the
// fallback differs: an NVMe cluster with no dedicated journal device carves a
// journal partition out of every device, so "left unset" is a layout the backend
// builds. A block cluster has no such fallback, because the control plane
// refuses partitioned-journal mode for the class outright, so a block draft with
// the flag unset is a document that cannot deploy: every worker's node_add fails
// on it after the cluster has already been created and the drives formatted.
//
// The refusal is therefore the point of this file, and so is what it does not
// refuse: a fleet the run can read is still drafted, and a fleet whose shape is
// merely ambiguous is drafted when the run was told to force it.

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

// A block fleet whose disks do not name a journal device is refused, and the
// refusal names the worker and what it found. Drafting it anyway produces a
// document whose every node_add fails, after the cluster exists and the drives
// have been formatted, which is the failure this replaces.
func TestABlockFleetWithNoJournalDiskIsRefused(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes, twoTerabytes},
	)

	_, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err == nil {
		t.Fatal("the run drafted a block cluster with no journal device, which cannot deploy")
	}
	for _, want := range []string{"worker-1", "forceJournalDevice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal reads %q, and it has to name %q", err, want)
		}
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

// A block fleet that does name a journal disk is drafted without being forced,
// and the flag is proposed. Nothing about the refusal changes the fleet that was
// always readable.
func TestABlockFleetWithAJournalDiskIsDraftedUnforced(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{oneTerabyte, twoTerabytes},
		[]uint64{oneTerabyte, twoTerabytes},
	)

	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{})
	if err != nil {
		t.Fatalf("a readable fleet was refused: %v", err)
	}
	if template.Template.EnableJournalDevice == nil || !*template.Template.EnableJournalDevice {
		t.Error("a fleet with a sole smallest disk did not get enableJournalDevice")
	}
}

// Forcing does not buy a worker that hands over one disk. That is impossible
// rather than ambiguous: dedicating the disk leaves the worker nothing to store
// on, and no flag should produce a node that stores nothing.
func TestForcingDoesNotTakeAWorkersOnlyDisk(t *testing.T) {
	plan := blockFleetWithDisks(
		[]uint64{twoTerabytes, twoTerabytes},
		[]uint64{twoTerabytes},
	)

	_, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{ForceJournalDevice: true})
	if err == nil {
		t.Fatal("a forced run took a worker's only disk, leaving it nothing to store on")
	}
	if !strings.Contains(err.Error(), "worker-2") {
		t.Errorf("the refusal reads %q, and it has to name the worker with one disk", err)
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
