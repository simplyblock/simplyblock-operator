package main

// cpapi.go serves the control plane v2 read API (/api/v2, the surface
// shared/openapi.json describes) projected from the store: clusters, storage
// nodes, devices, pools, volumes, and snapshots are the CRDs and core objects
// reshaped into the snake_case DTOs. Tasks and the event log are the
// control plane's own state. Capacity, iostats, and device health-info are
// seeded series. ?watch=true on list and detail routes streams Server-Sent
// Events (snapshot, then created/updated/deleted) off the store's watch.
// GET only: the console never writes through this surface.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CPAPI struct {
	store *Store
	seed  uint64

	bearerOnce sync.Once
}

func NewCPAPI(st *Store, seed uint64) *CPAPI {
	return &CPAPI{store: st, seed: seed}
}

// Timing of a watch stream.
const (
	ssePing    = 15 * time.Second
	sseMaxLife = time.Hour
	sseRetry   = 3000 // ms, the reconnect hint
)

// ---- routing -------------------------------------------------------------------

func (c *CPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		c.fail(w, 405, "Method Not Allowed")
		return
	}
	if r.Header.Get("Authorization") == "" {
		c.bearerOnce.Do(func() { log.Printf("cp: a request without a bearer token; the mock accepts it (%s)", r.URL.Path) })
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2")
	if !ok {
		c.fail(w, 404, "Not Found")
		return
	}
	p := splitPath(rest)
	switch {
	case len(p) == 2 && p[0] == "_meta" && (p[1] == "health" || p[1] == "ready"):
		w.WriteHeader(204)
	case len(p) >= 1 && p[0] == "management-nodes":
		c.serveManagementNodes(w, r, p[1:])
	case len(p) >= 1 && p[0] == "clusters":
		c.serveClusters(w, r, p[1:])
	default:
		c.fail(w, 404, "Not Found")
	}
}

// fail answers the way FastAPI does: {"detail": ...}.
func (c *CPAPI) fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"detail": msg})
}

func (c *CPAPI) serveManagementNodes(w http.ResponseWriter, r *http.Request, p []string) {
	d, _ := c.store.Lookup("", "v1", "nodes")
	nodes, _ := c.store.List(d, "", "node-role.kubernetes.io/control-plane", "")
	var list []map[string]any
	for _, n := range nodes {
		list = append(list, map[string]any{
			"id": getStr(n, "metadata.uid"), "status": "online",
			"hostname": getStr(n, "metadata.name"), "ip": nodeAddress(n, "InternalIP"),
		})
	}
	if list == nil {
		list = []map[string]any{}
	}
	switch len(p) {
	case 0:
		writeJSON(w, 200, list)
	case 1:
		for _, m := range list {
			if m["id"] == p[0] {
				writeJSON(w, 200, m)
				return
			}
		}
		c.fail(w, 404, "management node not found")
	default:
		c.fail(w, 404, "Not Found")
	}
}

func nodeAddress(n map[string]any, typ string) string {
	addrs, _ := getPath(n, "status.addresses").([]any)
	for _, a := range addrs {
		if getStr(a, "type") == typ {
			return getStr(a, "address")
		}
	}
	return ""
}

// serveClusters handles everything under /clusters.
func (c *CPAPI) serveClusters(w http.ResponseWriter, r *http.Request, p []string) {
	scDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storageclusters")
	if len(p) == 0 {
		c.list(w, r, scDef, "", notProjection, c.clusterDTO)
		return
	}
	cluster := c.cluster(p[0])
	if cluster == nil {
		c.fail(w, 404, "cluster not found")
		return
	}
	cid := getStr(cluster, "status.uuid")
	if len(p) == 1 {
		c.detail(w, r, scDef, "", func(o map[string]any) bool { return notProjection(o) && getStr(o, "status.uuid") == cid }, c.clusterDTO)
		return
	}
	switch p[1] {
	case "storage-nodes":
		c.serveNodes(w, r, cluster, p[2:])
		return
	case "storage-pools":
		c.servePools(w, r, cluster, p[2:])
		return
	case "tasks":
		c.serveTasks(w, r, cid, p[2:])
		return
	case "logs":
		if len(p) != 2 {
			break
		}
		limit := 50
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = n
		}
		writeJSON(w, 200, c.store.Logs(cid, limit))
		return
	case "capacity":
		if len(p) == 2 {
			c.serveSeries(w, r, "capacity", "cluster|"+cid, c.clusterCapacity(cluster))
			return
		}
	case "iostats":
		if len(p) == 2 {
			c.serveSeries(w, r, "iostats", "cluster|"+cid, nil)
			return
		}
	}
	c.fail(w, 404, "Not Found")
}

