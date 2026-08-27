package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconcileActivateTransitions(t *testing.T) {
	t.Run("initializes running status for activate", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-a",
				Namespace:  "default",
				Generation: 9,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionActivate,
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.reconcileActivate(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileActivate returned error: %v", err)
		}
		if cluster.Status.ActionStatus == nil {
			t.Fatalf("expected actionStatus to be initialized")
		}
		if cluster.Status.ActionStatus.Action != utils.ClusterActionActivate {
			t.Fatalf("expected activate action, got %q", cluster.Status.ActionStatus.Action)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected running state, got %q", cluster.Status.ActionStatus.State)
		}
	})

	t.Run("short-circuits when already successful for current generation", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-b",
				Namespace:  "default",
				Generation: 4,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionActivate,
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.ClusterActionActivate,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 4,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.reconcileActivate(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileActivate returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected no delayed requeue, got %+v", res)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateSuccess {
			t.Fatalf("expected success to remain stable, got %q", cluster.Status.ActionStatus.State)
		}
	})

	t.Run("resets state machine when previous action differs", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-c",
				Namespace:  "default",
				Generation: 2,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionActivate,
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: utils.ClusterActionExpand,
					State:  utils.ActionStateSuccess,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.reconcileActivate(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileActivate returned error: %v", err)
		}
		if cluster.Status.ActionStatus.Action != utils.ClusterActionActivate {
			t.Fatalf("expected activate action, got %q", cluster.Status.ActionStatus.Action)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected running state after reset, got %q", cluster.Status.ActionStatus.State)
		}
	})
}

func TestReconcileActivateInitializesObservedGeneration(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-activate-observed-generation",
			Namespace:  "default",
			Generation: 17,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Action: utils.ClusterActionActivate,
		},
	}

	r := newClusterStateTestReconciler(t, cluster)
	_, err := r.reconcileActivate(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcileActivate returned error: %v", err)
	}

	if cluster.Status.ActionStatus == nil {
		t.Fatalf("expected actionStatus to be initialized")
	}
	if cluster.Status.ActionStatus.ObservedGeneration != cluster.Generation {
		t.Fatalf(
			"expected observedGeneration=%d for activate initialization, got %d",
			cluster.Generation,
			cluster.Status.ActionStatus.ObservedGeneration,
		)
	}
}

func TestReconcileExpandTransitions(t *testing.T) {
	t.Run("initializes running status for expand with observed generation", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-d",
				Namespace:  "default",
				Generation: 11,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionExpand,
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.reconcileExpand(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileExpand returned error: %v", err)
		}
		if cluster.Status.ActionStatus == nil {
			t.Fatalf("expected actionStatus to be initialized")
		}
		if cluster.Status.ActionStatus.ObservedGeneration != cluster.Generation {
			t.Fatalf("expected observedGeneration=%d, got %d", cluster.Generation, cluster.Status.ActionStatus.ObservedGeneration)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected running state, got %q", cluster.Status.ActionStatus.State)
		}
	})

	t.Run("short-circuits when already successful for current generation", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-e",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionExpand,
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.ClusterActionExpand,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 3,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.reconcileExpand(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileExpand returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected no delayed requeue, got %+v", res)
		}
	})

	t.Run("resets state machine when previous action differs", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-f",
				Namespace:  "default",
				Generation: 6,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionExpand,
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: utils.ClusterActionActivate,
					State:  utils.ActionStateSuccess,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.reconcileExpand(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileExpand returned error: %v", err)
		}
		if cluster.Status.ActionStatus.Action != utils.ClusterActionExpand {
			t.Fatalf("expected expand action, got %q", cluster.Status.ActionStatus.Action)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected running state after reset, got %q", cluster.Status.ActionStatus.State)
		}
	})
}

