package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	opsTestNS       = "test"
	opsTestCluster  = "cluster-a"
	opsTestWorker   = "worker-1.example.com"
	opsTestNodeUUID = "aaaa0000-0000-0000-0000-000000000001"
	opsTestOpsName  = "ops-1"
	opsTestOtherOps = "ops-other"
)

func newOpsReconciler(t *testing.T, objects ...client.Object) *StorageNodeOpsReconciler {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cl := newTestClient(t, scheme,
		[]client.Object{
			&simplyblockv1alpha1.StorageNode{},
			&simplyblockv1alpha1.StorageNodeOps{},
			&simplyblockv1alpha1.StorageNodeSet{},
			&simplyblockv1alpha1.StorageCluster{},
			&simplyblockv1alpha1.VolumeMigration{},
		},
		objects...,
	)
	return &StorageNodeOpsReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(16),
		apiReader: cl,
	}
}

//nolint:unparam
func newTestStorageNode(name, ns, snsRef, worker, uuid string) *simplyblockv1alpha1.StorageNode {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: snsRef,
			WorkerNode:        worker,
		},
	}
	sn.Status.UUID = uuid
	return sn
}

//nolint:unparam
func newTestStorageNodeOps(name, ns, snRef, action string) *simplyblockv1alpha1.StorageNodeOps {
	return &simplyblockv1alpha1.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeOpsSpec{
			StorageNodeRef: snRef,
			Action:         action,
		},
	}
}

// ── TestAcquireLock ───────────────────────────────────────────────────────────

func TestAcquireLock_SetsActiveOpsRefAndTransitionsToRunning(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "suspend")
	r := newOpsReconciler(t, sn, ops)

	_, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}

	// Check StorageNode.status.activeOpsRef was set.
	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != opsTestOpsName {
		t.Errorf("activeOpsRef: got %q want ops-1", updatedSN.Status.ActiveOpsRef)
	}

	// Check ops phase was set to Running.
	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseRunning {
		t.Errorf("phase: got %q want Running", updatedOps.Status.Phase)
	}
}

func TestAcquireLock_RequeuesWhenAnotherOpsActive(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = opsTestOtherOps
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "suspend")
	r := newOpsReconciler(t, sn, ops)

	result, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when another ops is active")
	}

	// StorageNode.activeOpsRef must NOT be changed.
	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != opsTestOtherOps {
		t.Errorf("activeOpsRef should not change: got %q", updatedSN.Status.ActiveOpsRef)
	}
}

// ── TestFdRemovalBalanceCheck ────────────────────────────────────────────────
//
// Mirrors the live 2026-08-13 incident's exact topology: 7 nodes split
// FD1=2/FD2=2/FD3=3. Removing a node from the already-smallest domain (FD1)
// drops it to 1 while FD3 stays at 3 -- a spread the backend's own
// check_fd_admission_for_remove correctly refuses (populations {1,2,3}).
// Removing instead from the domain with slack (FD3) leaves 2/2/2, which is
// fine. This is the gate that must fire in drainValidate BEFORE Suspending,
// so an infeasible removal never suspends the node in the first place.

//nolint:unparam
func newTestStorageNodeSet(name, ns, clusterName string, nodes ...simplyblockv1alpha1.NodeStatus) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.StorageNodeSetStatus{Nodes: nodes},
	}
}

//nolint:unparam
func newTestStorageClusterWithFD(name, ns string, enableFD bool) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{EnableFailureDomains: &enableFD},
	}
}

