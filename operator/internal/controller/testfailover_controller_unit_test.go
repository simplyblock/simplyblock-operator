package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/simplyblock/atlas/statemachine"
	workv1 "open-cluster-management.io/api/work/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The handles the drill fixtures resolve to at each step. The source lives on
// clusterA and the recovery bubble on clusterB, so bubble != source in the sample.
const (
	testSourceHandle   = "clusterA:poolA:lvolX"
	testSnapshotHandle = "clusterB:poolB:snapY"
	testCloneHandle    = "clusterB:poolB:cloneVol"
	testFSTypeXFS      = "xfs"
	testFabricTCP      = "tcp"
	testSourceCluster  = "ramen-cluster-a"
)

// atResolvingPoint returns a drill seeded at ResolvingPoint with its source
// already resolved to testSourceHandle.
func atResolvingPoint() *simplyblockv1alpha2.TestFailover {
	tf := atResolvingSource()
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingPoint)}
	tf.Status.Clones = []simplyblockv1alpha2.TestFailoverClone{{
		SourceRef:    tf.Spec.SourceRef,
		SourceHandle: testSourceHandle,
	}}
	return tf
}

func newTestFailoverReconciler(t *testing.T, objects ...client.Object) (*TestFailoverReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	// The ManagedClusterView is driven unstructured, so the fake client needs its
	// GVK (and list GVK) registered to create and read it.
	scheme.AddKnownTypeWithName(managedClusterViewGVK, &unstructured.Unstructured{})
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	if err := workv1.Install(scheme); err != nil {
		t.Fatalf("register work/v1 scheme: %v", err)
	}
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.TestFailover{}, &workv1.ManifestWork{}},
		objects...)
	return &TestFailoverReconciler{Client: cl, Scheme: scheme, Recorder: &fakeRecorder{}}, cl
}

// getView reads a ManagedClusterView the controller created.
func getView(t *testing.T, cl client.Client, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	v := &unstructured.Unstructured{}
	v.SetGroupVersionKind(managedClusterViewGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, v); err != nil {
		t.Fatalf("get ManagedClusterView %s/%s: %v", namespace, name, err)
	}
	return v
}

// setViewResult simulates the OCM view controller having projected an object,
// by writing status.result onto the view.
func setViewResult(t *testing.T, cl client.Client, v *unstructured.Unstructured, result map[string]interface{}) {
	t.Helper()
	if err := unstructured.SetNestedMap(v.Object, result, "status", "result"); err != nil {
		t.Fatalf("set status.result: %v", err)
	}
	if err := cl.Update(context.Background(), v); err != nil {
		t.Fatalf("update view with result: %v", err)
	}
}

// atResolvingSource returns a drill seeded at the ResolvingSource step, the state
// the entry transition leaves it in.
func atResolvingSource() *simplyblockv1alpha2.TestFailover {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingSource)}
	return tf
}

func sampleTestFailover() *simplyblockv1alpha2.TestFailover {
	return &simplyblockv1alpha2.TestFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "drill-1", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.TestFailoverSpec{
			Scope:           simplyblockv1alpha2.TestFailoverScopeVolume,
			SourceCluster:   testSourceCluster,
			SourceNamespace: "prod-app",
			SourceRef:       "postgres-data",
			BubbleCluster:   "ramen-cluster-b",
		},
	}
}

func testFailoverRequest(tf *simplyblockv1alpha2.TestFailover) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: tf.Name, Namespace: tf.Namespace}}
}

// TestFailoverAddsFinalizerThenEntersResolvingSource covers the entry into the
// drill: the first reconcile adds the teardown finalizer, and the next begins
// the drill at the initial step with the phase and bookkeeping set.
func TestFailoverAddsFinalizerThenEntersResolvingSource(t *testing.T) {
	tf := sampleTestFailover()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	// First pass: the finalizer is added and nothing else has happened yet.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, finalizerTestFailover) {
		t.Fatalf("finalizer %q was not added", finalizerTestFailover)
	}
	if got.Status.Phase != "" {
		t.Errorf("phase = %q, want empty before the drill begins", got.Status.Phase)
	}

	// Second pass: the drill begins at ResolvingSource.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatalf("get after second reconcile: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseProvisioning {
		t.Errorf("phase = %q, want %q", got.Status.Phase, simplyblockv1alpha2.TestFailoverPhaseProvisioning)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepResolvingSource) {
		t.Errorf("step = %q, want %q", got.Status.Step.State, simplyblockv1alpha2.TestFailoverStepResolvingSource)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	if got.Status.StartedAt == nil {
		t.Errorf("startedAt was not set")
	}
}

// TestFailoverDeletionClearsTheFinalizer covers that a deleted drill is torn
// down and does not hang on its finalizer.
func TestFailoverDeletionClearsTheFinalizer(t *testing.T) {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	now := metav1.Now()
	tf.DeletionTimestamp = &now
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile of a deleting drill: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got)
	if err == nil && controllerutil.ContainsFinalizer(&got, finalizerTestFailover) {
		t.Fatalf("finalizer still present after deletion reconcile")
	}
}

