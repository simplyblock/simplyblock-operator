// Tests for the StoragePool reconciler: creation, the class it writes for a
// default pool, the node list it resolves, and the classes it indexes.
//
// The scenarios are the U- rows of docs/tests/test-plan-storagepool.md, and the
// ones worth reading first are the two that are about not doing something. The
// creation claim exists so a second reconciler does not create a second backend
// pool, and a non-default pool exists so that the operator stops generating
// classes; neither is visible in a passing reconcile, and both are what the
// tests below pin.

package pool

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// settledPasses is how many reconciles a pool needs before nothing more
// changes. The reconciler returns after each write that invalidates its own copy
// of the object — the owner reference, the creation claim, the UUID — so a test
// that ran one pass would be asserting against an object one step short of the
// state it means to check.
const settledPasses = 4

// reconcileTimes runs n passes and returns the pool as it stands afterward,
// together with the last pass's result.
func reconcileTimes(
	t *testing.T, r *StoragePoolReconciler, name string, n int,
) (*simplyblockv1alpha2.StoragePool, ctrl.Result) {
	t.Helper()
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	var result ctrl.Result
	for i := 0; i < n; i++ {
		var err error
		result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}
	// A pool that finished deleting is gone, and that is a result the deletion
	// tests assert on rather than an error: they read it back through
	// poolExists, which is why the absence is returned as a nil pool here.
	var p simplyblockv1alpha2.StoragePool
	if err := r.Get(context.Background(), key, &p); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("read the pool back: %v", err)
		}
		return nil, result
	}
	return &p, result
}

// reconcileSettled drives the pool to the state it holds at.
func reconcileSettled(
	t *testing.T, r *StoragePoolReconciler, name string,
) (*simplyblockv1alpha2.StoragePool, ctrl.Result) {
	t.Helper()
	return reconcileTimes(t, r, name, settledPasses)
}

func newReconciler(
	t *testing.T, cp *controlPlane, rec *recorder, objects ...client.Object,
) *StoragePoolReconciler {
	t.Helper()
	c := newClient(t, objects...)
	return &StoragePoolReconciler{
		Client:       c,
		Scheme:       testScheme(t),
		Recorder:     rec,
		NewAPIClient: cp.client(),
	}
}

// U-07: a pool with no UUID is created and the UUID recorded.
func TestCreatesThePoolAndRecordsItsUUID(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), newPool("tenant-a"))

	p, _ := reconcileSettled(t, r, "tenant-a")

	if p.Status.UUID != testPoolUUID {
		t.Errorf("status.uuid = %q, want %q", p.Status.UUID, testPoolUUID)
	}
	if cp.posts != 1 {
		t.Errorf("the control plane was asked to create the pool %d times, want 1", cp.posts)
	}
	if !rec.has(PoolCreated) {
		t.Errorf("no %s event: %+v", PoolCreated, rec.events)
	}
}

// U-14: a pool that already has a UUID is not created again. This is the one
// that keeps a non-idempotent POST from running on every reconcile.
func TestDoesNotCreateAPoolThatAlreadyHasAUUID(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready)

	reconcileSettled(t, r, "tenant-a")

	if cp.posts != 0 {
		t.Errorf("the control plane was asked to create the pool %d times, want 0", cp.posts)
	}
}

// U-08: the claim is persisted before the POST is issued.
//
// The stub reads the pool back out of Kubernetes at the moment it is asked to
// create one, which is the only way to observe the ordering: after the call both
// orderings look identical, and the whole point of the claim is what a second
// reconciler sees while the call is in flight.
func TestTheClaimIsPersistedBeforeTheCreateIsIssued(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), newPool("tenant-a"))

	var phaseWhenCalled simplyblockv1alpha2.StoragePoolPhase
	cp.onCreate = func() {
		var p simplyblockv1alpha2.StoragePool
		key := client.ObjectKey{Namespace: testNamespace, Name: "tenant-a"}
		if err := r.Get(context.Background(), key, &p); err != nil {
			t.Errorf("read the pool during the create: %v", err)
			return
		}
		phaseWhenCalled = p.Status.Phase
	}

	reconcileSettled(t, r, "tenant-a")

	if cp.posts != 1 {
		t.Fatalf("the control plane was asked to create the pool %d times, want 1", cp.posts)
	}
	if phaseWhenCalled != simplyblockv1alpha2.StoragePoolPhasePending {
		t.Errorf("the pool's phase when the create was issued = %q, want Pending: "+
			"the claim was not persisted first, so a second reconciler would create a second pool",
			phaseWhenCalled)
	}
}

