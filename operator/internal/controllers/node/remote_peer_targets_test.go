// A drain on one site of a stretched cluster moves volumes within that site.
// The control plane's node list carries both sites' nodes, so the drain's
// target choice must leave the peer site's nodes out.

package node

import (
	"testing"
)

// withSite places a node the control plane reports on one sync-replication site.
func (c *scriptedControlPlane) withSite(nodeID, site string) *scriptedControlPlane {
	reading := c.nodes[nodeID]
	reading.Site = site
	c.nodes[nodeID] = reading
	return c
}

// Regression: 2026-10-22-drain-targets-peer-site.
// A drain of a site-b node chose an online site-a node as a migration target,
// moving the volume to the other site of the stretched cluster.
func TestADrainNeverTargetsAPeerSiteNode(t *testing.T) {
	const siteAPeer = "node-aaaa"
	api := aControlPlane().
		withSite(opsNodeID, "site-b").
		withPeer(opsPeerID, nodeStatusOnline).
		withSite(opsPeerID, "site-b").
		withPeer(siteAPeer, nodeStatusOnline).
		withSite(siteAPeer, "site-a")

	targets, err := targetsOf(t, api, movable("pv-a", "pv-b", "pv-c", "pv-d"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	for volume, target := range targets {
		if target != opsPeerID {
			t.Errorf("volume %s was aimed at %s, want the site-b peer %s", volume, target, opsPeerID)
		}
	}
}

// A drain whose only online peer is on the other site has nowhere to go and
// holds, rather than moving the volumes across sites.
//
// Regression: 2026-10-22-drain-targets-peer-site.
// With no same-site peer online, the drain aimed every volume at the peer site.
func TestADrainWithOnlyAPeerSiteNodeHolds(t *testing.T) {
	api := aControlPlane().
		withSite(opsNodeID, "site-b").
		withPeer(opsPeerID, nodeStatusOnline).
		withSite(opsPeerID, "site-a")

	targets, err := targetsOf(t, api, movable("pv-a"))
	if err == nil {
		t.Fatalf("the drain chose targets %v across sites, want it to hold", targets)
	}
}
