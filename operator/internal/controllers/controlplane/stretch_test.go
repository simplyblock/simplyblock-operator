// One site of a stretched control plane (docs/designs/design-stretched-control-plane.md):
// the FoundationDBCluster it renders is one region of a database the two sites
// share, every control-plane workload learns its site, and a Join site starts
// from the Bootstrap site's connection string or not at all.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func stretchedControlPlane(role simplyblockv1alpha2.StretchedControlPlaneRole) *simplyblockv1alpha2.ControlPlane {
	cp := localControlPlane()
	site, peer := "a", "b"
	if role == simplyblockv1alpha2.StretchedControlPlaneRoleJoin {
		site, peer = "b", "a"
	}
	cp.Spec.Source.Local.Stretch = &simplyblockv1alpha2.StretchedControlPlaneSpec{
		Site:                          site,
		PeerSite:                      peer,
		Role:                          role,
		APIEndpoint:                   "http://cp-" + site + ".example:5000",
		SeedConnectionStringSecretRef: &corev1.LocalObjectReference{Name: "fdb-seed"},
	}
	return cp
}

func regionIDs(t *testing.T, cluster *unstructured.Unstructured) []string {
	t.Helper()
	regions, found, err := unstructured.NestedSlice(cluster.Object, "spec", "databaseConfiguration", "regions")
	if err != nil || !found {
		t.Fatalf("spec.databaseConfiguration.regions missing: %v", err)
	}
	ids := make([]string, 0, len(regions))
	for _, region := range regions {
		dcs := region.(map[string]any)["datacenters"].([]any)
		ids = append(ids, dcs[0].(map[string]any)["id"].(string))
	}
	return ids
}

// Both sites render the same database: two regions, the Bootstrap site's
// first and with the priority, three copies in each; and each site's own
// processes carry its name as their data center and process-group prefix.
func TestBothSitesRenderTheSameTwoRegionDatabase(t *testing.T) {
	bootstrap := foundationDBCluster(stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap), "", true)
	join := foundationDBCluster(stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleJoin), "sb:x@10.0.0.1:4500", true)

	for name, cluster := range map[string]*unstructured.Unstructured{"bootstrap": bootstrap, "join": join} {
		if got := regionIDs(t, cluster); strings.Join(got, ",") != "a,b" {
			t.Errorf("%s site renders regions %v, want [a b] with the Bootstrap site first", name, got)
		}
		mode, _, _ := unstructured.NestedString(cluster.Object, "spec", "databaseConfiguration", "redundancy_mode")
		if mode != "triple" {
			t.Errorf("%s site: redundancy_mode = %q, want triple (three copies per site)", name, mode)
		}
		usable, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "databaseConfiguration", "usable_regions")
		if usable != 2 {
			t.Errorf("%s site: usable_regions = %d, want 2", name, usable)
		}
		for _, class := range []string{"storage", "log"} {
			n, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "processCounts", class)
			if n < 3 {
				t.Errorf("%s site runs %d %s processes, and triple needs three in each region", name, n, class)
			}
		}
	}

	dc, _, _ := unstructured.NestedString(bootstrap.Object, "spec", "dataCenter")
	prefix, _, _ := unstructured.NestedString(bootstrap.Object, "spec", "processGroupIDPrefix")
	if dc != "a" || prefix != "a" {
		t.Errorf("bootstrap site: dataCenter=%q processGroupIDPrefix=%q, want both a", dc, prefix)
	}
	dc, _, _ = unstructured.NestedString(join.Object, "spec", "dataCenter")
	prefix, _, _ = unstructured.NestedString(join.Object, "spec", "processGroupIDPrefix")
	if dc != "b" || prefix != "b" {
		t.Errorf("join site: dataCenter=%q processGroupIDPrefix=%q, want both b", dc, prefix)
	}
	if _, found, _ := unstructured.NestedString(bootstrap.Object, "spec", "seedConnectionString"); found {
		t.Error("the Bootstrap site creates the database; it must not carry a seed connection string")
	}
	if seed, _, _ := unstructured.NestedString(join.Object, "spec", "seedConnectionString"); seed != "sb:x@10.0.0.1:4500" {
		t.Errorf("join site: seedConnectionString = %q, want the Bootstrap site's", seed)
	}
}

