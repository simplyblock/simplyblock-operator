package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ---- pure helpers ----

func TestGetDrainState(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseDetected},
				{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDraining},
			},
		},
	}

	s := getDrainState(snCR, "node-a")
	if s == nil || s.Phase != simplyblockv1alpha1.DrainPhaseDetected {
		t.Fatalf("expected DrainPhaseDetected for node-a, got %v", s)
	}

	if getDrainState(snCR, "node-c") != nil {
		t.Fatalf("expected nil for unknown hostname")
	}
}

func TestUpsertDrainState(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{}

	upsertDrainState(snCR, simplyblockv1alpha1.NodeDrainState{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseDetected})
	if len(snCR.Status.DrainCoordination) != 1 {
		t.Fatalf("expected 1 entry after insert, got %d", len(snCR.Status.DrainCoordination))
	}

	upsertDrainState(snCR, simplyblockv1alpha1.NodeDrainState{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseShutdownCalled})
	if len(snCR.Status.DrainCoordination) != 1 {
		t.Fatalf("expected 1 entry after update (no duplicate), got %d", len(snCR.Status.DrainCoordination))
	}
	if snCR.Status.DrainCoordination[0].Phase != simplyblockv1alpha1.DrainPhaseShutdownCalled {
		t.Fatalf("expected updated phase, got %q", snCR.Status.DrainCoordination[0].Phase)
	}

	upsertDrainState(snCR, simplyblockv1alpha1.NodeDrainState{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDraining})
	if len(snCR.Status.DrainCoordination) != 2 {
		t.Fatalf("expected 2 entries after inserting second node, got %d", len(snCR.Status.DrainCoordination))
	}
}

func TestRemoveDrainState(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseComplete},
				{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDraining},
			},
		},
	}

	removeDrainState(snCR, "node-a")
	if len(snCR.Status.DrainCoordination) != 1 {
		t.Fatalf("expected 1 entry after remove, got %d", len(snCR.Status.DrainCoordination))
	}
	if snCR.Status.DrainCoordination[0].Hostname != "node-b" {
		t.Fatalf("expected node-b to remain, got %q", snCR.Status.DrainCoordination[0].Hostname)
	}

	// removing an absent entry is a no-op
	removeDrainState(snCR, "node-missing")
	if len(snCR.Status.DrainCoordination) != 1 {
		t.Fatalf("expected no change when removing absent hostname")
	}
}

func TestFindNodeUUID(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
				{Hostname: "node-b", UUID: "uuid-b"},
			},
		},
	}

	if got := findNodeUUID(snCR, "node-a"); got != "uuid-a" {
		t.Fatalf("expected uuid-a, got %q", got)
	}
	if got := findNodeUUID(snCR, "node-missing"); got != "" {
		t.Fatalf("expected empty string for unknown hostname, got %q", got)
	}
}

func TestIsWorkerOnline(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-online", Status: "online"},
				{Hostname: "node-offline", Status: "offline"},
			},
		},
	}

	if !isWorkerOnline(snCR, "node-online") {
		t.Fatalf("expected node-online to be online")
	}
	if isWorkerOnline(snCR, "node-offline") {
		t.Fatalf("expected node-offline to not be online")
	}
	if isWorkerOnline(snCR, "node-missing") {
		t.Fatalf("expected missing node to not be online")
	}
}

func TestIsNodeReady(t *testing.T) {
	ready := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	notReady := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	noConditions := &corev1.Node{}

	if !isNodeReady(ready) {
		t.Fatalf("expected ready node to return true")
	}
	if isNodeReady(notReady) {
		t.Fatalf("expected not-ready node to return false")
	}
	if isNodeReady(noConditions) {
		t.Fatalf("expected node with no conditions to return false")
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	short := "node-abc"
	if got := sanitizeLabelValue(short); got != short {
		t.Fatalf("short value should be unchanged, got %q", got)
	}

	exactly63 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"[:63]
	if got := sanitizeLabelValue(exactly63); got != exactly63 {
		t.Fatalf("63-char value should be unchanged")
	}

	long := "a" + exactly63 // 64 chars
	got := sanitizeLabelValue(long)
	if len(got) != 63 {
		t.Fatalf("expected truncation to 63 chars, got len=%d", len(got))
	}
}

func TestCountActiveDrainsControllerState(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "n1", Phase: simplyblockv1alpha1.DrainPhaseShutdownCalled},
				{Hostname: "n2", Phase: simplyblockv1alpha1.DrainPhaseDraining},
				{Hostname: "n3", Phase: simplyblockv1alpha1.DrainPhaseRestartCalled},
				{Hostname: "n4", Phase: simplyblockv1alpha1.DrainPhaseComplete},
				{Hostname: "n5", Phase: simplyblockv1alpha1.DrainPhaseDetected},
				{Hostname: "n6", Phase: simplyblockv1alpha1.DrainPhaseFailed},
			},
			// No Nodes with UUIDs → no backend calls.
		},
	}

	got := countActiveDrains(context.Background(), snCR, webapi.NewClient("http://127.0.0.1:1"), "cluster")
	if got != 3 {
		t.Fatalf("expected 3 active drains (shutdown_called, draining, restart_called), got %d", got)
	}
}

