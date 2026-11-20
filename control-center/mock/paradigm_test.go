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

// The hub tenancy layout: the tenant namespace holds the cluster, a projected
// copy lives in sb-mc-*, and the RBAC vocabulary is present.
func TestTenancyLayout(t *testing.T) {
	st := NewStore(5)
	st.Register(allResourceDefs()...)
	sc, _ := scenarioByName("medium-degraded")
	Generate(st, sc, 5, "simplyblock-system")

	nsDef, _ := st.Lookup("", "v1", "namespaces")
	nss, _ := st.List(nsDef, "", "", "")
	want := map[string]bool{"simplyblock-system": false, "sb-tenant-a": false}
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
	for _, n := range []string{"simplyblock-system", "sb-tenant-a"} {
		if !want[n] {
			t.Errorf("expected namespace %s to exist", n)
		}
	}
	if kinds["sb-tenant-a"] != scopeTenant {
		t.Errorf("sb-tenant-a scope-kind = %q, want %q", kinds["sb-tenant-a"], scopeTenant)
	}

	// StorageCluster exists both user-facing (sb-sc-*) and as a transport projection (sb-mc-*)
	scDef, _ := st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	all, _ := st.List(scDef, "", "", "")
	var userFacing, projected int
	for _, c := range all {
		ns := getStr(c, "metadata.namespace")
		switch {
		case ns == "sb-tenant-a":
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
	body := `{"spec":{"resourceAttributes":{"namespace":"sb-tenant-a","verb":"create","resource":"storageclusters"}}}`
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

// The product-facing role model: one global admin, one full-admin per tenant,
// and one read-only role per main object covering its sub-objects.
func TestRoleModel(t *testing.T) {
	names := map[string]sbRole{}
	for _, r := range sbRoles() {
		names[r.Name] = r
	}
	for _, want := range []string{"sb:infra-admin", "sb:tenant-admin", "sb:cluster-reader", "sb:dr-policy-reader", "sb:dr-application-reader"} {
		if _, ok := names[want]; !ok {
			t.Errorf("role %s missing", want)
		}
	}
	// full admin is the only tenant-scoped writer and spans all three trees
	ta := names["sb:tenant-admin"]
	if !ta.Write {
		t.Error("sb:tenant-admin must be a writer (CRUD)")
	}
	for _, res := range []string{"storageclusters", "replicationpolicies", "protectedapplications"} {
		found := false
		for _, r := range ta.Resources {
			if r == res {
				found = true
			}
		}
		if !found {
			t.Errorf("sb:tenant-admin missing %s", res)
		}
	}
	// readers are read-only and each is scoped to its main object's tree
	for _, name := range []string{"sb:cluster-reader", "sb:dr-policy-reader", "sb:dr-application-reader"} {
		if names[name].Write {
			t.Errorf("%s must be read-only", name)
		}
	}
	// the DR-policy reader must NOT see cluster objects and vice-versa
	az := &AuthZ{viewer: Viewer{Grants: []ViewerGrant{{Role: "sb:dr-policy-reader", Namespace: "sb-tenant-a"}}}}
	if az.allow("sb-tenant-a", "get", "storageclusters") {
		t.Error("dr-policy-reader must not read storageclusters")
	}
	if !az.allow("sb-tenant-a", "get", "replicationpolicies") {
		t.Error("dr-policy-reader must read replicationpolicies")
	}
	if az.allow("sb-tenant-a", "create", "replicationpolicies") {
		t.Error("dr-policy-reader must not create (read-only)")
	}

	// per-tenant grants generated
	st := NewStore(11)
	st.Register(allResourceDefs()...)
	scn, _ := scenarioByName("large-scale") // two tenants
	Generate(st, scn, 11, "simplyblock-system")
	rbDef, _ := st.Lookup(rbacGroup, "v1", "rolebindings")
	byNs := map[string]int{}
	for _, rb := range mustList(st, rbDef) {
		byNs[getStr(rb, "metadata.namespace")]++
	}
	if byNs["sb-tenant-a"] < 4 { // admin + 3 readers
		t.Errorf("primary tenant should have >=4 rolebindings, got %d", byNs["sb-tenant-a"])
	}
	if byNs["sb-tenant-b"] < 1 { // empty tenant still gets an admin
		t.Errorf("empty tenant should have an admin binding, got %d", byNs["sb-tenant-b"])
	}
}

// A scoped viewer denies outside its grant.
func TestAuthZScopedDeny(t *testing.T) {
	sc, _ := scenarioByName("small-healthy")
	srv := newServer(sc, 1, "simplyblock-system", "", "sb:tenant-admin@sb-tenant-a")
	if srv.authz.allow("sb-tenant-a", "create", "storageclusters") != true {
		t.Fatal("tenant-admin should create storageclusters in its own namespace")
	}
	if srv.authz.allow("sb-sc-other", "create", "storageclusters") != false {
		t.Fatal("tenant-admin must NOT act in another storage cluster's namespace")
	}
	if srv.authz.allow("sb-tenant-a", "get", "secrets") != false {
		t.Fatal("tenant-admin role does not include secrets")
	}
}
