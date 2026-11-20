// What a document asks of its members beyond the expansion itself
// (design-superclusters.md §5.1 to §5.3): a worker appears once, a member's
// failure domains are its own node set's, its size is counted in slots, its
// name is a DNS label, and a document of one node set makes no supercluster.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// U-19: a worker in two groups is a finding whatever the groups are called,
// because a storage node is identified by its worker and slot alone.
func TestAWorkerInTwoDifferentlyNamedGroupsIsReported(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		second := c.Spec.NodeSets[0].Groups[0]
		second.Name = "group-2"
		c.Spec.NodeSets[0].Groups = append(c.Spec.NodeSets[0].Groups, second)
	})

	found := findingOf(findingsOf(t, config, workers("worker-1", "worker-2")...), WorkerNotFound)
	if found == nil || !strings.Contains(found.message, "worker-1") {
		t.Errorf("findings = %+v, want worker-1 named as in more than one group", found)
	}
}

// U-76: a member whose groups name three failure domains maps all three onto
// its own cluster.
func TestAMemberMapsEveryFailureDomainOfItsNodeSet(t *testing.T) {
	config, objects := expandedMembers(true)
	config.Spec.NodeSets[0] = withDomains("fast", map[string]string{
		"worker-1": "rack-a", "worker-3": "rack-b", "worker-4": "rack-c",
	})
	r := reconcilerFor(t, append(objects, workers("worker-3", "worker-4")...)...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	want := []simplyblockv1alpha2.FailureDomainIndex{
		{Name: "rack-a", Index: 0}, {Name: "rack-b", Index: 1}, {Name: "rack-c", Index: 2},
	}
	if diff := cmp.Diff(want, readCluster(t, r, theCluster+"-fast-kqv").Status.FailureDomains); diff != "" {
		t.Errorf("the fast member's failure domains (-want +got):\n%s", diff)
	}
	if got := readCluster(t, r, theCluster+"-dense-abc").Status.FailureDomains; len(got) != 0 {
		t.Errorf("the dense member mapped %+v, want none of the fast node set's", got)
	}
}

// U-84: at two slots per worker, sixteen workers are a full member and
// seventeen are too many.
func TestAtTwoSlotsSixteenWorkersFitAndSeventeenDoNot(t *testing.T) {
	for _, tc := range []struct {
		workers int
		refused bool
	}{{16, false}, {17, true}} {
		names := make([]string, 0, tc.workers)
		for i := 1; i <= tc.workers; i++ {
			names = append(names, fmt.Sprintf("worker-%d", i))
		}
		config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.SocketsToUse = []string{"0", "1"}
			c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
				aNodeSet("large", "eth1", names...),
				aNodeSet("small", "eth1", "worker-99"),
			}
		})
		found := findingOf(findingsOf(t, config, workers(append(names, "worker-99")...)...), MemberTooLarge)
		if (found != nil) != tc.refused {
			t.Errorf("%d workers at two slots: MemberTooLarge = %+v, want refused=%v", tc.workers, found, tc.refused)
		}
	}
}

// U-154: a member's name is <cluster>-<nodeset>-<suffix>, so a cluster name of
// 35 characters and a node set name of 23 make a name of 63, and one more
// character in either is a name no StorageCluster can carry.
func TestTheMemberNameLimitIsSixtyThree(t *testing.T) {
	for _, tc := range []struct {
		cluster, nodeSet int
		refused          bool
	}{{35, 23, false}, {36, 23, true}, {35, 24, true}} {
		config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.Name = strings.Repeat("c", tc.cluster)
			c.Spec.NodeSets[1].Name = strings.Repeat("n", tc.nodeSet)
		})
		found := findingOf(findingsOf(t, config, workers("worker-1", "worker-2")...), MemberNameInvalid)
		if (found != nil) != tc.refused {
			t.Errorf("cluster %d, node set %d: MemberNameInvalid = %+v, want refused=%v",
				tc.cluster, tc.nodeSet, found, tc.refused)
		}
	}
}

// U-153: two growth documents whose node sets share a name add two members,
// told apart by their suffixes.
func TestTwoGrowthDocumentsWithAlikeNodeSetNamesAddTwoMembers(t *testing.T) {
	objects := aGrownSupercluster()
	first := aMembersDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Name = "first"
		c.Spec.Approved = true
		c.Spec.NodeSets = c.Spec.NodeSets[:1]
	})
	second := first.DeepCopy()
	second.Name = "second"
	second.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-3"}
	r := reconcilerFor(t, append(objects, first, second)...)

	for _, config := range []*simplyblockv1alpha2.ClusterDeploymentConfig{first, second} {
		if done, err := r.createCluster(context.Background(), config); err != nil || !done {
			t.Fatalf("createCluster %s: done=%v err=%v", config.Name, done, err)
		}
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(context.Background(), &clusters); err != nil {
		t.Fatalf("list the clusters: %v", err)
	}
	var added []string
	for _, cluster := range clusters.Items {
		if cluster.Spec.SuperclusterRef == grownSupercluster && strings.HasPrefix(cluster.Name, grownSupercluster+"-b-") {
			added = append(added, cluster.Name)
		}
	}
	if len(added) != 2 || added[0] == added[1] {
		t.Errorf("members added from node set b = %v, want two distinct ones", added)
	}
}

// I-13: a document of one node set expands into one cluster, as it always has,
// and makes no supercluster.
func TestADocumentOfOneNodeSetMakesNoSupercluster(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = true
	})
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(context.Background(), &clusters); err != nil {
		t.Fatalf("list the clusters: %v", err)
	}
	if len(clusters.Items) != 1 || clusters.Items[0].Spec.Supercluster != nil ||
		clusters.Items[0].Spec.SuperclusterRef != "" {
		t.Errorf("clusters = %d, want one standalone cluster", len(clusters.Items))
	}
	if got := readDocument(t, r).Status.Members; len(got) != 0 {
		t.Errorf("status.members = %+v, want none", got)
	}
}
