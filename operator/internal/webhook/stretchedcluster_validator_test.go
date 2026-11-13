// The StretchedCluster role-swap guard: a swap reaches both sites' clusters, so
// it is admitted only while both are Online.

package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/utils"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func stretchedRequest(leaderPhase, followerPhase simplyblockv1alpha2.StorageClusterPhase) *simplyblockv1alpha2.StretchedCluster {
	sc := &simplyblockv1alpha2.StretchedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "simplyblock"},
		Status:     simplyblockv1alpha2.StretchedClusterStatus{ClusterUUID: "cluster-1"},
		Spec: simplyblockv1alpha2.StretchedClusterSpec{
			ClusterName: "production",
			Sites: []simplyblockv1alpha2.StretchedSite{
				{Cluster: "site-a", Site: "a", Role: simplyblockv1alpha2.StretchedRoleLeader},
				{Cluster: "site-b", Site: "b", Role: simplyblockv1alpha2.StretchedRoleFollower},
			},
		},
	}
	for _, s := range []struct {
		cluster string
		phase   simplyblockv1alpha2.StorageClusterPhase
	}{{"site-a", leaderPhase}, {"site-b", followerPhase}} {
		entry := simplyblockv1alpha2.StretchedClusterSiteStatus{Cluster: s.cluster}
		if s.phase != "" {
			entry.StorageCluster = &simplyblockv1alpha2.StorageSiteCluster{Name: "production", Phase: string(s.phase)}
		}
		sc.Status.Sites = append(sc.Status.Sites, entry)
	}
	return sc
}

func swapped(sc *simplyblockv1alpha2.StretchedCluster) *simplyblockv1alpha2.StretchedCluster {
	out := sc.DeepCopy()
	out.Spec.Sites[0].Role, out.Spec.Sites[1].Role = out.Spec.Sites[1].Role, out.Spec.Sites[0].Role
	return out
}

func updateOf(t *testing.T, oldObj, newObj any) admission.Request {
	t.Helper()
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update, Object: raw(t, newObj), OldObject: raw(t, oldObj),
	}}
}

// U-23: a swap while either site's cluster is not Online would land on a cluster
// mid-creation or mid-recovery.
func TestARoleSwapIsRefusedUntilBothSitesAreOnline(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	v := validatorOver(allOnline())
	for name, old := range map[string]*simplyblockv1alpha2.StretchedCluster{
		"leader still creating":  stretchedRequest(simplyblockv1alpha2.StorageClusterPhaseCreating, online),
		"follower not projected": stretchedRequest(online, ""),
		"follower degraded":      stretchedRequest(online, simplyblockv1alpha2.StorageClusterPhaseDegraded),
	} {
		t.Run(name, func(t *testing.T) {
			resp := v.Handle(context.Background(), updateOf(t, old, swapped(old)))
			if resp.Allowed {
				t.Fatal("a role swap was admitted with a site not Online")
			}
			if !strings.Contains(resp.Result.Message, "RoleSwapNotReady") {
				t.Errorf("the refusal does not say RoleSwapNotReady: %s", resp.Result.Message)
			}
		})
	}
}

// U-23: a swap on a ready pair is admitted; the hub then re-delivers both works.
func TestARoleSwapOnAReadyPairIsAdmitted(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	old := stretchedRequest(online, online)
	if resp := validatorOver(allOnline()).Handle(context.Background(), updateOf(t, old, swapped(old))); !resp.Allowed {
		t.Fatalf("a swap on two Online sites was refused: %s", resp.Result.Message)
	}
}

// Any other change is the schema's business, whatever the sites' state.
func TestAnUpdateThatSwapsNothingIsNotThisGuardsBusiness(t *testing.T) {
	old := stretchedRequest(simplyblockv1alpha2.StorageClusterPhaseCreating, "")
	changed := old.DeepCopy()
	changed.Spec.Approved = true
	if resp := validatorOver(allOnline()).Handle(context.Background(), updateOf(t, old, changed)); !resp.Allowed {
		t.Fatalf("an approval was refused by the role-swap guard: %s", resp.Result.Message)
	}
}

// lostLeader is the request with the leader site a lost: the hub cannot read its
// StorageCluster, the survivor b reports Degraded, and the control plane records
// a as lost with its record at state.
func lostLeader(state string) *simplyblockv1alpha2.StretchedCluster {
	sc := stretchedRequest("", simplyblockv1alpha2.StorageClusterPhaseDegraded)
	sc.Status.LostSite = "a"
	sc.Status.Sites[1].SiteRecord = state
	return sc
}

// lostSiteBackend is a control plane recording site lost at state, its nodes offline.
func lostSiteBackend(site, state string) *fakeSwapBackend {
	b := allOnline()
	b.cluster.LostSite, b.cluster.LostSiteState = site, state
	for i := range b.nodes {
		if b.nodes[i].Site == site {
			b.nodes[i].Status = "offline"
		}
	}
	return b
}

// §11.1: when the leader's site is lost for long, the hub promotes the follower.
// A swap away from a lost leader whose fence is done is the one swap admitted
// with a site down.
func TestARoleSwapAwayFromAFencedLostLeaderIsAdmitted(t *testing.T) {
	old := lostLeader("done")
	if resp := validatorOver(lostSiteBackend("a", "done")).Handle(context.Background(), updateOf(t, old, swapped(old))); !resp.Allowed {
		t.Fatalf("a swap away from a fenced lost leader was refused: %s", resp.Result.Message)
	}
}

