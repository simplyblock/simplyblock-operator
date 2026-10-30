// The hub's StretchedCluster flow: two site-deployment flows, one per managed
// cluster, sized as one sync cluster with each site's stretched block, the
// leader approved first and the follower only once the leader's cluster exists,
// with the credentials to adopt it.

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	workv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	strName        = "production"
	strNamespace   = "simplyblock"
	strLeaderMC    = "site-a"
	strFollowerMC  = "site-b"
	strLeaderSite  = "a"
	strFollowerSit = "b"
	strUUID        = "c0ffee00-0000-0000-0000-000000000001"
	strSecret      = "the-cluster-secret"
)

type fakeStretchedPlane struct{ reads int }

func (f *fakeStretchedPlane) GetCluster(_ context.Context, id string) (webapi.ClusterResponse, error) {
	f.reads++
	return webapi.ClusterResponse{UUID: id, Secret: strSecret, SyncReplication: true}, nil
}

func newStretched(mutate func(*simplyblockv1alpha2.StretchedCluster)) *simplyblockv1alpha2.StretchedCluster {
	sc := &simplyblockv1alpha2.StretchedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: strName, Namespace: strNamespace, UID: types.UID("uid-stretched")},
		Spec: simplyblockv1alpha2.StretchedClusterSpec{
			ClusterName: strName,
			Sites: []simplyblockv1alpha2.StretchedSite{
				{Cluster: strLeaderMC, Site: strLeaderSite, Role: simplyblockv1alpha2.StretchedRoleLeader},
				{Cluster: strFollowerMC, Site: strFollowerSit, Role: simplyblockv1alpha2.StretchedRoleFollower,
					Discover: simplyblockv1alpha2.StorageSiteDiscovery{NodeSelector: map[string]string{"storage": "yes"}}},
			},
		},
	}
	if mutate != nil {
		mutate(sc)
	}
	return sc
}

func newStretchedReconciler(t *testing.T, objects ...client.Object) (*StretchedClusterReconciler, client.Client, *fakeStretchedPlane) {
	t.Helper()
	scheme := newTestScheme(t)
	scheme.AddKnownTypeWithName(managedClusterViewGVK, &unstructured.Unstructured{})
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	if err := workv1.Install(scheme); err != nil {
		t.Fatalf("register work/v1 scheme: %v", err)
	}
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.StretchedCluster{}, &workv1.ManifestWork{}}, objects...)
	plane := &fakeStretchedPlane{}
	return &StretchedClusterReconciler{Client: cl, Scheme: scheme, Recorder: &fakeRecorder{}, ControlPlane: plane}, cl, plane
}

func reconcileStretched(t *testing.T, r *StretchedClusterReconciler, cl client.Client, n int) *simplyblockv1alpha2.StretchedCluster {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: strNamespace, Name: strName}}
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var sc simplyblockv1alpha2.StretchedCluster
	if err := cl.Get(context.Background(), req.NamespacedName, &sc); err != nil {
		t.Fatalf("get request: %v", err)
	}
	return &sc
}

func siteWork(t *testing.T, cl client.Client, sc *simplyblockv1alpha2.StretchedCluster, mc, site string) *workv1.ManifestWork {
	t.Helper()
	var w workv1.ManifestWork
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: mc, Name: stretchedWorkName(sc, site)}, &w); err != nil {
		t.Fatalf("get the work for %s: %v", mc, err)
	}
	return &w
}

func project(t *testing.T, cl client.Client, sc *simplyblockv1alpha2.StretchedCluster, mc, site, suffix string, obj any) {
	t.Helper()
	v := getView(t, cl, mc, stretchedViewName(sc, site, suffix))
	result, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("convert projection: %v", err)
	}
	setViewResult(t, cl, v, result)
}

func stretchedDraft(sc *simplyblockv1alpha2.StretchedCluster, approved bool) *simplyblockv1alpha2.ClusterDeploymentConfig {
	d := siteDraft(approved, "", nil)
	d.Name = stretchedDraftName(sc)
	// The template carries the cluster's name the hub's apply wrote, which is
	// how an approved draft names the StorageCluster it expands into.
	d.Spec.Cluster = &simplyblockv1alpha2.ClusterTemplate{Name: sc.Spec.ClusterName}
	return d
}

