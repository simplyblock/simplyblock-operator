// A Ready ObjectStore keeps reading its Deployment (design-objectstore.md §6):
// pods no longer Available send it back to Provisioning, a rollout stuck past
// its progress deadline degrades it, and an ordinary rollout leaves it Ready.
// Deployment.status is fixture data, as in the phase-transition tests.

package objectstore

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// rolloutDeployment is the gateway Deployment with the given Available and
// Progressing conditions; progressReason is the Progressing condition's reason.
func rolloutDeployment(
	store *simplyblockv1alpha2.ObjectStore, available, progressing bool, progressReason string,
) *appsv1.Deployment {
	status := func(b bool) corev1.ConditionStatus {
		if b {
			return corev1.ConditionTrue
		}
		return corev1.ConditionFalse
	}
	d := availableDeployment(store, available, 1)
	d.Status.Conditions = append(d.Status.Conditions, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: status(progressing), Reason: progressReason,
	})
	return d
}

// readyOpenShiftStore is a Ready store whose certificate is in place, so only
// the Deployment decides where a pass takes it.
func readyOpenShiftStore() (*simplyblockv1alpha2.ObjectStore, *corev1.Secret) {
	store := readyStore(utils.TLSProviderOpenShift)
	return store, servingSecret(store, utils.TLSProviderOpenShift)
}

// U-29: a Ready store whose Deployment is no longer Available returns to
// Provisioning, whose deadline degrades it if the pods never come back.
func TestAReadyStoreWhosePodsAreGoneProvisionsAgain(t *testing.T) {
	store, secret := readyOpenShiftStore()
	cli := newTestClient(t, store, secret, rolloutDeployment(store, false, true, "ReplicaSetUpdated"))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
	if got.Status.PhaseDeadline == nil {
		t.Error("Provisioning was entered without a deadline, so it never degrades")
	}
}

// U-30: a Ready store whose rollout is stuck past its progress deadline, old
// pods still serving, degrades and names the stuck rollout.
func TestAReadyStoreWithAStuckRolloutDegrades(t *testing.T) {
	store, secret := readyOpenShiftStore()
	cli := newTestClient(t, store, secret, rolloutDeployment(store, true, false, rolloutDeadlineExceeded))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	cond := findCondition(got.Status.Conditions, conditionDeploymentProgressing)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != rolloutDeadlineExceeded {
		t.Errorf("%s condition = %+v, want False with reason %s", conditionDeploymentProgressing, cond, rolloutDeadlineExceeded)
	}
}

// U-31: a rollout in progress, pods Available, leaves a Ready store Ready.
func TestAReadyStoreStaysReadyThroughARollout(t *testing.T) {
	store, secret := readyOpenShiftStore()
	cli := newTestClient(t, store, secret, rolloutDeployment(store, true, true, "ReplicaSetUpdated"))

	if got := reconcileStore(t, cli, store); got.Status.Phase != osPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

// U-32: a ProgressDeadlineExceeded the Deployment controller reported before
// it observed the current spec is stale, and leaves a Ready store Ready.
func TestAStaleStuckRolloutLeavesAReadyStoreReady(t *testing.T) {
	store, secret := readyOpenShiftStore()
	deployment := rolloutDeployment(store, true, false, rolloutDeadlineExceeded)
	deployment.Generation = 3
	deployment.Status.ObservedGeneration = 2
	cli := newTestClient(t, store, secret, deployment)

	if got := reconcileStore(t, cli, store); got.Status.Phase != osPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}