func TestCountActiveDrainsBackendConservative(t *testing.T) {
	// Backend API unreachable → node is counted as active (conservative).
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
			},
		},
	}

	// Use an unreachable address to force backend error.
	got := countActiveDrains(context.Background(), snCR, webapi.NewClient("http://127.0.0.1:1"), "cluster")
	if got < 1 {
		t.Fatalf("expected at least 1 (conservative count on API error), got %d", got)
	}
}

func TestCountActiveDrainsBackendTakesPrecedence(t *testing.T) {
	// Backend reports 2 in_shutdown; controller state has 0 active.
	// countActiveDrains should return the backend count when it's higher.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"in_shutdown","health_check":false}`))
	}))
	defer srv.Close()

	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
				{Hostname: "node-b", UUID: "uuid-b"},
			},
		},
	}

	got := countActiveDrains(context.Background(), snCR, webapi.NewClient(srv.URL), "cluster")
	if got != 2 {
		t.Fatalf("expected backend count of 2, got %d", got)
	}
}

func TestActiveDrainWorkersUnionsControllerAndBackend(t *testing.T) {
	// Controller state flags node-a; backend independently flags node-b (e.g.
	// a real failure with no coordinator-driven drain in progress). Both must
	// appear in the set so the failure-domain gate sees each affected domain.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"in_shutdown","health_check":false}`))
	}))
	defer srv.Close()

	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseShutdownCalled},
			},
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-b", UUID: "uuid-b"},
			},
		},
	}

	got := activeDrainWorkers(context.Background(), snCR, webapi.NewClient(srv.URL), "cluster")
	if !got["node-a"] || !got["node-b"] || len(got) != 2 {
		t.Fatalf("expected {node-a, node-b}, got %v", got)
	}
}

func TestActiveDrainWorkersConservativeOnBackendError(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
			},
		},
	}

	got := activeDrainWorkers(context.Background(), snCR, webapi.NewClient("http://127.0.0.1:1"), "cluster")
	if !got["node-a"] {
		t.Fatalf("expected node-a marked active conservatively on API error, got %v", got)
	}
}

func TestDistinctDomainCount(t *testing.T) {
	fd1, fd2, fd3 := int32(1), int32(2), int32(3)
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "a", FailureDomain: &fd1},
				{Hostname: "b", FailureDomain: &fd1},
				{Hostname: "c", FailureDomain: &fd2},
				{Hostname: "d", FailureDomain: &fd3},
				{Hostname: "e"}, // unassigned -> excluded
			},
		},
	}

	if got := distinctDomainCount(snCR); got != 3 {
		t.Fatalf("expected 3 distinct domains, got %d", got)
	}
	if got := distinctDomainCount(&simplyblockv1alpha1.StorageNodeSet{}); got != 0 {
		t.Fatalf("expected 0 for empty status, got %d", got)
	}
}

func TestActiveDrainDomainCountsTallies(t *testing.T) {
	fd1, fd2 := int32(1), int32(2)
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", FailureDomain: &fd1},
				{Hostname: "node-b", FailureDomain: &fd1},
				{Hostname: "node-c", FailureDomain: &fd2},
				// node-d intentionally has no status entry -> excluded
			},
		},
	}
	activeWorkers := map[string]bool{"node-a": true, "node-b": true, "node-c": true, "node-d": true}
	counts := activeDrainDomainCounts(snCR, activeWorkers)
	if counts[1] != 2 || counts[2] != 1 || len(counts) != 2 {
		t.Fatalf("expected {1:2, 2:1}, got %v", counts)
	}
}

// fdDrainGate is confirmed against the backend team's stated requirements
// (2026-08, Dmitrii Iakovlev) for a 2+2 layout:
//   - 2 domains: "1 whole domain" or "1 node in each of the 2 domains" are
//     the only safe combinations.
//   - 3 domains: only 1 domain may be fully down, not 2.
//   - 4 domains: 2 domains may be fully down (well-provisioned case).

func TestFdDrainGate2DomainsOneWholeDomainSafe(t *testing.T) {
	// chunksPerDomain = ceil(4/2) = 2. Domain 1 already has 3 nodes down
	// (maxed at chunksPerDomain regardless of how many more) -- a 4th is free.
	counts := map[int32]int{1: 3}
	if blocked, _ := fdDrainGate(counts, 1, 2, 4, 2); blocked {
		t.Fatalf("expected piling further within an already-maxed domain to proceed")
	}
}

