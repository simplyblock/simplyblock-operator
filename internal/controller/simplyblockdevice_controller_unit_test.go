package controller

import (
	"context"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeviceActionCompleted(t *testing.T) {
	r := &DeviceReconciler{}

	tests := []struct {
		name   string
		action string
		status string
		want   bool
	}{
		{
			name:   "remove success",
			action: utils.DeviceActionRemove,
			status: utils.DeviceStatusRemoved,
			want:   true,
		},
		{
			name:   "remove not yet",
			action: utils.DeviceActionRemove,
			status: "online",
			want:   false,
		},
		{
			name:   "restart success",
			action: utils.DeviceActionRestart,
			status: utils.DeviceStatusOnline,
			want:   true,
		},
		{
			name:   "restart not yet",
			action: utils.DeviceActionRestart,
			status: "restarting",
			want:   false,
		},
		{
			name:   "unknown action",
			action: "noop",
			status: "anything",
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.deviceActionCompleted(tc.action, tc.status)
			if got != tc.want {
				t.Fatalf("deviceActionCompleted(%q,%q) = %v, want %v", tc.action, tc.status, got, tc.want)
			}
		})
	}
}

func TestReconcileDeviceActionTransitions(t *testing.T) {
	t.Run("initializes running status when action status is missing", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-a",
				Namespace:  "default",
				Generation: 7,
			},
			Spec: simplyblockv1alpha1.DeviceSpec{
				Action:   utils.DeviceActionRemove,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		_, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if dev.Status.ActionStatus == nil {
			t.Fatalf("expected action status to be initialized")
		}
		if dev.Status.ActionStatus.Action != utils.DeviceActionRemove {
			t.Fatalf("unexpected action initialized: %q", dev.Status.ActionStatus.Action)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("unexpected state initialized: %q", dev.Status.ActionStatus.State)
		}
		if dev.Status.ActionStatus.ObservedGeneration != dev.Generation {
			t.Fatalf("expected observedGeneration=%d, got %d", dev.Generation, dev.Status.ActionStatus.ObservedGeneration)
		}
	})

	t.Run("does not re-enter terminal success for same generation", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-b",
				Namespace:  "default",
				Generation: 5,
			},
			Spec: simplyblockv1alpha1.DeviceSpec{
				Action:   utils.DeviceActionRestart,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.DeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.DeviceActionRestart,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 5,
					Triggered:          true,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		_, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if !dev.Status.ActionStatus.Triggered {
			t.Fatalf("expected action status to remain terminal and unchanged")
		}
	})

	t.Run("re-initializes running status when action changes", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-c",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: simplyblockv1alpha1.DeviceSpec{
				Action:   utils.DeviceActionRemove,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.DeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.DeviceActionRestart,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 2,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		_, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if dev.Status.ActionStatus.Action != utils.DeviceActionRemove {
			t.Fatalf("expected action to be reset to remove, got %q", dev.Status.ActionStatus.Action)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected state running after action change, got %q", dev.Status.ActionStatus.State)
		}
	})

	t.Run("rejects unsupported action transition", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-d",
				Namespace:  "default",
				Generation: 2,
			},
			Spec: simplyblockv1alpha1.DeviceSpec{
				Action:   "invalid-action",
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.DeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: "invalid-action",
					State:  utils.ActionStateRunning,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		_, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err == nil {
			t.Fatalf("expected error for unsupported device action")
		}
		if !strings.Contains(err.Error(), "unsupported device action") {
			t.Fatalf("unexpected error: %v", err)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("unsupported action should not transition to success, got %q", dev.Status.ActionStatus.State)
		}
	})
}

func TestFailDeviceActionTransitionsToFailed(t *testing.T) {
	dev := &simplyblockv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-f",
			Namespace: "default",
		},
		Status: simplyblockv1alpha1.DeviceStatus{
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action: utils.DeviceActionRemove,
				State:  utils.ActionStateRunning,
			},
		},
	}

	r := newDeviceStateTestReconciler(t, dev)
	_, err := r.failDeviceAction(context.Background(), dev, context.DeadlineExceeded)
	if err != nil {
		t.Fatalf("failDeviceAction returned error: %v", err)
	}
	if dev.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected failed state, got %q", dev.Status.ActionStatus.State)
	}
	if !strings.Contains(dev.Status.ActionStatus.Message, "deadline") {
		t.Fatalf("expected failure message to be set, got %q", dev.Status.ActionStatus.Message)
	}
}