func (c *CPAPI) serveNodes(w http.ResponseWriter, r *http.Request, cluster map[string]any, p []string) {
	cid, ns := getStr(cluster, "status.uuid"), getStr(cluster, "metadata.namespace")
	snDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagenodes")
	inCluster := c.nodeFilter(cluster)
	if len(p) == 0 {
		c.list(w, r, snDef, ns, inCluster, c.nodeDTO)
		return
	}
	node := c.findOne(snDef, ns, func(o map[string]any) bool { return inCluster(o) && getStr(o, "status.uuid") == p[0] })
	if node == nil {
		c.fail(w, 404, "storage node not found")
		return
	}
	nid := p[0]
	if len(p) == 1 {
		c.detail(w, r, snDef, ns, func(o map[string]any) bool { return getStr(o, "status.uuid") == nid }, c.nodeDTO)
		return
	}
	switch p[1] {
	case "capacity":
		if len(p) == 2 {
			c.serveSeries(w, r, "capacity", "node|"+nid, capacityOf(getPath(node, "status.resources.capacity")))
			return
		}
	case "iostats":
		if len(p) == 2 {
			c.serveSeries(w, r, "iostats", "node|"+nid, nil)
			return
		}
	case "devices":
		devDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagedevices")
		ofNode := func(o map[string]any) bool { return getStr(o, "status.nodeID") == nid }
		if len(p) == 2 {
			c.list(w, r, devDef, ns, ofNode, c.deviceDTO)
			return
		}
		dev := c.findOne(devDef, ns, func(o map[string]any) bool { return ofNode(o) && getStr(o, "spec.deviceID") == p[2] })
		if dev == nil {
			c.fail(w, 404, "device not found")
			return
		}
		did := p[2]
		if len(p) == 3 {
			c.detail(w, r, devDef, ns, func(o map[string]any) bool { return getStr(o, "spec.deviceID") == did }, c.deviceDTO)
			return
		}
		if len(p) == 4 {
			switch p[3] {
			case "health-info":
				writeJSON(w, 200, c.healthInfo(dev))
				return
			case "capacity":
				c.serveSeries(w, r, "capacity", "device|"+did, capacityOf(getPath(dev, "status.capacity")))
				return
			case "iostats":
				c.serveSeries(w, r, "iostats", "device|"+did, nil)
				return
			}
		}
	}
	_ = cid
	c.fail(w, 404, "Not Found")
}

func (c *CPAPI) servePools(w http.ResponseWriter, r *http.Request, cluster map[string]any, p []string) {
	cid, ns, name := getStr(cluster, "status.uuid"), getStr(cluster, "metadata.namespace"), getStr(cluster, "metadata.name")
	poolDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagepools")
	ofCluster := func(o map[string]any) bool { return getStr(o, "spec.clusterName") == name }
	if len(p) == 0 {
		c.list(w, r, poolDef, ns, ofCluster, c.poolDTO)
		return
	}
	pool := c.findOne(poolDef, ns, func(o map[string]any) bool { return ofCluster(o) && getStr(o, "status.uuid") == p[0] })
	if pool == nil {
		c.fail(w, 404, "storage pool not found")
		return
	}
	pid := p[0]
	if len(p) == 1 {
		c.detail(w, r, poolDef, ns, func(o map[string]any) bool { return getStr(o, "status.uuid") == pid }, c.poolDTO)
		return
	}
	prefix := cid + ":" + pid + ":"
	switch p[1] {
	case "iostats":
		if len(p) == 2 {
			c.serveSeries(w, r, "iostats", "pool|"+pid, nil)
			return
		}
	case "volumes":
		pvDef, _ := c.store.Lookup("", "v1", "persistentvolumes")
		inPool := func(o map[string]any) bool { return strings.HasPrefix(getStr(o, "spec.csi.volumeHandle"), prefix) }
		if len(p) == 2 {
			c.list(w, r, pvDef, "", inPool, c.volumeDTO)
			return
		}
		vid := p[2]
		pv := c.findOne(pvDef, "", func(o map[string]any) bool { return getStr(o, "spec.csi.volumeHandle") == prefix+vid })
		if pv == nil {
			c.fail(w, 404, "volume not found")
			return
		}
		if len(p) == 3 {
			c.detail(w, r, pvDef, "", func(o map[string]any) bool { return getStr(o, "spec.csi.volumeHandle") == prefix+vid }, c.volumeDTO)
			return
		}
		if len(p) == 4 {
			switch p[3] {
			case "capacity":
				size := quantityBytes(getStr(pv, "spec.capacity.storage"))
				c.serveSeries(w, r, "capacity", "volume|"+vid, map[string]any{"size_total": size, "size_used": float64(int64(size * usedFraction(vid))), "size_prov": size})
				return
			case "iostats":
				c.serveSeries(w, r, "iostats", "volume|"+vid, nil)
				return
			case "snapshots":
				vscDef, _ := c.store.Lookup("snapshot.storage.k8s.io", "v1", "volumesnapshotcontents")
				ofVolume := func(o map[string]any) bool {
					return strings.HasPrefix(getStr(o, "status.snapshotHandle"), prefix) && getStr(c.snapshotDTO(o), "lvol") == vid
				}
				c.list(w, r, vscDef, "", ofVolume, c.snapshotDTO)
				return
			}
		}
	case "snapshots":
		vscDef, _ := c.store.Lookup("snapshot.storage.k8s.io", "v1", "volumesnapshotcontents")
		inPool := func(o map[string]any) bool { return strings.HasPrefix(getStr(o, "status.snapshotHandle"), prefix) }
		if len(p) == 2 {
			c.list(w, r, vscDef, "", inPool, c.snapshotDTO)
			return
		}
		if len(p) == 3 {
			sid := p[2]
			match := func(o map[string]any) bool { return getStr(o, "status.snapshotHandle") == prefix+sid }
			if c.findOne(vscDef, "", match) == nil {
				c.fail(w, 404, "snapshot not found")
				return
			}
			c.detail(w, r, vscDef, "", match, c.snapshotDTO)
			return
		}
	}
	c.fail(w, 404, "Not Found")
}