func TestFailActivateAndExpandTransitionToFailed(t *testing.T) {
	t.Run("activate failure transitions running to failed", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-g",
				Namespace: "default",
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: utils.ClusterActionActivate,
					State:  utils.ActionStateRunning,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.failActivate(context.Background(), cluster, context.Canceled)
		if err != nil {
			t.Fatalf("failActivate returned error: %v", err)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateFailed {
			t.Fatalf("expected failed state, got %q", cluster.Status.ActionStatus.State)
		}
		if !strings.Contains(cluster.Status.ActionStatus.Message, "canceled") {
			t.Fatalf("expected cancellation message, got %q", cluster.Status.ActionStatus.Message)
		}
	})

	t.Run("expand failure transitions running to failed", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-h",
				Namespace: "default",
			},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: utils.ClusterActionExpand,
					State:  utils.ActionStateRunning,
				},
			},
		}

		r := newClusterStateTestReconciler(t, cluster)
		_, err := r.failExpand(context.Background(), cluster, context.DeadlineExceeded)
		if err != nil {
			t.Fatalf("failExpand returned error: %v", err)
		}
		if cluster.Status.ActionStatus.State != utils.ActionStateFailed {
			t.Fatalf("expected failed state, got %q", cluster.Status.ActionStatus.State)
		}
		if !strings.Contains(cluster.Status.ActionStatus.Message, "deadline") {
			t.Fatalf("expected deadline message, got %q", cluster.Status.ActionStatus.Message)
		}
	})
}

func TestReconcileActivateRejectsIllegalSuccessState(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-illegal-activate",
			Namespace:  "default",
			Generation: 8,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Action: utils.ClusterActionActivate,
		},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			// Illegal/stale success: generation gate does not match.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:             utils.ClusterActionActivate,
				State:              utils.ActionStateSuccess,
				ObservedGeneration: 7,
				Triggered:          true,
			},
		},
	}

	r := newClusterStateTestReconciler(t, cluster)
	res, err := r.reconcileActivate(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcileActivate returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("activate failure path should not schedule delayed requeue, got %+v", res)
	}
	if cluster.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected illegal activate success to be rejected and moved to failed, got %q", cluster.Status.ActionStatus.State)
	}
}

func TestReconcileExpandRejectsIllegalSuccessState(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-illegal-expand",
			Namespace:  "default",
			Generation: 12,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Action: utils.ClusterActionExpand,
		},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			// Illegal/stale success: generation gate does not match.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:             utils.ClusterActionExpand,
				State:              utils.ActionStateSuccess,
				ObservedGeneration: 11,
				Triggered:          true,
			},
		},
	}

	r := newClusterStateTestReconciler(t, cluster)
	res, err := r.reconcileExpand(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcileExpand returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expand failure path should not schedule delayed requeue, got %+v", res)
	}
	if cluster.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected illegal expand success to be rejected and moved to failed, got %q", cluster.Status.ActionStatus.State)
	}
}

func TestFdActivationDomainCountViolation(t *testing.T) {
	hosts := func(domainCounts ...int32) map[string]int32 {
		m := map[string]int32{}
		for i, fd := range domainCounts {
			m[fmt.Sprintf("10.0.0.%d", i)] = fd
		}
		return m
	}

	cases := []struct {
		name    string
		npcs    int
		hosts   map[string]int32
		wantErr bool
	}{
		{"empty", 1, map[string]int32{}, true},
		{"npcs1 two domains violates", 1, hosts(0, 0, 1, 1), true},
		{"npcs1 three domains ok", 1, hosts(0, 1, 2), false},
		{"npcs1 three domains unequal violates", 1, hosts(0, 1, 1, 2), true},
		{"npcs2 two domains violates", 2, hosts(0, 0, 1, 1), true},
		{"npcs2 three domains violates", 2, hosts(0, 1, 2), true},
		{"npcs2 four domains ok", 2, hosts(0, 1, 2, 3), false},
		{"npcs2 four domains unequal violates", 2, hosts(0, 0, 1, 2, 3), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := fdActivationDomainCountViolation(tc.npcs, tc.hosts)
			if tc.wantErr && reason == "" {
				t.Fatalf("expected a violation reason, got none")
			}
			if !tc.wantErr && reason != "" {
				t.Fatalf("expected no violation, got: %s", reason)
			}
		})
	}
}

