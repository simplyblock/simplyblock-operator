package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---- pure helpers ----

func TestGetDrainState(t *testing.T) {
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	snCR := &simplyblockv1alpha1.StorageNode{}

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
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
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

	got := countActiveDrains(context.Background(), snCR, webapi.NewClient("http://127.0.0.1:1"), "cluster", "secret")
	if got != 3 {
		t.Fatalf("expected 3 active drains (shutdown_called, draining, restart_called), got %d", got)
	}
}

func TestCountActiveDrainsBackendConservative(t *testing.T) {
	// Backend API unreachable → node is counted as active (conservative).
	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
			},
		},
	}

	// Use an unreachable address to force backend error.
	got := countActiveDrains(context.Background(), snCR, webapi.NewClient("http://127.0.0.1:1"), "cluster", "secret")
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

	snCR := &simplyblockv1alpha1.StorageNode{
		Status: simplyblockv1alpha1.StorageNodeStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-a", UUID: "uuid-a"},
				{Hostname: "node-b", UUID: "uuid-b"},
			},
		},
	}

	got := countActiveDrains(context.Background(), snCR, webapi.NewClient(srv.URL), "cluster", "secret")
	if got != 2 {
		t.Fatalf("expected backend count of 2, got %d", got)
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-no-auth", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
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
	const clusterUUID = "cluster-uuid-no-cr"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-no-cluster-cr", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: clusterName,
		},
	}
	r := newNodeDrainTestReconciler(t, snCR, secret)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snCR)})
	if err != nil {
		t.Fatalf("expected no error when cluster CR unavailable, got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster CR is missing")
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
	snCR := &simplyblockv1alpha1.StorageNode{
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{WorkerNodes: []string{"node-g"}},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	requeue, shouldBreak := r.processWorker(
		context.Background(), snCR, "node-g",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", "secret", 1,
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-h", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{WorkerNodes: []string{"node-h"}},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	requeue, shouldBreak := r.processWorker(
		context.Background(), snCR, "node-h",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", "secret", 1,
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-i", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{WorkerNodes: []string{"node-i"}},
		Status: simplyblockv1alpha1.StorageNodeStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "node-i", Status: "online", UUID: ""},
			},
		},
	}
	r := newNodeDrainTestReconciler(t, snCR, node)

	r.processWorker(
		context.Background(), snCR, "node-i",
		webapi.NewClient("http://127.0.0.1:1"), "cluster", "secret", 1,
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

	rebalancing, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "secret", "cluster-uuid")
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

	rebalancing, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "secret", "cluster-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rebalancing {
		t.Fatalf("expected rebalancing=false, got true")
	}
}

func TestIsClusterRebalancingAPIError(t *testing.T) {
	// Unreachable address → error expected.
	_, err := isClusterRebalancing(context.Background(), webapi.NewClient("http://127.0.0.1:1"), "secret", "cluster-uuid")
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

	_, err := isClusterRebalancing(context.Background(), webapi.NewClient(srv.URL), "secret", "cluster-uuid")
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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-rebal", Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	requeue, err := r.handleRestartCalled(context.Background(), snCR, state, webapi.NewClient(srv.URL), "cluster-uuid", "secret")

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
	snCR := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-done", Namespace: "default"},
		Status: simplyblockv1alpha1.StorageNodeStatus{
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
	requeue, err := r.handleRestartCalled(context.Background(), snCR, state, webapi.NewClient(srv.URL), "cluster-uuid", "secret")

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
		&simplyblockv1alpha1.StorageNode{},
		&simplyblockv1alpha1.StorageCluster{},
	}, objects...)

	return &NodeDrainCoordinatorReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
