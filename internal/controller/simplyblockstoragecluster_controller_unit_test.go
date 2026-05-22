package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestClusterDeleteClusterSecret(t *testing.T) {
	t.Run("deletes named status secret", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-secret-delete", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{SecretName: "custom-secret-name"},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "custom-secret-name", Namespace: "default"},
		}
		r := newClusterStateTestReconciler(t, cluster, secret)

		if err := r.deleteClusterSecret(context.Background(), cluster); err != nil {
			t.Fatalf("deleteClusterSecret returned error: %v", err)
		}

		err := r.Get(context.Background(), client.ObjectKey{Name: "custom-secret-name", Namespace: "default"}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected secret to be deleted, got err=%v", err)
		}
	})

	t.Run("uses default secret name fallback", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-b", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		}
		secretName := "simplyblock-cluster-cluster-b"
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		}
		r := newClusterStateTestReconciler(t, cluster, secret)

		if err := r.deleteClusterSecret(context.Background(), cluster); err != nil {
			t.Fatalf("deleteClusterSecret returned error: %v", err)
		}

		err := r.Get(context.Background(), client.ObjectKey{Name: secretName, Namespace: "default"}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected fallback secret to be deleted, got err=%v", err)
		}
	})
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

	t.Run("successful API delete removes secret and finalizer", func(t *testing.T) {
		const clusterName = "cluster-delete-ok"
		const clusterUUID = "cluster-uuid-delete-ok"
		const clusterSecret = "secret-delete-ok"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
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
				UUID:       clusterUUID,
				SecretName: "secret-custom-delete-ok",
			},
		}
		authSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-" + clusterName, Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte(clusterUUID),
				"secret": []byte(clusterSecret),
			},
		}
		customSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-custom-delete-ok", Namespace: "default"},
		}
		r := newClusterStateTestReconciler(t, cluster, authSecret, customSecret)

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
		err = r.Get(context.Background(), client.ObjectKey{Name: "secret-custom-delete-ok", Namespace: "default"}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected cluster secret to be deleted, got err=%v", err)
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
	const clusterName = "cluster-activate-mock"
	const clusterUUID = "cluster-uuid-activate-mock"
	const clusterSecret = "secret-activate-mock"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
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
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-" + clusterName, Namespace: "default"},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte(clusterSecret),
		},
	}
	r := newClusterStateTestReconciler(t, cluster, authSecret)

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
	const clusterName = "cluster-expand-mock"
	const clusterUUID = "cluster-uuid-expand-mock"
	const clusterSecret = "secret-expand-mock"

	// expand endpoint is currently missing from openapi.json, so allow unknown paths.
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
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
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-" + clusterName, Namespace: "default"},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte(clusterSecret),
		},
	}
	r := newClusterStateTestReconciler(t, cluster, authSecret)

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
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
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

	t.Run("existing cluster auth lookup failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-auth-fail",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		existing := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-existing", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-existing-uuid"},
		}
		r := newClusterStateTestReconciler(t, cluster, existing)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when existing cluster auth cannot be fetched")
		}
	})

	t.Run("cluster create api failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v1/cluster/create_first/",
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

	t.Run("create_first payload parse failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v1/cluster/create_first/",
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
			t.Fatalf("expected delayed requeue when create_first response is invalid")
		}
	})

	t.Run("create_v2 payload parse failure requeues", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
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
				Name:       "cluster-v2-parse-fail",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		existing := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-existing-v2", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-existing-v2-uuid"},
		}
		existingSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-existing-v2", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-existing-v2-uuid"),
				"secret": []byte("existing-secret"),
			},
		}
		r := newClusterStateTestReconciler(t, cluster, existing, existingSecret)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when create_v2 response is invalid")
		}
	})

	t.Run("create_first success populates status and secret", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v1/cluster/create_first/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `{
					"results":{
						"uuid":"cluster-new-uuid",
						"secret":"cluster-new-secret",
						"nqn":"nqn.2026-04.io.simplyblock:cluster-new",
						"distr_ndcs":2,
						"distr_npcs":1,
						"is_re_balancing":false,
						"status":"online"
					}
				}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-first-ok",
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
			t.Fatalf("expected terminal result after successful create_first, got %+v", res)
		}

		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if current.Status.UUID != "cluster-new-uuid" || !current.Status.Configured {
			t.Fatalf("unexpected cluster status after create_first: %#v", current.Status)
		}
		authSecret := &corev1.Secret{}
		if err := r.Get(context.Background(), client.ObjectKey{
			Name:      "simplyblock-cluster-cluster-create-first-ok",
			Namespace: "default",
		}, authSecret); err != nil {
			t.Fatalf("expected auth secret to be created: %v", err)
		}
	})

	t.Run("create_v2 success populates status and secret", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `{
					"id":"cluster-v2-new-uuid",
					"secret":"cluster-v2-new-secret",
					"nqn":"nqn.2026-04.io.simplyblock:cluster-v2-new",
					"distr_ndcs":3,
					"distr_npcs":1,
					"is_re_balancing":true,
					"status":"online"
				}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-create-v2-ok",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		existing := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-existing-ok", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-existing-ok-uuid"},
		}
		existingSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-existing-ok", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-existing-ok-uuid"),
				"secret": []byte("existing-ok-secret"),
			},
		}
		r := newClusterStateTestReconciler(t, cluster, existing, existingSecret)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result after successful create_v2, got %+v", res)
		}

		current := &simplyblockv1alpha1.StorageCluster{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), current); err != nil {
			t.Fatalf("failed to fetch cluster: %v", err)
		}
		if current.Status.UUID != "cluster-v2-new-uuid" || !current.Status.Configured {
			t.Fatalf("unexpected cluster status after create_v2: %#v", current.Status)
		}
		authSecret := &corev1.Secret{}
		if err := r.Get(context.Background(), client.ObjectKey{
			Name:      "simplyblock-cluster-cluster-create-v2-ok",
			Namespace: "default",
		}, authSecret); err != nil {
			t.Fatalf("expected create_v2 auth secret to be created: %v", err)
		}
	})

	t.Run("create_v2 supports cluster dto response shape", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v1/health/fdb/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
		)
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/",
			webapimock.RouteResponse{
				Status: http.StatusCreated,
				Body: `{
					"id":"cluster-dto-new-uuid",
					"name":"cluster-create-v2-dto",
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
				Name:       "cluster-create-v2-dto",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerStorageCluster},
			},
			Spec: simplyblockv1alpha1.StorageClusterSpec{},
		}
		existing := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-existing-dto", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-existing-dto-uuid"},
		}
		existingSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-existing-dto", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-existing-dto-uuid"),
				"secret": []byte("existing-dto-secret"),
			},
		}
		r := newClusterStateTestReconciler(t, cluster, existing, existingSecret)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result after dto-shaped create_v2, got %+v", res)
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

func TestStorageClusterCreateFirstSecretHasOwnerReference(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodGet,
		"/api/v1/health/fdb/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodPost,
		"/api/v1/cluster/create_first/",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `{
				"results":{
					"uuid":"cluster-ownerref-uuid",
					"secret":"cluster-ownerref-secret",
					"nqn":"nqn.2026-04.io.simplyblock:cluster-ownerref",
					"distr_ndcs":2,
					"distr_npcs":1,
					"is_re_balancing":false,
					"status":"online"
				}
			}`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-ownerref",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerStorageCluster},
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{},
	}

	r := newClusterStateTestReconciler(t, cluster)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	secret := &corev1.Secret{}
	if err := r.Get(
		context.Background(),
		client.ObjectKey{Name: "simplyblock-cluster-cluster-ownerref", Namespace: "default"},
		secret,
	); err != nil {
		t.Fatalf("expected auth secret to be created: %v", err)
	}

	if len(secret.OwnerReferences) == 0 {
		t.Fatalf("expected auth secret to carry ownerReference to storagecluster CR")
	}
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
		Recorder: record.NewFakeRecorder(10),
	}
}