func TestFdDrainGate2DomainsOneNodePerDomainSafe(t *testing.T) {
	counts := map[int32]int{1: 1}
	if blocked, _ := fdDrainGate(counts, 2, 2, 4, 2); blocked {
		t.Fatalf("expected 1 node in each of the 2 domains to proceed")
	}
}

func TestFdDrainGate2DomainsWholeDomainPlusOtherIsUnsafe(t *testing.T) {
	// Domain 1 fully down (4 nodes, maxed at chunksPerDomain=2) -- a node in
	// domain 2 must now be blocked (not one of the two safe combinations).
	counts := map[int32]int{1: 4}
	blocked, reason := fdDrainGate(counts, 2, 2, 4, 2)
	if !blocked || reason == "" {
		t.Fatalf("expected domain 2 to be blocked once domain 1 is fully down, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestFdDrainGate2DomainsOnePerDomainPlusExtraInEitherIsUnsafe(t *testing.T) {
	// 1 node down in each of domains 1 and 2 already (the safe combo) --
	// piling a SECOND node onto EITHER domain must now be blocked, even
	// though that domain is already "active". This is the exact gap the old
	// unconditional-piling logic missed.
	counts := map[int32]int{1: 1, 2: 1}
	if blocked, _ := fdDrainGate(counts, 2, 2, 4, 2); !blocked {
		t.Fatalf("expected a 2nd node in domain 2 to be blocked once 1+1 is already committed")
	}
	if blocked, _ := fdDrainGate(counts, 1, 2, 4, 2); !blocked {
		t.Fatalf("expected a 2nd node in domain 1 to be blocked once 1+1 is already committed")
	}
}

func TestFdDrainGate3DomainsOneWholeDomainSafeTwoUnsafe(t *testing.T) {
	// chunksPerDomain = ceil(4/3) = 2, same per-domain cap as 2 domains, but
	// spread across 3. 1 domain fully down is safe; opening a 2nd is not.
	counts := map[int32]int{1: 2} // domain 1 already maxed
	if blocked, _ := fdDrainGate(counts, 1, 3, 4, 2); blocked {
		t.Fatalf("expected piling within the already-maxed domain 1 to proceed")
	}
	if blocked, _ := fdDrainGate(counts, 2, 3, 4, 2); !blocked {
		t.Fatalf("expected opening domain 2 to be blocked once domain 1 has maxed the risk budget")
	}
}

func TestFdDrainGate4DomainsTwoWholeDomainsSafeThreeUnsafe(t *testing.T) {
	// chunksPerDomain = ceil(4/4) = 1 (well-provisioned): up to npcs=2 whole
	// domains may be fully down.
	counts := map[int32]int{1: 1} // domain 1 already maxed (chunksPerDomain=1)
	if blocked, _ := fdDrainGate(counts, 2, 4, 4, 2); blocked {
		t.Fatalf("expected opening a 2nd domain to proceed while under the npcs=2 domain budget")
	}
	countsTwoActive := map[int32]int{1: 1, 2: 1}
	if blocked, _ := fdDrainGate(countsTwoActive, 3, 4, 4, 2); !blocked {
		t.Fatalf("expected opening a 3rd domain to be blocked once 2 domains already max the npcs=2 budget")
	}
}

func TestFdDrainGateUnknownSchemeTreatedAsSingleDomainChunk(t *testing.T) {
	// domainsNeededForFullDisjoint=0 signals the scheme could not be parsed;
	// chunksPerDomain must fall back to >= 1, not 0 (which would divide by
	// zero / always-block via a degenerate cap).
	counts := map[int32]int{1: 1}
	blocked, _ := fdDrainGate(counts, 2, 5, 0, 2)
	if blocked {
		t.Fatalf("expected an unparsed scheme to still allow a 2nd domain within the npcs budget, got blocked")
	}
}

// ---- reconciler tests ----

func TestNodeDrainReconcileNotFound(t *testing.T) {
	r := newNodeDrainTestReconciler(t)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error for missing CR, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for missing CR, got %+v", res)
	}
}

func TestNodeDrainReconcileNoClusterAuthRequeues(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-no-auth", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: "cluster-missing",
		},
	}
	r := newNodeDrainTestReconciler(t, snCR)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snCR)})
	if err != nil {
		t.Fatalf("expected no error when cluster auth unavailable, got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster auth is unavailable")
	}
}

func TestNodeDrainReconcileNoClusterCRRequeues(t *testing.T) {
	const clusterName = "cluster-no-cr"

	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-no-cluster-cr", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: clusterName,
		},
	}
	r := newNodeDrainTestReconciler(t, snCR)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snCR)})
	if err != nil {
		t.Fatalf("expected no error when cluster CR unavailable, got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster CR is missing")
	}
}

