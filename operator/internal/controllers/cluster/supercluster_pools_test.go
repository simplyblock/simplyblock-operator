// The supercluster's view of its pools (design-superclusters.md §7.1, §10.1):
// a member is given new volumes only once it holds every pool of the
// supercluster, and the supercluster is created with a default pool of its
// own, whose class names the supercluster.

package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// aSuperclusterPool is a pool of the test supercluster whose rows report it
// Ready in the members named.
func aSuperclusterPool(name string, readyIn ...string) *simplyblockv1alpha2.StoragePool {
	p := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: objectMeta(name),
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testSuperclusterName},
	}
	for _, member := range readyIn {
		p.Status.Members = append(p.Status.Members, simplyblockv1alpha2.PoolMemberStatus{
			Cluster: member, UUID: "pool-" + member, Phase: simplyblockv1alpha2.StoragePoolPhaseReady,
		})
	}
	return p
}

func memberStatus(
	t *testing.T, sc *simplyblockv1alpha2.StorageCluster, name string,
) simplyblockv1alpha2.SuperclusterMemberStatus {
	t.Helper()
	for _, row := range sc.Status.Members {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("no member row %s in %+v", name, sc.Status.Members)
	return simplyblockv1alpha2.SuperclusterMemberStatus{}
}

// §10.1 step 4: a member that does not hold every pool of the supercluster is
// not given new volumes, and the event names the pool it lacks. Once it holds
// the pool, it returns to placement.
func TestAMemberWithoutEveryPoolIsNotPlacementEligible(t *testing.T) {
	rec := &recorder{}
	r := newSuperclusterReconciler(t, nil, rec,
		newTestSupercluster(func(c *simplyblockv1alpha2.StorageCluster) {
			c.Status.Members = []simplyblockv1alpha2.SuperclusterMemberStatus{
				{Name: "prod-dense-kqv", PlacementEligible: true},
				{Name: "prod-fast-mzt", PlacementEligible: true},
			}
		}),
		newTestMember("prod-dense-kqv", "uuid-a"),
		newTestMember("prod-fast-mzt", "uuid-b"),
		aSuperclusterPool("tenant-a", "prod-dense-kqv", "prod-fast-mzt"),
		aSuperclusterPool("tenant-b", "prod-dense-kqv"),
	)

	_, sc := reconcileSupercluster(t, r)

	if !memberStatus(t, sc, "prod-dense-kqv").PlacementEligible {
		t.Error("the member holding every pool is not eligible")
	}
	if memberStatus(t, sc, "prod-fast-mzt").PlacementEligible {
		t.Error("the member lacking tenant-b is eligible")
	}
	if got := rec.noteFor(MemberPlacementChanged); rec.count(MemberPlacementChanged) != 1 ||
		!strings.Contains(got, "prod-fast-mzt") || !strings.Contains(got, "tenant-b") {
		t.Errorf("%s events = %d (%q), want one naming prod-fast-mzt and tenant-b",
			MemberPlacementChanged, rec.count(MemberPlacementChanged), got)
	}

	var tenantB simplyblockv1alpha2.StoragePool
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "tenant-b"}, &tenantB); err != nil {
		t.Fatal(err)
	}
	tenantB.Status.Members = append(tenantB.Status.Members, simplyblockv1alpha2.PoolMemberStatus{
		Cluster: "prod-fast-mzt", UUID: "pool-b", Phase: simplyblockv1alpha2.StoragePoolPhaseReady,
	})
	if err := r.Status().Update(context.Background(), &tenantB); err != nil {
		t.Fatal(err)
	}
	_, sc = reconcileSupercluster(t, r)
	if !memberStatus(t, sc, "prod-fast-mzt").PlacementEligible {
		t.Error("the member is not eligible once it holds every pool")
	}
}

// A pool of an ordinary cluster in the namespace is not the supercluster's,
// and a member need not hold it.
func TestAnotherClustersPoolIsNotRequiredOfAMember(t *testing.T) {
	other := aSuperclusterPool("standalone-pool")
	other.Spec.ClusterRef = "standalone"
	r := newSuperclusterReconciler(t, nil, &recorder{},
		newTestSupercluster(),
		newTestMember("prod-dense-kqv", "uuid-a"),
		other,
	)

	_, sc := reconcileSupercluster(t, r)

	if !memberStatus(t, sc, "prod-dense-kqv").PlacementEligible {
		t.Error("a member is held to another cluster's pool")
	}
}

// The supercluster is created with a default pool, owned by it and named for
// it, which the pool controller writes the supercluster's class for. It is
// written once: a deleted default pool is not recreated.
func TestASuperclusterIsCreatedWithADefaultPool(t *testing.T) {
	r := newSuperclusterReconciler(t, nil, &recorder{}, newTestSupercluster())

	_, sc := reconcileSupercluster(t, r)

	var p simplyblockv1alpha2.StoragePool
	key := client.ObjectKey{Namespace: testNamespace, Name: pool.DefaultPoolName(testSuperclusterName)}
	if err := r.Get(context.Background(), key, &p); err != nil {
		t.Fatalf("the supercluster has no default pool: %v", err)
	}
	if p.Spec.ClusterRef != testSuperclusterName || !metav1.IsControlledBy(&p, sc) {
		t.Errorf("default pool = %+v, want one owned by and naming the supercluster", p.ObjectMeta)
	}

	if err := r.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileSupercluster(t, r)
	if err := r.Get(context.Background(), key, &p); err == nil {
		t.Error("a deleted default pool was written again")
	}
}
