package main

// Fleet tests: a fleet renders one world per context, each served under its
// own `/k8s/{ctx}/` and `/cp/{ctx}/` prefixes, with the context labels reported
// by /mockctl/info and the generated config.js.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFleetTestServer(t *testing.T, fleet string) (*server, string) {
	t.Helper()
	f, ok := fleetByName(fleet)
	if !ok {
		t.Fatalf("unknown fleet %s", fleet)
	}
	srv := newFleetServer(f, 1234, "simplyblock", "", "global")
	ts := newHTTPTestServer(t, srv.routes(""))
	return srv, ts
}

// clusterUUIDs lists the user-facing StorageCluster UUIDs a context serves.
func clusterUUIDs(t *testing.T, base string) []string {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	resp := getJSON(t, base+"/apis/storage.simplyblock.io/v1alpha1/storageclusters", &list)
	if resp.StatusCode != 200 {
		t.Fatalf("GET storageclusters via %s: %d", base, resp.StatusCode)
	}
	var out []string
	for _, c := range list.Items {
		if strings.HasPrefix(getStr(c, "metadata.namespace"), "sb-mc-") {
			continue
		}
		out = append(out, getStr(c, "status.uuid"))
	}
	return out
}

func cpClusterIDs(t *testing.T, base string) []string {
	t.Helper()
	var list []map[string]any
	resp := getJSON(t, base+"/api/v2/clusters/", &list)
	if resp.StatusCode != 200 {
		t.Fatalf("GET clusters via %s: %d", base, resp.StatusCode)
	}
	var out []string
	for _, c := range list {
		out = append(out, getStr(c, "id"))
	}
	return out
}

func TestFleetNamesResolve(t *testing.T) {
	for _, name := range []string{"fleet-stages", "fleet-tenants", "fleet-namespaces", "fleet-edge-shared", "fleet-edge-sites"} {
		f, ok := fleetByName(name)
		if !ok {
			t.Errorf("fleet %s missing", name)
			continue
		}
		if len(f.Contexts) == 0 {
			t.Errorf("fleet %s has no contexts", name)
		}
	}
	// a plain scenario is a one-context fleet named local
	f, ok := fleetByName("small-healthy")
	if !ok || len(f.Contexts) != 1 || f.Contexts[0].Name != "local" || f.Contexts[0].Scenario.Name != "small-healthy" {
		t.Fatalf("plain scenario should resolve to a local one-context fleet, got %+v", f)
	}
	if _, ok := fleetByName("no-such-thing"); ok {
		t.Fatal("unknown names must not resolve")
	}
	if !strings.Contains(datasetNames(), "fleet-edge-sites") {
		t.Fatal("--dataset help must list the fleets")
	}
}

func TestFleetStagesAreDistinctWorlds(t *testing.T) {
	_, ts := newFleetTestServer(t, "fleet-stages")
	seen := map[string]string{}
	for _, ctx := range []string{"dev", "uat", "prd"} {
		k8s := clusterUUIDs(t, ts+"/k8s/"+ctx)
		cp := cpClusterIDs(t, ts+"/cp/"+ctx)
		if len(k8s) == 0 || len(cp) != len(k8s) {
			t.Fatalf("%s: %d StorageClusters, %d control plane clusters", ctx, len(k8s), len(cp))
		}
		for i := range k8s {
			if k8s[i] != cp[i] {
				t.Errorf("%s: control plane id %s != StorageCluster uuid %s", ctx, cp[i], k8s[i])
			}
			if other, dup := seen[k8s[i]]; dup {
				t.Errorf("cluster %s appears in both %s and %s", k8s[i], other, ctx)
			}
			seen[k8s[i]] = ctx
		}
	}
	// the root routes keep serving the first context
	if root := clusterUUIDs(t, ts+"/k8s"); len(root) == 0 || seen[root[0]] != "dev" {
		t.Fatalf("root /k8s should serve the first context (dev), got %v", root)
	}
	if root := clusterUUIDs(t, ts); len(root) == 0 || seen[root[0]] != "dev" {
		t.Fatalf("root /apis should serve the first context (dev), got %v", root)
	}
	if local := cpClusterIDs(t, ts+"/cp/local"); len(local) == 0 || seen[local[0]] != "dev" {
		t.Fatalf("/cp/local should alias the first context, got %v", local)
	}
	if resp := getJSON(t, ts+"/k8s/nowhere/apis", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown context should 404, got %d", resp.StatusCode)
	}
}

