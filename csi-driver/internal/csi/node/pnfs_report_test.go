// Reporting a pNFS staging mount whose block layout this node cannot use.

package node

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const reportVolumeID = "c:p:6e17"

// recordedEvent is one event a capturingRecorder saw.
type recordedEvent struct {
	Object  runtime.Object
	Type    string
	Reason  string
	Message string
}

// capturingRecorder keeps every event with the object it was recorded on.
type capturingRecorder struct{ events []recordedEvent }

func (r *capturingRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	r.events = append(r.events, recordedEvent{object, eventtype, reason, message})
}

func (r *capturingRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any) {
	r.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *capturingRecorder) AnnotatedEventf(
	object runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...any,
) {
	r.Eventf(object, eventtype, reason, messageFmt, args...)
}

func csiPV(name, handle string, claim *corev1.ObjectReference) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: claim,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: primeDriver, VolumeHandle: handle},
			},
		},
	}
}

func unusable() unusableLayout {
	return unusableLayout{Path: stagingMount, FailingFor: 6 * time.Minute, Err: errProbeForTest}
}

// Regression: SFAM-2850. A mount sent all of its I/O through the metadata
// server for the rest of a run, and nothing a user looks at said so. The claim
// is where they look.
func TestAnUnusableLayoutIsReportedOnTheVolumesClaim(t *testing.T) {
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"}}
	kube := fake.NewClientset(
		csiPV("pvc-other", "c:p:0000", nil),
		csiPV("pvc-8e7a", reportVolumeID, &corev1.ObjectReference{Namespace: "app", Name: "data"}),
		claim,
	)
	rec := &capturingRecorder{}

	if err := reportUnusableLayout(context.Background(), kube, rec, "worker-3", unusable()); err != nil {
		t.Fatalf("reportUnusableLayout: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %+v, want one", rec.events)
	}
	ev := rec.events[0]
	pvc, ok := ev.Object.(*corev1.PersistentVolumeClaim)
	if !ok || pvc.Namespace != "app" || pvc.Name != "data" {
		t.Errorf("event recorded on %#v, want the claim app/data", ev.Object)
	}
	if ev.Type != corev1.EventTypeWarning || ev.Reason != reasonDataThroughMDS {
		t.Errorf("event = %s %s, want %s %s", ev.Type, ev.Reason, corev1.EventTypeWarning, reasonDataThroughMDS)
	}
	if !strings.Contains(ev.Message, "worker-3") {
		t.Errorf("message %q does not name the node", ev.Message)
	}
}

// A volume with no claim, a statically provisioned one released by its
// claim, still gets its report, on the PersistentVolume.
func TestAnUnusableLayoutOfAnUnclaimedVolumeIsReportedOnItsPV(t *testing.T) {
	kube := fake.NewClientset(csiPV("pvc-8e7a", reportVolumeID, nil))
	rec := &capturingRecorder{}

	if err := reportUnusableLayout(context.Background(), kube, rec, "worker-3", unusable()); err != nil {
		t.Fatalf("reportUnusableLayout: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %+v, want one", rec.events)
	}
	if pv, ok := rec.events[0].Object.(*corev1.PersistentVolume); !ok || pv.Name != "pvc-8e7a" {
		t.Errorf("event recorded on %#v, want the PersistentVolume pvc-8e7a", rec.events[0].Object)
	}
}

// A volume this driver has no PersistentVolume for is an error the caller logs,
// not a silent success.
func TestAnUnusableLayoutWithNoPVIsAnError(t *testing.T) {
	rec := &capturingRecorder{}
	if err := reportUnusableLayout(context.Background(), fake.NewClientset(), rec, "worker-3", unusable()); err == nil {
		t.Error("reportUnusableLayout found no PersistentVolume and reported success")
	}
	if len(rec.events) != 0 {
		t.Errorf("events = %+v, want none", rec.events)
	}
}
