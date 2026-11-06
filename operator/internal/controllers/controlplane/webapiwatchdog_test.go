// Tests for the webappapi readiness watchdog, driven pass by pass against a
// fake client and a fake event recorder, the same shape the device health
// monitor's tests use.

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// drainReasons and announced read a FakeRecorder the way the device health
// monitor's own tests do, so a test can assert both what was announced and
// what was not.
func drainReasons(rec *events.FakeRecorder) []string {
	var reasons []string
	for {
		select {
		case e := <-rec.Events:
			reasons = append(reasons, e)
		default:
			return reasons
		}
	}
}

func announced(rec *events.FakeRecorder) bool {
	for _, e := range drainReasons(rec) {
		if strings.Contains(e, WebAPIPodWedgedReason) {
			return true
		}
	}
	return false
}

const wdPodName = "simplyblock-webappapi-abc123"

// wdPod builds a management API pod with the Ready condition the kubelet
// would have written, optionally already carrying the watchdog's own
// "answered ready before" marker.
func wdPod(ready corev1.ConditionStatus, since time.Time, everReady bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: wdPodName, Namespace: testNamespace,
			Labels: map[string]string{appLabel: ComponentWebAPI},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.Time{Time: since},
			}},
		},
	}
	if everReady {
		pod.Annotations = map[string]string{webAPIWasReadyAnnotation: webAPIWasReadyValue}
	}
	return pod
}

func wdWatchdog(t *testing.T, now time.Time, objects ...client.Object) (*WebAPIReadinessWatchdog, *events.FakeRecorder) {
	t.Helper()
	rec := events.NewFakeRecorder(16)
	w := &WebAPIReadinessWatchdog{
		Client:           newClient(t, objects...),
		Recorder:         rec,
		UnreadyThreshold: 3 * time.Minute,
		Now:              func() time.Time { return now },
	}
	return w, rec
}

func wdGetPod(t *testing.T, w *WebAPIReadinessWatchdog) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := w.Get(context.Background(), types.NamespacedName{Name: wdPodName, Namespace: testNamespace}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return &pod, true
}

// A pod that answers ready is marked as having done so, which is the only
// record that lets a later pass tell a regression from a pod still starting.
func TestWatchdogRecordsThatAReadyPodHasAnsweredReady(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, wdPod(corev1.ConditionTrue, now, false))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	pod, ok := wdGetPod(t, w)
	if !ok {
		t.Fatal("the ready pod was deleted")
	}
	if pod.Annotations[webAPIWasReadyAnnotation] != webAPIWasReadyValue {
		t.Errorf("annotations = %v, want the ready marker set", pod.Annotations)
	}
	if announced(rec) {
		t.Error("a ready pod was treated as wedged")
	}
}

// A pod that has never answered ready is starting up, or waiting on a
// dependency, which is the readiness probe's own job to report. The watchdog
// leaves it alone no matter how long that has been true.
func TestWatchdogLeavesAPodThatHasNeverBeenReadyAlone(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, wdPod(corev1.ConditionFalse, now.Add(-10*time.Minute), false))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	if _, ok := wdGetPod(t, w); !ok {
		t.Error("a pod that never answered ready was restarted")
	}
	if announced(rec) {
		t.Error("a pod that never answered ready was treated as wedged")
	}
}

// A brief gap is what the readiness probe's own failure threshold already
// tolerates (design: six failures, five seconds apart). The watchdog must not
// restart a pod before its own threshold has actually elapsed.
func TestWatchdogWaitsOutABriefUnreadySpell(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, wdPod(corev1.ConditionFalse, now.Add(-1*time.Minute), true))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	if _, ok := wdGetPod(t, w); !ok {
		t.Error("the pod was restarted before the threshold elapsed")
	}
	if announced(rec) {
		t.Error("a brief unready spell was treated as wedged")
	}
}

// The regression this watchdog exists for: a pod that was ready, then was not,
// for longer than a database hiccup explains.
func TestWatchdogRestartsAPodWedgedPastTheThreshold(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, wdPod(corev1.ConditionFalse, now.Add(-5*time.Minute), true))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	if _, ok := wdGetPod(t, w); ok {
		t.Error("the wedged pod was not restarted")
	}
	if !announced(rec) {
		t.Error("no event was raised for the restart")
	}
}

// A pod already terminating is already going; deleting it again, or raising a
// second event, says nothing new.
func TestWatchdogLeavesAPodAlreadyTerminatingAlone(t *testing.T) {
	now := time.Now()
	pod := wdPod(corev1.ConditionFalse, now.Add(-5*time.Minute), true)
	pod.Finalizers = []string{"storage.simplyblock.io/test"}
	pod.DeletionTimestamp = &metav1.Time{Time: now}

	w, rec := wdWatchdog(t, now, pod)

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	if announced(rec) {
		t.Error("a pod already terminating was announced again")
	}
}