// sevenNodeTopology returns the live 2026-08-13 incident's exact topology: 7
// nodes split FD1=2/FD2=2/FD3=3, sn-1 in FD1 at mgmtIP. The entry at mgmtIP
// gets UUID=opsTestNodeUUID -- matching sn.Status.UUID in every caller --
// so hostHasSurvivingSibling correctly recognizes it as sn-1 itself rather
// than an unrelated sibling on the same host (the other six entries are
// distinct hosts at distinct IPs, so their empty UUID never collides with
// a real lookup at a different IP).
func sevenNodeTopology(mgmtIP string, snFD int32) []simplyblockv1alpha1.NodeStatus {
	fd := func(v int32) *int32 { return &v }
	all := []struct {
		ip     string
		domain int32
	}{
		{"10.0.0.1", 1}, {"10.0.0.2", 1},
		{"10.0.0.3", 2}, {"10.0.0.4", 2},
		{"10.0.0.5", 3}, {"10.0.0.6", 3}, {"10.0.0.7", 3},
	}
	nodes := make([]simplyblockv1alpha1.NodeStatus, 0, len(all))
	for _, n := range all {
		domain := n.domain
		uuid := ""
		if n.ip == mgmtIP {
			domain = snFD // let the caller override sn-1's own domain
			uuid = opsTestNodeUUID
		}
		nodes = append(nodes, simplyblockv1alpha1.NodeStatus{MgmtIp: n.ip, FailureDomain: fd(domain), UUID: uuid})
	}
	return nodes
}

func TestFdRemovalBalanceCheck_RemovingFromSlackDomainAllowed(t *testing.T) {
	// Removing from FD3 (2/2/3 -> 2/2/2) is fine.
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.7"}
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, sevenNodeTopology("10.0.0.7", 3)...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, true)
	r := newOpsReconciler(t, sn, sns, cluster)

	reason, err := r.fdRemovalBalanceCheck(context.Background(), sn)
	if err != nil {
		t.Fatalf("fdRemovalBalanceCheck returned error: %v", err)
	}
	if reason != "" {
		t.Errorf("expected removal from FD3 (2/2/3 -> 2/2/2) to be allowed, got blocked: %q", reason)
	}
}

func TestFdRemovalBalanceCheck_RemovingFromThinDomainBlocked(t *testing.T) {
	// Removing from FD1 (2/2/3 -> 1/2/3) violates the +/-1 rule.
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.2"}
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, sevenNodeTopology("10.0.0.2", 1)...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, true)
	r := newOpsReconciler(t, sn, sns, cluster)

	reason, err := r.fdRemovalBalanceCheck(context.Background(), sn)
	if err != nil {
		t.Fatalf("fdRemovalBalanceCheck returned error: %v", err)
	}
	if reason == "" {
		t.Error("expected removal from FD1 (2/2/3 -> 1/2/3) to be blocked, got none")
	}
}

// TestFdRemovalBalanceCheck_NoOpWhenFailureDomainsDisabled locks in the
// gate's very first early-out, mirroring check_fd_admission_for_remove's
// own first line (simplyblock_core): with EnableFailureDomains unset/false,
// this must never block a removal, regardless of topology -- the exact
// same 1/2/3 split that TestFdRemovalBalanceCheck_RemovingFromThinDomainBlocked
// correctly blocks above must be a no-op when the cluster hasn't opted in.
func TestFdRemovalBalanceCheck_NoOpWhenFailureDomainsDisabled(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.2"}
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, sevenNodeTopology("10.0.0.2", 1)...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, false)
	r := newOpsReconciler(t, sn, sns, cluster)

	reason, err := r.fdRemovalBalanceCheck(context.Background(), sn)
	if err != nil {
		t.Fatalf("fdRemovalBalanceCheck returned error: %v", err)
	}
	if reason != "" {
		t.Errorf("expected no-op with failure domains disabled, got blocked: %q", reason)
	}
}