func TestClusterFailureDomainHosts(t *testing.T) {
	fd := func(v int32) *int32 { return &v }

	nsA := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-x"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "w0", MgmtIp: "10.0.0.1", FailureDomain: fd(0)},
				{Hostname: "w1", MgmtIp: "10.0.0.2", FailureDomain: fd(1)},
				{Hostname: "w2", MgmtIp: "10.0.0.3"},                                                        // no domain yet, must be skipped
				{Hostname: "w4", MgmtIp: "10.0.0.5", FailureDomain: fd(0), Status: utils.NodeStatusRemoved}, // removed, must be skipped
			},
		},
	}
	nsB := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-b", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-x"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "w3", MgmtIp: "10.0.0.4", FailureDomain: fd(2)},
			},
		},
	}
	nsOther := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-other", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-y"},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "y0", MgmtIp: "10.0.0.9", FailureDomain: fd(0)},
			},
		},
	}

	r := newClusterStateTestReconciler(t, nsA, nsB, nsOther)

	got, err := clusterFailureDomainHosts(context.Background(), r.Client, "default", "cluster-x")
	if err != nil {
		t.Fatalf("clusterFailureDomainHosts returned error: %v", err)
	}
	want := map[string]int32{"10.0.0.1": 0, "10.0.0.2": 1, "10.0.0.4": 2}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for ip, fdVal := range want {
		if got[ip] != fdVal {
			t.Fatalf("expected %s -> domain %d, got %v", ip, fdVal, got)
		}
	}
}

func TestFdRemovalBalanceViolation(t *testing.T) {
	tests := []struct {
		name        string
		counts      map[int32]int
		wantBlocked bool
	}{
		{
			name:        "empty (feature unused) is fine",
			counts:      map[int32]int{},
			wantBlocked: false,
		},
		{
			name:        "balanced 2/2/3 stays within +/-1",
			counts:      map[int32]int{0: 2, 1: 2, 2: 3},
			wantBlocked: false,
		},
		{
			name:        "1/2/3 exceeds +/-1 spread",
			counts:      map[int32]int{0: 1, 1: 2, 2: 3},
			wantBlocked: true,
		},
		{
			name:        "single host in a domain violates the 2-per-domain floor even within +/-1",
			counts:      map[int32]int{0: 1, 1: 2},
			wantBlocked: true,
		},
		{
			name: "a domain present with count zero violates the floor",
			// Regression for the 2026-08-26 finding: A=2,B=2,C=1, C's only
			// host removed. The caller must decrement C to 0 rather than
			// drop it from the map entirely -- a dropped key would leave
			// counts={A:2,B:2}, which both the +/-1 and floor checks below
			// would wrongly accept.
			counts:      map[int32]int{0: 2, 1: 2, 2: 0},
			wantBlocked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := fdRemovalBalanceViolation(tt.counts)
			if tt.wantBlocked && reason == "" {
				t.Errorf("expected a violation reason, got none")
			}
			if !tt.wantBlocked && reason != "" {
				t.Errorf("expected no violation, got %q", reason)
			}
		})
	}
}

func TestClusterEnsureFinalizer(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-finalizer", Namespace: "default"},
	}
	r := newClusterStateTestReconciler(t, cluster)

	updated, err := r.ensureFinalizer(context.Background(), cluster)
	if err != nil {
		t.Fatalf("ensureFinalizer returned error: %v", err)
	}
	if !updated {
		t.Fatalf("expected ensureFinalizer to add finalizer")
	}
	if !contains(cluster.Finalizers, utils.FinalizerStorageCluster) {
		t.Fatalf("expected cluster finalizer to be present")
	}
}

