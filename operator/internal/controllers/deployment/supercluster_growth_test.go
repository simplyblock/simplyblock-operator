// Growth of a supercluster (design-superclusters.md §5.6): a document naming a
// supercluster in clusterRef adds one member per node set, one naming a member
// adds nodes to it, and the storage-node limit counts what the cluster
// already has.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	grownSupercluster = "prod"
	existingMember    = grownSupercluster + "-a-xyz"
)

// aGrownSupercluster is a supercluster with one member, whose node is on
// worker-1.
func aGrownSupercluster() []client.Object {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: grownSupercluster, Namespace: theNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
	member := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Name = existingMember
		c.Spec.SuperclusterRef = grownSupercluster
		c.Spec.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0))}
	})
	return append(workers("worker-1", "worker-2", "worker-3"), supercluster, member, aStorageNode(existingMember, "worker-1"))
}

// aStorageNode is a node of the cluster on the worker, at slot 0.
func aStorageNode(cluster, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: cluster + "-" + worker + "-0", Namespace: theNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: cluster, WorkerNode: worker, Slot: ptr.To(int32(0)),
		},
	}
}

// aMembersDocument names the supercluster and adds node sets b and c, on
// worker-2 and worker-3, at the single-node sizing of aSuperclusterDocument.
func aMembersDocument(mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig)) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = grownSupercluster
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("b", "eth1", "worker-2"),
			aNodeSet("c", "eth1", "worker-3"),
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestADocumentAddingMembersAtTheirSizingIsValid(t *testing.T) {
	if found := findingsOf(t, aMembersDocument(nil), aGrownSupercluster()...); len(found) > 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}

// A member's sizing is the document's to state, since the supercluster holds
// none of its own.
func TestADocumentAddingMembersWithoutSizingIsRefused(t *testing.T) {
	config := aMembersDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster = nil
	})

	found := findingsOf(t, config, aGrownSupercluster()...)
	reasons := make([]string, 0, len(found))
	for _, f := range found {
		reasons = append(reasons, f.reason)
	}
	if !strings.Contains(strings.Join(reasons, " "), MemberSizingUnstated) {
		t.Errorf("findings = %v, want %s", reasons, MemberSizingUnstated)
	}
}

// One new member per node set, named after the supercluster rather than the
// template, naming the supercluster, and with a suffix no member of the
// supercluster carries already.
func TestADocumentNamingASuperclusterCreatesAMemberPerNodeSet(t *testing.T) {
	draws := []string{"xyz", "def", "ghi"}
	drawSuffix = func() (string, error) {
		next := draws[0]
		draws = draws[1:]
		return next, nil
	}
	t.Cleanup(func() { drawSuffix = drawMemberSuffix })

	config := aMembersDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) { c.Spec.Approved = true })
	r := reconcilerFor(t, append(aGrownSupercluster(), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	stored := readDocument(t, r)
	if stored.Status.ClusterRef != grownSupercluster {
		t.Errorf("status.clusterRef = %q, want the supercluster", stored.Status.ClusterRef)
	}
	want := map[string]string{"b": grownSupercluster + "-b-def", "c": grownSupercluster + "-c-ghi"}
	if len(stored.Status.Members) != 2 {
		t.Fatalf("status.members = %+v, want one per node set", stored.Status.Members)
	}
	for _, entry := range stored.Status.Members {
		if entry.Cluster != want[entry.NodeSet] {
			t.Errorf("member of node set %s is %s, want %s", entry.NodeSet, entry.Cluster, want[entry.NodeSet])
		}
		if member := readCluster(t, r, entry.Cluster); member.Spec.SuperclusterRef != grownSupercluster {
			t.Errorf("member %s names supercluster %q", member.Name, member.Spec.SuperclusterRef)
		}
	}
	if taken, err := r.clusterExists(context.Background(), theNamespace, theCluster); err != nil || taken {
		t.Errorf("a cluster named after the template exists (%v, %v), want the template's name unused", taken, err)
	}
}

// A new member's nodes are created in the member, not in the supercluster.
func TestANewMembersNodesAreItsOwn(t *testing.T) {
	config := aMembersDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = true
		c.Spec.NodeSets = c.Spec.NodeSets[:1]
		c.Status.ClusterRef = grownSupercluster
		c.Status.Members = []simplyblockv1alpha2.ExpansionMember{{NodeSet: "b", Cluster: grownSupercluster + "-b-def"}}
	})
	member := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Name = grownSupercluster + "-b-def"
		c.Spec.SuperclusterRef = grownSupercluster
	})
	r := reconcilerFor(t, append(aGrownSupercluster(), member, config)...)

	if done, err := r.createNodes(context.Background(), config); err != nil || !done {
		t.Fatalf("createNodes: done=%v err=%v", done, err)
	}
	if owners := nodeOwners(t, r); owners["worker-2"] != member.Name {
		t.Errorf("nodes by worker = %v, want worker-2 in %s", owners, member.Name)
	}
}

// The storage-node limit counts the nodes the cluster already has: eight
// workers added to a member of thirty are refused, naming both numbers.
func TestAddingNodesToAFullMemberIsRefused(t *testing.T) {
	objects := aGrownSupercluster()
	for i := 2; i <= 30; i++ {
		objects = append(objects, aStorageNode(existingMember, fmt.Sprintf("held-%d", i)))
	}
	added := make([]string, 0, 8)
	for i := 1; i <= 8; i++ {
		added = append(added, fmt.Sprintf("new-%d", i))
	}
	objects = append(objects, workers(added...)...)
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = existingMember
		c.Spec.Cluster = nil
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{aNodeSet("more", "eth1", added...)}
	})

	var limit *finding
	for _, f := range findingsOf(t, config, objects...) {
		if f.reason == MemberTooLarge {
			limit = &f
		}
	}
	if limit == nil || !strings.Contains(limit.message, "8") || !strings.Contains(limit.message, "30") {
		t.Errorf("MemberTooLarge = %+v, want it to name the 8 added and the 30 held", limit)
	}
}

// A document naming a member grows that member like any other cluster: its
// nodes are the member's, and no member is drawn.
func TestADocumentNamingAMemberAddsNodesToIt(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = existingMember
		c.Spec.Cluster = nil
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{aNodeSet("more", "eth1", "worker-2")}
	})
	r := reconcilerFor(t, append(aGrownSupercluster(), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}
	config = readDocument(t, r)
	if config.Status.ClusterRef != existingMember || len(config.Status.Members) != 0 {
		t.Fatalf("status.clusterRef = %q, members %+v, want the member and no members drawn",
			config.Status.ClusterRef, config.Status.Members)
	}
	if done, err := r.createNodes(context.Background(), config); err != nil || !done {
		t.Fatalf("createNodes: done=%v err=%v", done, err)
	}
	if owners := nodeOwners(t, r); owners["worker-2"] != existingMember {
		t.Errorf("nodes by worker = %v, want worker-2 in %s", owners, existingMember)
	}
}
