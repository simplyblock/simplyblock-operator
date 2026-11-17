// What a draft tells its reviewer about the software-RAID arrays the deployment
// will stop to take their members.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"

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