func TestFleetEdgeSharedHasTwelveClusters(t *testing.T) {
	_, ts := newFleetTestServer(t, "fleet-edge-shared")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	getJSON(t, ts+"/k8s/edge/apis/storage.simplyblock.io/v1alpha1/storageclusters", &list)
	sites := map[string]bool{}
	n := 0
	for _, c := range list.Items {
		if strings.HasPrefix(getStr(c, "metadata.namespace"), "sb-mc-") {
			continue
		}
		n++
		sites[labelOf(c, "storage.simplyblock.io/site")] = true
	}
	if n != 12 || len(sites) != 12 || sites[""] {
		t.Fatalf("want 12 clusters with 12 distinct site labels, got %d clusters, sites %v", n, sites)
	}
	if ids := cpClusterIDs(t, ts+"/cp/edge"); len(ids) != 12 {
		t.Fatalf("control plane should list 12 clusters, got %d", len(ids))
	}
}

func TestFleetEdgeSitesOneClusterPerContext(t *testing.T) {
	_, ts := newFleetTestServer(t, "fleet-edge-sites")
	for _, ctx := range []string{"edge-01", "edge-03", "edge-08"} {
		var list struct {
			Items []map[string]any `json:"items"`
		}
		getJSON(t, ts+"/k8s/"+ctx+"/apis/storage.simplyblock.io/v1alpha1/namespaces/simplyblock/storageclusters", &list)
		if len(list.Items) != 1 {
			t.Fatalf("%s: want one StorageCluster in namespace simplyblock, got %d", ctx, len(list.Items))
		}
		ids := cpClusterIDs(t, ts+"/cp/"+ctx)
		if len(ids) != 1 || ids[0] != getStr(list.Items[0], "status.uuid") {
			t.Fatalf("%s: control plane ids %v, StorageCluster uuid %s", ctx, ids, getStr(list.Items[0], "status.uuid"))
		}
	}
}

func TestMockctlInfoReportsContexts(t *testing.T) {
	_, ts := newFleetTestServer(t, "fleet-stages")
	var info struct {
		Dataset  string `json:"dataset"`
		Contexts map[string]struct {
			Labels  map[string]string `json:"labels"`
			Objects map[string]int    `json:"objects"`
		} `json:"contexts"`
	}
	getJSON(t, ts+"/mockctl/info", &info)
	if info.Dataset != "fleet-stages" || len(info.Contexts) != 3 {
		t.Fatalf("info: %+v", info)
	}
	if info.Contexts["prd"].Labels["stage"] != "prd" || info.Contexts["prd"].Objects["storage.simplyblock.io/storageclusters"] == 0 {
		t.Fatalf("prd context: %+v", info.Contexts["prd"])
	}
}

func TestConfigJSCarriesContextsAndFeatures(t *testing.T) {
	f, _ := fleetByName("fleet-edge-sites")
	srv := newFleetServer(f, 7, "simplyblock", "", "global")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644)
	ts := newHTTPTestServer(t, srv.routes(dir))

	resp := getJSON(t, ts+"/config.js", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("config.js: %d", resp.StatusCode)
	}
	body := readBodyString(t, ts+"/config.js")
	for _, want := range []string{`cpBase: "/cp/local"`, `"k8sBase":"/k8s/edge-03"`, `"cpBase":"/cp/edge-03"`, `"site":"edge-03"`, `"zones":true`, `"fdbBackups":true`, `"namespaces":["simplyblock"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("config.js lacks %s:\n%s", want, body)
		}
	}
}
