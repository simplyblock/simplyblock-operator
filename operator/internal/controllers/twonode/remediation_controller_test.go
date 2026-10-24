// The StorageFenceRemediation reconciler: the out-of-service taint goes on only
// while the control plane holds the node's host fence and no foreign volume is
// attached, and comes off only where this remediation put it.

package twonode

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

type remSetup struct {
	fenced      bool // worker-b carries StorageFenced=True
	hostFencing bool
	extra       []client.Object
}

func remediationSetup(t *testing.T, api *fakeAPI, s remSetup) (*RemediationReconciler, client.Client) {
	t.Helper()
	cluster := testsupport.Cluster(ns, "edge", "c-uuid")
	cluster.Spec.ClusterMode = simplyblockv1alpha2.ClusterModeDual
	cluster.Spec.EnableArbitration = ptr.To(true)
	cluster.Spec.Arbitration = &simplyblockv1alpha2.ArbitrationSpec{HostFencing: ptr.To(s.hostFencing)}
	objs := make([]client.Object, 0, 6+len(s.extra))
	objs = append(objs, cluster,
		&simplyblockv1alpha2.StorageFenceRemediation{ObjectMeta: metav1.ObjectMeta{Name: workerB, Namespace: ns}})
	for _, w := range []struct{ sn, worker, id string }{{"sn-a", workerA, "id-a"}, {"sn-b", workerB, "id-b"}} {
		objs = append(objs, &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: w.sn, Namespace: ns},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "edge", WorkerNode: w.worker},
			Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: w.id},
		})
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.worker}}
		if s.fenced && w.worker == workerB {
			node.Status.Conditions = []corev1.NodeCondition{{Type: simplyblockv1alpha2.ConditionStorageFenced,
				Status: corev1.ConditionTrue}}
		}
		objs = append(objs, node)
	}
	objs = append(objs, s.extra...)
	scheme := testsupport.NewScheme(t, clientgoscheme.AddToScheme)
	c := testsupport.NewClient(t, scheme, []client.Object{&simplyblockv1alpha2.StorageFenceRemediation{},
		&simplyblockv1alpha2.StorageNode{}, &simplyblockv1alpha2.StorageCluster{}, &corev1.Node{}}, objs...)
	fixed := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return &RemediationReconciler{Client: c, Scheme: scheme, API: api, Now: func() time.Time { return fixed }}, c
}

func reconcileRemediation(t *testing.T, r *RemediationReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: workerB}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func outOfService(t *testing.T, c client.Client, worker string) (*corev1.Taint, string) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: worker}, &n); err != nil {
		t.Fatal(err)
	}
	for i := range n.Spec.Taints {
		if n.Spec.Taints[i].Key == simplyblockv1alpha2.TaintOutOfService {
			return &n.Spec.Taints[i], n.Annotations[simplyblockv1alpha2.AnnoOutOfServiceBy]
		}
	}
	return nil, n.Annotations[simplyblockv1alpha2.AnnoOutOfServiceBy]
}

func remediation(t *testing.T, c client.Client) simplyblockv1alpha2.StorageFenceRemediation {
	t.Helper()
	var rem simplyblockv1alpha2.StorageFenceRemediation
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: workerB}, &rem); err != nil {
		t.Fatal(err)
	}
	return rem
}

func condStatus(rem simplyblockv1alpha2.StorageFenceRemediation, typ string) metav1.ConditionStatus {
	for _, c := range rem.Status.Conditions {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

func TestRemediationTaintsAFencedNode(t *testing.T) {
	r, c := remediationSetup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)}, remSetup{fenced: true, hostFencing: true})
	if res := reconcileRemediation(t, r); res.RequeueAfter != 0 {
		t.Fatalf("a tainted node needs no requeue: %v", res.RequeueAfter)
	}
	taint, by := outOfService(t, c, workerB)
	if taint == nil || taint.Effect != corev1.TaintEffectNoExecute || taint.Value != simplyblockv1alpha2.TaintOutOfServiceValue {
		t.Fatalf("taint %+v", taint)
	}
	if by != simplyblockv1alpha2.OutOfServiceByStorageFence {
		t.Fatalf("ownership annotation %q", by)
	}
	if tA, _ := outOfService(t, c, workerA); tA != nil {
		t.Fatal("the winner must not be tainted")
	}
	rem := remediation(t, c)
	if rem.Status.Phase != simplyblockv1alpha2.StorageFenceRemediationTainted || rem.Status.Epoch != 7 ||
		condStatus(rem, simplyblockv1alpha2.RemediationConditionSucceeded) != metav1.ConditionTrue ||
		condStatus(rem, simplyblockv1alpha2.RemediationConditionProcessing) != metav1.ConditionFalse {
		t.Fatalf("status %+v", rem.Status)
	}
	if !strings.Contains(strings.Join(rem.Finalizers, ","), RemediationFinalizer) {
		t.Fatalf("finalizers %v", rem.Finalizers)
	}
}