// TestFdRemovalBalanceCheck_MultiNodeHostSiblingSurvivesAllowsRemoval is a
// regression for the 2026-08-26 finding: a host running more than one
// StorageNode (spec.socketsToUse / spec.nodesPerSocket > 1) must not vanish
// from the failure-domain host count when only one of its nodes is removed
// -- the sibling node still lives there. 3 domains, 2 hosts each (balanced
// 2/2/2); FD3's second host (10.0.0.6) runs two StorageNodes sharing that
// management IP. Removing one of them must leave FD3 at 2 hosts, not drop
// it to 1 -- the old delete(hostDomains, ip) removed the whole host and
// produced a spurious "failure domain 3 would drop to 1 host(s)" block.
func TestFdRemovalBalanceCheck_MultiNodeHostSiblingSurvivesAllowsRemoval(t *testing.T) {
	fd := func(v int32) *int32 { return &v }
	const siblingUUID = "bbbb0000-0000-0000-0000-000000000002"
	nodes := []simplyblockv1alpha1.NodeStatus{
		{MgmtIp: "10.0.0.1", FailureDomain: fd(1), UUID: "n1"},
		{MgmtIp: "10.0.0.2", FailureDomain: fd(1), UUID: "n2"},
		{MgmtIp: "10.0.0.3", FailureDomain: fd(2), UUID: "n3"},
		{MgmtIp: "10.0.0.4", FailureDomain: fd(2), UUID: "n4"},
		{MgmtIp: "10.0.0.5", FailureDomain: fd(3), UUID: "n5"},
		{MgmtIp: "10.0.0.6", FailureDomain: fd(3), UUID: opsTestNodeUUID}, // node being removed
		{MgmtIp: "10.0.0.6", FailureDomain: fd(3), UUID: siblingUUID},     // surviving sibling, same host
	}
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.6"}
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, nodes...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, true)
	r := newOpsReconciler(t, sn, sns, cluster)

	reason, err := r.fdRemovalBalanceCheck(context.Background(), sn)
	if err != nil {
		t.Fatalf("fdRemovalBalanceCheck returned error: %v", err)
	}
	if reason != "" {
		t.Errorf("expected removal to be allowed (sibling survives on the host), got blocked: %q", reason)
	}
}

// TestFdRemovalBalanceCheck_DomainDroppingToZeroBlocked is a regression for
// the 2026-08-26 finding: A=2,B=2,C=1 -- removing C's only host must be
// blocked. It would drop the cluster from 3 domains to 2, exactly the
// topology fdActivationDomainCountViolation itself calls unsupported; the
// removal gate must not permit what the activation gate would refuse. The
// old code derived counts from a hostDomains map with C's key deleted, so C
// vanished from the map instead of appearing at count zero, and neither the
// +/-1 spread nor the 2-per-domain floor check ever saw it.
func TestFdRemovalBalanceCheck_DomainDroppingToZeroBlocked(t *testing.T) {
	fd := func(v int32) *int32 { return &v }
	nodes := []simplyblockv1alpha1.NodeStatus{
		{MgmtIp: "10.0.0.1", FailureDomain: fd(1), UUID: "n1"},
		{MgmtIp: "10.0.0.2", FailureDomain: fd(1), UUID: "n2"},
		{MgmtIp: "10.0.0.3", FailureDomain: fd(2), UUID: "n3"},
		{MgmtIp: "10.0.0.4", FailureDomain: fd(2), UUID: "n4"},
		{MgmtIp: "10.0.0.5", FailureDomain: fd(3), UUID: opsTestNodeUUID}, // domain 3's only host
	}
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.5"}
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, nodes...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, true)
	r := newOpsReconciler(t, sn, sns, cluster)

	reason, err := r.fdRemovalBalanceCheck(context.Background(), sn)
	if err != nil {
		t.Fatalf("fdRemovalBalanceCheck returned error: %v", err)
	}
	if reason == "" {
		t.Error("expected removal to be blocked (would drop domain 3 to zero hosts), got allowed")
	}
}

