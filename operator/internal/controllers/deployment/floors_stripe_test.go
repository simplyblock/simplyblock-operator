// The device and failure-domain floors of a document's erasure coding: every
// storage node hands over a data device, the cluster has enough of them for the
// stripe and its spares (fewer when raidDevices protects them below
// simplyblock), the journal device is counted out, and groups that declare
// failure domains declare one per chunk of a stripe.
//
// The cases are written against validate(), as the node-count cases are,
// because the thing being guaranteed is that a reviewer is told before approval.

package deployment

import (
	"fmt"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// stripeFindings keeps the findings about erasure coding, which is what these
// cases assert on. A fixture's other findings are other tests' business.
func stripeFindings(findings []finding) []finding {
	var out []finding
	for _, f := range findings {
		switch f.reason {
		case StripeUnsupported, StripeBelowMinimumNodes, StripeBelowMinimumWorkers,
			StripeSingleNodeCount, StripeBelowMinimumDevices, StripeBelowMinimumFailureDomains:
			out = append(out, f)
		}
	}
	return out
}

// groupsOf is one group per worker, naming count NVMe devices each, with the
// workers dealt round the failure domains given (none when domains is empty).
func groupsOf(workers []string, count int, domains []string) []simplyblockv1alpha2.NodeGroup {
	groups := make([]simplyblockv1alpha2.NodeGroup, 0, len(workers))
	for i, worker := range workers {
		group := simplyblockv1alpha2.NodeGroup{
			Name:           fmt.Sprintf("group-%d", i+1),
			Workers:        []string{worker},
			MgmtInterface:  "eth1",
			DataInterfaces: []string{"eth2"},
		}
		if count > 0 {
			devices := make([]string, 0, count)
			for d := 0; d < count; d++ {
				devices = append(devices, fmt.Sprintf("0000:5e:00.%d", d))
			}
			group.Devices = &simplyblockv1alpha2.DeviceSelection{NVMe: devices}
		}
		if len(domains) > 0 {
			group.FailureDomain = domains[i%len(domains)]
		}
		groups = append(groups, group)
	}
	return groups
}

func workerNames(count int) []string {
	names := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		names = append(names, fmt.Sprintf("worker-%d", i))
	}
	return names
}

