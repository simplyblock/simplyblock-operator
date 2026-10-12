package cpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/twonode"
)

// TestNoPreferredNodeCallToTheControlPlane runs the arbitration controller
// through the real adapter and HTTP client against a fake control plane. The
// spec still carries the deprecated preferredNode and the record still carries
// preferred_node (an older control plane); the operator must read the record
// and never call PUT .../arbitration/preferred.
func TestNoPreferredNodeCallToTheControlPlane(t *testing.T) {
	const ns = "simplyblock"
	var (
		mu    sync.Mutex
		calls []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/clusters/c-uuid/arbitration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cluster_id": "c-uuid", "epoch": 8, "state": "partitioned", "preferred_node": "id-a",
				"taint_requests": []string{"id-b"}, "enabled": true,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v2/clusters/c-uuid/arbitration/remediation":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	cluster := testsupport.Cluster(ns, "edge", "c-uuid")
	cluster.Spec.ClusterMode = simplyblockv1alpha2.ClusterModeDual
	cluster.Spec.EnableArbitration = ptr.To(true)
	objs := make([]client.Object, 0, 5)
	objs = append(objs, cluster)
	for _, w := range []struct{ sn, worker, id string }{{"sn-a", "worker-a", "id-a"}, {"sn-b", "worker-b", "id-b"}} {
		objs = append(objs,
			&simplyblockv1alpha2.StorageNode{
				ObjectMeta: metav1.ObjectMeta{Name: w.sn, Namespace: ns},
				Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "edge", WorkerNode: w.worker},
				Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: w.id},
			},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.worker},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	}
	scheme := testsupport.NewScheme(t, clientgoscheme.AddToScheme)
	c := testsupport.NewClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.StorageCluster{}, &simplyblockv1alpha2.StorageNode{}}, objs...)

	r := &twonode.ArbitrationReconciler{Client: c, Scheme: scheme,
		API: &Adapter{Reader: c, EndpointResolver: func(context.Context) string { return srv.URL }}}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "edge"}}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	sawGet := false
	for _, call := range calls {
		if strings.HasSuffix(call, "/arbitration/preferred") {
			t.Fatalf("the operator called the deprecated preferred-node endpoint: %v", calls)
		}
		if call == "GET /api/v2/clusters/c-uuid/arbitration" {
			sawGet = true
		}
	}
	if !sawGet {
		t.Fatalf("the record was never read: %v", calls)
	}
	var node corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-b"}, &node); err != nil {
		t.Fatal(err)
	}
	fenced := false
	for _, tt := range node.Spec.Taints {
		if tt.Key == simplyblockv1alpha2.TaintStorageFenced && tt.Value == "8" {
			fenced = true
		}
	}
	if !fenced {
		t.Fatalf("the arbiter's fence was not applied: %+v (calls %v)", node.Spec.Taints, calls)
	}
}
