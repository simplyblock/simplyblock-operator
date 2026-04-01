package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
	webapimock "github.com/simplyblock/simplyblock-manager/internal/webapi/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureNodeStatus(t *testing.T) {
	cr := &simplyblockv1alpha1.SimplyBlockStorageNode{}

	s := ensureNodeStatus(cr, "node-a", "10.0.0.1")
	if s == nil {
		t.Fatalf("ensureNodeStatus returned nil")
	}
	if s.Hostname != "node-a" || s.MgmtIp != "10.0.0.1" || s.Status != "in_creation" {
		t.Fatalf("unexpected initial node status: %#v", *s)
	}
	if len(cr.Status.Nodes) != 1 {
		t.Fatalf("expected one node, got %d", len(cr.Status.Nodes))
	}

	s2 := ensureNodeStatus(cr, "node-a", "10.0.0.99")
	if s2 == nil {
		t.Fatalf("ensureNodeStatus second call returned nil")
	}
	if len(cr.Status.Nodes) != 1 {
		t.Fatalf("should not append duplicate node entry")
	}
	// existing value should be retained
	if s2.MgmtIp != "10.0.0.1" {
		t.Fatalf("expected existing node status to be reused, got %#v", *s2)
	}
}

func TestWaitForActionCompletionUnknownAction(t *testing.T) {
	r := &SimplyBlockStorageNodeReconciler{}
	c := webapi.NewClient("http://127.0.0.1:1")
	err := r.waitForActionCompletion(context.Background(), c, "cluster", "secret", "node", "invalid-action")
	if err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

func TestWaitForActionCompletionValidTransitions(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		statusCode int
		respStatus string
	}{
		{
			name:       "suspend reaches suspended",
			action:     "suspend",
			statusCode: http.StatusOK,
			respStatus: "suspended",
		},
		{
			name:       "resume reaches online",
			action:     "resume",
			statusCode: http.StatusOK,
			respStatus: "online",
		},
		{
			name:       "shutdown reaches offline",
			action:     "shutdown",
			statusCode: http.StatusOK,
			respStatus: "offline",
		},
		{
			name:       "restart reaches online",
			action:     "restart",
			statusCode: http.StatusOK,
			respStatus: "online",
		},
		{
			name:       "remove reaches deleted via 404",
			action:     "remove",
			statusCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.statusCode == http.StatusNotFound {
					w.WriteHeader(http.StatusNotFound)
					return
				}

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(NodeStatusResponse{
					Status: tc.respStatus,
				})
			}))
			defer srv.Close()

			r := &SimplyBlockStorageNodeReconciler{}
			c := webapi.NewClient(srv.URL)
			err := r.waitForActionCompletion(context.Background(), c, "cluster", "secret", "node", tc.action)
			if err != nil {
				t.Fatalf("waitForActionCompletion returned error: %v", err)
			}
		})
	}
}

func TestHandleNodeActionTransitions(t *testing.T) {
	t.Run("does not re-enter terminal success for same action and node", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-a",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:   "restart",
					NodeUUID: "node-1",
					State:    utils.ActionStateSuccess,
				},
			},
		}

		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("handleNodeAction returned error: %v", err)
		}
		if sn.Status.ActionStatus.State != utils.ActionStateSuccess {
			t.Fatalf("expected success to remain stable, got %q", sn.Status.ActionStatus.State)
		}
	})

	t.Run("transitions running to failed when action call fails", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-b",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-2",
			},
		}

		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
		if err == nil {
			t.Fatalf("expected action failure")
		}

		current := &simplyblockv1alpha1.SimplyBlockStorageNode{}
		if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); getErr != nil {
			t.Fatalf("failed to fetch storagenode: %v", getErr)
		}
		if current.Status.ActionStatus == nil {
			t.Fatalf("expected action status")
		}
		if current.Status.ActionStatus.State != utils.ActionStateFailed {
			t.Fatalf("expected failed state, got %q", current.Status.ActionStatus.State)
		}
		if strings.TrimSpace(current.Status.ActionStatus.Message) == "" {
			t.Fatalf("expected failure message to be set")
		}
	})
}

