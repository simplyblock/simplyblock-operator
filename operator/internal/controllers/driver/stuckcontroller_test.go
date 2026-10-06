// Tests for recycling a controller pod that holds a StatefulSet rollout, driven
// through the fake client because the decision is a function of the StatefulSet's
// revisions and one pod's readiness.

package driver

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	revisionCurrent = "simplyblock-csi-controller-aaaa"
	revisionUpdate  = "simplyblock-csi-controller-bbbb"
	revisionBroken  = "simplyblock-csi-controller-cccc"
)

// stuckFixture builds the driver, a controller StatefulSet mid-rollout when
// rolling is true, and its one pod at hash, unready or ready since sinceReady.
func stuckFixture(
	t *testing.T, rolling bool, hash string, ready bool, since time.Duration,
) (*SimplyblockDriverReconciler, *simplyblockv1alpha2.SimplyblockDriver, *events.FakeRecorder) {
	t.Helper()
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	n := names(d)

	update := revisionCurrent
	if rolling {
		update = revisionUpdate
	}
	labels := map[string]string{"app": "simplyblock-csi-controller"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.controllerStatefulSet, Namespace: d.Namespace},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		Status: appsv1.StatefulSetStatus{
			CurrentRevision: revisionCurrent,
			UpdateRevision:  update,
		},
	}

	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	podLabels := map[string]string{"app": "simplyblock-csi-controller", "controller-revision-hash": hash}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.controllerStatefulSet + "-0", Namespace: d.Namespace, Labels: podLabels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-since)),
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: status,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-since)),
		}}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, sts, pod).WithStatusSubresource(d).Build()
	rec := events.NewFakeRecorder(10)
	return &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Recorder: rec}, d, rec
}

func podExists(t *testing.T, r *SimplyblockDriverReconciler, d *simplyblockv1alpha2.SimplyblockDriver) bool {
	t.Helper()
	var pod corev1.Pod
	err := r.Get(context.Background(),
		client.ObjectKey{Namespace: d.Namespace, Name: names(d).controllerStatefulSet + "-0"}, &pod)
	if errors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the controller pod: %v", err)
	}
	return true
}

// Regression: 2026-10-05-driver-adoption-rollout — a controller pod crash-looping
// on a revision the StatefulSet had already replaced held the rollout there for
// good, because the StatefulSet waits for the pod to be ready before it applies
// the revision that fixes it. The driver stayed Unavailable until somebody
// deleted the pod by hand.
func TestAControllerPodStuckOnAStaleRevisionIsRecycled(t *testing.T) {
	r, d, rec := stuckFixture(t, true, revisionBroken, false, 5*time.Minute)

	if _, err := r.recycleStuckController(context.Background(), d); err != nil {
		t.Fatalf("recycleStuckController: %v", err)
	}

	if podExists(t, r, d) {
		t.Error("the stuck controller pod survived, and the rollout cannot move past it")
	}
	select {
	case got := <-rec.Events:
		if want := reasonControllerPodRecycled; !contains(got, want) {
			t.Errorf("event %q does not carry the reason %q", got, want)
		}
	default:
		t.Error("deleting the pod emitted no event")
	}
}

// Inside the grace period the pod may only be slow to start, and the rollout is
// asked to wait for the remainder.
func TestAControllerPodInsideTheGracePeriodIsLeftAlone(t *testing.T) {
	r, d, _ := stuckFixture(t, true, revisionBroken, false, 30*time.Second)

	wait, err := r.recycleStuckController(context.Background(), d)
	if err != nil {
		t.Fatalf("recycleStuckController: %v", err)
	}

	if !podExists(t, r, d) {
		t.Error("a pod unready for 30 seconds was deleted inside the grace period")
	}
	if wait <= 0 || wait > stuckControllerGrace {
		t.Errorf("requeue = %s, want the rest of the grace period", wait)
	}
}

// A pod already on the revision the StatefulSet wants is not stuck on a stale one,
// and a replacement would come back identical.
func TestAControllerPodOnTheUpdateRevisionIsLeftAlone(t *testing.T) {
	r, d, _ := stuckFixture(t, true, revisionUpdate, false, 10*time.Minute)

	if _, err := r.recycleStuckController(context.Background(), d); err != nil {
		t.Fatalf("recycleStuckController: %v", err)
	}
	if !podExists(t, r, d) {
		t.Error("a pod on the update revision was deleted, and a replacement would crash the same way")
	}
}

// A ready pod on an older revision is an ordinary rolling update in progress.
func TestAReadyControllerPodOnAStaleRevisionIsLeftAlone(t *testing.T) {
	r, d, _ := stuckFixture(t, true, revisionCurrent, true, 10*time.Minute)

	if _, err := r.recycleStuckController(context.Background(), d); err != nil {
		t.Fatalf("recycleStuckController: %v", err)
	}
	if !podExists(t, r, d) {
		t.Error("a ready pod was deleted during an ordinary rollout")
	}
}

// With no rollout in progress there is no newer revision to move the pod to.
func TestAnUnreadyControllerPodWithNoRolloutIsLeftAlone(t *testing.T) {
	r, d, _ := stuckFixture(t, false, revisionCurrent, false, 10*time.Minute)

	if _, err := r.recycleStuckController(context.Background(), d); err != nil {
		t.Fatalf("recycleStuckController: %v", err)
	}
	if !podExists(t, r, d) {
		t.Error("a pod was deleted with no rollout to move it onto")
	}
}