func (c *CPAPI) serveTasks(w http.ResponseWriter, r *http.Request, cid string, p []string) {
	d := cpTaskDef()
	project := func(o map[string]any) map[string]any {
		t, _ := o["task"].(map[string]any)
		return t
	}
	switch len(p) {
	case 0:
		c.list(w, r, d, cid, func(map[string]any) bool { return true }, project)
	case 1:
		match := func(o map[string]any) bool { return getStr(o, "metadata.name") == p[0] }
		if c.findOne(d, cid, match) == nil {
			c.fail(w, 404, "task not found")
			return
		}
		c.detail(w, r, d, cid, match, project)
	default:
		c.fail(w, 404, "Not Found")
	}
}

// ---- lookups --------------------------------------------------------------------

func notProjection(o map[string]any) bool { return !isProjection(o) }

// cluster finds a user-facing StorageCluster by backend UUID.
func (c *CPAPI) cluster(id string) map[string]any {
	for _, sc := range userStorageClusters(c.store) {
		if getStr(sc, "status.uuid") == id {
			return sc
		}
	}
	return nil
}

func (c *CPAPI) findOne(d ResourceDef, ns string, match func(map[string]any) bool) map[string]any {
	items, _ := c.store.List(d, ns, "", "")
	for _, o := range items {
		if match(o) {
			return o
		}
	}
	return nil
}

// nodeFilter accepts the StorageNodes of a cluster: those whose
// StorageNodeSet (same namespace) names the cluster.
func (c *CPAPI) nodeFilter(cluster map[string]any) func(map[string]any) bool {
	ns, name := getStr(cluster, "metadata.namespace"), getStr(cluster, "metadata.name")
	setDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagenodesets")
	sets, _ := c.store.List(setDef, ns, "", "")
	owned := map[string]bool{}
	for _, s := range sets {
		if getStr(s, "spec.clusterName") == name {
			owned[getStr(s, "metadata.name")] = true
		}
	}
	return func(o map[string]any) bool { return owned[getStr(o, "spec.storageNodeSetRef")] }
}

func (c *CPAPI) clusterNodes(cluster map[string]any) []map[string]any {
	snDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagenodes")
	items, _ := c.store.List(snDef, getStr(cluster, "metadata.namespace"), "", "")
	in := c.nodeFilter(cluster)
	var out []map[string]any
	for _, n := range items {
		if in(n) {
			out = append(out, n)
		}
	}
	return out
}

func (c *CPAPI) nodeDevices(ns, nodeUUID string) []map[string]any {
	devDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagedevices")
	items, _ := c.store.List(devDef, ns, "", "")
	var out []map[string]any
	for _, d := range items {
		if getStr(d, "status.nodeID") == nodeUUID {
			out = append(out, d)
		}
	}
	return out
}

// clusterByUUID finds the cluster an object belongs to by UUID, for the
// projections that need cluster-level facts (stripe, nodes).
func (c *CPAPI) clusterByUUID(id string) map[string]any { return c.cluster(id) }

