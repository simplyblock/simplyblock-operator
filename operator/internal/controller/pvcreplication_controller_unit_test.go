package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// newPVCWatcherReconciler creates a PVCAnnotationWatcher backed by a fake client.
func newPVCWatcherReconciler(t *testing.T, objects ...client.Object) (*PVCAnnotationWatcher, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme, storagev1.AddToScheme)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.ReplicationSlot{},
			&simplyblockv1alpha1.ReplicationPolicy{},
			&corev1.PersistentVolumeClaim{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.ReplicationSlot{}, "spec.pvcRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationSlot).Spec.PVCRef}
		}).
		WithIndex(&corev1.PersistentVolumeClaim{}, "spec.storageClassName", func(obj client.Object) []string {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			if pvc.Spec.StorageClassName == nil {
				return nil
			}
			return []string{*pvc.Spec.StorageClassName}
		}).
		Build()
	return &PVCAnnotationWatcher{Client: cl, Scheme: scheme}, cl
}

const testSCName = "fast"

func pvcRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func boundPVC(annotations map[string]string) *corev1.PersistentVolumeClaim {
	scPtr := testSCName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pvc1",
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scPtr,
			VolumeName:       "pv1",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
	return pvc
}

func testCSIPV(volumeHandle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: volumeHandle,
				},
			},
		},
	}
}

func readyPolicy(name string) *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status:     simplyblockv1alpha1.ReplicationPolicyStatus{Ready: true, BackendPolicyID: "bpol"},
	}
}

// ---------- ignore not-found ----------

func TestPVCWatcher_IgnoreNotFound(t *testing.T) {
	r, _ := newPVCWatcherReconciler(t)
	res, err := r.Reconcile(context.Background(), pvcRequest("missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
}

// ---------- no policy, no slot — nothing to do ----------

func TestPVCWatcher_NoPolicyNoSlot_NoOp(t *testing.T) {
	scName := testSCName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc1", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: testSCName},
	}
	r, _ := newPVCWatcherReconciler(t, pvc, sc)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
}

// ---------- PVC unbound — requeue ----------

func TestPVCWatcher_PVCAnnotation_UnboundRequeue(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc1", Namespace: "default",
			Annotations: map[string]string{utils.AnnotationReplicationPolicy: "pol"},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	pol := readyPolicy("pol")
	r, _ := newPVCWatcherReconciler(t, pvc, pol)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != pvcReplRequeueUnbound {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pvcReplRequeueUnbound)
	}
}

// ---------- creates slot when PVC is bound ----------

func TestPVCWatcher_CreatesSlotWhenBound(t *testing.T) {
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	pv := testCSIPV("cluster-uuid:pool-uuid:vol-uuid")
	pol := readyPolicy("pol")

	r, cl := newPVCWatcherReconciler(t, pvc, pv, pol)

	_, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := cl.List(context.Background(), &slotList, client.InNamespace("default")); err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slotList.Items) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slotList.Items))
	}
	slot := slotList.Items[0]
	if slot.Spec.PolicyRef != "pol" {
		t.Errorf("PolicyRef = %q, want pol", slot.Spec.PolicyRef)
	}
	if slot.Spec.PVCRef != "pvc1" {
		t.Errorf("PVCRef = %q, want pvc1", slot.Spec.PVCRef)
	}
	if slot.Spec.VolumeID != "cluster-uuid:pool-uuid:vol-uuid" {
		t.Errorf("VolumeID = %q", slot.Spec.VolumeID)
	}
}

// ---------- StorageClass annotation propagates ----------

func TestPVCWatcher_SCAnnotation_CreatesSlot(t *testing.T) {
	scName := testSCName
	pvc := boundPVC(nil)
	pv := testCSIPV("c:p:v")
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        scName,
			Annotations: map[string]string{utils.AnnotationReplicationPolicy: "pol"},
		},
	}
	pol := readyPolicy("pol")

	r, cl := newPVCWatcherReconciler(t, pvc, pv, sc, pol)

	_, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := cl.List(context.Background(), &slotList, client.InNamespace("default")); err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slotList.Items) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slotList.Items))
	}
}

// ---------- policy changed — delete old slot, wait ----------

func TestPVCWatcher_PolicyChanged_DeletesOldSlot(t *testing.T) {
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "pol-new"})
	pv := testCSIPV("c:p:v")
	existingSlot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-old-pvc1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol-old", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	pol := readyPolicy("pol-new")

	r, cl := newPVCWatcherReconciler(t, pvc, pv, existingSlot, pol)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue to wait for old slot to be GC'd.
	if res.RequeueAfter != pvcReplRequeueDetaching {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pvcReplRequeueDetaching)
	}

	// Old slot should be deleted.
	var slot simplyblockv1alpha1.ReplicationSlot
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-old-pvc1"}, &slot)
	if err == nil {
		if slot.DeletionTimestamp == nil {
			t.Errorf("old slot still exists and not marked for deletion")
		}
	}
}

// ---------- policy removed — delete slot, no new slot ----------

func TestPVCWatcher_PolicyRemoved_DeletesSlot(t *testing.T) {
	// No annotation on PVC.
	pvc := boundPVC(nil)
	existingSlot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-pvc1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, cl := newPVCWatcherReconciler(t, pvc, existingSlot)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No new slot → no requeue.
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}

	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := cl.List(context.Background(), &slotList, client.InNamespace("default")); err != nil {
		t.Fatalf("list slots: %v", err)
	}
	for _, s := range slotList.Items {
		if s.DeletionTimestamp == nil && s.Name == "pol-pvc1" {
			t.Errorf("slot %q was not deleted", s.Name)
		}
	}
}

// ---------- slot pending deletion — wait ----------

func TestPVCWatcher_SlotPendingDeletion_Waits(t *testing.T) {
	now := metav1.Now()
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	existingSlot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol-pvc1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationSlot},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, _ := newPVCWatcherReconciler(t, pvc, existingSlot)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != pvcReplRequeueDetaching {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pvcReplRequeueDetaching)
	}
}

// ---------- policy not found — requeue ----------

func TestPVCWatcher_PolicyNotFound_Requeues(t *testing.T) {
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	pv := testCSIPV("c:p:v")
	r, _ := newPVCWatcherReconciler(t, pvc, pv)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when policy not found")
	}
}

// ---------- policy not ready — requeue ----------

func TestPVCWatcher_PolicyNotReady_Requeues(t *testing.T) {
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	pv := testCSIPV("c:p:v")
	pol := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status:     simplyblockv1alpha1.ReplicationPolicyStatus{Ready: false},
	}
	r, _ := newPVCWatcherReconciler(t, pvc, pv, pol)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when policy not ready")
	}
}

// ---------- replicationSlotName ----------

func TestReplicationSlotName(t *testing.T) {
	cases := []struct {
		policy, pvc string
		want        string
	}{
		{"my-policy", "my-pvc", "my-policy-my-pvc"},
		{"pol", "claim-1", "pol-claim-1"},
	}
	for _, tc := range cases {
		got := replicationSlotName(tc.policy, tc.pvc)
		if got != tc.want {
			t.Errorf("replicationSlotName(%q, %q) = %q, want %q", tc.policy, tc.pvc, got, tc.want)
		}
	}
}
