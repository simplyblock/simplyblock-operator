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

// atResolvingPoint returns a drill seeded at ResolvingPoint with its source
// already resolved to sourceHandle.
func atResolvingPoint(sourceHandle string) *simplyblockv1alpha2.TestFailover {
	tf := atResolvingSource()
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepResolvingPoint)}
	tf.Status.Clones = []simplyblockv1alpha2.TestFailoverClone{{
		SourceRef:    tf.Spec.SourceRef,
		SourceHandle: sourceHandle,
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
	listGVK.Kind += "List"
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
			SourceCluster:   "ramen-cluster-a",
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
	gvk.Kind += "List"
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
	tf := atResolvingPoint("clusterA:poolA:lvolX") // bubble != source in the sample
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
	if got.Status.Clones[0].SnapshotTaken {
		t.Errorf("snapshotTaken = true, want false for a replicated point")
	}
	if got.Status.Report == nil || got.Status.Report.RecoveryPoint != "snapY" {
		t.Errorf("report.recoveryPoint not set to snapY: %+v", got.Status.Report)
	}
}

// TestFailoverResolvingPointDRTargetNoReplicaFails covers that a target with no
// replicated point yet is a terminal failure, not an endless hold.
func TestFailoverResolvingPointDRTargetNoReplicaFails(t *testing.T) {
	tf := atResolvingPoint("clusterA:poolA:lvolX")
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

// TestFailoverResolvingPointInPlaceTakesFreshSnapshot covers the in-place path:
// the drill takes a fresh snapshot of the source and advances to Cloning.
func TestFailoverResolvingPointInPlaceTakesFreshSnapshot(t *testing.T) {
	tf := atResolvingPoint("clusterA:poolA:lvolX")
	tf.Spec.BubbleCluster = tf.Spec.SourceCluster // in-place
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/storage-pools/poolA/snapshots"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/volumes/lvolX/snapshots"):
			w.Header().Set("Location", "/api/v2/clusters/clusterA/storage-pools/poolA/snapshots/snapNew")
			w.WriteHeader(http.StatusCreated)
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
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepCloning) {
		t.Errorf("step = %q, want Cloning", got.Status.Step.State)
	}
	if got.Status.Clones[0].SnapshotID != "clusterA:poolA:snapNew" {
		t.Errorf("snapshot handle = %q, want clusterA:poolA:snapNew", got.Status.Clones[0].SnapshotID)
	}
	if !got.Status.Clones[0].SnapshotTaken {
		t.Errorf("snapshotTaken = false, want true for a freshly taken snapshot")
	}
}

// TestFailoverResolvingPointInPlaceReusesExistingSnapshot covers ask-then-act
// idempotency: an already-present snapshot with the drill's name is reused, and
// no second snapshot is taken.
func TestFailoverResolvingPointInPlaceReusesExistingSnapshot(t *testing.T) {
	tf := atResolvingPoint("clusterA:poolA:lvolX")
	tf.Spec.BubbleCluster = tf.Spec.SourceCluster
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	wantName := testFailoverSnapshotName(tf)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/storage-pools/poolA/snapshots") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "snapExisting", "name": wantName},
			})
			return
		}
		if req.Method == http.MethodPost {
			t.Errorf("a second snapshot was taken though one already existed: %s", req.URL.Path)
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
	if got.Status.Clones[0].SnapshotID != "clusterA:poolA:snapExisting" {
		t.Errorf("snapshot handle = %q, want clusterA:poolA:snapExisting", got.Status.Clones[0].SnapshotID)
	}
}

// TestFailoverResolvingPointInPlacePinnedSnapshot covers a pinned recovery point:
// it is used directly, nothing is taken, and it is not marked for deletion.
func TestFailoverResolvingPointInPlacePinnedSnapshot(t *testing.T) {
	tf := atResolvingPoint("clusterA:poolA:lvolX")
	tf.Spec.BubbleCluster = tf.Spec.SourceCluster
	tf.Spec.RecoveryPoint = "pinnedSnap"
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()
	// The control plane must not be called at all for a pinned point.
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	if _, err := r.Reconcile(ctx, testFailoverRequest(tf)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got simplyblockv1alpha2.TestFailover
	if err := cl.Get(ctx, testFailoverRequest(tf).NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Clones[0].SnapshotID != "clusterA:poolA:pinnedSnap" {
		t.Errorf("snapshot handle = %q, want clusterA:poolA:pinnedSnap", got.Status.Clones[0].SnapshotID)
	}
	if got.Status.Clones[0].SnapshotTaken {
		t.Errorf("snapshotTaken = true, want false for a pinned snapshot")
	}
	if got.Status.Step.State != string(simplyblockv1alpha2.TestFailoverStepCloning) {
		t.Errorf("step = %q, want Cloning", got.Status.Step.State)
	}
}

// atCloning returns a drill seeded at Cloning with its recovery point resolved.
func atCloning(snapshotHandle string) *simplyblockv1alpha2.TestFailover {
	tf := atResolvingPoint("clusterA:poolA:lvolX")
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepCloning)}
	tf.Status.Clones[0].SnapshotID = snapshotHandle
	return tf
}