func TestHandleNodeActionRejectsIllegalSuccessIdentity(t *testing.T) {
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-illegal-success",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:   "restart",
			NodeUUID: "node-expected",
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
			// Illegal success for another identity should not be accepted.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:   "restart",
				NodeUUID: "node-other",
				State:    utils.ActionStateSuccess,
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn)
	err := r.handleNodeAction(context.Background(), webapi.NewClient("http://127.0.0.1:1"), sn, "cluster", "secret")
	if err == nil {
		t.Fatalf("expected failure after rejecting illegal success identity")
	}

	current := &simplyblockv1alpha1.SimplyBlockStorageNode{}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); getErr != nil {
		t.Fatalf("failed to fetch storagenode: %v", getErr)
	}
	if current.Status.ActionStatus == nil {
		t.Fatalf("expected action status")
	}
	if current.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected stale/illegal success to be rejected and transitioned to failed, got %q", current.Status.ActionStatus.State)
	}
	if strings.TrimSpace(current.Status.ActionStatus.Message) == "" {
		t.Fatalf("expected failure message to be set")
	}
}

func TestStorageNodeFinalizerLifecycleHelpers(t *testing.T) {
	now := metav1.NewTime(time.Now())

	t.Run("ensureFinalizer adds finalizer when missing", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-finalizer-add",
				Namespace: "default",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		updated, err := r.ensureFinalizer(context.Background(), sn)
		if err != nil {
			t.Fatalf("ensureFinalizer returned error: %v", err)
		}
		if !updated {
			t.Fatalf("expected ensureFinalizer to report update")
		}
		if !contains(sn.Finalizers, "simplyblock.storagenode.finalizer") {
			t.Fatalf("expected storagenode finalizer to be set")
		}
	})

	t.Run("handleDeletion removes finalizer when deletion timestamp is set", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "sn-finalizer-del",
				Namespace:         "default",
				Finalizers:        []string{"simplyblock.storagenode.finalizer"},
				DeletionTimestamp: &now,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		updated, err := r.handleDeletion(context.Background(), sn)
		if err != nil {
			t.Fatalf("handleDeletion returned error: %v", err)
		}
		if !updated {
			t.Fatalf("expected handleDeletion to report update")
		}
		if contains(sn.Finalizers, "simplyblock.storagenode.finalizer") {
			t.Fatalf("expected storagenode finalizer to be removed")
		}
	})
}

func TestStorageNodeLabelingHelpers(t *testing.T) {
	t.Run("labelWorkerNodes labels all configured workers", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-label-all",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-a",
				WorkerNodes: []string{"node-a", "node-b"},
			},
		}
		nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
		nodeB := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
		r := newStorageNodeStateTestReconciler(t, sn, nodeA, nodeB)

		if err := r.labelWorkerNodes(context.Background(), sn); err != nil {
			t.Fatalf("labelWorkerNodes returned error: %v", err)
		}

		for _, nodeName := range []string{"node-a", "node-b"} {
			var n corev1.Node
			if err := r.Get(context.Background(), client.ObjectKey{Name: nodeName}, &n); err != nil {
				t.Fatalf("failed to fetch node %s: %v", nodeName, err)
			}
			got := n.Labels["io.simplyblock.node-type"]
			want := "simplyblock-storage-plane-cluster-a"
			if got != want {
				t.Fatalf("node %s label mismatch: got %q want %q", nodeName, got, want)
			}
		}
	})

	t.Run("labelWorkerNode labels single worker node", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-label-one",
				Namespace: "default",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-b",
				WorkerNode:  "node-one",
			},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-one"}}
		r := newStorageNodeStateTestReconciler(t, sn, node)

		if err := r.labelWorkerNode(context.Background(), sn); err != nil {
			t.Fatalf("labelWorkerNode returned error: %v", err)
		}

		var out corev1.Node
		if err := r.Get(context.Background(), client.ObjectKey{Name: "node-one"}, &out); err != nil {
			t.Fatalf("failed to fetch node: %v", err)
		}
		if out.Labels["io.simplyblock.node-type"] != "simplyblock-storage-plane-cluster-b" {
			t.Fatalf("expected worker node label to be set")
		}
	})
}

