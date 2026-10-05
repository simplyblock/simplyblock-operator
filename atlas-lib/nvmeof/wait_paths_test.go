// The wait between a multipath connect and the paths it made serving I/O.
//
// A connect returns once a path's controller is live, and the kernel attaches
// the subsystem's namespaces to that controller one by one afterward. A check
// that runs in between sees a live controller serving some of the namespaces,
// which is the same picture a broken path presents, so the cases here are about
// telling the wait from the defect by waiting a bounded time for it to settle.

package nvmeof

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nvme"
)

// servedBy is a namespace whose paths run over the given controllers.
func servedBy(nsid nvme.NamespaceID, controllers ...string) nvme.Namespace {
	ns := nvme.Namespace{ID: nsid}
	for _, c := range controllers {
		ns.Paths = append(ns.Paths, nvme.Path{Controller: nvme.ControllerID(c), NSID: nsid})
	}
	return ns
}

// scanning is a subsystem with an established path on nvme0 and a new one on
// nvme1, which serves the first scanned namespaces of the two.
func scanning(scanned int) nvme.Subsystem {
	s := nvme.Subsystem{
		NQN: testNQN,
		Controllers: []nvme.Controller{
			ctrl("nvme0", "10.0.0.1", "live"),
			ctrl("nvme1", "10.0.0.2", "live"),
		},
	}
	for nsid := nvme.NamespaceID(1); nsid <= 2; nsid++ {
		if int(nsid) <= scanned {
			s.Namespaces = append(s.Namespaces, servedBy(nsid, "nvme0", "nvme1"))
		} else {
			s.Namespaces = append(s.Namespaces, servedBy(nsid, "nvme0"))
		}
	}
	return s
}

// Regression: 2026-10-05-migration-path-namespace-scan — a migration's paths
// were verified the moment the connect returned, while the kernel was still
// attaching the subsystem's namespaces to the new controller, and every batched
// migration spent a validation attempt on a path that was only scanning.
func TestWaitForPathsToServeWaitsForTheNamespaceScan(t *testing.T) {
	reads := 0
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		reads++
		if reads < 3 {
			return scanning(1), nil
		}
		return scanning(2), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitForPathsToServe(ctx, subs, testNQN); err != nil {
		t.Fatalf("the wait failed although the scan finished: %v", err)
	}
	if reads < 3 {
		t.Errorf("the subsystem was read %d time(s), so the wait returned before the new "+
			"controller served every namespace", reads)
	}
}

// Regression: 2026-10-05-migration-path-namespace-scan — a controller that
// never finishes is a defect rather than a scan, and the wait has to end and
// say which controller is short of which namespaces.
func TestWaitForPathsToServeNamesWhatNeverSettled(t *testing.T) {
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return scanning(1), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := WaitForPathsToServe(ctx, subs, testNQN)
	if err == nil {
		t.Fatal("the wait succeeded with a controller serving 1 of 2 namespaces")
	}
	if !strings.Contains(err.Error(), "nvme1") || !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("err = %q, want it to name nvme1 serving 1 of 2 namespaces", err)
	}
}

// A namespace with no per-controller path list is what the kernel publishes
// with native multipath off, and it says nothing about which controller serves
// it. The wait returns rather than waiting for a list that will never appear.
func TestWaitForPathsToServeAcceptsASubsystemWithoutPathLists(t *testing.T) {
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return nvme.Subsystem{
			NQN:         testNQN,
			Controllers: []nvme.Controller{ctrl("nvme0", "10.0.0.1", "live")},
			Namespaces:  []nvme.Namespace{{ID: 1}},
		}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := WaitForPathsToServe(ctx, subs, testNQN); err != nil {
		t.Errorf("the wait failed on a subsystem whose namespaces carry no path lists: %v", err)
	}
}