func TestClusterHandleDeletionPaths(t *testing.T) {
	now := metav1.NewTime(time.Now())

	t.Run("no deletion timestamp is passthrough", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-no-delete", Namespace: "default"},
		}
		r := newClusterStateTestReconciler(t, cluster)

		res, done, err := r.handleDeletion(context.Background(), cluster)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if done {
			t.Fatalf("expected done=false for non-deleting resource")
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("unexpected requeueAfter for passthrough path: %+v", res)
		}
	})

	t.Run("activate action removes finalizer without API delete", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-activate-delete",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerStorageCluster},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{
				Action: utils.ClusterActionActivate,
			},
		}
		r := newClusterStateTestReconciler(t, cluster)

		_, done, err := r.handleDeletion(context.Background(), cluster)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !done {
			t.Fatalf("expected done=true for handled deletion")
		}
		if contains(cluster.Finalizers, utils.FinalizerStorageCluster) {
			t.Fatalf("expected finalizer to be removed for activate-action deletion")
		}
	})

	t.Run("missing auth requeues when uuid exists", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-auth-missing",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerStorageCluster},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				UUID: "cluster-uuid-auth-missing",
			},
		}
		r := newClusterStateTestReconciler(t, cluster)

		res, done, err := r.handleDeletion(context.Background(), cluster)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !done {
			t.Fatalf("expected done=true for deletion path")
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected requeueAfter when auth is missing")
		}
		if !contains(cluster.Finalizers, utils.FinalizerStorageCluster) {
			t.Fatalf("expected finalizer to remain on requeue path")
		}
	})

	t.Run("successful API delete removes finalizer", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-delete-ok"
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
		defer mock.Close()
		mock.Register(
			http.MethodDelete,
			"/api/v2/clusters/"+clusterUUID,
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-delete-ok",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerStorageCluster},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				UUID: clusterUUID,
			},
		}
		r := newClusterStateTestReconciler(t, cluster)

		_, done, err := r.handleDeletion(context.Background(), cluster)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !done {
			t.Fatalf("expected done=true for handled deletion")
		}
		if contains(cluster.Finalizers, utils.FinalizerStorageCluster) {
			t.Fatalf("expected finalizer removed after successful delete")
		}
		if len(mock.Requests()) != 1 || mock.Requests()[0].Path != "/api/v2/clusters/"+clusterUUID {
			t.Fatalf("expected delete API call for cluster UUID, got %#v", mock.Requests())
		}
	})
}

func TestUpsertCSICredentialsSecret(t *testing.T) {
	r := newClusterStateTestReconciler(t)
	ctx := context.Background()

	if err := r.upsertCSICredentialsSecret(ctx, "default", "cluster-1", "http://ep1", "sec1"); err != nil {
		t.Fatalf("upsertCSICredentialsSecret returned error: %v", err)
	}
	// idempotent for same cluster ID
	if err := r.upsertCSICredentialsSecret(ctx, "default", "cluster-1", "http://ep1", "sec1"); err != nil {
		t.Fatalf("idempotent upsert failed: %v", err)
	}
	// append another cluster
	if err := r.upsertCSICredentialsSecret(ctx, "default", "cluster-2", "http://ep2", "sec2"); err != nil {
		t.Fatalf("second cluster upsert failed: %v", err)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: "simplyblock-csi-secret-v2", Namespace: "default"}, secret); err != nil {
		t.Fatalf("failed to fetch CSI credentials secret: %v", err)
	}

	var creds CSICredentials
	if err := json.Unmarshal(secret.Data["secret.json"], &creds); err != nil {
		t.Fatalf("failed to unmarshal secret payload: %v", err)
	}
	if len(creds.Clusters) != 2 {
		t.Fatalf("expected 2 unique clusters, got %#v", creds.Clusters)
	}
}

func TestStorageClusterReconcileTopLevelPaths(t *testing.T) {
	t.Run("adds finalizer on first reconcile", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-top-finalizer", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)

		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if !contains(current.Finalizers, utils.FinalizerStorageCluster) {
			t.Fatalf("expected finalizer to be added")
		}
	})

	t.Run("syncs status periodically when cluster UUID already present and no action", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-top-noop",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{
				UUID: "cluster-uuid-top-noop",
			},
		}
		r := newClusterStateTestReconciler(t, cluster)

		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected periodic requeue for status sync, got %+v", res)
		}
	})
}

