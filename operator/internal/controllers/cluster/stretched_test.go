// The StorageCluster controller's behavior on one site of a stretched cluster:
// a leader creates the backend cluster, a follower only ever adopts the one its
// leader created and never deletes it, and both project the control plane's
// site record into status.stretched.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	thisSite = "b"
	peerSite = "a"
)

func asSite(role simplyblockv1alpha2.StretchedRole) func(*simplyblockv1alpha2.StorageCluster) {
	return func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.EnableSyncReplication = ptr.To(true)
		c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: thisSite, PeerSite: peerSite, Role: role}
	}
}

func syncCluster() atlascp.Cluster {
	reading := activeCluster()
	reading.SyncReplication = true
	return reading
}

func upgradeSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: objectMeta("simplyblock-" + testClusterName + "-upgrade"),
		Data: map[string][]byte{
			"uuid":   []byte(testClusterUUID),
			"secret": []byte(testClusterSecret),
		},
	}
}

func clusterSecretOf() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: objectMeta("simplyblock-cluster-" + testClusterName),
		Data: map[string][]byte{
			"uuid":   []byte(testClusterUUID),
			"secret": []byte(testClusterSecret),
		},
	}
}

// U-01: the leader runs the ordinary creation path, as a sync cluster.
func TestALeaderCreatesASyncReplicationCluster(t *testing.T) {
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

	cluster := reconcileCluster(t, r, 6)
	if api.createCalls != 1 {
		t.Fatalf("the leader posted %d clusters, want 1 (step %q, message %q)",
			api.createCalls, cluster.Status.Step.State, cluster.Status.Message)
	}
	if !posted.SyncReplication {
		t.Error("the leader's create body does not ask for sync_replication")
	}
}

// U-03 (Regression: 2026-10-22-follower-creates-a-second-cluster): a follower
// whose leader has not created the cluster yet waits for it. The creation
// machine's only other exits post a create, which on a follower makes a second
// backend cluster of the same name that the leader's nodes never join.
func TestAFollowerWaitsForItsLeaderInsteadOfCreating(t *testing.T) {
	api := &fakeControlPlane{
		ready:  func() error { return nil },
		byName: func(string) (atlascp.Cluster, bool, error) { return atlascp.Cluster{}, false, nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)))

	cluster := reconcileCluster(t, r, 6)
	if api.createCalls != 0 {
		t.Fatalf("a follower posted %d clusters; only its leader may create one", api.createCalls)
	}
	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q with no cluster to adopt", cluster.Status.UUID)
	}
	if got := cluster.Status.Step.State; got != string(simplyblockv1alpha2.StorageClusterStepCheckingControlPlane) {
		t.Errorf("step = %q, want the follower held in CheckingControlPlane", got)
	}
	if !rec.has(PeerClusterMissing) {
		t.Error("a follower waiting for its leader emitted no PeerClusterMissing")
	}
}

// A follower that can see the leader's cluster by name but holds no credentials
// for it waits for the hub to deliver them. Adoption by name yields no secret,
// and posting a create would make a second cluster.
func TestAFollowerWaitsForItsCredentialsWhenTheClusterExists(t *testing.T) {
	api := &fakeControlPlane{
		ready: func() error { return nil },
		byName: func(string) (atlascp.Cluster, bool, error) {
			return atlascp.Cluster{ID: testClusterUUID, Name: testClusterName}, true, nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)))

	cluster := reconcileCluster(t, r, 6)
	if api.createCalls != 0 {
		t.Fatalf("a follower posted %d clusters", api.createCalls)
	}
	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q before any credentials were delivered", cluster.Status.UUID)
	}
	if !rec.has(PeerClusterMissing) {
		t.Error("a follower waiting for credentials emitted no PeerClusterMissing")
	}
}

// U-02: the follower adopts through the credentials the hub delivered, without
// posting anything.
func TestAFollowerAdoptsThroughTheDeliveredCredentials(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != testClusterUUID {
		t.Fatalf("status.uuid = %q, want the leader's cluster (step %q, message %q)",
			cluster.Status.UUID, cluster.Status.Step.State, cluster.Status.Message)
	}
	if api.createCalls != 0 {
		t.Errorf("an adopting follower posted %d clusters", api.createCalls)
	}
	if !rec.has(ClusterAdopted) {
		t.Error("the follower's adoption emitted no ClusterAdopted")
	}
}

// U-04: a stretched cluster is a sync cluster, so a follower pointed at a
// cluster without sync replication refuses to join it.
func TestAFollowerRefusesAClusterThatIsNotASyncCluster(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q: the follower joined a cluster that is not a sync cluster", cluster.Status.UUID)
	}
	if api.createCalls != 0 {
		t.Errorf("a follower posted %d clusters", api.createCalls)
	}
	if !rec.has(SyncReplicationMismatch) {
		t.Error("the refusal emitted no SyncReplicationMismatch")
	}
}