// projectedCluster is the site's StorageCluster on the leader's backend cluster.
func projectedCluster(phase simplyblockv1alpha2.StorageClusterPhase, record string) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "StorageCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: strName, Namespace: strNamespace},
	}
	c.Status.UUID = strUUID
	c.Status.Phase = phase
	if record != "" {
		c.Status.Stretched = &simplyblockv1alpha2.StretchedStatus{LostSite: strFollowerSit, SiteRecord: record}
	}
	return c
}

func draftApply(t *testing.T, w *workv1.ManifestWork) map[string]any {
	t.Helper()
	cdc, ok := workManifests(t, w)["ClusterDeploymentConfig"]
	if !ok {
		return nil
	}
	spec, _ := cdc["spec"].(map[string]any)
	return spec
}

// U-18: one request becomes one work per managed cluster, each carrying a
// discovery scoped by that site's own discover block.
func TestAStretchedClusterDeliversADiscoveryToEachSite(t *testing.T) {
	sc := newStretched(nil)
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)

	for _, site := range got.Spec.Sites {
		ops, ok := workManifests(t, siteWork(t, cl, got, site.Cluster, site.Site))["OperatorOps"]
		if !ok {
			t.Fatalf("the work for %s carries no discovery", site.Cluster)
		}
		spec := ops["spec"].(map[string]any)
		discover := spec["discover"].(map[string]any)
		if discover["configName"] != stretchedDraftName(got) {
			t.Errorf("%s: discovery writes draft %v, want %s", site.Cluster, discover["configName"], stretchedDraftName(got))
		}
		_, scoped := discover["nodeSelector"]
		if want := site.Cluster == strFollowerMC; scoped != want {
			t.Errorf("%s: discovery nodeSelector present = %v, want %v", site.Cluster, scoped, want)
		}
	}
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseDiscovering {
		t.Errorf("phase = %q, want Discovering", got.Status.Phase)
	}
}

// U-19 and U-20: once a site's draft names nodes, its work sizes it as one site
// of a sync cluster: the shared sizing, the cluster's name, sync replication on,
// and that site's stretched block. Neither site's draft names a clusterRef: each
// creates its own StorageCluster, and the follower's adopts.
func TestEachDraftIsSizedAsOneSiteOfASyncCluster(t *testing.T) {
	dataChunks := int32(2)
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) {
		s.Spec.Sizing = &simplyblockv1alpha2.StorageSiteSizing{Stripe: &simplyblockv1alpha2.StripeSpec{DataChunks: &dataChunks}}
	})
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, false))
	}
	got = reconcileStretched(t, r, cl, 2)

	for _, site := range got.Spec.Sites {
		spec := draftApply(t, siteWork(t, cl, got, site.Cluster, site.Site))
		if spec == nil {
			t.Fatalf("%s: the work does not size the draft", site.Cluster)
		}
		if _, has := spec["clusterRef"]; has {
			t.Errorf("%s: the draft names a clusterRef; each site creates its own StorageCluster", site.Cluster)
		}
		tpl := spec["cluster"].(map[string]any)
		if tpl["name"] != strName || tpl["enableSyncReplication"] != true {
			t.Errorf("%s: template name %v, enableSyncReplication %v", site.Cluster, tpl["name"], tpl["enableSyncReplication"])
		}
		if stripe, _ := tpl["stripe"].(map[string]any); stripe == nil || stripe["dataChunks"] != float64(2) {
			t.Errorf("%s: the shared sizing is missing: %v", site.Cluster, tpl["stripe"])
		}
		stretched, _ := tpl["stretched"].(map[string]any)
		peer := strLeaderSite
		if site.Site == strLeaderSite {
			peer = strFollowerSit
		}
		if stretched["site"] != site.Site || stretched["peerSite"] != peer || stretched["role"] != string(site.Role) {
			t.Errorf("%s: stretched block %v, want site %s, peer %s, role %s", site.Cluster, stretched, site.Site, peer, site.Role)
		}
		if spec["approved"] != false {
			t.Errorf("%s: an unapproved request approved the draft", site.Cluster)
		}
	}
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseDrafted {
		t.Errorf("phase = %q, want Drafted", got.Status.Phase)
	}
}

// Design §7: each draft's stretched block names the other site's managed
// cluster, which is the name the site link addresses that site's operator by.
func TestEachDraftNamesThePeersManagedCluster(t *testing.T) {
	r, cl, _ := newStretchedReconciler(t, newStretched(nil))
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, false))
	}
	got = reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		tpl := draftApply(t, siteWork(t, cl, got, site.Cluster, site.Site))["cluster"].(map[string]any)
		stretched, _ := tpl["stretched"].(map[string]any)
		want := strLeaderMC
		if site.Cluster == strLeaderMC {
			want = strFollowerMC
		}
		if stretched["peerCluster"] != want {
			t.Errorf("%s: stretched.peerCluster = %v, want %s", site.Cluster, stretched["peerCluster"], want)
		}
	}
}

