// What the draft proposes for the journal device, against the disks the run found.
//
// A fleet built with a small disk beside its big ones was built that way on
// purpose: the small one is there to carry the journal. Saying nothing about it
// spends that disk as storage and carves a journal partition out of every disk
// instead, which is the layout the hardware was chosen to avoid. The flag is
// immutable on the cluster it lands on, so the draft is the only place the
// choice can still be made.
//
// The shape the rule turns on is a worker whose smallest disk is smaller than
// every other disk it hands over. A tie is not that shape, and neither is a
// uniform fleet, and in both the draft stays quiet rather than giving up a disk
// nobody offered.

package discovery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// fleetWithDisks is a plan whose workers each hand over disks of the given
// sizes, in the NVMe class, which is what decides how the note names a disk.
func fleetWithDisks(sizes ...[]uint64) Plan {
	plan := Plan{Class: ClassNVMe}
	for index, worker := range sizes {
		devices := make([]nodeprobe.Device, 0, len(worker))
		for slot, size := range worker {
			devices = append(devices, disk(
				"nvme"+string(rune('0'+slot))+"n1",
				"0000:5e:0"+string(rune('0'+slot))+".0", 0, size))
		}
		plan.Workers = append(plan.Workers, Worker{
			Name:    "worker-" + string(rune('1'+index)),
			Class:   ClassNVMe,
			Devices: devices,
		})
	}
	return plan
}

func proposedJournalFlag(t *testing.T, plan Plan) (bool, bool) {
	t.Helper()
	template := ClusterTemplateFor("a-cluster", plan)
	if template.Template.EnableJournalDevice == nil {
		return false, false
	}
	return *template.Template.EnableJournalDevice, true
}

// The rule itself, over the fleets it has to tell apart.
func TestTheDraftProposesAJournalDeviceForAFleetThatHasOne(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		fleet   Plan
		propose bool
		why     string
	}{
		{
			name:    "one disk far smaller than the rest",
			fleet:   fleetWithDisks([]uint64{3 * tib, 3 * tib, 3 * tib, 32 * gib}),
			propose: true,
			why:     "the 32G disk is there to carry the journal",
		},
		{
			name:    "one disk smaller by a whole drive of capacity",
			fleet:   fleetWithDisks([]uint64{4 * tib, 4 * tib, 4 * tib, 2 * tib}),
			propose: true,
			why:     "the smallest is unique, which is the shape the rule turns on",
		},
		{
			name:    "two disks tie for smallest",
			fleet:   fleetWithDisks([]uint64{4 * tib, 4 * tib, 2 * tib, 2 * tib}),
			propose: false,
			why:     "neither of the two is the disk the fleet set aside",
		},
		{
			name:    "every disk the same size",
			fleet:   fleetWithDisks([]uint64{3 * tib, 3 * tib, 3 * tib, 3 * tib}),
			propose: false,
			why:     "no disk was set aside, so proposing it gives up a full drive",
		},
		{
			name:    "two disks, the smaller one set aside",
			fleet:   fleetWithDisks([]uint64{3 * tib, 32 * gib}),
			propose: true,
			why:     "one journal and one storage device is a node the backend accepts",
		},
		{
			name:    "a single disk",
			fleet:   fleetWithDisks([]uint64{3 * tib}),
			propose: false,
			why:     "dedicating the only disk would leave the node no storage at all",
		},
		{
			name: "one worker has the shape and another does not",
			fleet: fleetWithDisks(
				[]uint64{3 * tib, 3 * tib, 32 * gib},
				[]uint64{3 * tib, 3 * tib, 3 * tib}),
			propose: false,
			why:     "the flag is one field for the whole cluster and immutable once it lands",
		},
		{
			name:    "a disk whose size the probe could not read",
			fleet:   fleetWithDisks([]uint64{3 * tib, 3 * tib, 0}),
			propose: false,
			why:     "an unsized disk would be the smallest of anything",
		},
		{
			name:    "no workers at all",
			fleet:   Plan{},
			propose: false,
			why:     "there is no fleet to read a journal disk off",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value, set := proposedJournalFlag(t, testCase.fleet)
			if testCase.propose {
				if !set {
					t.Fatalf("the draft leaves enableJournalDevice unset: %s", testCase.why)
				}
				if !value {
					t.Errorf("the draft states enableJournalDevice false: %s", testCase.why)
				}
				return
			}
			if set {
				t.Errorf("the draft proposes enableJournalDevice=%v: %s", value, testCase.why)
			}
		})
	}
}

// A proposal that costs a drive says which drive, because the reviewer's job is
// to strike it if the fleet was not built that way.
func TestTheDraftSaysWhichDiskItGaveToTheJournal(t *testing.T) {
	template := ClusterTemplateFor("a-cluster",
		fleetWithDisks([]uint64{3 * tib, 3 * tib, 3 * tib, 32 * gib}))

	var found string
	for _, note := range template.Notes {
		if strings.Contains(note, "enableJournalDevice") {
			found = note
		}
	}
	if found == "" {
		t.Fatal("the draft sets the flag and the notes do not mention it")
	}
	if !strings.Contains(found, "32G") {
		t.Errorf("the note does not say how big the disk being given up is: %q", found)
	}
	if !strings.Contains(found, "worker-1") {
		t.Errorf("the note does not say which worker it was read off: %q", found)
	}

	// The disk is named the way the document names it. A note calling it
	// nvme3n1 sends the reviewer looking for a string the draft does not carry.
	plan := fleetWithDisks([]uint64{3 * tib, 3 * tib, 3 * tib, 32 * gib})
	address := plan.Workers[0].Class.Address(plan.Workers[0].Devices[3])
	if !strings.Contains(found, address) {
		t.Errorf("the note does not name the disk as the draft does (%s): %q", address, found)
	}
}