// An ordinary control plane renders exactly what it did before: one region,
// no data center, no prefix.
func TestAnOrdinaryControlPlaneRendersNoRegions(t *testing.T) {
	cluster := foundationDBCluster(localControlPlane(), "", false)
	if _, found, _ := unstructured.NestedSlice(cluster.Object, "spec", "databaseConfiguration", "regions"); found {
		t.Error("a single-site control plane renders a regions block")
	}
	for _, key := range []string{"dataCenter", "processGroupIDPrefix", "seedConnectionString"} {
		if _, found, _ := unstructured.NestedString(cluster.Object, "spec", key); found {
			t.Errorf("a single-site control plane sets spec.%s", key)
		}
	}
	mode, _, _ := unstructured.NestedString(cluster.Object, "spec", "databaseConfiguration", "redundancy_mode")
	if mode != "double" {
		t.Errorf("redundancy_mode = %q, want double for the default three coordinators", mode)
	}
}

// Every workload built from the control plane's image learns the site, which
// is what the lease agent, the service gate and the API's redirect key on.
func TestEveryControlPlaneWorkloadLearnsItsSite(t *testing.T) {
	cp := stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap)

	var checked int
	for _, obj := range managementAPIObjects(cp) {
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Image != localImage(cp) {
				continue
			}
			checked++
			var found bool
			for _, e := range c.Env {
				if e.Name == controlPlaneSiteEnv && e.Value == "a" {
					found = true
				}
			}
			if !found {
				t.Errorf("Deployment %s container %s does not carry %s=a", d.Name, c.Name, controlPlaneSiteEnv)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no container runs the control plane's image; the test checked nothing")
	}

	plain := findDeployment(t, managementAPIObjects(localControlPlane()), ComponentWebAPI)
	for _, e := range plain.Spec.Template.Spec.Containers[0].Env {
		if e.Name == controlPlaneSiteEnv {
			t.Fatalf("a single-site control plane names a site: %#v", e)
		}
	}
}

// A Join site renders its database from the Bootstrap site's connection string
// and from nothing else: without the Secret, or with an empty one, the step
// holds rather than creating a second database.
func TestAJoinSiteHoldsWithoutTheSeedConnectionString(t *testing.T) {
	cp := stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleJoin)

	for name, tc := range map[string]struct {
		secret *corev1.Secret
		want   string
	}{
		"missing": {secret: nil, want: "fdb-seed"},
		"empty": {secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "fdb-seed", Namespace: testNamespace},
		}, want: "cluster-file"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := []client.Object{cp}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			r := &ControlPlaneReconciler{Client: newClient(t, objs...), Scheme: testScheme(t)}
			seed, err := r.seedConnectionString(context.Background(), cp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("seed = %q, err = %v; want an error naming %q", seed, err, tc.want)
			}
		})
	}

	ready := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fdb-seed", Namespace: testNamespace},
		Data:       map[string][]byte{"cluster-file": []byte("sb:x@10.0.0.1:4500\n")},
	}
	r := &ControlPlaneReconciler{Client: newClient(t, cp, ready), Scheme: testScheme(t)}
	seed, err := r.seedConnectionString(context.Background(), cp)
	if err != nil || seed != "sb:x@10.0.0.1:4500" {
		t.Fatalf("seed = %q, err = %v", seed, err)
	}

	bootstrap := &ControlPlaneReconciler{Client: newClient(t), Scheme: testScheme(t)}
	seed, err = bootstrap.seedConnectionString(context.Background(),
		stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap))
	if err != nil || seed != "" {
		t.Fatalf("the Bootstrap site read a seed: %q, %v", seed, err)
	}
}