func TestStorageNodeDaemonSetReconcile(t *testing.T) {
	t.Run("creates daemonset when missing", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-ds-create",
				Namespace: "default",
				UID:       "uid-create",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-a",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
			t.Fatalf("reconcileDaemonSet returned error: %v", err)
		}

		var ds appsv1.DaemonSet
		if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
			t.Fatalf("daemonset should be created: %v", err)
		}
		if len(ds.OwnerReferences) == 0 || ds.OwnerReferences[0].Name != sn.Name {
			t.Fatalf("expected daemonset to be owned by storagenode")
		}
	})

	t.Run("updates existing daemonset", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sn-ds-update",
				Namespace: "default",
				UID:       "uid-update",
			},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-a",
			},
		}
		existing := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "simplyblock-storage-node-ds-cluster-a",
				Namespace: "default",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn, existing)

		if err := r.reconcileDaemonSet(context.Background(), sn); err != nil {
			t.Fatalf("reconcileDaemonSet returned error: %v", err)
		}

		var ds appsv1.DaemonSet
		if err := r.Get(context.Background(), client.ObjectKey{Name: "simplyblock-storage-node-ds-cluster-a", Namespace: "default"}, &ds); err != nil {
			t.Fatalf("failed to fetch daemonset: %v", err)
		}
		if len(ds.OwnerReferences) == 0 || ds.OwnerReferences[0].Name != sn.Name {
			t.Fatalf("expected updated daemonset to carry owner reference")
		}
	})
}

func TestGetNodeInternalIP(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-ip"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-ip"},
				{Type: corev1.NodeInternalIP, Address: "10.1.2.3"},
			},
		},
	}
	r := newStorageNodeStateTestReconciler(t, node)

	got, err := getNodeInternalIP(context.Background(), r.Client, "node-ip")
	if err != nil {
		t.Fatalf("getNodeInternalIP returned error: %v", err)
	}
	if got != "10.1.2.3" {
		t.Fatalf("expected internal IP 10.1.2.3, got %q", got)
	}
}

func TestGetNodeInternalIPNoAddress(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-no-ip"},
	}
	r := newStorageNodeStateTestReconciler(t, node)

	_, err := getNodeInternalIP(context.Background(), r.Client, "node-no-ip")
	if err == nil {
		t.Fatalf("expected error when node has no internal IP")
	}
}

func TestStorageNodeReconcileActionFastPaths(t *testing.T) {
	t.Run("reconcileAction returns no requeue when action already successful", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-ra-ok", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-1",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:   "restart",
					NodeUUID: "node-1",
					State:    utils.ActionStateSuccess,
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.reconcileAction(context.Background(), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileAction returned error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected no delayed requeue for successful action, got %+v", res)
		}
	})

	t.Run("reconcileAction requeues on action failure", func(t *testing.T) {
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-ra-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "restart",
				NodeUUID: "node-2",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		res, err := r.reconcileAction(context.Background(), sn, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileAction returned unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("expected delayed requeue after failed action, got %+v", res)
		}
	})
}

func TestStorageNodeHandleDeletionNoopWithoutDeletionTimestamp(t *testing.T) {
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-no-delete",
			Namespace: "default",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	updated, err := r.handleDeletion(context.Background(), sn)
	if err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}
	if updated {
		t.Fatalf("expected no update when deletion timestamp is zero")
	}
}

func TestStorageNodeHandleDeletionDoneWithoutFinalizer(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sn-delete-done",
			Namespace:         "default",
			DeletionTimestamp: &now,
		},
	}
	r := newStorageNodeStateTestReconciler(t)

	updated, err := r.handleDeletion(context.Background(), sn)
	if err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}
	if !updated {
		t.Fatalf("expected deletion flow to be treated as handled without finalizer")
	}
}

func TestStorageNodeReconcileClusterUnavailableRequeues(t *testing.T) {
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-reconcile-no-cluster",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: "cluster-missing",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster UUID is unavailable")
	}
}

func TestStorageNodeReconcileSecretMissingRequeues(t *testing.T) {
	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: "cluster-a"},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: "cluster-uuid-no-secret"},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-reconcile-no-secret",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: "cluster-a",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, cluster)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when cluster secret is unavailable")
	}
}

func TestStorageNodeReconcileNotFoundReturnsNil(t *testing.T) {
	r := newStorageNodeStateTestReconciler(t)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for missing object, got %+v", res)
	}
}

