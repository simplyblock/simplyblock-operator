// Validation of the CEL rules that make a stretched cluster's site and role
// facts safe to rely on: the StorageCluster's stretched block, the remote marker
// on a StorageNode, and the hub-side StretchedCluster. Run against a real
// apiserver with only the CRDs installed, so a rejection here can only have come
// from the schema (see cel_validation_test.go for why this package hosts it).

package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	siteA = "a"
	siteB = "b"
)

func stretchedCluster(stretched *simplyblockv1alpha2.StretchedSpec, sync *bool) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-str-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount:     ptr.To(int32(10)),
			VCPUCount:             ptr.To(int32(6)),
			EnableSyncReplication: sync,
			Stretched:             stretched,
		},
	}
}

// leaderWithPeer is site a's leader block, peered with peer.
func leaderWithPeer(peer string) *simplyblockv1alpha2.StretchedSpec {
	return &simplyblockv1alpha2.StretchedSpec{Site: siteA, PeerSite: peer, Role: simplyblockv1alpha2.StretchedRoleLeader}
}

func expectDenied(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the apiserver to refuse the write (%q), but it was accepted", message)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("refused for the wrong reason, want %q: %v", message, err)
	}
}

func cleanup(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
}

// U-10: a stretched cluster is a sync cluster by definition; the block alone,
// or with sync explicitly off, is refused.
func TestStorageClusterStretchedRequiresSyncReplication(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		sync       *bool
		wantDenied bool
	}{
		{name: "sync unset", sync: nil, wantDenied: true},
		{name: "sync off", sync: ptr.To(false), wantDenied: true},
		{name: "sync on", sync: ptr.To(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := stretchedCluster(leaderWithPeer(siteB), tc.sync)
			err := c.Create(ctx, cluster)
			if tc.wantDenied {
				expectDenied(t, err, "stretched requires enableSyncReplication to be true")
				return
			}
			if err != nil {
				t.Fatalf("expected a stretched sync cluster to be accepted: %v", err)
			}
			cleanup(t, c, cluster)
		})
	}
}

// A site and its peer naming the same backend site would make every node of
// both sites look local to both.
func TestStorageClusterStretchedSiteAndPeerMustDiffer(t *testing.T) {
	c := apiServer(t)
	err := c.Create(context.Background(), stretchedCluster(leaderWithPeer(siteA), ptr.To(true)))
	expectDenied(t, err, "site and peerSite must differ")
}

// U-11: the site and the peer site are facts of the installation; the role is
// the one stretched field a hub may change.
func TestStorageClusterStretchedSitesAreImmutableAndTheRoleIsNot(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	cluster := stretchedCluster(leaderWithPeer(siteB), ptr.To(true))
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, c, cluster)

	for _, tc := range []struct {
		name   string
		mutate func(*simplyblockv1alpha2.StretchedSpec)
	}{
		{name: "site", mutate: func(s *simplyblockv1alpha2.StretchedSpec) { s.Site = "c" }},
		{name: "peerSite", mutate: func(s *simplyblockv1alpha2.StretchedSpec) { s.PeerSite = "c" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := cluster.DeepCopy()
			tc.mutate(changed.Spec.Stretched)
			expectDenied(t, c.Update(ctx, changed), "immutable")
		})
	}

	t.Run("role", func(t *testing.T) {
		var fresh simplyblockv1alpha2.StorageCluster
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); err != nil {
			t.Fatalf("get: %v", err)
		}
		fresh.Spec.Stretched.Role = simplyblockv1alpha2.StretchedRoleFollower
		if err := c.Update(ctx, &fresh); err != nil {
			t.Fatalf("a role swap must be admitted: %v", err)
		}
	})
}

// A standalone cluster cannot become a site of a stretched one, nor a site stop
// being one: both would change which cluster owns the backend's create and
// delete under a running installation.
func TestStorageClusterStretchedCannotBeAddedOrRemoved(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	standalone := stretchedCluster(nil, ptr.To(true))
	if err := c.Create(ctx, standalone); err != nil {
		t.Fatalf("create standalone: %v", err)
	}
	cleanup(t, c, standalone)
	added := standalone.DeepCopy()
	added.Spec.Stretched = leaderWithPeer(siteB)
	expectDenied(t, c.Update(ctx, added), "stretched cannot be added to or removed from an existing cluster")

	stretched := stretchedCluster(leaderWithPeer(siteB), ptr.To(true))
	if err := c.Create(ctx, stretched); err != nil {
		t.Fatalf("create stretched: %v", err)
	}
	cleanup(t, c, stretched)
	removed := stretched.DeepCopy()
	removed.Spec.Stretched = nil
	expectDenied(t, c.Update(ctx, removed), "stretched cannot be added to or removed from an existing cluster")
}

// storageNode is a node of site b with no worker, so the worker rule decides it.
func storageNode(stretched *simplyblockv1alpha2.StretchedNodeSpec) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-sn-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "stretched",
			Config: simplyblockv1alpha2.StorageNodeConfig{
				Sizing: simplyblockv1alpha2.StorageNodeSizing{VCPUCount: ptr.To(int32(4))},
				Site:   siteB,
			},
			Stretched: stretched,
		},
	}
}

// A node of this site runs on one of this cluster's workers and must name it; a
// remote node runs on none and may not.
func TestStorageNodeWorkerIsRequiredUnlessRemote(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	expectDenied(t, c.Create(ctx, storageNode(nil)), "workerNode is required for a node of this site")
	expectDenied(t, c.Create(ctx, storageNode(&simplyblockv1alpha2.StretchedNodeSpec{})),
		"workerNode is required for a node of this site")

	remote := storageNode(&simplyblockv1alpha2.StretchedNodeSpec{Remote: true})
	if err := c.Create(ctx, remote); err != nil {
		t.Fatalf("a remote node without a worker must be accepted: %v", err)
	}
	cleanup(t, c, remote)
}