// A lost leader the control plane has not fenced yet may still be running: a
// swap then would make two leaders.
func TestARoleSwapAwayFromALostLeaderNotYetFencedIsRefused(t *testing.T) {
	for _, state := range []string{"draining", "down", "fencing"} {
		t.Run(state, func(t *testing.T) {
			old := lostLeader(state)
			resp := validatorOver(lostSiteBackend("a", state)).Handle(context.Background(), updateOf(t, old, swapped(old)))
			if resp.Allowed {
				t.Fatal("a swap was admitted before the lost leader's fence was done")
			}
			if !strings.Contains(resp.Result.Message, "fence") {
				t.Errorf("the refusal does not say the fence is not done: %s", resp.Result.Message)
			}
		})
	}
}

// A lost site cannot lead: a swap that hands the leader role to the site the
// control plane records as lost is refused.
func TestARoleSwapOntoTheLostSiteIsRefused(t *testing.T) {
	old := stretchedRequest(simplyblockv1alpha2.StorageClusterPhaseDegraded, "")
	old.Status.LostSite = "b"
	old.Status.Sites[0].SiteRecord = "done"
	if resp := validatorOver(lostSiteBackend("b", "done")).Handle(context.Background(), updateOf(t, old, swapped(old))); resp.Allowed {
		t.Fatal("a swap that makes the lost site leader was admitted")
	}
}

// The survivor must be a working cluster to take the lead.
func TestARoleSwapToASurvivorStillCreatingIsRefused(t *testing.T) {
	old := lostLeader("done")
	old.Status.Sites[1].StorageCluster.Phase = string(simplyblockv1alpha2.StorageClusterPhaseCreating)
	if resp := validatorOver(lostSiteBackend("a", "done")).Handle(context.Background(), updateOf(t, old, swapped(old))); resp.Allowed {
		t.Fatal("a swap to a survivor still being created was admitted")
	}
}

// fakeSwapBackend is the control plane as the guard reads it.
type fakeSwapBackend struct {
	cluster atlascp.Cluster
	nodes   []utils.NodeStatusResponse
	err     error
}

func (f *fakeSwapBackend) Cluster(context.Context, string) (atlascp.Cluster, error) {
	return f.cluster, f.err
}

func (f *fakeSwapBackend) StorageNodes(context.Context, string) ([]utils.NodeStatusResponse, error) {
	return f.nodes, f.err
}

// allOnline is a control plane with no site recorded lost and every node of
// both sites online.
func allOnline() *fakeSwapBackend {
	return &fakeSwapBackend{
		cluster: atlascp.Cluster{ID: "cluster-1", SyncReplication: true},
		nodes: []utils.NodeStatusResponse{
			{UUID: "a1", Site: "a", Status: "online"}, {UUID: "a2", Site: "a", Status: "online"},
			{UUID: "b1", Site: "b", Status: "online"}, {UUID: "b2", Site: "b", Status: "online"},
		},
	}
}

func validatorOver(b *fakeSwapBackend) *StretchedClusterValidator {
	return &StretchedClusterValidator{Backend: b}
}

// Regression: 2026-10-26-swap-on-stale-projection: the leader site's nodes went
// offline and the hub's projections still showed both sites Online, so the
// guard admitted a swap before the control plane had fenced the leader, the
// two-leader case it exists to prevent (live, 12-leader-swap.sh). The guard
// reads the control plane: with a node of either site not online and no site
// recorded lost, a swap is refused.
func TestASwapWhileANodeIsDownAndNoSiteIsRecordedLostIsRefused(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	old := stretchedRequest(online, online)
	backend := allOnline()
	backend.nodes[0].Status = "offline"
	resp := validatorOver(backend).Handle(context.Background(), updateOf(t, old, swapped(old)))
	if resp.Allowed {
		t.Fatal("a swap was admitted with a node offline and no site recorded lost")
	}
	if !strings.Contains(resp.Result.Message, "a1") {
		t.Errorf("the refusal does not name the node: %s", resp.Result.Message)
	}
}

// The control plane recording the leader lost but not fenced refuses the swap,
// whatever the request's projections still say.
func TestASwapIsRefusedOnTheControlPlanesRecordNotTheProjections(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	old := stretchedRequest(online, online) // stale: no lost site projected
	resp := validatorOver(lostSiteBackend("a", "down")).Handle(context.Background(), updateOf(t, old, swapped(old)))
	if resp.Allowed || !strings.Contains(resp.Result.Message, "fence") {
		t.Fatalf("allowed=%v %s; want refused until the fence is done", resp.Allowed, resp.Result.Message)
	}
}

// A control plane the guard cannot read refuses the swap rather than guessing.
func TestASwapIsRefusedWhenTheControlPlaneCannotBeRead(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	old := stretchedRequest(online, online)
	old.Status.ClusterUUID = "cluster-1"
	backend := allOnline()
	backend.err = errors.New("connection refused")
	if resp := validatorOver(backend).Handle(context.Background(), updateOf(t, old, swapped(old))); resp.Allowed {
		t.Fatal("a swap was admitted with the control plane unreadable")
	}
}