func TestStorageClusterReconcileActivateViaMock(t *testing.T) {
	const clusterUUID = "cluster-uuid-activate-mock"

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/activate",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID,
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `{
				"id":"` + clusterUUID + `",
				"status":"active",
				"distr_ndcs":2,
				"distr_npcs":1,
				"is_re_balancing":false
			}`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-activate-mock",
			Namespace:  "default",
			Generation: 2,
			Finalizers: []string{utils.FinalizerStorageCluster},
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Action: utils.ClusterActionActivate,
		},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: clusterUUID,
		},
	}
	r := newClusterStateTestReconciler(t, cluster)

	// 1) initialize action status
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatalf("initial activate reconcile returned error: %v", err)
	}

	// 2) trigger activate API call
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	if err != nil {
		t.Fatalf("trigger activate reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after activate trigger")
	}

	// 3) observe active cluster and mark success
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	if err != nil {
		t.Fatalf("finalize activate reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal result after active status, got %+v", res)
	}

	current := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
		t.Fatalf("failed to fetch updated cluster: %v", err)
	}
	if current.Status.ActionStatus == nil || current.Status.ActionStatus.State != utils.ActionStateSuccess {
		t.Fatalf("expected activate action to complete successfully, got %#v", current.Status.ActionStatus)
	}
	if current.Status.Status != utils.ClusterStatusActive {
		t.Fatalf("expected cluster status active, got %q", current.Status.Status)
	}
	if len(mock.Requests()) < 2 {
		t.Fatalf("expected activate POST and cluster GET calls, got %#v", mock.Requests())
	}
}

func TestStorageClusterReconcileExpandViaMock(t *testing.T) {
	const clusterUUID = "cluster-uuid-expand-mock"

	// expand endpoint is currently missing from openapi.json, so allow unknown paths.
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/expand",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID,
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `{
				"id":"` + clusterUUID + `",
				"status":"active",
				"distr_ndcs":3,
				"distr_npcs":1,
				"is_re_balancing":false
			}`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-expand-mock",
			Namespace:  "default",
			Generation: 3,
			Finalizers: []string{utils.FinalizerStorageCluster},
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Action: utils.ClusterActionExpand,
		},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: clusterUUID,
		},
	}
	r := newClusterStateTestReconciler(t, cluster)

	// 1) initialize action status
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatalf("initial expand reconcile returned error: %v", err)
	}

	// 2) trigger expand API call
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	if err != nil {
		t.Fatalf("trigger expand reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after expand trigger")
	}

	// 3) observe active cluster and mark success
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	if err != nil {
		t.Fatalf("finalize expand reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal result after expanded active status, got %+v", res)
	}

	current := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
		t.Fatalf("failed to fetch updated cluster: %v", err)
	}
	if current.Status.ActionStatus == nil || current.Status.ActionStatus.State != utils.ActionStateSuccess {
		t.Fatalf("expected expand action to complete successfully, got %#v", current.Status.ActionStatus)
	}
	if len(mock.Requests()) < 2 {
		t.Fatalf("expected expand POST and cluster GET calls, got %#v", mock.Requests())
	}
}

