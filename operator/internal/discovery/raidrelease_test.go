// What a draft tells its reviewer about the software-RAID arrays the deployment
// will stop to take their members.

package discovery

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// arrayNotes runs the finding over a plan and returns what it recorded.
func arrayNotes(plan Plan) []string {
	plan.noteStoppedArrays()
	var notes []string
	for _, found := range plan.Findings {
		if found.Reason == RAIDArrayStopped {
			notes = append(notes, found.Message)
		}
	}
	return notes
}

// idleMember is a drafted device the probe refused only as the member of an
// array nothing uses.
func idleMember(name, array string) nodeprobe.Device {
	member := disk(name, "0000:00:04.0", 0, tb)
	member.Available = false
	member.Rejections = []nodeprobe.Rejection{{
		Reason: string(blockdev.ReasonIdleRAIDMember),
		Detail: "a member of the MD RAID array " + array + " (raid0), which nothing mounts, holds, or formatted",
	}}
	return member
}

// Regression: 2026-11-17-unused-md-array-members — drafting the members of an
// idle array means the deployment stops the array and erases the members' RAID
// superblocks, and a draft that did not say so would have a reviewer approve a
// destructive step they never saw.
func TestTheDraftSaysWhichArraysTheDeploymentStops(t *testing.T) {
	plan := Plan{Workers: []Worker{
		{Name: "worker-01", Devices: []nodeprobe.Device{idleMember("nvme0n1", "md0"), idleMember("nvme0n2", "md0")}},
		{Name: "worker-02", Devices: []nodeprobe.Device{disk("nvme0n1", "0000:00:04.0", 0, tb)}},
	}}

	notes := arrayNotes(plan)
	if len(notes) != 1 {
		t.Fatalf("the notes are %v, want one for the worker whose array is stopped", notes)
	}
	for _, want := range []string{"worker-01", "md0", "nvme0n1", "nvme0n2", "stop"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("the note %q does not say %q", notes[0], want)
		}
	}
}

// Regression: 2026-11-17-unused-md-array-members — one warning per worker named
// every member with its array, and with eight members of md0 ahead of one of md1
// it outgrew an event and was cut before md1: a reviewer approved stopping an
// array the warning never named.
func TestEveryArrayTheDeploymentStopsIsNamedOnItsOwn(t *testing.T) {
	devices := make([]nodeprobe.Device, 0, 9)
	for i := range 8 {
		devices = append(devices, idleMember(fmt.Sprintf("nvme%dn1", i), "md0"))
	}
	devices = append(devices, idleMember("nvme8n1", "md1"))
	notes := arrayNotes(Plan{Workers: []Worker{{Name: "worker-01", Devices: devices}}})

	if len(notes) != 2 {
		t.Fatalf("the notes are %v, want one for each array", notes)
	}
	for i, array := range []string{"md0", "md1"} {
		if !strings.Contains(notes[i], array) {
			t.Errorf("note %d, %q, does not name %s", i, notes[i], array)
		}
		if len(notes[i]) > 1024 {
			t.Errorf("note %d is %d bytes, more than an event carries", i, len(notes[i]))
		}
	}
}

// Regression: 2026-11-17-unused-md-array-members — a run whose filter or
// placement drafted only some members of an idle array drafted members the
// configure on the worker then could not take: it stops an array only when
// every member is a device the node takes, so the array stayed assembled and
// the drafted members stayed held.
func TestTheDraftTakesAnIdleArrayWholeOrNotAtAll(t *testing.T) {
	member := func(name, pci string) nodeprobe.Device {
		d := idleMember(name, "md0")
		d.PCIAddress = pci
		return d
	}
	fleet := []nodeprobe.Report{report("worker-1",
		member("nvme0n1", "0000:5e:00.0"),
		member("nvme1n1", "0000:5f:00.0"),
		disk("nvme2n1", "0000:60:00.0", 0, tb),
	)}

	whole := Planner{}.Plan(fleet, nil)
	if got := draftedNames(whole); !slices.Equal(got, []string{"nvme0n1", "nvme1n1", "nvme2n1"}) {
		t.Errorf("drafted %v from a worker whose whole array is free, refusals %+v", got, whole.Refusals)
	}

	run := &simplyblockv1alpha2.DiscoverSpec{DeviceFilter: &simplyblockv1alpha2.DeviceFilter{
		PcieAllowList: []string{"0000:5e:00.0", "0000:60:00.0"},
	}}
	partial := Planner{}.Plan(fleet, run)
	if got := draftedNames(partial); !slices.Equal(got, []string{"nvme2n1"}) {
		t.Fatalf("drafted %v when the filter left out a member of md0; it should leave out all of md0", got)
	}
	refusal := partial.Refusals[slices.IndexFunc(partial.Refusals, func(r Refusal) bool {
		return r.Device == "nvme0n1"
	})]
	for _, want := range []string{"md0", "nvme1n1"} {
		if !strings.Contains(refusal.Reason, want) {
			t.Errorf("the refusal %q of nvme0n1 does not name %s", refusal.Reason, want)
		}
	}
}

// draftedNames is every device a plan drafted, by name and in order.
func draftedNames(plan Plan) []string {
	var names []string
	for _, worker := range plan.Workers {
		for _, device := range worker.Devices {
			names = append(names, device.Name)
		}
	}
	slices.Sort(names)
	return names
}
