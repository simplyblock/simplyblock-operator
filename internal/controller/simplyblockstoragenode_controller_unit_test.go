package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func newStorageNodeStateTestReconciler(
	t *testing.T,
	objects ...client.Object,
) *SimplyBlockStorageNodeReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add API scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.SimplyBlockStorageNode{}).
		WithObjects(objects...).
		Build()

	return &SimplyBlockStorageNodeReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
