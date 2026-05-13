package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newSnapRepTestReconciler(t *testing.T, objects ...client.Object) *SnapshotReplicationReconciler {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.SnapshotReplication{},
	}, objects...)
	return &SnapshotReplicationReconciler{
		Client: cl,
		Scheme: scheme,
	}
}

// clusterSecret builds the auth secret that resolveSourceClusterAuth looks up.
func snapRepClusterSecret(clusterName, uuid, secret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"uuid":   []byte(uuid),
			"secret": []byte(secret),
		},
	}
}

// clusterCR builds a StorageCluster CR with status.uuid set so ResolveClusterIdentifier resolves it.
func snapRepClusterCR(clusterName, uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: uuid, SecretName: "simplyblock-cluster-" + clusterName},
	}
}

func lvolListJSON(lvols ...map[string]any) string {
	b, _ := json.Marshal(lvols)
	return string(b)
}

// ── top-level reconcile paths ─────────────────────────────────────────────────

func TestSnapshotReplicationTopLevelPaths(t *testing.T) {
	t.Run("ignores not-found resource", func(t *testing.T) {
		r := newSnapRepTestReconciler(t)
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
		})
		if err != nil {
			t.Fatalf("expected no error for not-found, got %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected no requeue for not-found, got %v", res)
		}
	})

	t.Run("requeues when source cluster auth secret is missing", func(t *testing.T) {
		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-no-secret", Namespace: "default"},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "cluster-tgt",
				TargetPool:    "pool-tgt",
			},
		}
		r := newSnapRepTestReconciler(t, cr)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected requeue when auth secret is missing")
		}
	})

	t.Run("adds finalizer on first reconcile", func(t *testing.T) {
		const clusterUUID = "src-uuid-finalizer"
		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-finalizer", Namespace: "default"},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "cluster-tgt",
				TargetPool:    "pool-tgt",
			},
		}
		secret := snapRepClusterSecret("cluster-src", clusterUUID, "src-secret")
		srcCluster := snapRepClusterCR("cluster-src", clusterUUID)
		r := newSnapRepTestReconciler(t, cr, secret, srcCluster)

		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})

		current := &simplyblockv1alpha1.SnapshotReplication{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), current); err != nil {
			t.Fatalf("failed to fetch CR: %v", err)
		}
		if !contains(current.Finalizers, utils.FinalizerSnapshotReplication) {
			t.Fatalf("expected finalizer to be added, got %v", current.Finalizers)
		}
	})
}

// ── configured / addreplication ───────────────────────────────────────────────

