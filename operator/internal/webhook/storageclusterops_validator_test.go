// What the StorageClusterOps guard admits and refuses.
//
// A rolling restart is the case that motivates it. The walk holds the cluster's
// lock across every node, and from the shutdown of a node to its restart that
// operation is the only thing that will bring the node back. A forced delete
// there skips the finalizer, so the lock stays on a cluster whose object is gone
// and a storage node stays down with nothing driving it up.

package webhook

import (
	"context"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
)

func runningClusterOpsAt(
	step simplyblockv1alpha2.StorageClusterOpsStep,
) *simplyblockv1alpha2.StorageClusterOps {
	return &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-rolling-restart", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
			ClusterRef: "simplyblock",
			Action:     simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		},
		Status: simplyblockv1alpha2.StorageClusterOpsStatus{
			Phase: simplyblockv1alpha2.StorageClusterOpsPhaseRunning,
			Step:  statemachine.KubeSnapshot{State: string(step)},
		},
	}
}

// A record may not be withdrawn from a step the graph declares no abort edge
// from: the cluster or one of its nodes is mid-flight, and this operation is
// what carries it the rest of the way.
func TestStorageClusterOpsDeleteIsRefusedWhereNoAbortEdgeExists(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	for _, step := range cluster.UnabortableSteps() {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), opsDeleteRequest(t, runningClusterOpsAt(step)))
			if resp.Allowed {
				t.Fatalf("the record was withdrawn at step %s, which cannot be aborted", step)
			}
			if !strings.Contains(resp.Result.Message, string(step)) {
				t.Errorf("the refusal is %q, want it to name the step", resp.Result.Message)
			}
		})
	}
}

// The four middle steps of a rolling restart are the sharpest case, because the
// node is offline across all of them. The refusal says that rather than naming
// the step alone.
func TestTheRefusalMidWalkSaysTheNodeIsDown(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	for _, step := range []simplyblockv1alpha2.StorageClusterOpsStep{
		simplyblockv1alpha2.StorageClusterOpsStepShuttingDownNode,
		simplyblockv1alpha2.StorageClusterOpsStepRefreshingPod,
		simplyblockv1alpha2.StorageClusterOpsStepAwaitingPod,
		simplyblockv1alpha2.StorageClusterOpsStepRestartingNode,
	} {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), opsDeleteRequest(t, runningClusterOpsAt(step)))
			if resp.Allowed {
				t.Fatalf("the walk's record was withdrawn at step %s", step)
			}
			if !strings.Contains(resp.Result.Message, "offline") {
				t.Errorf("the refusal is %q, want it to say the node is down", resp.Result.Message)
			}
		})
	}
}

// Requesting has issued nothing, CheckingPeers performs no side effect, and
// Rebalancing is the cluster settling on its own, which it finishes whether or
// not this operation is watching.
func TestStorageClusterOpsDeleteIsAllowedWhereTheAbortEdgeExists(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	for _, step := range []simplyblockv1alpha2.StorageClusterOpsStep{
		simplyblockv1alpha2.StorageClusterOpsStepRequesting,
		simplyblockv1alpha2.StorageClusterOpsStepCheckingPeers,
		simplyblockv1alpha2.StorageClusterOpsStepRebalancing,
	} {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), opsDeleteRequest(t, runningClusterOpsAt(step)))
			if !resp.Allowed {
				t.Errorf("the record was held at step %s, which an abort stops cleanly: %s",
					step, resp.Result.Message)
			}
		})
	}
}

// A terminal operation is a record of work that has finished, so withdrawing it
// stops nothing whatever step it ended on.
func TestStorageClusterOpsDeleteIsAllowedOnceTerminal(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	for _, phase := range []simplyblockv1alpha2.StorageClusterOpsPhase{
		simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
		simplyblockv1alpha2.StorageClusterOpsPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ops := runningClusterOpsAt(simplyblockv1alpha2.StorageClusterOpsStepRestartingNode)
			ops.Status.Phase = phase

			resp := v.Handle(context.Background(), opsDeleteRequest(t, ops))
			if !resp.Allowed {
				t.Errorf("a %s operation could not be deleted: %s", phase, resp.Result.Message)
			}
		})
	}
}

// An operation waiting for the cluster's lock holds nothing and is on no step.
func TestStorageClusterOpsDeleteIsAllowedBeforeTheOperationStarted(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	ops := runningClusterOpsAt("")
	ops.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhasePending

	resp := v.Handle(context.Background(), opsDeleteRequest(t, ops))
	if !resp.Allowed {
		t.Errorf("a Pending operation could not be deleted: %s", resp.Result.Message)
	}
}

// Nothing to read is nothing to refuse on.
func TestStorageClusterOpsDeleteIsAllowedWithNoObjectToRead(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	resp := v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete},
	})
	if !resp.Allowed {
		t.Errorf("a delete carrying no object was refused: %s", resp.Result.Message)
	}
}

// The guard is registered on DELETE alone, and answers nothing else even if it
// is asked.
func TestStorageClusterOpsCreateIsNotThisGuardsBusiness(t *testing.T) {
	v := &StorageClusterOpsValidator{}

	req := opsDeleteRequest(t,
		runningClusterOpsAt(simplyblockv1alpha2.StorageClusterOpsStepRestartingNode))
	req.Operation = admissionv1.Create

	if resp := v.Handle(context.Background(), req); !resp.Allowed {
		t.Errorf("a create was refused by the delete guard: %s", resp.Result.Message)
	}
}

// The refusal table and the graph are two statements of one rule, and this is
// what makes them agree. A deletion may not express something spec.abort could
// not, so a step in one set and not the other is a defect either way round.
func TestTheClusterRefusalTableAndTheGraphAgree(t *testing.T) {
	refused := make([]simplyblockv1alpha2.StorageClusterOpsStep, 0, len(undeletableClusterSteps))
	for step := range undeletableClusterSteps {
		refused = append(refused, step)
	}
	slices.Sort(refused)

	if want := cluster.UnabortableSteps(); !slices.Equal(refused, want) {
		t.Errorf("the guard refuses %v, and the graph declares no abort edge from %v", refused, want)
	}
}

// A refusal that names the step and nothing else tells the reader what they may
// not do and never why.
func TestEveryUndeletableClusterStepStatesWhatItIsDoing(t *testing.T) {
	for step, doing := range undeletableClusterSteps {
		if doing == "" {
			t.Errorf("%s is undeletable and the refusal says nothing about why", step)
		}
	}
}