// Tests for the defects the review on #525 found, each pinned so the same shape
// cannot come back.
//
// Four of them share a root: a cluster-scoped name or an AlreadyExists result
// was treated as if it belonged to this pool. A StorageClass is cluster-scoped
// and the operator watches every namespace, so "the object with the name I would
// have used" and "my object" are different claims, and the second one has to be
// checked rather than assumed.

package pool

import (
	"context"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A StorageClass is cluster-scoped and a StorageCluster is not, so two
// namespaces may each hold a cluster called production. Their default pools must
// not contend for one class name.
func TestTwoNamespacesGetDistinctDefaultClassNames(t *testing.T) {
	here := DefaultStorageClassName("simplyblock", testCluster)
	there := DefaultStorageClassName("another-namespace", testCluster)

	if here == there {
		t.Errorf("both namespaces derive %q, so the second default pool collides "+
			"with the first one's class", here)
	}
}

// The managed-by label is the whole of the difference between a class the
// operator deletes with its pool and one it refuses to touch, so the value has
// to match rather than merely be present.
func TestSomebodyElsesManagedByIsNotOurs(t *testing.T) {
	theirs := newClass("theirs", map[string]string{
		LabelNamespace: testNamespace,
		LabelCluster:   testCluster,
		LabelPool:      "tenant-a",
		LabelManagedBy: "some-other-operator",
	}, nil)

	if IsOperatorManaged(theirs) {
		t.Error("a class another operator manages was read as this one's, " +
			"so a pool deletion would take it")
	}
}

// And the same class holds a deletion, because the operator cannot clean up what
// it did not write.
func TestAClassAnotherOperatorManagesHoldsTheDeletion(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	theirs := newClass("theirs", map[string]string{
		LabelNamespace: testNamespace,
		LabelCluster:   testCluster,
		LabelPool:      "tenant-a",
		LabelManagedBy: "some-other-operator",
	}, nil)
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), deleting("tenant-a"), theirs)

	reconcileTimes(t, r, "tenant-a", 2)

	if !poolExists(t, r, "tenant-a") {
		t.Fatal("the pool deleted past a class another operator manages")
	}
	var class storagev1.StorageClass
	if err := r.Get(context.Background(), client.ObjectKey{Name: "theirs"}, &class); err != nil {
		t.Errorf("the operator deleted a class it does not manage: %v", err)
	}
}

// The default class's name may already belong to a class that has nothing to do
// with this pool. Recording it as the pool's own would leave the pool claiming a
// class that provisions somewhere else, and never writing the one it needs,
// because this runs once.
func TestAForeignClassDoesNotBecomeTheDefault(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	squatter := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: DefaultStorageClassName(testNamespace, testCluster)},
		Provisioner: "another.csi.driver",
	}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool, squatter)

	p, _ := reconcileSettled(t, r, name)

	if p.Status.DefaultStorageClassName != "" {
		t.Errorf("status.defaultStorageClassName = %q, which is somebody else's class",
			p.Status.DefaultStorageClassName)
	}
	if !rec.has(StorageClassNameTaken) {
		t.Errorf("no %s event, so the pool has no default class and nothing says why: %+v",
			StorageClassNameTaken, rec.events)
	}

	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testNamespace, testCluster)}
	if err := r.Get(context.Background(), key, &class); err != nil {
		t.Fatalf("read the class back: %v", err)
	}
	if class.Provisioner != "another.csi.driver" {
		t.Error("the operator overwrote a class it did not write")
	}
}

// A class the operator wrote on an earlier pass is adopted rather than reported,
// which is what makes the write idempotent across a lost status patch.
func TestTheOperatorsOwnClassIsAdoptedOnRetry(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	labels := map[string]string{
		LabelNamespace: testNamespace,
		LabelCluster:   testCluster,
		LabelPool:      name,
		LabelManagedBy: ManagedByStorageCluster,
	}
	ours := newClass(DefaultStorageClassName(testNamespace, testCluster), labels, nil)
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool, ours)

	p, _ := reconcileSettled(t, r, name)

	if p.Status.DefaultStorageClassName != DefaultStorageClassName(testNamespace, testCluster) {
		t.Errorf("status.defaultStorageClassName = %q, want the class the operator had written",
			p.Status.DefaultStorageClassName)
	}
}

// spec.limits is mutable, and an edit that never reaches the control plane is
// worse than one that fails: the generation is reported as observed while the
// old ceilings are still enforced, which is indistinguishable from success.
func TestAChangedLimitReachesTheControlPlane(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ObservedGeneration = 1
		p.Generation = 2
		p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{Capacity: "20T"}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready)

	reconcileSettled(t, r, "tenant-a")

	if cp.puts == 0 {
		t.Error("the edited limits never reached the control plane, " +
			"but the generation would be reported as observed")
	}
}

// A pool nobody has edited sends no update. The generation is the trigger
// because the two vocabularies do not line up: a capacity of 10T becomes a byte
// count and an unset ceiling becomes zero, so a value diff would send an update
// on every pass for a pool that asked for nothing.
func TestAnUneditedPoolSendsNoUpdate(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ObservedGeneration = 1
		p.Generation = 1
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready)

	reconcileSettled(t, r, "tenant-a")

	if cp.puts != 0 {
		t.Errorf("the control plane was updated %d times for a pool nobody edited", cp.puts)
	}
}

// The three defaults that had no class parameter are written now. A pool can
// state them and spec.volumeDefaults is immutable, so a key omitted here is one
// that pool could never acquire without being replaced.
func TestEveryVolumeDefaultReachesTheClass(t *testing.T) {
	p := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{
			EnableCompression: ptr.To(true),
			EnableReplication: ptr.To(false),
			PriorityClass:     "high",
		}
	})

	params := ClassParameters(p, testClusterUUID)

	for key, want := range map[string]string{
		kube.ParamCompression:   "true",
		kube.ParamReplication:   "false",
		kube.ParamPriorityClass: "high",
	} {
		if got := params[key]; got != want {
			t.Errorf("the class's %s = %q, want %q", key, got, want)
		}
	}
}
