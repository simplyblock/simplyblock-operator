/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const migrateTargetWorker = "worker-2.example.com"

// migrateTestServer counts the /restart and /promote POSTs the migrate flow makes
// and serves GET node-status (always "online"). It records the last /restart body
// so tests can assert node_address / new_ssd_pcie were sent.
type migrateTestServer struct {
	restartPosts int
	promotePosts int
	lastRestart  map[string]any
}

func newMigrateTestServer(t *testing.T) (*httptest.Server, *migrateTestServer) {
	t.Helper()
	state := &migrateTestServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restart"):
			state.restartPosts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			state.lastRestart = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/promote"):
			state.promotePosts++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default: // GET node status
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"online"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

// migrateFixture builds the objects a migrate op needs: origin StorageNode (with
// UUID + socket metadata), its StorageNodeSet, the target k8s Node, and the ops CR
// pre-seeded as if acquireLock has run (Running / Preparing / StartedAt).
func migrateFixture(t *testing.T) *StorageNodeOpsReconciler {
	t.Helper()

	socketIdx := int32(0)
	nodeIdx := int32(0)
	origin := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-origin", Namespace: opsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: "sns",
			WorkerNode:        opsTestWorker,
			SocketID:          "0",
			NodeIndex:         &nodeIdx,
			SocketIndex:       &socketIdx,
		},
	}
	origin.Status.UUID = opsTestNodeUUID
	origin.Status.ActiveOpsRef = opsTestOpsName

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: opsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: opsTestCluster,
			WorkerNodes: []string{opsTestWorker},
		},
	}

	targetNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: migrateTargetWorker}}

	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-origin", "migrate")
	ops.Spec.TargetWorkerNode = migrateTargetWorker
	ops.Spec.NewSsdPcie = []string{"0000:81:00.0"}
	now := metav1.Now()
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhasePreparing
	ops.Status.StartedAt = &now

	r := newOpsReconciler(t, origin, sns, targetNode, ops)
	return r
}

// driveMigrate runs runMigrate to a terminal phase (or the step budget), re-reading
// ops/origin/sns each step like the real loop. It fails the test if any non-terminal
// step blocks (RequeueAfter == 0 and Requeue == false).
func driveMigrate(t *testing.T, r *StorageNodeOpsReconciler, apiClient *webapi.Client) *simplyblockv1alpha1.StorageNodeOps {
	t.Helper()
	ctx := context.Background()
	const budget = 40
	terminal := func(p simplyblockv1alpha1.StorageNodeOpsPhase) bool {
		return p == simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded ||
			p == simplyblockv1alpha1.StorageNodeOpsPhaseFailed
	}
	for i := 0; i < budget; i++ {
		var ops simplyblockv1alpha1.StorageNodeOps
		if err := r.Get(ctx, types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &ops); err != nil {
			t.Fatalf("get ops: %v", err)
		}
		if terminal(ops.Status.Phase) {
			return &ops
		}
		var origin simplyblockv1alpha1.StorageNode
		if err := r.Get(ctx, types.NamespacedName{Name: "sn-origin", Namespace: opsTestNS}, &origin); err != nil {
			t.Fatalf("get origin: %v", err)
		}
		var sns simplyblockv1alpha1.StorageNodeSet
		if err := r.Get(ctx, types.NamespacedName{Name: "sns", Namespace: opsTestNS}, &sns); err != nil {
			t.Fatalf("get sns: %v", err)
		}

		subPhase := ops.Status.SubPhase
		res, err := r.runMigrate(ctx, &ops, &origin, &sns, "cluster-uuid", apiClient)
		if err != nil {
			t.Fatalf("runMigrate step %d error: %v", i, err)
		}

		// Re-read: a step that reached a terminal phase (succeed/fail) legitimately
		// returns an empty Result; only NON-terminal steps must free the loop.
		var after simplyblockv1alpha1.StorageNodeOps
		if err := r.Get(ctx, types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &after); err != nil {
			t.Fatalf("get ops after step: %v", err)
		}
		if terminal(after.Status.Phase) {
			return &after
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("runMigrate step %d blocked (no requeue) at subphase %q", i, subPhase)
		}
	}
	t.Fatalf("migrate did not terminate within %d steps", budget)
	return nil
}

