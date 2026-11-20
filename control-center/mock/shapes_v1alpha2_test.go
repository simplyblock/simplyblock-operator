package main

// The storage kinds the mock serves are storage.simplyblock.io/v1alpha2, shaped
// exactly as the operator's CRDs on main validate them (crd-inventory): no
// field the schema does not have, every enum value one the schema lists, the
// labels the operator writes, and the Ops kinds walking the operator's own
// step graphs. These tests pin that; a generator that invents a field fails
// here before the console can show it.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const v2 = "/apis/storage.simplyblock.io/v1alpha2"

type objList struct {
	Items []map[string]any `json:"items"`
}

func listV2(t *testing.T, ts, ns, resource string) []map[string]any {
	t.Helper()
	var l objList
	url := ts + v2
	if ns != "" {
		url += "/namespaces/" + ns
	}
	resp := getJSON(t, url+"/"+resource, &l)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url+"/"+resource, resp.StatusCode)
	}
	return l.Items
}

func mustMap(t *testing.T, o map[string]any, path string) map[string]any {
	t.Helper()
	v, _ := getPath(o, path).(map[string]any)
	if v == nil {
		t.Fatalf("%s: missing object at %s", getStr(o, "metadata.name"), path)
	}
	return v
}

func decodeInto(resp *http.Response, into any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(into)
}

