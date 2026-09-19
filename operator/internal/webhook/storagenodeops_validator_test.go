// What the StorageNodeOps guard admits and refuses.
//
// The delete cases are the whole of it. The controller's teardown releases the
// node's lock and drops the finalizer from any step, so without this webhook a
// `kubectl delete` on a Promoting migrate is admitted, and the topology re-point
// the operation still owes goes with it.
//
// The last test is the one that keeps the guard honest over time: the refusal
// table is checked against the graph's own answer, so a step that becomes
// abortable, or a new step that is not, cannot leave the two disagreeing.

package webhook

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/node"
)

// opsDeleteRequest is the request the API server sends on a DELETE: the object
// being removed arrives in OldObject, and there is no new one.
func opsDeleteRequest(t *testing.T, ops runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal the operation: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{Raw: raw},
	}}
}

func runningNodeOpsAt(step simplyblockv1alpha2.StorageNodeOpsStep) *simplyblockv1alpha2.StorageNodeOps {
	return &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-migration", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			NodeRef: "worker-3",
			Action:  simplyblockv1alpha2.StorageNodeOpsActionMigrate,
		},
		Status: simplyblockv1alpha2.StorageNodeOpsStatus{
			Phase: simplyblockv1alpha2.StorageNodeOpsPhaseRunning,
			Step:  statemachine.KubeSnapshot{State: string(step)},
		},
	}
}

// A record may not be withdrawn from a step the graph declares no abort edge
// from. Each of these has taken the node out of service or moved it, and the
// operation is the only thing that finishes what it started.
func TestStorageNodeOpsDeleteIsRefusedWhereNoAbortEdgeExists(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	for _, step := range node.UnabortableSteps() {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), opsDeleteRequest(t, runningNodeOpsAt(step)))
			if resp.Allowed {
				t.Fatalf("the record was withdrawn at step %s, which cannot be aborted", step)
			}
			if !strings.Contains(resp.Result.Message, string(step)) {
				t.Errorf("the refusal is %q, want it to name the step", resp.Result.Message)
			}
		})
	}
}

// Promoting is the case design-storagenode.md §11 names: the promote has
// re-homed the logical volumes and the record is what carries the topology
// re-point still owed, so the refusal says so rather than naming the step alone.
func TestTheRefusalAtPromotingSaysWhatTheRecordStillOwes(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	resp := v.Handle(context.Background(),
		opsDeleteRequest(t, runningNodeOpsAt(simplyblockv1alpha2.StorageNodeOpsStepPromoting)))
	if resp.Allowed {
		t.Fatal("a Promoting migrate was withdrawn")
	}
	if !strings.Contains(resp.Result.Message, "topology") {
		t.Errorf("the refusal is %q, want it to name the re-point the record carries",
			resp.Result.Message)
	}
}

// A step the graph can abort from is one whose unwind exists, so withdrawing the
// record there leaves nothing the finalizer cannot finish.
func TestStorageNodeOpsDeleteIsAllowedWhereTheAbortEdgeExists(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	for _, step := range []simplyblockv1alpha2.StorageNodeOpsStep{
		simplyblockv1alpha2.StorageNodeOpsStepRequesting,
		simplyblockv1alpha2.StorageNodeOpsStepValidating,
		simplyblockv1alpha2.StorageNodeOpsStepSuspending,
		simplyblockv1alpha2.StorageNodeOpsStepMigratingVolumes,
		simplyblockv1alpha2.StorageNodeOpsStepVerifying,
		simplyblockv1alpha2.StorageNodeOpsStepPreparing,
		simplyblockv1alpha2.StorageNodeOpsStepHolding,
	} {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), opsDeleteRequest(t, runningNodeOpsAt(step)))
			if !resp.Allowed {
				t.Errorf("the record was held at step %s, which an abort stops cleanly: %s",
					step, resp.Result.Message)
			}
		})
	}
}

// A terminal operation is a record of work that has finished, so withdrawing it
// stops nothing whatever step it ended on.
func TestStorageNodeOpsDeleteIsAllowedOnceTerminal(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	for _, phase := range []simplyblockv1alpha2.StorageNodeOpsPhase{
		simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageNodeOpsPhaseFailed,
		simplyblockv1alpha2.StorageNodeOpsPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ops := runningNodeOpsAt(simplyblockv1alpha2.StorageNodeOpsStepPromoting)
			ops.Status.Phase = phase

			resp := v.Handle(context.Background(), opsDeleteRequest(t, ops))
			if !resp.Allowed {
				t.Errorf("a %s operation could not be deleted: %s", phase, resp.Result.Message)
			}
		})
	}
}

// An operation that never started holds nothing and is on no step.
func TestStorageNodeOpsDeleteIsAllowedBeforeTheOperationStarted(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	ops := runningNodeOpsAt("")
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending

	resp := v.Handle(context.Background(), opsDeleteRequest(t, ops))
	if !resp.Allowed {
		t.Errorf("a Pending operation could not be deleted: %s", resp.Result.Message)
	}
}

// Nothing to read is nothing to refuse on. Blocking a delete on the basis of no
// information is the one answer that cannot be justified.
func TestStorageNodeOpsDeleteIsAllowedWithNoObjectToRead(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	resp := v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete},
	})
	if !resp.Allowed {
		t.Errorf("a delete carrying no object was refused: %s", resp.Result.Message)
	}
}

// The guard is registered on DELETE alone, and answers nothing else even if it
// is asked.
func TestStorageNodeOpsCreateIsNotThisGuardsBusiness(t *testing.T) {
	v := &StorageNodeOpsValidator{}

	req := opsDeleteRequest(t, runningNodeOpsAt(simplyblockv1alpha2.StorageNodeOpsStepPromoting))
	req.Operation = admissionv1.Create

	if resp := v.Handle(context.Background(), req); !resp.Allowed {
		t.Errorf("a create was refused by the delete guard: %s", resp.Result.Message)
	}
}

// The refusal table and the graph are two statements of one rule, and this is
// what makes them agree. A deletion may not express something spec.abort could
// not, so a step in one set and not the other is a defect either way round: an
// extra entry refuses a delete the abort channel would have honored, and a
// missing one admits the withdrawal of a record nothing else accounts for.
func TestTheNodeRefusalTableAndTheGraphAgree(t *testing.T) {
	refused := make([]simplyblockv1alpha2.StorageNodeOpsStep, 0, len(undeletableNodeSteps))
	for step := range undeletableNodeSteps {
		refused = append(refused, step)
	}
	slices.Sort(refused)

	if want := node.UnabortableSteps(); !slices.Equal(refused, want) {
		t.Errorf("the guard refuses %v, and the graph declares no abort edge from %v", refused, want)
	}
}

// A refusal that names the step and nothing else tells the reader what they may
// not do and never why.
func TestEveryUndeletableNodeStepStatesWhatItIsDoing(t *testing.T) {
	for step, doing := range undeletableNodeSteps {
		if doing == "" {
			t.Errorf("%s is undeletable and the refusal says nothing about why", step)
		}
	}
}