// Regression: 2026-10-22-follower-deletes-the-shared-cluster. Deleting a
// follower's StorageCluster deleted the backend cluster behind it, which is the
// leader's cluster and every node of both sites. A follower only lets go.
func TestAFollowerNeverDeletesTheSharedCluster(t *testing.T) {
	api := &fakeControlPlane{}
	now := metav1.Now()
	deleting := newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower),
		func(c *simplyblockv1alpha2.StorageCluster) { c.DeletionTimestamp = &now })
	r := newClusterReconciler(t, api, &recorder{}, deleting, clusterSecretOf())

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	reconcileOnce(t, r)
	if api.deleteCalls != 0 {
		t.Fatalf("a follower asked the control plane to delete the shared cluster %d times", api.deleteCalls)
	}
	var gone simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, key, &gone); err == nil && len(gone.Finalizers) > 0 {
		t.Errorf("the follower's finalizer was kept: %v", gone.Finalizers)
	}
}

// U-79: the control plane's record of a lost site reaches status on either site,
// with one event per change and none on a pass that changes nothing.
func TestTheSiteRecordIsProjectedAndAnnouncedOnce(t *testing.T) {
	reading := syncCluster()
	reading.LostSite = peerSite
	reading.LostSiteState = "fencing"
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return reading, nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), clusterSecretOf())

	cluster := reconcileCluster(t, r, 2)
	got := cluster.Status.Stretched
	if got == nil {
		t.Fatal("status.stretched is unset on a stretched cluster")
	}
	if got.LostSite != peerSite || got.SiteRecord != "fencing" {
		t.Errorf("status.stretched = %+v, want lostSite %q and siteRecord fencing", got, peerSite)
	}
	if got.Role != simplyblockv1alpha2.StretchedRoleFollower {
		t.Errorf("status.stretched.role = %q, want Follower", got.Role)
	}
	if n := rec.count(SiteRecordChanged); n != 1 {
		t.Errorf("SiteRecordChanged was emitted %d times over two passes with one change, want 1", n)
	}
}

// The peer site's node counts come from its remote StorageNodes, which mirror
// the control plane's node list for the other site.
func TestPeerNodesAreCountedFromTheRemoteNodes(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	remote := func(name, status string) *simplyblockv1alpha2.StorageNode {
		return &simplyblockv1alpha2.StorageNode{
			ObjectMeta: objectMeta(name),
			Spec: simplyblockv1alpha2.StorageNodeSpec{
				ClusterRef: testClusterName,
				Config:     simplyblockv1alpha2.StorageNodeConfig{Site: peerSite},
				Stretched:  &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
			},
			Status: simplyblockv1alpha2.StorageNodeStatus{Status: status},
		}
	}
	local := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: objectMeta("local-1"),
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: testClusterName, WorkerNode: "worker-1",
			Config: simplyblockv1alpha2.StorageNodeConfig{Site: thisSite},
		},
		Status: simplyblockv1alpha2.StorageNodeStatus{Status: utils.NodeStatusOnline},
	}
	r := newClusterReconciler(t, api, &recorder{},
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)), clusterSecretOf(),
		remote("peer-1", utils.NodeStatusOnline), remote("peer-2", "offline"), local)

	got := reconcileCluster(t, r, 1).Status.Stretched
	if got == nil || got.PeerNodes != 2 || got.PeerNodesOnline != 1 {
		t.Errorf("status.stretched = %+v, want peerNodes 2 and peerNodesOnline 1", got)
	}
}

// U-08/U-09 (the status half): a role swap delivered from the hub is taken up
// on the next pass and announced.
func TestARoleSwapIsRecordedAndAnnounced(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	rec := &recorder{}
	swapped := newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower),
		func(c *simplyblockv1alpha2.StorageCluster) {
			c.Status.Stretched = &simplyblockv1alpha2.StretchedStatus{Role: simplyblockv1alpha2.StretchedRoleLeader}
		})
	r := newClusterReconciler(t, api, rec, swapped, clusterSecretOf())

	got := reconcileCluster(t, r, 1).Status.Stretched
	if got == nil || got.Role != simplyblockv1alpha2.StretchedRoleFollower {
		t.Errorf("status.stretched = %+v, want role Follower after the swap", got)
	}
	if !rec.has(RoleChanged) {
		t.Error("a role swap emitted no RoleChanged")
	}
}

// U-17 (status half): a standalone cluster gains no stretched status.
func TestAStandaloneClusterHasNoStretchedStatus(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster(), clusterSecretOf())
	if got := reconcileCluster(t, r, 1).Status.Stretched; got != nil {
		t.Errorf("a standalone cluster carries status.stretched %+v", got)
	}
}