func TestNodeDrainReconcileSkipsWhenClusterUnready(t *testing.T) {
	const clusterName = "cluster-unready"

	clusterCR := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			Status: utils.ClusterStatusUnready,
		},
	}
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-unready", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: clusterName,
		},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{UUID: "node-1", Status: utils.NodeStatusOnline, Hostname: "worker-1"},
			},
		},
	}
	r := newNodeDrainTestReconciler(t, snCR, clusterCR)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snCR)})
	if err != nil {
		t.Fatalf("expected no error when cluster is unready, got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a delayed requeue when cluster is unready")
	}

	// No PDBs should have been created — drain coordinator must not run while cluster is unready.
	var pdbList policyv1.PodDisruptionBudgetList
	if err := r.List(context.Background(), &pdbList); err != nil {
		t.Fatalf("list PDBs: %v", err)
	}
	if len(pdbList.Items) != 0 {
		t.Errorf("expected no PDBs when cluster is unready, got %d", len(pdbList.Items))
	}
}

func TestEnsurePDBCreatesWhenMissing(t *testing.T) {
	r := newNodeDrainTestReconciler(t)

	if err := r.ensurePDB(context.Background(), "default", "node-a", 0); err != nil {
		t.Fatalf("ensurePDB returned error: %v", err)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Name:      drainPDBPrefix + "node-a",
		Namespace: "default",
	}, pdb); err != nil {
		t.Fatalf("PDB should have been created: %v", err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 0 {
		t.Fatalf("expected maxUnavailable=0, got %v", pdb.Spec.MaxUnavailable)
	}
}

func TestEnsurePDBUpdatesExisting(t *testing.T) {
	maxUnavailable := intstr.FromInt32(0)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drainPDBPrefix + "node-b",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{drainNodeLabelKey: "node-b"},
			},
		},
	}
	r := newNodeDrainTestReconciler(t, existing)

	if err := r.ensurePDB(context.Background(), "default", "node-b", 1); err != nil {
		t.Fatalf("ensurePDB update returned error: %v", err)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Name:      drainPDBPrefix + "node-b",
		Namespace: "default",
	}, pdb); err != nil {
		t.Fatalf("failed to fetch PDB: %v", err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("expected maxUnavailable=1 after update, got %v", pdb.Spec.MaxUnavailable)
	}
}

func TestCleanupPDBDeletesWhenPresent(t *testing.T) {
	maxUnavailable := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drainPDBPrefix + "node-c",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{drainNodeLabelKey: "node-c"}},
		},
	}
	r := newNodeDrainTestReconciler(t, pdb)

	if err := r.cleanupPDB(context.Background(), "default", "node-c"); err != nil {
		t.Fatalf("cleanupPDB returned error: %v", err)
	}

	out := &policyv1.PodDisruptionBudget{}
	err := r.Get(context.Background(), client.ObjectKey{Name: drainPDBPrefix + "node-c", Namespace: "default"}, out)
	if err == nil {
		t.Fatalf("expected PDB to be deleted")
	}
}

func TestCleanupPDBNoopWhenMissing(t *testing.T) {
	r := newNodeDrainTestReconciler(t)
	if err := r.cleanupPDB(context.Background(), "default", "node-missing"); err != nil {
		t.Fatalf("cleanupPDB should be no-op for missing PDB, got error: %v", err)
	}
}

func TestLabelStoragePodLabelsMatchingPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spdk-pod",
			Namespace: "default",
			Labels:    map[string]string{"role": "simplyblock-storage-node"},
		},
		Spec: corev1.PodSpec{NodeName: "node-d"},
	}
	r := newNodeDrainTestReconciler(t, pod)

	if err := r.labelStoragePod(context.Background(), "default", "node-d"); err != nil {
		t.Fatalf("labelStoragePod returned error: %v", err)
	}

	out := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), out); err != nil {
		t.Fatalf("failed to fetch pod: %v", err)
	}
	if out.Labels[drainNodeLabelKey] != sanitizeLabelValue("node-d") {
		t.Fatalf("expected drain label to be set, got %q", out.Labels[drainNodeLabelKey])
	}
}

func TestLabelStoragePodSkipsDifferentNode(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spdk-pod-other",
			Namespace: "default",
			Labels:    map[string]string{"role": "simplyblock-storage-node"},
		},
		Spec: corev1.PodSpec{NodeName: "node-other"},
	}
	r := newNodeDrainTestReconciler(t, pod)

	if err := r.labelStoragePod(context.Background(), "default", "node-target"); err != nil {
		t.Fatalf("labelStoragePod returned error: %v", err)
	}

	out := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), out); err != nil {
		t.Fatalf("failed to fetch pod: %v", err)
	}
	if _, ok := out.Labels[drainNodeLabelKey]; ok {
		t.Fatalf("expected drain label NOT to be set on pod from different node")
	}
}