// A document is held to what its nodes hand over and how they are spread, not
// only to how many there are.
func TestADraftIsHeldToTheDeviceAndFailureDomainFloors(t *testing.T) {
	stripe := func(d, p int32) *simplyblockv1alpha2.StripeSpec {
		return &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(d), ParityChunks: ptr.To(p)}
	}
	for _, tc := range []struct {
		name     string
		stripe   *simplyblockv1alpha2.StripeSpec
		workers  int
		devices  int // per worker, and 0 when the groups name none
		journal  bool
		slots    int32 // nodesPerSocket, and 0 when unstated
		dual     bool
		raid     bool
		domains  []string
		reason   string // "": no stripe finding
		mentions []string
	}{
		{name: "1+1 on three workers, a device each", stripe: stripe(1, 1), workers: 3, devices: 1},
		{name: "1+1 on three workers, a device each, taken by the journal", stripe: stripe(1, 1), workers: 3,
			devices: 1, journal: true, reason: StripeBelowMinimumDevices,
			mentions: []string{"group-1", "worker-1", "journal", "0"}},
		{name: "1+1 on three workers, two devices each, one the journal", stripe: stripe(1, 1), workers: 3,
			devices: 2, journal: true},
		{name: "1+1 on three workers, two nodes per worker, a device each", stripe: stripe(1, 1), workers: 3,
			devices: 1, slots: 2, reason: StripeBelowMinimumDevices, mentions: []string{"2 storage nodes", "group-1"}},
		{name: "1+1 on three workers, two nodes per worker, two devices each", stripe: stripe(1, 1), workers: 3,
			devices: 2, slots: 2},
		{name: "1+1 on three workers, two nodes per worker, a journal each, three devices", stripe: stripe(1, 1),
			workers: 3, devices: 3, slots: 2, journal: true, reason: StripeBelowMinimumDevices},
		{name: "1+1 on three workers, two nodes per worker, a journal each, four devices", stripe: stripe(1, 1),
			workers: 3, devices: 4, slots: 2, journal: true},
		{name: "two-node 1+1, a device each", stripe: stripe(1, 1), workers: 2, devices: 1, dual: true,
			reason: StripeBelowMinimumDevices, mentions: []string{"3 data devices", "spare", "2"}},
		{name: "two-node 1+1, a device each, RAID below", stripe: stripe(1, 1), workers: 2, devices: 1,
			dual: true, raid: true},
		{name: "two-node 1+1, two devices each", stripe: stripe(1, 1), workers: 2, devices: 2, dual: true},
		{name: "2+1 on four workers, devices unnamed", stripe: stripe(2, 1), workers: 4},
		{name: "2+1 on four workers in two failure domains", stripe: stripe(2, 1), workers: 4, devices: 1,
			domains: []string{"rack-a", "rack-b"}, reason: StripeBelowMinimumFailureDomains,
			mentions: []string{"3", "rack-a, rack-b", "produces 2"}},
		{name: "2+1 on four workers in three failure domains", stripe: stripe(2, 1), workers: 4, devices: 1,
			domains: []string{"rack-a", "rack-b", "rack-c"}},
		{name: "2+1 on four workers, no failure domains", stripe: stripe(2, 1), workers: 4, devices: 1},
		{name: "1+1 on three workers in one failure domain", stripe: stripe(1, 1), workers: 3, devices: 1,
			domains: []string{"rack-a"}, reason: StripeBelowMinimumFailureDomains},
		{name: "1+1 on three workers in two failure domains", stripe: stripe(1, 1), workers: 3, devices: 1,
			domains: []string{"rack-a", "rack-b"}},
		{name: "4+2 on eight workers in five failure domains", stripe: stripe(4, 2), workers: 8, devices: 1,
			domains: []string{"a", "b", "c", "d", "e"}, reason: StripeBelowMinimumFailureDomains,
			mentions: []string{"6"}},
		{name: "4+2 on eight workers in six failure domains", stripe: stripe(4, 2), workers: 8, devices: 1,
			domains: []string{"a", "b", "c", "d", "e", "f"}},
		{name: "2+1 on three workers in two failure domains is short of nodes first", stripe: stripe(2, 1),
			workers: 3, devices: 1, domains: []string{"rack-a", "rack-b"}, reason: StripeBelowMinimumNodes},
		{name: "1+0 on one worker, its device the journal", stripe: stripe(1, 0), workers: 1, devices: 1,
			journal: true, reason: StripeBelowMinimumDevices},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := workerNames(tc.workers)
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Approved = false
				c.Spec.Cluster.Stripe = tc.stripe
				c.Spec.NodeSets[0].Groups = groupsOf(names, tc.devices, tc.domains)
				if tc.journal {
					c.Spec.Cluster.EnableJournalDevice = ptr.To(true)
				}
				if tc.slots > 0 {
					c.Spec.Cluster.NodesPerSocket = ptr.To(tc.slots)
				}
				if tc.dual {
					c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeDual
				}
				if tc.raid {
					c.Spec.Cluster.RaidDevices = ptr.To(true)
				}
			})
			found := stripeFindings(findingsOf(t, config, workers(names...)...))
			if tc.reason == "" {
				if len(found) != 0 {
					t.Fatalf("stripe findings = %+v, want none", found)
				}
				return
			}
			if len(found) != 1 || found[0].reason != tc.reason {
				t.Fatalf("stripe findings = %+v, want one %s", found, tc.reason)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(found[0].message, want) {
					t.Errorf("the finding does not mention %q: %s", want, found[0].message)
				}
			}
		})
	}
}

