// Tests that SimplyblockDriver.spec.pnfs.mds.unhealthyDeadline reaches the
// runner, the only lever short of pinning an image (SFAM-2857).

package driver

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMDSRunnerTakesTheUnhealthyDeadline(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.UnhealthyDeadline = &metav1.Duration{Duration: 0}
	_, sts, err := MDSObjects(d, testClusterID, "simplyblock-state", nil)
	if err != nil {
		t.Fatalf("MDSObjects: %v", err)
	}
	if args := runnerContainer(t, sts).Args; !slices.Contains(args, "--unhealthy-deadline=0s") {
		t.Errorf("runner args = %v, want --unhealthy-deadline=0s", args)
	}

	d.Spec.PNFS.MDS.UnhealthyDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	_, sts, _ = MDSObjects(d, testClusterID, "simplyblock-state", nil)
	if args := runnerContainer(t, sts).Args; !slices.Contains(args, "--unhealthy-deadline=10m0s") {
		t.Errorf("runner args = %v, want --unhealthy-deadline=10m0s", args)
	}
}

// Unset leaves the runner's own default in force.
func TestMDSRunnerWithoutAnUnhealthyDeadlineKeepsItsDefault(t *testing.T) {
	_, sts, err := MDSObjects(withMDS(testDriver("simplyblock")), testClusterID, "simplyblock-state", nil)
	if err != nil {
		t.Fatalf("MDSObjects: %v", err)
	}
	for _, arg := range runnerContainer(t, sts).Args {
		if strings.HasPrefix(arg, "--unhealthy-deadline") {
			t.Errorf("runner args carry %q with the field unset", arg)
		}
	}
}