func TestLabelStoragePodIdempotent(t *testing.T) {
	nodeName := "node-e"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spdk-pod-idempotent",
			Namespace: "default",
			Labels: map[string]string{
				"role":            "simplyblock-storage-node",
				drainNodeLabelKey: sanitizeLabelValue(nodeName),
			},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
	}
	r := newNodeDrainTestReconciler(t, pod)

	// Should succeed without error (patch is skipped for already-labeled pods).
	if err := r.labelStoragePod(context.Background(), "default", nodeName); err != nil {
		t.Fatalf("labelStoragePod returned error on idempotent call: %v", err)
	}
}

func TestCleanupDrainResources(t *testing.T) {
	nodeName := "node-f"
	maxUnavailable := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drainPDBPrefix + nodeName,
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{drainNodeLabelKey: nodeName}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spdk-pod-cleanup",
			Namespace: "default",
			Labels: map[string]string{
				drainNodeLabelKey: sanitizeLabelValue(nodeName),
			},
		},
	}
	r := newNodeDrainTestReconciler(t, pdb, pod)

	if err := r.cleanupDrainResources(context.Background(), "default", nodeName); err != nil {
		t.Fatalf("cleanupDrainResources returned error: %v", err)
	}

	// PDB should be gone.
	out := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: drainPDBPrefix + nodeName, Namespace: "default"}, out); err == nil {
		t.Fatalf("expected PDB to be deleted")
	}

	// Drain label should be removed from the pod.
	outPod := &corev1.Pod{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), outPod); err != nil {
		t.Fatalf("failed to fetch pod: %v", err)
	}
	if _, ok := outPod.Labels[drainNodeLabelKey]; ok {
		t.Fatalf("expected drain label to be removed from pod")
	}
}

func TestEnsureManagerPDBCreates(t *testing.T) {
	r := newNodeDrainTestReconciler(t)

	if err := r.ensureManagerPDB(context.Background(), "default"); err != nil {
		t.Fatalf("ensureManagerPDB returned error: %v", err)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: managerPDBName, Namespace: "default"}, pdb); err != nil {
		t.Fatalf("manager PDB should have been created: %v", err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 0 {
		t.Fatalf("expected manager PDB maxUnavailable=0")
	}
}

func TestDeleteManagerPDBDeletesWhenPresent(t *testing.T) {
	maxUnavailable := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: managerPDBName, Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "simplyblock-operator"}},
		},
	}
	r := newNodeDrainTestReconciler(t, pdb)

	if err := r.deleteManagerPDB(context.Background(), "default"); err != nil {
		t.Fatalf("deleteManagerPDB returned error: %v", err)
	}

	out := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: managerPDBName, Namespace: "default"}, out); err == nil {
		t.Fatalf("expected manager PDB to be deleted")
	}
}

func TestDeleteManagerPDBNoopWhenMissing(t *testing.T) {
	r := newNodeDrainTestReconciler(t)
	if err := r.deleteManagerPDB(context.Background(), "default"); err != nil {
		t.Fatalf("deleteManagerPDB should be no-op when missing, got error: %v", err)
	}
}

func TestCleanupManagerPDBIfStaleRemovesWhenNotInDetectedPhase(t *testing.T) {
	maxUnavailable := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: managerPDBName, Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "simplyblock-operator"}},
		},
	}
	// Manager node is NOT in detected phase (no drain state at all).
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
	}
	r := newNodeDrainTestReconciler(t, pdb)
	r.ManagerNodeName = "manager-node"

	r.cleanupManagerPDBIfStale(context.Background(), snCR)

	out := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: managerPDBName, Namespace: "default"}, out); err == nil {
		t.Fatalf("expected stale manager PDB to be deleted")
	}
}

func TestCleanupManagerPDBIfStaleKeepsWhenDetected(t *testing.T) {
	maxUnavailable := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: managerPDBName, Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "simplyblock-operator"}},
		},
	}
	// Manager node IS in the detected phase — PDB should be kept.
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "manager-node", Phase: simplyblockv1alpha1.DrainPhaseDetected},
			},
		},
	}
	r := newNodeDrainTestReconciler(t, pdb)
	r.ManagerNodeName = "manager-node"

	r.cleanupManagerPDBIfStale(context.Background(), snCR)

	out := &policyv1.PodDisruptionBudget{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: managerPDBName, Namespace: "default"}, out); err != nil {
		t.Fatalf("expected manager PDB to be kept during detected phase: %v", err)
	}
}

func TestProcessWorkerUncordonedNoState(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-g"},
		Spec:       corev1.NodeSpec{Unschedulable: false},
	}
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{WorkerNodes: []string{"node-g"}},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	requeue, shouldBreak := r.processWorker(
		context.Background(), snCR, "node-g",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", 1, false, 0, 0,
	)
	if requeue != 0 || shouldBreak {
		t.Fatalf("expected (0, false) for uncordoned node with no state, got (%v, %v)", requeue, shouldBreak)
	}
	if getDrainState(snCR, "node-g") != nil {
		t.Fatalf("expected no drain state to be created")
	}
}

