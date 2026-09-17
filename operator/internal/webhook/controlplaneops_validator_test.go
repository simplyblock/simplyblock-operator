// What the ControlPlaneOps guard admits and refuses.
//
// The delete cases matter most. The controller's deletion path releases the
// control plane's lock and drops the finalizer from any phase, so without this
// webhook the record of a running rollout can be withdrawn while the rollout is
// still moving, and the next operation starts against it.

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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
)

func cpOpsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("build the scheme: %v", err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("build the scheme: %v", err)
	}
	return scheme
}

func cpOpsValidator(t *testing.T) *ControlPlaneOpsValidator {
	t.Helper()
	return &ControlPlaneOpsValidator{
		Client: fake.NewClientBuilder().WithScheme(cpOpsScheme(t)).Build(),
	}
}

func deleteRequestFor(t *testing.T, ops *simplyblockv1alpha2.ControlPlaneOps) admission.Request {
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

func runningOpsAt(step simplyblockv1alpha2.ControlPlaneOpsStep) *simplyblockv1alpha2.ControlPlaneOps {
	return &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: "an-operation", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: "simplyblock",
			Action:          simplyblockv1alpha2.ControlPlaneOpsActionUpgrade,
		},
		Status: simplyblockv1alpha2.ControlPlaneOpsStatus{
			Phase: simplyblockv1alpha2.ControlPlaneOpsPhaseRunning,
			Step:  statemachine.KubeSnapshot{State: string(step)},
		},
	}
}

// A record may not be withdrawn from a step that has started a rollout. The
// controller's deletion path would release the lock and let the next operation
// begin against a half-applied control plane.
func TestControlPlaneOpsDeleteIsRefusedMidRollout(t *testing.T) {
	v := cpOpsValidator(t)

	for _, step := range []simplyblockv1alpha2.ControlPlaneOpsStep{
		simplyblockv1alpha2.ControlPlaneOpsStepRestarting,
		simplyblockv1alpha2.ControlPlaneOpsStepApplying,
		simplyblockv1alpha2.ControlPlaneOpsStepAwaiting,
		simplyblockv1alpha2.ControlPlaneOpsStepVerifying,
	} {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), deleteRequestFor(t, runningOpsAt(step)))
			if resp.Allowed {
				t.Fatalf("the record was withdrawn at step %s, while the rollout was moving", step)
			}
			if !strings.Contains(resp.Result.Message, string(step)) {
				t.Errorf("the refusal is %q, want it to name the step", resp.Result.Message)
			}
		})
	}
}

// A step that has changed nothing is deletable, because withdrawing the record
// there stops nothing that anything else has to finish.
func TestControlPlaneOpsDeleteIsAllowedBeforeAnythingChanged(t *testing.T) {
	v := cpOpsValidator(t)

	for _, step := range []simplyblockv1alpha2.ControlPlaneOpsStep{
		simplyblockv1alpha2.ControlPlaneOpsStepDraining,
		simplyblockv1alpha2.ControlPlaneOpsStepPreflight,
		simplyblockv1alpha2.ControlPlaneOpsStepRequesting,
	} {
		t.Run(string(step), func(t *testing.T) {
			resp := v.Handle(context.Background(), deleteRequestFor(t, runningOpsAt(step)))
			if !resp.Allowed {
				t.Errorf("the record was held at step %s, where nothing has been changed: %s",
					step, resp.Result.Message)
			}
		})
	}
}

// A terminal operation is a record of work that has finished, so withdrawing it
// stops nothing whatever step it ended on.
func TestControlPlaneOpsDeleteIsAllowedOnceTerminal(t *testing.T) {
	v := cpOpsValidator(t)

	for _, phase := range []simplyblockv1alpha2.ControlPlaneOpsPhase{
		simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded,
		simplyblockv1alpha2.ControlPlaneOpsPhaseFailed,
		simplyblockv1alpha2.ControlPlaneOpsPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ops := runningOpsAt(simplyblockv1alpha2.ControlPlaneOpsStepApplying)
			ops.Status.Phase = phase

			resp := v.Handle(context.Background(), deleteRequestFor(t, ops))
			if !resp.Allowed {
				t.Errorf("a %s operation could not be deleted: %s", phase, resp.Result.Message)
			}
		})
	}
}

// Every step the graphs declare is either undeletable for a stated reason or
// deletable. A step in neither set is one this guard has no opinion about, which
// is how a new step silently becomes withdrawable mid-rollout.
func TestEveryUndeletableStepStatesWhatItIsDoing(t *testing.T) {
	for step, doing := range undeletableControlPlaneSteps {
		if doing == "" {
			t.Errorf("%s is undeletable and the refusal says nothing about why", step)
		}
	}
}

// The refusal table and the graph are two statements of one rule, and this holds
// them equal in both directions. A deletion may not express something spec.abort
// could not, so an extra entry refuses a delete the abort channel would have
// honored, and a missing one admits the withdrawal of a record nothing else
// accounts for.
func TestTheControlPlaneRefusalTableAndTheGraphAgree(t *testing.T) {
	refused := make([]simplyblockv1alpha2.ControlPlaneOpsStep, 0, len(undeletableControlPlaneSteps))
	for step := range undeletableControlPlaneSteps {
		refused = append(refused, step)
	}
	slices.Sort(refused)

	if want := controlplane.UnabortableSteps(); !slices.Equal(refused, want) {
		t.Errorf("the guard refuses %v, and the graph declares no abort edge from %v", refused, want)
	}
}