func TestStorageNodeReconcileDeletionFlow(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-del"
	const clusterUUID = "cluster-uuid-del"
	now := metav1.NewTime(time.Now())

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-del", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sn-delete-flow",
			Namespace:         namespace,
			Finalizers:        []string{"simplyblock.storagenode.finalizer"},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected deletion flow to complete without requeue, got %+v", res)
	}

	current := &simplyblockv1alpha1.SimplyBlockStorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to fetch storagenode: %v", err)
		}
		return
	}
	if contains(current.Finalizers, "simplyblock.storagenode.finalizer") {
		t.Fatalf("expected finalizer to be removed during deletion flow")
	}
}

func TestStorageNodeReconcileAddsFinalizer(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-finalizer"
	const clusterUUID = "cluster-uuid-finalizer"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-finalizer", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sn-finalizer-flow",
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected finalizer add path to return without requeue, got %+v", res)
	}

	current := &simplyblockv1alpha1.SimplyBlockStorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		t.Fatalf("failed to fetch storagenode: %v", err)
	}
	if !contains(current.Finalizers, "simplyblock.storagenode.finalizer") {
		t.Fatalf("expected finalizer to be added by reconcile")
	}
}

func TestStorageNodeReconcileActionPath(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-action"
	const clusterUUID = "cluster-uuid-action"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-action", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-action-flow",
			Namespace:  namespace,
			Finalizers: []string{"simplyblock.storagenode.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
			Action:      "restart",
			NodeUUID:    "node-action-1",
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:   "restart",
				NodeUUID: "node-action-1",
				State:    utils.ActionStateSuccess,
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected action fast-path to avoid delayed requeue, got %+v", res)
	}
}

func TestStorageNodeReconcileLabelWorkerNodesFailure(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-label-fail"
	const clusterUUID = "cluster-uuid-label-fail"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-label-fail", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-label-fail",
			Namespace:  namespace,
			Finalizers: []string{"simplyblock.storagenode.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{"missing-worker"},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err == nil {
		t.Fatalf("expected reconcile to fail when worker node lookup fails")
	}
}

func TestStorageNodeReconcileKnownWorkerSkipsProvisioning(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-known-worker"
	const clusterUUID = "cluster-uuid-known-worker"
	const workerName = "node-known"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-known-worker", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-known-worker",
			Namespace:  namespace,
			Finalizers: []string{"simplyblock.storagenode.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
		Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{
					Hostname: workerName,
					MgmtIp:   "10.0.0.10",
					Status:   "online",
					UUID:     "node-uuid-known",
				},
			},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no delayed requeue when worker already known, got %+v", res)
	}
}

func TestStorageNodeReconcileMissingInternalIPRequeues(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-missing-ip"
	const clusterUUID = "cluster-uuid-missing-ip"
	const workerName = "node-no-ip"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-missing-ip", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-missing-ip",
			Namespace:  namespace,
			Finalizers: []string{"simplyblock.storagenode.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when worker has no internal IP")
	}
}

func TestStorageNodeReconcileUnreachableNodeInfoRequeues(t *testing.T) {
	const namespace = "default"
	const clusterName = "cluster-unreachable-info"
	const clusterUUID = "cluster-uuid-unreachable-info"
	const workerName = "node-bad-ip"

	cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-unreachable-info", Namespace: namespace},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: clusterUUID},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + clusterName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(clusterUUID),
			"secret": []byte("s3cr3t"),
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerName},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeInternalIP,
					Address: "bad ip",
				},
			},
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sn-unreachable-info",
			Namespace:  namespace,
			Finalizers: []string{"simplyblock.storagenode.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			ClusterName: clusterName,
			WorkerNodes: []string{workerName},
		},
	}

	r := newStorageNodeStateTestReconciler(t, sn, cluster, secret, node)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sn)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue when node info endpoint is unreachable")
	}
}

func TestCheckNodeInfoReachable(t *testing.T) {
	// checkNodeInfoReachable always probes http://<ip>:5000/snode/info.
	// Use an unroutable test-net address to deterministically exercise error path.
	err := checkNodeInfoReachable(context.Background(), "192.0.2.1")
	if err == nil {
		t.Fatalf("expected error when node info endpoint is unreachable")
	}
}