func in(v string, set ...string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func TestMovedKindsAreNotServedAtV1alpha1(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	for _, r := range []string{"storageclusters", "storagenodes", "storagedevices", "storagepools", "storagenodeops", "storageclusterops", "persistentvolumeops"} {
		resp, err := http.Get(ts.URL + "/apis/storage.simplyblock.io/v1alpha1/" + r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s still served at v1alpha1 (%d); the console reads v1alpha2", r, resp.StatusCode)
		}
	}
}

func TestStorageClusterShape(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	cs := listV2(t, ts.URL, "sb-tenant-a", "storageclusters")
	if len(cs) != 1 {
		t.Fatalf("want one StorageCluster in the tenant namespace, got %d", len(cs))
	}
	c := cs[0]
	if getStr(c, "apiVersion") != "storage.simplyblock.io/v1alpha2" {
		t.Errorf("apiVersion %q", getStr(c, "apiVersion"))
	}
	for _, invented := range []string{"status.extras", "status.conditions", "status.storageNodes", "status.mgmtNodes", "status.created", "status.lastUpdated", "spec.nodePoolAllocationRef"} {
		if getPath(c, invented) != nil {
			t.Errorf("invented field %s present", invented)
		}
	}
	if !in(getStr(c, "status.phase"), "Pending", "Creating", "Provisioning", "Activating", "Online", "Shrinking", "Rebalancing", "Degraded", "Unavailable", "Suspended") {
		t.Errorf("status.phase %q is not in the CRD enum", getStr(c, "status.phase"))
	}
	if getStr(c, "status.uuid") == "" || getStr(c, "status.nqn") == "" || getStr(c, "status.erasureCodingScheme") == "" {
		t.Errorf("status.uuid, status.nqn, status.erasureCodingScheme are what kubectl prints; got %q %q %q", getStr(c, "status.uuid"), getStr(c, "status.nqn"), getStr(c, "status.erasureCodingScheme"))
	}
	if getStr(c, "status.step.state") != "Persisting" {
		t.Errorf("a created cluster rests in step Persisting, got %q", getStr(c, "status.step.state"))
	}
	mustMap(t, c, "spec.stripe")
	mustMap(t, c, "spec.storageNodes")
	mustMap(t, c, "status.workloadNames")
	if _, ok := getPath(c, "status.tasks").([]any); !ok {
		t.Errorf("status.tasks must be the operator's task window (an array), got %T", getPath(c, "status.tasks"))
	}
}

func TestStorageNodeShape(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	cs := listV2(t, ts.URL, "sb-tenant-a", "storageclusters")
	cluster := getStr(cs[0], "metadata.name")
	nodes := listV2(t, ts.URL, "sb-tenant-a", "storagenodes")
	if len(nodes) != 8 {
		t.Fatalf("want 8 nodes, got %d", len(nodes))
	}
	offline := 0
	for _, n := range nodes {
		name := getStr(n, "metadata.name")
		if getStr(n, "spec.clusterRef") != cluster {
			t.Errorf("%s: spec.clusterRef %q, want %q", name, getStr(n, "spec.clusterRef"), cluster)
		}
		if getPath(n, "spec.storageNodeSetRef") != nil || getPath(n, "status.postedAt") != nil {
			t.Errorf("%s: v1alpha1 fields present", name)
		}
		if getStr(n, "spec.workerNode") == "" || getPath(n, "spec.config.sizing.vcpuCount") == nil {
			t.Errorf("%s: spec.workerNode and spec.config.sizing.vcpuCount are required", name)
		}
		if !strings.HasPrefix(name, cluster+"-"+getStr(n, "spec.workerNode")+"-") {
			t.Errorf("%s: a node is named <cluster>-<worker>-<slot>", name)
		}
		labels := mustMap(t, n, "metadata.labels")
		if labels["storage.simplyblock.io/cluster"] != cluster || labels["storage.simplyblock.io/worker"] != getStr(n, "spec.workerNode") {
			t.Errorf("%s: labels %v", name, labels)
		}
		phase := getStr(n, "status.phase")
		if !in(phase, "Pending", "Provisioning", "Online", "Removing", "Removed", "Offline", "Degraded", "Failed") {
			t.Errorf("%s: status.phase %q not in enum", name, phase)
		}
		if phase == "Offline" {
			offline++
			if getStr(n, "status.status") != "offline" {
				t.Errorf("%s: Offline phase with control plane status %q", name, getStr(n, "status.status"))
			}
		}
		if getPath(n, "status.resources.capacity.totalBytes") == nil || getPath(n, "status.resources.devices.total") == nil {
			t.Errorf("%s: status.resources.capacity and status.resources.devices are where the counts live", name)
		}
		if getStr(n, "status.ports.management") == "" {
			t.Errorf("%s: status.ports.management missing", name)
		}
		if _, ok := getPath(n, "status.network").([]any); !ok {
			t.Errorf("%s: status.network[] missing", name)
		}
	}
	if offline != 1 {
		t.Errorf("medium-degraded has one offline node, got %d", offline)
	}
}

func TestStorageDeviceShape(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	nodes := listV2(t, ts.URL, "sb-tenant-a", "storagenodes")
	byName := map[string]map[string]any{}
	for _, n := range nodes {
		byName[getStr(n, "metadata.name")] = n
	}
	devs := listV2(t, ts.URL, "", "storagedevices")
	if len(devs) != 24 {
		t.Fatalf("want 24 devices, got %d", len(devs))
	}
	degraded := 0
	for _, d := range devs {
		name := getStr(d, "metadata.name")
		node := byName[getStr(d, "spec.nodeRef")]
		if node == nil {
			t.Fatalf("%s: spec.nodeRef %q names no StorageNode", name, getStr(d, "spec.nodeRef"))
		}
		if getStr(d, "spec.deviceID") == "" {
			t.Errorf("%s: spec.deviceID missing", name)
		}
		labels := mustMap(t, d, "metadata.labels")
		if labels["storage.simplyblock.io/node"] != getStr(node, "metadata.name") || labels["storage.simplyblock.io/cluster"] != getStr(node, "spec.clusterRef") || labels["storage.simplyblock.io/worker"] != getStr(node, "spec.workerNode") {
			t.Errorf("%s: labels %v", name, labels)
		}
		if getPath(d, "status.capacity.usedBytes") != nil {
			t.Errorf("%s: StorageDevice.status.capacity has only totalBytes", name)
		}
		if !in(getStr(d, "status.phase"), "Online", "Degraded", "Unknown", "Removed", "Failed", "Migrated") {
			t.Errorf("%s: status.phase %q", name, getStr(d, "status.phase"))
		}
		if !in(getStr(d, "status.role"), "Storage", "Journal") {
			t.Errorf("%s: status.role %q", name, getStr(d, "status.role"))
		}
		if !in(getStr(d, "status.health.smart.verdict"), "Ok", "Warning", "Failing", "Unsupported", "Unknown") {
			t.Errorf("%s: status.health.smart.verdict %q", name, getStr(d, "status.health.smart.verdict"))
		}
		if !in(getStr(d, "status.health.performance.verdict"), "Ok", "Slow", "Unknown") {
			t.Errorf("%s: status.health.performance.verdict %q", name, getStr(d, "status.health.performance.verdict"))
		}
		if getStr(d, "status.hardware.serialNumber") == "" || getStr(d, "status.hardware.pciAddress") == "" {
			t.Errorf("%s: status.hardware incomplete", name)
		}
		if getStr(d, "status.phase") == "Degraded" {
			degraded++
		}
	}
	if degraded == 0 {
		t.Errorf("medium-degraded has degraded devices")
	}
}

func TestStoragePoolAndClassShape(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	cs := listV2(t, ts.URL, "sb-tenant-a", "storageclusters")
	cluster, clusterUUID := getStr(cs[0], "metadata.name"), getStr(cs[0], "status.uuid")
	pools := listV2(t, ts.URL, "sb-tenant-a", "storagepools")
	if len(pools) != 2 {
		t.Fatalf("want 2 pools, got %d", len(pools))
	}
	var classes objList
	getJSON(t, ts.URL+"/apis/storage.k8s.io/v1/storageclasses", &classes)
	classByName := map[string]map[string]any{}
	for _, c := range classes.Items {
		classByName[getStr(c, "metadata.name")] = c
	}
	for _, p := range pools {
		name := getStr(p, "metadata.name")
		if getStr(p, "spec.clusterRef") != cluster {
			t.Errorf("%s: spec.clusterRef %q", name, getStr(p, "spec.clusterRef"))
		}
		for _, old := range []string{"spec.clusterName", "spec.capacityLimit", "spec.qos", "spec.storageClassParameters"} {
			if getPath(p, old) != nil {
				t.Errorf("%s: v1alpha1 field %s present", name, old)
			}
		}
		if getStr(p, "spec.limits.capacity") == "" {
			t.Errorf("%s: spec.limits.capacity missing", name)
		}
		if !in(getStr(p, "status.phase"), "Pending", "Ready", "Deleting") {
			t.Errorf("%s: status.phase %q", name, getStr(p, "status.phase"))
		}
		names, _ := getPath(p, "status.storageClassNames").([]any)
		if len(names) == 0 {
			t.Fatalf("%s: status.storageClassNames is how a pool names its classes", name)
		}
		for _, cn := range names {
			c := classByName[cn.(string)]
			if c == nil {
				t.Fatalf("%s: class %v does not exist", name, cn)
			}
			labels := mustMap(t, c, "metadata.labels")
			if labels["storage.simplyblock.io/pool"] != name || labels["storage.simplyblock.io/cluster"] != cluster || labels["storage.simplyblock.io/namespace"] != "sb-tenant-a" || labels["storage.simplyblock.io/managed-by"] != "StorageCluster" {
				t.Errorf("class %v: labels %v", cn, labels)
			}
			params := mustMap(t, c, "parameters")
			if params["cluster_id"] != clusterUUID || params["pool_name"] != name {
				t.Errorf("class %v: parameters %v", cn, params)
			}
		}
	}
}

func TestOpsShapes(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	steps := map[string][]string{
		"storageclusterops": {"Requesting", "Awaiting", "ShuttingDown", "Starting", "CheckingPeers", "ShuttingDownNode", "RefreshingPod", "AwaitingPod", "RestartingNode", "Rebalancing", "Completed"},
		"storagenodeops":    {"Requesting", "Departing", "Awaiting", "Validating", "MigratingDevices", "MigratingVolumes", "Verifying", "Removing", "AwaitingRemoval", "Preparing", "Relocating", "AwaitingNode", "Promoting", "Holding", "ShuttingDown", "Releasing", "AwaitingHost", "Restarting", "Cleanup", "Completed"},
		"storagedeviceops":  {"Requesting", "Awaiting", "Removing", "Failing", "Completed"},
	}
	actions := map[string][]string{
		"storageclusterops": {"Activate", "Expand", "Shutdown", "Start", "Restart", "RollingRestart", "CancelTask"},
		"storagenodeops":    {"Shutdown", "Restart", "Suspend", "Resume", "Remove", "Migrate", "HostMaintenance"},
		"storagedeviceops":  {"Restart", "Fail", "CheckHealth"},
	}
	refs := map[string]string{"storageclusterops": "clusterRef", "storagenodeops": "nodeRef", "storagedeviceops": "deviceRef"}
	running, failed := 0, 0
	for res, enum := range steps {
		for _, o := range listV2(t, ts.URL, "", res) {
			name := getStr(o, "metadata.name")
			if !in(getStr(o, "spec.action"), actions[res]...) {
				t.Errorf("%s %s: spec.action %q not in enum", res, name, getStr(o, "spec.action"))
			}
			if getStr(o, "spec."+refs[res]) == "" {
				t.Errorf("%s %s: spec.%s missing", res, name, refs[res])
			}
			for _, old := range []string{"spec.storageNodeRef", "spec.targetRef", "status.subPhase", "status.nodeRollingRestartStatus", "status.events"} {
				if getPath(o, old) != nil {
					t.Errorf("%s %s: invented field %s", res, name, old)
				}
			}
			phase := getStr(o, "status.phase")
			if !in(phase, "Pending", "Running", "Succeeded", "Failed", "Aborted") {
				t.Errorf("%s %s: status.phase %q", res, name, phase)
			}
			if st := getStr(o, "status.step.state"); st != "" && !in(st, enum...) {
				t.Errorf("%s %s: status.step.state %q not in enum", res, name, st)
			}
			switch phase {
			case "Running":
				running++
				if getStr(o, "status.startedAt") == "" || getStr(o, "status.step.state") == "" {
					t.Errorf("%s %s: a running operation has startedAt and a step", res, name)
				}
			case "Failed":
				failed++
				if getStr(o, "status.completedAt") == "" || getStr(o, "status.message") == "" {
					t.Errorf("%s %s: a failed operation has completedAt and a message", res, name)
				}
			case "Succeeded":
				if getStr(o, "status.step.state") != "Completed" {
					t.Errorf("%s %s: a succeeded operation rests in step Completed", res, name)
				}
			}
		}
	}
	if running == 0 || failed == 0 {
		t.Errorf("medium-degraded seeds running and failed operations, got %d running, %d failed", running, failed)
	}
	// the rolling restart records its walk the way the CRD does
	var rr map[string]any
	for _, o := range listV2(t, ts.URL, "", "storageclusterops") {
		if getStr(o, "spec.action") == "RollingRestart" {
			rr = o
		}
	}
	if rr == nil {
		t.Fatal("medium-degraded seeds a RollingRestart")
	}
	if _, ok := getPath(rr, "status.rollingRestart.nodes").([]any); !ok || getPath(rr, "status.rollingRestart.nodeIndex") == nil {
		t.Errorf("RollingRestart: status.rollingRestart.{nodes,nodeIndex} missing: %v", getPath(rr, "status"))
	}
}

func TestPersistentVolumeOpsIsClusterScopedAndNamesItsCreator(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	resp, err := http.Get(ts.URL + v2 + "/namespaces/sb-tenant-a/persistentvolumeops")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Errorf("PersistentVolumeOps is cluster-scoped; a namespaced path must not list it")
	}
	ops := listV2(t, ts.URL, "", "persistentvolumeops")
	if len(ops) == 0 {
		t.Fatal("medium-degraded seeds volume migrations as PersistentVolumeOps")
	}
	drains := 0
	for _, o := range ops {
		if getStr(o, "spec.persistentVolumeName") == "" || getStr(o, "spec.action") != "Migrate" || getStr(o, "spec.migrate.targetNodeRef.name") == "" {
			t.Errorf("%s: spec %v", getStr(o, "metadata.name"), getPath(o, "spec"))
		}
		if getStr(o, "spec.creatorRef.kind") == "StorageNodeOps" {
			drains++
			labels := mustMap(t, o, "metadata.labels")
			if labels["storage.simplyblock.io/managed-by"] != "StorageNodeOps" || labels["storage.simplyblock.io/drain-node"] == "" {
				t.Errorf("%s: a drain's migration carries the drain labels, got %v", getStr(o, "metadata.name"), labels)
			}
		}
	}
	if drains == 0 {
		t.Errorf("the seeded node removal creates drain migrations with spec.creatorRef")
	}
	var vm objList
	resp = getJSON(t, ts.URL+"/apis/storage.simplyblock.io/v1alpha1/volumemigrations", &vm)
	if resp.StatusCode == 200 && len(vm.Items) > 0 {
		t.Errorf("VolumeMigration is the legacy mover; the mock seeds none")
	}
}

