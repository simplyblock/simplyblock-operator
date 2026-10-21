// Validation of a document that describes a supercluster: two or more node
// sets, each becoming a member cluster (design-superclusters.md §5.1 to §5.3).
// What spans one cluster spans one node set, and each node set is held to the
// limits of a cluster.

package deployment

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aNodeSet is one node set of one group on the given workers, on the given
// interfaces, with three block devices per worker.
func aNodeSet(name, mgmt string, workerNames ...string) simplyblockv1alpha2.NodeSet {
	return simplyblockv1alpha2.NodeSet{
		Name: name,
		Groups: []simplyblockv1alpha2.NodeGroup{{
			Name:           "group-1",
			Workers:        workerNames,
			MgmtInterface:  mgmt,
			DataInterfaces: []string{mgmt + "-data"},
			Devices: &simplyblockv1alpha2.DeviceSelection{
				Block: []string{"/dev/sdb", "/dev/sdc", "/dev/sdd"},
			},
		}},
	}
}

// aSuperclusterDocument is a draft of two node sets of one worker each, on two
// different networks, as single-node members striped 2+1 across their own
// three devices.
func aSuperclusterDocument(
	mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("fast", "eth1", "worker-1"),
			aNodeSet("dense", "eth3", "worker-2"),
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestADocumentOfTwoSingleNodeMembersOnTwoNetworksIsValid(t *testing.T) {
	if found := findingsOf(t, aSuperclusterDocument(nil), workers("worker-1", "worker-2")...); len(found) > 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}

// The erasure-coding minimum is a member's, since a member stripes across its
// own nodes alone: four workers of 1+1 are enough for one cluster and not for a
// member of three and a member of one.
func TestTheStripeMinimumIsCheckedPerMember(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.ClusterMode = ""
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("fast", "eth1", "worker-1", "worker-2", "worker-3"),
			aNodeSet("dense", "eth1", "worker-4"),
		}
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2", "worker-3", "worker-4")...),
		StripeBelowMinimumNodes)
	if !strings.Contains(found.message, "dense") {
		t.Errorf("the finding does not name the node set that is short: %s", found.message)
	}
}

// A cluster holds at most 32 storage nodes, a member as much as any other.
func TestANodeSetOverTheStorageNodeLimitIsReported(t *testing.T) {
	names := make([]string, 0, 33)
	for i := 1; i <= 33; i++ {
		names = append(names, fmt.Sprintf("worker-%d", i))
	}
	for _, sets := range [][]simplyblockv1alpha2.NodeSet{
		{aNodeSet("large", "eth1", names...)},
		{aNodeSet("large", "eth1", names...), aNodeSet("small", "eth1", "worker-99")},
	} {
		config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Approved = false
			c.Spec.NodeSets = sets
		})

		var objects []client.Object
		objects = append(objects, workers(append(names, "worker-99")...)...)
		var limit *finding
		for _, f := range findingsOf(t, config, objects...) {
			if f.reason == MemberTooLarge {
				limit = &f
			}
		}
		if limit == nil || !strings.Contains(limit.message, "large") || !strings.Contains(limit.message, "33") {
			t.Errorf("%d node sets: MemberTooLarge = %+v, want it to name node set large and its 33 nodes", len(sets), limit)
		}
	}
}

// A member is named <cluster>-<nodeset>-<suffix>, which a StorageCluster name
// must hold to, so a document whose names do not fit says so before approval.
func TestAMemberNameThatCannotBeANameIsReported(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Name = strings.Repeat("c", 50)
		c.Spec.NodeSets[1].Name = "a-long-node-set"
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2")...), MemberNameInvalid)
	if !strings.Contains(found.message, "a-long-node-set") {
		t.Errorf("the finding does not name the node set: %s", found.message)
	}
}

// Every cluster records workload names of its own (§6.1) and members share
// their supercluster's (§6.2), so a namespace holding a cluster takes another.
func TestADocumentBesideAnotherClusterIsValid(t *testing.T) {
	occupant := &simplyblockv1alpha2.StorageCluster{ObjectMeta: metav1.ObjectMeta{Name: "already-there", Namespace: theNamespace}}
	objects := append(workers("worker-1", "worker-2"), occupant, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unused"}})

	if found := findingsOf(t, aSuperclusterDocument(nil), objects...); len(found) > 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}
