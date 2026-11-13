// A stretched cluster's other site, listed on this site as remote StorageNodes:
// one per peer-site node the control plane reports, recreated if deleted,
// removed when the node leaves, never touched before the snapshot, and never a
// workload or a backend call.

package node

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	remoteNS      = "simplyblock"
	remoteCluster = "production"
	remoteUUID    = "11111111-2222-3333-4444-555555555555"
	peerNodeID    = "aaaaaaaa-0000-0000-0000-000000000001"
	ownNodeID     = "bbbbbbbb-0000-0000-0000-000000000001"
)

func aStretchedCluster(stretched bool) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: remoteCluster, Namespace: remoteNS, UID: "cluster-uid"},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: remoteUUID},
	}
	if stretched {
		c.Spec.EnableSyncReplication = ptr.To(true)
		c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{
			Site: "b", PeerSite: "a", Role: simplyblockv1alpha2.StretchedRoleLeader,
		}
	}
	return c
}

func streamed(synced bool, nodes ...subscriptions.NodeDTO) *deliveredNodes {
	return &deliveredNodes{nodes: nodes, synced: synced}
}

func peerNode() subscriptions.NodeDTO {
	return subscriptions.NodeDTO{ID: peerNodeID, Site: "a", Status: "online", Hostname: "site-a-worker-1", CPUCount: 4}
}

func ownNode() subscriptions.NodeDTO {
	return subscriptions.NodeDTO{ID: ownNodeID, Site: "b", Status: "online", CPUCount: 4}
}

func aRemoteWorld(
	t *testing.T, cache NodeCache, objects ...client.Object,
) (*RemoteNodeReconciler, client.Client) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}, &simplyblockv1alpha2.StorageCluster{}).
		Build()
	return &RemoteNodeReconciler{
		Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(64), Nodes: cache,
	}, c
}

func reconcileRemote(t *testing.T, r *RemoteNodeReconciler) {
	t.Helper()
	key := types.NamespacedName{Namespace: remoteNS, Name: remoteCluster}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func remoteNodes(t *testing.T, c client.Client) []simplyblockv1alpha2.StorageNode {
	t.Helper()
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := c.List(context.Background(), &nodes, client.InNamespace(remoteNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []simplyblockv1alpha2.StorageNode
	for _, n := range nodes.Items {
		if n.IsRemote() {
			out = append(out, n)
		}
	}
	return out
}

// U-83: every node the control plane reports on the peer site gets a remote
// record; this site's own nodes do not.
func TestARemoteNodeIsListedForEveryPeerSiteNode(t *testing.T) {
	r, c := aRemoteWorld(t, streamed(true, peerNode(), ownNode()), aStretchedCluster(true))
	reconcileRemote(t, r)

	got := remoteNodes(t, c)
	if len(got) != 1 {
		t.Fatalf("listed %d remote nodes, want the one peer-site node", len(got))
	}
	node := got[0]
	if node.Status.UUID != peerNodeID {
		t.Errorf("status.uuid = %q, want %q", node.Status.UUID, peerNodeID)
	}
	if node.Spec.Config.Site != "a" || node.Spec.WorkerNode != "" {
		t.Errorf("site %q, worker %q; want site a and no worker", node.Spec.Config.Site, node.Spec.WorkerNode)
	}
	if node.Spec.Config.Sizing.VCPUCount == nil || *node.Spec.Config.Sizing.VCPUCount != 4 {
		t.Errorf("sizing.vcpuCount = %v, want the node's reported SPDK CPUs", node.Spec.Config.Sizing.VCPUCount)
	}
	if owner := metav1.GetControllerOf(&node); owner == nil || owner.Name != remoteCluster {
		t.Errorf("controller owner = %+v, want the cluster, so a cluster delete takes its remote nodes", owner)
	}
}

// U-89: a standalone cluster lists no other site.
func TestAStandaloneClusterListsNoRemoteNodes(t *testing.T) {
	r, c := aRemoteWorld(t, streamed(true, peerNode(), ownNode()), aStretchedCluster(false))
	reconcileRemote(t, r)
	if got := remoteNodes(t, c); len(got) != 0 {
		t.Errorf("a standalone cluster listed %d remote nodes", len(got))
	}
}

func aRemoteRecord(nodeID string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: remoteNodeName(remoteCluster, "a", nodeID), Namespace: remoteNS},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: remoteCluster,
			Config: simplyblockv1alpha2.StorageNodeConfig{
				Site: "a", Sizing: simplyblockv1alpha2.StorageNodeSizing{VCPUCount: ptr.To(int32(4))},
			},
			Stretched: &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
		Status: simplyblockv1alpha2.StorageNodeStatus{UUID: nodeID},
	}
}

// U-85: a node that left the cluster takes its remote record with it.
func TestARemoteNodeWhoseNodeLeftIsRemoved(t *testing.T) {
	const goneID = "aaaaaaaa-0000-0000-0000-00000000dead"
	r, c := aRemoteWorld(t, streamed(true, peerNode()), aStretchedCluster(true), aRemoteRecord(goneID))
	reconcileRemote(t, r)

	for _, n := range remoteNodes(t, c) {
		if n.Status.UUID == goneID {
			t.Fatal("the remote record of a node that left the cluster was kept")
		}
	}
}

// U-84: a deleted remote record is the control plane's to restore, and it is.
func TestADeletedRemoteNodeComesBack(t *testing.T) {
	r, c := aRemoteWorld(t, streamed(true, peerNode()), aStretchedCluster(true))
	reconcileRemote(t, r)
	got := remoteNodes(t, c)
	if len(got) != 1 {
		t.Fatalf("listed %d remote nodes, want 1", len(got))
	}
	if err := c.Delete(context.Background(), &got[0]); err != nil {
		t.Fatalf("delete: %v", err)
	}

	reconcileRemote(t, r)
	if got := remoteNodes(t, c); len(got) != 1 || got[0].Status.UUID != peerNodeID {
		t.Errorf("after a delete the listing holds %d remote nodes, want the peer node back", len(got))
	}
}

// Before the snapshot an empty cache and a cluster with no peer nodes look the
// same, so nothing is created and, above all, nothing is removed.
func TestNothingIsListedOrRemovedBeforeTheSnapshot(t *testing.T) {
	r, c := aRemoteWorld(t, streamed(false), aStretchedCluster(true), aRemoteRecord(peerNodeID))
	reconcileRemote(t, r)

	if got := remoteNodes(t, c); len(got) != 1 {
		t.Errorf("a cold cache changed the listing to %d remote nodes", len(got))
	}
}

// U-78 (the status half): a remote node's status comes from the control plane,
// and it gains no finalizer, since deleting it removes nothing.
func TestARemoteNodesStatusComesFromTheControlPlane(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	record := aRemoteRecord(peerNodeID)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aStretchedCluster(true), record).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).Build()
	r := &StorageNodeReconciler{
		Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(64),
		Nodes: streamed(true, peerNode()), Workload: &Workload{Client: c},
	}

	key := client.ObjectKeyFromObject(record)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got simplyblockv1alpha2.StorageNode
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Status != "online" || got.Status.Hostname != "site-a-worker-1" {
		t.Errorf("status = %q on %q, want the control plane's online on site-a-worker-1",
			got.Status.Status, got.Status.Hostname)
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		t.Errorf("phase = %q, want Online", got.Status.Phase)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("a remote node gained finalizers %v; deleting it removes nothing", got.Finalizers)
	}
}

