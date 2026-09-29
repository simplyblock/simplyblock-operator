// What a logical-block run makes of the captured QEMU worker, stated by name.
//
// HOST-03 is recorded like every other case, and a recording says only that the
// output has not changed since somebody read it. This file says what the output
// has to be, for the two devices the fleet was captured for: a machine whose
// only real storage is NVMe is deployable as logical block devices, and the
// optical drive beside them is not storage at all.
//
// Both are claims about one real export rather than about a rule, which is why
// they are here and not beside the rules. A rule test states what ClassRule or
// the candidacy pass does with a device somebody wrote down; this states what
// the whole pipeline does with a machine somebody has.

package deployment

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// qemuHostCase is the recorded case this file makes claims about.
const qemuHostCase = "host/host-03-a-captured-qemu-worker-with-an-optical-drive"

// The two Samsung namespaces and the drive, as the kernel presents them.
const (
	qemuFirstNVMe  = "/dev/nvme0n1"
	qemuSecondNVMe = "/dev/nvme1n1"
	qemuOptical    = "sr0"
)

// A block run takes this machine's NVMe disks, because the kernel presents a
// local namespace as a block device like any other and the class reaches every
// device through the kernel. Refusing them would leave the machine undeployable:
// the NVMe pair is the only storage on it, and which class a machine is
// deployed as is the administrator's statement rather than the bus's.
func TestCapturedQEMUWorkerOffersItsNVMeDisksByPath(t *testing.T) {
	document := runQEMUHostCase(t)

	offered := map[string]bool{}
	for _, set := range document.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.Devices == nil {
				continue
			}
			for _, path := range group.Devices.Block {
				offered[path] = true
			}
			if len(group.Devices.NVMe) > 0 {
				t.Errorf("group %s names %v under devices.nvme, and a block run names devices by path",
					group.Name, group.Devices.NVMe)
			}
		}
	}

	for _, path := range []string{qemuFirstNVMe, qemuSecondNVMe} {
		if !offered[path] {
			t.Errorf("%s is not offered; the document offers %v", path, names(offered))
		}
	}
	if len(offered) != 2 {
		t.Errorf("the document offers %v, want the two NVMe disks and nothing else", names(offered))
	}
}

// The optical drive has install media in it, so it reports a whole disk of
// 924 MB on the SATA bus and reads as blank. Nothing but the removable bit
// separates it from a disk, and a cluster that took it would lose the device
// the moment it was ejected, with no fault to diagnose.
//
// The refusal is asserted as well as the absence. A device missing from the
// document proves only that something declined it, and this is the one machine
// in the fixture tree that can say which rule did.
func TestCapturedQEMUWorkerRefusesItsOpticalDrive(t *testing.T) {
	document, refusals := runQEMUHostCaseWithRefusals(t)

	for _, set := range document.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.Devices == nil {
				continue
			}
			for _, path := range group.Devices.Block {
				if filepath.Base(path) == qemuOptical {
					t.Fatalf("group %s offers the optical drive at %s as backend storage",
						group.Name, path)
				}
			}
		}
	}

	var refused string
	for _, refusal := range refusals {
		if strings.Contains(refusal, "/"+qemuOptical+":") {
			refused = refusal
			break
		}
	}
	if refused == "" {
		t.Fatalf("no refusal names %s; the run reported %v", qemuOptical, refusals)
	}
	if !strings.Contains(refused, "Removable") {
		t.Errorf("the refusal of %s is %q, want it to rest on the device being removable",
			qemuOptical, refused)
	}
}

// runQEMUHostCase drives the recorded case and returns the document it wrote.
func runQEMUHostCase(t *testing.T) *simplyblockv1alpha2.ClusterDeploymentConfig {
	t.Helper()
	document, _ := runQEMUHostCaseWithRefusals(t)
	return document
}

// runQEMUHostCaseWithRefusals drives it and returns the refusals with it,
// failing when the run wrote no document at all.
func runQEMUHostCaseWithRefusals(
	t *testing.T,
) (*simplyblockv1alpha2.ClusterDeploymentConfig, []string) {
	t.Helper()

	loaded, err := loadDiscoveryCase(filepath.Join(caseRoot, filepath.FromSlash(qemuHostCase)))
	if err != nil {
		t.Fatalf("load %s: %v", qemuHostCase, err)
	}
	loaded.Name = qemuHostCase

	result := runDiscoveryCase(t, loaded)
	if result.Config == nil {
		t.Fatalf("the run wrote no document: %s", result.Failure)
	}
	return result.Config, result.Refusals
}

// names lists a set, ascending, for a failure that has to print it.
func names(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