// §5.4: a site whose StorageCluster holds in its preflight states why in its
// status message, and the hub carries that reason into status.sites[], so the
// hold reads from the hub without a site kubeconfig.
func TestTheHubShowsWhyASitesStorageClusterIsHeld(t *testing.T) {
	const reason = "follower site: backend cluster c0ffee00 already has node(s) f0e1 on site b"
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, true))
	}
	got = reconcileStretched(t, r, cl, 1)
	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseProvisioning, ""))
	got = reconcileStretched(t, r, cl, 1)
	held := projectedCluster(simplyblockv1alpha2.StorageClusterPhaseCreating, "")
	held.Status.UUID = ""
	held.Status.Message = reason
	project(t, cl, got, strFollowerMC, strFollowerSit, "cluster", held)
	got = reconcileStretched(t, r, cl, 1)

	for _, s := range got.Status.Sites {
		if s.Cluster == strFollowerMC && !strings.Contains(s.Message, reason) {
			t.Errorf("status.sites[%s].message = %q, want the StorageCluster's hold reason %q", s.Cluster, s.Message, reason)
		}
	}
}

// The images a stretched cluster pins reach both drafts, so the two sites'
// storage nodes run the same SPDK and SPDK proxy. Without them each site's
// expansion falls back to the control plane's default SPDK image, which need not
// carry synchronous replication.
func TestBothDraftsPinTheStretchedClustersImages(t *testing.T) {
	const spdk = "example.test/ultra:sync"
	const proxy = "example.test/simplyblock:stretched"
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) {
		s.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
			SPDK:      &simplyblockv1alpha2.ImageSpec{Image: spdk},
			SPDKProxy: &simplyblockv1alpha2.ImageSpec{Image: proxy},
		}
	})
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, false))
	}
	got = reconcileStretched(t, r, cl, 2)

	for _, site := range got.Spec.Sites {
		spec := draftApply(t, siteWork(t, cl, got, site.Cluster, site.Site))
		images, _ := spec["images"].(map[string]any)
		spdkSlot, _ := images["spdk"].(map[string]any)
		proxySlot, _ := images["spdkProxy"].(map[string]any)
		if spdkSlot["image"] != spdk || proxySlot["image"] != proxy {
			t.Errorf("%s: the draft's images are %v, want spdk %s and spdkProxy %s", site.Cluster, spec["images"], spdk, proxy)
		}
	}
}

// U-21: approval reaches the leader's draft at once and the follower's only once
// the leader's cluster exists, together with the credentials to adopt it, so the
// follower never races the create.
func TestTheFollowerIsApprovedOnlyOnceTheLeadersClusterExists(t *testing.T) {
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, plane := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, false))
	}
	got = reconcileStretched(t, r, cl, 2)

	if spec := draftApply(t, siteWork(t, cl, got, strLeaderMC, strLeaderSite)); spec["approved"] != true {
		t.Fatalf("the leader's draft is not approved: %v", spec)
	}
	follower := siteWork(t, cl, got, strFollowerMC, strFollowerSit)
	if spec := draftApply(t, follower); spec["approved"] != false {
		t.Fatalf("the follower was approved before the leader's cluster existed: %v", spec)
	}
	if _, has := workManifests(t, follower)["Secret"]; has {
		t.Fatal("credentials were delivered before there was a cluster to adopt")
	}

	// The leader's draft expands and its StorageCluster reports a UUID.
	project(t, cl, got, strLeaderMC, strLeaderSite, "draft", stretchedDraft(got, true))
	got = reconcileStretched(t, r, cl, 1)
	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseProvisioning, ""))
	got = reconcileStretched(t, r, cl, 2)

	follower = siteWork(t, cl, got, strFollowerMC, strFollowerSit)
	if spec := draftApply(t, follower); spec["approved"] != true {
		t.Errorf("the follower was not approved once the leader's cluster existed: %v", spec)
	}
	secret, has := workManifests(t, follower)["Secret"]
	if !has {
		t.Fatal("the follower received no credentials to adopt the leader's cluster")
	}
	meta := secret["metadata"].(map[string]any)
	data := secret["stringData"].(map[string]any)
	if meta["name"] != "simplyblock-"+strName+"-upgrade" || data["uuid"] != strUUID || data["secret"] != strSecret {
		t.Errorf("credentials secret %v / %v", meta["name"], data)
	}
	if plane.reads == 0 {
		t.Error("the cluster secret was never read from the hub's control plane")
	}
	if got.Status.ClusterUUID != strUUID {
		t.Errorf("status.clusterUUID = %q, want the leader's", got.Status.ClusterUUID)
	}
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseLeaderCreating {
		t.Errorf("phase = %q, want LeaderCreating while the leader's cluster is not Online", got.Status.Phase)
	}
}

