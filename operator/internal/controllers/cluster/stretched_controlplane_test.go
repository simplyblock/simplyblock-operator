// A StorageCluster on a stretched control plane
// (docs/designs/design-stretched-control-plane.md): the leader's create names the
// site and its API endpoint, a StorageCluster that names another site is
// refused, and the follower adopts the leader's cluster by name through the
// shared database and registers its site's control plane with it.

package cluster

import (
	"context"
	"errors"
	"net/http"
	"testing"

	atlascp "github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// anotherSite is a site neither this control plane nor its peer is.
const anotherSite = "elsewhere"

// onStretchedControlPlane is the resolver of a control plane that is thisSite
// of a stretched control plane whose peer is peerSite.
func onStretchedControlPlane() func(context.Context) *simplyblockv1alpha2.StretchedControlPlaneSpec {
	return func(context.Context) *simplyblockv1alpha2.StretchedControlPlaneSpec {
		return &simplyblockv1alpha2.StretchedControlPlaneSpec{
			Site: thisSite, PeerSite: peerSite, APIEndpoint: "http://cp-" + thisSite + ".example:5000",
			Role: simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap,
		}
	}
}

// The leader's create carries stretched_control_plane, its site and the
// endpoint the other site's standby will redirect to.
func TestALeaderOnAStretchedControlPlaneCreatesWithItsSiteAndEndpoint(t *testing.T) {
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
	r := newClusterReconciler(t, api, &recorder{},
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)))
	r.Stretch = onStretchedControlPlane()

	cluster := reconcileCluster(t, r, 6)
	if api.createCalls != 1 {
		t.Fatalf("the leader posted %d clusters, want 1 (step %q, message %q)",
			api.createCalls, cluster.Status.Step.State, cluster.Status.Message)
	}
	if !posted.StretchedControlPlane || posted.Site != thisSite ||
		posted.SiteAPIEndpoint != "http://cp-"+thisSite+".example:5000" {
		t.Errorf("create body: stretched=%v site=%q endpoint=%q", posted.StretchedControlPlane,
			posted.Site, posted.SiteAPIEndpoint)
	}
	if posted.LeaderSite != thisSite {
		t.Errorf("leader_site = %q, want %q", posted.LeaderSite, thisSite)
	}
}

// A StorageCluster that does not name the control plane's site cannot be
// created for both sites: the control plane records the creating site, and the
// other site's StorageCluster would never find its peer.
func TestAStorageClusterMustNameTheControlPlanesSite(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no stretched block": newUncreatedCluster(),
		"another site": newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
			asSite(simplyblockv1alpha2.StretchedRoleLeader)(c)
			c.Spec.Stretched.Site = anotherSite
		}),
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeControlPlane{ready: func() error { return nil }}
			rec := &recorder{}
			r := newClusterReconciler(t, api, rec, cluster)
			r.Stretch = onStretchedControlPlane()

			got := reconcileCluster(t, r, 4)
			if api.createCalls != 0 {
				t.Fatalf("posted %d clusters with a site mismatch", api.createCalls)
			}
			if !rec.has(StretchedSiteMismatch) {
				t.Errorf("no StretchedSiteMismatch event; message %q", got.Status.Message)
			}
		})
	}
}

// The follower shares the leader's database, so the cluster the leader created
// is in its own control plane's list, secret included: it adopts by name with
// no hub-delivered credentials, and registers its site's control plane once.
func TestAFollowerOnAStretchedControlPlaneAdoptsByNameAndRegistersItsSite(t *testing.T) {
	follower := newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower))
	leaders := syncCluster()
	leaders.Secret, leaders.LeaderSite = testClusterSecret, peerSite
	// The leader created the cluster under the names both sites derive from
	// the StorageCluster's name, and the control plane recorded them.
	leaders.WorkloadNames = utils.BackendWorkloadNames(atlaskube.DerivedWorkloadNames(testClusterName))
	var registered []atlascp.RegisterManagementNodeParams
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		byName:  func(string) (atlascp.Cluster, bool, error) { return leaders, true, nil },
		cluster: func(string) (atlascp.Cluster, error) { return leaders, nil },
		managementNodes: func(string) ([]atlascp.ManagementNode, error) {
			return []atlascp.ManagementNode{{ID: "m-a", Site: peerSite}}, nil
		},
		registerSite: func(_ string, p atlascp.RegisterManagementNodeParams) error {
			registered = append(registered, p)
			return nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, follower)
	r.Stretch = onStretchedControlPlane()

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != testClusterUUID {
		t.Fatalf("status.uuid = %q, want the leader's cluster (step %q, message %q)",
			cluster.Status.UUID, cluster.Status.Step.State, cluster.Status.Message)
	}
	if api.createCalls != 0 {
		t.Errorf("an adopting follower posted %d clusters", api.createCalls)
	}
	if len(registered) != 1 || registered[0].Site != thisSite ||
		registered[0].APIEndpoint != "http://cp-"+thisSite+".example:5000" {
		t.Errorf("registered %+v, want this site once with its endpoint", registered)
	}
	if !rec.has(SiteRegistered) || !rec.has(ClusterAdopted) {
		t.Errorf("events: SiteRegistered=%v ClusterAdopted=%v", rec.has(SiteRegistered), rec.has(ClusterAdopted))
	}
}