// The Bootstrap site installs alone: its database is two-region from the
// start and cannot be fully replicated until the Join site's region exists,
// so a stretched site waits for availability only.
func TestAStretchedSiteDoesNotWaitForTheSecondRegion(t *testing.T) {
	h := fdbHealth{found: true, available: true, fullReplication: false, desired: 7, reconciled: 7}
	if got := h.waitingOn(true); got != "" {
		t.Errorf("a stretched site waits on %q; its own region is available", got)
	}
	if got := h.waitingOn(false); got == "" {
		t.Error("a single-site control plane must still wait for full replication")
	}
	if got := (fdbHealth{found: true}).waitingOn(true); got == "" {
		t.Error("an unavailable database is still waited for on a stretched site")
	}
}

// Until the peer's processes have joined, a site runs the database on its own
// region: one usable region, the other named at a negative priority so that
// processes joining under it take no data. FoundationDB then wants five
// coordinators, which one site provides; nine, once both regions are usable.
func TestASiteAloneConfiguresOneUsableRegion(t *testing.T) {
	alone := foundationDBCluster(stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap), "", false)
	usable, _, _ := unstructured.NestedInt64(alone.Object, "spec", "databaseConfiguration", "usable_regions")
	if usable != 1 {
		t.Errorf("usable_regions = %d before the peer joined, want 1", usable)
	}
	regions, _, _ := unstructured.NestedSlice(alone.Object, "spec", "databaseConfiguration", "regions")
	second := regions[1].(map[string]any)["datacenters"].([]any)[0].(map[string]any)
	if second["id"] != "b" || second["priority"] != int64(-1) {
		t.Errorf("the peer's region = %v, want b at priority -1", second)
	}
	joined := foundationDBCluster(stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap), "", true)
	regions, _, _ = unstructured.NestedSlice(joined.Object, "spec", "databaseConfiguration", "regions")
	second = regions[1].(map[string]any)["datacenters"].([]any)[0].(map[string]any)
	if second["priority"] != int64(0) {
		t.Errorf("the peer's region after joining = %v, want priority 0", second)
	}
}

// The peer's joining is read from the management API's datacenter listing and
// recorded once: a later listing without the peer (a region outage) does not
// take it back.
func TestThePeerRegionIsRecordedOnceSeen(t *testing.T) {
	cp := stretchedControlPlane(simplyblockv1alpha2.StretchedControlPlaneRoleBootstrap)
	prober := &stubProber{datacenters: []string{"a"}}
	r := &ControlPlaneReconciler{Client: newClient(t, cp), Scheme: testScheme(t), Prober: prober}
	access := managedAccess{endpoint: "http://cp.example:5000"}

	if err := r.observePeerRegion(context.Background(), cp, access); err != nil {
		t.Fatal(err)
	}
	if r.peerJoined(cp) {
		t.Fatal("the peer was recorded joined while only this site's data center is present")
	}

	prober.datacenters = []string{"a", "b"}
	if err := r.observePeerRegion(context.Background(), cp, access); err != nil {
		t.Fatal(err)
	}
	if !r.peerJoined(cp) || cp.Status.Stretch.PeerJoinedAt == nil {
		t.Fatalf("the peer's region was not recorded: %+v", cp.Status.Stretch)
	}

	prober.datacenters = []string{"a"}
	prober.dcErr = nil
	if err := r.observePeerRegion(context.Background(), cp, access); err != nil {
		t.Fatal(err)
	}
	if !r.peerJoined(cp) {
		t.Fatal("a listing without the peer took the record back")
	}

	plain := localControlPlane()
	if (&ControlPlaneReconciler{Prober: prober}).peerJoined(plain) {
		t.Fatal("an ordinary control plane reports a joined peer")
	}
}
