// What a run says about workers whose kernel cannot mount a volume over more
// than one path.
//
// Such a worker is still a storage node: the storage engine runs in user space
// and does not use the kernel's NVMe initiator. The note is for the reviewer
// who would otherwise schedule workloads with volumes onto it.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/inventory"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// withMultipath is a worker whose probe read this native NVMe multipath state.
func withMultipath(name string, state inventory.MultipathState) Worker {
	worker := runningOS(name, "amzn", "RedHat", "2023")
	worker.Report.HostOS.Kernel = nodeprobe.Kernel{
		Release:       "6.1.155-176.282.amzn2023.x86_64",
		NVMeMultipath: nodeprobe.Fact{State: string(state), Detail: "the evidence"},
	}
	return worker
}

// multipathNotes runs the finding over a plan and returns what it recorded.
func multipathNotes(plan Plan) []string {
	plan.noteMultipath()
	var notes []string
	for _, found := range plan.Findings {
		if found.Reason == NVMeMultipathUnavailable {
			notes = append(notes, found.Message)
		}
	}
	return notes
}

func TestTheRunNamesTheWorkersThatCannotMountOverSeveralPaths(t *testing.T) {
	plan := Plan{Workers: []Worker{
		withMultipath("worker-01", inventory.MultipathNotBuilt),
		withMultipath("worker-02", inventory.MultipathEnabled),
		withMultipath("worker-03", inventory.MultipathDisabled),
	}}

	notes := multipathNotes(plan)
	if len(notes) != 2 {
		t.Fatalf("the notes are %v, want one for each worker that cannot mount over several paths", notes)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"worker-01", "worker-03", "CONFIG_NVME_MULTIPATH", "nvme_core.multipath"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the notes %v do not say %q", notes, want)
		}
	}
	if strings.Contains(joined, "worker-02") {
		t.Errorf("the notes %v name worker-02, whose multipath is enabled", notes)
	}
}

func TestTheRunNamesNoWorkerWithoutAnAnswer(t *testing.T) {
	// An unread state is not evidence of a problem. A probe that could not read
	// the kernel already says so among its unreadable readings.
	plan := Plan{Workers: []Worker{
		withMultipath("worker-01", inventory.MultipathEnabled),
		withMultipath("worker-02", inventory.MultipathUnknown),
		withMultipath("worker-03", ""),
	}}

	if notes := multipathNotes(plan); len(notes) != 0 {
		t.Errorf("the notes are %v for a fleet with no worker known to lack multipath", notes)
	}
}