// TestFailoverResolvingSourceResolvesHandleThenAdvances covers the full source
// read: the controller creates a ManagedClusterView for the PVC, then for its
// PV, and once both are projected it records the volume handle and advances to
// ResolvingPoint. Each projection arrives across reconciles, never blocking.
func TestFailoverResolvingSourceResolvesHandleThenAdvances(t *testing.T) {
	tf := atResolvingSource()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	// Pass 1: the PVC view is created and the drill holds on its projection.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	pvcView := getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pvc"))
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepResolvingSource) {
		t.Fatalf("step advanced before the source was projected: %q", got.Status.Step.State)
	}
	setViewResult(t, cl, pvcView, map[string]interface{}{
		"spec": map[string]interface{}{"volumeName": "pv-1"},
	})

	// Pass 2: the PVC result is read, and the PV view is created.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	pvView := getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pv"))
	setViewResult(t, cl, pvView, map[string]interface{}{
		"spec": map[string]interface{}{
			"csi": map[string]interface{}{"volumeHandle": "clusterA:pool:lvolX"},
		},
	})

	// Pass 3: the handle is read and the drill advances to ResolvingPoint.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepResolvingPoint) {
		t.Errorf("step = %q, want %q", got.Status.Step.State, simplyblockv1alpha2.TestFailoverStepResolvingPoint)
	}
	if len(got.Status.Clones) != 1 || got.Status.Clones[0].SourceHandle != "clusterA:pool:lvolX" {
		t.Errorf("clones = %+v, want one with sourceHandle clusterA:pool:lvolX", got.Status.Clones)
	}
}

// Regression: 2026-09-29-testfailover-nil-volumecontext — the bubble PV must
// carry a VolumeContext or the node plugin panics staging it. The source PV's
// volumeAttributes are captured here, minus the identity and provisioner keys:
// the class params are needed to stage, but the identity keys would point a
// failed clone lookup back at the source, so they are dropped.
func TestFailoverResolvingSourceCapturesStrippedVolumeContext(t *testing.T) {
	tf := atResolvingSource()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	setViewResult(t, cl, getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pvc")),
		map[string]interface{}{"spec": map[string]interface{}{"volumeName": "pv-1"}})

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	setViewResult(t, cl, getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pv")),
		map[string]interface{}{"spec": map[string]interface{}{"csi": map[string]interface{}{
			"volumeHandle": "clusterA:pool:lvolX",
			"fsType":       testFSTypeXFS,
			"volumeAttributes": map[string]interface{}{
				// class params — kept
				"tune2fs_reserved_blocks": "",
				"fabric":                  testFabricTCP,
				"qos_rw_iops":             "0",
				// identity — stripped (would mis-point a failed clone lookup)
				"cluster_id":  "clusterA",
				"pool_name":   "poolA",
				"nqn":         "nqn.source",
				"connections": "[{\"ip\":\"10.0.0.1\",\"port\":4420}]",
				"uuid":        "lvolX",
				"nsId":        "1",
				"model":       "lvolX",
				// provisioner-injected — stripped (stale source metadata)
				"csi.storage.k8s.io/pv/name":                   "pv-1",
				"storage.kubernetes.io/csiProvisionerIdentity": "x",
			},
		}}})

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Clones) != 1 {
		t.Fatalf("clones = %+v, want one", got.Status.Clones)
	}
	if got.Status.Clones[0].SourceFSType != testFSTypeXFS {
		t.Errorf("sourceFSType = %q, want the source PV's xfs", got.Status.Clones[0].SourceFSType)
	}
	vc := got.Status.Clones[0].SourceVolumeContext
	for _, k := range []string{"tune2fs_reserved_blocks", "fabric", "qos_rw_iops"} {
		if _, ok := vc[k]; !ok {
			t.Errorf("class param %q was dropped from the bubble VolumeContext: %+v", k, vc)
		}
	}
	for _, k := range []string{
		"cluster_id", "pool_name", "nqn", "connections", "uuid", "nsId", "model",
		"csi.storage.k8s.io/pv/name", "storage.kubernetes.io/csiProvisionerIdentity",
	} {
		if _, ok := vc[k]; ok {
			t.Errorf("identity/provisioner key %q leaked into the bubble VolumeContext: %+v", k, vc)
		}
	}
}

// TestFailoverResolvingSourceGroupResolvesMembers covers the group source path:
// the members come from the source cluster's backend (each carries only an lvol
// id, resolved to its PVC), one representative PV supplies the shared class
// metadata, and the drill advances to ResolvingPoint with one clone slot per
// member.
func TestFailoverResolvingSourceGroupResolvesMembers(t *testing.T) {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	tf.Spec.Scope = simplyblockv1alpha2.TestFailoverScopeGroup
	tf.Spec.SourceRef = "cg"
	tf.Spec.SourceCluster = testSourceCluster // not a UUID: exercises the sole-StorageCluster fallback
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingSource)}

	sc := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "local-sc", Namespace: tf.Namespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: "C"},
	}
	r, cl := newTestFailoverReconciler(t, tf, sc)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/consistency-groups/") && req.URL.Query().Get("name") == "cg":
			_, _ = w.Write([]byte(`[{"id":"g1","name":"cg","lvs_name":"lvs-a","node_id":"node-a"}]`))
		case strings.HasSuffix(p, "/consistency-groups/g1/members"):
			_, _ = w.Write([]byte(`[{"lvol_id":"lvol-a"},{"lvol_id":"lvol-b"}]`))
		case strings.HasSuffix(p, "/storage-pools/"):
			_, _ = w.Write([]byte(`[{"id":"pool-1"}]`))
		case strings.HasSuffix(p, "/storage-pools/pool-1/volumes"):
			_, _ = w.Write([]byte(`[{"id":"lvol-a","pvc_name":"app/data-1","namespace":"nvme-ns","pool_id":null,"size":1073741824},` +
				`{"id":"lvol-b","pvc_name":"app/data-2","namespace":"nvme-ns","pool_id":null,"size":1073741824}]`))
		default:
			t.Errorf("unexpected request %s %s", req.Method, p)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	// Pass 1: backend resolution done, the representative PVC view is created.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	setViewResult(t, cl, getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pvc")),
		map[string]interface{}{"spec": map[string]interface{}{"volumeName": "pv-1"}})

	// Pass 2: the representative PV view is created.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	setViewResult(t, cl, getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pv")),
		map[string]interface{}{"spec": map[string]interface{}{"csi": map[string]interface{}{
			"volumeHandle": "C:pool-1:lvol-a",
			"fsType":       testFSTypeXFS,
			"volumeAttributes": map[string]interface{}{
				"fabric": testFabricTCP,
				"nqn":    "nqn.source", // identity: must be stripped
			},
		}}})

	// Pass 3: the clones are built and the drill advances.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepResolvingPoint) {
		t.Fatalf("step = %q, want ResolvingPoint", got.Status.Step.State)
	}
	if len(got.Status.Clones) != 2 {
		t.Fatalf("clones = %+v, want one per member (2)", got.Status.Clones)
	}
	want := map[string]string{"data-1": "C:pool-1:lvol-a", "data-2": "C:pool-1:lvol-b"}
	for _, c := range got.Status.Clones {
		if want[c.SourceRef] != c.SourceHandle {
			t.Errorf("clone %q handle = %q, want %q", c.SourceRef, c.SourceHandle, want[c.SourceRef])
		}
		if c.SourceFSType != testFSTypeXFS {
			t.Errorf("clone %q fsType = %q, want xfs", c.SourceRef, c.SourceFSType)
		}
		if c.SourceVolumeContext["fabric"] != testFabricTCP {
			t.Errorf("clone %q did not carry the shared class attrs: %+v", c.SourceRef, c.SourceVolumeContext)
		}
		if _, leaked := c.SourceVolumeContext["nqn"]; leaked {
			t.Errorf("clone %q leaked the identity key nqn", c.SourceRef)
		}
		if c.SizeBytes != 1073741824 {
			t.Errorf("clone %q size = %d, want 1Gi", c.SourceRef, c.SizeBytes)
		}
	}
}