func TestReconcileDeviceActionRejectsIllegalSuccessState(t *testing.T) {
	dev := &simplyblockv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "dev-illegal",
			Namespace:  "default",
			Generation: 10,
		},
		Spec: simplyblockv1alpha1.DeviceSpec{
			Action:   utils.DeviceActionRemove,
			DeviceID: "d1",
			NodeUUID: "n1",
		},
		Status: simplyblockv1alpha1.DeviceStatus{
			// Illegal/stale success: observedGeneration does not match current generation.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:             utils.DeviceActionRemove,
				State:              utils.ActionStateSuccess,
				ObservedGeneration: 9,
				Triggered:          true,
			},
		},
	}

	r := newDeviceStateTestReconciler(t, dev)
	res, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
	if err != nil {
		t.Fatalf("reconcileDeviceAction returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue after rejecting illegal success state, got %+v", res)
	}
	if dev.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected illegal success to be rejected and moved to failed, got %q", dev.Status.ActionStatus.State)
	}
}

func TestDeviceReconcileTopLevelPaths(t *testing.T) {
	t.Run("returns nil for not found resource", func(t *testing.T) {
		r := newDeviceStateTestReconciler(t)
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
		})
		if err != nil {
			t.Fatalf("expected ignore-not-found behavior, got err=%v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("unexpected delayed requeue for not-found: %+v", res)
		}
	})

	t.Run("requeues when cluster uuid is not ready", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-no-cluster", Namespace: "default"},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		r := newDeviceStateTestReconciler(t, dev)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when cluster uuid unresolved")
		}
	})

	t.Run("requeues when cluster auth secret is missing", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-no-auth", Namespace: "default"},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-a"},
		}
		r := newDeviceStateTestReconciler(t, dev, cluster)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue when cluster secret is missing")
		}
	})

	t.Run("deletion removes finalizer and returns terminal result", func(t *testing.T) {
		now := metav1.NewTime(time.Now())
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "dev-delete-ok",
				Namespace:         "default",
				Finalizers:        []string{utils.FinalizerDevice},
				DeletionTimestamp: &now,
			},
			Spec: simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-delete"},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-a", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-uuid-delete"),
				"secret": []byte("cluster-secret"),
			},
		}
		r := newDeviceStateTestReconciler(t, dev, cluster, secret)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected terminal result in deletion path, got %+v", res)
		}
		current := &simplyblockv1alpha1.Device{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(dev), current); err != nil {
			if !apierrors.IsNotFound(err) {
				t.Fatalf("failed to fetch device after deletion flow: %v", err)
			}
			return
		}
		if contains(current.Finalizers, utils.FinalizerDevice) {
			t.Fatalf("expected device finalizer to be removed")
		}
	})

	t.Run("adds finalizer when missing and exits", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-add-finalizer", Namespace: "default"},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-finalizer"},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-a", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-uuid-finalizer"),
				"secret": []byte("cluster-secret"),
			},
		}
		r := newDeviceStateTestReconciler(t, dev, cluster, secret)
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected immediate return after adding finalizer, got %+v", res)
		}
		current := &simplyblockv1alpha1.Device{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(dev), current); err != nil {
			t.Fatalf("failed to fetch device: %v", err)
		}
		if !contains(current.Finalizers, utils.FinalizerDevice) {
			t.Fatalf("expected finalizer to be added")
		}
	})

	t.Run("delegates to action reconcile path when action is set", func(t *testing.T) {
		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-action-delegate",
				Namespace:  "default",
				Finalizers: []string{utils.FinalizerDevice},
			},
			Spec: simplyblockv1alpha1.DeviceSpec{
				ClusterName: "cluster-a",
				Action:      utils.DeviceActionRestart,
				// Missing deviceID/nodeUUID forces reconcileDeviceAction error, proving delegation path.
			},
		}
		cluster := &simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
			Spec:       simplyblockv1alpha1.StorageClusterSpec{},
			Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-action"},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-a", Namespace: "default"},
			Data: map[string][]byte{
				"uuid":   []byte("cluster-uuid-action"),
				"secret": []byte("cluster-secret"),
			},
		}
		r := newDeviceStateTestReconciler(t, dev, cluster, secret)
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err == nil {
			t.Fatalf("expected action path error due missing deviceID/nodeUUID")
		}
		if !strings.Contains(err.Error(), "deviceID and nodeUUID must be set") {
			t.Fatalf("unexpected error from delegated action path: %v", err)
		}
	})
}