func TestStorageClusterReconcileCreationPaths(t *testing.T) {
	t.Run("returns nil for not-found cluster", func(t *testing.T) {
		r := newClusterStateTestReconciler(t)
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
		})
		if err != nil {
			t.Fatalf("expected ignore-not-found behavior, got err=%v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("unexpected delayed requeue for missing resource: %+v", res)
		}
	})

	t.Run("health check failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusInternalServerError, Body: `{"error":"fdb down"}`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-health-fail",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue on failed health check")
		}
	})

	t.Run("cluster create api failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{Status: http.StatusBadGateway, Body: `{"error":"create failed"}`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-fail",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when create API fails")
		}
	})

	t.Run("create payload parse failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-parse-fail",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when create response is invalid")
		}
	})

	t.Run("create success populates status and secret", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `{
					"id":"cluster-new-uuid",
					"secret":"cluster-new-secret",
					"nqn":"nqn.2026-04.io.simplyblock:cluster-new",
					"distr_ndcs":2,
					"distr_npcs":1,
					"is_re_balancing":false,
					"status":"online"
				}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-ok",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result after successful create, got %+v", res)
		}

		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if current.Status.UUID != "cluster-new-uuid" || !current.Status.Configured {
			t.Fatalf("unexpected cluster status after create: %#v", current.Status)
		}
	})

	t.Run("create populates erasure coding scheme", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{
				Status: http.StatusCreated,
				Body: `{
					"id":"cluster-dto-new-uuid",
					"name":"cluster-create-dto",
					"secret":"cluster-dto-new-secret",
					"nqn":"nqn.2026-04.io.simplyblock:cluster-dto-new",
					"status":"inactive",
					"is_re_balancing":false,
					"distr_ndcs":4,
					"distr_npcs":2
				}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-dto",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result after dto-shaped create, got %+v", res)
		}

		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if current.Status.UUID != "cluster-dto-new-uuid" {
			t.Fatalf("expected dto id to populate status uuid, got %#v", current.Status)
		}
		if current.Status.ErasureCodingScheme != "4x2" {
			t.Fatalf("expected dto coding tuple to map to erasureCodingScheme, got %#v", current.Status)
		}
	})
}

// TestNodeRecycleSnodeRefreshWriteAheadPreventsDoubleDelete verifies the
// write-ahead fix for #153: the phase must be persisted to "snode-refresh-wait"
// BEFORE the pod is deleted so that an operator restart during the snode-refresh
// phase does not delete the newly-started replacement pod.
//
// The test simulates a restart by calling nodeRecycleSnodeRefresh a second time
// while the in-memory phase still reads "snode-refresh" (as would happen if the
// status update had not been committed before the crash). The interceptor counts
// pod deletions and asserts exactly one deletion occurred.
func TestNodeRecycleSnodeRefreshWriteAheadPreventsDoubleDelete(t *testing.T) {
	const (
		ns          = "default"
		clusterName = "cluster-nr"
		clusterUUID = "uuid-nr"
		nodeUUID    = "node-uuid-nr"
		nodeIP      = "192.168.10.99"
		k8sNodeName = "k8s-worker-nr"
	)

	// Backend: return one storage node with nodeIP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal([]utils.NodeStatusResponse{
			{UUID: nodeUUID, IP: nodeIP, Status: utils.NodeStatusOnline},
		})
		_, _ = w.Write(resp)
	}))
	defer srv.Close()
	apiClient := webapi.NewClient(srv.URL)

	clusterCR := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: clusterUUID,
			NodeRecycleStatus: &simplyblockv1alpha1.NodeRecycleStatus{
				PendingNodes: []string{nodeUUID},
				NodePhase:    utils.NodeRecyclePhaseSnodeRefresh,
			},
		},
	}
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: k8sNodeName},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "storage-node-pod",
			Namespace: ns,
			Labels: map[string]string{
				"app":                 "storage-node",
				"simplyblock-cluster": clusterName,
			},
		},
		Spec: corev1.PodSpec{NodeName: k8sNodeName},
	}

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)

	// Count pod deletions via an interceptor.
	podDeleteCount := 0
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.StorageCluster{}).
		WithObjects(clusterCR, k8sNode, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					podDeleteCount++
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &StorageClusterReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
	}

	// First call — this should persist "snode-refresh-wait" then delete the pod.
	_, err := r.nodeRecycleSnodeRefresh(context.Background(), clusterCR, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		t.Fatalf("first nodeRecycleSnodeRefresh returned error: %v", err)
	}

	// Verify write-ahead: phase must be persisted to snode-refresh-wait.
	var updated simplyblockv1alpha1.StorageCluster
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(clusterCR), &updated); err != nil {
		t.Fatalf("failed to get updated cluster: %v", err)
	}
	if updated.Status.NodeRecycleStatus == nil {
		t.Fatal("NodeRecycleStatus is nil after first call")
	}
	if got := updated.Status.NodeRecycleStatus.NodePhase; got != utils.NodeRecyclePhaseSnodeRefreshWait {
		t.Fatalf("expected phase %q after first call, got %q", utils.NodeRecyclePhaseSnodeRefreshWait, got)
	}
	if podDeleteCount != 1 {
		t.Fatalf("expected exactly 1 pod deletion after first call, got %d", podDeleteCount)
	}

	// Simulate operator restart: the in-memory clusterCR still shows the old
	// phase (stale), but the persisted CR already has "snode-refresh-wait".
	// A restarted operator would re-read the CR and dispatch to
	// nodeRecycleSnodeRefreshWait — not nodeRecycleSnodeRefresh.
	// Calling nodeRecycleSnodeRefresh again with the stale in-memory object
	// proves the write-ahead guard works: the Status().Update will see the
	// already-advanced phase and the pod must NOT be deleted a second time.
	clusterCR.Status.NodeRecycleStatus.NodePhase = utils.NodeRecyclePhaseSnodeRefresh

	_, err = r.nodeRecycleSnodeRefresh(context.Background(), clusterCR, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		t.Fatalf("second nodeRecycleSnodeRefresh returned error: %v", err)
	}

	// The pod must still have been deleted exactly once — the second call
	// persists "snode-refresh-wait" again but the pod no longer exists so
	// deleteStorageNodeSetPod is a no-op (pod == nil → returns true,nil).
	if podDeleteCount != 1 {
		t.Fatalf("pod was deleted %d times — replacement pod would be killed on restart (bug #153)", podDeleteCount)
	}
}