// A growth document is answered with the nodes the cluster already has: their
// devices and failure domains count, a node that reports no device is named,
// and a node nobody can count holds the total back from being asserted.
func TestAGrowthDocumentCountsTheDevicesAndDomainsTheClusterHas(t *testing.T) {
	twoOne := &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1))}
	existing := func(name, worker, domain string, devices []string, reported *int32) *simplyblockv1alpha2.StorageNode {
		node := aNode(name, worker).(*simplyblockv1alpha2.StorageNode)
		node.Spec.Config.FailureDomain = domain
		node.Spec.Config.DeviceNames = devices
		if reported != nil {
			node.Status.Resources = &simplyblockv1alpha2.StorageNodeResources{
				Devices: &simplyblockv1alpha2.StorageNodeDevices{Online: *reported, Total: *reported},
			}
		}
		return node
	}
	growth := func(domains []string) *simplyblockv1alpha2.ClusterDeploymentConfig {
		return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Approved = false
			c.Spec.ClusterRef = theCluster
			c.Spec.Cluster = nil
			c.Spec.NodeSets[0].Groups = groupsOf([]string{"worker-3", "worker-4"}, 1, domains)
		})
	}
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.Stripe = twoOne })
	all := workers("worker-1", "worker-2", "worker-3", "worker-4")

	t.Run("the existing domains count toward the floor", func(t *testing.T) {
		objects := append(all, cluster,
			existing("node-1", "worker-1", "rack-a", []string{"0000:5e:00.0"}, nil),
			existing("node-2", "worker-2", "rack-b", []string{"0000:5e:00.0"}, nil))
		found := stripeFindings(findingsOf(t, growth([]string{"rack-c"}), objects...))
		if len(found) != 0 {
			t.Errorf("stripe findings = %+v, want none", found)
		}
	})

	t.Run("the existing domains are too few with the new ones", func(t *testing.T) {
		objects := append(all, cluster,
			existing("node-1", "worker-1", "rack-a", []string{"0000:5e:00.0"}, nil),
			existing("node-2", "worker-2", "rack-b", []string{"0000:5e:00.0"}, nil))
		found := stripeFindings(findingsOf(t, growth([]string{"rack-a"}), objects...))
		if len(found) != 1 || found[0].reason != StripeBelowMinimumFailureDomains {
			t.Fatalf("stripe findings = %+v, want one %s", found, StripeBelowMinimumFailureDomains)
		}
		for _, want := range []string{"leaves the cluster with 2", "rack-a, rack-b"} {
			if !strings.Contains(found[0].message, want) {
				t.Errorf("the finding does not mention %q: %s", want, found[0].message)
			}
		}
	})

	t.Run("a node reporting no device is named", func(t *testing.T) {
		objects := append(all, cluster,
			existing("node-1", "worker-1", "", nil, ptr.To(int32(0))),
			existing("node-2", "worker-2", "", []string{"0000:5e:00.0"}, nil))
		found := stripeFindings(findingsOf(t, growth(nil), objects...))
		if len(found) != 1 || found[0].reason != StripeBelowMinimumDevices {
			t.Fatalf("stripe findings = %+v, want one %s", found, StripeBelowMinimumDevices)
		}
		for _, want := range []string{"node-1", "worker-1"} {
			if !strings.Contains(found[0].message, want) {
				t.Errorf("the finding does not mention %q: %s", want, found[0].message)
			}
		}
	})

	t.Run("a node nobody can count is not reported", func(t *testing.T) {
		objects := append(all, cluster,
			existing("node-1", "worker-1", "", nil, nil),
			existing("node-2", "worker-2", "", nil, nil))
		if found := stripeFindings(findingsOf(t, growth(nil), objects...)); len(found) != 0 {
			t.Errorf("stripe findings = %+v, want none", found)
		}
	})
}

// A declared single-node cluster keeps its own rule, a device per chunk of a
// stripe on the one node, and is not held to the failure-domain floor its one
// node could never meet.
func TestASingleNodeDraftIsNotHeldToTheFailureDomainFloor(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.NodeSets[0].Groups = groupsOf([]string{"worker-1"}, 3, []string{"rack-a"})
	})
	if found := stripeFindings(findingsOf(t, config, workers("worker-1")...)); len(found) != 0 {
		t.Errorf("stripe findings = %+v, want none", found)
	}
}

// The raidDevices declaration reaches the cluster the document creates, as the
// other immutable layout fields do, stated or unstated.
func TestTheRaidDevicesDeclarationReachesTheCluster(t *testing.T) {
	for _, stated := range []*bool{ptr.To(true), ptr.To(false), nil} {
		config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.RaidDevices = stated
		})
		r := reconcilerFor(t)

		cluster, err := r.buildCluster(config, theCluster)
		if err != nil {
			t.Fatalf("buildCluster: %v", err)
		}
		switch {
		case stated == nil && cluster.Spec.RaidDevices != nil:
			t.Errorf("a document that says nothing produced %v", *cluster.Spec.RaidDevices)
		case stated != nil && cluster.Spec.RaidDevices == nil:
			t.Errorf("a document that said %v produced nothing", *stated)
		case stated != nil && *cluster.Spec.RaidDevices != *stated:
			t.Errorf("the cluster got %v, want the document's %v", *cluster.Spec.RaidDevices, *stated)
		}
	}
}