// Regression: 2026-10-22-follower-approval-withdrawn: once the leader's draft
// failed, the hub's reading of the leader stopped before its StorageCluster, the
// leader's UUID read as empty, and the hub put approved: false and no
// credentials into the follower's work. The site refused the withdrawal of an
// approved draft, the work stopped applying, and the follower could neither
// adopt nor be re-run.
func TestTheFollowersApprovalAndCredentialsOutliveALostReadOfTheLeader(t *testing.T) {
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, true))
	}
	got = reconcileStretched(t, r, cl, 1)
	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseProvisioning, ""))
	got = reconcileStretched(t, r, cl, 2)
	if spec := draftApply(t, siteWork(t, cl, got, strFollowerMC, strFollowerSit)); spec["approved"] != true {
		t.Fatalf("precondition: the follower was not approved once the leader's cluster existed: %v", spec)
	}

	// The leader's draft fails, so the hub no longer reads the leader's
	// StorageCluster.
	failed := stretchedDraft(got, true)
	failed.Status.Phase = simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed
	failed.Status.Message = "step Activating outlived its deadline"
	project(t, cl, got, strLeaderMC, strLeaderSite, "draft", failed)
	got = reconcileStretched(t, r, cl, 2)

	follower := siteWork(t, cl, got, strFollowerMC, strFollowerSit)
	if spec := draftApply(t, follower); spec["approved"] != true {
		t.Errorf("the follower's approval was withdrawn: %v", spec)
	}
	if _, has := workManifests(t, follower)["Secret"]; !has {
		t.Error("the follower's credentials to adopt the leader's cluster were withdrawn")
	}
	if got.Status.ClusterUUID != strUUID {
		t.Errorf("status.clusterUUID = %q, want the leader's %s kept", got.Status.ClusterUUID, strUUID)
	}
}

// The phase walks LeaderCreating -> FollowerJoining -> Ready as the two sites'
// clusters come online, and Ready turns Degraded when the control plane records
// a site as out.
func TestTheRequestIsReadyWhenBothSitesAreOnline(t *testing.T) {
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, true))
	}
	got = reconcileStretched(t, r, cl, 1)
	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseOnline, ""))
	got = reconcileStretched(t, r, cl, 1)
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseFollowerJoining {
		t.Fatalf("phase = %q, want FollowerJoining with the leader Online", got.Status.Phase)
	}

	project(t, cl, got, strFollowerMC, strFollowerSit, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseOnline, ""))
	got = reconcileStretched(t, r, cl, 1)
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseReady {
		t.Fatalf("phase = %q, want Ready with both sites Online", got.Status.Phase)
	}

	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseOnline, "down"))
	got = reconcileStretched(t, r, cl, 1)
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseDegraded {
		t.Errorf("phase = %q, want Degraded with a site recorded down", got.Status.Phase)
	}
	for _, s := range got.Status.Sites {
		if s.Cluster == strLeaderMC && s.SiteRecord != "down" {
			t.Errorf("status.sites[%s].siteRecord = %q, want down", s.Cluster, s.SiteRecord)
		}
	}
}

// The lost site the control plane records is projected once on the request,
// read from whichever site reports it, so the role-swap guard can tell a lost
// leader from a lost follower (design-stretched-sync-replication-ramen.md §11.1).
func TestTheLostSiteIsProjectedOnTheRequest(t *testing.T) {
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, true))
	}
	got = reconcileStretched(t, r, cl, 1)
	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseDegraded, "done"))
	got = reconcileStretched(t, r, cl, 1)
	if got.Status.LostSite != strFollowerSit {
		t.Errorf("status.lostSite = %q, want %q as the leader site's StorageCluster reports it", got.Status.LostSite, strFollowerSit)
	}

	project(t, cl, got, strLeaderMC, strLeaderSite, "cluster",
		projectedCluster(simplyblockv1alpha2.StorageClusterPhaseOnline, ""))
	got = reconcileStretched(t, r, cl, 1)
	if got.Status.LostSite != "" {
		t.Errorf("status.lostSite = %q after the site returned, want empty", got.Status.LostSite)
	}
}

