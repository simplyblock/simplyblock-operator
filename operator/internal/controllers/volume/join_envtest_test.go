// The JoinConsistencyGroup against a real apiserver: every status write the
// flip makes is validated by the CRD's schema and CEL rules, which a fake
// client never checks, and the lock and the child migration are real objects.
// The control plane and the CSI nodes are the same fakes the unit tests use.

package volume

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// newEnvJoinWorld is newJoinWorld on the shared apiserver. Every object it
// creates is deleted when the test ends, so the next test starts from nothing.
func newEnvJoinWorld(t *testing.T) *joinWorld {
	t.Helper()
	c := apiServer(t)
	ctx := context.Background()

	w := newJoinWorld(t)
	fake := w.r

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	var created []client.Object
	t.Cleanup(func() {
		var list simplyblockv1alpha2.PersistentVolumeOpsList
		if err := c.List(ctx, &list); err == nil {
			for i := range list.Items {
				item := &list.Items[i]
				item.Finalizers = nil
				_ = c.Update(ctx, item)
				_ = c.Delete(ctx, item)
			}
		}
		for _, obj := range created {
			// The apiserver protects a PersistentVolume with a finalizer that
			// nothing in envtest removes, so it is cleared here and the delete
			// waited out: the next test creates the same names.
			if len(obj.GetFinalizers()) > 0 || obj.GetObjectKind() != nil {
				fresh := obj.DeepCopyObject().(client.Object)
				if c.Get(ctx, client.ObjectKeyFromObject(obj), fresh) == nil && len(fresh.GetFinalizers()) > 0 {
					fresh.SetFinalizers(nil)
					_ = c.Update(ctx, fresh)
				}
			}
			_ = c.Delete(ctx, obj)
			for range 100 {
				if apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object))) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	create := func(obj client.Object) {
		t.Helper()
		status := obj.DeepCopyObject().(client.Object)
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
		created = append(created, obj)
		switch o := status.(type) {
		case *simplyblockv1alpha2.StorageCluster:
			obj.(*simplyblockv1alpha2.StorageCluster).Status = o.Status
		case *simplyblockv1alpha2.StorageNode:
			obj.(*simplyblockv1alpha2.StorageNode).Status = o.Status
		default:
			return
		}
		if err := c.Status().Update(ctx, obj); err != nil {
			t.Fatalf("set the status of %T %s: %v", obj, obj.GetName(), err)
		}
	}
	for _, obj := range []client.Object{
		joinVolume(testPVName, testVolumeID, "joiner"),
		joinVolume(memberPVName, memberVolumeID, "member"),
		attachment(testPVName, joinHost),
		attachment(memberPVName, memberHost),
		envClusterObject(),
		envNodeObject(),
	} {
		create(obj)
	}
	ops := joinOps()
	ops.UID = ""
	create(ops)

	w.r = &PersistentVolumeOpsReconciler{
		Client:   c,
		Reader:   c,
		Scheme:   scheme.Scheme,
		Recorder: events.NewFakeRecorder(256),
		API:      fake.API,
		Join:     fake.Join,
		Swap:     fake.Swap,
		Clock:    fake.Clock,
	}
	return w
}

// envClusterObject is the test cluster with the fields admission requires.
func envClusterObject() *simplyblockv1alpha2.StorageCluster {
	cluster := testClusterObject()
	subsystems, vcpus := int32(500), int32(4)
	cluster.Spec.MaxSubsystemCount = &subsystems
	cluster.Spec.VCPUCount = &vcpus
	return cluster
}

// envNodeObject is the target node with the fields admission requires.
func envNodeObject() *simplyblockv1alpha2.StorageNode {
	node := testNodeObject()
	vcpus := int32(8)
	node.Spec.WorkerNode = testTargetNode
	node.Spec.Config.Sizing = simplyblockv1alpha2.StorageNodeSizing{VCPUCount: &vcpus, MinHugePagesSize: "100G"}
	return node
}

// I-01, I-02, I-03
func TestAWholeGroupFlipsThroughTheAPIServer(t *testing.T) {
	w := newEnvJoinWorld(t)

	ops := w.run(60)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	move := w.log.index("move")
	for _, name := range []string{joinHost, memberHost} {
		if s := w.log.index("suspend " + name); s < 0 || s > move {
			t.Errorf("%s was suspended at %d, the move ran at %d", name, s, move)
		}
	}
	for _, prefix := range []string{"join", "create", "move", "commit"} {
		if n := w.log.count(prefix); n != 1 {
			t.Errorf("%s called %d times, want 1", prefix, n)
		}
	}
	status := ops.Status.JoinConsistencyGroup
	if !status.TargetIsNew || len(status.Hosts) != 2 || status.NamespacesAt != simplyblockv1alpha2.SubsystemSideTarget {
		t.Errorf("status = %+v", status)
	}
	for _, name := range []string{testPVName, memberPVName} {
		if got := w.pv(name).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != "" {
			t.Errorf("volume %s is still locked by %q", name, got)
		}
	}
}

// I-04
func TestAnOffPinJoinWaitsForItsChildOnTheAPIServer(t *testing.T) {
	w := newEnvJoinWorld(t)
	w.offPin()

	w.runUntil(stepJoinMigrating, 10)
	w.run(3)
	children := w.children()
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	if w.log.count("join") != 0 {
		t.Fatal("joined before the migration finished")
	}
	w.setChildPhase(simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded)
	if ops := w.run(60); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
}

// I-05
func TestACapacityRefusalTakesNoLockAndNoChildOnTheAPIServer(t *testing.T) {
	w := newEnvJoinWorld(t)
	w.offPin()
	w.api.plan.Colocation = &controlplane.Colocation{Refusal: "SubsystemLimitReached: node n-1 holds 500"}

	ops := w.run(10)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if n := len(w.children()); n != 0 {
		t.Errorf("children = %d, want 0", n)
	}
	for _, name := range []string{testPVName, memberPVName} {
		if got := w.pv(name).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != "" {
			t.Errorf("volume %s locked by %q", name, got)
		}
	}
}

// I-07
func TestAnOperatorRestartInsideTheWindowSuspendsEachHostOnceOnTheAPIServer(t *testing.T) {
	w := newEnvJoinWorld(t)
	shared := w.r

	w.runUntil(stepSuspending, 40)
	for range 60 {
		fresh := *shared
		fresh.Recorder = events.NewFakeRecorder(256)
		w.r = &fresh
		if _, err := w.pass(); err != nil {
			t.Fatal(err)
		}
		if terminal(w.ops().Status.Phase) {
			break
		}
	}
	w.r = shared
	if ops := w.ops(); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	for _, name := range []string{joinHost, memberHost} {
		if n := w.log.count("suspend " + name); n != 1 {
			t.Errorf("%s suspended %d times, want 1", name, n)
		}
	}
}

// I-08
func TestAHostRestartBeforeTheMoveRollsBackOnTheAPIServer(t *testing.T) {
	w := newEnvJoinWorld(t)

	w.runUntil(stepMovingNamespaces, 40)
	w.node(memberHost).instance = "instance-restarted"

	ops := w.run(60)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed || w.log.count("move") != 0 {
		t.Fatalf("phase = %q, moves = %d, want a rollback without a move", ops.Status.Phase, w.log.count("move"))
	}
	if ops.Status.JoinConsistencyGroup.RollbackReason == "" {
		t.Error("status.rollbackReason was not persisted")
	}
}