// U-84 (the delete half): deleting a remote record calls nothing on the control
// plane and raises no removal; the node is its own site's.
func TestDeletingARemoteNodeRemovesNothingFromTheControlPlane(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	record := aRemoteRecord(peerNodeID)
	record.Finalizers = []string{NodeFinalizer}
	now := metav1.NewTime(time.Now())
	record.DeletionTimestamp = &now
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aStretchedCluster(true), record).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).Build()
	r := &StorageNodeReconciler{
		Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(64),
		Nodes: streamed(true, peerNode()), Workload: &Workload{Client: c},
	}

	key := client.ObjectKeyFromObject(record)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var ops simplyblockv1alpha2.StorageNodeOpsList
	if err := c.List(context.Background(), &ops); err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(ops.Items) != 0 {
		t.Errorf("deleting a remote node raised %d operations, want none", len(ops.Items))
	}
	var gone simplyblockv1alpha2.StorageNode
	if err := c.Get(context.Background(), key, &gone); err == nil {
		t.Errorf("the remote record kept finalizers %v; its deletion waits on nothing", gone.Finalizers)
	}
}

// U-90: a remote node's devices are mirrored like any other, owned by the remote
// node, and say which site they are on and that they are remote.
func TestARemoteNodesDevicesSayTheyAreRemote(t *testing.T) {
	cache := &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{
		sdName(): {ID: sdDevice, ClusterID: sdCluster, StorageNodeID: sdNodeID, Status: "online"},
	}}
	node := sdNodeObject()
	node.Spec.Config.Site = "a"
	node.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	r := sdReconciler(t, cache, node)

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("a remote node's device was not mirrored: %v", err)
	}
	if !sd.Status.Remote || sd.Status.Site != "a" {
		t.Errorf("status = remote %v, site %q; want remote true on site a", sd.Status.Remote, sd.Status.Site)
	}
}

// A device of this site's own node is not remote.
func TestALocalNodesDevicesAreNotRemote(t *testing.T) {
	cache := &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{
		sdName(): {ID: sdDevice, ClusterID: sdCluster, StorageNodeID: sdNodeID, Status: "online"},
	}}
	node := sdNodeObject()
	node.Spec.Config.Site = "b"
	r := sdReconciler(t, cache, node)

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, _ := getSD(t, r.Client)
	if sd.Status.Remote || sd.Status.Site != "b" {
		t.Errorf("status = remote %v, site %q; want remote false on site b", sd.Status.Remote, sd.Status.Site)
	}
}