// TestFailoverResolvingSourceHoldsWithoutAProjection covers that a pending view
// holds the drill on its step rather than advancing or failing.
func TestFailoverResolvingSourceHoldsWithoutAProjection(t *testing.T) {
	tf := atResolvingSource()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, testFailoverRequest(tf))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected a requeue while waiting for the projection")
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("drill failed while merely waiting for a projection")
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepResolvingSource) {
		t.Errorf("step moved off ResolvingSource while waiting: %q", got.Status.Step.State)
	}
}

// TestFailoverResolvingSourceReuseViewOnRestart covers restart safety: a second
// reconcile before the projection arrives finds the existing view rather than
// creating a duplicate.
func TestFailoverResolvingSourceReuseViewOnRestart(t *testing.T) {
	tf := atResolvingSource()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2 (restart): %v", err)
	}

	list := &unstructured.UnstructuredList{}
	gvk := managedClusterViewGVK
	gvk.Kind += listKindSuffix
	list.SetGroupVersionKind(gvk)
	if err := cl.List(ctx, list, client.InNamespace(tf.Spec.SourceCluster)); err != nil {
		t.Fatalf("list views: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d ManagedClusterViews, want exactly 1 (no duplicate on restart)", len(list.Items))
	}
}

// TestFailoverResolvingSourceUnboundPVCFails covers that a source PVC bound to no
// volume is a terminal failure, not an endless hold.
func TestFailoverResolvingSourceUnboundPVCFails(t *testing.T) {
	tf := atResolvingSource()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	pvcView := getView(t, cl, tf.Spec.SourceCluster, testFailoverViewName(tf, "src-pvc"))
	setViewResult(t, cl, pvcView, map[string]interface{}{
		"spec": map[string]interface{}{}, // no volumeName: unbound
	})
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("phase = %q, want Failed for an unbound source PVC", got.Status.Phase)
	}
}

// TestFailoverResolvingPointDRTargetUsesReplicatedSnapshot covers the DR-target
// path: the recovery point is the latest replicated snapshot already on the
// target backend, read (never taken), and the drill advances to Cloning.
func TestFailoverResolvingPointDRTargetUsesReplicatedSnapshot(t *testing.T) {
	tf := atResolvingPoint()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/relationships/lvolX/latest-snapshot") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"snapshot_id": "snapY", "cluster_id": "clusterB", "pool_id": "poolB",
				"created_at": "2026-09-29T00:00:00Z",
			})
			return
		}
		t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepCloning) {
		t.Errorf("step = %q, want Cloning", got.Status.Step.State)
	}
	if got.Status.Clones[0].SnapshotID != "clusterB:poolB:snapY" {
		t.Errorf("snapshot handle = %q, want clusterB:poolB:snapY", got.Status.Clones[0].SnapshotID)
	}
	if got.Status.Report == nil || got.Status.Report.RecoveryPoint != "snapY" {
		t.Errorf("report.recoveryPoint not set to snapY: %+v", got.Status.Report)
	}
}