func TestClusterTaskWindow(t *testing.T) {
	_, ts := newTestServer(t, "medium-degraded")
	cs := listV2(t, ts.URL, "sb-tenant-a", "storageclusters")
	tasks, _ := getPath(cs[0], "status.tasks").([]any)
	if len(tasks) == 0 {
		t.Fatal("medium-degraded has running operations, so the task window is not empty")
	}
	for _, x := range tasks {
		task := x.(map[string]any)
		if getStr(task, "id") == "" || !in(getStr(task, "status"), "new", "running", "suspended") {
			t.Errorf("task %v: the window holds unfinished tasks with id and status", task)
		}
		if !in(getStr(task, "type"), "device_restart", "node_restart", "device_migration", "failed_device_migration", "new_device_migration", "node_add", "node_removal", "port_allow", "balancing_on_restart", "balancing_on_dev_rem", "balancing_on_dev_add", "jc_comp_resume", "snapshot_replication", "lvol_sync_del", "lvol_sync_op", "lvol_migration", "lvol_batch_migration", "s3_backup", "s3_backup_restore", "s3_backup_merge", "group_backup_commit", "group_backup_delete", "cluster_expand", "replication_final", "fdb_backup", "sync_promote", "sync_resync", "sync_site_fence") {
			t.Errorf("task %v: type is not a control plane function name", task)
		}
		for _, invented := range []string{"uuid", "taskType", "parent_id", "startedAt"} {
			if _, has := task[invented]; has {
				t.Errorf("task %v: invented field %s", task, invented)
			}
		}
	}
	resp, err := http.Get(ts.URL + "/apis/storage.simplyblock.io/v1alpha1/namespaces/sb-tenant-a/tasks")
	if err != nil {
		t.Fatal(err)
	}
	var tl objList
	_ = decodeInto(resp, &tl)
	if len(tl.Items) > 0 {
		t.Errorf("the Task CRD mirror is not seeded; the window on the cluster is the source")
	}
}