// U-09: two reconcilers holding the same resourceVersion both try to claim the
// creation, and exactly one of them wins. The loser's optimistic patch is
// refused and it never reaches the control plane.
func TestOnlyOneOfTwoReconcilersClaimsTheCreation(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), newPool("tenant-a"))

	ctx := context.Background()
	key := client.ObjectKey{Namespace: testNamespace, Name: "tenant-a"}
	var first, second simplyblockv1alpha2.StoragePool
	if err := r.Get(ctx, key, &first); err != nil {
		t.Fatalf("read the pool: %v", err)
	}
	if err := r.Get(ctx, key, &second); err != nil {
		t.Fatalf("read the pool: %v", err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("the two copies are not at the same resourceVersion, so there is no race to lose")
	}

	api := cp.client()()
	if _, err := r.reconcileCreate(ctx, &first, api, testClusterUUID); err != nil {
		t.Fatalf("the first reconciler: %v", err)
	}
	if _, err := r.reconcileCreate(ctx, &second, api, testClusterUUID); err != nil {
		t.Fatalf("the second reconciler: %v", err)
	}

	if cp.posts != 1 {
		t.Errorf("the control plane was asked to create the pool %d times, want 1", cp.posts)
	}
}

// U-11: a refusal is reported with the control plane's own words, and the pool
// keeps no UUID.
func TestReportsACreationRefusal(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	cp.createStatus, cp.createBody = http.StatusBadRequest, `{"error":"pool name taken"}`
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), newPool("tenant-a"))

	p, result := reconcileSettled(t, r, "tenant-a")

	if p.Status.UUID != "" {
		t.Errorf("status.uuid = %q, want it left empty", p.Status.UUID)
	}
	if !rec.has(PoolCreationFailed) {
		t.Errorf("no %s event: %+v", PoolCreationFailed, rec.events)
	}
	if result.RequeueAfter == 0 {
		t.Error("the reconcile did not ask to be retried")
	}
}

// U-12: a cluster with no UUID holds the pool at Pending rather than failing it.
// A manifest declaring a cluster and its pools in one apply is the ordinary way
// to bring a deployment up, so this is a not-yet and not a mistake.
func TestHoldsWhileTheClusterIsNotReady(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newCluster(""), newPool("tenant-a"))

	p, result := reconcileSettled(t, r, "tenant-a")

	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhasePending {
		t.Errorf("status.phase = %q, want Pending", p.Status.Phase)
	}
	if !rec.has(ClusterNotReady) {
		t.Errorf("no %s event: %+v", ClusterNotReady, rec.events)
	}
	if cp.posts != 0 {
		t.Errorf("the control plane was called %d times with no cluster UUID, want 0", cp.posts)
	}
	if result.RequeueAfter == 0 {
		t.Error("the reconcile did not ask to be retried")
	}
}

// U-13: a pool whose cluster does not exist is held too, not failed. The webhook
// refuses this at admission; an object in this state was written while the
// webhook was not serving, and the cluster may still arrive.
func TestHoldsWhenTheClusterDoesNotExist(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newPool("tenant-a"))

	p, _ := reconcileSettled(t, r, "tenant-a")

	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhasePending {
		t.Errorf("status.phase = %q, want Pending", p.Status.Phase)
	}
	if !rec.has(ClusterNotReady) {
		t.Errorf("no %s event: %+v", ClusterNotReady, rec.events)
	}
}

// U-15: a pool that is not its cluster's default gets no class from its own
// reconcile, ever. This is the behavior change of design §5 in one assertion:
// the operator stopped generating a class per pool.
func TestANonDefaultPoolGetsNoGeneratedClass(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready)

	p, _ := reconcileSettled(t, r, "tenant-a")

	var classes storagev1.StorageClassList
	if err := r.List(context.Background(), &classes); err != nil {
		t.Fatalf("list the storage classes: %v", err)
	}
	if len(classes.Items) != 0 {
		t.Errorf("the reconcile created %d storage classes, want none", len(classes.Items))
	}
	if p.Status.DefaultStorageClassName != "" {
		t.Errorf("status.defaultStorageClassName = %q, want empty on a non-default pool",
			p.Status.DefaultStorageClassName)
	}
}

