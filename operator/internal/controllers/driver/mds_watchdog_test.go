// Tests for the MDS readiness watchdog, driven pass by pass against a fake
// client and a fake event recorder, like the management API's watchdog.

package driver

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	wdNamespace = "simplyblock"
	wdMDSPod    = "simplyblock-pnfs-mds-2600692a-0"
)

func wdPod(name string, labels map[string]string, ready corev1.ConditionStatus, since time.Time, everReady bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wdNamespace, Labels: labels, UID: types.UID(name)},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.Time{Time: since},
		}}},
	}
	if everReady {
		pod.Annotations = map[string]string{mdsWasReadyAnnotation: "true"}
	}
	return pod
}

func mdsPod(ready corev1.ConditionStatus, since time.Time, everReady bool) *corev1.Pod {
	return wdPod(wdMDSPod, map[string]string{MDSClusterLabel: "2600692a-7b7d-4402-b7dc-43b8c12e5d81"},
		ready, since, everReady)
}

func wdWatchdog(t *testing.T, now time.Time, objects ...client.Object) (*MDSReadinessWatchdog, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFakeRecorder(16)
	return &MDSReadinessWatchdog{
		Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Recorder:         rec,
		UnreadyThreshold: 3 * time.Minute,
		Now:              func() time.Time { return now },
	}, rec
}

func wdExists(t *testing.T, w *MDSReadinessWatchdog, name string) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := w.Get(context.Background(), types.NamespacedName{Name: name, Namespace: wdNamespace}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return &pod, true
}

func wdAnnounced(rec *events.FakeRecorder) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, MDSPodStuckReason) {
				return true
			}
		default:
			return false
		}
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// An MDS that lost its storage stayed unready for 19 h 39 m. Restarting its
// container does not help either: the host removed the state volume's device,
// and only a new pod stages the volume again (reproduced on cluster 111, where
// the restarted container crash-looped on a missing /dev/mds-state until the
// pod was deleted). Every pNFS client in the cluster had no data path.
func TestAnMDSPodUnreadyPastTheThresholdIsRecreated(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, mdsPod(corev1.ConditionFalse, now.Add(-4*time.Minute), true))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); exists {
		t.Error("the MDS pod stayed after four minutes unready: nothing recovers the pNFS data path")
	}
	if !wdAnnounced(rec) {
		t.Errorf("no %s event: recreating the metadata server went unannounced", MDSPodStuckReason)
	}
}

func TestAnMDSPodUnreadyBrieflyIsLeftAlone(t *testing.T) {
	now := time.Now()
	w, rec := wdWatchdog(t, now, mdsPod(corev1.ConditionFalse, now.Add(-time.Minute), true))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); !exists {
		t.Error("an MDS pod unready for a minute was recreated")
	}
	if wdAnnounced(rec) {
		t.Error("announced a recreation that did not happen")
	}
}

// A pod still booting, or waiting to be scheduled with its state volume, is
// the boot deadline's business, not this watchdog's.
func TestAnMDSPodThatWasNeverReadyIsLeftAlone(t *testing.T) {
	now := time.Now()
	w, _ := wdWatchdog(t, now, mdsPod(corev1.ConditionFalse, now.Add(-time.Hour), false))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); !exists {
		t.Error("an MDS pod that never turned ready was recreated")
	}
}

func TestAReadyMDSPodIsMarkedAndKept(t *testing.T) {
	now := time.Now()
	w, _ := wdWatchdog(t, now, mdsPod(corev1.ConditionTrue, now.Add(-time.Hour), false))

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	pod, exists := wdExists(t, w, wdMDSPod)
	if !exists {
		t.Fatal("a ready MDS pod was deleted")
	}
	if pod.Annotations[mdsWasReadyAnnotation] != "true" {
		t.Error("a ready MDS pod was not marked as having been ready")
	}
}

func TestOnlyMDSPodsAreWatched(t *testing.T) {
	now := time.Now()
	other := wdPod("fio-0", map[string]string{"app": "fio"}, corev1.ConditionFalse, now.Add(-time.Hour), true)
	w, _ := wdWatchdog(t, now, other)

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, "fio-0"); !exists {
		t.Error("a pod that is not a metadata server was deleted")
	}
}

// wdCluster is the StorageCluster behind mdsPod, with one storage node per
// control-plane status given.
func wdCluster(nodeStatuses ...string) []client.Object {
	objs := []client.Object{&simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: wdNamespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: "2600692a-7b7d-4402-b7dc-43b8c12e5d81"},
	}}
	for i, st := range nodeStatuses {
		objs = append(objs, &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-" + string(rune('a'+i)), Namespace: wdNamespace},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "cluster"},
			Status:     simplyblockv1alpha2.StorageNodeStatus{Status: st},
		})
	}
	return objs
}

// Regression: SFAM-2860. With the runner's own deadline held during a storage
// outage, this watchdog recreated the MDS 3m28s into the outage instead
// (cluster 66), and the replacement could not attach the volumes whose paths
// were still down. While a storage node of its cluster is not online, an
// unready MDS is waiting on the outage and is left alone.
func TestAnMDSPodUnreadyDuringAStorageOutageIsLeftAlone(t *testing.T) {
	now := time.Now()
	objs := append(wdCluster("online", "in_restart", "online"),
		mdsPod(corev1.ConditionFalse, now.Add(-4*time.Minute), true))
	w, rec := wdWatchdog(t, now, objs...)

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); !exists {
		t.Error("the MDS pod was recreated while a storage node was restarting: its replacement cannot attach that node's volumes")
	}
	if wdAnnounced(rec) {
		t.Error("a recreation was announced during a storage outage")
	}
}

// A node that never returns does not keep a dead MDS forever: past the
// outage ceiling the pod is recreated regardless.
func TestAnMDSPodUnreadyPastTheOutageCeilingIsRecreated(t *testing.T) {
	now := time.Now()
	objs := append(wdCluster("online", "offline"),
		mdsPod(corev1.ConditionFalse, now.Add(-25*time.Minute), true))
	w, _ := wdWatchdog(t, now, objs...)

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); exists {
		t.Error("the MDS pod stayed 25 minutes unready because one node is offline")
	}
}

// Every node online: nothing to wait for, so the plain threshold applies.
func TestAnMDSPodUnreadyWithEveryNodeOnlineIsRecreated(t *testing.T) {
	now := time.Now()
	objs := append(wdCluster("online", "online"),
		mdsPod(corev1.ConditionFalse, now.Add(-4*time.Minute), true))
	w, _ := wdWatchdog(t, now, objs...)

	if err := w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, exists := wdExists(t, w, wdMDSPod); exists {
		t.Error("the MDS pod stayed four minutes unready with every storage node online")
	}
}