// U-94: a changed discovery on one site starts another discovery on that site
// only.
func TestAChangedDiscoveryReRunsThatSiteOnly(t *testing.T) {
	sc := newStretched(nil)
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	name := func(mc, site string) any {
		return workManifests(t, siteWork(t, cl, got, mc, site))["OperatorOps"]["metadata"].(map[string]any)["name"]
	}
	leaderBefore, followerBefore := name(strLeaderMC, strLeaderSite), name(strFollowerMC, strFollowerSit)

	got.Spec.Sites[1].Discover.NodeSelector = map[string]string{"storage": "only-these"}
	if err := cl.Update(context.Background(), got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got = reconcileStretched(t, r, cl, 1)
	if name(strLeaderMC, strLeaderSite) != leaderBefore {
		t.Error("a change to the follower's discovery re-ran the leader's")
	}
	if name(strFollowerMC, strFollowerSit) == followerBefore {
		t.Error("the follower's changed discovery did not start a new run")
	}
}

// Deleting the request releases both works and every view, and orphans what
// the sites built.
func TestDeletingTheRequestReleasesBothSites(t *testing.T) {
	sc := newStretched(nil)
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		if orphan := siteWork(t, cl, got, site.Cluster, site.Site).Spec.DeleteOption; orphan == nil ||
			orphan.PropagationPolicy != workv1.DeletePropagationPolicyTypeOrphan {
			t.Errorf("%s: the work does not orphan the site's storage", site.Cluster)
		}
	}

	if err := cl.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: strNamespace, Name: strName}}
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for _, site := range got.Spec.Sites {
		var w workv1.ManifestWork
		err := cl.Get(context.Background(), client.ObjectKey{Namespace: site.Cluster, Name: stretchedWorkName(got, site.Site)}, &w)
		if err == nil {
			t.Errorf("%s: the work survived the request", site.Cluster)
		}
	}
	var gone simplyblockv1alpha2.StretchedCluster
	if err := cl.Get(context.Background(), req.NamespacedName, &gone); err == nil {
		t.Errorf("the request kept its finalizer: %v", gone.Finalizers)
	}
}

// Regression: 2026-10-26-refused-swap-cannot-be-undone: a site that refused the
// work the hub re-delivered lost its StorageCluster from the request's status,
// so the role-swap guard, which reads both sites' StorageClusters, refused the
// swap back too (live, 12-leader-swap.sh). A refused work still projects what
// the site runs.
func TestARefusedWorkStillProjectsTheSitesStorageCluster(t *testing.T) {
	sc := newStretched(func(s *simplyblockv1alpha2.StretchedCluster) { s.Spec.Approved = true })
	r, cl, _ := newStretchedReconciler(t, sc)
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, true))
	}
	got = reconcileStretched(t, r, cl, 1)
	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "cluster", projectedCluster(simplyblockv1alpha2.StorageClusterPhaseOnline, ""))
	}
	got = reconcileStretched(t, r, cl, 1)

	w := siteWork(t, cl, got, strFollowerMC, strFollowerSit)
	w.Status.ResourceStatus.Manifests = []workv1.ManifestCondition{{
		ResourceMeta: workv1.ManifestResourceMeta{Kind: "ClusterDeploymentConfig", Name: stretchedDraftName(got)},
		Conditions: []metav1.Condition{{Type: workv1.ManifestApplied, Status: metav1.ConditionFalse,
			Reason: "AppliedManifestFailed", Message: "an approved deployment config is immutable"}},
	}}
	if err := cl.Status().Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	got = reconcileStretched(t, r, cl, 1)
	for _, s := range got.Status.Sites {
		if s.Cluster != strFollowerMC {
			continue
		}
		if s.StorageCluster == nil || s.StorageCluster.Phase != string(simplyblockv1alpha2.StorageClusterPhaseOnline) {
			t.Errorf("status.sites[%s].storageCluster = %+v; want the site's Online StorageCluster still projected", s.Cluster, s.StorageCluster)
		}
		if s.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed {
			t.Errorf("status.sites[%s].phase = %q; want Failed with the refusal", s.Cluster, s.Phase)
		}
	}
}
