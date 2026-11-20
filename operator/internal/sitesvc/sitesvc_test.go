package sitesvc

// What one site's operator serves the other's over the site link
// (design-stretched-sync-replication-ramen.md §11.3).

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/link/sitev1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	ns          = "simplyblock"
	clusterName = "stretched"
	clusterUUID = "7416b334-72c7-42cf-9630-41686c8d83d3"
	nodeUUID    = "3fbdafb7-43d0-4a10-8f5d-253c1786cdd9"
	worker      = "vm02"
)

var since = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func siteCluster() *simplyblockv1alpha2.StorageCluster {
	c := testsupport.Cluster(ns, clusterName, clusterUUID)
	c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: "site-b", PeerSite: "site-a", Role: simplyblockv1alpha2.StretchedRoleFollower}
	return c
}

func localNode(uuid, nodeStatus string) *simplyblockv1alpha2.StorageNode {
	n := &simplyblockv1alpha2.StorageNode{ObjectMeta: metav1.ObjectMeta{Name: "node-" + uuid[:8], Namespace: ns}}
	n.Spec.WorkerNode = worker
	n.Status.UUID = uuid
	n.Status.Status = nodeStatus
	return n
}

func remoteNodeOf(uuid string) *simplyblockv1alpha2.StorageNode {
	n := localNode(uuid, "online")
	n.Name = "remote-" + uuid[:8]
	n.Spec.WorkerNode = ""
	n.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	return n
}

// storagePod is the node's storage-node pod, created at created.
func storagePod(name string, created time.Time, ready bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{"app": "storage-node", "simplyblock-cluster": clusterName},
		},
		Spec: corev1.PodSpec{NodeName: worker},
	}
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}
	return p
}

func newServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	return &Server{Client: testsupport.NewClient(t, scheme, nil, objs...), Namespace: ns}
}

func refresh(t *testing.T, s *Server, cluster string) (*sitev1.RefreshNodeResponse, error) {
	t.Helper()
	return s.RefreshNode(context.Background(), &sitev1.RefreshNodeRequest{
		ClusterUuid: cluster, Node: nodeUUID, SinceUnix: since.Unix(),
	})
}

// A pod older than the asking operation is deleted, and the refresh is under way.
func TestAPodOlderThanTheOperationIsReplaced(t *testing.T) {
	s := newServer(t, siteCluster(), localNode(nodeUUID, "online"), storagePod("sn-old", since.Add(-time.Hour), true))
	resp, err := refresh(t, s, clusterUUID)
	if err != nil || resp.GetState() != sitev1.RefreshNodeResponse_STATE_IN_PROGRESS {
		t.Fatalf("RefreshNode = %v, %v; want IN_PROGRESS", resp, err)
	}
	var pod corev1.Pod
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "sn-old"}, &pod); err == nil {
		t.Error("the pod older than the operation was not deleted")
	}
}

// A replacement created after the operation started is the refresh: Ready is
// done, not Ready yet is still under way, and it is never deleted again.
func TestAReplacementNewerThanTheOperationIsNotDeletedAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		ready bool
		want  sitev1.RefreshNodeResponse_State
	}{
		"ready":     {true, sitev1.RefreshNodeResponse_STATE_REFRESHED},
		"not ready": {false, sitev1.RefreshNodeResponse_STATE_IN_PROGRESS},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, siteCluster(), localNode(nodeUUID, "online"), storagePod("sn-new", since.Add(time.Minute), tc.ready))
			resp, err := refresh(t, s, clusterUUID)
			if err != nil || resp.GetState() != tc.want {
				t.Fatalf("RefreshNode = %v, %v; want %v", resp, err, tc.want)
			}
			var pod corev1.Pod
			if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "sn-new"}, &pod); err != nil {
				t.Errorf("the replacement was deleted: %v", err)
			}
		})
	}
}

// While the old pod is gone and the DaemonSet has not made its replacement,
// the refresh is under way.
func TestNoPodYetIsARefreshUnderWay(t *testing.T) {
	s := newServer(t, siteCluster(), localNode(nodeUUID, "online"))
	if resp, err := refresh(t, s, clusterUUID); err != nil || resp.GetState() != sitev1.RefreshNodeResponse_STATE_IN_PROGRESS {
		t.Fatalf("RefreshNode = %v, %v; want IN_PROGRESS", resp, err)
	}
}

// A node this site does not run, its own or the other site's, is not found.
func TestANodeNotRunOnThisSiteIsNotFound(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"unknown":          {siteCluster()},
		"the other site's": {siteCluster(), remoteNodeOf(nodeUUID)},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := refresh(t, newServer(t, objs...), clusterUUID)
			if err != nil || resp.GetState() != sitev1.RefreshNodeResponse_STATE_NOT_FOUND {
				t.Fatalf("RefreshNode = %v, %v; want NOT_FOUND", resp, err)
			}
		})
	}
}

// A request for another backend cluster is refused, not acted on.
func TestARefreshForAnotherClusterIsRefused(t *testing.T) {
	s := newServer(t, siteCluster(), localNode(nodeUUID, "online"), storagePod("sn-old", since.Add(-time.Hour), true))
	_, err := refresh(t, s, "11111111-1111-1111-1111-111111111111")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	var pod corev1.Pod
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "sn-old"}, &pod); err != nil {
		t.Error("a refresh for another cluster deleted a pod")
	}
}

// Describe reports the site, its role, its cluster, and its own nodes.
func TestDescribeReportsTheSite(t *testing.T) {
	s := newServer(t, siteCluster(), localNode(nodeUUID, "online"),
		localNode("a22c1578-ecef-4727-9719-f0ab84c62b0e", "offline"), remoteNodeOf("4532e8bd-cd1b-42c6-bc40-b753731cae20"))
	d, err := s.Describe(context.Background(), &sitev1.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetSite() != "site-b" || d.GetRole() != string(simplyblockv1alpha2.StretchedRoleFollower) ||
		d.GetClusterUuid() != clusterUUID || d.GetNodes() != 2 || d.GetNodesOnline() != 1 {
		t.Errorf("Describe = %+v", d)
	}
}
