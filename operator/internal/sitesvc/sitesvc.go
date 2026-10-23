// Package sitesvc is what one site's operator of a stretched cluster serves the
// other's over the site link (design-stretched-sync-replication-ramen.md §11.3):
// a description of the site, and the refresh of a storage node's pod, which
// only the operator of the site the pod runs on can make. The leader's rolling
// restart walks both sites' nodes and asks the other site for this.
package sitesvc

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/link/sitev1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The labels of a storage node's pod, which the StorageCluster's DaemonSet sets.
const (
	podAppLabel     = "app"
	podApp          = "storage-node"
	podClusterLabel = "simplyblock-cluster"
	nodeOnline      = "online"
)

// Server serves SiteService.
type Server struct {
	sitev1.UnimplementedSiteServiceServer
	Client client.Client
	// Namespace is the operator's, where the stretched StorageCluster lives.
	Namespace string
}

// Register adds SiteService to a server.
func (s *Server) Register(srv grpc.ServiceRegistrar) { sitev1.RegisterSiteServiceServer(srv, s) }

// Describe reports the site, the role its operator acts on, and its own nodes.
func (s *Server) Describe(ctx context.Context, _ *sitev1.DescribeRequest) (*sitev1.SiteDescription, error) {
	cluster, err := s.stretchedCluster(ctx)
	if err != nil {
		return nil, err
	}
	role := cluster.Spec.Stretched.Role
	if cluster.Status.Stretched != nil && cluster.Status.Stretched.Role != "" {
		role = cluster.Status.Stretched.Role
	}
	nodes, err := s.storageNodes(ctx)
	if err != nil {
		return nil, err
	}
	d := &sitev1.SiteDescription{Site: cluster.Spec.Stretched.Site, Role: string(role), ClusterUuid: cluster.Status.UUID}
	for i := range nodes {
		if nodes[i].IsRemote() {
			continue
		}
		d.Nodes++
		if nodes[i].Status.Status == nodeOnline {
			d.NodesOnline++
		}
	}
	return d, nil
}

// RefreshNode replaces node's storage-node pod on this site once: a pod created
// before since is deleted, and a pod created after it is the refresh.
func (s *Server) RefreshNode(ctx context.Context, req *sitev1.RefreshNodeRequest) (*sitev1.RefreshNodeResponse, error) {
	cluster, err := s.stretchedCluster(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetClusterUuid() != cluster.Status.UUID {
		return nil, status.Errorf(codes.FailedPrecondition,
			"this site is one site of backend cluster %s, not %s", cluster.Status.UUID, req.GetClusterUuid())
	}
	nodes, err := s.storageNodes(ctx)
	if err != nil {
		return nil, err
	}
	var node *simplyblockv1alpha2.StorageNode
	for i := range nodes {
		if nodes[i].Status.UUID == req.GetNode() && !nodes[i].IsRemote() {
			node = &nodes[i]
		}
	}
	if node == nil || node.Spec.WorkerNode == "" {
		return &sitev1.RefreshNodeResponse{State: sitev1.RefreshNodeResponse_STATE_NOT_FOUND,
			Message: fmt.Sprintf("node %s runs on no worker of site %s", req.GetNode(), cluster.Spec.Stretched.Site)}, nil
	}

	pod, err := s.storagePod(ctx, cluster.Name, node.Spec.WorkerNode)
	if err != nil {
		return nil, err
	}
	since := time.Unix(req.GetSinceUnix(), 0)
	inProgress := func(msg string) *sitev1.RefreshNodeResponse {
		return &sitev1.RefreshNodeResponse{State: sitev1.RefreshNodeResponse_STATE_IN_PROGRESS, Message: msg}
	}
	switch {
	case pod == nil:
		return inProgress("waiting for the replacement pod on worker " + node.Spec.WorkerNode), nil
	case !pod.DeletionTimestamp.IsZero():
		return inProgress("the old pod " + pod.Name + " is terminating"), nil
	case pod.CreationTimestamp.Time.Before(since):
		if err := s.Client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return nil, status.Errorf(codes.Unavailable, "delete pod %s: %v", pod.Name, err)
		}
		return inProgress("deleted pod " + pod.Name + ", which predates the refresh"), nil
	case !podReady(pod):
		return inProgress("the replacement pod " + pod.Name + " is not Ready yet"), nil
	}
	return &sitev1.RefreshNodeResponse{State: sitev1.RefreshNodeResponse_STATE_REFRESHED,
		Message: "pod " + pod.Name + " was created after the refresh began and is Ready"}, nil
}

// stretchedCluster is this site's StorageCluster of a stretched cluster.
func (s *Server) stretchedCluster(ctx context.Context) (*simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &clusters, client.InNamespace(s.Namespace)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list StorageClusters: %v", err)
	}
	for i := range clusters.Items {
		if c := &clusters.Items[i]; c.Spec.Stretched != nil && c.Status.UUID != "" {
			return c, nil
		}
	}
	return nil, status.Error(codes.FailedPrecondition, "this site runs no stretched cluster")
}

func (s *Server) storageNodes(ctx context.Context) ([]simplyblockv1alpha2.StorageNode, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := s.Client.List(ctx, &nodes, client.InNamespace(s.Namespace)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list StorageNodes: %v", err)
	}
	return nodes.Items, nil
}

// storagePod is the cluster's storage-node pod on worker, or nil.
func (s *Server) storagePod(ctx context.Context, cluster, worker string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(s.Namespace),
		client.MatchingLabels{podAppLabel: podApp, podClusterLabel: cluster}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list the storage-node pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
