package cluster

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func entryIDs(c CSICredentials) map[string]CSIClusterEntry {
	out := map[string]CSIClusterEntry{}
	for _, e := range c.Clusters {
		out[e.ClusterID] = e
	}
	return out
}

func TestMergeCSICredentialsRegistersEveryClusterOfTheControlPlane(t *testing.T) {
	// Site A's operator manages cluster A; the control plane also runs
	// cluster B (site B). A volume failed over from B to A arrives under a
	// PV whose handle names B: the driver on A must reach B's cluster too.
	creds := CSICredentials{}
	own := CSIClusterEntry{ClusterID: "A", ClusterEndpoint: "http://cp:5000", ClusterSecret: "sa", Local: true}
	peers := []utils.ClusterListEntry{{UUID: "A", Secret: "sa"}, {UUID: "B", Secret: "sb"}}
	mergeCSICredentials(&creds, own, peers, true)
	got := entryIDs(creds)
	if len(got) != 2 || !got["A"].Local || got["B"].Local || got["B"].ClusterSecret != "sb" || got["B"].ClusterEndpoint != "http://cp:5000" {
		t.Fatalf("entries %+v", creds.Clusters)
	}
}

func TestMergeCSICredentialsKeepsAnotherLocalEntryAndPrunesStaleForeignOnes(t *testing.T) {
	creds := CSICredentials{Clusters: []CSIClusterEntry{
		{ClusterID: "A2", ClusterEndpoint: "http://cp:5000", ClusterSecret: "sa2", Local: true}, // another operator here
		{ClusterID: "OLD", ClusterEndpoint: "http://cp:5000", ClusterSecret: "x"},               // a cluster since removed
		{ClusterID: "B", ClusterEndpoint: "http://cp:5000", ClusterSecret: "kept"},
	}}
	own := CSIClusterEntry{ClusterID: "A", ClusterEndpoint: "http://cp:5000", ClusterSecret: "sa", Local: true}
	// The list withholds B's secret: the recorded one stays.
	peers := []utils.ClusterListEntry{{UUID: "A"}, {UUID: "A2"}, {UUID: "B"}}
	mergeCSICredentials(&creds, own, peers, true)
	got := entryIDs(creds)
	if _, stale := got["OLD"]; stale {
		t.Fatal("a cluster the control plane no longer lists stays registered")
	}
	if !got["A2"].Local || got["A2"].ClusterSecret != "sa2" {
		t.Fatalf("the other operator's local entry changed: %+v", got["A2"])
	}
	if got["B"].Local || got["B"].ClusterSecret != "kept" {
		t.Fatalf("B: %+v", got["B"])
	}
	if !got["A"].Local {
		t.Fatalf("own: %+v", got["A"])
	}
}

func TestMergeCSICredentialsWithoutTheListOnlyWritesTheOwnEntry(t *testing.T) {
	creds := CSICredentials{Clusters: []CSIClusterEntry{{ClusterID: "B", ClusterSecret: "sb"}}}
	own := CSIClusterEntry{ClusterID: "A", ClusterSecret: "sa", Local: true}
	mergeCSICredentials(&creds, own, nil, false)
	got := entryIDs(creds)
	if len(got) != 2 || got["B"].ClusterSecret != "sb" || !got["A"].Local {
		t.Fatalf("entries %+v", creds.Clusters)
	}
}