// ---- DTO projections ------------------------------------------------------------

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func boolOr(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func numOr(v any, def float64) float64 {
	if v == nil {
		return def
	}
	return num(v)
}

// quantityBytes parses the Kubernetes quantities the generator writes (Gi, Mi, Ti).
func quantityBytes(q string) float64 {
	q = strings.TrimSpace(q)
	for suffix, mult := range map[string]float64{"Ti": 1 << 40, "Gi": 1 << 30, "Mi": 1 << 20, "Ki": 1 << 10} {
		if strings.HasSuffix(q, suffix) {
			return num(strings.TrimSuffix(q, suffix)) * mult
		}
	}
	return num(q)
}

// usedFraction is a stable per-id utilization in [0.2, 0.7).
func usedFraction(id string) float64 {
	return 0.2 + float64(hash32(id)%500)/1000
}

// capacityOf turns a CR capacity block into a CapacityStatDTO-like map.
func capacityOf(v any) map[string]any {
	total, used := num(getPath(v, "totalBytes")), num(getPath(v, "usedBytes"))
	return map[string]any{"size_total": total, "size_used": used, "size_prov": total * 1.4}
}

// capacityStat completes a CapacityStatDTO from totals.
func capacityStat(m map[string]any, at time.Time) map[string]any {
	total, used := num(m["size_total"]), num(m["size_used"])
	prov := numOr(m["size_prov"], total)
	util := 0.0
	if total > 0 {
		util = float64(int64(used / total * 100))
	}
	return map[string]any{
		"date": float64(at.Unix()), "size_total": total, "size_prov": prov,
		"size_used": used, "size_free": total - used, "size_util": util,
	}
}

func (c *CPAPI) clusterCapacity(cluster map[string]any) map[string]any {
	total, used := 0.0, 0.0
	for _, n := range c.clusterNodes(cluster) {
		total += num(getPath(n, "status.resources.capacity.totalBytes"))
		used += num(getPath(n, "status.resources.capacity.usedBytes"))
	}
	return map[string]any{"size_total": total, "size_used": used, "size_prov": total * 1.4}
}

func (c *CPAPI) clusterDTO(sc map[string]any) map[string]any {
	spec, status := sc["spec"], sc["status"]
	_, backup := getPath(spec, "backup").(map[string]any)
	return map[string]any{
		"id":                            getStr(status, "uuid"),
		"name":                          getStr(sc, "metadata.name"),
		"nqn":                           getStr(status, "nqn"),
		"status":                        getStr(status, "status"),
		"is_re_balancing":               boolOf(getPath(status, "rebalancing")),
		"block_size":                    float64(4096),
		"distr_ndcs":                    numOr(getPath(spec, "stripe.dataChunks"), 2),
		"distr_npcs":                    numOr(getPath(spec, "stripe.parityChunks"), 1),
		"ha":                            true,
		"utilization_critical":          numOr(getPath(spec, "criticalThreshold.capacity"), 90),
		"utilization_warning":           numOr(getPath(spec, "warningThreshold.capacity"), 75),
		"provisioned_capacity_critical": numOr(getPath(spec, "criticalThreshold.provisionedCapacity"), 220),
		"provisioned_capacity_warning":  numOr(getPath(spec, "warningThreshold.provisionedCapacity"), 180),
		"node_affinity":                 boolOf(getPath(spec, "enableNodeAffinity")),
		"anti_affinity":                 false,
		"enable_failure_domain":         boolOf(getPath(spec, "enableFailureDomains")),
		"secret":                        "",
		"tls_enabled":                   false,
		"max_fault_tolerance":           numOr(getPath(status, "maxFaultTolerance"), 1),
		"backup_enabled":                backup,
		"capacity":                      capacityStat(c.clusterCapacity(sc), c.store.now()),
	}
}

func (c *CPAPI) nodeDTO(sn map[string]any) map[string]any {
	status := sn["status"]
	ns, uuid := getStr(sn, "metadata.namespace"), getStr(status, "uuid")
	devs := c.nodeDevices(ns, uuid)
	online := 0
	for _, d := range devs {
		if getStr(d, "status.deviceStatus") == "online" {
			online++
		}
	}
	mgmt, _, _ := strings.Cut(getStr(status, "ports.management"), ":")
	cpu := numOr(getPath(status, "resources.cpu"), 8)
	mem := quantityBytes(getStr(status, "resources.memory"))
	var secondary any
	if s := c.secondaryNode(sn); s != "" {
		secondary = s
	}
	var health any
	if h, ok := getPath(status, "health").(bool); ok {
		health = h
	}
	return map[string]any{
		"id":                  uuid,
		"cluster_id":          c.clusterUUIDOfNode(sn),
		"secondary_node_id":   secondary,
		"status":              getStr(status, "status"),
		"uptime":              getStr(status, "uptime"),
		"hostname":            getStr(status, "hostname"),
		"host_nqn":            "nqn.2014-08.org.nvmexpress:uuid:" + uuid,
		"cpu_total_count":     cpu,
		"cpu_spdk_count":      max(1, cpu-2),
		"cpu_poller_count":    float64(1),
		"memory":              mem,
		"hugepage_memory":     mem / 2,
		"spdk_mem":            mem / 4,
		"lvols":               numOr(getPath(status, "resources.volumes"), 0),
		"lvols_max":           float64(1000),
		"snapshots_max":       float64(500),
		"rpc_port":            numOr(getPath(status, "ports.rpc"), 8080),
		"lvol_subsys_port":    numOr(getPath(status, "ports.lvol"), 9100),
		"hublvol_port":        numOr(getPath(status, "ports.lvol"), 9100) + 1000,
		"nvmf_port":           numOr(getPath(status, "ports.nvmeof"), 4420),
		"mgmt_ip":             mgmt,
		"health_check":        health,
		"device_count":        float64(len(devs)),
		"online_device_count": float64(online),
		"failure_domain":      numOr(getPath(status, "failureDomain"), 0),
		"capacity":            capacityStat(capacityOf(getPath(status, "resources.capacity")), c.store.now()),
	}
}

// clusterUUIDOfNode walks node -> StorageNodeSet -> StorageCluster.
func (c *CPAPI) clusterUUIDOfNode(sn map[string]any) string {
	ns := getStr(sn, "metadata.namespace")
	setDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagenodesets")
	set, err := c.store.Get(setDef, ns, getStr(sn, "spec.storageNodeSetRef"))
	if err != nil {
		return ""
	}
	scDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storageclusters")
	sc, err := c.store.Get(scDef, ns, getStr(set, "spec.clusterName"))
	if err != nil {
		return ""
	}
	return getStr(sc, "status.uuid")
}

// secondaryNode pairs each node with the next one of its set, the way the
// backend assigns HA secondaries.
func (c *CPAPI) secondaryNode(sn map[string]any) string {
	snDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagenodes")
	items, _ := c.store.List(snDef, getStr(sn, "metadata.namespace"), "", "")
	var peers []string
	for _, n := range items {
		if getStr(n, "spec.storageNodeSetRef") == getStr(sn, "spec.storageNodeSetRef") {
			peers = append(peers, getStr(n, "status.uuid"))
		}
	}
	if len(peers) < 2 {
		return ""
	}
	for i, p := range peers {
		if p == getStr(sn, "status.uuid") {
			return peers[(i+1)%len(peers)]
		}
	}
	return ""
}

func (c *CPAPI) deviceDTO(dev map[string]any) map[string]any {
	status := dev["status"]
	hw := getPath(status, "hardware")
	devStatus := getStr(status, "deviceStatus")
	return map[string]any{
		"id":                   getStr(dev, "spec.deviceID"),
		"cluster_id":           getStr(status, "clusterID"),
		"storage_node_id":      getStr(status, "nodeID"),
		"model":                getStr(hw, "model"),
		"serial_number":        getStr(hw, "serialNumber"),
		"nvme_controller":      getStr(hw, "nvmeController"),
		"pcie_address":         getStr(hw, "pciAddress"),
		"status":               devStatus,
		"health_check":         devStatus == "online",
		"retries_exhausted":    devStatus == "removed",
		"size":                 num(getPath(status, "capacity.totalBytes")),
		"cluster_device_order": float64(hash32(getStr(dev, "spec.deviceID")) % 64),
		"io_error":             devStatus == "unavailable",
		"is_partition":         false,
		"nvmf_ips":             []any{},
		"nvmf_nqn":             "",
		"nvmf_port":            float64(0),
		"capacity":             capacityStat(capacityOf(getPath(status, "capacity")), c.store.now()),
	}
}

// healthInfo is the device's SMART page, deterministic per device id so a
// refresh reads the same wear and the same error counts.
func (c *CPAPI) healthInfo(dev map[string]any) map[string]any {
	id := getStr(dev, "spec.deviceID")
	h := fnv.New64a()
	h.Write([]byte("health|" + id))
	hs := h.Sum64() ^ c.seed
	rng := rand.New(rand.NewPCG(hs, hs>>5))
	hw := getPath(dev, "status.hardware")
	degraded := getStr(dev, "status.deviceStatus") != "online"
	mediaErrors := float64(rng.IntN(3))
	critical := float64(0)
	if degraded {
		mediaErrors += float64(10 + rng.IntN(200))
		critical = 4 // media errors bit
	}
	return map[string]any{
		"id":                                   id,
		"model_number":                         getStr(hw, "model"),
		"serial_number":                        getStr(hw, "serialNumber"),
		"firmware_revision":                    fmt.Sprintf("GDC%dQ", 5+rng.IntN(5)),
		"traddr":                               getStr(hw, "pciAddress"),
		"critical_warning":                     critical,
		"temperature_celsius":                  float64(30 + rng.IntN(20)),
		"available_spare_percentage":           float64(90 + rng.IntN(11)),
		"available_spare_threshold_percentage": float64(10),
		"percentage_used":                      float64(rng.IntN(40)),
		"data_units_read":                      float64(rng.IntN(900_000_000)),
		"data_units_written":                   float64(rng.IntN(600_000_000)),
		"host_read_commands":                   float64(rng.IntN(8_000_000_000)),
		"host_write_commands":                  float64(rng.IntN(5_000_000_000)),
		"controller_busy_time":                 float64(rng.IntN(20000)),
		"power_cycles":                         float64(rng.IntN(60)),
		"power_on_hours":                       float64(1000 + rng.IntN(30000)),
		"unsafe_shutdowns":                     float64(rng.IntN(10)),
		"media_errors":                         mediaErrors,
		"num_err_log_entries":                  float64(rng.IntN(20)),
		"warning_temperature_time_minutes":     float64(rng.IntN(30)),
		"critical_composite_temperature_time_minutes": float64(0),
	}
}

func (c *CPAPI) poolDTO(pool map[string]any) map[string]any {
	spec := pool["spec"]
	ns, name := getStr(pool, "metadata.namespace"), getStr(spec, "clusterName")
	scDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storageclusters")
	cid := ""
	if sc, err := c.store.Get(scDef, ns, name); err == nil {
		cid = getStr(sc, "status.uuid")
	}
	pid := getStr(pool, "status.uuid")
	// capacity: what the pool's volumes provision and use
	pvDef, _ := c.store.Lookup("", "v1", "persistentvolumes")
	pvs, _ := c.store.List(pvDef, "", "", "")
	prov, used := 0.0, 0.0
	for _, pv := range pvs {
		if strings.HasPrefix(getStr(pv, "spec.csi.volumeHandle"), cid+":"+pid+":") {
			size := quantityBytes(getStr(pv, "spec.capacity.storage"))
			prov += size
			used += size * usedFraction(getStr(pv, "spec.csi.volumeHandle"))
		}
	}
	limit := quantityBytes(getStr(spec, "capacityLimit"))
	return map[string]any{
		"id":              pid,
		"cluster_id":      cid,
		"name":            getStr(pool, "metadata.name"),
		"status":          getStr(pool, "status.status"),
		"max_size":        limit,
		"volume_max_size": quantityBytes(getStr(spec, "logicalVolumeMaxSize")),
		"max_rw_iops":     numOr(getPath(spec, "qos.iops"), 0),
		"max_rw_mbytes":   numOr(getPath(spec, "qos.rwMbytes"), 0),
		"max_r_mbytes":    numOr(getPath(spec, "qos.rMbytes"), 0),
		"max_w_mbytes":    numOr(getPath(spec, "qos.wMbytes"), 0),
		"capacity":        capacityStat(map[string]any{"size_total": limit, "size_used": float64(int64(used)), "size_prov": prov}, c.store.now()),
		"dhchap":          getPath(spec, "dhchap") != nil,
		"allowed_hosts":   []any{},
	}
}

// volumeDTO projects a PV (and its PVC) into a VolumeDTO. The lvol's
// identity is the handle's third part, its placement a stable node of the
// cluster.
func (c *CPAPI) volumeDTO(pv map[string]any) map[string]any {
	parts := strings.Split(getStr(pv, "spec.csi.volumeHandle"), ":")
	if len(parts) != 3 {
		return nil
	}
	cid, pid, vid := parts[0], parts[1], parts[2]
	cluster := c.clusterByUUID(cid)
	nodes := c.clusterNodes(cluster)
	var primary, secondary map[string]any
	if len(nodes) > 0 {
		primary = nodes[int(hash32(vid))%len(nodes)]
		if len(nodes) > 1 {
			secondary = nodes[(int(hash32(vid))+1)%len(nodes)]
		}
	}
	pnid, hostname, port := "", "", 0.0
	nodeIDs := []any{}
	if primary != nil {
		pnid, hostname, port = getStr(primary, "status.uuid"), getStr(primary, "status.hostname"), numOr(getPath(primary, "status.ports.lvol"), 9100)
		nodeIDs = append(nodeIDs, pnid)
	}
	if secondary != nil {
		nodeIDs = append(nodeIDs, getStr(secondary, "status.uuid"))
	}
	status := "online"
	if primary != nil && getStr(primary, "status.status") != "online" {
		status = "offline"
	}
	poolName := ""
	poolDef, _ := c.store.Lookup(sbGroup, "v1alpha1", "storagepools")
	if cluster != nil {
		if pool := c.findOne(poolDef, getStr(cluster, "metadata.namespace"), func(o map[string]any) bool { return getStr(o, "status.uuid") == pid }); pool != nil {
			poolName = getStr(pool, "metadata.name")
		}
	}
	size := quantityBytes(getStr(pv, "spec.capacity.storage"))
	pvcNs, pvcName := getStr(pv, "spec.claimRef.namespace"), getStr(pv, "spec.claimRef.name")
	migrating := c.migrating(getStr(pv, "metadata.name"))
	return map[string]any{
		"id":                       vid,
		"cluster_id":               cid,
		"storage_node_id":          pnid,
		"name":                     getStr(pv, "metadata.name"),
		"status":                   status,
		"health_check":             status == "online",
		"io_error":                 false,
		"migrating":                migrating,
		"nqn":                      "nqn.2023-02.io.simplyblock:" + cid + ":lvol:" + vid,
		"hostname":                 hostname,
		"priority_class":           float64(0),
		"namespace":                pvcNs,
		"fabric":                   "tcp",
		"nodes":                    nodeIDs,
		"port":                     port,
		"size":                     size,
		"ndcs":                     numOr(getPath(cluster, "spec.stripe.dataChunks"), 2),
		"npcs":                     numOr(getPath(cluster, "spec.stripe.parityChunks"), 1),
		"pool_uuid":                pid,
		"pool_name":                poolName,
		"pvc_name":                 pvcNs + "/" + pvcName,
		"snapshot_name":            "",
		"blobid":                   float64(hash32(vid) % 100000),
		"ns_id":                    float64(1),
		"cloned_from":              nil,
		"high_availability":        secondary != nil,
		"do_replicate":             false,
		"max_namespace_per_subsys": float64(1),
		"max_rw_iops":              float64(0),
		"max_rw_mbytes":            float64(0),
		"max_r_mbytes":             float64(0),
		"max_w_mbytes":             float64(0),
		"allowed_hosts":            []any{},
		"policy":                   "",
		"capacity":                 capacityStat(map[string]any{"size_total": size, "size_used": float64(int64(size * usedFraction(vid))), "size_prov": size}, c.store.now()),
		"rep_info":                 nil,
		"from_source":              false,
	}
}

// migrating reports whether a running VolumeMigration names the PV.
func (c *CPAPI) migrating(pvName string) bool {
	d, ok := c.store.Lookup(sbGroup, "v1alpha1", "volumemigrations")
	if !ok {
		return false
	}
	items, _ := c.store.List(d, "", "", "")
	for _, m := range items {
		if getStr(m, "spec.pvName") == pvName && getStr(m, "status.phase") == "Running" {
			return true
		}
	}
	return false
}

// snapshotDTO projects a VolumeSnapshotContent (plus its VolumeSnapshot and
// the source PVC's PV) into a SnapshotDTO.
func (c *CPAPI) snapshotDTO(vsc map[string]any) map[string]any {
	parts := strings.Split(getStr(vsc, "status.snapshotHandle"), ":")
	if len(parts) != 3 {
		return nil
	}
	cid, pid, sid := parts[0], parts[1], parts[2]
	refNs, refName := getStr(vsc, "spec.volumeSnapshotRef.namespace"), getStr(vsc, "spec.volumeSnapshotRef.name")
	vsDef, _ := c.store.Lookup("snapshot.storage.k8s.io", "v1", "volumesnapshots")
	vs, _ := c.store.Get(vsDef, refNs, refName)
	lvol := ""
	size := 0.0
	created := getStr(vs, "status.creationTime")
	if vs != nil {
		size = quantityBytes(getStr(vs, "status.restoreSize"))
		pvcDef, _ := c.store.Lookup("", "v1", "persistentvolumeclaims")
		if pvc, err := c.store.Get(pvcDef, refNs, getStr(vs, "spec.source.persistentVolumeClaimName")); err == nil {
			pvDef, _ := c.store.Lookup("", "v1", "persistentvolumes")
			if pv, err := c.store.Get(pvDef, "", getStr(pvc, "spec.volumeName")); err == nil {
				if h := strings.Split(getStr(pv, "spec.csi.volumeHandle"), ":"); len(h) == 3 && h[0] == cid && h[1] == pid {
					lvol = h[2]
				}
			}
		}
	}
	var lvolAny any
	if lvol != "" {
		lvolAny = lvol
	}
	return map[string]any{
		"id":           sid,
		"name":         refName,
		"status":       "online",
		"health_check": boolOr(getPath(vsc, "status.readyToUse"), true),
		"size":         size,
		"used_size":    float64(int64(size * usedFraction(sid) / 2)),
		"migrating":    false,
		"lvol":         lvolAny,
		"created_at":   created,
	}
}

// ---- series: capacity and iostats ------------------------------------------------

// parseHistory reads the control plane's history parameter (`30m`, `2h`, `1d`)
// into a duration. An empty value means the latest record only.
func parseHistory(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return 0
}

// serveSeries answers a capacity or iostats route: one record without
// history, else records at a step that keeps a window around 60 to 120 points.
func (c *CPAPI) serveSeries(w http.ResponseWriter, r *http.Request, kind, instance string, totals map[string]any) {
	now := c.store.now()
	span := parseHistory(r.URL.Query().Get("history"))
	var times []time.Time
	if span <= 0 {
		times = []time.Time{now}
	} else {
		step := span / 60
		if step < time.Minute {
			step = time.Minute
		}
		for t := now.Add(-span); !t.After(now); t = t.Add(step) {
			times = append(times, t)
		}
	}
	out := make([]map[string]any, 0, len(times))
	for _, t := range times {
		if kind == "capacity" {
			out = append(out, c.capacityRecord(instance, totals, t))
		} else {
			out = append(out, c.iostatRecord(instance, t))
		}
	}
	writeJSON(w, 200, out)
}

// capacityRecord drifts the used figure around the CR's value with the
// seeded series, so a history chart moves while the latest point agrees with
// what the CRD reports.
func (c *CPAPI) capacityRecord(instance string, totals map[string]any, t time.Time) map[string]any {
	total, used, prov := num(totals["size_total"]), num(totals["size_used"]), numOr(totals["size_prov"], num(totals["size_total"]))
	drift := seriesValue(c.seed, "sb_capacity_util_percent", instance, t) - seriesValue(c.seed, "sb_capacity_util_percent", instance, c.store.now())
	used = used * (1 + drift/400)
	if used < 0 {
		used = 0
	}
	if used > total {
		used = total
	}
	rec := capacityStat(map[string]any{"size_total": total, "size_used": float64(int64(used)), "size_prov": prov}, t)
	if total > 0 {
		rec["size_prov_util"] = float64(int64(prov / total * 100))
	} else {
		rec["size_prov_util"] = float64(0)
	}
	return rec
}

// iostatRecord is one I/O statistics record in the control plane's field names.
func (c *CPAPI) iostatRecord(instance string, t time.Time) map[string]any {
	v := func(metric string) float64 { return float64(int64(seriesValue(c.seed, metric, instance, t))) }
	readBps, writeBps := v("sb_read_bytes"), v("sb_write_bytes")
	readIOPS, writeIOPS := v("sb_read_iops"), v("sb_write_iops")
	return map[string]any{
		"date":             float64(t.Unix()),
		"read_bytes":       readBps * 60,
		"read_bytes_ps":    readBps,
		"read_io":          readIOPS * 60,
		"read_io_ps":       readIOPS,
		"read_latency_ps":  v("sb_read_latency_us"),
		"write_bytes":      writeBps * 60,
		"write_bytes_ps":   writeBps,
		"write_io":         writeIOPS * 60,
		"write_io_ps":      writeIOPS,
		"write_latency_ps": v("sb_write_latency_us"),
		"unmap_bytes_ps":   float64(0),
		"unmap_io_ps":      float64(0),
		"record_duration":  float64(60),
	}
}

// ---- list, detail, and SSE -----------------------------------------------------------

type projectFn func(map[string]any) map[string]any

// list answers a collection route, or streams it when ?watch=true.
func (c *CPAPI) list(w http.ResponseWriter, r *http.Request, d ResourceDef, ns string, accept func(map[string]any) bool, project projectFn) {
	snapshot := func() any {
		items, _ := c.store.List(d, ns, "", "")
		out := []map[string]any{}
		for _, o := range items {
			if accept(o) {
				if dto := project(o); dto != nil {
					out = append(out, dto)
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return getStr(out[i], "id") < getStr(out[j], "id") })
		return out
	}
	if watching(r) {
		c.serveSSE(w, r, snapshot, d, ns, accept, project)
		return
	}
	writeJSON(w, 200, snapshot())
}

// detail answers an object route, or streams the object when ?watch=true.
func (c *CPAPI) detail(w http.ResponseWriter, r *http.Request, d ResourceDef, ns string, match func(map[string]any) bool, project projectFn) {
	snapshot := func() any {
		if o := c.findOne(d, ns, match); o != nil {
			return project(o)
		}
		return map[string]any{}
	}
	if watching(r) {
		c.serveSSE(w, r, snapshot, d, ns, match, project)
		return
	}
	if o := c.findOne(d, ns, match); o != nil {
		writeJSON(w, 200, project(o))
		return
	}
	c.fail(w, 404, "not found")
}

func watching(r *http.Request) bool {
	v := strings.ToLower(r.URL.Query().Get("watch"))
	return v == "true" || v == "1"
}

// serveSSE streams: retry hint, a snapshot event, then created/updated/deleted
// events mapped from the store's watch (ADDED/MODIFIED/DELETED), a ping
// comment every 15 s, and a close after an hour so clients exercise their
// reconnect path.
func (c *CPAPI) serveSSE(w http.ResponseWriter, r *http.Request, snapshot func() any, d ResourceDef, ns string, accept func(map[string]any) bool, project projectFn) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.fail(w, 500, "streaming unsupported")
		return
	}
	ch, cancel := c.store.Watch(d, ns)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprintf(w, "retry: %d\n", sseRetry)
	writeSSE(w, "snapshot", snapshot())
	flusher.Flush()

	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	deadline := time.NewTimer(sseMaxLife)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			if !accept(ev.Object) {
				continue
			}
			name := map[string]string{"ADDED": "created", "MODIFIED": "updated", "DELETED": "deleted"}[ev.Type]
			if name == "" {
				continue
			}
			var payload any = project(ev.Object)
			if payload == nil {
				payload = map[string]any{}
			}
			if err := writeSSE(w, name, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