// TestFailoverResolvingPointGroupResolvesGeneration covers the group recovery
// point: the drill resolves the group's replication policy, reads its latest
// group-consistent generation, and records one target snapshot handle per clone
// slot before advancing to Cloning.
//
// Regression (2026-09-30): the drill inferred the policy from the policies list
// by matching the group's placement (group_lvs_name/group_node_id) and a
// consistency_group flag. A group attached with attach_group_policy sets
// group.policy_id and leaves both empty, so the heuristic matched nothing and the
// group drill failed at ResolvingPoint with "no consistency-group replication
// policy found." The policy id is read off the group DTO, which now carries it.
func TestFailoverResolvingPointGroupResolvesGeneration(t *testing.T) {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	tf.Spec.Scope = simplyblockv1alpha2.TestFailoverScopeGroup
	tf.Spec.SourceRef = "cg"
	tf.Spec.SourceCluster = testSourceCluster
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingPoint)}
	tf.Status.Clones = []simplyblockv1alpha2.TestFailoverClone{
		{SourceRef: "data-1", SourceHandle: "C:pool-1:lvol-a", SourceFSType: testFSTypeXFS, SizeBytes: 1073741824},
		{SourceRef: "data-2", SourceHandle: "C:pool-1:lvol-b", SourceFSType: testFSTypeXFS, SizeBytes: 1073741824},
	}

	sc := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "local-sc", Namespace: tf.Namespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: "C"},
	}
	r, cl := newTestFailoverReconciler(t, tf, sc)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := req.URL.Path
		switch {
		// The live group-first shape: the group carries its policy_id, and the
		// policy itself exposes no placement and no consistency_group flag, so the
		// drill must read the policy off the group rather than the policies list.
		case strings.HasSuffix(p, "/consistency-groups/") && req.URL.Query().Get("name") == "cg":
			_, _ = w.Write([]byte(`[{"id":"g1","name":"cg","lvs_name":"lvs-a","node_id":"node-a","policy_id":"p1"}]`))
		case strings.HasSuffix(p, "/replication/policies/p1/latest-generation"):
			_, _ = w.Write([]byte(`{"group_seq":7,"members":[` +
				`{"snapshot_id":"s1","cluster_id":"B","pool_id":"pb","lvol_id":"t1","size":1073741824,"group_seq":7},` +
				`{"snapshot_id":"s2","cluster_id":"B","pool_id":"pb","lvol_id":"t2","size":1073741824,"group_seq":7}]}`))
		default:
			t.Errorf("unexpected request %s %s", req.Method, p)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepCloning) {
		t.Fatalf("step = %q, want Cloning", got.Status.Step.State)
	}
	if len(got.Status.Clones) != 2 || got.Status.Clones[0].SnapshotID != "B:pb:s1" || got.Status.Clones[1].SnapshotID != "B:pb:s2" {
		t.Errorf("clone snapshot handles = %+v, want B:pb:s1 and B:pb:s2", got.Status.Clones)
	}
	if got.Status.Report == nil || got.Status.Report.RecoveryPoint != "generation 7" {
		t.Errorf("report.recoveryPoint = %+v, want 'generation 7'", got.Status.Report)
	}
}

// TestFailoverResolvingPointDRTargetNoReplicaFails covers that a target with no
// replicated point yet is a terminal failure, not an endless hold.
func TestFailoverResolvingPointDRTargetNoReplicaFails(t *testing.T) {
	tf := atResolvingPoint()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("phase = %q, want Failed when no replicated point exists", got.Status.Phase)
	}
}

// TestFailoverSameClusterIsRejected covers that a drill whose bubble is the
// source's own cluster fails immediately: test-failover recovers onto a DIFFERENT
// cluster, never in place.
func TestFailoverSameClusterIsRejected(t *testing.T) {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	tf.Spec.BubbleCluster = tf.Spec.SourceCluster
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingSource)}
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Fatalf("phase = %q, want Failed for a same-cluster drill; message=%q", got.Status.Phase, got.Status.Message)
	}
	if !strings.Contains(got.Status.Message, "same cluster") {
		t.Errorf("message = %q, want it to explain same-cluster is unsupported", got.Status.Message)
	}
}

// atCloning returns a drill seeded at Cloning with its recovery point resolved.
func atCloning() *simplyblockv1alpha2.TestFailover {
	tf := atResolvingPoint()
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepCloning)}
	tf.Status.Clones[0].SnapshotID = testSnapshotHandle
	return tf
}

// TestFailoverCloningClonesThenAdvances covers cloning the recovery point into a
// writable volume and advancing to Placing with the clone handle recorded.
func TestFailoverCloningClonesThenAdvances(t *testing.T) {
	tf := atCloning()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/storage-pools/poolB/volumes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/storage-pools/poolB/volumes"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "cloneVol", "size": 1073741824})
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepPlacing) {
		t.Errorf("step = %q, want Placing", got.Status.Step.State)
	}
	if got.Status.Clones[0].CloneID != "clusterB:poolB:cloneVol" {
		t.Errorf("clone handle = %q, want clusterB:poolB:cloneVol", got.Status.Clones[0].CloneID)
	}
	if got.Status.Clones[0].SizeBytes != 1073741824 {
		t.Errorf("clone size = %d, want 1073741824", got.Status.Clones[0].SizeBytes)
	}
}

// TestFailoverCloningReusesExistingClone covers ask-then-act idempotency: an
// existing clone with the drill's name is reused, and no second clone is built.
func TestFailoverCloningReusesExistingClone(t *testing.T) {
	tf := atCloning()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	wantName := testFailoverCloneName(tf)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/storage-pools/poolB/volumes") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "cloneExisting", "name": wantName, "size": 2048},
			})
			return
		}
		if req.Method == http.MethodPost {
			t.Errorf("a second clone was built though one already existed: %s", req.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Clones[0].CloneID != "clusterB:poolB:cloneExisting" {
		t.Errorf("clone handle = %q, want clusterB:poolB:cloneExisting", got.Status.Clones[0].CloneID)
	}
}

// TestFailoverCloningRetriesOnServerError covers that a transient control-plane
// error is retried (error returned, no state advance), not swallowed.
func TestFailoverCloningRetriesOnServerError(t *testing.T) {
	tf := atCloning()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err == nil {
		t.Fatalf("expected an error to trigger a retry on a 5xx")
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepCloning) {
		t.Errorf("step = %q, want it to stay Cloning after a transient error", got.Status.Step.State)
	}
}

// atPlacing returns a drill seeded at Placing with its clone built.
func atPlacing() *simplyblockv1alpha2.TestFailover {
	tf := atCloning()
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepPlacing)}
	tf.Status.Clones[0].CloneID = testCloneHandle
	tf.Status.Clones[0].SizeBytes = 1073741824
	return tf
}