// U-18 and U-20: the default pool's class is written once, labeled managed-by,
// and not written again after somebody deletes it.
func TestTheDefaultPoolsClassIsWrittenOnceAndNotRecreated(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	p, _ := reconcileSettled(t, r, name)

	className := DefaultStorageClassName(testCluster)
	if p.Status.DefaultStorageClassName != className {
		t.Fatalf("status.defaultStorageClassName = %q, want %q",
			p.Status.DefaultStorageClassName, className)
	}
	var class storagev1.StorageClass
	if err := r.Get(context.Background(), client.ObjectKey{Name: className}, &class); err != nil {
		t.Fatalf("the default class was not written: %v", err)
	}
	if class.Labels[LabelManagedBy] != ManagedByStorageCluster {
		t.Errorf("the class carries managed-by %q, want %q",
			class.Labels[LabelManagedBy], ManagedByStorageCluster)
	}

	// Somebody deletes it deliberately. Recreating it would be the operator
	// arguing with an administrator about a class the pool does not need.
	if err := r.Delete(context.Background(), &class); err != nil {
		t.Fatalf("delete the class: %v", err)
	}
	reconcileSettled(t, r, name)
	err := r.Get(context.Background(), client.ObjectKey{Name: className}, &class)
	if err == nil {
		t.Error("the deleted default class was written again")
	}
}

// U-22: status.storageClassNames lists every assigned class and nothing else,
// which is what makes the assignment readable from the pool.
func TestPublishesTheClassesAssignedToThePool(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), ready,
		newClass("fast-xfs", assignmentLabels("tenant-a", false), nil),
		newClass("archive-ext4", assignmentLabels("tenant-a", false), nil),
		newClass("another-pools-class", assignmentLabels("tenant-b", false), nil),
		newClass("nobodys-class", nil, nil),
	)

	p, _ := reconcileSettled(t, r, "tenant-a")

	want := []string{"archive-ext4", "fast-xfs"}
	if len(p.Status.StorageClassNames) != len(want) {
		t.Fatalf("status.storageClassNames = %v, want %v", p.Status.StorageClassNames, want)
	}
	for i, name := range want {
		if p.Status.StorageClassNames[i] != name {
			t.Errorf("status.storageClassNames[%d] = %q, want %q",
				i, p.Status.StorageClassNames[i], name)
		}
	}
	if !rec.has(StorageClassAssigned) {
		t.Errorf("no %s event: %+v", StorageClassAssigned, rec.events)
	}
}

// U-23: a class stating one ceiling under both spellings is reported rather than
// merged. Quietly preferring one is how a volume ends up throttled at a number
// nobody chose.
func TestReportsAClassThatStatesOneCeilingTwice(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	conflicted := newClass("both-spellings", assignmentLabels("tenant-a", false), map[string]string{
		kube.ParamMaxIOPS:   "1000",
		kube.ParamQoSRWIOPS: "5000",
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, conflicted)

	reconcileSettled(t, r, "tenant-a")

	if !rec.has(QoSParameterConflict) {
		t.Errorf("no %s event: %+v", QoSParameterConflict, rec.events)
	}
}

// U-17 in its other half: a class whose parameters disagree with its labels is
// reported and not rewritten. Nothing the reconcile does touches an existing
// class, which is the property this pins.
func TestNeverRewritesAnAssignedClass(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	authored := newClass("hand-written", assignmentLabels("tenant-a", false), map[string]string{
		kube.ParamClusterID: "a-different-cluster",
		kube.ParamPool:      "a-different-pool",
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, authored)

	reconcileSettled(t, r, "tenant-a")

	var class storagev1.StorageClass
	if err := r.Get(context.Background(), client.ObjectKey{Name: "hand-written"}, &class); err != nil {
		t.Fatalf("read the class back: %v", err)
	}
	if class.Parameters[kube.ParamPool] != "a-different-pool" {
		t.Errorf("the reconcile rewrote the class: pool_name = %q", class.Parameters[kube.ParamPool])
	}
}

// §4.3: an unresolvable entry in spec.allowedNodes is dropped from the resolved
// set, announced once rather than every pass, and left in the spec.
func TestResolvesTheAllowedNodesAndReportsAMissingOneOnce(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.AllowedNodes = []string{"worker-1", "worker-gone"}
	})
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: "worker-1-uid"}}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, worker)

	reconcileSettled(t, r, "tenant-a")
	p, _ := reconcileSettled(t, r, "tenant-a")

	if len(p.Status.AllowedNodes) != 1 || p.Status.AllowedNodes[0] != "worker-1" {
		t.Errorf("status.allowedNodes = %v, want just worker-1", p.Status.AllowedNodes)
	}
	if len(p.Spec.AllowedNodes) != 2 {
		t.Errorf("spec.allowedNodes = %v, want the authored list left alone", p.Spec.AllowedNodes)
	}
	if got := rec.count(AllowedNodeMissing); got != 1 {
		t.Errorf("%s was emitted %d times over two passes, want 1", AllowedNodeMissing, got)
	}
}