func TestRunMigrate_EndToEnd(t *testing.T) {
	r := migrateFixture(t)
	srv, calls := newMigrateTestServer(t)
	apiClient := webapi.NewClient(srv.URL)

	// Reachability is stubbed (no live storage-node-api in unit tests).
	prev := checkNodeInfoReachableFn
	checkNodeInfoReachableFn = func(context.Context, string, string, bool, bool) error { return nil }
	t.Cleanup(func() { checkNodeInfoReachableFn = prev })

	ops := driveMigrate(t, r, apiClient)

	if ops.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Fatalf("phase = %q want Succeeded (message: %q)", ops.Status.Phase, ops.Status.Message)
	}
	if calls.restartPosts != 1 {
		t.Errorf("restart POSTs = %d want 1", calls.restartPosts)
	}
	if calls.promotePosts != 1 {
		t.Errorf("promote POSTs = %d want 1", calls.promotePosts)
	}
	// The restart must have been directed at the target host and carried new_ssd_pcie.
	if calls.lastRestart["node_address"] == nil || calls.lastRestart["node_address"] == "" {
		t.Errorf("restart body missing node_address: %#v", calls.lastRestart)
	}
	if _, ok := calls.lastRestart["new_ssd_pcie"]; !ok {
		t.Errorf("restart body missing new_ssd_pcie: %#v", calls.lastRestart)
	}

	ctx := context.Background()

	// Target CR created, adopting the origin UUID, with migration-pending cleared.
	targetName := storageNodeCRName("sns", migrateTargetWorker, "0", 0)
	var target simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: opsTestNS}, &target); err != nil {
		t.Fatalf("target CR not found: %v", err)
	}
	if target.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID] != opsTestNodeUUID {
		t.Errorf("target adopt-uuid = %q want %q", target.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID], opsTestNodeUUID)
	}
	if _, pending := target.Annotations[simplyblockv1alpha1.AnnotationMigrationPending]; pending {
		t.Error("migration-pending should be cleared on the target CR after rebind")
	}
	if len(target.OwnerReferences) != 0 {
		t.Errorf("target CR must have no ownerRef (manual, GC-safe); got %d", len(target.OwnerReferences))
	}

	// Origin CR annotated migrated-away.
	var origin simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{Name: "sn-origin", Namespace: opsTestNS}, &origin); err != nil {
		t.Fatalf("origin CR not found: %v", err)
	}
	if _, migrated := origin.Annotations[simplyblockv1alpha1.AnnotationMigratedAway]; !migrated {
		t.Error("origin CR should be annotated migrated-away")
	}

	// workerNodes swapped: origin removed, target added.
	var sns simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: "sns", Namespace: opsTestNS}, &sns); err != nil {
		t.Fatalf("sns not found: %v", err)
	}
	if contains(sns.Spec.WorkerNodes, opsTestWorker) {
		t.Errorf("origin worker %q should be removed from workerNodes: %v", opsTestWorker, sns.Spec.WorkerNodes)
	}
	if !contains(sns.Spec.WorkerNodes, migrateTargetWorker) {
		t.Errorf("target worker %q should be added to workerNodes: %v", migrateTargetWorker, sns.Spec.WorkerNodes)
	}
}

func TestRunMigrate_FailsWhenTargetNodeMissing(t *testing.T) {
	r := migrateFixture(t)
	srv, _ := newMigrateTestServer(t)
	apiClient := webapi.NewClient(srv.URL)

	// Delete the target k8s Node so validation fails.
	var node corev1.Node
	_ = r.Get(context.Background(), types.NamespacedName{Name: migrateTargetWorker}, &node)
	if err := r.Delete(context.Background(), &node); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	prev := checkNodeInfoReachableFn
	checkNodeInfoReachableFn = func(context.Context, string, string, bool, bool) error { return nil }
	t.Cleanup(func() { checkNodeInfoReachableFn = prev })

	ops := driveMigrate(t, r, apiClient)
	if ops.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Fatalf("phase = %q want Failed for missing target node", ops.Status.Phase)
	}
}

