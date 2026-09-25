// What a run concludes about the fleet's operating system, and when it refuses
// to conclude anything.
//
// The cases that matter are the disagreements. One document states one host OS
// because one cluster runs one DaemonSet, so a fleet whose workers differ has
// no answer to state, and the interesting question is whether the run says so
// or averages it away.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/inventory"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// runningOS is a worker whose probe read this distribution.
func runningOS(name, distro, family, version string) Worker {
	return Worker{
		Name: name,
		Report: nodeprobe.Report{
			Node: name,
			HostOS: nodeprobe.HostOS{
				Distro:       distro,
				Family:       family,
				Version:      version,
				Architecture: "x86_64",
			},
		},
	}
}

func TestHostOSForStatesWhatEveryWorkerAgreesOn(t *testing.T) {
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "ubuntu", "Debian", "22.04"),
		runningOS("worker-02", "ubuntu", "Debian", "22.04"),
		runningOS("worker-03", "ubuntu", "Debian", "22.04"),
	}}

	os, notes := HostOSFor(plan)
	if os == nil {
		t.Fatalf("stated no host OS for a fleet that agrees: %v", notes)
	}
	if os.Distro != simplyblockv1alpha2.DistroUbuntu {
		t.Errorf("stated the distro %q, want %q", os.Distro, simplyblockv1alpha2.DistroUbuntu)
	}
	if os.Family != simplyblockv1alpha2.HostOSFamilyDebian {
		t.Errorf("stated the family %q, want %q", os.Family, simplyblockv1alpha2.HostOSFamilyDebian)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "ubuntu") {
		t.Errorf("the notes are %v, and one of them has to name what was read", notes)
	}
}

func TestHostOSForRefusesToStateOneForAFleetThatDisagrees(t *testing.T) {
	// One document becomes one cluster, and one cluster runs one storage-node
	// DaemonSet with one UBUNTU_HOST in it. There is no per-worker answer to
	// state, so a fleet that disagrees gets none and a reviewer is told which
	// workers run what.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "ubuntu", "Debian", "22.04"),
		runningOS("worker-02", "rocky", "RedHat", "9.4"),
		runningOS("worker-03", "ubuntu", "Debian", "22.04"),
	}}

	os, notes := HostOSFor(plan)
	if os != nil {
		t.Fatalf("stated %+v for a fleet running two distributions", os)
	}
	if len(notes) != 1 {
		t.Fatalf("the notes are %v, want the one that says the fleet disagrees", notes)
	}
	for _, want := range []string{"ubuntu", "rocky", "worker-02", "hostOS"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("the note is %q, and it does not name %q", notes[0], want)
		}
	}
}

func TestHostOSForRefusesToStateOneNobodyRead(t *testing.T) {
	// A probe that was not given the host's root filesystem reports no distro,
	// and a document that stated an empty one would read as a fleet whose OS is
	// known to be nothing.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "", "", ""),
		runningOS("worker-02", "", "", ""),
	}}

	os, notes := HostOSFor(plan)
	if os != nil {
		t.Fatalf("stated %+v for a fleet whose OS nothing read", os)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "os-release") {
		t.Errorf("the notes are %v, want the one that says nothing was read", notes)
	}
}

func TestHostOSForStatesTheFamilyItCanConcludeWhenTheProbeDidNot(t *testing.T) {
	// An older probe reports the distro and no family. The family is the same
	// table either way, so the run concludes it rather than leaving the
	// document less than what was read.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "rocky", "", "9.4"),
		runningOS("worker-02", "rocky", "", "9.4"),
	}}

	os, _ := HostOSFor(plan)
	if os == nil || os.Family != simplyblockv1alpha2.HostOSFamilyRedHat {
		t.Fatalf("stated %+v, want the family %q concluded from the distro", os, inventory.OSFamilyRedHat)
	}
}

func TestHostOSForStatesNoFamilyForAHostThatHasNone(t *testing.T) {
	// Talos has no package manager, so it belongs to no packaging family, and
	// the distro alone is what the document states.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "talos", "", "v1.7.5"),
		runningOS("worker-02", "talos", "", "v1.7.5"),
	}}

	os, _ := HostOSFor(plan)
	if os == nil || os.Distro != "talos" || os.Family != "" {
		t.Fatalf("stated %+v, want talos with no family", os)
	}
}

func TestHostOSForIgnoresAWorkerTheDraftLeftOut(t *testing.T) {
	// The plan's workers are the ones the draft names. A machine refused for
	// having no disks is not part of the deployment and its distribution is not
	// a disagreement about it.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "ubuntu", "Debian", "22.04"),
		runningOS("worker-02", "ubuntu", "Debian", "22.04"),
	}}

	os, _ := HostOSFor(plan)
	if os == nil || os.Distro != simplyblockv1alpha2.DistroUbuntu {
		t.Fatalf("stated %+v, want ubuntu", os)
	}
}
