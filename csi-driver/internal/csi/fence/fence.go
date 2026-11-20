// Package fence serves the csi-addons NetworkFence service for a stretched,
// sync-replicated cluster (design-stretched-sync-replication-ramen.md §10.2).
//
// On a failover Ramen fences the lost cluster by delivering a NetworkFence to
// the surviving one, naming the lost cluster's hosts as CIDRs. This cluster's
// fence unit is a site, not a host, so the driver resolves the CIDRs to the one
// site whose storage nodes' management addresses they cover, and asks the
// control plane to fence that site. CIDRs that cover both sites, or no node at
// all, are refused rather than guessed at: a fence of the wrong site would stop
// the survivor.
//
// The site fence runs the control plane's own proof that the site is lost and
// is resumable, so the service never waits on it: a fence still running is
// Unavailable and csi-addons calls again. An unfence is a confirmation: the
// site returns by the control plane's own activation once its nodes are back,
// and the unfence succeeds once the control plane no longer records it.
package fence

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/csi-addons/spec/lib/go/fence"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	atlascp "github.com/simplyblock/atlas/controlplane"
)

// clusterIDParameter is the fence parameter naming the cluster, which Ramen
// takes from the DRCluster's storage-clusterid annotation.
const clusterIDParameter = "clusterID"

// Backend is the control plane's site fence.
type Backend interface {
	// SiteNodes lists the cluster's storage nodes with their management
	// addresses and sites.
	SiteNodes(ctx context.Context, clusterID string) ([]atlascp.SiteNode, error)
	// FenceSite fences the site, and reports whether the fence is done. A
	// fence the control plane refuses is a *atlascp.SyncRefusedError.
	FenceSite(ctx context.Context, clusterID, site string) (done bool, err error)
	// UnfenceSite reports whether the control plane no longer records the
	// site as lost.
	UnfenceSite(ctx context.Context, clusterID, site string) (cleared bool, err error)
}

// Server serves the csi-addons FenceController.
type Server struct {
	fence.UnimplementedFenceControllerServer

	backend      func(clusterID string) (Backend, error)
	localCluster func() (string, error)
}

// New returns a server reaching each cluster through backend, and fencing
// localCluster's cluster when a request names none.
func New(backend func(clusterID string) (Backend, error), localCluster func() (string, error)) *Server {
	return &Server{backend: backend, localCluster: localCluster}
}

// Register adds the FenceController to a server.
func (s *Server) Register(srv grpc.ServiceRegistrar) { fence.RegisterFenceControllerServer(srv, s) }

// FenceClusterNetwork fences the site the CIDRs name.
func (s *Server) FenceClusterNetwork(
	ctx context.Context, req *fence.FenceClusterNetworkRequest,
) (*fence.FenceClusterNetworkResponse, error) {
	clusterID, b, site, err := s.resolve(ctx, req.GetParameters(), req.GetCidrs())
	if err != nil {
		return nil, err
	}
	klog.Infof("fence: fencing site %s of cluster %s", site, clusterID)
	done, err := b.FenceSite(ctx, clusterID, site)
	if err != nil {
		var refused *atlascp.SyncRefusedError
		if errors.As(err, &refused) {
			return nil, status.Errorf(codes.FailedPrecondition, "fence: site %s: %s", site, refused.Error())
		}
		return nil, status.Errorf(codes.Unavailable, "fence: site %s: %v", site, err)
	}
	if !done {
		return nil, status.Errorf(codes.Unavailable, "fence: site %s is being fenced", site)
	}
	klog.Infof("fence: site %s of cluster %s is fenced", site, clusterID)
	return &fence.FenceClusterNetworkResponse{}, nil
}

// UnfenceClusterNetwork succeeds once the control plane no longer records the
// site the CIDRs name as lost.
func (s *Server) UnfenceClusterNetwork(
	ctx context.Context, req *fence.UnfenceClusterNetworkRequest,
) (*fence.UnfenceClusterNetworkResponse, error) {
	clusterID, b, site, err := s.resolve(ctx, req.GetParameters(), req.GetCidrs())
	if err != nil {
		return nil, err
	}
	cleared, err := b.UnfenceSite(ctx, clusterID, site)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "unfence: site %s: %v", site, err)
	}
	if !cleared {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unfence: the control plane still records site %s as lost; it returns once its nodes do", site)
	}
	klog.Infof("unfence: site %s of cluster %s is back", site, clusterID)
	return &fence.UnfenceClusterNetworkResponse{}, nil
}

// resolve finds the cluster and the one site the CIDRs name.
func (s *Server) resolve(
	ctx context.Context, params map[string]string, cidrs []*fence.CIDR,
) (string, Backend, string, error) {
	clusterID := params[clusterIDParameter]
	if clusterID == "" {
		var err error
		if clusterID, err = s.localCluster(); err != nil {
			return "", nil, "", status.Errorf(codes.FailedPrecondition, "fence: which cluster: %v", err)
		}
	}
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c.GetCidr())
		if err != nil {
			return "", nil, "", status.Errorf(codes.InvalidArgument, "fence: %q is not a CIDR", c.GetCidr())
		}
		prefixes = append(prefixes, p)
	}
	if len(prefixes) == 0 {
		return "", nil, "", status.Error(codes.InvalidArgument, "fence: no CIDRs")
	}

	b, err := s.backend(clusterID)
	if err != nil {
		return "", nil, "", status.Errorf(codes.Unavailable, "fence: cluster %s: %v", clusterID, err)
	}
	nodes, err := b.SiteNodes(ctx, clusterID)
	if err != nil {
		return "", nil, "", status.Errorf(codes.Unavailable, "fence: the nodes of cluster %s: %v", clusterID, err)
	}
	site, err := siteOf(nodes, prefixes)
	if err != nil {
		return "", nil, "", status.Errorf(codes.InvalidArgument, "fence: %v", err)
	}
	return clusterID, b, site, nil
}

// siteOf is the one site whose nodes the prefixes cover.
func siteOf(nodes []atlascp.SiteNode, prefixes []netip.Prefix) (string, error) {
	sites := map[string][]string{}
	for _, n := range nodes {
		addr, err := netip.ParseAddr(n.MgmtIP)
		if err != nil {
			continue
		}
		for _, p := range prefixes {
			if p.Contains(addr) {
				sites[n.Site] = append(sites[n.Site], n.ID)
				break
			}
		}
	}
	switch len(sites) {
	case 0:
		return "", fmt.Errorf("the CIDRs match no storage node of the cluster")
	case 1:
		for site := range sites {
			if site == "" {
				return "", fmt.Errorf("the CIDRs match nodes with no site; the cluster is not a stretched one")
			}
			return site, nil
		}
	}
	names := make([]string, 0, len(sites))
	for site, ids := range sites {
		names = append(names, fmt.Sprintf("%s (%s)", site, strings.Join(ids, ", ")))
	}
	sort.Strings(names)
	return "", fmt.Errorf("the CIDRs span more than one site: %s; a fence names one site", strings.Join(names, "; "))
}