func TestWaitForNodeInfoReachable(t *testing.T) {
	origCheckFn := waitForNodeInfoReachableCheckFn
	origRetries := waitForNodeInfoReachableMaxRetries
	origDelay := waitForNodeInfoReachableRetryDelay
	t.Cleanup(func() {
		waitForNodeInfoReachableCheckFn = origCheckFn
		waitForNodeInfoReachableMaxRetries = origRetries
		waitForNodeInfoReachableRetryDelay = origDelay
	})

	t.Run("returns nil on first successful check", func(t *testing.T) {
		attempts := 0
		waitForNodeInfoReachableMaxRetries = 3
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string) error {
			attempts++
			return nil
		}

		if err := waitForNodeInfoReachable(context.Background(), "10.0.0.1", "node-a"); err != nil {
			t.Fatalf("waitForNodeInfoReachable returned error: %v", err)
		}
		if attempts != 1 {
			t.Fatalf("expected one attempt, got %d", attempts)
		}
	})

	t.Run("retries and then succeeds", func(t *testing.T) {
		attempts := 0
		waitForNodeInfoReachableMaxRetries = 4
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string) error {
			attempts++
			if attempts < 3 {
				return errors.New("temporary failure")
			}
			return nil
		}

		if err := waitForNodeInfoReachable(context.Background(), "10.0.0.2", "node-b"); err != nil {
			t.Fatalf("waitForNodeInfoReachable returned error: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("expected three attempts, got %d", attempts)
		}
	})

	t.Run("returns context cancellation", func(t *testing.T) {
		waitForNodeInfoReachableMaxRetries = 5
		waitForNodeInfoReachableRetryDelay = time.Second
		waitForNodeInfoReachableCheckFn = func(context.Context, string) error {
			return errors.New("still down")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := waitForNodeInfoReachable(ctx, "10.0.0.3", "node-c")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled error, got %v", err)
		}
	})

	t.Run("returns wrapped error after max retries", func(t *testing.T) {
		waitForNodeInfoReachableMaxRetries = 3
		waitForNodeInfoReachableRetryDelay = time.Millisecond
		waitForNodeInfoReachableCheckFn = func(context.Context, string) error {
			return errors.New("permanent failure")
		}

		err := waitForNodeInfoReachable(context.Background(), "10.0.0.4", "node-d")
		if err == nil {
			t.Fatalf("expected timeout error after retries")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("after %d retries", waitForNodeInfoReachableMaxRetries)) {
			t.Fatalf("unexpected retry error message: %v", err)
		}
		if !strings.Contains(err.Error(), "permanent failure") {
			t.Fatalf("expected wrapped failure message, got: %v", err)
		}
	})
}