func TestSnapshotReplicationEnsureConfigured(t *testing.T) {
	t.Run("calls addreplication and sets configured=true", func(t *testing.T) {
		const (
			srcUUID  = "src-uuid-cfg"
			tgtUUID  = "tgt-uuid-cfg"
			poolUUID = "pool-uuid-cfg"
		)
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(http.MethodPost, "/api/v2/clusters/"+srcUUID+"/addreplication/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`})
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-configured",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: tgtUUID, // pass UUID directly to skip CR lookup
				TargetPool:    poolUUID,
			},
		}
		srcSecret := snapRepClusterSecret("cluster-src", srcUUID, "src-s")
		srcCluster := snapRepClusterCR("cluster-src", srcUUID)
		tgtCluster := snapRepClusterCR(tgtUUID, tgtUUID)
		tgtPool := &simplyblockv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: poolUUID, Namespace: "default"},
			Spec:       simplyblockv1alpha1.PoolSpec{ClusterName: tgtUUID},
			Status:     simplyblockv1alpha1.PoolStatus{UUID: poolUUID},
		}
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster, tgtCluster, tgtPool)

		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}

		current := &simplyblockv1alpha1.SnapshotReplication{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), current); err != nil {
			t.Fatalf("fetch CR: %v", err)
		}
		if !current.Status.Configured {
			t.Fatalf("expected status.configured=true after addreplication succeeded")
		}
	})

	t.Run("requeues when addreplication returns error", func(t *testing.T) {
		const srcUUID = "src-uuid-cfg-fail"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(http.MethodPost, "/api/v2/clusters/"+srcUUID+"/addreplication/",
			webapimock.RouteResponse{Status: http.StatusInternalServerError, Body: `internal error`})
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "snap-cfg-fail",
				Namespace: "default",
				// pre-set finalizer so single Reconcile call reaches ensureConfigured
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "tgt-uuid",
				TargetPool:    "pool-uuid",
			},
			Status: simplyblockv1alpha1.SnapshotReplicationStatus{Configured: false},
		}
		srcSecret := snapRepClusterSecret("cluster-src", srcUUID, "src-s")
		srcCluster := snapRepClusterCR("cluster-src", srcUUID)
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster)

		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected requeue after addreplication failure")
		}

		current := &simplyblockv1alpha1.SnapshotReplication{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), current); err != nil {
			t.Fatalf("fetch CR: %v", err)
		}
		if current.Status.Configured {
			t.Fatalf("expected status.configured to remain false on failure")
		}
	})

	t.Run("skips addreplication when already configured", func(t *testing.T) {
		const srcUUID = "src-uuid-skip-cfg"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		// register addreplication — it must NOT be called
		mock.Register(http.MethodPost, "/api/v2/clusters/"+srcUUID+"/addreplication/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`})
		// normal replication — source cluster active check
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID,
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{"id":"` + srcUUID + `","status":"active"}`})
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-skip-cfg",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "tgt-uuid",
				TargetPool:    "pool-uuid",
			},
			Status: simplyblockv1alpha1.SnapshotReplicationStatus{Configured: true},
		}
		srcSecret := snapRepClusterSecret("cluster-src", srcUUID, "src-s")
		srcCluster := snapRepClusterCR("cluster-src", srcUUID)
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster)

		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})

		for _, req := range mock.Requests() {
			if req.Path == "/api/v2/clusters/"+srcUUID+"/addreplication/" {
				t.Fatalf("addreplication should not be called when already configured")
			}
		}
	})
}

// ── normal replication ─────────────────────────────────────────────────────

func TestSnapshotReplicationNormalReplication(t *testing.T) {
	t.Run("triggers replication for eligible lvol and updates volume status", func(t *testing.T) {
		const (
			srcUUID  = "src-uuid-normal"
			poolUUID = "pool-uuid-normal"
			lvolUUID = "lvol-uuid-normal"
		)
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		// cluster active check
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID,
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{"id":"` + srcUUID + `","status":"active"}`})
		// list pools
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"` + poolUUID + `"}]`})
		// list lvols
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: lvolListJSON(map[string]any{
				"id": lvolUUID, "do_replicate": true,
				"nqn": "nqn.2024-01.io.simplyblock:" + lvolUUID,
			})})
		// get lvol detail — also used by GetReplicationActiveSides (from_source=true)
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/"+lvolUUID,
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{
				"id":"` + lvolUUID + `","do_replicate":true,"from_source":true,
				"nqn":"nqn.2024-01.io.simplyblock:` + lvolUUID + `",
				"rep_info":{"last_replication_time":"2020-01-01T00:00:00Z","replicated_count":5,"last_snapshot_id":"snap-001"}
			}`})
		// last snapshot task done
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/"+lvolUUID+"/list_replication_tasks/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"task-1","status":"done"}]`})
		// trigger replication
		mock.Register(http.MethodPost, "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/"+lvolUUID+"/replication_trigger/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`})
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-normal",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: srcUUID, // use UUID directly to avoid CR lookup
				TargetPool:    poolUUID,
			},
			Status: simplyblockv1alpha1.SnapshotReplicationStatus{Configured: true},
		}
		srcSecret := snapRepClusterSecret("cluster-src", srcUUID, "src-s")
		srcCluster := snapRepClusterCR("cluster-src", srcUUID)
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster)

		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected periodic requeue after normal replication")
		}

		current := &simplyblockv1alpha1.SnapshotReplication{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), current); err != nil {
			t.Fatalf("fetch CR: %v", err)
		}
		if len(current.Status.Volumes) == 0 {
			t.Fatalf("expected volume status to be populated")
		}
		vol := current.Status.Volumes[0]
		if vol.VolumeID != lvolUUID {
			t.Fatalf("unexpected volumeID: %s", vol.VolumeID)
		}
		if vol.Phase != simplyblockv1alpha1.VolPhaseRunning {
			t.Fatalf("expected phase Running, got %s", vol.Phase)
		}
	})

	t.Run("skips lvol not in VolumeIDs allowlist", func(t *testing.T) {
		const (
			srcUUID     = "src-uuid-filter"
			poolUUID    = "pool-uuid-filter"
			lvolUUID    = "lvol-not-allowed"
			allowedUUID = "lvol-allowed"
		)
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID,
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{"id":"` + srcUUID + `","status":"active"}`})
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"` + poolUUID + `"}]`})
		mock.Register(http.MethodGet, "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: lvolListJSON(
				map[string]any{"id": lvolUUID, "do_replicate": true, "nqn": "nqn.x:" + lvolUUID},
			)})
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-filter",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "tgt-uuid",
				TargetPool:    "pool-uuid",
				VolumeIDs:     []string{allowedUUID}, // only allowedUUID — lvolUUID should be skipped
			},
			Status: simplyblockv1alpha1.SnapshotReplicationStatus{Configured: true},
		}
		srcSecret := snapRepClusterSecret("cluster-src", srcUUID, "src-s")
		srcCluster := snapRepClusterCR("cluster-src", srcUUID)
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster)

		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})

		// No replicate call should have been made for the excluded lvol
		for _, req := range mock.Requests() {
			if req.Path == "/api/v2/clusters/"+srcUUID+"/storage-pools/"+poolUUID+"/volumes/"+lvolUUID+"/replicate/" {
				t.Fatalf("replicate was called for an excluded lvol")
			}
		}
	})
}

