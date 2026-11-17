// What the pool reports about a class assigned to it whose parameters depart
// from the pool's volume defaults (design-storagepool.md §5): a Warning for a
// class that provisions somewhere else or stripes otherwise than the pool, and a
// Normal event for a default a class overrides on purpose.
package pool

import (
	"testing"

	"k8s.io/utils/ptr"

	"github.com/simplyblock/atlas/kube"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// stripedPool is a ready pool whose volumes are striped across four members.
func stripedPool() *simplyblockv1alpha2.StoragePool {
	return newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: ptr.To[int32](4),
			StripeSize:  "64Ki",
			Filesystem:  "xfs",
			IOPS:        ptr.To[int32](20000),
		}
	})
}

// classFor is a class assigned to the pool, reaching it the way the driver
// needs to, plus extra parameters.
func classFor(extra map[string]string) map[string]string {
	params := map[string]string{
		kube.ParamClusterID: testClusterUUID,
		kube.ParamPool:      "tenant-a",
	}
	for k, v := range extra {
		params[k] = v
	}
	return params
}

// Regression: 2026-11-17-pool-stripe-never-reaches-class (PR #922 review). A
// pool's stripeWidth reaches a volume only through a class that states it, and
// the operator writes no class for a pool other than the default. A class
// assigned to a striped pool that does not stripe provisioned ordinary volumes
// with nothing anywhere saying so.
func TestWarnsOfAClassThatDoesNotStripeAsItsPoolDoes(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	class := newClass("fast", assignmentLabels("tenant-a", false), classFor(map[string]string{
		"csi.storage.k8s.io/fstype": "xfs",
		kube.ParamMaxIOPS:           "20000",
	}))
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), stripedPool(), class)

	reconcileSettled(t, r, "tenant-a")

	if !rec.hasTyped("Warning", StorageClassDisagreesWithPool) {
		t.Errorf("no Warning %s event: %+v", StorageClassDisagreesWithPool, rec.events)
	}
}

// A class that states the pool's stripe in another spelling of the same size,
// and the QoS ceiling under its older key, agrees with the pool.
func TestAClassStatingThePoolsDefaultsInOtherSpellingsAgrees(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	class := newClass("fast", assignmentLabels("tenant-a", false), classFor(map[string]string{
		"csi.storage.k8s.io/fstype": "xfs",
		kube.ParamQoSRWIOPS:         "20000",
		kube.ParamStripeWidth:       "4",
		kube.ParamStripeSize:        "65536",
	}))
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), stripedPool(), class)

	reconcileSettled(t, r, "tenant-a")

	if rec.has(StorageClassDisagreesWithPool) || rec.has(StorageClassOverridesPoolDefault) {
		t.Errorf("a class stating the pool's defaults was reported: %+v", rec.events)
	}
}

// A class that reaches another cluster or another pool than the one its labels
// assign it to provisions somewhere else, which is the hard case.
func TestWarnsOfAClassThatProvisionsSomewhereElse(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	class := newClass("elsewhere", assignmentLabels("tenant-a", false), map[string]string{
		kube.ParamClusterID: "a-different-cluster",
		kube.ParamPool:      "a-different-pool",
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, class)

	reconcileSettled(t, r, "tenant-a")

	if !rec.hasTyped("Warning", StorageClassDisagreesWithPool) {
		t.Errorf("no Warning %s event: %+v", StorageClassDisagreesWithPool, rec.events)
	}
}

// A class naming the pool by its UUID reaches it as well as one naming it by
// name, and is not reported.
func TestAClassNamingThePoolByItsUUIDAgrees(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	class := newClass("by-uuid", assignmentLabels("tenant-a", false), map[string]string{
		kube.ParamClusterID: testClusterUUID,
		kube.ParamPool:      testPoolUUID,
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), ready, class)

	reconcileSettled(t, r, "tenant-a")

	if rec.has(StorageClassDisagreesWithPool) {
		t.Errorf("a class naming the pool by UUID was reported: %+v", rec.events)
	}
}

// A class that overrides an ordinary default, another filesystem, is what §5
// allows a pool's classes to do, and is reported as information, not a
// warning.
func TestReportsAnOverriddenDefaultAsInformation(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	class := newClass("fast-ext4", assignmentLabels("tenant-a", false), classFor(map[string]string{
		"csi.storage.k8s.io/fstype": "ext4",
		kube.ParamMaxIOPS:           "20000",
		kube.ParamStripeWidth:       "4",
	}))
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), stripedPool(), class)

	reconcileSettled(t, r, "tenant-a")

	if !rec.hasTyped("Normal", StorageClassOverridesPoolDefault) {
		t.Errorf("no Normal %s event: %+v", StorageClassOverridesPoolDefault, rec.events)
	}
	if rec.has(StorageClassDisagreesWithPool) {
		t.Errorf("an overridden filesystem was reported as a disagreement: %+v", rec.events)
	}
}