func getManifestWork(t *testing.T, cl client.Client, namespace, name string) *workv1.ManifestWork {
	t.Helper()
	var mw workv1.ManifestWork
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &mw); err != nil {
		t.Fatalf("get ManifestWork %s/%s: %v", namespace, name, err)
	}
	return &mw
}

// markManifestWorkPVCBound simulates the recovery cluster's work-agent reporting
// the bubble PVC bound through the ManifestWork status feedback.
func markManifestWorkPVCBound(t *testing.T, cl client.Client, mw *workv1.ManifestWork) {
	t.Helper()
	bound := string(corev1.ClaimBound)
	mw.Status.ResourceStatus.Manifests = []workv1.ManifestCondition{{
		ResourceMeta: workv1.ManifestResourceMeta{Resource: "persistentvolumeclaims"},
		StatusFeedbacks: workv1.StatusFeedbackResult{Values: []workv1.FeedbackValue{{
			Name:  "phase",
			Value: workv1.FieldValue{Type: workv1.String, String: &bound},
		}}},
	}}
	if err := cl.Status().Update(context.Background(), mw); err != nil {
		t.Fatalf("update ManifestWork status: %v", err)
	}
}

// TestFailoverPlacingDeliversManifestWorkThenReady covers the placement step: a
// ManifestWork carrying the bubble PV and PVC (the namespace has its own work) is delivered to the
// recovery cluster, and the drill reaches Ready once the PVC binds.
func TestFailoverPlacingDeliversManifestWorkThenReady(t *testing.T) {
	tf := atPlacing()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	// Pass 1: the ManifestWork is created and the drill holds on the bind.
	res, err := r.Reconcile(ctx, testFailoverRequest(tf))
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected a requeue while waiting for the bubble PVC to bind")
	}
	mw := getManifestWork(t, cl, tf.Spec.BubbleCluster, testFailoverManifestWorkName(tf))
	if len(mw.Spec.Workload.Manifests) != 2 {
		t.Errorf("ManifestWork carries %d manifests, want 2 (PV, PVC)", len(mw.Spec.Workload.Manifests))
	}
	if len(mw.Spec.ManifestConfigs) != 2 || mw.Spec.ManifestConfigs[0].ResourceIdentifier.Name != tf.Spec.SourceRef {
		t.Errorf("feedback rule not set on the bubble PVC %q: %+v", tf.Spec.SourceRef, mw.Spec.ManifestConfigs)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("drill reached Ready before the PVC was reported bound")
	}

	// The work-agent reports the PVC bound.
	markManifestWorkPVCBound(t, cl, mw)

	// Pass 2: the drill reaches Ready.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ReadyAt == nil {
		t.Errorf("readyAt was not set")
	}
}

// Regression: 2026-09-29-testfailover-nil-volumecontext — the bubble PV must
// carry the source's class-level VolumeContext (so the node plugin has a non-nil
// context to stage) while still pointing at the clone by handle.
func TestFailoverPlacingPVCarriesSourceVolumeContext(t *testing.T) {
	tf := atPlacing()
	tf.Status.Clones[0].SourceVolumeContext = map[string]string{
		"tune2fs_reserved_blocks": "",
		"fabric":                  testFabricTCP,
	}
	tf.Status.Clones[0].SourceFSType = testFSTypeXFS
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	mw := getManifestWork(t, cl, tf.Spec.BubbleCluster, testFailoverManifestWorkName(tf))
	// manifests are [PV, PVC]; decode the PV.
	if len(mw.Spec.Workload.Manifests) != 2 {
		t.Fatalf("ManifestWork carries %d manifests, want 2", len(mw.Spec.Workload.Manifests))
	}
	var pv corev1.PersistentVolume
	if err := json.Unmarshal(mw.Spec.Workload.Manifests[0].Raw, &pv); err != nil {
		t.Fatalf("decode bubble PV manifest: %v", err)
	}
	if pv.Spec.CSI == nil {
		t.Fatal("bubble PV has no CSI source")
	}
	if pv.Spec.CSI.VolumeHandle != tf.Status.Clones[0].CloneID {
		t.Errorf("bubble PV points at %q, want the clone handle %q", pv.Spec.CSI.VolumeHandle, tf.Status.Clones[0].CloneID)
	}
	if pv.Spec.CSI.VolumeAttributes["fabric"] != testFabricTCP {
		t.Errorf("bubble PV VolumeAttributes did not carry the source class params: %+v", pv.Spec.CSI.VolumeAttributes)
	}
	// The clone carries the source's filesystem; without this the node plugin
	// defaults to ext4 and refuses to mount the XFS volume.
	if pv.Spec.CSI.FSType != testFSTypeXFS {
		t.Errorf("bubble PV fsType = %q, want the source's xfs", pv.Spec.CSI.FSType)
	}
}