// ── failback generation guard ─────────────────────────────────────────────────

func TestSnapshotReplicationFailbackGenerationGuard(t *testing.T) {
	t.Run("skips failback when generation already processed", func(t *testing.T) {
		const srcUUID = "src-uuid-gen-guard"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-gen-guard",
				Namespace:  "default",
				Generation: 3,
				// pre-set finalizer so single Reconcile reaches the failback guard check
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: srcUUID, // UUID directly avoids CR lookup
				TargetCluster: "tgt-uuid",
				TargetPool:    "pool-uuid",
				Action:        "failback",
			},
			Status: simplyblockv1alpha1.SnapshotReplicationStatus{
				Configured:                 true,
				ObservedFailbackGeneration: 3, // already processed
			},
		}
		srcSecret := snapRepClusterSecret(srcUUID, srcUUID, "src-s")
		srcCluster := snapRepClusterCR(srcUUID, srcUUID)
		r := newSnapRepTestReconciler(t, cr, srcSecret, srcCluster)

		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should requeue after 120s without making any failback API calls
		if res.RequeueAfter != 120*time.Second {
			t.Fatalf("expected 120s requeue, got %v", res.RequeueAfter)
		}
		for _, req := range mock.Requests() {
			if req.Path != "" {
				t.Fatalf("expected no API calls, got %s %s", req.Method, req.Path)
			}
		}
	})
}

// ── deletion / finalizer ──────────────────────────────────────────────────────

func TestSnapshotReplicationDeletion(t *testing.T) {
	t.Run("removes finalizer when deletion timestamp is set", func(t *testing.T) {
		now := metav1.Now()
		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "snap-delete",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerSnapshotReplication},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.SnapshotReplicationSpec{
				SourceCluster: "cluster-src",
				TargetCluster: "tgt-uuid",
				TargetPool:    "pool-uuid",
			},
		}
		r := newSnapRepTestReconciler(t, cr)

		updated, err := r.handleDeletion(context.Background(), cr)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !updated {
			t.Fatalf("expected updated=true on deletion")
		}
		if contains(cr.Finalizers, utils.FinalizerSnapshotReplication) {
			t.Fatalf("expected finalizer to be removed on deletion")
		}
	})

	t.Run("passthrough when no deletion timestamp", func(t *testing.T) {
		cr := &simplyblockv1alpha1.SnapshotReplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "snap-nodelete",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerSnapshotReplication},
			},
		}
		r := newSnapRepTestReconciler(t, cr)

		updated, err := r.handleDeletion(context.Background(), cr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated {
			t.Fatalf("expected updated=false when no deletion timestamp")
		}
	})
}

