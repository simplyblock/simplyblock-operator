// Which control-plane phases let a deployment proceed.
//
// The phase separates a control plane that answers from one that does not, and
// only the second is a reason to wait. design-controlplane.md §4.3 states it
// directly: a Degraded control plane is one that answers, so nothing holds on
// it. It is the phase a management API pod restarting behind a Service produces,
// and a FoundationDB pod recycled without losing quorum, and a metrics exporter
// that is not running at all.

package deployment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aControlPlaneIn builds the reconciler over a singleton in the phase given.
func aControlPlaneIn(
	t *testing.T, phase simplyblockv1alpha2.ControlPlanePhase,
) *ClusterDeploymentConfigReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)

	controlPlane := &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: singletonControlPlane, Namespace: "simplyblock"},
	}
	controlPlane.Status.Phase = phase

	return &ClusterDeploymentConfigReconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(controlPlane).Build(),
		Scheme:    scheme,
		Namespace: "simplyblock",
	}
}

// TestADegradedControlPlaneDoesNotHoldTheDeployment is the gate this corrects.
//
// Regression: 2026-09-21-degraded-held-the-expansion — the gate compared the
// phase to Available and nothing else, so every phase that was not that word
// waited, Degraded included. On the deployment this was found on the whole
// expansion stopped at "ControlPlane simplyblock is Degraded rather than
// Available" because the FoundationDB metrics exporter was not running: a
// component that is non-essential by its own table, whose absence loses no work
// and stops nothing, held six workers out of a cluster.
func TestADegradedControlPlaneDoesNotHoldTheDeployment(t *testing.T) {
	r := aControlPlaneIn(t, simplyblockv1alpha2.ControlPlanePhaseDegraded)

	ready, message := r.controlPlaneReady(context.Background())
	if !ready {
		t.Errorf("a control plane that answers held the deployment: %s", message)
	}
}

// Available proceeds, which is the case that always worked.
func TestAnAvailableControlPlaneProceeds(t *testing.T) {
	r := aControlPlaneIn(t, simplyblockv1alpha2.ControlPlanePhaseAvailable)

	if ready, message := r.controlPlaneReady(context.Background()); !ready {
		t.Errorf("an available control plane held the deployment: %s", message)
	}
}

// Unavailable is the phase that means the control plane does not answer, and it
// is the one worth waiting for: every call the expansion is about to make would
// fail.
func TestAnUnavailableControlPlaneHolds(t *testing.T) {
	r := aControlPlaneIn(t, simplyblockv1alpha2.ControlPlanePhaseUnavailable)

	ready, message := r.controlPlaneReady(context.Background())
	if ready {
		t.Error("a control plane that does not answer let the deployment proceed")
	}
	if message == "" {
		t.Error("the hold says nothing about why")
	}
}

// The phases before the control plane exists hold too. There is nothing to call
// yet, rather than something impaired.
func TestAControlPlaneStillBeingBuiltHolds(t *testing.T) {
	for _, phase := range []simplyblockv1alpha2.ControlPlanePhase{
		simplyblockv1alpha2.ControlPlanePhaseInstalling,
		"",
	} {
		r := aControlPlaneIn(t, phase)
		if ready, _ := r.controlPlaneReady(context.Background()); ready {
			t.Errorf("a control plane in %q let the deployment proceed", phase)
		}
	}
}