// TestFailoverPlacingGroupDeliversAllMembersThenReady covers the group placement:
// one ManifestWork carries the namespace and a PV+PVC pair per member, and the
// drill reaches Ready only once every member's PVC binds.
func TestFailoverPlacingGroupDeliversAllMembersThenReady(t *testing.T) {
	tf := sampleTestFailover()
	tf.Finalizers = []string{finalizerTestFailover}
	tf.Spec.Scope = simplyblockv1alpha2.TestFailoverScopeGroup
	tf.Spec.SourceRef = "cg"
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepPlacing)}
	tf.Status.Clones = []simplyblockv1alpha2.TestFailoverClone{
		{SourceRef: "data-1", SourceHandle: "C:pool-1:lvol-a", SnapshotID: "B:pb:s1", CloneID: "B:pb:c1", SourceFSType: testFSTypeXFS, SizeBytes: 1073741824},
		{SourceRef: "data-2", SourceHandle: "C:pool-1:lvol-b", SnapshotID: "B:pb:s2", CloneID: "B:pb:c2", SourceFSType: testFSTypeXFS, SizeBytes: 1073741824},
	}
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	key := testFailoverRequest(tf).NamespacedName

	// Pass 1: the ManifestWork is created carrying 2*(PV,PVC) = 4 manifests and
	// two configs per member (PVC feedback, PV strategy); the drill holds until both bind.
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	mw := getManifestWork(t, cl, tf.Spec.BubbleCluster, testFailoverManifestWorkName(tf))
	if len(mw.Spec.Workload.Manifests) != 4 {
		t.Errorf("ManifestWork carries %d manifests, want 4 (2*(PV,PVC))", len(mw.Spec.Workload.Manifests))
	}
	if len(mw.Spec.ManifestConfigs) != 4 {
		t.Errorf("ManifestWork has %d configs, want two per member (4)", len(mw.Spec.ManifestConfigs))
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("drill reached Ready before any PVC bound")
	}

	// Only one member bound: still not Ready.
	bound := string(corev1.ClaimBound)
	pvcBound := func(name string) workv1.ManifestCondition {
		return workv1.ManifestCondition{
			ResourceMeta:    workv1.ManifestResourceMeta{Resource: "persistentvolumeclaims", Name: name},
			StatusFeedbacks: workv1.StatusFeedbackResult{Values: []workv1.FeedbackValue{{Name: "phase", Value: workv1.FieldValue{Type: workv1.String, String: &bound}}}},
		}
	}
	mw.Status.ResourceStatus.Manifests = []workv1.ManifestCondition{pvcBound("data-1")}
	if err := cl.Status().Update(ctx, mw); err != nil {
		t.Fatalf("update MW status (one bound): %v", err)
	}
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("drill reached Ready with only one of two member PVCs bound")
	}

	// Both bound: Ready.
	mw.Status.ResourceStatus.Manifests = []workv1.ManifestCondition{pvcBound("data-1"), pvcBound("data-2")}
	if err := cl.Status().Update(ctx, mw); err != nil {
		t.Fatalf("update MW status (both bound): %v", err)
	}
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("phase = %q, want Ready once both member PVCs are bound", got.Status.Phase)
	}
}

// TestFailoverPlacingReuseManifestWorkOnRestart covers restart safety: a second
// reconcile before the PVC binds finds the existing ManifestWork, not a duplicate.
func TestFailoverPlacingReuseManifestWorkOnRestart(t *testing.T) {
	tf := atPlacing()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("pass 2 (restart): %v", err)
	}
	var list workv1.ManifestWorkList
	if err := cl.List(ctx, &list, client.InNamespace(tf.Spec.BubbleCluster)); err != nil {
		t.Fatalf("list ManifestWorks: %v", err)
	}
	// The drill's own work plus the bubble's namespace work; neither duplicated.
	if len(list.Items) != 2 {
		t.Errorf("got %d ManifestWorks, want exactly 2 (drill + namespace, no duplicate on restart)", len(list.Items))
	}
}

// deletingReadyDrill returns a Ready drill with a clone recorded, being deleted.
func deletingReadyDrill() *simplyblockv1alpha2.TestFailover {
	tf := atPlacing()
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseReady
	tf.Status.Clones[0].SnapshotID = "clusterA:poolA:snapS"
	now := metav1.Now()
	tf.DeletionTimestamp = &now
	return tf
}

func srcPVView(tf *simplyblockv1alpha2.TestFailover, handle string) *unstructured.Unstructured {
	v := &unstructured.Unstructured{}
	v.SetGroupVersionKind(managedClusterViewGVK)
	v.SetNamespace(tf.Spec.SourceCluster)
	v.SetName(testFailoverViewName(tf, "src-pv"))
	_ = unstructured.SetNestedMap(v.Object, map[string]interface{}{
		"spec": map[string]interface{}{"csi": map[string]interface{}{"volumeHandle": handle}},
	}, "status", "result")
	return v
}

// TestFailoverTeardownReclaimsThenClearsFinalizer covers that deleting a drill
// reclaims the clone, removes the ManifestWork, and only then clears the
// finalizer. The recovery point is a replicated snapshot the drill only resolved,
// so it is left alone.
func TestFailoverTeardownReclaimsThenClearsFinalizer(t *testing.T) {
	tf := deletingReadyDrill()
	mw := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{
		Name: testFailoverManifestWorkName(tf), Namespace: tf.Spec.BubbleCluster,
	}}
	r, cl := newTestFailoverReconciler(t, tf, mw)
	ctx := context.Background()

	var reclaimedClone bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/volumes/cloneVol"):
			reclaimedClone = true
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/snapshots/"):
			t.Errorf("teardown deleted a snapshot it only resolved: %s", req.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("teardown reconcile: %v", err)
	}
	if !reclaimedClone {
		t.Errorf("the clone was not reclaimed")
	}
	err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &simplyblockv1alpha2.TestFailover{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("drill still present after teardown (finalizer not cleared): %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: tf.Spec.BubbleCluster, Name: mw.Name}, &workv1.ManifestWork{}); !apierrors.IsNotFound(err) {
		t.Errorf("ManifestWork still present after teardown: %v", err)
	}
}