// The condition is the operator's copy; the taint relies on the control
// plane's fence itself, which must still hold.
func TestRemediationWaitsForTheControlPlanesFence(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *Record
		set  remSetup
	}{
		{"no condition", fenceRecord(HostFenceRevoked), remSetup{fenced: false, hostFencing: true}},
		{"fence still revoking", fenceRecord("revoking"), remSetup{fenced: true, hostFencing: true}},
		{"no fence held", &Record{State: "steady"}, remSetup{fenced: true, hostFencing: true}},
		{"host fencing off", fenceRecord(HostFenceRevoked), remSetup{fenced: true, hostFencing: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := remediationSetup(t, &fakeAPI{rec: tc.rec}, tc.set)
			if res := reconcileRemediation(t, r); res.RequeueAfter != requeueRemediation {
				t.Fatalf("requeue %v", res.RequeueAfter)
			}
			if taint, _ := outOfService(t, c, workerB); taint != nil {
				t.Fatal("tainted without a fence in force")
			}
			if rem := remediation(t, c); rem.Status.Phase != simplyblockv1alpha2.StorageFenceRemediationPending ||
				condStatus(rem, simplyblockv1alpha2.RemediationConditionProcessing) != metav1.ConditionTrue {
				t.Fatalf("status %+v", rem.Status)
			}
		})
	}
}

// The taint force-detaches every volume of the node; only simplyblock's are fenced.
func TestRemediationRefusesWithAForeignVolume(t *testing.T) {
	foreign := &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "va-ebs"},
		Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: workerB}}
	ours := &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "va-sb"},
		Spec: storagev1.VolumeAttachmentSpec{Attacher: kube.DriverName, NodeName: workerB}}
	elsewhere := &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "va-a"},
		Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: workerA}}
	r, c := remediationSetup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)},
		remSetup{fenced: true, hostFencing: true, extra: []client.Object{foreign, ours, elsewhere}})
	reconcileRemediation(t, r)
	if taint, _ := outOfService(t, c, workerB); taint != nil {
		t.Fatal("tainted with a foreign volume attached")
	}
	rem := remediation(t, c)
	if rem.Status.Phase != simplyblockv1alpha2.StorageFenceRemediationBlocked || !strings.Contains(rem.Status.Message, "va-ebs") ||
		strings.Contains(rem.Status.Message, "va-a") {
		t.Fatalf("status %+v", rem.Status)
	}

	rem.Spec.AllowForeignVolumes = true
	if err := c.Update(context.Background(), &rem); err != nil {
		t.Fatal(err)
	}
	reconcileRemediation(t, r)
	if taint, _ := outOfService(t, c, workerB); taint == nil {
		t.Fatal("allowForeignVolumes must let the taint go on")
	}
}

func TestRemediationDeletionRemovesOnlyItsOwnTaint(t *testing.T) {
	r, c := remediationSetup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)}, remSetup{fenced: true, hostFencing: true})
	reconcileRemediation(t, r)
	rem := remediation(t, c)
	if err := c.Delete(context.Background(), &rem); err != nil {
		t.Fatal(err)
	}
	reconcileRemediation(t, r)
	if taint, by := outOfService(t, c, workerB); taint != nil || by != "" {
		t.Fatalf("taint %+v, annotation %q after deletion", taint, by)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: workerB}, &rem); err == nil {
		t.Fatal("the finalizer must be released")
	}
}

// An out-of-service taint an administrator set (a node really shut down) is
// theirs to remove.
func TestRemediationDeletionKeepsAnAdministratorsTaint(t *testing.T) {
	r, c := remediationSetup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)}, remSetup{fenced: true, hostFencing: true})
	var n corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: workerB}, &n)
	n.Spec.Taints = []corev1.Taint{{Key: simplyblockv1alpha2.TaintOutOfService, Value: "nodeshutdown",
		Effect: corev1.TaintEffectNoExecute}}
	if err := c.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	reconcileRemediation(t, r)
	if rem := remediation(t, c); rem.Status.Phase != simplyblockv1alpha2.StorageFenceRemediationTainted {
		t.Fatalf("an already tainted node is remediated: %+v", rem.Status)
	}
	rem := remediation(t, c)
	if err := c.Delete(context.Background(), &rem); err != nil {
		t.Fatal(err)
	}
	reconcileRemediation(t, r)
	if taint, _ := outOfService(t, c, workerB); taint == nil {
		t.Fatal("an administrator's taint must stay")
	}
}

// NodeHealthCheck may name the remediation otherwise and the node in an annotation.
func TestRemediationFollowsTheNodeNameAnnotation(t *testing.T) {
	r, c := remediationSetup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)}, remSetup{fenced: true, hostFencing: true})
	other := &simplyblockv1alpha2.StorageFenceRemediation{ObjectMeta: metav1.ObjectMeta{Name: "worker-b-sfr", Namespace: ns,
		Annotations: map[string]string{AnnoRemediationNodeName: workerB}}}
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns,
		Name: "worker-b-sfr"}}); err != nil {
		t.Fatal(err)
	}
	if taint, _ := outOfService(t, c, workerB); taint == nil {
		t.Fatal("the annotated node must be tainted")
	}
}
