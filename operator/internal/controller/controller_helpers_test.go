package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testWorkerNodeName is the node the test workload pods run on.
const testWorkerNodeName = "worker-1"

// A migrated volume: the PV's handle still names the volume the PVC was
// created with, the slot names the volume where it lives now.
func migratedConsumer(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "cluster-a:pool:original-id"},
			},
			ClaimRef: &corev1.ObjectReference{Name: "pvc1", Namespace: "default"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: testWorkerNodeName,
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"},
			}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t, corev1.AddToScheme)).
		WithObjects(pv, pod)
}

func TestFindSlotConsumerNode_FindsTheConsumerOfAMigratedVolume(t *testing.T) {
	cl := migratedConsumer(t).Build()
	ctx := context.Background()

	// By volume ID the moved volume has no PV: the preconnect used to be skipped.
	if node, err := findConsumerNode(ctx, cl, "moved-id"); err != nil || node != "" {
		t.Fatalf("findConsumerNode(moved-id) = %q, %v; want no match", node, err)
	}

	node, err := findSlotConsumerNode(ctx, cl, "default", "pvc1", "moved-id")
	if err != nil || node != testWorkerNodeName {
		t.Fatalf("findSlotConsumerNode = %q, %v; want worker-1", node, err)
	}
}

func TestFindSlotConsumerNode_FallsBackToTheVolumeHandle(t *testing.T) {
	cl := migratedConsumer(t).Build()

	node, err := findSlotConsumerNode(context.Background(), cl, "default", "", "original-id")
	if err != nil || node != testWorkerNodeName {
		t.Fatalf("findSlotConsumerNode without PVC = %q, %v; want worker-1", node, err)
	}
}

func TestFindSlotConsumerNode_NoRunningPodIsNoConsumer(t *testing.T) {
	cl := migratedConsumer(t).Build()

	node, err := findSlotConsumerNode(context.Background(), cl, "default", "other-pvc", "moved-id")
	if err != nil || node != "" {
		t.Fatalf("findSlotConsumerNode(other-pvc) = %q, %v; want no consumer", node, err)
	}
}