// TestFailoverTeardownTolersatesAlreadyGoneResources covers idempotency: a 404
// from every reclaim is treated as success, so a re-run after a partial teardown
// still completes.
func TestFailoverTeardownToleratesAlreadyGone(t *testing.T) {
	tf := deletingReadyDrill()
	r, cl := newTestFailoverReconciler(t, tf) // no ManifestWork seeded
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("teardown reconcile: %v", err)
	}
	err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &simplyblockv1alpha2.TestFailover{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("drill still present after teardown of already-gone resources: %v", err)
	}
}

// TestFailoverTeardownHoldsWhenReclaimFails covers that a reclaim that cannot be
// confirmed holds the object with its finalizer rather than orphaning storage.
func TestFailoverTeardownHoldsWhenReclaimFails(t *testing.T) {
	tf := deletingReadyDrill()
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err == nil {
		t.Fatalf("expected an error to hold teardown when a reclaim fails")
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatalf("drill was removed despite a failed reclaim: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, finalizerTestFailover) {
		t.Errorf("finalizer was cleared despite a failed reclaim")
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseTearingDown {
		t.Errorf("phase = %q, want TearingDown while holding", got.Status.Phase)
	}
}

// TestFailoverPlacingFailsWhenSourceChanged covers the non-disruptiveness guard:
// if the source's projected volume handle differs at Ready, the drill fails
// rather than reporting a passing, non-disruptive test.
func TestFailoverPlacingFailsWhenSourceChanged(t *testing.T) {
	tf := atPlacing() // SourceHandle is clusterA:poolA:lvolX
	view := srcPVView(tf, "clusterA:poolA:DIFFERENT")
	mw := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{
		Name: testFailoverManifestWorkName(tf), Namespace: tf.Spec.BubbleCluster,
	}}
	r, cl := newTestFailoverReconciler(t, tf, view, mw)
	ctx := context.Background()

	markManifestWorkPVCBound(t, cl, getManifestWork(t, cl, tf.Spec.BubbleCluster, mw.Name))

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("phase = %q, want Failed when the source changed", got.Status.Phase)
	}
	if got.Status.Report == nil || got.Status.Report.InvariantsHeld {
		t.Errorf("invariantsHeld = true, want false when the source changed")
	}
}

// TestFailoverPlacingConfirmsInvariantHeld covers the passing guard: an unchanged
// source projection yields Ready with invariantsHeld true.
func TestFailoverPlacingConfirmsInvariantHeld(t *testing.T) {
	tf := atPlacing()
	view := srcPVView(tf, "clusterA:poolA:lvolX") // same as SourceHandle
	mw := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{
		Name: testFailoverManifestWorkName(tf), Namespace: tf.Spec.BubbleCluster,
	}}
	r, cl := newTestFailoverReconciler(t, tf, view, mw)
	ctx := context.Background()

	markManifestWorkPVCBound(t, cl, getManifestWork(t, cl, tf.Spec.BubbleCluster, mw.Name))

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Report == nil || !got.Status.Report.InvariantsHeld {
		t.Errorf("invariantsHeld = false, want true for an unchanged source")
	}
}

// TestFailoverRefusesSecondDrillForSameSource covers the concurrency guard: a
// second drill against the same source and bubble as an active one is refused.
func TestFailoverRefusesSecondDrillForSameSource(t *testing.T) {
	existing := atResolvingSource() // active (Provisioning) on the sample source/bubble
	existing.Name = "drill-existing"
	existing.UID = "uid-existing"

	second := sampleTestFailover() // same source/bubble, not yet started
	second.Name = "drill-second"
	second.UID = "uid-second"
	second.Finalizers = []string{finalizerTestFailover}

	r, cl := newTestFailoverReconciler(t, existing, second)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(second).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("phase = %q, want Failed for a conflicting second drill", got.Status.Phase)
	}
}