func TestProcessWorkerSkipsCordonedNotYetOnline(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-h"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	// No Nodes in status → isWorkerOnline returns false.
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-h", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{WorkerNodes: []string{"node-h"}},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	requeue, shouldBreak := r.processWorker(
		context.Background(), snCR, "node-h",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", 1, false, 0, 0,
	)
	if requeue != 0 || shouldBreak {
		t.Fatalf("expected (0, false) for cordoned node not yet online, got (%v, %v)", requeue, shouldBreak)
	}
	if getDrainState(snCR, "node-h") != nil {
		t.Fatalf("expected no drain state created for node that was never online")
	}
}

func TestProcessWorkerCordonedOnlineInitializesState(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-i"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	// Node is online in backend status.
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-i", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{WorkerNodes: []string{"node-i"}},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-i", Status: "online", UUID: ""},
			},
		},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	r.processWorker(
		context.Background(), snCR, "node-i",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", 1, false, 0, 0,
	)

	// Drain state must have been initialized (phase may be detected or failed
	// depending on backend reachability, but the entry must exist).
	if getDrainState(snCR, "node-i") == nil {
		t.Fatalf("expected drain state to be created for cordoned online node")
	}
}

func TestIsClusterRebalancingTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"is_re_balancing":true}`))
	}))
	defer srv.Close()

	rebalancing, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "cluster-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebalancing {
		t.Fatalf("expected rebalancing=true, got false")
	}
}

func TestIsClusterRebalancingFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"is_re_balancing":false}`))
	}))
	defer srv.Close()

	rebalancing, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "cluster-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rebalancing {
		t.Fatalf("expected rebalancing=false, got true")
	}
}

func TestIsClusterRebalancingAPIError(t *testing.T) {
	// Unreachable address → error expected.
	_, err := isClusterRebalancing(context.Background(), webapi.NewClient("http://127.0.0.1:1"), "cluster-uuid")
	if err == nil {
		t.Fatalf("expected error when API is unreachable")
	}
}

func TestIsClusterRebalancingNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	_, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "cluster-uuid")
	if err == nil {
		t.Fatalf("expected error on non-2xx response")
	}
}

func TestHandleRestartCalledHoldsDrainSlotWhileRebalancing(t *testing.T) {
	// Scenario: all socket nodes are online+healthy but cluster is still
	// rebalancing. handleRestartCalled must NOT mark phase complete — it should
	// requeue and keep the message about rebalancing.
	const nodeName = "node-rebal"
	const nodeUUID = "uuid-rebal"

	// Backend: node is online and healthy; cluster is rebalancing.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		callCount++
		// Node info endpoint returns online+healthy.
		// Cluster info endpoint returns rebalancing=true.
		if r.URL.Path == "/api/v2/clusters/cluster-uuid" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"is_re_balancing":true}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"online","health_check":true}`))
		}
	}))
	defer srv.Close()

	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-rebal", Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: nodeName, UUID: nodeUUID, Status: "online"},
			},
		},
	}
	state := &simplyblockv1alpha1.NodeDrainState{
		Hostname:       nodeName,
		Phase:          simplyblockv1alpha1.DrainPhaseRestartCalled,
		ActiveNodeUUID: nodeUUID,
	}

	r := newNodeDrainTestReconciler(t, snCR, k8sNode)
	requeue, err := r.handleRestartCalled(context.Background(), snCR, state, webapi.NewClient(srv.URL), "cluster-uuid")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requeue == 0 {
		t.Fatalf("expected non-zero requeue while cluster is rebalancing")
	}
	if state.Phase == simplyblockv1alpha1.DrainPhaseComplete {
		t.Fatalf("drain phase must NOT be complete while cluster is rebalancing")
	}
	if state.Message == "" {
		t.Fatalf("expected a status message about rebalancing")
	}
}

func TestHandleRestartCalledCompletesWhenNotRebalancing(t *testing.T) {
	// Scenario: all socket nodes are online+healthy and cluster is NOT
	// rebalancing. handleRestartCalled should mark phase complete.
	const nodeName = "node-done"
	const nodeUUID = "uuid-done"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/api/v2/clusters/cluster-uuid" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"is_re_balancing":false}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"online","health_check":true}`))
		}
	}))
	defer srv.Close()

	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-done", Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: nodeName, UUID: nodeUUID, Status: "online"},
			},
		},
	}
	state := &simplyblockv1alpha1.NodeDrainState{
		Hostname:       nodeName,
		Phase:          simplyblockv1alpha1.DrainPhaseRestartCalled,
		ActiveNodeUUID: nodeUUID,
	}

	r := newNodeDrainTestReconciler(t, snCR, k8sNode)
	requeue, err := r.handleRestartCalled(context.Background(), snCR, state, webapi.NewClient(srv.URL), "cluster-uuid")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Phase != simplyblockv1alpha1.DrainPhaseComplete {
		t.Fatalf("expected DrainPhaseComplete when node is online+healthy and cluster not rebalancing, got %q", state.Phase)
	}
	if requeue != 0 {
		t.Fatalf("expected zero requeue on completion, got %v", requeue)
	}
}

