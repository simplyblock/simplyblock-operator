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
			&simplyblockv1alpha1.ReplicationPair{},
			&simplyblockv1alpha1.ReplicationPolicy{},
			&corev1.PersistentVolumeClaim{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.ReplicationPair{}, "spec.pvcRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationPair).Spec.PVCRef}
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

func pvcRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func boundPVC(pvName string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	scPtr := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pvc1",
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scPtr,
			VolumeName:       pvName,
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
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-a"},
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

// ---------- no policy, no pair — nothing to do ----------

func TestPVCWatcher_NoPolicyNoPair_NoOp(t *testing.T) {
	scName := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc1", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "fast"},
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

// ---------- creates pair when PVC is bound ----------

func TestPVCWatcher_CreatesPairWhenBound(t *testing.T) {
	pvc := boundPVC("pv1",
		map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	pv := testCSIPV("cluster-uuid:pool-uuid:vol-uuid")
	pol := readyPolicy("pol")

	r, cl := newPVCWatcherReconciler(t, pvc, pv, pol)

	_, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pairList simplyblockv1alpha1.ReplicationPairList
	if err := cl.List(context.Background(), &pairList, client.InNamespace("default")); err != nil {
		t.Fatalf("list pairs: %v", err)
	}
	if len(pairList.Items) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairList.Items))
	}
	pair := pairList.Items[0]
	if pair.Spec.PolicyRef != "pol" {
		t.Errorf("PolicyRef = %q, want pol", pair.Spec.PolicyRef)
	}
	if pair.Spec.PVCRef != "pvc1" {
		t.Errorf("PVCRef = %q, want pvc1", pair.Spec.PVCRef)
	}
	if pair.Spec.VolumeID != "cluster-uuid:pool-uuid:vol-uuid" {
		t.Errorf("VolumeID = %q", pair.Spec.VolumeID)
	}
}

// ---------- StorageClass annotation propagates ----------

func TestPVCWatcher_SCAnnotation_CreatesPair(t *testing.T) {
	scName := "fast"
	pvc := boundPVC("pv1", nil)
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

	var pairList simplyblockv1alpha1.ReplicationPairList
	if err := cl.List(context.Background(), &pairList, client.InNamespace("default")); err != nil {
		t.Fatalf("list pairs: %v", err)
	}
	if len(pairList.Items) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairList.Items))
	}
}

// ---------- policy changed — delete old pair, wait ----------

func TestPVCWatcher_PolicyChanged_DeletesOldPair(t *testing.T) {
	pvc := boundPVC("pv1",
		map[string]string{utils.AnnotationReplicationPolicy: "pol-new"})
	pv := testCSIPV("c:p:v")
	existingPair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-old-pvc1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol-old", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	pol := readyPolicy("pol-new")

	r, cl := newPVCWatcherReconciler(t, pvc, pv, existingPair, pol)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue to wait for old pair to be GC'd.
	if res.RequeueAfter != pvcReplRequeueDetaching {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, pvcReplRequeueDetaching)
	}

	// Old pair should be deleted.
	var pair simplyblockv1alpha1.ReplicationPair
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-old-pvc1"}, &pair)
	if err == nil {
		// Fake client marks as deleted via DeletionTimestamp.
		if pair.DeletionTimestamp == nil {
			t.Errorf("old pair still exists and not marked for deletion")
		}
	}
}

// ---------- policy removed — delete pair, no new pair ----------

func TestPVCWatcher_PolicyRemoved_DeletesPair(t *testing.T) {
	// No annotation on PVC.
	pvc := boundPVC("pv1", nil)
	existingPair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-pvc1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, cl := newPVCWatcherReconciler(t, pvc, existingPair)

	res, err := r.Reconcile(context.Background(), pvcRequest("pvc1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No new pair → no requeue.
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}

	var pairList simplyblockv1alpha1.ReplicationPairList
	if err := cl.List(context.Background(), &pairList, client.InNamespace("default")); err != nil {
		t.Fatalf("list pairs: %v", err)
	}
	// After deletion the pair should be gone (fake client removes it unless it has a finalizer).
	for _, p := range pairList.Items {
		if p.DeletionTimestamp == nil && p.Name == "pol-pvc1" {
			t.Errorf("pair %q was not deleted", p.Name)
		}
	}
}

// ---------- pair pending deletion — wait ----------

func TestPVCWatcher_PairPendingDeletion_Waits(t *testing.T) {
	now := metav1.Now()
	pvc := boundPVC("pv1",
		map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	existingPair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol-pvc1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, _ := newPVCWatcherReconciler(t, pvc, existingPair)

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
	pvc := boundPVC("pv1",
		map[string]string{utils.AnnotationReplicationPolicy: "pol"})
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
	pvc := boundPVC("pv1",
		map[string]string{utils.AnnotationReplicationPolicy: "pol"})
	pv := testCSIPV("c:p:v")
	pol := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-a"},
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

// ---------- replicationPairName ----------

func TestReplicationPairName(t *testing.T) {
	cases := []struct {
		policy, pvc string
		want        string
	}{
		{"my-policy", "my-pvc", "my-policy-my-pvc"},
		{"pol", "claim-1", "pol-claim-1"},
	}
	for _, tc := range cases {
		got := replicationPairName(tc.policy, tc.pvc)
		if got != tc.want {
			t.Errorf("replicationPairName(%q, %q) = %q, want %q", tc.policy, tc.pvc, got, tc.want)
		}
	}
}