// TestFailoverCloningClonesThenAdvances covers cloning the recovery point into a
// writable volume and advancing to Placing with the clone handle recorded.
func TestFailoverCloningClonesThenAdvances(t *testing.T) {
	tf := atCloning("clusterB:poolB:snapY")
	r, cl := newTestFailoverReconciler(t, tf)
	ctx := context.Background()

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/storage-pools/poolB/volumes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/storage-pools/poolB/volumes"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "cloneZ", "size": 1073741824})
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
	if got.Status.Clones[0].CloneID != "clusterB:poolB:cloneZ" {
		t.Errorf("clone handle = %q, want clusterB:poolB:cloneZ", got.Status.Clones[0].CloneID)
	}
	if got.Status.Clones[0].SizeBytes != 1073741824 {
		t.Errorf("clone size = %d, want 1073741824", got.Status.Clones[0].SizeBytes)
	}
}

// TestFailoverCloningReusesExistingClone covers ask-then-act idempotency: an
// existing clone with the drill's name is reused, and no second clone is built.
func TestFailoverCloningReusesExistingClone(t *testing.T) {
	tf := atCloning("clusterB:poolB:snapY")
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
	tf := atCloning("clusterB:poolB:snapY")
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
func atPlacing(cloneHandle string) *simplyblockv1alpha2.TestFailover {
	tf := atCloning("clusterB:poolB:snapY")
	tf.Status.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepPlacing)}
	tf.Status.Clones[0].CloneID = cloneHandle
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
// ManifestWork carrying the bubble namespace, PV, and PVC is delivered to the
// recovery cluster, and the drill reaches Ready once the PVC binds.
func TestFailoverPlacingDeliversManifestWorkThenReady(t *testing.T) {
	tf := atPlacing("clusterB:poolB:cloneZ")
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
	if len(mw.Spec.Workload.Manifests) != 3 {
		t.Errorf("ManifestWork carries %d manifests, want 3 (namespace, PV, PVC)", len(mw.Spec.Workload.Manifests))
	}
	if len(mw.Spec.ManifestConfigs) != 1 || mw.Spec.ManifestConfigs[0].ResourceIdentifier.Name != tf.Spec.SourceRef {
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

// TestFailoverPlacingReuseManifestWorkOnRestart covers restart safety: a second
// reconcile before the PVC binds finds the existing ManifestWork, not a duplicate.
func TestFailoverPlacingReuseManifestWorkOnRestart(t *testing.T) {
	tf := atPlacing("clusterB:poolB:cloneZ")
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
	if len(list.Items) != 1 {
		t.Errorf("got %d ManifestWorks, want exactly 1 (no duplicate on restart)", len(list.Items))
	}
}

// deletingReadyDrill returns a Ready drill with a clone and a drill-taken
// snapshot recorded, being deleted.
func deletingReadyDrill() *simplyblockv1alpha2.TestFailover {
	tf := atPlacing("clusterB:poolB:cloneZ")
	tf.Status.Phase = simplyblockv1alpha2.TestFailoverPhaseReady
	tf.Status.Clones[0].SnapshotID = "clusterA:poolA:snapS"
	tf.Status.Clones[0].SnapshotTaken = true
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
// reclaims the clone and the drill-taken snapshot, removes the ManifestWork, and
// only then clears the finalizer.
func TestFailoverTeardownReclaimsThenClearsFinalizer(t *testing.T) {
	tf := deletingReadyDrill()
	mw := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{
		Name: testFailoverManifestWorkName(tf), Namespace: tf.Spec.BubbleCluster,
	}}
	r, cl := newTestFailoverReconciler(t, tf, mw)
	ctx := context.Background()

	var reclaimedClone, deletedSnapshot bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/volumes/cloneZ"):
			reclaimedClone = true
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/snapshots/snapS"):
			deletedSnapshot = true
			w.WriteHeader(http.StatusNoContent)
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
	if !deletedSnapshot {
		t.Errorf("the drill-taken snapshot was not deleted")
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
	tf := atPlacing("clusterB:poolB:cloneZ") // SourceHandle is clusterA:poolA:lvolX
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
	tf := atPlacing("clusterB:poolB:cloneZ")
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