// The fleet with no journal disk is not silently left with a partitioned
// layout: a reviewer who expected the flag has to be able to see it was
// considered.
func TestAFleetWithNoJournalDiskIsSaidSo(t *testing.T) {
	template := ClusterTemplateFor("a-cluster",
		fleetWithDisks([]uint64{3 * tib, 3 * tib, 3 * tib, 3 * tib}))

	for _, note := range template.Notes {
		if strings.Contains(note, "enableJournalDevice") {
			return
		}
	}
	t.Error("the draft says nothing about the journal for a fleet of identical disks")
}

// namespaces builds a worker whose devices carry the addresses given, so that
// two namespaces of one controller can be expressed: the probe reports a device
// per namespace and the draft names the controller once.
func namespacesOn(name string, devices ...[2]any) Worker {
	out := Worker{Name: name, Class: ClassNVMe}
	for index, device := range devices {
		out.Devices = append(out.Devices, nodeprobe.Device{
			Name:       fmt.Sprintf("nvme%dn%d", index, index),
			Path:       fmt.Sprintf("/dev/nvme%dn%d", index, index),
			PCIAddress: device[0].(string),
			SizeBytes:  device[1].(uint64),
			Kind:       "Disk",
			Transport:  "NVMe",
			Available:  true,
			Content:    "Blank",
		})
	}
	return out
}

// Regression: 2026-09-28-journal-rule-counts-namespaces-not-devices. The rule
// read the probe's devices, and the draft names deduplicated class addresses: a
// controller with two namespaces is two devices and one entry in the document.
//
// Both halves of that mattered. A worker with one controller and two namespaces
// hands the draft a single device, and the rule proposed dedicating it, which
// leaves the node no storage at all — the case the single-disk guard exists to
// stop, walked around by counting namespaces. And where a second controller did
// exist, the smallest namespace was named as the journal disk while the address
// it was named by is the whole controller, so the note pointed at a device
// larger than the size beside it.
func TestTheJournalRuleReadsWhatTheDraftNames(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		worker  Worker
		propose bool
		why     string
	}{
		{
			name: "one controller, two namespaces, nothing else",
			worker: namespacesOn("worker-1",
				[2]any{"0000:5e:00.0", 3 * tib},
				[2]any{"0000:5e:00.0", 1 * tib}),
			propose: false,
			why:     "the draft names one device, and dedicating it leaves no storage",
		},
		{
			name: "two namespaces on one controller beside a whole one",
			worker: namespacesOn("worker-1",
				[2]any{"0000:5e:00.0", 3 * tib},
				[2]any{"0000:5e:00.0", 1 * tib},
				[2]any{"0000:5f:00.0", 3 * tib}),
			propose: true,
			why: "the controllers carry 4T and 3T, so the smaller is unique. " +
				"Counting namespaces would have made the 1T one the smallest instead",
		},
		{
			name: "namespaces that make the controllers unequal",
			worker: namespacesOn("worker-1",
				[2]any{"0000:5e:00.0", 2 * tib},
				[2]any{"0000:5e:00.0", 2 * tib},
				[2]any{"0000:5f:00.0", 1 * tib}),
			propose: true,
			why:     "one controller carries 4T and the other 1T, which is the shape",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := Plan{Class: ClassNVMe, Workers: []Worker{testCase.worker}}
			value, set := proposedJournalFlag(t, plan)
			if testCase.propose {
				if !set || !value {
					t.Fatalf("the draft does not propose enableJournalDevice: %s", testCase.why)
				}
				return
			}
			if set {
				t.Errorf("the draft proposes enableJournalDevice=%v: %s", value, testCase.why)
			}
		})
	}
}

// The note names a device the draft carries, and states the capacity of that
// device rather than of one namespace of it.
func TestTheJournalNoteNamesAnAddressTheDraftCarries(t *testing.T) {
	worker := namespacesOn("worker-1",
		[2]any{"0000:5e:00.0", 2 * tib},
		[2]any{"0000:5e:00.0", 2 * tib},
		[2]any{"0000:5f:00.0", 1 * tib})
	template := ClusterTemplateFor("a-cluster", Plan{Class: ClassNVMe, Workers: []Worker{worker}})

	var note string
	for _, candidate := range template.Notes {
		if strings.Contains(candidate, "enableJournalDevice") {
			note = candidate
		}
	}
	if note == "" {
		t.Fatal("no note about the journal device")
	}
	if !strings.Contains(note, "0000:5f:00.0") {
		t.Errorf("the note does not name the smaller controller: %q", note)
	}
	if strings.Contains(note, "0000:5e:00.0") {
		t.Errorf("the note names the larger controller as the journal disk: %q", note)
	}
	if !strings.Contains(note, "1T") {
		t.Errorf("the note does not state the controller's capacity: %q", note)
	}
}
