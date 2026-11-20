// What a run concludes about the fleet's operating system, and what it drafts
// for the workers that run another one.
//
// The cases that matter are the disagreements. The document states one host OS
// for the fleet only where every worker agrees, and a fleet whose workers differ
// is drafted as groups that each state their own, so the interesting question
// is whether the run says so or averages it away.

package discovery

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"

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
	if os.Version != "22.04" {
		t.Errorf("stated the version %q, want the 22.04 every worker read", os.Version)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "ubuntu") || !strings.Contains(notes[0], "22.04") {
		t.Errorf("the notes are %v, and one of them has to name what was read", notes)
	}
}

func TestHostOSForStatesTheDistroAndNoVersionForAFleetOfTwoReleases(t *testing.T) {
	// Amazon Linux 2 and Amazon Linux 2023 share the ID amzn. The fleet agrees
	// on the distribution, so that is stated, and disagrees on the release, so
	// no version is, and a reviewer is told which workers run which.
	plan := Plan{Workers: []Worker{
		runningOS("worker-01", "amzn", "RedHat", "2"),
		runningOS("worker-02", "amzn", "RedHat", "2023"),
		runningOS("worker-03", "amzn", "RedHat", "2023"),
	}}

	os, notes := HostOSFor(plan)
	if os == nil || os.Distro != "amzn" {
		t.Fatalf("stated %+v, want amzn", os)
	}
	if os.Version != "" {
		t.Errorf("stated the version %q for a fleet running two releases", os.Version)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"worker-01", "2023", "version"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the notes are %v, and none of them names %q", notes, want)
		}
	}
}

func TestHostOSForRefusesToStateOneForAFleetThatDisagrees(t *testing.T) {
	// A fleet that disagrees has no one host OS for the document to state, so
	// it states none, its groups state their own, and a reviewer is told which
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

// osPlan plans a uniform fleet whose workers worker-1, worker-2, and so on read
// the operating systems given, each a distro and a version, in that order.
func osPlan(oses ...[2]string) Plan {
	reports := uniformFleet(len(oses))
	for i, os := range oses {
		reports[i].HostOS = nodeprobe.HostOS{Distro: os[0], Version: os[1], Architecture: "x86_64"}
	}
	return Planner{}.Plan(reports, nil)
}

// hostOSByWorker maps every drafted worker to its group's host OS.
func hostOSByWorker(plan Plan) map[string]*simplyblockv1alpha2.HostOSSpec {
	out := map[string]*simplyblockv1alpha2.HostOSSpec{}
	for _, set := range plan.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				out[worker] = group.HostOS
			}
		}
	}
	return out
}

func TestAFleetOnTwoOperatingSystemsIsDraftedAsGroupsStatingTheirOwn(t *testing.T) {
	// An EKS cluster with an Ubuntu node group beside Amazon Linux ones: the
	// document states no host OS, so every group states the one its workers
	// run, and identical hardware on two operating systems is two groups.
	plan := osPlan([2]string{"ubuntu", "22.04"}, [2]string{"amzn", "2023"}, [2]string{"amzn", "2023"})

	want := map[string]simplyblockv1alpha2.HostOSSpec{
		"worker-1": {Distro: "ubuntu", Family: simplyblockv1alpha2.HostOSFamilyDebian, Version: "22.04"},
		"worker-2": {Distro: "amzn", Family: simplyblockv1alpha2.HostOSFamilyRedHat, Version: "2023"},
		"worker-3": {Distro: "amzn", Family: simplyblockv1alpha2.HostOSFamilyRedHat, Version: "2023"},
	}
	got := hostOSByWorker(plan)
	for worker, os := range want {
		if got[worker] == nil || !equality.Semantic.DeepEqual(*got[worker], os) {
			t.Errorf("%s is drafted with hostOS %+v, want %+v", worker, got[worker], os)
		}
	}
	if n := groupCount(plan); n != 2 {
		t.Errorf("the draft has %d group(s), want one per operating system", n)
	}
}

func TestAFleetOfTwoReleasesIsDraftedAsGroupsStatingTheirRelease(t *testing.T) {
	// The document states amzn and no version, which no worker runs exactly,
	// so each group states its release.
	plan := osPlan([2]string{"amzn", "2"}, [2]string{"amzn", "2023"})

	got := hostOSByWorker(plan)
	for worker, version := range map[string]string{"worker-1": "2", "worker-2": "2023"} {
		if got[worker] == nil || got[worker].Version != version {
			t.Errorf("%s is drafted with hostOS %+v, want the release %q", worker, got[worker], version)
		}
	}
}

func TestAUniformFleetIsDraftedWithNoGroupHostOS(t *testing.T) {
	// The document states what every worker runs, so no group repeats it.
	plan := osPlan([2]string{"ubuntu", "22.04"}, [2]string{"ubuntu", "22.04"}, [2]string{"ubuntu", "22.04"})

	for worker, os := range hostOSByWorker(plan) {
		if os != nil {
			t.Errorf("%s is drafted with hostOS %+v, which the document already states", worker, *os)
		}
	}
	if n := groupCount(plan); n != 1 {
		t.Errorf("the draft has %d group(s), want the one a uniform fleet is", n)
	}
}
