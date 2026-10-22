// A node set's overrides of the cluster template (design-superclusters.md §5.8):
// the member is created with the merged values, its nodes carry them, and the
// checks that span a cluster read the member's own scheme and slots.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// denseOverrides is the sizing the dense node set states in place of the
// template's.
func denseOverrides() *simplyblockv1alpha2.NodeSetClusterOverrides {
	return &simplyblockv1alpha2.NodeSetClusterOverrides{
		VCPUCount:         ptr.To(int32(12)),
		MaxSubsystemCount: ptr.To(int32(50)),
		MinHugePagesSize:  "1T",
		SocketsToUse:      []string{"0", "1"},
		NodesPerSocket:    ptr.To(int32(2)),
		Stripe: &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
		},
	}
}

// The member is created with the node set's values where it states them and
// the template's elsewhere, and the other member keeps the template.
func TestAMemberIsCreatedWithItsNodeSetsOverrides(t *testing.T) {
	config := expandingDocument()
	config.Spec.NodeSets[1].Cluster = denseOverrides()
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	for _, entry := range readDocument(t, r).Status.Members {
		member := readCluster(t, r, entry.Cluster)
		vcpu, subsystems, pages, data, sockets, perSocket := int32(8), int32(20), "100G", int32(1), 0, int32(0)
		if entry.NodeSet == "dense" {
			vcpu, subsystems, pages, data, sockets, perSocket = 12, 50, "1T", 2, 2, 2
		}
		if ptr.FromOrZero(member.Spec.VCPUCount) != vcpu ||
			ptr.FromOrZero(member.Spec.MaxSubsystemCount) != subsystems ||
			member.Spec.MinHugePagesSize != pages ||
			member.Spec.Stripe == nil || ptr.FromOrZero(member.Spec.Stripe.DataChunks) != data {
			t.Errorf("member of node set %s: vcpu %v subsystems %v pages %q stripe %+v, want %d, %d, %q, %d data chunks",
				entry.NodeSet, ptr.FromOrZero(member.Spec.VCPUCount), ptr.FromOrZero(member.Spec.MaxSubsystemCount),
				member.Spec.MinHugePagesSize, member.Spec.Stripe, vcpu, subsystems, pages, data)
		}
		// The dense node set's override carries its parity through; approval
		// refuses it on a single-node member, which
		// TestTheStripeMinimumIsCheckedAgainstTheMembersOwnScheme covers.
		parity := int32(0)
		if entry.NodeSet == "dense" {
			parity = 1
		}
		if ptr.FromOrZero(member.Spec.Stripe.ParityChunks) != parity {
			t.Errorf("member of node set %s: parity chunks %v, want %d",
				entry.NodeSet, ptr.FromOrZero(member.Spec.Stripe.ParityChunks), parity)
		}
		layout := member.Spec.StorageNodes
		if len(layout.SocketsToUse) != sockets || ptr.FromOrZero(layout.NodesPerSocket) != perSocket {
			t.Errorf("member of node set %s: sockets %v, %v per socket, want %d sockets at %d",
				entry.NodeSet, layout.SocketsToUse, ptr.FromOrZero(layout.NodesPerSocket), sockets, perSocket)
		}
	}
}

// The stripe is checked against each member's own scheme: a single-node
// member runs the template's 1+0, and the node set that overrides it to 2+1 is
// the one reported.
func TestTheStripeMinimumIsCheckedAgainstTheMembersOwnScheme(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[1].Cluster = &simplyblockv1alpha2.NodeSetClusterOverrides{
			Stripe: &simplyblockv1alpha2.StripeSpec{
				DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
			},
		}
	})

	found := findingsOf(t, config, workers("worker-1", "worker-2")...)
	messages := make([]string, 0, len(found))
	for _, f := range found {
		messages = append(messages, f.message)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "node set dense") || strings.Contains(joined, "node set fast") {
		t.Errorf("findings = %v, want the dense node set's scheme reported and the fast one's not", messages)
	}
}

// The storage-node limit counts the node set's own slots: twenty workers at
// two nodes each are forty storage nodes, over the limit of thirty-two.
func TestTheSizeLimitCountsTheNodeSetsOwnSlots(t *testing.T) {
	names := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		names = append(names, fmt.Sprintf("worker-%d", i))
	}
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("large", "eth1", names...),
			aNodeSet("small", "eth1", "worker-99"),
		}
		c.Spec.NodeSets[0].Cluster = &simplyblockv1alpha2.NodeSetClusterOverrides{NodesPerSocket: ptr.To(int32(2))}
	})

	var limit *finding
	for _, f := range findingsOf(t, config, workers(append(names, "worker-99")...)...) {
		if f.reason == MemberTooLarge {
			limit = &f
		}
	}
	if limit == nil || !strings.Contains(limit.message, "large") || !strings.Contains(limit.message, "40") {
		t.Errorf("MemberTooLarge = %+v, want it to name node set large and its 40 nodes", limit)
	}
}

// Sixteen workers at two slots and thirty-two at one are both a full member,
// and neither is over the limit.
func TestSlotsAreCountedPerNodeSet(t *testing.T) {
	names := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, fmt.Sprintf("%s-%d", prefix, i))
		}
		return out
	}
	doubled, single := names("double", 16), names("single", 32)
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("doubled", "eth1", doubled...),
			aNodeSet("single", "eth1", single...),
		}
		c.Spec.NodeSets[0].Cluster = &simplyblockv1alpha2.NodeSetClusterOverrides{NodesPerSocket: ptr.To(int32(2))}
	})

	for _, f := range findingsOf(t, config, workers(append(doubled, single...)...)...) {
		if f.reason == MemberTooLarge {
			t.Errorf("MemberTooLarge = %s, want none for two members of 32 storage nodes", f.message)
		}
	}
}