func TestDeviceReconcileInventoryPaths(t *testing.T) {
	baseCluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "cluster-uuid-inv"},
	}
	baseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-cluster-a", Namespace: "default"},
		Data: map[string][]byte{
			"uuid":   []byte("cluster-uuid-inv"),
			"secret": []byte("cluster-secret"),
		},
	}

	t.Run("requeues when node-list api call fails", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/",
			webapimock.RouteResponse{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-node-list-fail", Namespace: "default", Finalizers: []string{utils.FinalizerDevice}},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		r := newDeviceStateTestReconciler(t, dev, baseCluster.DeepCopy(), baseSecret.DeepCopy())
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue for node-list fetch failure")
		}
	})

	t.Run("requeues when node-list payload is invalid", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `{`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-node-list-invalid", Namespace: "default", Finalizers: []string{utils.FinalizerDevice}},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		r := newDeviceStateTestReconciler(t, dev, baseCluster.DeepCopy(), baseSecret.DeepCopy())
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue for invalid node-list payload")
		}
	})

	t.Run("requeues when no nodes are returned", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/",
			webapimock.RouteResponse{Status: http.StatusOK, Body: `[]`},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-no-nodes", Namespace: "default", Finalizers: []string{utils.FinalizerDevice}},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		r := newDeviceStateTestReconciler(t, dev, baseCluster.DeepCopy(), baseSecret.DeepCopy())
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue for empty node list")
		}
	})

	t.Run("requeues when per-node device fetches produce empty map", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `[{"uuid":"node-1"}]`,
			},
		)
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/node-1/devices/",
			webapimock.RouteResponse{
				Status: http.StatusBadGateway,
				Body:   `{"error":"downstream"}`,
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-empty-map", Namespace: "default", Finalizers: []string{utils.FinalizerDevice}},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
		}
		r := newDeviceStateTestReconciler(t, dev, baseCluster.DeepCopy(), baseSecret.DeepCopy())
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue for empty node->device map")
		}
	})

	t.Run("requeues when computed status is unchanged", func(t *testing.T) {
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `[{"uuid":"node-1"}]`,
			},
		)
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/cluster-uuid-inv/storage-nodes/node-1/devices/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[{
					"id":"dev-1",
					"status":"online",
					"health_check":true,
					"size":1073741824
				}]`,
			},
		)
		t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

		dev := &simplyblockv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-unchanged", Namespace: "default", Finalizers: []string{utils.FinalizerDevice}},
			Spec:       simplyblockv1alpha1.DeviceSpec{ClusterName: "cluster-a"},
			Status: simplyblockv1alpha1.DeviceStatus{
				Nodes: []simplyblockv1alpha1.NodeDevices{
					{
						NodeUUID: "node-1",
						Devices: []simplyblockv1alpha1.DeviceInfo{
							{
								UUID:   "dev-1",
								Status: "online",
								Size:   "1.0 GiB",
								Health: "true",
								Model:  "nvme",
							},
						},
					},
				},
			},
		}
		r := newDeviceStateTestReconciler(t, dev, baseCluster.DeepCopy(), baseSecret.DeepCopy())
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dev)})
		if err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue for unchanged status")
		}
	})
}

func newDeviceStateTestReconciler(t *testing.T, objects ...client.Object) *DeviceReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.Device{},
	}, objects...)

	return &DeviceReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
