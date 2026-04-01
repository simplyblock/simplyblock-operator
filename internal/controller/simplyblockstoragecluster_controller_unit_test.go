package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-manager/internal/webapi/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileActivateTransitions(t *testing.T) {
	t.Run("initializes running status for activate", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-a",
				Namespace:  "default",
				Generation: 9,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionActivate,
				ClusterName: "c1",
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-b",
				Namespace:  "default",
				Generation: 4,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionActivate,
				ClusterName: "c1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-c",
				Namespace:  "default",
				Generation: 2,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionActivate,
				ClusterName: "c1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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

func TestReconcileExpandTransitions(t *testing.T) {
	t.Run("initializes running status for expand with observed generation", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-d",
				Namespace:  "default",
				Generation: 11,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionExpand,
				ClusterName: "c1",
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-e",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionExpand,
				ClusterName: "c1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "cluster-f",
				Namespace:  "default",
				Generation: 6,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionExpand,
				ClusterName: "c1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-g",
				Namespace: "default",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-h",
				Namespace: "default",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-illegal-activate",
			Namespace:  "default",
			Generation: 8,
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
			Action:      utils.ClusterActionActivate,
			ClusterName: "c1",
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-illegal-expand",
			Namespace:  "default",
			Generation: 12,
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
			Action:      utils.ClusterActionExpand,
			ClusterName: "c1",
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
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
	if !contains(cluster.Finalizers, "simplyblock.cluster.finalizer") {
		t.Fatalf("expected cluster finalizer to be present")
	}
}

func TestClusterDeleteClusterSecret(t *testing.T) {
	t.Run("deletes named status secret", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-secret-delete", Namespace: "default"},
			Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: "cluster-a"},
			Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{SecretName: "custom-secret-name"},
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-secret-default", Namespace: "default"},
			Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: "cluster-b"},
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
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
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-activate-delete",
				Namespace:         "default",
				Finalizers:        []string{"simplyblock.cluster.finalizer"},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				Action:      utils.ClusterActionActivate,
				ClusterName: "cluster-a",
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
		if contains(cluster.Finalizers, "simplyblock.cluster.finalizer") {
			t.Fatalf("expected finalizer to be removed for activate-action deletion")
		}
	})

	t.Run("missing auth requeues when uuid exists", func(t *testing.T) {
		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-auth-missing",
				Namespace:         "default",
				Finalizers:        []string{"simplyblock.cluster.finalizer"},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				ClusterName: "cluster-auth-missing",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		if !contains(cluster.Finalizers, "simplyblock.cluster.finalizer") {
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

		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cluster-delete-ok",
				Namespace:         "default",
				Finalizers:        []string{"simplyblock.cluster.finalizer"},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageClusterSpec{
				ClusterName: clusterName,
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
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
		if contains(cluster.Finalizers, "simplyblock.cluster.finalizer") {
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

func newClusterStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockStorageClusterReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add API scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.SimplyBlockStorageCluster{}).
		WithObjects(objects...).
		Build()

	return &SimplyBlockStorageClusterReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
