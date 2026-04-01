package controller

import (
	"context"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
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

func newClusterStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockStorageClusterReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add API scheme: %v", err)
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