// §4.3: a pool whose every allowed node is gone resolves to an empty set, which
// is not the same as an absent list. Absent means every node; empty after
// resolution means the pool can place nothing, so the phase holds.
func TestAPoolWhoseEveryAllowedNodeIsGoneHolds(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.AllowedNodes = []string{"worker-gone"}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready)

	p, _ := reconcileSettled(t, r, "tenant-a")

	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhasePending {
		t.Errorf("status.phase = %q, want Pending", p.Status.Phase)
	}
	if p.Status.Message == "" {
		t.Error("status.message says nothing about why the pool is held")
	}
}

// §4.3: the allowed nodes carry the pool's label, which is the key the generated
// class republishes as dhchap_node_selector.
func TestLabelsTheAllowedNodes(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.AllowedNodes = []string{"worker-1"}
	})
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: "worker-1-uid"}}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, worker)

	reconcileSettled(t, r, "tenant-a")

	var node corev1.Node
	if err := r.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &node); err != nil {
		t.Fatalf("read the node back: %v", err)
	}
	key := kube.PoolNodeLabelKey(testPoolUUID)
	if node.Labels[key] != kube.LabelPoolAllowed {
		t.Errorf("node label %s = %q, want %q", key, node.Labels[key], kube.LabelPoolAllowed)
	}
}

// U-35 and U-36: the pool's own ceilings go to the control plane and the
// volumes' defaults go to the class, and neither is written into the other's
// destination. That separation is what the regrouping exists for.
func TestTheTwoLimitGroupsGoToDifferentPlaces(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{
			Capacity: "10T",
			IOPS:     ptr.To(int32(200000)),
		}
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{
			IOPS:       ptr.To(int32(20000)),
			Filesystem: "xfs",
		}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	reconcileSettled(t, r, name)

	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testCluster)}
	if err := r.Get(context.Background(), key, &class); err != nil {
		t.Fatalf("the default class was not written: %v", err)
	}
	if got := class.Parameters[kube.ParamMaxIOPS]; got != "20000" {
		t.Errorf("the class's %s = %q, want the volume default 20000", kube.ParamMaxIOPS, got)
	}
	if _, ok := class.Parameters["capacity"]; ok {
		t.Error("the pool's own capacity limit reached the class")
	}
}

// U-39: a volume default of 0 is unlimited rather than unset, so it has to reach
// the class as "0" rather than be omitted.
func TestAZeroVolumeDefaultReachesTheClassAsZero(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{IOPS: ptr.To(int32(0))}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	reconcileSettled(t, r, name)

	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testCluster)}
	if err := r.Get(context.Background(), key, &class); err != nil {
		t.Fatalf("the default class was not written: %v", err)
	}
	if got, ok := class.Parameters[kube.ParamMaxIOPS]; !ok || got != "0" {
		t.Errorf("the class's %s = %q (present %t), want \"0\"", kube.ParamMaxIOPS, got, ok)
	}
}

// The class the operator writes carries the current QoS spelling only. A class
// it generates carries one generation, so nothing it creates needs the fallback
// the driver keeps for the older keys.
func TestTheGeneratedClassCarriesOnlyTheCurrentQoSSpelling(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{IOPS: ptr.To(int32(20000))}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	reconcileSettled(t, r, name)

	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testCluster)}
	if err := r.Get(context.Background(), key, &class); err != nil {
		t.Fatalf("the default class was not written: %v", err)
	}
	for _, old := range []string{
		kube.ParamQoSRWIOPS, kube.ParamQoSRWMBytes, kube.ParamQoSRMBytes, kube.ParamQoSWMBytes,
	} {
		if _, ok := class.Parameters[old]; ok {
			t.Errorf("the generated class carries the older spelling %q", old)
		}
	}
	if len(kube.QoSParamConflicts(class.Parameters)) != 0 {
		t.Error("the generated class states a ceiling twice")
	}
}

// §6: the cluster owns its pools, which is what makes deleting a cluster reach
// them at all. The finalizer is the other half and is tested next door.
func TestTheClusterOwnsItsPools(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	unowned := newPool("tenant-a")
	unowned.Finalizers = nil
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), unowned)

	p, _ := reconcileSettled(t, r, "tenant-a")

	owner := metav1.GetControllerOf(p)
	if owner == nil || owner.Kind != "StorageCluster" || owner.Name != testCluster {
		t.Errorf("controller reference = %+v, want the StorageCluster %q", owner, testCluster)
	}
	if len(p.Finalizers) == 0 {
		t.Error("the pool has no finalizer, so a cluster deletion would take it silently")
	}
}
