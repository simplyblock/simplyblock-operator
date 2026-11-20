// Tests for the supercluster's two gauges: one series set per supercluster,
// namespaced like the object, and nothing left behind that the status no
// longer reports.

package cluster

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func superclusterIn(namespace string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: testSuperclusterName},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
}

// StorageClusters are namespaced, so team-a/prod and team-b/prod are two
// superclusters, and each reports its own members.
func TestTwoSuperclustersOfOneNameInTwoNamespacesReportApart(t *testing.T) {
	superclusterMembers.Reset()
	superclusterCapacityBytes.Reset()

	observeSupercluster(superclusterIn("team-a"), superclusterRollup{
		Members:  []simplyblockv1alpha2.SuperclusterMemberStatus{{Name: "a", PlacementEligible: true}},
		Capacity: reading(1000, 100, 500),
	})
	observeSupercluster(superclusterIn("team-b"), superclusterRollup{})

	if got := testutil.ToFloat64(superclusterMembers.WithLabelValues("team-a", testSuperclusterName, "eligible")); got != 1 {
		t.Errorf("team-a eligible members = %v, want 1", got)
	}
	if got := testutil.ToFloat64(superclusterMembers.WithLabelValues("team-b", testSuperclusterName, "eligible")); got != 0 {
		t.Errorf("team-b eligible members = %v, want 0", got)
	}
	if got := testutil.ToFloat64(superclusterCapacityBytes.WithLabelValues("team-a", testSuperclusterName, "total")); got != 1000 {
		t.Errorf("team-a total bytes = %v, want 1000", got)
	}
}

// A capacity the status no longer reports is not exported as its last value.
func TestACapacityReadingThatGoesAwayIsNotExported(t *testing.T) {
	superclusterCapacityBytes.Reset()
	supercluster := superclusterIn(testNamespace)

	observeSupercluster(supercluster, superclusterRollup{Capacity: reading(1000, 100, 500)})
	observeSupercluster(supercluster, superclusterRollup{Capacity: &simplyblockv1alpha2.AggregateCapacity{
		TotalBytes: ptr.To(int64(2000)),
	}})
	if got := testutil.CollectAndCount(superclusterCapacityBytes); got != 1 {
		t.Errorf("%d capacity series after used and provisioned went unknown, want total alone", got)
	}

	observeSupercluster(supercluster, superclusterRollup{})
	if got := testutil.CollectAndCount(superclusterCapacityBytes); got != 0 {
		t.Errorf("%d capacity series after the last reading went away, want none", got)
	}
}

func TestADeletedSuperclustersSeriesGo(t *testing.T) {
	superclusterMembers.Reset()
	superclusterCapacityBytes.Reset()
	supercluster := superclusterIn(testNamespace)
	observeSupercluster(supercluster, superclusterRollup{Capacity: reading(1000, 100, 500)})

	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{})
	key := types.NamespacedName{Namespace: testNamespace, Name: testSuperclusterName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := testutil.CollectAndCount(superclusterMembers) + testutil.CollectAndCount(superclusterCapacityBytes); got != 0 {
		t.Errorf("%d series remain for a supercluster that is gone", got)
	}
}