func TestWaitForNodeOnlinePaths(t *testing.T) {
	t.Run("updates node status and exits when cluster already active", func(t *testing.T) {
		const clusterName = "cluster-a"
		const clusterUUID = "cluster-uuid-online"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"uuid":"node-uuid-1",
						"status":"online",
						"mgmt_ip":"10.0.0.1",
						"health_check":true,
						"hostname":"node-a",
						"online_devices":"nvme0n1",
						"cpu":4,
						"spdk_mem":2147483648,
						"lvols":3,
						"rpc_port":9000,
						"lvol_subsys_port":9001,
						"nvmf_port":9002
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		apiClient := webapi.NewClient(mock.URL())

		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr", Namespace: "default"},
			Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
				Status: "active",
				MOD:    "1x0",
			},
		}
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-online", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: clusterName,
				WorkerNodes: []string{"node-a"},
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-a", MgmtIp: "10.0.0.1", Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, cluster, sn)

		err := waitForNodeOnline(
			context.Background(),
			apiClient,
			"secret",
			clusterUUID,
			"10.0.0.1",
			"node-a",
			sn,
			r,
		)
		if err != nil {
			t.Fatalf("waitForNodeOnline returned error: %v", err)
		}

		if len(sn.Status.Nodes) != 1 {
			t.Fatalf("unexpected node status length: %d", len(sn.Status.Nodes))
		}
		got := sn.Status.Nodes[0]
		if got.Status != "online" || got.UUID != "node-uuid-1" {
			t.Fatalf("node status not updated as expected: %#v", got)
		}
	})

	t.Run("returns invariant error when node missing in status list", func(t *testing.T) {
		const clusterName = "cluster-b"
		const clusterUUID = "cluster-uuid-missing-status"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"uuid":"node-uuid-2",
						"status":"online",
						"mgmt_ip":"10.0.0.2",
						"health_check":true,
						"hostname":"node-b",
						"online_devices":"nvme0n2",
						"cpu":8,
						"spdk_mem":4294967296,
						"lvols":1,
						"rpc_port":9100,
						"lvol_subsys_port":9101,
						"nvmf_port":9102
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)
		apiClient := webapi.NewClient(mock.URL())

		cluster := &simplyblockv1alpha1.SimplyBlockStorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-cr-b", Namespace: "default"},
			Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: clusterName},
			Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{
				Status: "active",
				MOD:    "1x0",
			},
		}
		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-missing-status", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: clusterName,
				WorkerNodes: []string{"node-b"},
			},
		}
		r := newStorageNodeStateTestReconciler(t, cluster, sn)

		err := waitForNodeOnline(
			context.Background(),
			apiClient,
			"secret",
			clusterUUID,
			"10.0.0.2",
			"node-b",
			sn,
			r,
		)
		if err == nil {
			t.Fatalf("expected invariant violation error for missing node status entry")
		}
		if !strings.Contains(err.Error(), "missing from status") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestWaitForNodeOnlineErrorAndTimeoutPaths(t *testing.T) {
	origRetries := waitForNodeOnlineRetries
	origWait := waitForNodeOnlineWaitInterval
	origActivationDelay := waitForNodeOnlineActivationDelay
	origSleepFn := waitForNodeOnlineSleepFn
	t.Cleanup(func() {
		waitForNodeOnlineRetries = origRetries
		waitForNodeOnlineWaitInterval = origWait
		waitForNodeOnlineActivationDelay = origActivationDelay
		waitForNodeOnlineSleepFn = origSleepFn
	})

	waitForNodeOnlineRetries = 1
	waitForNodeOnlineWaitInterval = 0
	waitForNodeOnlineActivationDelay = 0
	waitForNodeOnlineSleepFn = func(time.Duration) {}

	t.Run("returns error on invalid storage-node payload", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-invalid-json"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `{`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-invalid-json", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-a",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-a", MgmtIp: "10.0.0.1", Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		err := waitForNodeOnline(
			context.Background(),
			webapi.NewClient(mock.URL()),
			"secret",
			clusterUUID,
			"10.0.0.1",
			"node-a",
			sn,
			r,
		)
		if err == nil {
			t.Fatalf("expected unmarshal error for invalid payload")
		}
		if !strings.Contains(err.Error(), "failed to unmarshal") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("returns cluster-not-found when activation precheck cannot resolve cluster CR", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-cluster-missing"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body: `[
					{
						"uuid":"node-uuid-3",
						"status":"online",
						"mgmt_ip":"10.0.0.3",
						"health_check":true,
						"hostname":"node-c",
						"online_devices":"nvme1n1",
						"cpu":4,
						"spdk_mem":2147483648,
						"lvols":1,
						"rpc_port":9200,
						"lvol_subsys_port":9201,
						"nvmf_port":9202
					}
				]`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		)

		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-cluster-missing", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-missing",
			},
			Status: simplyblockv1alpha1.SimplyBlockStorageNodeStatus{
				Nodes: []simplyblockv1alpha1.NodeStatus{
					{Hostname: "node-c", MgmtIp: "10.0.0.3", Status: "in_creation"},
				},
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		err := waitForNodeOnline(
			context.Background(),
			webapi.NewClient(mock.URL()),
			"secret",
			clusterUUID,
			"10.0.0.3",
			"node-c",
			sn,
			r,
		)
		if err == nil {
			t.Fatalf("expected cluster resolution error")
		}
		if !strings.Contains(err.Error(), "cluster not found yet") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("returns timeout and writes timeout node status", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-wfno-timeout"
		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodGet,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/",
			webapimock.RouteResponse{
				Status: http.StatusOK,
				Body:   `[]`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-wfno-timeout", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				ClusterName: "cluster-a",
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)

		err := waitForNodeOnline(
			context.Background(),
			webapi.NewClient(mock.URL()),
			"secret",
			clusterUUID,
			"10.0.0.4",
			"node-timeout",
			sn,
			r,
		)
		if err == nil {
			t.Fatalf("expected timeout error")
		}
		if !strings.Contains(err.Error(), "did not become online in time") {
			t.Fatalf("unexpected timeout error: %v", err)
		}
		if len(sn.Status.Nodes) != 1 {
			t.Fatalf("expected timeout status node entry, got %d", len(sn.Status.Nodes))
		}
		if sn.Status.Nodes[0].Hostname != "node-timeout" || sn.Status.Nodes[0].Status != "timeout" {
			t.Fatalf("unexpected timeout node status: %#v", sn.Status.Nodes[0])
		}
	})
}

func TestWaitForActionCompletionRetryBehavior(t *testing.T) {
	origRetries := waitForActionCompletionRetries
	origWait := waitForActionCompletionWaitInterval
	origSleepFn := waitForActionCompletionSleepFn
	t.Cleanup(func() {
		waitForActionCompletionRetries = origRetries
		waitForActionCompletionWaitInterval = origWait
		waitForActionCompletionSleepFn = origSleepFn
	})

	waitForActionCompletionRetries = 4
	waitForActionCompletionWaitInterval = 0
	waitForActionCompletionSleepFn = func(time.Duration) {}

	t.Run("returns terminal error when target status never reached", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(NodeStatusResponse{
				Status: "creating",
			})
		}))
		defer srv.Close()

		r := &SimplyBlockStorageNodeReconciler{}
		err := r.waitForActionCompletion(
			context.Background(),
			webapi.NewClient(srv.URL),
			"cluster-a",
			"secret",
			"node-a",
			"restart",
		)
		if err == nil {
			t.Fatalf("expected terminal status error")
		}
		if !strings.Contains(err.Error(), "did not reach expected status") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("recovers from transient API and payload errors before success", func(t *testing.T) {
		attempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			switch attempts {
			case 1:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"temporary"}`))
			case 2:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{`))
			default:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(NodeStatusResponse{
					Status: "online",
				})
			}
		}))
		defer srv.Close()

		r := &SimplyBlockStorageNodeReconciler{}
		err := r.waitForActionCompletion(
			context.Background(),
			webapi.NewClient(srv.URL),
			"cluster-b",
			"secret",
			"node-b",
			"restart",
		)
		if err != nil {
			t.Fatalf("expected eventual success, got error: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts)
		}
	})
}

func TestPerformNodeActionRemoveHappyPath(t *testing.T) {
	const clusterUUID = "cluster-uuid-remove"
	const nodeUUID = "node-uuid-remove"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodDelete,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusNotFound},
	)
	apiClient := webapi.NewClient(mock.URL())

	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-remove", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:   "remove",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	if err := r.performNodeAction(context.Background(), apiClient, clusterUUID, "secret", sn); err != nil {
		t.Fatalf("performNodeAction(remove) returned error: %v", err)
	}
}

func TestPerformNodeActionRestartWorkerNodeLabelFailure(t *testing.T) {
	const clusterUUID = "cluster-uuid-restart-label-fail"
	const nodeUUID = "node-uuid-restart-label-fail"

	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-label-fail", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:     "restart",
			NodeUUID:   nodeUUID,
			WorkerNode: "missing-node",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	err := r.performNodeAction(
		context.Background(),
		webapi.NewClient("http://127.0.0.1:1"),
		clusterUUID,
		"secret",
		sn,
	)
	if err == nil {
		t.Fatalf("expected restart action to fail when worker node lookup fails")
	}
	if !strings.Contains(err.Error(), "failed to label worker node") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPerformNodeActionRestartWorkerNodeMissingInternalIP(t *testing.T) {
	const clusterUUID = "cluster-uuid-restart-ip-fail"
	const nodeUUID = "node-uuid-restart-ip-fail"
	const workerNode = "node-no-ip"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerNode},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-ip-fail", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:      "restart",
			NodeUUID:    nodeUUID,
			WorkerNode:  workerNode,
			ClusterName: "cluster-a",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, node)

	err := r.performNodeAction(
		context.Background(),
		webapi.NewClient("http://127.0.0.1:1"),
		clusterUUID,
		"secret",
		sn,
	)
	if err == nil {
		t.Fatalf("expected restart action to fail when worker node has no InternalIP")
	}
	if !strings.Contains(err.Error(), "has no InternalIP") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPerformNodeActionRestartWorkerNodeReachabilityCanceled(t *testing.T) {
	const clusterUUID = "cluster-uuid-restart-cancel"
	const nodeUUID = "node-uuid-restart-cancel"
	const workerNode = "node-cancel"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: workerNode},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.99"},
			},
		},
	}
	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-cancel", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:      "restart",
			NodeUUID:    nodeUUID,
			WorkerNode:  workerNode,
			ClusterName: "cluster-a",
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn, node)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.performNodeAction(
		ctx,
		webapi.NewClient("http://127.0.0.1:1"),
		clusterUUID,
		"secret",
		sn,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation error from restart reachability path, got %v", err)
	}
}

func TestPerformNodeActionAPIFailure(t *testing.T) {
	t.Run("restart returns API failure on non-2xx", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-restart-api-fail"
		const nodeUUID = "node-uuid-restart-api-fail"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/restart",
			webapimock.RouteResponse{
				Status: http.StatusInternalServerError,
				Body:   `{"error":"boom"}`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-restart-api-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "restart",
				NodeUUID: nodeUUID,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn)
		if err == nil {
			t.Fatalf("expected restart API failure")
		}
		if !strings.Contains(err.Error(), "action API failed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("default action endpoint returns API failure on non-2xx", func(t *testing.T) {
		const clusterUUID = "cluster-uuid-default-api-fail"
		const nodeUUID = "node-uuid-default-api-fail"

		mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
		defer mock.Close()
		mock.Register(
			http.MethodPost,
			"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/suspend?force=true",
			webapimock.RouteResponse{
				Status: http.StatusBadGateway,
				Body:   `{"error":"upstream failed"}`,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
		)

		sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "sn-default-api-fail", Namespace: "default"},
			Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
				Action:   "suspend",
				NodeUUID: nodeUUID,
			},
		}
		r := newStorageNodeStateTestReconciler(t, sn)
		err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn)
		if err == nil {
			t.Fatalf("expected default-action API failure")
		}
		if !strings.Contains(err.Error(), "action API failed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestPerformNodeActionDefaultActionSuccess(t *testing.T) {
	const clusterUUID = "cluster-uuid-default-success"
	const nodeUUID = "node-uuid-default-success"

	origDelay := performNodeActionPostTriggerDelay
	origSleep := performNodeActionSleepFn
	t.Cleanup(func() {
		performNodeActionPostTriggerDelay = origDelay
		performNodeActionSleepFn = origSleep
	})
	performNodeActionPostTriggerDelay = 0
	performNodeActionSleepFn = func(time.Duration) {}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID+"/suspend",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `{}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `{"status":"suspended"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-default-success", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:   "suspend",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)
	if err := r.performNodeAction(context.Background(), webapi.NewClient(mock.URL()), clusterUUID, "secret", sn); err != nil {
		t.Fatalf("performNodeAction(default suspend) returned error: %v", err)
	}
}

func TestHandleNodeActionTransitionsToSuccess(t *testing.T) {
	const clusterUUID = "cluster-uuid-action-success"
	const nodeUUID = "node-uuid-action-success"

	origDelay := performNodeActionPostTriggerDelay
	origSleep := performNodeActionSleepFn
	t.Cleanup(func() {
		performNodeActionPostTriggerDelay = origDelay
		performNodeActionSleepFn = origSleep
	})
	performNodeActionPostTriggerDelay = 0
	performNodeActionSleepFn = func(time.Duration) {}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(
		http.MethodDelete,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-nodes/"+nodeUUID,
		webapimock.RouteResponse{Status: http.StatusNotFound},
	)

	sn := &simplyblockv1alpha1.SimplyBlockStorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-action-success", Namespace: "default"},
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			Action:   "remove",
			NodeUUID: nodeUUID,
		},
	}
	r := newStorageNodeStateTestReconciler(t, sn)

	if err := r.handleNodeAction(context.Background(), webapi.NewClient(mock.URL()), sn, clusterUUID, "secret"); err != nil {
		t.Fatalf("handleNodeAction returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockStorageNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), current); err != nil {
		t.Fatalf("failed to fetch storagenode: %v", err)
	}
	if current.Status.ActionStatus == nil {
		t.Fatalf("expected action status to be set")
	}
	if current.Status.ActionStatus.State != utils.ActionStateSuccess {
		t.Fatalf("expected success action state, got %q", current.Status.ActionStatus.State)
	}
}

func newStorageNodeStateTestReconciler(
	t *testing.T,
	objects ...client.Object,
) *SimplyBlockStorageNodeReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add API scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add appsv1 scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add rbacv1 scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.SimplyBlockStorageNode{},
			&simplyblockv1alpha1.SimplyBlockStorageCluster{},
			&appsv1.DaemonSet{},
		).
		WithObjects(objects...).
		Build()

	return &SimplyBlockStorageNodeReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