// ---- 409 conflict retry test ----

// TestNodeDrainStatusPatch409RetryPreservesDrainState verifies that a 409
// Conflict on the final Status().Patch() does NOT discard the drain phase
// transitions computed during the reconcile. Without RetryOnConflict the
// controller would silently revert to the pre-reconcile state.
//
// Setup: one worker already in DrainPhaseComplete (no backend HTTP calls
// needed) so processWorker is a pure no-op. The interesting behaviour is in
// the final patch: the interceptor returns 409 on the first attempt and
// succeeds on the second, verifying that RetryOnConflict re-reads and retries
// rather than logging and returning the 5-second requeue.
func TestNodeDrainStatusPatch409RetryPreservesDrainState(t *testing.T) {
	const (
		ns          = "default"
		clusterName = "cluster-drain-409"
		clusterUUID = "uuid-drain-409"
		workerName  = "worker-409.example.com"
		snsName     = "sn-drain-409"
	)

	clusterCR := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			Status: utils.ClusterStatusActive,
			UUID:   clusterUUID,
		},
	}
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: snsName, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: workerName, Status: utils.NodeStatusOnline, UUID: "backend-uuid-409"},
			},
			// DrainPhaseComplete → processWorker is a pure no-op (no backend calls).
			// The patch carries this state; the interceptor will conflict on the
			// first attempt and succeed on the second.
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{
					Hostname: workerName,
					Phase:    simplyblockv1alpha1.DrainPhaseComplete,
				},
			},
		},
	}

	scheme := newTestScheme(
		t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		policyv1.AddToScheme,
	)

	patchCalls := 0
	conflictErr := apierrors.NewConflict(
		simplyblockv1alpha1.GroupVersion.WithResource("storagenodesets").GroupResource(),
		snsName, nil,
	)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.StorageNodeSet{}, &simplyblockv1alpha1.StorageCluster{}).
		WithObjects(clusterCR, snCR).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subResourceName string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				if subResourceName == "status" {
					patchCalls++
					if patchCalls == 1 {
						return conflictErr
					}
				}
				return c.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &NodeDrainCoordinatorReconciler{Client: cl, Scheme: scheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: snsName, Namespace: ns},
	})

	if err != nil {
		t.Fatalf("expected no error despite initial 409, got: %v", err)
	}
	// RetryOnConflict should succeed — must NOT return the 5-second requeue
	// that the old bare-patch code returned on any error.
	if res.RequeueAfter == 5*time.Second {
		t.Fatal("reconcile returned the 5s conflict requeue — RetryOnConflict did not succeed")
	}
	if patchCalls < 2 {
		t.Fatalf("expected ≥2 Status.Patch calls (conflict + retry), got %d", patchCalls)
	}

	// Drain state must be persisted after the conflict retry.
	var updated simplyblockv1alpha1.StorageNodeSet
	if err := cl.Get(context.Background(), client.ObjectKey{Name: snsName, Namespace: ns}, &updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	state := getDrainState(&updated, workerName)
	if state == nil {
		t.Fatal("drain state was lost after 409 — RetryOnConflict did not preserve the phase")
		return
	}
	if state.Phase != simplyblockv1alpha1.DrainPhaseComplete {
		t.Fatalf("expected DrainPhaseComplete after retry, got %q", state.Phase)
	}
}

// ---- failure-domain gate tests ----

// snCRWithFD builds a minimal StorageNodeSet with two nodes whose failure domains
// are set in status.nodes (populated from the backend API), and node-a already
// in an active drain phase.
func snCRWithFD(nodeADomain, nodeBDomain int32) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-fd", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			WorkerNodes: []string{"node-a", "node-b"},
		},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			// FailureDomain sourced from backend API response, stored in status.
			// No UUIDs so activeDrainWorkers makes no backend API calls.
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", FailureDomain: &nodeADomain},
				{Hostname: "node-b", FailureDomain: &nodeBDomain},
			},
			DrainCoordination: []simplyblockv1alpha1.NodeDrainState{
				{Hostname: "node-a", Phase: simplyblockv1alpha1.DrainPhaseShutdownCalled},
			},
		},
	}
}

// TestHandleDetectedFDDisabledUsesGlobalGate verifies that when fdEnabled=false
// the existing node-count gate blocks a second drain when activeDrains >= maxFaultTolerance.
func TestHandleDetectedFDDisabledUsesGlobalGate(t *testing.T) {
	snCR := snCRWithFD(1, 2)
	state := &simplyblockv1alpha1.NodeDrainState{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDetected}
	r := newNodeDrainTestReconciler(t, snCR)

	requeue, err := r.handleDetected(
		context.Background(), snCR, state,
		webapi.NewClient("http://127.0.0.1:1"), "cluster",
		1,     // maxFaultTolerance=1 → node-a already consumes the slot
		false, // fdEnabled=false → global gate
		0,     // domainsNeededForFullDisjoint unused when fdEnabled=false
		0,     // npcs unused when fdEnabled=false
	)

	if err != nil {
		t.Fatalf("expected nil error when blocked, got %v", err)
	}
	if requeue != 10*time.Second {
		t.Fatalf("expected 10s requeue when blocked by global gate, got %v", requeue)
	}
	if state.Phase == simplyblockv1alpha1.DrainPhaseShutdownCalled {
		t.Fatalf("phase must not advance while drain slot is unavailable")
	}
}

