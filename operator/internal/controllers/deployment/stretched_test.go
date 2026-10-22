// A deployment of one site of a stretched cluster: the delivered document's
// stretched block reaches the StorageCluster, its node groups must sit on its
// site, and the activation belongs to the leader, which waits for the other
// site's nodes before asking for it.

package deployment

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	stretchedSite = "b"
	stretchedPeer = "a"
)

func stretchedAs(role simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.StretchedSpec {
	return &simplyblockv1alpha2.StretchedSpec{Site: stretchedSite, PeerSite: stretchedPeer, Role: role}
}

// aStretchedDocument is a document for this site of a stretched cluster, with
// every node group on groupSite.
func aStretchedDocument(groupSite string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.EnableSyncReplication = ptr.To(true)
		c.Spec.Cluster.Stretched = stretchedAs(simplyblockv1alpha2.StretchedRoleLeader)
		for i := range c.Spec.NodeSets {
			for j := range c.Spec.NodeSets[i].Groups {
				c.Spec.NodeSets[i].Groups[j].Site = groupSite
			}
		}
	})
}

// The stretched block is expanded verbatim: it is what makes the StorageCluster
// a leader or a follower, so a copy that dropped it would create the cluster on
// a follower.
func TestTheExpansionCarriesTheStretchedBlock(t *testing.T) {
	config := aStretchedDocument(stretchedSite)
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	cluster, err := r.buildCluster(config, theCluster)
	if err != nil {
		t.Fatalf("buildCluster: %v", err)
	}
	if diff := cmp.Diff(config.Spec.Cluster.Stretched, cluster.Spec.Stretched); diff != "" {
		t.Errorf("spec.stretched (-template +cluster):\n%s", diff)
	}
}

// U-13: a node group of this site's document that names the other site would
// add nodes running here to the other site's half of the cluster.
func TestANodeGroupOnAnotherSiteIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		groupSite string
		want      bool
	}{
		{name: "the cluster's site", groupSite: stretchedSite},
		{name: "the peer site", groupSite: stretchedPeer, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := aStretchedDocument(tc.groupSite)
			r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

			findings, err := r.validate(context.Background(), config)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			var mismatch *finding
			for i := range findings {
				if findings[i].reason == SiteMismatch {
					mismatch = &findings[i]
				}
			}
			if tc.want && mismatch == nil {
				t.Fatalf("a group on site %q passed validation for a cluster on site %q (findings %+v)",
					tc.groupSite, stretchedSite, findings)
			}
			if !tc.want && mismatch != nil {
				t.Fatalf("a group on the cluster's own site was refused: %s", mismatch.message)
			}
			if tc.want && !strings.Contains(mismatch.message, stretchedPeer) {
				t.Errorf("the finding does not name the group's site: %s", mismatch.message)
			}
		})
	}
}

// The same holds for a document that grows an existing stretched cluster:
// the site to match is the cluster's.
func TestANodeGroupAddedToAStretchedClusterMustBeOnItsSite(t *testing.T) {
	config := aStretchedDocument(stretchedPeer)
	config.Spec.ClusterRef = theCluster
	config.Spec.Cluster = nil
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.EnableSyncReplication = ptr.To(true)
		c.Spec.Stretched = stretchedAs(simplyblockv1alpha2.StretchedRoleFollower)
	})
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config, cluster)...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range findings {
		if f.reason == SiteMismatch {
			return
		}
	}
	t.Fatalf("a group on the peer site passed validation against a stretched cluster (findings %+v)", findings)
}

// anExpandedStretchedDocument is anExpandedDocument for one site of a stretched
// cluster, with this site's nodes online and peers remote nodes of the other
// site, the first onlinePeers of them online.
func anExpandedStretchedDocument(
	t *testing.T, role simplyblockv1alpha2.StretchedRole, peers, onlinePeers int,
) (*simplyblockv1alpha2.ClusterDeploymentConfig, *ClusterDeploymentConfigReconciler) {
	t.Helper()
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	names := []string{theCluster + "-worker-1-0", theCluster + "-worker-2-0"}
	config.Status.NodeRefs = names

	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.EnableSyncReplication = ptr.To(true)
		c.Spec.Stretched = stretchedAs(role)
		// 1+1: the backend needs ndcs+npcs = 2 online nodes on each site.
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1))}
		c.Status.ErasureCodingScheme = "1x1"
	})
	objects := append(workers("worker-1", "worker-2"), config, cluster)
	for _, name := range names {
		node := &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: theNamespace},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: theCluster},
		}
		node.Status.Phase = simplyblockv1alpha2.StorageNodePhaseOnline
		objects = append(objects, node)
	}
	for i := 0; i < peers; i++ {
		peer := &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: theCluster + "-peer-" + string(rune('a'+i)), Namespace: theNamespace},
			Spec: simplyblockv1alpha2.StorageNodeSpec{
				ClusterRef: theCluster,
				Config:     simplyblockv1alpha2.StorageNodeConfig{Site: stretchedPeer},
				Stretched:  &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
			},
		}
		peer.Status.Status = "offline"
		if i < onlinePeers {
			peer.Status.Status = "online"
		}
		objects = append(objects, peer)
	}
	return config, reconcilerFor(t, objects...)
}

