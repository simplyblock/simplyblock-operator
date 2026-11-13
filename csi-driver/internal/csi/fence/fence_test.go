package fence

// The csi-addons NetworkFence service of a stretched cluster
// (design-stretched-sync-replication-ramen.md §10.2): Ramen names the lost
// site's hosts as CIDRs, and the driver resolves them to one site of the cluster
// and fences that site through the control plane. A fence of hosts spanning both
// sites, or matching no node, is refused rather than guessed at.

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/fence"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atlascp "github.com/simplyblock/atlas/controlplane"
)

const cluster = "11111111-1111-1111-1111-111111111111"

type fakeBackend struct {
	fenceDone bool
	fenceErr  error
	cleared   bool
	fenced    []string
	unfenced  []string
	nodesOf   string
}

func (f *fakeBackend) SiteNodes(_ context.Context, clusterID string) ([]atlascp.SiteNode, error) {
	f.nodesOf = clusterID
	return []atlascp.SiteNode{
		{ID: "a-1", MgmtIP: "192.0.2.11", Site: "site-a"},
		{ID: "a-2", MgmtIP: "192.0.2.12", Site: "site-a"},
		{ID: "b-1", MgmtIP: "198.51.100.21", Site: "site-b"},
	}, nil
}

func (f *fakeBackend) FenceSite(_ context.Context, _ string, site string) (bool, error) {
	f.fenced = append(f.fenced, site)
	return f.fenceDone, f.fenceErr
}

func (f *fakeBackend) UnfenceSite(_ context.Context, _ string, site string) (bool, error) {
	f.unfenced = append(f.unfenced, site)
	return f.cleared, nil
}

func server(b *fakeBackend) *Server {
	return &Server{
		backend:      func(string) (Backend, error) { return b, nil },
		localCluster: func() (string, error) { return cluster, nil },
	}
}

func cidrs(cs ...string) []*fence.CIDR {
	out := make([]*fence.CIDR, 0, len(cs))
	for _, c := range cs {
		out = append(out, &fence.CIDR{Cidr: c})
	}
	return out
}

// Ramen's CIDRs for site a's hosts fence site a, once the backend reports done.
func TestTheCIDRsOfOneSiteFenceThatSite(t *testing.T) {
	b := &fakeBackend{fenceDone: true}
	_, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.11/32", "192.0.2.12/32")})
	if err != nil {
		t.Fatalf("FenceClusterNetwork: %v", err)
	}
	if len(b.fenced) != 1 || b.fenced[0] != "site-a" || b.nodesOf != cluster {
		t.Errorf("fenced %v of cluster %q; want site-a of %s", b.fenced, b.nodesOf, cluster)
	}
}

// A CIDR range covering a site's hosts resolves the same way.
func TestARangeCoveringOneSitesHostsFencesIt(t *testing.T) {
	b := &fakeBackend{fenceDone: true}
	if _, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.0/24")}); err != nil || len(b.fenced) != 1 || b.fenced[0] != "site-a" {
		t.Errorf("err %v, fenced %v; want site-a", err, b.fenced)
	}
}

// A fence the backend is still running is retried by csi-addons.
func TestAFenceStillRunningIsRetried(t *testing.T) {
	b := &fakeBackend{}
	_, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.11/32")})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

// A fence the backend refuses (its proof that the site is lost does not hold)
// is a FailedPrecondition carrying the reason.
func TestAFenceTheBackendRefusesIsAFailedPrecondition(t *testing.T) {
	b := &fakeBackend{fenceErr: &atlascp.SyncRefusedError{Message: "node a-1 is still reachable"}}
	_, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.11/32")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
}

// CIDRs spanning both sites, or matching no node, are refused and nothing is fenced.
func TestCIDRsThatDoNotNameOneSiteAreRefused(t *testing.T) {
	for name, cs := range map[string][]string{
		"both sites": {"192.0.2.11/32", "198.51.100.21/32"},
		"no node":    {"203.0.113.5/32"},
		"not a CIDR": {"site-a"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{fenceDone: true}
			_, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{Cidrs: cidrs(cs...)})
			if status.Code(err) != codes.InvalidArgument || len(b.fenced) != 0 {
				t.Errorf("err %v, fenced %v; want InvalidArgument and nothing fenced", err, b.fenced)
			}
		})
	}
}

// The parameters' clusterID, which Ramen takes from the DRCluster, wins over the
// driver's own sync cluster.
func TestTheFencesClusterIDIsHonored(t *testing.T) {
	b := &fakeBackend{fenceDone: true}
	other := "22222222-2222-2222-2222-222222222222"
	_, err := server(b).FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Parameters: map[string]string{"clusterID": other}, Cidrs: cidrs("192.0.2.11/32")})
	if err != nil || b.nodesOf != other {
		t.Errorf("err %v, resolved through cluster %q; want %s", err, b.nodesOf, other)
	}
}

// An unfence confirms the backend no longer records the site; while it does,
// csi-addons is told to retry.
func TestAnUnfenceConfirmsTheSiteIsBack(t *testing.T) {
	b := &fakeBackend{}
	_, err := server(b).UnfenceClusterNetwork(context.Background(), &fence.UnfenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.11/32")})
	if status.Code(err) != codes.FailedPrecondition || len(b.unfenced) != 1 {
		t.Fatalf("err %v after %v; want FailedPrecondition while site-a is recorded", err, b.unfenced)
	}
	b.cleared = true
	if _, err := server(b).UnfenceClusterNetwork(context.Background(), &fence.UnfenceClusterNetworkRequest{
		Cidrs: cidrs("192.0.2.11/32")}); err != nil {
		t.Errorf("unfence once the site is back: %v", err)
	}
}
