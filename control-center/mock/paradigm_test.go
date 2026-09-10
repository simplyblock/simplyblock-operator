package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// labelOf reads a label whose key contains dots (getPath splits on '.').
func labelOf(obj map[string]any, key string) string {
	labels, _ := getPath(obj, "metadata.labels").(map[string]any)
	if labels == nil {
		return ""
	}
	s, _ := labels[key].(string)
	return s
}

// The hub tenancy layout: storage objects in sb-sc-*, a projected copy in
// sb-mc-*, and the RBAC vocabulary present.
func TestTenancyLayout(t *testing.T) {
	st := NewStore(5)
	st.Register(allResourceDefs()...)
	sc, _ := scenarioByName("medium-degraded")
	Generate(st, sc, 5, "simplyblock-system")

	nsDef, _ := st.Lookup("", "v1", "namespaces")
	nss, _ := st.List(nsDef, "", "", "")
	want := map[string]bool{"simplyblock-system": false, "sb-sc-cluster1": false, "sb-mc-prod-fra1-1": false, "sb-dr-system": false}
	kinds := map[string]string{}
	for _, ns := range nss {
		name := getStr(ns, "metadata.name")
		if _, ok := want[name]; ok {
			want[name] = true
		}
		if k := labelOf(ns, labScopeKind); k != "" {
			kinds[name] = k
		}
	}
	// sb-mc name depends on the seeded site; assert the sc/dr/system ones which are fixed
	for _, n := range []string{"simplyblock-system", "sb-sc-cluster1", "sb-dr-system"} {
		if !want[n] {
			t.Errorf("expected namespace %s to exist", n)
		}
	}
	if kinds["sb-sc-cluster1"] != scopeStorageCluster {
		t.Errorf("sb-sc-cluster1 scope-kind = %q, want %q", kinds["sb-sc-cluster1"], scopeStorageCluster)
	}

	// StorageCluster exists both user-facing (sb-sc-*) and as a transport projection (sb-mc-*)
	scDef, _ := st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	all, _ := st.List(scDef, "", "", "")
	var userFacing, projected int
	for _, c := range all {
		ns := getStr(c, "metadata.namespace")
		switch {
		case strings.HasPrefix(ns, "sb-sc-"):
			userFacing++
		case strings.HasPrefix(ns, "sb-mc-"):
			projected++
			if labelOf(c, labManagedBy) != "hub" {
				t.Errorf("projected StorageCluster missing managed-by=hub")
			}
			if getStr(c, "status.lastSyncTime") == "" {
				t.Errorf("projected StorageCluster missing agent-authored status.lastSyncTime")
			}
		}
	}
	if userFacing != 1 || projected != 1 {
		t.Fatalf("want 1 user-facing + 1 projected StorageCluster, got %d/%d", userFacing, projected)
	}

	// RBAC vocabulary
	crDef, _ := st.Lookup(rbacGroup, "v1", "clusterroles")
	crs, _ := st.List(crDef, "", "", "")
	haveInfra := false
	for _, c := range crs {
		if getStr(c, "metadata.name") == "sb:infra-admin" {
			haveInfra = true
		}
	}
	if !haveInfra {
		t.Error("sb:infra-admin ClusterRole not generated")
	}
}

// Every simplyblock entity carries observedGeneration (the drift primitive).
func TestObservedGenerationStamped(t *testing.T) {
	st := NewStore(3)
	st.Register(allResourceDefs()...)
	sc, _ := scenarioByName("small-healthy")
	Generate(st, sc, 3, "simplyblock-system")

	for _, res := range []string{"storageclusters", "storagenodes", "storagedevices", "storagepools", "managedclusters"} {
		d, _ := st.Lookup(sbGroup, "v1alpha1", res)
		items, _ := st.List(d, "", "", "")
		if len(items) == 0 {
			t.Errorf("%s: none generated", res)
			continue
		}
		for _, o := range items {
			if _, ok := getPath(o, "status.observedGeneration").(float64); !ok {
				t.Errorf("%s/%s missing status.observedGeneration", res, getStr(o, "metadata.name"))
				break
			}
		}
	}
}

// The simulator heals drift: observedGeneration catches up to a bumped spec
// generation, flipping Applied from Pending to Applied.
func TestDriftHeals(t *testing.T) {
	st := NewStore(9)
	st.Register(allResourceDefs()...)
	sc, _ := scenarioByName("medium-degraded") // driftPresent() true (offline node)
	Generate(st, sc, 9, "simplyblock-system")
	sim := NewSimulator(st, "simplyblock-system", 0, 9)

	scDef, _ := st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	projNs := ""
	all, _ := st.List(scDef, "", "", "")
	for _, c := range all {
		if strings.HasPrefix(getStr(c, "metadata.namespace"), "sb-mc-") {
			projNs = getStr(c, "metadata.namespace")
		}
	}
	if projNs == "" {
		t.Fatal("no projected StorageCluster")
	}
	proj, _ := st.Get(scDef, projNs, "cluster1")
	if obsGenOf(proj) >= genOf(proj) {
		t.Skip("no drift seeded in this world; nothing to heal")
	}
	tickUntil(t, sim, 10, func() bool {
		p, _ := st.Get(scDef, projNs, "cluster1")
		return obsGenOf(p) >= genOf(p)
	})
}

// The authorization stand-in: global viewer allows; a scoped viewer denies
// outside its grant; scopes projection lists the visible tree.
func TestAuthZViewer(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")

	// global viewer: SAR allowed anywhere
	var sar struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	body := `{"spec":{"resourceAttributes":{"namespace":"sb-sc-cluster1","verb":"create","resource":"storageclusters"}}}`
	resp, err := http.Post(ts.URL+"/apis/authorization.k8s.io/v1/subjectaccessreviews", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&sar)
	resp.Body.Close()
	if !sar.Status.Allowed {
		t.Fatal("global viewer should be allowed to create storageclusters")
	}

	// scopes projection returns the tree
	var scopes struct {
		Data []map[string]any `json:"data"`
	}
	getJSON(t, ts.URL+"/operator/v1/access/scopes", &scopes)
	if len(scopes.Data) == 0 {
		t.Fatal("scopes projection returned nothing for global viewer")
	}
}

// A scoped viewer denies outside its grant.
func TestAuthZScopedDeny(t *testing.T) {
	sc, _ := scenarioByName("small-healthy")
	srv := newServer(sc, 1, "simplyblock-system", "", "sb:cluster-admin@sb-sc-cluster1")
	if srv.authz.allow("sb-sc-cluster1", "create", "storageclusters") != true {
		t.Fatal("cluster-admin should create storageclusters in its own namespace")
	}
	if srv.authz.allow("sb-sc-other", "create", "storageclusters") != false {
		t.Fatal("cluster-admin must NOT act in another storage cluster's namespace")
	}
	if srv.authz.allow("sb-sc-cluster1", "get", "secrets") != false {
		t.Fatal("cluster-admin role does not include secrets")
	}
}
