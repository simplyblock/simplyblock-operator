package cluster

// The rolling restart on a stretched cluster: the leader walks both sites'
// nodes, and a node of the other site has its storage-node pod there, which
// only that site's operator can refresh (design-stretched-sync-replication-
// ramen.md §11.3).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const peerManagedCluster = "site-b-mc"

// fakePeer answers RefreshNode with refreshed once asked enough times.
type fakePeer struct {
	refreshedAfter int
	err            error
	asked          []peerRefresh
}

type peerRefresh struct {
	peerCluster, cluster, node string
	since                      time.Time
}

func (f *fakePeer) RefreshNode(_ context.Context, peerCluster, cluster, node string, since time.Time) (bool, error) {
	f.asked = append(f.asked, peerRefresh{peerCluster, cluster, node, since})
	if f.err != nil {
		return false, f.err
	}
	return len(f.asked) > f.refreshedAfter, nil
}

func stretchedLeaderCluster() *simplyblockv1alpha2.StorageCluster {
	return newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{
			Site: "a", PeerSite: "b", PeerCluster: peerManagedCluster, Role: simplyblockv1alpha2.StretchedRoleLeader,
		}
	})
}

// remoteNodeB is the StorageNode the leader's site lists for nodeB, a node of site b.
func remoteNodeB() *simplyblockv1alpha2.StorageNode {
	n := &simplyblockv1alpha2.StorageNode{ObjectMeta: objectMeta("remote-node-b")}
	n.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	n.Status.UUID = nodeB
	return n
}

func refreshingOps(created time.Time) *simplyblockv1alpha2.StorageClusterOps {
	return newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) { o.CreationTimestamp = metav1.NewTime(created) })
}

// The refresh of a node of the other site is asked of that site's operator,
// with the operation's start as the line a pod must be newer than, and the walk
// waits until it reports the replacement Ready.
func TestARemoteNodesPodIsRefreshedByThePeerSite(t *testing.T) {
	created := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ops := refreshingOps(created)
	peer := &fakePeer{refreshedAfter: 2}
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA, nodeB)), &recorder{},
		stretchedLeaderCluster(), ops, remoteNodeB())
	r.Peer = peer
	nodes := newRollingFleet(nodeA, nodeB).nodes()

	done, err := r.refreshPod(context.Background(), ops, testClusterUUID, nodes, nodeB)
	if err != nil || !done {
		t.Fatalf("refreshPod = %v, %v; want the refresh asked of the peer and the step done", done, err)
	}
	want := peerRefresh{peerManagedCluster, testClusterUUID, nodeB, created}
	if len(peer.asked) != 1 || peer.asked[0] != want {
		t.Fatalf("asked %+v, want %+v", peer.asked, want)
	}

	if done, err := r.awaitPod(context.Background(), ops, testClusterUUID, nodes, nodeB); err != nil || done {
		t.Fatalf("awaitPod = %v, %v; want held while the peer reports the refresh running", done, err)
	}
	if done, err := r.awaitPod(context.Background(), ops, testClusterUUID, nodes, nodeB); err != nil || !done {
		t.Fatalf("awaitPod = %v, %v; want done once the peer reports the replacement Ready", done, err)
	}
}

// Without a site link the other site cannot be asked: the walk holds, and says
// why, rather than skipping the node's refresh or waiting forever in silence.
func TestARemoteNodeWithoutASiteLinkHolds(t *testing.T) {
	ops := refreshingOps(time.Now())
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA, nodeB)), &recorder{},
		stretchedLeaderCluster(), ops, remoteNodeB())
	nodes := newRollingFleet(nodeA, nodeB).nodes()

	done, err := r.refreshPod(context.Background(), ops, testClusterUUID, nodes, nodeB)
	if done || err == nil || !strings.Contains(err.Error(), "site link") {
		t.Errorf("refreshPod = %v, %v; want a hold naming the site link", done, err)
	}
}

// A peer that cannot be reached holds the walk with its error.
func TestAnUnreachablePeerHoldsTheWalk(t *testing.T) {
	ops := refreshingOps(time.Now())
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA, nodeB)), &recorder{},
		stretchedLeaderCluster(), ops, remoteNodeB())
	r.Peer = &fakePeer{err: errors.New("no session to site b")}
	nodes := newRollingFleet(nodeA, nodeB).nodes()

	if done, err := r.awaitPod(context.Background(), ops, testClusterUUID, nodes, nodeB); done || err == nil {
		t.Errorf("awaitPod = %v, %v; want held with the peer's error", done, err)
	}
}

// A node of this site is refreshed here, as on any cluster: the peer is not asked.
func TestALocalNodeIsNotAskedOfThePeer(t *testing.T) {
	ops := refreshingOps(time.Now())
	peer := &fakePeer{}
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA, nodeB)), &recorder{},
		stretchedLeaderCluster(), ops, remoteNodeB())
	r.Peer = peer
	nodes := newRollingFleet(nodeA, nodeB).nodes()

	if _, err := r.refreshPod(context.Background(), ops, testClusterUUID, nodes, nodeA); err != nil {
		t.Fatal(err)
	}
	if len(peer.asked) != 0 {
		t.Errorf("the peer was asked to refresh %v, a node of this site", peer.asked)
	}
}
