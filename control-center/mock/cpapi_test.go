package main

// Control plane v2 API tests: the read routes under /api/v2 are projected
// from the store and shaped like the shared/openapi.json DTOs, and ?watch=true
// streams Server-Sent Events.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newHTTPTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

func readBodyString(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// cpWorld starts a server on a dataset and returns the v2 base and the
// user-facing cluster of the world.
func cpWorld(t *testing.T, dataset string) (*server, string, map[string]any) {
	t.Helper()
	srv, ts := newTestServer(t, dataset)
	clusters := userClusters(t, srv.local.store)
	if len(clusters) == 0 {
		t.Fatal("no cluster")
	}
	return srv, ts.URL + "/cp/local/api/v2", clusters[0]
}

func TestCPClustersMatchStorageClusterUUIDs(t *testing.T) {
	srv, base, _ := cpWorld(t, "medium-degraded")
	var list []map[string]any
	getJSON(t, base+"/clusters/", &list)
	want := map[string]bool{}
	for _, c := range userClusters(t, srv.local.store) {
		want[getStr(c, "status.uuid")] = true
	}
	if len(list) != len(want) {
		t.Fatalf("want %d clusters (one per user-facing StorageCluster, no projections), got %d", len(want), len(list))
	}
	for _, c := range list {
		if !want[getStr(c, "id")] {
			t.Errorf("cluster id %s is no StorageCluster status.uuid", getStr(c, "id"))
		}
		for _, k := range []string{"name", "nqn", "status", "distr_ndcs", "distr_npcs", "utilization_warning", "utilization_critical", "capacity"} {
			if _, ok := c[k]; !ok {
				t.Errorf("ClusterDTO lacks %s: %v", k, c)
			}
		}
		if _, ok := getPath(c, "capacity.size_total").(float64); !ok {
			t.Errorf("capacity is no CapacityStatDTO: %v", c["capacity"])
		}
		var one map[string]any
		if resp := getJSON(t, base+"/clusters/"+getStr(c, "id")+"/", &one); resp.StatusCode != 200 || getStr(one, "id") != getStr(c, "id") {
			t.Errorf("cluster detail %s: %d %v", getStr(c, "id"), resp.StatusCode, one)
		}
	}
	if resp := getJSON(t, base+"/clusters/00000000-0000-4000-8000-000000000000/", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown cluster should 404, got %d", resp.StatusCode)
	}
}

func TestCPNodesDevicesAndHealthInfo(t *testing.T) {
	srv, base, cluster := cpWorld(t, "medium-degraded")
	cid := getStr(cluster, "status.uuid")
	sc, _ := scenarioByName("medium-degraded")

	var nodes []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-nodes/", &nodes)
	if len(nodes) != sc.Workers {
		t.Fatalf("want %d nodes, got %d", sc.Workers, len(nodes))
	}
	storeNodes := map[string]map[string]any{}
	for _, n := range listOf(t, srv.local.store, sbGroup, "v1alpha1", "storagenodes") {
		storeNodes[getStr(n, "status.uuid")] = n
	}
	for _, n := range nodes {
		sn, ok := storeNodes[getStr(n, "id")]
		if !ok {
			t.Fatalf("node id %s is no StorageNode uuid", getStr(n, "id"))
		}
		if getStr(n, "cluster_id") != cid || getStr(n, "hostname") != getStr(sn, "status.hostname") || getStr(n, "status") != getStr(sn, "status.status") {
			t.Errorf("StorageNodeDTO %v does not match %v", n, sn["status"])
		}
		if rpc, _ := n["rpc_port"].(float64); rpc != getPath(sn, "status.ports.rpc").(float64) {
			t.Errorf("rpc_port %v != status.ports.rpc %v", n["rpc_port"], getPath(sn, "status.ports.rpc"))
		}
		var devs []map[string]any
		getJSON(t, base+"/clusters/"+cid+"/storage-nodes/"+getStr(n, "id")+"/devices/", &devs)
		if len(devs) != sc.DevicesPerNode {
			t.Fatalf("node %s: want %d devices, got %d", getStr(n, "id"), sc.DevicesPerNode, len(devs))
		}
		if dc, _ := n["device_count"].(float64); int(dc) != len(devs) {
			t.Errorf("device_count %v != %d", n["device_count"], len(devs))
		}
		d := devs[0]
		for _, k := range []string{"id", "cluster_id", "storage_node_id", "model", "serial_number", "pcie_address", "status", "size", "capacity"} {
			if _, ok := d[k]; !ok {
				t.Errorf("DeviceDTO lacks %s", k)
			}
		}
		devURL := base + "/clusters/" + cid + "/storage-nodes/" + getStr(n, "id") + "/devices/" + getStr(d, "id")
		var one map[string]any
		if resp := getJSON(t, devURL+"/", &one); resp.StatusCode != 200 || getStr(one, "id") != getStr(d, "id") {
			t.Errorf("device detail: %d", resp.StatusCode)
		}
		var h1, h2 map[string]any
		getJSON(t, devURL+"/health-info", &h1)
		getJSON(t, devURL+"/health-info", &h2)
		if getStr(h1, "id") != getStr(d, "id") || getStr(h1, "serial_number") != getStr(d, "serial_number") {
			t.Errorf("health-info identity: %v", h1)
		}
		for _, k := range []string{"temperature_celsius", "percentage_used", "media_errors", "power_on_hours", "critical_warning"} {
			if h1[k] != h2[k] {
				t.Errorf("health-info %s is not deterministic: %v vs %v", k, h1[k], h2[k])
			}
			if _, ok := h1[k].(float64); !ok {
				t.Errorf("health-info lacks %s", k)
			}
		}
	}
}

func TestCPVolumesJoinPVsByHandle(t *testing.T) {
	srv, base, cluster := cpWorld(t, "medium-degraded")
	cid := getStr(cluster, "status.uuid")

	byHandle := map[string]map[string]any{}
	for _, pv := range listOf(t, srv.local.store, "", "v1", "persistentvolumes") {
		byHandle[getStr(pv, "spec.csi.volumeHandle")] = pv
	}
	var pools []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-pools/", &pools)
	if len(pools) == 0 {
		t.Fatal("no pools")
	}
	total := 0
	for _, p := range pools {
		pid := getStr(p, "id")
		if getStr(p, "cluster_id") != cid || getStr(p, "name") == "" || getStr(p, "status") == "" {
			t.Errorf("StoragePoolDTO %v", p)
		}
		var vols []map[string]any
		getJSON(t, base+"/clusters/"+cid+"/storage-pools/"+pid+"/volumes/", &vols)
		total += len(vols)
		for _, v := range vols {
			handle := cid + ":" + pid + ":" + getStr(v, "id")
			pv, ok := byHandle[handle]
			if !ok {
				t.Fatalf("volume %s has no PV with handle %s", getStr(v, "id"), handle)
			}
			wantPVC := getStr(pv, "spec.claimRef.namespace") + "/" + getStr(pv, "spec.claimRef.name")
			if getStr(v, "pvc_name") != wantPVC || getStr(v, "pool_uuid") != pid || getStr(v, "pool_name") != getStr(p, "name") || getStr(v, "cluster_id") != cid {
				t.Errorf("VolumeDTO %v does not join PV %s", v, getStr(pv, "metadata.name"))
			}
			if !strings.Contains(getStr(v, "nqn"), getStr(v, "id")) || getStr(v, "storage_node_id") == "" {
				t.Errorf("VolumeDTO nqn/node: %v", v)
			}
			var one map[string]any
			if resp := getJSON(t, base+"/clusters/"+cid+"/storage-pools/"+pid+"/volumes/"+getStr(v, "id")+"/", &one); resp.StatusCode != 200 || getStr(one, "id") != getStr(v, "id") {
				t.Errorf("volume detail: %d", resp.StatusCode)
			}
		}
		var snaps []map[string]any
		getJSON(t, base+"/clusters/"+cid+"/storage-pools/"+pid+"/snapshots/", &snaps)
		for _, s := range snaps {
			if getStr(s, "id") == "" || getStr(s, "lvol") == "" || getStr(s, "created_at") == "" {
				t.Errorf("SnapshotDTO %v", s)
			}
		}
	}
	if total != len(byHandle) {
		t.Fatalf("pools serve %d volumes, the world has %d PVs", total, len(byHandle))
	}
}

func TestCPTasksHaveMasterAndSubShapes(t *testing.T) {
	_, base, cluster := cpWorld(t, "medium-degraded")
	cid := getStr(cluster, "status.uuid")
	var tasks []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/tasks/", &tasks)
	if len(tasks) == 0 {
		t.Fatal("no tasks")
	}
	masters := map[string]string{}
	for _, tk := range tasks {
		for _, k := range []string{"id", "cluster_id", "device_id", "storage_node_id", "status", "canceled", "function_name", "function_params", "function_result", "max_retry", "retry"} {
			if _, ok := tk[k]; !ok {
				t.Errorf("TaskDTO lacks %s: %v", k, tk)
			}
		}
		switch getStr(tk, "status") {
		case "new", "running", "suspended", "done":
		default:
			t.Errorf("task %s status %q", getStr(tk, "id"), getStr(tk, "status"))
		}
		if getStr(tk, "cluster_id") != cid {
			t.Errorf("task %s cluster %s", getStr(tk, "id"), getStr(tk, "cluster_id"))
		}
		if getStr(tk, "function_params.parent_task_id") == "" {
			masters[getStr(tk, "id")] = getStr(tk, "function_name")
		}
	}
	subs := 0
	for _, tk := range tasks {
		parent := getStr(tk, "function_params.parent_task_id")
		if parent == "" {
			continue
		}
		subs++
		if _, ok := masters[parent]; !ok {
			t.Errorf("sub-task %s names unknown master %s", getStr(tk, "id"), parent)
		}
	}
	kinds := map[string]bool{}
	for _, fn := range masters {
		kinds[fn] = true
	}
	if subs == 0 || !kinds["node_restart"] || !kinds["device_restart"] || !kinds["node_add"] {
		t.Fatalf("want node_restart, device_restart, and node_add masters with sub-tasks; got masters %v, %d subs", masters, subs)
	}
	var one map[string]any
	if resp := getJSON(t, base+"/clusters/"+cid+"/tasks/"+getStr(tasks[0], "id")+"/", &one); resp.StatusCode != 200 || getStr(one, "id") != getStr(tasks[0], "id") {
		t.Fatalf("task detail: %d %v", resp.StatusCode, one)
	}
}

func TestCPLogsCarryStatusChanges(t *testing.T) {
	_, base, cluster := cpWorld(t, "medium-degraded")
	cid := getStr(cluster, "status.uuid")
	var recs []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/logs?limit=500", &recs)
	if len(recs) == 0 {
		t.Fatal("empty event log")
	}
	dateRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$`)
	events := map[string]bool{}
	nodeChange, devChange, clusterChange := false, false, false
	for _, r := range recs {
		for _, k := range []string{"Date", "NodeId", "Event", "Level", "Message", "Storage_ID", "VUID", "Status"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("log record lacks %s: %v", k, r)
			}
		}
		if !dateRe.MatchString(getStr(r, "Date")) {
			t.Errorf("Date %q is not YYYY-MM-DD HH:MM:SS.mmm", getStr(r, "Date"))
		}
		events[getStr(r, "Event")] = true
		msg := getStr(r, "Message")
		if getStr(r, "Event") == "STATUS_CHANGE" {
			nodeChange = nodeChange || strings.HasPrefix(msg, "Storage node status changed from: ")
			devChange = devChange || strings.HasPrefix(msg, "Device status changed from: ")
			clusterChange = clusterChange || strings.HasPrefix(msg, "Cluster status changed from: ")
		}
	}
	if !nodeChange || !devChange || !clusterChange {
		t.Errorf("STATUS_CHANGE coverage: node %v device %v cluster %v", nodeChange, devChange, clusterChange)
	}
	for _, want := range []string{"STATUS_CHANGE", "device_status", "node_status", "CAPACITY"} {
		if !events[want] {
			t.Errorf("event %s missing from the log (%v)", want, events)
		}
	}
	var few []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/logs?limit=3", &few)
	if len(few) != 3 {
		t.Fatalf("limit=3 returned %d records", len(few))
	}
}

func TestCPCapacityAndIostats(t *testing.T) {
	_, base, cluster := cpWorld(t, "small-healthy")
	cid := getStr(cluster, "status.uuid")
	var nodes []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-nodes/", &nodes)
	var devs []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-nodes/"+getStr(nodes[0], "id")+"/devices/", &devs)
	var pools []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-pools/", &pools)
	var vols []map[string]any
	getJSON(t, base+"/clusters/"+cid+"/storage-pools/"+getStr(pools[0], "id")+"/volumes/", &vols)

	c := "/clusters/" + cid
	n := c + "/storage-nodes/" + getStr(nodes[0], "id")
	p := c + "/storage-pools/" + getStr(pools[0], "id")
	for _, path := range []string{
		c + "/capacity", c + "/iostats",
		n + "/capacity", n + "/iostats",
		n + "/devices/" + getStr(devs[0], "id") + "/capacity", n + "/devices/" + getStr(devs[0], "id") + "/iostats",
		p + "/iostats",
		p + "/volumes/" + getStr(vols[0], "id") + "/capacity", p + "/volumes/" + getStr(vols[0], "id") + "/iostats",
	} {
		var latest []map[string]any
		if resp := getJSON(t, base+path, &latest); resp.StatusCode != 200 || len(latest) != 1 {
			t.Errorf("%s: status %d, %d records (want one latest record)", path, resp.StatusCode, len(latest))
			continue
		}
		var hist []map[string]any
		getJSON(t, base+path+"?history=1h", &hist)
		if len(hist) < 10 {
			t.Errorf("%s?history=1h: %d records", path, len(hist))
		}
		rec := latest[0]
		if _, ok := rec["date"].(float64); !ok {
			t.Errorf("%s: record lacks date: %v", path, rec)
		}
		if strings.HasSuffix(path, "capacity") {
			for _, k := range []string{"size_total", "size_used", "size_free", "size_util", "size_prov"} {
				if _, ok := rec[k].(float64); !ok {
					t.Errorf("%s: capacity record lacks %s", path, k)
				}
			}
		} else {
			for _, k := range []string{"read_bytes_ps", "write_bytes_ps", "read_io_ps", "write_io_ps", "read_latency_ps", "write_latency_ps"} {
				if _, ok := rec[k].(float64); !ok {
					t.Errorf("%s: iostats record lacks %s", path, k)
				}
			}
		}
	}
}

func TestCPMetaAndMethods(t *testing.T) {
	_, base, cluster := cpWorld(t, "small-healthy")
	for _, p := range []string{"/_meta/health", "/_meta/ready"} {
		if resp := getJSON(t, base+p, nil); resp.StatusCode != 204 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	resp, err := http.Post(base+"/clusters/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST should be refused with 405, got %d", resp.StatusCode)
	}
	var detail map[string]any
	if r := getJSON(t, base+"/clusters/"+getStr(cluster, "status.uuid")+"/nope", &detail); r.StatusCode != 404 || detail["detail"] == nil {
		t.Errorf("unknown route should be a FastAPI-shaped 404, got %d %v", r.StatusCode, detail)
	}
	// a bearer is accepted and never validated
	req, _ := http.NewRequest(http.MethodGet, base+"/clusters/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Errorf("bearer request: %d", r2.StatusCode)
	}
}

// sseEvent is one parsed Server-Sent Event.
type sseEvent struct {
	Event string
	Data  string
}

// readSSE parses events off the stream, as the console's EventSource does.
func readSSE(body io.Reader, out chan<- sseEvent, comments chan<- string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if cur.Data != "" || cur.Event != "" {
				out <- cur
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
			comments <- line
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			cur.Data += strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "retry:"):
			comments <- line
		}
	}
}

func TestCPWatchStreamsSnapshotThenCreated(t *testing.T) {
	srv, base, cluster := cpWorld(t, "small-healthy")
	cid := getStr(cluster, "status.uuid")

	resp, err := http.Get(base + "/clusters/" + cid + "/tasks/?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	events := make(chan sseEvent, 16)
	comments := make(chan string, 16)
	go readSSE(resp.Body, events, comments)

	next := func() sseEvent {
		select {
		case ev := <-events:
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("no SSE event within 3s")
			return sseEvent{}
		}
	}
	// the stream opens with the retry hint and a snapshot of the list
	select {
	case c := <-comments:
		if !strings.HasPrefix(c, "retry: 3000") {
			t.Fatalf("first line should be retry: 3000, got %q", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no retry line")
	}
	snap := next()
	if snap.Event != "snapshot" {
		t.Fatalf("first event %q, want snapshot", snap.Event)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(snap.Data), &list); err != nil || len(list) == 0 {
		t.Fatalf("snapshot data is no task array: %v %s", err, snap.Data)
	}

	// a task written to the store arrives as created, shaped like the DTO
	srv.local.store.AddTask(cid, map[string]any{
		"id": "11111111-1111-4111-8111-111111111111", "cluster_id": cid,
		"function_name": "device_restart", "status": "new", "function_params": map[string]any{},
	})
	ev := next()
	if ev.Event != "created" {
		t.Fatalf("want created, got %q", ev.Event)
	}
	var task map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &task); err != nil || getStr(task, "id") != "11111111-1111-4111-8111-111111111111" || getStr(task, "status") != "new" {
		t.Fatalf("created payload: %v %s", err, ev.Data)
	}

	// a detail watch snapshots the object itself
	resp2, err := http.Get(base + "/clusters/" + cid + "/?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	ev2 := make(chan sseEvent, 4)
	go readSSE(resp2.Body, ev2, make(chan string, 16))
	select {
	case s := <-ev2:
		var obj map[string]any
		if s.Event != "snapshot" || json.Unmarshal([]byte(s.Data), &obj) != nil || getStr(obj, "id") != cid {
			t.Fatalf("detail snapshot: %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no detail snapshot")
	}
}