func TestBuildMigrationTargetCR(t *testing.T) {
	nodeIdx := int32(0)
	socketIdx := int32(0)
	origin := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-origin", Namespace: opsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: "sns",
			WorkerNode:        opsTestWorker,
			SocketID:          "0",
			NodeIndex:         &nodeIdx,
			SocketIndex:       &socketIdx,
		},
	}
	origin.Status.UUID = opsTestNodeUUID
	sns := &simplyblockv1alpha1.StorageNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: opsTestNS}}

	name := storageNodeCRName("sns", migrateTargetWorker, "0", 0)
	cr := buildMigrationTargetCR(sns, origin, migrateTargetWorker, name)

	if cr.Spec.WorkerNode != migrateTargetWorker {
		t.Errorf("workerNode = %q want %q", cr.Spec.WorkerNode, migrateTargetWorker)
	}
	if cr.Spec.StorageNodeSetRef != "sns" {
		t.Errorf("storageNodeSetRef = %q want sns", cr.Spec.StorageNodeSetRef)
	}
	if cr.Spec.SocketID != "0" {
		t.Errorf("socketID = %q want 0", cr.Spec.SocketID)
	}
	if cr.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID] != opsTestNodeUUID {
		t.Errorf("adopt-uuid = %q want %q", cr.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID], opsTestNodeUUID)
	}
	if _, ok := cr.Annotations[simplyblockv1alpha1.AnnotationMigrationPending]; !ok {
		t.Error("migration-pending should be set")
	}
	if len(cr.OwnerReferences) != 0 {
		t.Errorf("target CR must have no ownerRef; got %d", len(cr.OwnerReferences))
	}
}

func TestMigrateSwapWorkerList(t *testing.T) {
	cases := []struct {
		name        string
		workers     []string
		origin      string
		target      string
		wantWorkers []string
		wantChanged bool
	}{
		{"swap", []string{"a", "b"}, "a", "c", []string{"b", "c"}, true},
		{"origin-absent-target-added", []string{"b"}, "a", "c", []string{"b", "c"}, true},
		{"target-already-present", []string{"a", "c"}, "a", "c", []string{"c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sns := &simplyblockv1alpha1.StorageNodeSet{
				Spec: simplyblockv1alpha1.StorageNodeSetSpec{WorkerNodes: append([]string{}, tc.workers...)},
			}
			changed := migrateSwapWorkerList(sns, tc.origin, tc.target)
			if changed != tc.wantChanged {
				t.Errorf("changed = %v want %v", changed, tc.wantChanged)
			}
			if len(sns.Spec.WorkerNodes) != len(tc.wantWorkers) {
				t.Fatalf("workerNodes = %v want %v", sns.Spec.WorkerNodes, tc.wantWorkers)
			}
			for _, w := range tc.wantWorkers {
				if !contains(sns.Spec.WorkerNodes, w) {
					t.Errorf("workerNodes %v missing %q", sns.Spec.WorkerNodes, w)
				}
			}
			if contains(sns.Spec.WorkerNodes, tc.origin) && tc.origin != tc.target {
				t.Errorf("origin %q should have been removed: %v", tc.origin, sns.Spec.WorkerNodes)
			}
		})
	}
}

func TestMigrateSwapWorkerList_MovesNodeConfig(t *testing.T) {
	cores := int32(50)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			WorkerNodes: []string{"a"},
			NodeConfigs: map[string]simplyblockv1alpha1.StorageNodeOverrides{
				"a": {CorePercentage: &cores},
			},
		},
	}
	migrateSwapWorkerList(sns, "a", "c")
	if _, ok := sns.Spec.NodeConfigs["a"]; ok {
		t.Error("nodeConfigs[a] should have been removed")
	}
	cfg, ok := sns.Spec.NodeConfigs["c"]
	if !ok || cfg.CorePercentage == nil || *cfg.CorePercentage != 50 {
		t.Errorf("nodeConfigs[c] not moved correctly: %#v", sns.Spec.NodeConfigs)
	}
}