// TestDrainValidate_FailureDomainBalanceViolationFailsOps exercises the full
// drainValidate wiring, not just fdRemovalBalanceCheck: a blocked removal
// must land in Phase=Failed (not a silent 60s-requeue Running loop) --
// deliberately different from the pinned/unmanaged-volume checks above it,
// since restoring failure-domain balance needs a cluster-wide human action
// this ops has no way to detect on its own. Safe to fail here specifically
// because handleDeletion (storagenode_controller.go) now refuses to remove
// the StorageNode's finalizer while its remove ops is Failed.
func TestDrainValidate_FailureDomainBalanceViolationFailsOps(t *testing.T) {
	// Empty storage-pools list -> fetchPoolVolumes returns zero volumes ->
	// pinned/unmanaged checks pass through, reaching the FD-balance gate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.2"}
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	sns := newTestStorageNodeSet("sns", opsTestNS, opsTestCluster, sevenNodeTopology("10.0.0.2", 1)...)
	cluster := newTestStorageClusterWithFD(opsTestCluster, opsTestNS, true)
	r := newOpsReconciler(t, sn, ops, sns, cluster)

	_, err := r.drainValidate(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("drainValidate returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	if err := r.Get(context.Background(), types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updated); err != nil {
		t.Fatalf("failed to fetch updated ops: %v", err)
	}
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("Phase: got %q, want Failed", updated.Status.Phase)
	}
	if updated.Status.Message == "" {
		t.Error("expected a failure message explaining the balance violation")
	}
}

func TestAcquireLock_RemoveDrainSetsValidatingSubPhase(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps("ops-drain", opsTestNS, "sn-1", "remove")
	r := newOpsReconciler(t, sn, ops)

	_, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-drain", Namespace: opsTestNS}, &updated)
	if updated.Status.SubPhase != simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating {
		t.Errorf("subPhase: got %q want Validating", updated.Status.SubPhase)
	}
}

// ── TestSucceedOps ────────────────────────────────────────────────────────────

func TestSucceedOps_SetsPhaseAndClearsLock(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = opsTestOpsName
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "suspend")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, ops)

	_, err := r.succeedOps(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("succeedOps returned error: %v", err)
	}

	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase: got %q want Succeeded", updatedOps.Status.Phase)
	}
	if updatedOps.Status.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared, got %q", updatedSN.Status.ActiveOpsRef)
	}
}

// ── TestFailOps ───────────────────────────────────────────────────────────────

func TestFailOps_SetsPhaseAndClearsLock(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = opsTestOpsName
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "suspend")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, ops)

	_, err := r.failOps(context.Background(), ops, "something went wrong")
	if err != nil {
		t.Fatalf("failOps returned error: %v", err)
	}

	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed", updatedOps.Status.Phase)
	}
	if updatedOps.Status.Message != "something went wrong" {
		t.Errorf("message: got %q", updatedOps.Status.Message)
	}

	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared after failure, got %q", updatedSN.Status.ActiveOpsRef)
	}
}

// ── TestReleaseLock ───────────────────────────────────────────────────────────

func TestReleaseLock_OnlyClearsIfOwner(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = opsTestOtherOps
	r := newOpsReconciler(t, sn)

	// Releasing with a different name should be a no-op.
	if err := r.releaseLock(context.Background(), sn, opsTestOpsName); err != nil {
		t.Fatalf("releaseLock returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updated)
	if updated.Status.ActiveOpsRef != opsTestOtherOps {
		t.Error("releaseLock should not clear a lock it does not own")
	}
}

// ── TestAdvanceSubPhase ───────────────────────────────────────────────────────

func TestAdvanceSubPhase_UpdatesSubPhaseAndResetsTrigger(t *testing.T) {
	ops := newTestStorageNodeOps("ops-drain", opsTestNS, "sn-1", "remove")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating
	ops.Status.Triggered = true
	r := newOpsReconciler(t, ops)

	_, err := r.advanceSubPhase(context.Background(), ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending)
	if err != nil {
		t.Fatalf("advanceSubPhase returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-drain", Namespace: opsTestNS}, &updated)
	if updated.Status.SubPhase != simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending {
		t.Errorf("subPhase: got %q want Suspending", updated.Status.SubPhase)
	}
	if updated.Status.Triggered {
		t.Error("Triggered should be reset to false on phase advance")
	}
}

// ── TestDispatch ──────────────────────────────────────────────────────────────

func TestDispatch_UnknownActionFails(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: opsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: opsTestCluster},
	}
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "bogus-action")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, sns, ops)

	_, err := r.dispatch(context.Background(), ops, sn, sns, "cluster-uuid", nil)
	if err != nil {
		t.Fatalf("dispatch returned unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updated)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("expected Failed for unknown action, got %q", updated.Status.Phase)
	}
}