func TestControlPlaneAndDriverShape(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	cps := listV2(t, ts.URL, "", "controlplanes")
	if len(cps) != 1 {
		t.Fatalf("want one ControlPlane, got %d", len(cps))
	}
	cp := cps[0]
	if getStr(cp, "spec.source.local.image") == "" || getPath(cp, "spec.image") != nil {
		t.Errorf("ControlPlane: spec.source.local.image is where the image lives, got spec %v", getPath(cp, "spec"))
	}
	if !in(getStr(cp, "status.phase"), "Installing", "Available", "Degraded", "Unavailable") {
		t.Errorf("ControlPlane: status.phase %q", getStr(cp, "status.phase"))
	}
	comps, _ := getPath(cp, "status.components").([]any)
	if len(comps) == 0 || getStr(cp, "status.endpoint") == "" || getStr(cp, "status.version") == "" {
		t.Errorf("ControlPlane: status.components, endpoint, version missing: %v", getPath(cp, "status"))
	}
	drivers := listV2(t, ts.URL, "", "simplyblockdrivers")
	if len(drivers) != 1 || !in(getStr(drivers[0], "status.phase"), "Installing", "Ready", "Degraded", "Unavailable") {
		t.Errorf("one SimplyblockDriver with a phase in the enum, got %v", drivers)
	}
}