// TestReconcileCreateOptimisticLockPreventsRace covers the TOCTOU fix for
// INCIDENT-027: a concurrent reconciler that loses the optimistic-lock patch
// on Status.Phase backs off without making any backend API calls, and
// Status.Phase is cleared to "" once creation succeeds.
func TestReconcileCreateOptimisticLockPreventsRace(t *testing.T) {
	t.Run("concurrent reconciler backs off on lock conflict", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-lock-backoff",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec:   simplyblockv1alpha1.StorageClusterSpec{},
			Status: simplyblockv1alpha1.StorageClusterStatus{},
		}

		scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)

		patchCount := 0
		cl := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&simplyblockv1alpha1.StorageCluster{}).
			WithObjects(cluster).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					patchCount++
					return fmt.Errorf("409 Conflict: the object has been modified; please apply your changes to the latest version")
				},
			}).
			Build()

		r := &StorageClusterReconciler{
			Client:   cl,
			Scheme:   scheme,
			Recorder: events.NewFakeRecorder(10),
		}

		res, err := r.reconcileCreate(context.Background(), cluster)
		if err != nil {
			t.Fatalf("reconcileCreate returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected back-off requeue after lock conflict, got RequeueAfter=0")
		}
		if patchCount != 1 {
			t.Fatalf("expected exactly one status patch attempt (the lock claim), got %d", patchCount)
		}
	})

	t.Run("phase is cleared after successful creation", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/_meta/ready",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `{
					"id":"cluster-phase-ok-uuid",
					"secret":"phase-secret",
					"nqn":"nqn.test:cluster-phase-ok",
					"distr_ndcs":2,
					"distr_npcs":1,
					"is_re_balancing":false,
					"status":"online"
				}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-phase-ok",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		r := newClusterStateTestReconciler(t, cluster)

		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result after successful create, got %+v", res)
		}

		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if current.Status.Phase != "" {
			t.Fatalf("expected Status.Phase cleared after creation, got %q", current.Status.Phase)
		}
		if current.Status.SubPhase != "" {
			t.Fatalf("expected Status.SubPhase cleared after creation, got %q", current.Status.SubPhase)
		}
		if current.Status.UUID != "cluster-phase-ok-uuid" {
			t.Fatalf("expected UUID populated after creation, got %q", current.Status.UUID)
		}
	})
}

func newClusterStateTestReconciler(t *testing.T, objects ...client.Object) *StorageClusterReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageCluster{},
	}, objects...)

	return &StorageClusterReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
	}
}