// ── TestResolveOpsSystemVolumeFilter ─────────────────────────────────────────

func TestResolveOpsSystemVolumeFilter_UsesDefaultWhenNoDrain(t *testing.T) {
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "remove")
	r := newOpsReconciler(t, ops)

	re, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Default pattern matches sb-fio-baseline-* names.
	if !re.MatchString("sb-fio-baseline-read") {
		t.Error("default filter should match sb-fio-baseline-read")
	}
	if re.MatchString("user-volume") {
		t.Error("default filter should not match user volumes")
	}
}

func TestResolveOpsSystemVolumeFilter_UsesCustomPattern(t *testing.T) {
	custom := "^bench-.*"
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "remove")
	ops.Spec.Drain = &simplyblockv1alpha1.DrainOpsSpec{SystemVolumeFilterRegex: &custom}
	r := newOpsReconciler(t, ops)

	re, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !re.MatchString("bench-read") {
		t.Error("custom filter should match bench-read")
	}
	if re.MatchString("sb-fio-baseline-read") {
		t.Error("custom filter should not match sb-fio-baseline-read")
	}
}

func TestResolveOpsSystemVolumeFilter_InvalidPatternReturnsError(t *testing.T) {
	bad := "["
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "remove")
	ops.Spec.Drain = &simplyblockv1alpha1.DrainOpsSpec{SystemVolumeFilterRegex: &bad}
	r := newOpsReconciler(t, ops)

	_, err := r.resolveOpsSystemVolumeFilter(ops)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

// TestEndpointSliceHasWorker_MatchesBuilderOutput guards the coupling between the
// EndpointSlice builder and the migrate flow's DNS gate: a slice built by
// BuildStorageNodeSetEndpointSlice must be found by endpointSliceHasWorker. The
// two independently encoded the slice name and hostname, and a rename that
// touched only the builder silently wedged migrations at "waiting for DNS".
func TestEndpointSliceHasWorker_MatchesBuilderOutput(t *testing.T) {
	const ns = "test"
	const worker = "worker-5.ocp.simplyblock.ai"

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-node", Namespace: ns},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
	}
	// Build the slice exactly as reconcileEndpointSlice does.
	slice := utils.BuildStorageNodeSetEndpointSlice(sns, map[string]string{worker: "10.0.0.15"})

	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		discoveryv1.AddToScheme,
	)
	cl := newTestClient(t, scheme, nil, slice)
	r := &StorageNodeOpsReconciler{Client: cl, Scheme: scheme, Recorder: events.NewFakeRecorder(16), apiReader: cl}

	// The enrolled worker is found — this is what the drifted name broke.
	ok, err := r.endpointSliceHasWorker(context.Background(), ns, sns.Name, worker)
	if err != nil {
		t.Fatalf("endpointSliceHasWorker returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected worker %q to be found in slice %q built for StorageNodeSet %q", worker, slice.Name, sns.Name)
	}

	// A worker not published is not found.
	ok, err = r.endpointSliceHasWorker(context.Background(), ns, sns.Name, "worker-0.ocp.simplyblock.ai")
	if err != nil {
		t.Fatalf("endpointSliceHasWorker returned error: %v", err)
	}
	if ok {
		t.Fatal("did not expect an unpublished worker to be found")
	}

	// A different StorageNodeSet name resolves to a different (absent) slice, so
	// the worker is not found — guards the per-set name derivation.
	ok, err = r.endpointSliceHasWorker(context.Background(), ns, "other-set", worker)
	if err != nil {
		t.Fatalf("endpointSliceHasWorker returned error: %v", err)
	}
	if ok {
		t.Fatal("expected miss when querying under the wrong StorageNodeSet name")
	}
}
