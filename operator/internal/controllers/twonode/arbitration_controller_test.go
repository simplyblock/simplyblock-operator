package twonode

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const ns = "simplyblock"

type fakeAPI struct {
	rec       *Record
	err       error
	preferred string
}

func (f *fakeAPI) Get(context.Context, *simplyblockv1alpha2.StorageCluster) (*Record, error) {
	return f.rec, f.err
}

func (f *fakeAPI) SetPreferred(_ context.Context, _ *simplyblockv1alpha2.StorageCluster, id string) error {
	f.preferred = id
	return nil
}

func setup(t *testing.T, api *fakeAPI, extraTaint *corev1.Taint) (*ArbitrationReconciler, client.Client) {
	t.Helper()
	cluster := testsupport.Cluster(ns, "edge", "c-uuid")
	cluster.Spec.TwoNode = &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true, PreferredNode: "worker-a"}
	objs := []client.Object{cluster}
	for _, w := range []struct{ sn, worker, id string }{{"sn-a", "worker-a", "id-a"}, {"sn-b", "worker-b", "id-b"}} {
		objs = append(objs, &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: w.sn, Namespace: ns},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "edge", WorkerNode: w.worker},
			Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: w.id},
		})
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.worker},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if extraTaint != nil && w.worker == "worker-b" {
			node.Spec.Taints = []corev1.Taint{*extraTaint}
		}
		objs = append(objs, node)
	}
	scheme := testsupport.NewScheme(t, clientgoscheme.AddToScheme)
	c := testsupport.NewClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.StorageCluster{}, &simplyblockv1alpha2.StorageNode{}}, objs...)
	fixed := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return &ArbitrationReconciler{Client: c, Scheme: scheme, API: api, Now: func() time.Time { return fixed }}, c
}

func reconcile(t *testing.T, r *ArbitrationReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "edge"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func fencedTaint(t *testing.T, c client.Client, worker string) *corev1.Taint {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: worker}, &n); err != nil {
		t.Fatal(err)
	}
	for i := range n.Spec.Taints {
		if n.Spec.Taints[i].Key == simplyblockv1alpha2.TaintStorageFenced {
			return &n.Spec.Taints[i]
		}
	}
	return nil
}

func TestVerdictTaintsTheFencedNode(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "partitioned", Epoch: 8, PreferredNode: "id-a", FencedNodes: []string{"id-b"}}}
	r, c := setup(t, api, nil)
	if res := reconcile(t, r); res.RequeueAfter != requeueActive {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	tb := fencedTaint(t, c, "worker-b")
	if tb == nil || tb.Value != "8" || tb.Effect != corev1.TaintEffectNoExecute {
		t.Fatalf("worker-b taint %+v", tb)
	}
	if fencedTaint(t, c, "worker-a") != nil {
		t.Fatal("the winner must not be tainted")
	}
	var sn simplyblockv1alpha2.StorageNode
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sn-b"}, &sn)
	if sn.Status.Remediation == nil || sn.Status.Remediation.StorageFencedEpoch != 8 {
		t.Fatalf("remediation %+v", sn.Status.Remediation)
	}
	var cl simplyblockv1alpha2.StorageCluster
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "edge"}, &cl)
	if a := cl.Status.Arbitration; a == nil || a.State != "partitioned" || len(a.FencedNodes) != 1 || a.FencedNodes[0] != "worker-b" {
		t.Fatalf("status %+v", a)
	}
}

func TestTaintStaysThroughHealingAndLeavesWhenSteady(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "healing", Epoch: 9, PreferredNode: "id-a", FencedNodes: []string{"id-b"}}}
	r, c := setup(t, api, nil)
	reconcile(t, r)
	if fencedTaint(t, c, "worker-b") == nil {
		t.Fatal("healing must keep the taint")
	}
	api.rec = &Record{State: "steady", Epoch: 9, PreferredNode: "id-a"}
	if res := reconcile(t, r); res.RequeueAfter != requeueSteady {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	if fencedTaint(t, c, "worker-b") != nil {
		t.Fatal("steady must remove the taint")
	}
}

func TestIdempotent(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "partitioned", Epoch: 8, PreferredNode: "id-a", FencedNodes: []string{"id-b"}}}
	r, c := setup(t, api, nil)
	reconcile(t, r)
	var before corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: "worker-b"}, &before)
	reconcile(t, r)
	var after corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: "worker-b"}, &after)
	if before.ResourceVersion != after.ResourceVersion || len(after.Spec.Taints) != 1 {
		t.Fatalf("second reconcile changed the node: %s -> %s, %d taints", before.ResourceVersion, after.ResourceVersion, len(after.Spec.Taints))
	}
}

func TestControlPlaneUnreachableChangesNothing(t *testing.T) {
	existing := &corev1.Taint{Key: simplyblockv1alpha2.TaintStorageFenced, Value: "7", Effect: corev1.TaintEffectNoExecute}
	r, c := setup(t, &fakeAPI{err: errors.New("connection refused")}, existing)
	if res := reconcile(t, r); res.RequeueAfter != requeueError {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	if tb := fencedTaint(t, c, "worker-b"); tb == nil || tb.Value != "7" {
		t.Fatalf("an unreadable record must leave the taint: %+v", tb)
	}
}

func TestOutOfServiceIsReportedNeverSet(t *testing.T) {
	oos := &corev1.Taint{Key: simplyblockv1alpha2.TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	api := &fakeAPI{rec: &Record{State: "steady", Epoch: 3, PreferredNode: "id-a"}}
	r, c := setup(t, api, oos)
	reconcile(t, r)
	var sn simplyblockv1alpha2.StorageNode
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sn-b"}, &sn)
	if sn.Status.Remediation == nil || !sn.Status.Remediation.OutOfService {
		t.Fatalf("remediation %+v", sn.Status.Remediation)
	}
	var a corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: "worker-a"}, &a)
	for _, tt := range a.Spec.Taints {
		if tt.Key == simplyblockv1alpha2.TaintOutOfService {
			t.Fatal("the operator must never set out-of-service")
		}
	}
}

func TestPreferredNodeIsPushed(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "steady", Epoch: 1, PreferredNode: "id-b"}}
	r, _ := setup(t, api, nil)
	reconcile(t, r)
	if api.preferred != "id-a" {
		t.Fatalf("preferred %q", api.preferred)
	}
}

func TestArbitrationOffRemovesTaints(t *testing.T) {
	existing := &corev1.Taint{Key: simplyblockv1alpha2.TaintStorageFenced, Value: "7", Effect: corev1.TaintEffectNoExecute}
	r, c := setup(t, &fakeAPI{}, existing)
	var cl simplyblockv1alpha2.StorageCluster
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "edge"}, &cl)
	cl.Spec.TwoNode.Arbitration = false
	if err := c.Update(context.Background(), &cl); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if fencedTaint(t, c, "worker-b") != nil {
		t.Fatal("arbitration off must withdraw the taint")
	}
}
