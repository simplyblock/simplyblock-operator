// Unit tests for replication across namespaces: a workload's claim lives in
// its own namespace, while the StorageCluster and the replication objects live
// in the storage namespace. A claim follows a policy of another namespace by
// a "<namespace>/<name>" reference, and a cluster-scoped migration covers the
// claims of every namespace.

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const workloadNS = "apps"

func workloadPVC(name, pvName string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	pvc := migBoundPVC(name, pvName, annotations)
	pvc.Namespace = workloadNS
	return pvc
}

func TestMigration_ClusterScope_CoversClaimsInEveryNamespace(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-w", "replicating", 100))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-w", "converging", 10))
	ops := migrationOps(utils.ReplicationOpsScopeCluster, testClusterName)
	ops.Spec.PolicyRef = testMigPolicyName
	src := testClusterUUID + ":pool:"
	r, cl := newOpsReplReconciler(t, readyPairForOps(), migrationPolicy(), ops,
		workloadPVC("pvc-w", "pv-w", nil), csiPV("pv-w", src+"vol-w"), // to enroll
		workloadPVC("pvc-x", "pv-x", nil), csiPV("pv-x", "other-cluster:pool:vol-x"), // another cluster
		workloadPVC("pvc-y", "pv-y", map[string]string{utils.AnnotationReplicationPolicy: "dr"}), csiPV("pv-y", src+"vol-y"),
	)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	pvcW := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: workloadNS, Name: "pvc-w"}, pvcW)
	if got := pvcW.Annotations[utils.AnnotationReplicationPolicy]; got != "default/mig" {
		t.Fatalf("pvc-w policy annotation = %q, want default/mig (the policy of the storage namespace)", got)
	}
	pvcX := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: workloadNS, Name: "pvc-x"}, pvcX)
	if _, ok := pvcX.Annotations[utils.AnnotationReplicationPolicy]; ok {
		t.Errorf("annotated pvc-x, which lives on another cluster")
	}
	skipped := map[string]bool{}
	for _, res := range getOps(t, cl).Status.Results {
		if res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			skipped[res.PVCRef] = true
		}
	}
	if !skipped[workloadNS+"/pvc-y"] || len(skipped) != 1 {
		t.Errorf("skipped = %v, want apps/pvc-y only (it follows another policy)", skipped)
	}

	// The watcher creates the slot beside the claim, naming the policy across namespaces.
	slotW := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-pvc-w", Namespace: workloadNS},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "default/mig", PVCRef: "pvc-w", VolumeID: src + "vol-w"},
	}
	if err := cl.Create(context.Background(), slotW); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	patch := client.MergeFrom(slotW.DeepCopy())
	slotW.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
	if err := cl.Status().Patch(context.Background(), slotW, patch); err != nil {
		t.Fatalf("patch slot: %v", err)
	}

	reconcileOps(t, r) // creates the plan
	reconcileOps(t, r) // tracks it

	if len(ps.posts) != 1 {
		t.Fatalf("posts = %d, want 1 plan", len(ps.posts))
	}
	if ids, _ := ps.posts[0]["lvol_ids"].([]interface{}); len(ids) != 1 || ids[0] != "vol-w" {
		t.Errorf("plan lvol_ids = %v, want [vol-w]", ps.posts[0]["lvol_ids"])
	}
	var planned *simplyblockv1alpha1.ReplicationOpsResult
	got := getOps(t, cl)
	for i := range got.Status.Results {
		if got.Status.Results[i].PVCRef == workloadNS+"/pvc-w" {
			planned = &got.Status.Results[i]
		}
	}
	if planned == nil || planned.SlotRef != workloadNS+"/mig-pvc-w" || planned.State != "converging" {
		t.Errorf("results = %+v, want apps/pvc-w tracked through apps/mig-pvc-w", got.Status.Results)
	}
}

func TestPVCWatcher_FollowsAPolicyOfAnotherNamespace(t *testing.T) {
	pvc := boundPVC(map[string]string{utils.AnnotationReplicationPolicy: "default/pol"})
	pvc.Namespace = workloadNS
	r, cl := newPVCWatcherReconciler(t, pvc, testCSIPV("cluster-uuid:pool-uuid:vol-uuid"), readyPolicy("pol"))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workloadNS, Name: "pvc1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var slots simplyblockv1alpha1.ReplicationSlotList
	if err := cl.List(context.Background(), &slots, client.InNamespace(workloadNS)); err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots.Items) != 1 {
		t.Fatalf("slots in %s = %d, want 1 beside the claim", workloadNS, len(slots.Items))
	}
	if s := slots.Items[0]; s.Name != testPolicySlotPVC1 || s.Spec.PolicyRef != "default/pol" {
		t.Errorf("slot = %s policyRef %q, want pol-pvc1 following default/pol", s.Name, s.Spec.PolicyRef)
	}
}

func TestSlot_ResolvesAPolicyOfAnotherNamespace(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.Ready = false // stop right after the lookup
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: testPolicySlotPVC1, Namespace: workloadNS,
			Finalizers: []string{utils.FinalizerReplicationSlot}},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "default/pol", PVCRef: "pvc1", VolumeID: "c:p:v"},
	}
	r, cl := newSlotReconciler(t, pol, slot)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workloadNS, Name: testPolicySlotPVC1}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &simplyblockv1alpha1.ReplicationSlot{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: workloadNS, Name: testPolicySlotPVC1}, got)
	if got.Status.State == string(simplyblockv1alpha1.ReplicationSlotStateError) {
		t.Fatalf("slot in error (%s); the policy in namespace default was not found", got.Status.Message)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Errorf("result = %+v, want the not-ready wait of the found policy", res)
	}
}

func TestPolicy_CountsSlotsOfEveryNamespace(t *testing.T) {
	pol := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default", Finalizers: []string{utils.FinalizerReplicationPolicy}},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status:     simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol"},
	}
	local := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "pol", PVCRef: "a", VolumeID: "c:p:a"},
	}
	foreign := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-b", Namespace: workloadNS},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "default/pol", PVCRef: "b", VolumeID: "c:p:b"},
	}
	r, cl := newPolicyReconciler(t, readyPairForPolicy(), pol, local, foreign)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	if _, err := r.Reconcile(context.Background(), policyRequest("pol")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getPolicy(t, cl).Status.SlotCount; got != 2 {
		t.Errorf("slotCount = %d, want 2 (one in default, one in %s)", got, workloadNS)
	}
}