func raisedOps(t *testing.T, c client.Client) int {
	t.Helper()
	var ops simplyblockv1alpha2.StorageClusterOpsList
	if err := c.List(context.Background(), &ops); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	return len(ops.Items)
}

func emitted(r *ClusterDeploymentConfigReconciler, reason string) bool {
	recorder, ok := r.Recorder.(*events.FakeRecorder)
	if !ok {
		return false
	}
	for {
		select {
		case e := <-recorder.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}

// A follower's activation is its leader's: once its own nodes are up, the step
// is done without raising an operation, which a follower may not run.
func TestAFollowerLeavesTheActivationToItsLeader(t *testing.T) {
	config, r := anExpandedStretchedDocument(t, simplyblockv1alpha2.StretchedRoleFollower, 0, 0)

	done, err := r.activateCluster(context.Background(), config)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !done {
		t.Error("a follower's activation step did not finish with its nodes online")
	}
	if n := raisedOps(t, r.Client); n != 0 {
		t.Errorf("a follower raised %d operations; the activation is its leader's", n)
	}
	if !emitted(r, ActivationLeftToLeader) {
		t.Error("a follower skipping the activation emitted no ActivationLeftToLeader")
	}
}

// U-06 and U-07: the leader waits until the other site has ndcs+npcs online
// nodes, the backend's precondition for activating a two-site cluster, and asks
// for the activation at exactly that number.
func TestALeaderWaitsForThePeerSiteBeforeActivating(t *testing.T) {
	for _, tc := range []struct {
		name         string
		peers, alive int
		wantDone     bool
	}{
		{name: "no peer nodes yet", peers: 0, alive: 0},
		{name: "one fewer than ndcs+npcs online", peers: 3, alive: 1},
		{name: "exactly ndcs+npcs online", peers: 3, alive: 2, wantDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, r := anExpandedStretchedDocument(t, simplyblockv1alpha2.StretchedRoleLeader, tc.peers, tc.alive)

			done, err := r.activateCluster(context.Background(), config)
			if err != nil {
				t.Fatalf("activate: %v", err)
			}
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v", done, tc.wantDone)
			}
			want := 0
			if tc.wantDone {
				want = 1
			}
			if n := raisedOps(t, r.Client); n != want {
				t.Errorf("raised %d operations, want %d", n, want)
			}
			if !tc.wantDone && !emitted(r, WaitingForPeerSite) {
				t.Error("a leader holding for its peer site emitted no WaitingForPeerSite")
			}
		})
	}
}

// Regression: 2026-10-22-stretched-groups-need-a-site. A stretched cluster's site
// is the whole Kubernetes cluster, so the hub does not write a site into every
// node group of the draft the discovery wrote; a group left without one held the
// whole provisioning on MissingSites. On a stretched cluster an empty group site
// is the cluster's site.
func TestAGroupWithoutASiteTakesTheStretchedClustersSite(t *testing.T) {
	config := aStretchedDocument("")
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range findings {
		if f.reason == MissingSites {
			t.Fatalf("a stretched document's groups without a site held provisioning: %s", f.message)
		}
	}

	cluster, err := r.buildCluster(config, theCluster)
	if err != nil {
		t.Fatalf("buildCluster: %v", err)
	}
	set := config.Spec.NodeSets[0]
	node := r.buildNode(config, cluster, set, set.Groups[0], set.Groups[0].Workers[0], 0)
	if node.Spec.Config.Site != stretchedSite {
		t.Errorf("the node's site = %q, want the stretched cluster's site %q", node.Spec.Config.Site, stretchedSite)
	}
}

// A standalone sync cluster still needs a site on every group: it has no
// installation-wide site to default to.
func TestASyncClustersGroupsStillNeedASite(t *testing.T) {
	config := aStretchedDocument("")
	config.Spec.Cluster.Stretched = nil
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range findings {
		if f.reason == MissingSites {
			return
		}
	}
	t.Fatalf("a standalone sync document's groups without a site passed validation (findings %+v)", findings)
}