// ── setVolumePhase / setVolumeRepInfo helpers ────────────────────────────────

func TestSetVolumePhase(t *testing.T) {
	r := &SnapshotReplicationReconciler{}
	cr := &simplyblockv1alpha1.SnapshotReplication{}

	// creates new entry
	r.setVolumePhase(cr, "vol-1", simplyblockv1alpha1.VolPhaseRunning, "")
	if len(cr.Status.Volumes) != 1 || cr.Status.Volumes[0].Phase != simplyblockv1alpha1.VolPhaseRunning {
		t.Fatalf("unexpected volumes: %#v", cr.Status.Volumes)
	}

	// updates existing entry
	r.setVolumePhase(cr, "vol-1", simplyblockv1alpha1.VolPhaseCompleted, "")
	if cr.Status.Volumes[0].Phase != simplyblockv1alpha1.VolPhaseCompleted {
		t.Fatalf("expected phase Completed, got %s", cr.Status.Volumes[0].Phase)
	}

	// Failed phase appends error
	r.setVolumePhase(cr, "vol-1", simplyblockv1alpha1.VolPhaseFailed, "something broke")
	if len(cr.Status.Volumes[0].Errors) == 0 {
		t.Fatalf("expected error entry on Failed phase")
	}
	if cr.Status.Volumes[0].Errors[0].Message != "something broke" {
		t.Fatalf("unexpected error message: %s", cr.Status.Volumes[0].Errors[0].Message)
	}
}

func TestSetVolumeRepInfo(t *testing.T) {
	r := &SnapshotReplicationReconciler{}
	cr := &simplyblockv1alpha1.SnapshotReplication{
		Status: simplyblockv1alpha1.SnapshotReplicationStatus{
			Volumes: []simplyblockv1alpha1.VolumeReplicationStatus{
				{VolumeID: "vol-1"},
			},
		},
	}

	setVolRepInfoOnCR(r, cr, "vol-1", "snap-abc", 7)

	vol := cr.Status.Volumes[0]
	if vol.LastSnapshotID != "snap-abc" {
		t.Fatalf("unexpected LastSnapshotID: %s", vol.LastSnapshotID)
	}
	if vol.ReplicatedCount == nil || *vol.ReplicatedCount != 7 {
		t.Fatalf("unexpected ReplicatedCount: %v", vol.ReplicatedCount)
	}
}

// setVolRepInfoOnCR is a test helper that exercises setVolumeRepInfo without
// needing to construct a full utils.Lvol.
func setVolRepInfoOnCR(_ *SnapshotReplicationReconciler, cr *simplyblockv1alpha1.SnapshotReplication, volumeID, snapshotID string, count int64) {
	c := int32(count)
	for i := range cr.Status.Volumes {
		if cr.Status.Volumes[i].VolumeID == volumeID {
			cr.Status.Volumes[i].LastSnapshotID = snapshotID
			cr.Status.Volumes[i].ReplicatedCount = &c
			return
		}
	}
}

// ── shouldProcessFailbackVolume ───────────────────────────────────────────────

func TestShouldProcessFailbackVolume(t *testing.T) {
	cases := []struct {
		name       string
		volumeID   string
		includeIDs []string
		excludeIDs []string
		want       bool
	}{
		{"no filters — process all", "vol-1", nil, nil, true},
		{"in include list", "vol-1", []string{"vol-1", "vol-2"}, nil, true},
		{"not in include list", "vol-3", []string{"vol-1", "vol-2"}, nil, false},
		{"in exclude list", "vol-1", nil, []string{"vol-1"}, false},
		{"not in exclude list", "vol-2", nil, []string{"vol-1"}, true},
		{"in include and exclude — exclude wins", "vol-1", []string{"vol-1"}, []string{"vol-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldProcessFailbackVolume(tc.volumeID, tc.includeIDs, tc.excludeIDs)
			if got != tc.want {
				t.Fatalf("shouldProcessFailbackVolume(%q, %v, %v) = %v, want %v",
					tc.volumeID, tc.includeIDs, tc.excludeIDs, got, tc.want)
			}
		})
	}
}