// U-26 (the operator half): the CSI entry tells the node plugin it is on a sync
// cluster and which site it is on.
func TestTheCSIEntryCarriesSyncAndSite(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{},
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), clusterSecretOf())
	reconcileCluster(t, r, 1)

	entry := csiEntry(t, r.Client)
	if !entry.Sync || entry.Site != thisSite {
		t.Errorf("CSI entry = sync %v, site %q; want sync true, site %q", entry.Sync, entry.Site, thisSite)
	}
}

func TestAStandaloneClustersCSIEntryCarriesNoSite(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster(), clusterSecretOf())
	reconcileCluster(t, r, 1)

	entry := csiEntry(t, r.Client)
	if entry.Sync || entry.Site != "" {
		t.Errorf("a standalone cluster's CSI entry = sync %v, site %q", entry.Sync, entry.Site)
	}
}

func csiEntry(t *testing.T, c client.Client) CSIClusterEntry {
	t.Helper()
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: testNamespace, Name: csiCredentialsSecret}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("read the CSI credentials secret: %v", err)
	}
	var creds CSICredentials
	if err := json.Unmarshal(secret.Data["secret.json"], &creds); err != nil {
		t.Fatalf("unmarshal secret.json: %v", err)
	}
	for _, entry := range creds.Clusters {
		if entry.ClusterID == testClusterUUID {
			return entry
		}
	}
	t.Fatalf("no CSI entry for %s in %+v", testClusterUUID, creds.Clusters)
	return CSIClusterEntry{}
}

// reconcileOnce runs one pass without reading the cluster back, for a pass that
// may delete it.
func reconcileOnce(t *testing.T, r *StorageClusterReconciler) {
	t.Helper()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// U-08: a role swap that made this site a follower ends an in-flight
// cluster-wide operation that can still be stopped, because the cluster's
// cluster-wide operations now belong to the other site.
func TestARoleSwapEndsAStoppableClusterWideOperation(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: string(stepRequesting)}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, &fakeControlPlane{}, rec,
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), ops)

	got, _ := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed on a follower", got.Status.Phase)
	}
	if !rec.has(RoleLost) {
		t.Error("the operation ended by a role swap emitted no RoleLost")
	}
}

// An operation the control plane is part-way through runs on after a role swap:
// failing the record would not stop the work, it would only stop accounting
// for it.
func TestARoleSwapLeavesAnUnstoppableOperationRunning(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (atlascp.Cluster, error) {
		return activeCluster(), nil
	}}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: string(stepAwaiting)}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec,
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), ops)

	got, _ := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation still Running", got.Status.Phase)
	}
	if rec.has(RoleLost) {
		t.Error("an operation that cannot be stopped was reported as ended by the role swap")
	}
}

// A task cancel is not cluster-wide, so a follower runs it on.
func TestARoleSwapLeavesATaskCancelAlone(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.Abort = true // ends it at the first abortable step, a sign it was not RoleLost
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: string(stepRequesting)}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, &fakeControlPlane{}, rec,
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), ops)

	reconcileOps(t, r, 1)
	if rec.has(RoleLost) {
		t.Error("a task cancel on a follower was ended as RoleLost")
	}
}

// fakeSiteLink answers a probe of the other site's operator with err, and
// records which site it was asked about.
type fakeSiteLink struct {
	err   error
	asked string
}

func (f *fakeSiteLink) ProbePeer(_ context.Context, site string) error {
	f.asked = site
	return f.err
}

// Design §7: a stretched site reports whether the site link reaches the other
// site's operator, addressed by the other site's member name.
func TestTheSiteLinkToThePeerSiteIsReported(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want simplyblockv1alpha2.StretchedSiteLink
	}{
		{name: "the peer answers", want: simplyblockv1alpha2.StretchedSiteLinkConnected},
		{name: "the peer does not", err: errors.New("no live session"), want: simplyblockv1alpha2.StretchedSiteLinkUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeControlPlane{
				cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
			}
			link := &fakeSiteLink{err: tc.err}
			cluster := newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader))
			cluster.Spec.Stretched.PeerCluster = "member-a"
			r := newClusterReconciler(t, api, &recorder{}, cluster, clusterSecretOf())
			r.SiteLink = link

			got := reconcileCluster(t, r, 1).Status.Stretched
			if got == nil || got.SiteLink != tc.want {
				t.Errorf("status.stretched.siteLink = %+v, want %s", got, tc.want)
			}
			if link.asked != "member-a" {
				t.Errorf("probed site %q, want the peer's member name member-a", link.asked)
			}
		})
	}
}

// A cluster whose site link is not configured reports nothing about it.
func TestNoSiteLinkReportsNothing(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{},
		newTestCluster(asSite(simplyblockv1alpha2.StretchedRoleLeader)), clusterSecretOf())
	if got := reconcileCluster(t, r, 1).Status.Stretched; got == nil || got.SiteLink != "" {
		t.Errorf("status.stretched = %+v, want no siteLink without a site link", got)
	}
}