// TestFailoverStepDeadlineFailsTheDrill covers that a step that blew its deadline
// fails the drill rather than holding forever.
func TestFailoverStepDeadlineFailsTheDrill(t *testing.T) {
	tf := atResolvingSource()
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	tf.Status.Step = statemachine.KubeSnapshot{
		State:    string(simplyblockv1alpha2.TestFailoverStepResolvingSource),
		Deadline: &past,
	}
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseFailed {
		t.Errorf("phase = %q, want Failed after the step deadline passed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "deadline") {
		t.Errorf("message = %q, want it to mention the deadline", got.Status.Message)
	}
}

// Regression: 2026-10-04 bubble PVC Lost. The work agent re-applied the whole PV
// under the default update strategy and wiped spec.claimRef.uid; every object a
// drill places is CreateOnly now, and the namespace is in its own work.
func TestFailoverPlacingPlacesEverythingCreateOnly(t *testing.T) {
	tf := atPlacing()
	tf.Spec.BubbleNamespace = bubbleNS
	r, cl := newTestFailoverReconciler(t, tf)
	if _, err := r.Reconcile(context.Background(), testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	mw := getManifestWork(t, cl, tf.Spec.BubbleCluster, testFailoverManifestWorkName(tf))
	seen := map[string]bool{}
	for _, c := range mw.Spec.ManifestConfigs {
		if c.UpdateStrategy == nil || c.UpdateStrategy.Type != workv1.UpdateStrategyTypeCreateOnly {
			t.Errorf("%s/%s: update strategy %+v, want CreateOnly",
				c.ResourceIdentifier.Resource, c.ResourceIdentifier.Name, c.UpdateStrategy)
		}
		seen[c.ResourceIdentifier.Resource] = true
	}
	if !seen["persistentvolumes"] || !seen["persistentvolumeclaims"] {
		t.Errorf("configs cover %v, want both the PV and the PVC", seen)
	}
	for _, m := range mw.Spec.Workload.Manifests {
		var obj metav1.TypeMeta
		if err := json.Unmarshal(m.Raw, &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind == "Namespace" {
			t.Errorf("the drill's work carries the bubble namespace; it belongs to the namespace work")
		}
	}
	nsw := getManifestWork(t, cl, tf.Spec.BubbleCluster,
		bubbleNamespaceWorkName(tf.Spec.BubbleCluster, tf.Spec.BubbleNamespace))
	if len(nsw.Spec.Workload.Manifests) != 1 || len(nsw.Spec.ManifestConfigs) != 1 ||
		nsw.Spec.ManifestConfigs[0].UpdateStrategy.Type != workv1.UpdateStrategyTypeCreateOnly {
		t.Errorf("namespace work = %+v, want the one namespace, CreateOnly", nsw.Spec)
	}
}

// Two drills of one test share the bubble namespace: the second finds the
// namespace work and does not fail; one work owns the namespace.
func TestFailoverPlacingSharesOneNamespaceWork(t *testing.T) {
	a := atPlacing()
	a.Spec.BubbleNamespace = bubbleNS
	b := atPlacing()
	b.Name, b.UID = "drill-2", "uid-2"
	b.Spec.BubbleNamespace = bubbleNS
	b.Spec.SourceRef = "other-data"
	r, cl := newTestFailoverReconciler(t, a, b)
	for _, tf := range []*simplyblockv1alpha2.TestFailover{a, b} {
		if _, err := r.Reconcile(context.Background(), testFailoverRequest(tf)); err != nil {
			t.Fatalf("reconcile %s: %v", tf.Name, err)
		}
	}
	var works workv1.ManifestWorkList
	if err := cl.List(context.Background(), &works, client.InNamespace(a.Spec.BubbleCluster)); err != nil {
		t.Fatal(err)
	}
	owners := 0
	for i := range works.Items {
		for _, m := range works.Items[i].Spec.Workload.Manifests {
			var obj metav1.TypeMeta
			if err := json.Unmarshal(m.Raw, &obj); err != nil {
				t.Fatal(err)
			}
			if obj.Kind == "Namespace" {
				owners++
			}
		}
	}
	if owners != 1 {
		t.Errorf("%d works carry the bubble namespace, want exactly one", owners)
	}
}

func TestLiveDrillOnBubble(t *testing.T) {
	self := sampleTestFailover()
	self.UID = "self"
	self.Spec.BubbleNamespace = "ns-1"
	other := func(uid, cluster, ns string, deleting bool) simplyblockv1alpha2.TestFailover {
		o := *sampleTestFailover()
		o.UID = types.UID(uid)
		o.Spec.BubbleCluster, o.Spec.BubbleNamespace = cluster, ns
		if deleting {
			now := metav1.Now()
			o.DeletionTimestamp = &now
		}
		return o
	}
	cases := []struct {
		name  string
		items []simplyblockv1alpha2.TestFailover
		want  bool
	}{
		{"only itself", []simplyblockv1alpha2.TestFailover{*self}, false},
		{"another live drill on the bubble", []simplyblockv1alpha2.TestFailover{*self,
			other("o1", self.Spec.BubbleCluster, "ns-1", false)}, true},
		{"the other is being deleted", []simplyblockv1alpha2.TestFailover{*self,
			other("o1", self.Spec.BubbleCluster, "ns-1", true)}, false},
		{"another bubble namespace", []simplyblockv1alpha2.TestFailover{*self,
			other("o1", self.Spec.BubbleCluster, "ns-2", false)}, false},
		{"another cluster", []simplyblockv1alpha2.TestFailover{*self,
			other("o1", "elsewhere", "ns-1", false)}, false},
	}
	for _, c := range cases {
		if got := liveDrillOnBubble(c.items, self); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Teardown keeps the namespace work while another live drill places into it, and
// removes it with the last one.
func TestFailoverDeletionRemovesTheNamespaceWorkWithTheLastDrill(t *testing.T) {
	mk := func(name, uid string, deleting bool) *simplyblockv1alpha2.TestFailover {
		tf := sampleTestFailover()
		tf.Name, tf.UID = name, types.UID(uid)
		tf.Spec.BubbleNamespace = bubbleNS
		tf.Finalizers = []string{finalizerTestFailover}
		if deleting {
			now := metav1.Now()
			tf.DeletionTimestamp = &now
		}
		return tf
	}
	first, second := mk("drill-1", "u1", true), mk("drill-2", "u2", false)
	nsw, err := bubbleNamespaceWork(first)
	if err != nil {
		t.Fatal(err)
	}
	r, cl := newTestFailoverReconciler(t, first, second, nsw)
	ctx := context.Background()
	key := client.ObjectKey{Namespace: nsw.Namespace, Name: nsw.Name}

	if _, err := r.Reconcile(ctx, testFailoverRequest(first)); err != nil {
		t.Fatalf("teardown of drill-1: %v", err)
	}
	if err := cl.Get(ctx, key, &workv1.ManifestWork{}); err != nil {
		t.Fatalf("namespace work removed while drill-2 still places into it: %v", err)
	}

	var live simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(second).NamespacedName, &live); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(ctx, &live); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, testFailoverRequest(second)); err != nil {
		t.Fatalf("teardown of drill-2: %v", err)
	}
	if err := cl.Get(ctx, key, &workv1.ManifestWork{}); !apierrors.IsNotFound(err) {
		t.Errorf("namespace work still present after the last drill: %v", err)
	}
}

// bubbleNS is the bubble namespace the binding tests place their clones in.
const bubbleNS = "app-drtest-1"