func TestBackupShapes(t *testing.T) {
	_, ts := newTestServer(t, "small-healthy")
	pols := listV2(t, ts.URL, "", "storagebackuppolicies")
	if len(pols) != 1 || getStr(pols[0], "spec.clusterRef") == "" || getStr(pols[0], "spec.schedule") == "" {
		t.Fatalf("backups are scheduled by one StorageBackupPolicy with clusterRef and schedule, got %v", pols)
	}
	claims, _ := getPath(pols[0], "status.attachedClaims").([]any)
	if len(claims) == 0 {
		t.Errorf("StorageBackupPolicy.status.attachedClaims lists the claims it covers")
	}
	for _, b := range listV2(t, ts.URL, "", "storagebackups") {
		name := getStr(b, "metadata.name")
		if getStr(b, "spec.source.claimName") == "" && getStr(b, "spec.clusterRef") == "" {
			t.Errorf("%s: a backup names its source claim or its cluster", name)
		}
		if !in(getStr(b, "status.phase"), "Pending", "Creating", "Available", "Failed") {
			t.Errorf("%s: status.phase %q", name, getStr(b, "status.phase"))
		}
		if getStr(b, "status.backup.backupID") == "" || getStr(b, "status.source.persistentVolumeName") == "" {
			t.Errorf("%s: status.backup and status.source are the record, got %v", name, getPath(b, "status"))
		}
	}
	if resp, err := http.Get(ts.URL + "/apis/storage.simplyblock.io/v1alpha1/backuppolicies"); err == nil {
		var l objList
		_ = decodeInto(resp, &l)
		if len(l.Items) > 0 {
			t.Errorf("BackupPolicy (v1alpha1) is not seeded; StorageBackupPolicy is the kind")
		}
	}
}

func TestCRDsAreServedFromTheOperator(t *testing.T) {
	srv, ts := newTestServer(t, "small-healthy")
	n, err := srv.loadCRDs("../../operator/config/crd/bases")
	if err != nil {
		t.Fatalf("loadCRDs: %v", err)
	}
	if n < 30 {
		t.Fatalf("loaded %d CRDs, want the operator's full set", n)
	}
	var crd map[string]any
	resp := getJSON(t, ts.URL+"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/storageclusters.storage.simplyblock.io", &crd)
	if resp.StatusCode != 200 {
		t.Fatalf("CRD get: %d", resp.StatusCode)
	}
	versions, _ := getPath(crd, "spec.versions").([]any)
	found := false
	for _, v := range versions {
		if getStr(v.(map[string]any), "name") == "v1alpha2" {
			found = true
			if getPath(v.(map[string]any), "schema.openAPIV3Schema.properties.spec.properties.stripe") == nil {
				t.Errorf("the served CRD carries the schema the console reads descriptions from")
			}
		}
	}
	if !found {
		t.Errorf("storageclusters CRD served without v1alpha2: %v", versions)
	}
}