// A site already in the cluster's management-node list, or a 409 from the
// control plane, is a registration an earlier pass made.
func TestASiteIsRegisteredOnce(t *testing.T) {
	spec := onStretchedControlPlane()(context.Background())
	found := adoption{UUID: testClusterUUID}

	listed := &fakeControlPlane{managementNodes: func(string) ([]atlascp.ManagementNode, error) {
		return []atlascp.ManagementNode{{ID: "m-b", Site: thisSite}}, nil
	}}
	r := newClusterReconciler(t, listed, &recorder{}, newUncreatedCluster())
	if err := r.registerSite(context.Background(), newUncreatedCluster(), found, spec); err != nil {
		t.Fatal(err)
	}
	if listed.registerSiteCalls != 0 {
		t.Errorf("registered a site the cluster already lists")
	}

	conflict := &fakeControlPlane{
		managementNodes: func(string) ([]atlascp.ManagementNode, error) { return nil, nil },
		registerSite: func(string, atlascp.RegisterManagementNodeParams) error {
			return &atlascp.StatusError{Op: "register", StatusCode: http.StatusConflict, Body: "exists"}
		},
	}
	r = newClusterReconciler(t, conflict, &recorder{}, newUncreatedCluster())
	if err := r.registerSite(context.Background(), newUncreatedCluster(), found, spec); err != nil {
		t.Fatalf("a 409 is a registration already made, got %v", err)
	}

	refused := &fakeControlPlane{
		managementNodes: func(string) ([]atlascp.ManagementNode, error) { return nil, nil },
		registerSite: func(string, atlascp.RegisterManagementNodeParams) error {
			return &atlascp.StatusError{Op: "register", StatusCode: http.StatusBadRequest, Body: "no such site"}
		},
	}
	r = newClusterReconciler(t, refused, &recorder{}, newUncreatedCluster())
	err := r.registerSite(context.Background(), newUncreatedCluster(), found, spec)
	var se *atlascp.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("a 400 must surface, got %v", err)
	}
}

// Without a stretched control plane nothing changes: a follower still waits
// for the hub's credentials and a leader's create names no site.
func TestAnOrdinaryControlPlaneLeavesTheStretchedFlowAlone(t *testing.T) {
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
	r := newClusterReconciler(t, api, &recorder{},
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)))
	reconcileCluster(t, r, 6)
	if posted.StretchedControlPlane || posted.Site != "" || posted.SiteAPIEndpoint != "" {
		t.Errorf("create body names a stretched control plane without one: %+v", posted)
	}
}

// On a stretched control plane the record's leader site belongs to the
// control plane (set-leader-site, takeover): the leader's StorageCluster does
// not write its own site back over a planned move to the peer.
func TestALeaderOnAStretchedControlPlaneLeavesAMovedLeaderSiteAlone(t *testing.T) {
	reading := syncCluster()
	reading.LeaderSite = peerSite
	var wrote []string
	api := &fakeControlPlane{
		cluster:       func(string) (atlascp.Cluster, error) { return reading, nil },
		setLeaderSite: func(_, site string) error { wrote = append(wrote, site); return nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)),
		clusterSecretOf())
	r.Stretch = onStretchedControlPlane()
	reconcileCluster(t, r, 1)
	if len(wrote) != 0 || rec.has(LeaderSiteRecorded) {
		t.Errorf("the leader wrote leader_site %v over the control plane's move to %q", wrote, peerSite)
	}
}