// U-88: whether a node is this site's or the other's is fixed for its life; a
// flip would hand a remote record a workload, or strand a managed node's.
func TestStorageNodeRemoteIsImmutable(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	remote := storageNode(&simplyblockv1alpha2.StretchedNodeSpec{Remote: true})
	if err := c.Create(ctx, remote); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	cleanup(t, c, remote)

	flipped := remote.DeepCopy()
	flipped.Spec.Stretched.Remote = false
	flipped.Spec.WorkerNode = "worker-1"
	expectDenied(t, c.Update(ctx, flipped), "immutable")

	dropped := remote.DeepCopy()
	dropped.Spec.Stretched = nil
	dropped.Spec.WorkerNode = "worker-1"
	expectDenied(t, c.Update(ctx, dropped), "stretched cannot be added to or removed from an existing node")
}

func stretchedRequest(mutate func(*simplyblockv1alpha2.StretchedClusterSpec)) *simplyblockv1alpha2.StretchedCluster {
	sc := &simplyblockv1alpha2.StretchedCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-stc-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StretchedClusterSpec{
			ClusterName: "production",
			Sites: []simplyblockv1alpha2.StretchedSite{
				{Cluster: "site-a", Site: siteA, Role: simplyblockv1alpha2.StretchedRoleLeader},
				{Cluster: "site-b", Site: siteB, Role: simplyblockv1alpha2.StretchedRoleFollower},
			},
		},
	}
	if mutate != nil {
		mutate(&sc.Spec)
	}
	return sc
}

// U-22: exactly one leader; two would both create the backend cluster, none
// would leave nobody to.
func TestStretchedClusterNeedsExactlyOneLeader(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		roles [2]simplyblockv1alpha2.StretchedRole
	}{
		{name: "two leaders", roles: [2]simplyblockv1alpha2.StretchedRole{
			simplyblockv1alpha2.StretchedRoleLeader, simplyblockv1alpha2.StretchedRoleLeader}},
		{name: "no leader", roles: [2]simplyblockv1alpha2.StretchedRole{
			simplyblockv1alpha2.StretchedRoleFollower, simplyblockv1alpha2.StretchedRoleFollower}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Create(ctx, stretchedRequest(func(s *simplyblockv1alpha2.StretchedClusterSpec) {
				s.Sites[0].Role, s.Sites[1].Role = tc.roles[0], tc.roles[1]
			}))
			expectDenied(t, err, "exactly one site must have role Leader")
		})
	}

	one := stretchedRequest(nil)
	if err := c.Create(ctx, one); err != nil {
		t.Fatalf("one leader must be accepted: %v", err)
	}
	cleanup(t, c, one)
}

func TestStretchedClusterSitesMustDiffer(t *testing.T) {
	c := apiServer(t)
	err := c.Create(context.Background(), stretchedRequest(func(s *simplyblockv1alpha2.StretchedClusterSpec) {
		s.Sites[1].Site = siteA
	}))
	expectDenied(t, err, "the two sites must name different backend sites")
}

// U-24: the managed clusters and the backend sites are fixed once the request
// exists; the roles are not.
func TestStretchedClusterClustersAndSitesAreImmutable(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	sc := stretchedRequest(nil)
	if err := c.Create(ctx, sc); err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, c, sc)

	replaced := sc.DeepCopy()
	replaced.Spec.Sites[1].Cluster = "site-c"
	expectDenied(t, c.Update(ctx, replaced), "the managed clusters of a stretched cluster cannot be replaced")

	renamed := sc.DeepCopy()
	renamed.Spec.Sites[1].Site = "c"
	expectDenied(t, c.Update(ctx, renamed), "immutable")

	var fresh simplyblockv1alpha2.StretchedCluster
	if err := c.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	fresh.Spec.Sites[0].Role = simplyblockv1alpha2.StretchedRoleFollower
	fresh.Spec.Sites[1].Role = simplyblockv1alpha2.StretchedRoleLeader
	if err := c.Update(ctx, &fresh); err != nil {
		t.Fatalf("a role swap must pass the schema (the webhook gates it on readiness): %v", err)
	}
}

// U-93 and U-94: approval is one-way, and an approved request's discovery is
// fixed; before approval, a site's discovery may change.
func TestStretchedClusterApprovalIsOneWayAndFreezesDiscovery(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()

	sc := stretchedRequest(nil)
	if err := c.Create(ctx, sc); err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, c, sc)

	var fresh simplyblockv1alpha2.StretchedCluster
	get := func() {
		if err := c.Get(ctx, client.ObjectKeyFromObject(sc), &fresh); err != nil {
			t.Fatalf("get: %v", err)
		}
	}

	get()
	fresh.Spec.Sites[1].Discover.NodeSelector = map[string]string{"storage": "yes"}
	if err := c.Update(ctx, &fresh); err != nil {
		t.Fatalf("a discovery change before approval must be admitted: %v", err)
	}

	get()
	fresh.Spec.Approved = true
	if err := c.Update(ctx, &fresh); err != nil {
		t.Fatalf("approve: %v", err)
	}

	get()
	unapproved := fresh.DeepCopy()
	unapproved.Spec.Approved = false
	expectDenied(t, c.Update(ctx, unapproved), "approval is one-way")

	rediscovered := fresh.DeepCopy()
	rediscovered.Spec.Sites[1].Discover.NodeSelector = map[string]string{"storage": "no"}
	expectDenied(t, c.Update(ctx, rediscovered), "discover cannot change once the stretched cluster is approved")
}
