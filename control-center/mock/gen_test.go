package main

// Generator format tests: the object shapes the reworked console joins on
// (PV and snapshot handles, StorageClass names, Node labels, pod names and
// labels, control plane workloads, CRDs) and the several-clusters-per-world
// layout. They read the store directly, the way the console reads the API.

import (
	"strconv"
	"strings"
	"testing"
)

// genWorld renders a dataset into a fresh store.
func genWorld(t *testing.T, dataset string) *Store {
	t.Helper()
	sc, ok := scenarioByName(dataset)
	if !ok {
		t.Fatalf("unknown dataset %s", dataset)
	}
	st := NewStore(21)
	st.Register(allResourceDefs()...)
	Generate(st, sc, 21, "simplyblock")
	return st
}

func listOf(t *testing.T, st *Store, group, version, resource string) []map[string]any {
	t.Helper()
	d, ok := st.Lookup(group, version, resource)
	if !ok {
		t.Fatalf("%s/%s/%s is not registered", group, version, resource)
	}
	items, _ := st.List(d, "", "", "")
	return items
}

// userClusters returns the user-facing StorageClusters (not the sb-mc-* projections).
func userClusters(t *testing.T, st *Store) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range listOf(t, st, sbGroup, "v1alpha1", "storageclusters") {
		if strings.HasPrefix(getStr(c, "metadata.namespace"), "sb-mc-") {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isUUID(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

func TestPVHandleIsClusterPoolVolumeTriple(t *testing.T) {
	st := genWorld(t, "medium-degraded")
	clusterUUID := getStr(userClusters(t, st)[0], "status.uuid")
	pools := map[string]bool{}
	for _, p := range listOf(t, st, sbGroup, "v1alpha1", "storagepools") {
		pools[getStr(p, "status.uuid")] = true
	}
	pvs := listOf(t, st, "", "v1", "persistentvolumes")
	if len(pvs) == 0 {
		t.Fatal("no PVs generated")
	}
	for _, pv := range pvs {
		handle := getStr(pv, "spec.csi.volumeHandle")
		parts := strings.Split(handle, ":")
		if len(parts) != 3 || !isUUID(parts[0]) || !isUUID(parts[1]) || !isUUID(parts[2]) {
			t.Fatalf("PV %s handle %q is not <cluster>:<pool>:<lvol>", getStr(pv, "metadata.name"), handle)
		}
		if parts[0] != clusterUUID {
			t.Errorf("PV %s handle cluster %s != StorageCluster uuid %s", getStr(pv, "metadata.name"), parts[0], clusterUUID)
		}
		if !pools[parts[1]] {
			t.Errorf("PV %s handle pool %s is no StoragePool uuid", getStr(pv, "metadata.name"), parts[1])
		}
		if getStr(pv, "metadata.name") != "pvc-"+parts[2] {
			t.Errorf("PV name %s does not carry the lvol uuid %s", getStr(pv, "metadata.name"), parts[2])
		}
	}
}

func TestSnapshotHandleIsClusterPoolSnapshotTriple(t *testing.T) {
	st := genWorld(t, "medium-degraded")
	clusterUUID := getStr(userClusters(t, st)[0], "status.uuid")
	contents := listOf(t, st, "snapshot.storage.k8s.io", "v1", "volumesnapshotcontents")
	if len(contents) == 0 {
		t.Fatal("no VolumeSnapshotContents generated")
	}
	for _, c := range contents {
		parts := strings.Split(getStr(c, "status.snapshotHandle"), ":")
		if len(parts) != 3 || parts[0] != clusterUUID || !isUUID(parts[1]) || !isUUID(parts[2]) {
			t.Fatalf("snapshotHandle %q is not <cluster>:<pool>:<snapshot>", getStr(c, "status.snapshotHandle"))
		}
	}
}

func TestStorageClassPerPoolNamingAndLabels(t *testing.T) {
	st := genWorld(t, "medium-degraded")
	cluster := userClusters(t, st)[0]
	ns, name, uuid := getStr(cluster, "metadata.namespace"), getStr(cluster, "metadata.name"), getStr(cluster, "status.uuid")
	scDef, _ := st.Lookup("storage.k8s.io", "v1", "storageclasses")
	pools := listOf(t, st, sbGroup, "v1alpha1", "storagepools")
	if len(pools) == 0 {
		t.Fatal("no pools")
	}
	for _, p := range pools {
		pool := getStr(p, "metadata.name")
		want := "simplyblock-" + ns + "-" + name + "-" + pool
		sc, err := st.Get(scDef, "", want)
		if err != nil {
			t.Fatalf("StorageClass %s missing", want)
		}
		if labelOf(sc, "storage.simplyblock.io/namespace") != ns || labelOf(sc, "storage.simplyblock.io/cluster") != name || labelOf(sc, "storage.simplyblock.io/pool") != pool {
			t.Errorf("StorageClass %s labels: %v", want, getPath(sc, "metadata.labels"))
		}
		if getStr(sc, "parameters.cluster_id") != uuid || getStr(sc, "parameters.pool_name") != pool {
			t.Errorf("StorageClass %s parameters: %v", want, sc["parameters"])
		}
	}
	// every PVC names a per-pool class
	for _, pvc := range listOf(t, st, "", "v1", "persistentvolumeclaims") {
		if _, err := st.Get(scDef, "", getStr(pvc, "spec.storageClassName")); err != nil {
			t.Errorf("PVC %s names missing StorageClass %s", getStr(pvc, "metadata.name"), getStr(pvc, "spec.storageClassName"))
		}
	}
}

func TestWorkerNodeLabels(t *testing.T) {
	st := genWorld(t, "small-healthy")
	clusterUUID := getStr(userClusters(t, st)[0], "status.uuid")
	nodeDef, _ := st.Lookup("", "v1", "nodes")
	for _, sn := range listOf(t, st, sbGroup, "v1alpha1", "storagenodes") {
		worker := getStr(sn, "spec.workerNode")
		node, err := st.Get(nodeDef, "", worker)
		if err != nil {
			t.Fatalf("StorageNode %s names missing worker %s", getStr(sn, "metadata.name"), worker)
		}
		key := "simplyblock.io/storage-node-uuid." + clusterUUID + ".0"
		if got := labelOf(node, key); got != getStr(sn, "status.uuid") {
			t.Errorf("Node %s label %s = %q, want %s", worker, key, got, getStr(sn, "status.uuid"))
		}
		if labelOf(node, "io.simplyblock.node-type") != "simplyblock-storage-plane" {
			t.Errorf("Node %s lacks io.simplyblock.node-type", worker)
		}
		if labelOf(node, "topology.kubernetes.io/zone") == "" {
			t.Errorf("Node %s lacks a zone", worker)
		}
		if labelOf(sn, "storage.simplyblock.io/storagenodeset") != getStr(sn, "spec.storageNodeSetRef") || labelOf(sn, "storage.simplyblock.io/worker") != worker {
			t.Errorf("StorageNode %s lacks the nodeset and worker labels: %v", getStr(sn, "metadata.name"), getPath(sn, "metadata.labels"))
		}
	}
}

func containerNames(pod map[string]any) []string {
	var out []string
	cs, _ := getPath(pod, "spec.containers").([]any)
	for _, c := range cs {
		out = append(out, getStr(c, "name"))
	}
	return out
}

func hasContainer(pod map[string]any, name string) bool {
	for _, c := range containerNames(pod) {
		if c == name {
			return true
		}
	}
	return false
}

func nodeSelectorHostname(pod map[string]any) string {
	sel, _ := getPath(pod, "spec.nodeSelector").(map[string]any)
	s, _ := sel["kubernetes.io/hostname"].(string)
	return s
}

func TestStorageNodeAndSPDKPods(t *testing.T) {
	st := genWorld(t, "small-healthy")
	cluster := userClusters(t, st)[0]
	clusterName, clusterUUID := getStr(cluster, "metadata.name"), getStr(cluster, "status.uuid")
	pods := listOf(t, st, "", "v1", "pods")
	byName := map[string]map[string]any{}
	for _, p := range pods {
		byName[getStr(p, "metadata.name")] = p
	}
	rpcPorts := map[float64]bool{}
	for _, sn := range listOf(t, st, sbGroup, "v1alpha1", "storagenodes") {
		worker := getStr(sn, "spec.workerNode")
		rpc, ok := getPath(sn, "status.ports.rpc").(float64)
		if !ok || rpc == 0 {
			t.Fatalf("StorageNode %s has no status.ports.rpc", getStr(sn, "metadata.name"))
		}
		if rpcPorts[rpc] {
			t.Errorf("rpc port %v is used by two nodes of the same cluster", rpc)
		}
		rpcPorts[rpc] = true

		// node API pod: the StorageNodeSet DaemonSet pod on the worker
		var api map[string]any
		for _, p := range pods {
			if labelOf(p, "app") == "storage-node" && labelOf(p, "simplyblock-cluster") == clusterName && getStr(p, "spec.nodeName") == worker {
				api = p
			}
		}
		if api == nil {
			t.Fatalf("no storage-node pod for cluster %s on %s", clusterName, worker)
		}
		if labelOf(api, "io.simplyblock.storagenodeset") != getStr(sn, "spec.storageNodeSetRef") || !hasContainer(api, "s-node-api-container") {
			t.Errorf("storage-node pod %s: labels %v containers %v", getStr(api, "metadata.name"), getPath(api, "metadata.labels"), containerNames(api))
		}

		// SPDK pod: backend-created, named by rpc port and cluster UUID
		spdkName := "snode-spdk-pod-" + strconv.Itoa(int(rpc)) + "-" + clusterUUID
		spdk, ok := byName[spdkName]
		if !ok {
			t.Fatalf("SPDK pod %s missing", spdkName)
		}
		if labelOf(spdk, "role") != "simplyblock-storage-node" || labelOf(spdk, "app") != "spdk-app-"+strconv.Itoa(int(rpc)) {
			t.Errorf("SPDK pod %s labels %v", spdkName, getPath(spdk, "metadata.labels"))
		}
		if !hasContainer(spdk, "spdk-container") || !hasContainer(spdk, "spdk-proxy-container") {
			t.Errorf("SPDK pod %s containers %v", spdkName, containerNames(spdk))
		}
		if nodeSelectorHostname(spdk) != worker || getStr(spdk, "spec.nodeName") != worker {
			t.Errorf("SPDK pod %s is not pinned to %s: selector %v, nodeName %s", spdkName, worker, getPath(spdk, "spec.nodeSelector"), getStr(spdk, "spec.nodeName"))
		}
	}
}

func TestControlPlaneWorkloads(t *testing.T) {
	st := genWorld(t, "small-healthy")
	deps := map[string]map[string]any{}
	for _, d := range listOf(t, st, "apps", "v1", "deployments") {
		deps[getStr(d, "metadata.name")] = d
	}
	pods := listOf(t, st, "", "v1", "pods")
	for _, name := range []string{"simplyblock-admin-control", "simplyblock-webappapi", "simplyblock-monitoring", "simplyblock-tasks", "simplyblock-fdb-controller-manager"} {
		d, ok := deps[name]
		if !ok {
			t.Errorf("Deployment %s missing", name)
			continue
		}
		if sel, _ := getPath(d, "spec.selector.matchLabels").(map[string]any); sel["app"] != name {
			t.Errorf("Deployment %s selector %v", name, sel)
		}
		found := 0
		for _, p := range pods {
			if labelOf(p, "app") != name {
				continue
			}
			found++
			if ann, _ := getPath(p, "metadata.annotations").(map[string]any); ann["log-collector/enabled"] != "true" {
				t.Errorf("pod %s lacks log-collector/enabled", getStr(p, "metadata.name"))
			}
		}
		if found == 0 {
			t.Errorf("no pod labeled app=%s", name)
		}
	}
	fdb := 0
	for _, p := range pods {
		if labelOf(p, "foundationdb.org/fdb-cluster-name") != "" {
			fdb++
		}
	}
	if fdb == 0 {
		t.Error("no FDB pods labeled foundationdb.org/fdb-cluster-name")
	}
}

func TestCRDsServedFromOperatorBases(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	resp := getJSON(t, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions", &list)
	if resp.StatusCode != 200 {
		t.Fatalf("CRD list: %d", resp.StatusCode)
	}
	var sc map[string]any
	for _, crd := range list.Items {
		if getStr(crd, "metadata.name") == "storageclusters.storage.simplyblock.io" {
			sc = crd
		}
	}
	if sc == nil {
		t.Fatalf("storageclusters CRD not served (%d CRDs)", len(list.Items))
	}
	versions, _ := getPath(sc, "spec.versions").([]any)
	if len(versions) == 0 {
		t.Fatal("CRD has no versions")
	}
	if !containsKey(getPath(versions[0], "schema.openAPIV3Schema"), "x-kubernetes-validations") {
		t.Fatal("storageclusters CRD schema carries no x-kubernetes-validations")
	}
}

// containsKey reports whether key appears anywhere in a JSON-shaped value.
func containsKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[key]; ok {
			return true
		}
		for _, vv := range t {
			if containsKey(vv, key) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if containsKey(vv, key) {
				return true
			}
		}
	}
	return false
}

func TestSeveralClustersPerWorld(t *testing.T) {
	sc := Scenario{
		Name: "three-edge", Workers: 2, MgmtNodes: 1, DevicesPerNode: 2, Pools: 1, Volumes: 4,
		AppNamespaces: []string{"demo"}, Clusters: 3, Site: "edge", SitePerCluster: true, Stage: "prd",
	}
	st := NewStore(3)
	st.Register(allResourceDefs()...)
	Generate(st, sc, 3, "simplyblock")

	clusters := userClusters(t, st)
	if len(clusters) != 3 {
		t.Fatalf("want 3 StorageClusters, got %d", len(clusters))
	}
	uuids, sites := map[string]bool{}, map[string]bool{}
	for _, c := range clusters {
		uuids[getStr(c, "status.uuid")] = true
		sites[labelOf(c, "storage.simplyblock.io/site")] = true
		if labelOf(c, "storage.simplyblock.io/stage") != "prd" {
			t.Errorf("cluster %s stage label %q", getStr(c, "metadata.name"), labelOf(c, "storage.simplyblock.io/stage"))
		}
	}
	if len(uuids) != 3 || len(sites) != 3 || sites[""] {
		t.Fatalf("clusters must have distinct uuids and sites: %v %v", uuids, sites)
	}
	nodes := listOf(t, st, sbGroup, "v1alpha1", "storagenodes")
	if len(nodes) != 6 {
		t.Fatalf("want 6 StorageNodes, got %d", len(nodes))
	}
	zones := map[string]bool{}
	for _, n := range listOf(t, st, "", "v1", "nodes") {
		if labelOf(n, "io.simplyblock.node-type") != "" {
			zones[labelOf(n, "topology.kubernetes.io/zone")] = true
		}
	}
	if len(zones) < 3 {
		t.Errorf("workers of three sites should span three zones, got %v", zones)
	}
	// every device points at a node of the same cluster (node -> nodeset -> cluster)
	setCluster := map[string]string{}
	for _, s := range listOf(t, st, sbGroup, "v1alpha1", "storagenodesets") {
		setCluster[getStr(s, "metadata.name")] = getStr(s, "spec.clusterName")
	}
	clusterUUID := map[string]string{}
	for _, c := range clusters {
		clusterUUID[getStr(c, "metadata.name")] = getStr(c, "status.uuid")
	}
	nodeCluster := map[string]string{}
	for _, n := range nodes {
		nodeCluster[getStr(n, "metadata.name")] = clusterUUID[setCluster[getStr(n, "spec.storageNodeSetRef")]]
	}
	for _, d := range listOf(t, st, sbGroup, "v1alpha1", "storagedevices") {
		if nodeCluster[getStr(d, "spec.nodeRef")] != getStr(d, "status.clusterID") {
			t.Errorf("device %s cluster %s but its node is in %s", getStr(d, "metadata.name"), getStr(d, "status.clusterID"), nodeCluster[getStr(d, "spec.nodeRef")])
		}
	}
}

func TestOneClusterPerNamespace(t *testing.T) {
	sc := Scenario{
		Name: "two-ns", Workers: 2, MgmtNodes: 1, DevicesPerNode: 1, Pools: 1, Volumes: 2,
		AppNamespaces: []string{"demo"}, ClusterNamespaces: []string{"sb-blue", "sb-green"},
	}
	st := NewStore(4)
	st.Register(allResourceDefs()...)
	Generate(st, sc, 4, "simplyblock")
	byNs := map[string]string{}
	for _, c := range userClusters(t, st) {
		byNs[getStr(c, "metadata.namespace")] = labelOf(c, "storage.simplyblock.io/tenant")
	}
	if len(byNs) != 2 || byNs["sb-blue"] != "blue" || byNs["sb-green"] != "green" {
		t.Fatalf("want one cluster per namespace with the tenant label, got %v", byNs)
	}
}
