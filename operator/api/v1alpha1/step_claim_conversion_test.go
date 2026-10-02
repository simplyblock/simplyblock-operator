// Tests that a claim on an operation's step survives a v1alpha1 round trip. A
// client that reads, modifies, and writes the object at v1alpha1 sends it back
// through both conversions, and a step v1alpha1 can spell carries no deadline
// annotation, so a claim that is not stashed with it is dropped. A dropped claim
// is a lease erased, and the next pass makes the step's call again at once.

package v1alpha1

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// claimedStep is a step v1alpha1 can spell, with no deadline and a live claim.
func claimedStep(state string) statemachine.KubeSnapshot {
	return statemachine.KubeSnapshot{
		State: state,
		Claim: &statemachine.KubeClaim{
			State:      state,
			Attempt:    1,
			LeaseUntil: metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second)),
		},
	}
}

func TestAStorageClusterOpsStepClaimSurvivesAV1alpha1RoundTrip(t *testing.T) {
	hub := &v1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: v1alpha2.StorageClusterOpsSpec{
			ClusterRef: "production",
			Action:     v1alpha2.StorageClusterOpsActionRollingRestart,
		},
		Status: v1alpha2.StorageClusterOpsStatus{
			Phase: v1alpha2.StorageClusterOpsPhaseRunning,
			Step:  claimedStep(string(v1alpha2.StorageClusterOpsStepShuttingDownNode)),
			RollingRestart: &v1alpha2.RollingRestartStatus{
				Nodes: []string{"node-a", "node-b"},
			},
		},
	}

	var spoke StorageClusterOps
	if err := spoke.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageClusterOps
	if err := spoke.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub.Status.Step, back.Status.Step); diff != "" {
		t.Errorf("the round trip changed the step (-before +after):\n%s", diff)
	}
}

func TestAStorageNodeOpsStepClaimSurvivesAV1alpha1RoundTrip(t *testing.T) {
	hub := &v1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: v1alpha2.StorageNodeOpsSpec{
			NodeRef: "node-1",
			Action:  v1alpha2.StorageNodeOpsActionRemove,
		},
		Status: v1alpha2.StorageNodeOpsStatus{
			Phase: v1alpha2.StorageNodeOpsPhaseRunning,
			Step:  claimedStep(string(v1alpha2.StorageNodeOpsStepSuspending)),
		},
	}

	var spoke StorageNodeOps
	if err := spoke.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageNodeOps
	if err := spoke.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub.Status.Step, back.Status.Step); diff != "" {
		t.Errorf("the round trip changed the step (-before +after):\n%s", diff)
	}
}