// TestHandleDetectedSameDomainParallelAllowed verifies that when fdEnabled=true,
// a worker in the same failure domain as an already-draining worker is allowed
// past the gate without waiting.
func TestHandleDetectedSameDomainParallelAllowed(t *testing.T) {
	// Both node-a and node-b are in failure domain 1.
	snCR := snCRWithFD(1, 1)
	state := &simplyblockv1alpha1.NodeDrainState{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDetected}
	r := newNodeDrainTestReconciler(t, snCR)

	requeue, err := r.handleDetected(
		context.Background(), snCR, state,
		webapi.NewClient("http://127.0.0.1:1"), "cluster",
		1,    // maxFaultTolerance=1 — unused inside the fdEnabled branch
		true, // fdEnabled=true
		1,    // domainsNeededForFullDisjoint=1, domainsAvailable=1 -> chunksPerDomain=1,
		// so node-a (already active in domain 1) has already maxed the domain's
		// contribution -- node-b proceeds unconditionally regardless of npcs.
		1, // npcs -- irrelevant here since the maxed-domain shortcut fires first
	)

	// The gate passes; the function proceeds until it finds no UUID for node-b
	// and returns a 15s requeue with a non-nil error (UUID missing). That proves
	// it was NOT held back at the drain-slot check.
	blocked := err == nil && requeue == 10*time.Second
	if blocked {
		t.Fatalf("node in the same failure domain must not be blocked; state.Message=%q", state.Message)
	}
}

// TestHandleDetectedCrossDomainGated verifies that when fdEnabled=true, a worker
// in a different failure domain from the already-draining worker is blocked when
// the active domain count meets maxFaultTolerance.
func TestHandleDetectedCrossDomainGated(t *testing.T) {
	// node-a is in domain 1 (already draining); node-b is in domain 2.
	snCR := snCRWithFD(1, 2)
	state := &simplyblockv1alpha1.NodeDrainState{Hostname: "node-b", Phase: simplyblockv1alpha1.DrainPhaseDetected}
	r := newNodeDrainTestReconciler(t, snCR)

	requeue, err := r.handleDetected(
		context.Background(), snCR, state,
		webapi.NewClient("http://127.0.0.1:1"), "cluster",
		1,    // maxFaultTolerance=1 — unused inside the fdEnabled branch
		true, // fdEnabled=true
		2,    // domainsNeededForFullDisjoint=2, domainsAvailable=2 -> chunksPerDomain=1 (well-provisioned)
		1,    // npcs=1: currentRisk(1)+1=2 > npcs(1) -> correctly blocked
	)

	if err != nil {
		t.Fatalf("expected nil error when blocked by domain gate, got %v", err)
	}
	if requeue != 10*time.Second {
		t.Fatalf("expected 10s requeue when cross-domain is gated, got %v", requeue)
	}
	if state.Phase == simplyblockv1alpha1.DrainPhaseShutdownCalled {
		t.Fatalf("phase must not advance when cross-domain drain slot is unavailable")
	}
}

// TestWorkerFailureDomainFromStatus verifies that the failure domain is read
// from status.nodes[].failureDomain (populated from the backend API response).
func TestWorkerFailureDomainFromStatus(t *testing.T) {
	fd := int32(5)
	snCR := &simplyblockv1alpha1.StorageNodeSet{
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "worker-a", FailureDomain: &fd},
			},
		},
	}

	got, ok := workerFailureDomain(snCR, "worker-a")
	if !ok {
		t.Fatal("expected domain to be found in status")
	}
	if got != 5 {
		t.Fatalf("expected 5 from status.nodes, got %d", got)
	}
}

// TestWorkerFailureDomainUnassigned verifies that a worker with no domain
// assignment returns (0, false).
func TestWorkerFailureDomainUnassigned(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNodeSet{}
	_, ok := workerFailureDomain(snCR, "worker-missing")
	if ok {
		t.Fatal("expected (0, false) for unassigned worker")
	}
}

// ---- helper ----

func newNodeDrainTestReconciler(t *testing.T, objects ...client.Object) *NodeDrainCoordinatorReconciler {
	t.Helper()

	scheme := newTestScheme(
		t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
		policyv1.AddToScheme,
	)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
	}, objects...)

	return &NodeDrainCoordinatorReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
