// What a maintenance window's AwaitingHost step waits for: the worker uncordoned
// and its storage-node API answering (design-storagenode.md §10).

package node

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Regression: 2026-10-13-maintenance-does-not-wait-for-the-uncordon -- a cordoned,
// never-rebooted worker answers at once, so the window restarted the node before
// the drain.
func TestAWindowWaitsForTheWorkerToBeUncordoned(t *testing.T) {
	for _, tc := range []struct {
		name              string
		present, cordoned bool
		answers           bool
		want              bool
	}{
		{"cordoned and answering", true, true, true, false},
		{"uncordoned and answering", true, false, true, true},
		{"uncordoned and silent", true, false, false, false},
		{"worker gone", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{}
			if tc.present {
				worker := aWorker(opsWorker, true)
				worker.Spec.Unschedulable = tc.cordoned
				objects = append(objects, worker)
			}
			r, _ := anOpsWorld(t, aControlPlane(), objects...)
			r.Workload.Probe = func(context.Context, string, string, bool, bool) error {
				if tc.answers {
					return nil
				}
				return errors.New("connection refused")
			}

			done, err := performing(t, r, aWindow("a-window"), stepAwaitingHost)
			if err != nil || done != tc.want {
				t.Errorf("done = %v, err = %v, want done = %v", done, err, tc.want)
			}
		})
	}
}
