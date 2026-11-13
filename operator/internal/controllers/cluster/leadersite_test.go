// The leader site on the backend cluster record and the follower's worker
// reachability check (design-stretched-sync-replication-ramen.md §5.4, §11.1).

package cluster

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The leader's create names its own site as the cluster's leader.
func TestALeadersCreateNamesItsSiteTheLeader(t *testing.T) {
	var posted atlascp.CreateClusterParams
	api := &fakeControlPlane{
		create: func(p atlascp.CreateClusterParams) (atlascp.Cluster, error) {
			posted = p
			reading := syncCluster()
			reading.Secret = testClusterSecret
			return reading, nil
		},
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)))
	reconcileCluster(t, r, 6)
	if posted.LeaderSite != thisSite {
		t.Errorf("the create's leader_site = %q, want %q", posted.LeaderSite, thisSite)
	}
}

// U-05: a follower handed a cluster whose record names the follower's own site
// as leader would make a second leader; it refuses with RoleConflict.
func TestAFollowerRefusesAClusterThatNamesItsSiteTheLeader(t *testing.T) {
	reading := syncCluster()
	reading.LeaderSite = thisSite
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return reading, nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != "" {
		t.Fatalf("status.uuid = %q: the follower adopted a cluster that names its site the leader", cluster.Status.UUID)
	}
	if !rec.has(RoleConflict) || !strings.Contains(cluster.Status.Message, "leader") {
		t.Errorf("message %q; want a RoleConflict hold naming the leader site", cluster.Status.Message)
	}
}

// U-82: the site acting as leader writes its own site to the cluster record, so
// the record follows a role swap; a follower, a record already naming this
// site, and a site the control plane records as lost write nothing.
func TestTheLeaderKeepsTheClusterRecordNamingItsSite(t *testing.T) {
	for name, tc := range map[string]struct {
		role             simplyblockv1alpha2.StretchedRole
		leaderSite, lost string
		want             int
	}{
		"leader, record names the peer":   {simplyblockv1alpha2.StretchedRoleLeader, peerSite, "", 1},
		"leader, record unset":            {simplyblockv1alpha2.StretchedRoleLeader, "", "", 1},
		"leader, record names this site":  {simplyblockv1alpha2.StretchedRoleLeader, thisSite, "", 0},
		"leader, this site recorded lost": {simplyblockv1alpha2.StretchedRoleLeader, peerSite, thisSite, 0},
		"follower":                        {simplyblockv1alpha2.StretchedRoleFollower, peerSite, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			reading := syncCluster()
			reading.LeaderSite, reading.LostSite = tc.leaderSite, tc.lost
			var wrote []string
			api := &fakeControlPlane{
				cluster:       func(string) (atlascp.Cluster, error) { return reading, nil },
				setLeaderSite: func(_, site string) error { wrote = append(wrote, site); return nil },
			}
			rec := &recorder{}
			r := newClusterReconciler(t, api, rec, newTestCluster(asSite(tc.role)), clusterSecretOf())
			reconcileCluster(t, r, 1)
			if len(wrote) != tc.want || (tc.want == 1 && wrote[0] != thisSite) {
				t.Errorf("wrote leader_site %v, want %d write(s) of %q", wrote, tc.want, thisSite)
			}
			if tc.want == 1 && !rec.has(LeaderSiteRecorded) {
				t.Error("the write emitted no LeaderSiteRecorded")
			}
		})
	}
}

// siteWorker is a StorageNode of this site on a Kubernetes worker at address.
func siteWorker(name, address string) []client.Object {
	node := &simplyblockv1alpha2.StorageNode{ObjectMeta: objectMeta("sn-" + name)}
	node.Spec.ClusterRef = testClusterName
	node.Spec.WorkerNode = name
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	worker.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
	return []client.Object{node, worker}
}

func followerWithWorkers(t *testing.T, api *fakeControlPlane, rec *recorder) *StorageClusterReconciler {
	t.Helper()
	objs := make([]client.Object, 0, 6)
	objs = append(objs, newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())
	objs = append(objs, siteWorker("vm02", "192.0.2.12")...)
	objs = append(objs, siteWorker("vm03", "192.0.2.13")...)
	return newClusterReconciler(t, api, rec, objs...)
}

// U-64: a worker of the follower's site the control plane cannot reach would
// fail later, when its node is added; the follower holds before adopting and
// names it.
func TestAFollowerHoldsWhileAWorkerIsUnreachable(t *testing.T) {
	var probed []string
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
		reachability: func(_ string, addrs []string) ([]WorkerReachability, error) {
			probed = addrs
			return []WorkerReachability{{"192.0.2.12", true}, {"192.0.2.13", false}}, nil
		},
	}
	rec := &recorder{}
	cluster := reconcileCluster(t, followerWithWorkers(t, api, rec), 6)
	if cluster.Status.UUID != "" {
		t.Fatalf("status.uuid = %q: the follower adopted with an unreachable worker", cluster.Status.UUID)
	}
	if len(probed) != 2 {
		t.Errorf("probed %v, want both of this site's workers", probed)
	}
	if !rec.has(WorkerUnreachable) || !strings.Contains(cluster.Status.Message, "vm03") {
		t.Errorf("message %q; want a WorkerUnreachable hold naming vm03", cluster.Status.Message)
	}
}

// U-63: every worker reachable, the follower adopts.
func TestAFollowerWhoseWorkersAreReachableAdopts(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
		reachability: func(_ string, addrs []string) ([]WorkerReachability, error) {
			out := make([]WorkerReachability, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, WorkerReachability{a, true})
			}
			return out, nil
		},
	}
	cluster := reconcileCluster(t, followerWithWorkers(t, api, &recorder{}), 6)
	if cluster.Status.UUID == "" {
		t.Errorf("the follower did not adopt with every worker reachable (message %q)", cluster.Status.Message)
	}
}

// U-65: a control plane without the address probe does not hold the follower.
func TestAControlPlaneWithoutTheProbeDoesNotHoldTheFollower(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
		reachability: func(string, []string) ([]WorkerReachability, error) {
			return nil, ErrReachabilityUnsupported
		},
	}
	cluster := reconcileCluster(t, followerWithWorkers(t, api, &recorder{}), 6)
	if cluster.Status.UUID == "" {
		t.Errorf("a missing probe held the follower (message %q)", cluster.Status.Message)
	}
}
