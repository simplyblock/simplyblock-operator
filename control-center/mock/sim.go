package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"
)

// Simulator is the mock's stand-in for every reconciler and for the control
// plane's task runners: it moves Ops objects through their phases, honors
// spec.abort, walks DRPC failovers and volume migrations forward, cuts
// replication slots over when their failover finishes, advances control
// plane tasks, and emits Events for each transition. Every status change it
// makes on a storage node, device, or cluster is written to the control
// plane event log as the STATUS_CHANGE record the real control plane would
// log, so the console's alert rules see the same stream on both surfaces.
// It performs no underlying change. Status is the only thing it writes,
// which is exactly what a UI needs in order to be tested against live
// transitions.
type Simulator struct {
	st       *Store
	ns       string
	failRate float64

	mu    sync.Mutex
	rng   *rand.Rand
	ticks map[string]int // uid -> ticks spent Running
}

func NewSimulator(st *Store, ns string, failRate float64, seed uint64) *Simulator {
	return &Simulator{
		st: st, ns: ns, failRate: failRate,
		rng:   rand.New(rand.NewPCG(seed^0xa5a5a5a5, seed<<1|1)),
		ticks: map[string]int{},
	}
}

func (s *Simulator) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick()
		}
	}
}

// opsKinds lists every resource that follows the Pending/Running/terminal
// convention, including the proposed kinds so UI-created objects of those
// types progress too.
var opsKinds = []struct {
	version, resource string
	terminalOK        string
	phases            []string // path Pending -> ... -> terminalOK
	subPhases         []string
}{
	{"v1alpha1", "storageclusterops", "Succeeded", nil, nil},
	{"v1alpha1", "storagenodeops", "Succeeded", nil, []string{"Validating", "Suspending", "Restarting", "Verifying"}},
	{"v1alpha1", "replicationops", "Succeeded", nil, nil},
	{"v1alpha2", "operatorops", "Succeeded", nil, nil},
	{"v1alpha1", "storagedeviceops", "Succeeded", nil, nil},
	{"v1alpha1", "storagepoolops", "Succeeded", nil, nil},
	{"v1alpha1", "storagebackupops", "Succeeded", nil, nil},
	{"v1alpha1", "controlplaneops", "Succeeded", nil, nil},
	{"v1alpha1", "persistentvolumeops", "Succeeded", nil, nil},
	{"v1alpha1", "volumemigrations", "Completed", []string{"Pending", "Validating", "Running"}, nil},
}

// Tick advances every in-flight object by one step.
func (s *Simulator) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range opsKinds {
		d, ok := s.st.Lookup(sbGroup, k.version, k.resource)
		if !ok {
			continue
		}
		items, _ := s.st.List(d, "", "", "")
		for _, obj := range items {
			s.advanceOp(d, obj, k.terminalOK, k.phases, k.subPhases)
		}
	}
	s.advanceTasks()
	s.advanceDRPCs()
	s.advanceProjection()
	s.reconcileObservedGeneration()
	s.refreshTelemetry()
	s.capacityNotice()
}

// advanceProjection plays the agent side of the hub/agent split: it refreshes
// the managed-cluster heartbeat (lastSyncTime) and, on the transport-namespace
// StorageCluster projection, catches observedGeneration up to the hub's spec
// generation, so a spec the UI just changed shows `Pending` for one tick and
// then `Applied`.
func (s *Simulator) advanceProjection() {
	now := time.Now().UTC().Format(time.RFC3339)

	if d, ok := s.st.Lookup(sbGroup, "v1alpha1", "managedclusters"); ok {
		for _, mc := range mustList(s.st, d) {
			if getStr(mc, "status.agentConnected") == "false" {
				continue
			}
			setPath(mc, "status.lastSyncTime", now)
			s.st.Update(d, "", mc)
		}
	}

	// The projected StorageCluster lives in a sb-mc-* namespace, and its status
	// is agent-authored. Catch observedGeneration up to generation (drift heals).
	if d, ok := s.st.Lookup(sbGroup, "v1alpha1", "storageclusters"); ok {
		for _, sc := range mustList(s.st, d) {
			ns := getStr(sc, "metadata.namespace")
			if len(ns) < 6 || ns[:6] != "sb-mc-" {
				continue
			}
			gen := genOf(sc)
			obs := obsGenOf(sc)
			setPath(sc, "status.lastSyncTime", now)
			if obs < gen {
				setPath(sc, "status.observedGeneration", float64(gen))
				setPath(sc, "status.conditions", []any{
					map[string]any{"type": "AgentConnected", "status": "True", "reason": "Heartbeat", "message": "agent pulling intent", "lastTransitionTime": now, "observedGeneration": float64(gen)},
					map[string]any{"type": "Applied", "status": "True", "reason": "Applied", "message": "spec applied on the managed cluster", "lastTransitionTime": now, "observedGeneration": float64(gen)},
				})
			}
			s.st.Update(d, ns, sc)
		}
	}
}

// reconcileObservedGeneration is the agent applying spec: for every simplyblock
// entity kind whose status lags metadata.generation (because the UI just
// patched spec), bring observedGeneration up. The one-tick lag is what lets the
// UI distinguish "applied" from "pending" (design-crd-model.md §7.9).
func (s *Simulator) reconcileObservedGeneration() {
	for _, d := range s.st.Defs() {
		if d.Group != sbGroup || !isEntityKind(d.Resource) {
			continue
		}
		for _, obj := range mustList(s.st, d) {
			ns := getStr(obj, "metadata.namespace")
			if len(ns) >= 6 && ns[:6] == "sb-mc-" {
				continue // transport projection handled in advanceProjection
			}
			st, _ := obj["status"].(map[string]any)
			if st == nil {
				continue
			}
			if obsGenOf(obj) < genOf(obj) {
				setPath(obj, "status.observedGeneration", float64(genOf(obj)))
				s.st.Update(d, ns, obj)
			}
		}
	}
}

func mustList(st *Store, d ResourceDef) []map[string]any {
	items, _ := st.List(d, "", "", "")
	return items
}

func genOf(obj map[string]any) int {
	if f, ok := getPath(obj, "metadata.generation").(float64); ok {
		return int(f)
	}
	return 1
}

func obsGenOf(obj map[string]any) int {
	if f, ok := getPath(obj, "status.observedGeneration").(float64); ok {
		return int(f)
	}
	return 0
}

func (s *Simulator) advanceOp(d ResourceDef, obj map[string]any, terminalOK string, phases, subPhases []string) {
	phase := getStr(obj, "status.phase")
	uid := getStr(obj, "metadata.uid")
	name := getStr(obj, "metadata.name")
	ns := getStr(obj, "metadata.namespace")
	now := time.Now().UTC().Format(time.RFC3339)

	switch phase {
	case terminalOK, "Failed", "Aborted", "Completed":
		delete(s.ticks, uid)
		return
	}

	abort, _ := getPath(obj, "spec.abort").(bool)
	if abort {
		setPath(obj, "status.phase", "Aborted")
		setPath(obj, "status.completedAt", now)
		setPath(obj, "status.message", "aborted on request")
		s.st.Update(d, ns, obj)
		s.emitEvent(d.Kind, name, ns, "Normal", "Aborted", "operation aborted on request")
		s.onFinish(d, obj, false)
		delete(s.ticks, uid)
		return
	}

	if phases == nil {
		phases = []string{"Pending", "Running"}
	}

	switch {
	case phase == "":
		setPath(obj, "status.phase", phases[0])
		s.st.Update(d, ns, obj)
	case phase != phases[len(phases)-1]:
		// walk the intermediate phases one tick at a time
		next := phases[1]
		for i, p := range phases {
			if p == phase && i+1 < len(phases) {
				next = phases[i+1]
			}
		}
		setPath(obj, "status.phase", next)
		if getStr(obj, "status.startedAt") == "" {
			setPath(obj, "status.startedAt", now)
		}
		setPath(obj, "status.message", "in progress")
		s.st.Update(d, ns, obj)
		s.emitEvent(d.Kind, name, ns, "Normal", "Started", d.Kind+" "+name+" started")
		if next == phases[len(phases)-1] {
			s.onRunning(d, obj)
		}
	default:
		// Running: burn ticks, then land on a terminal phase
		if s.ticks[uid] == 0 && d.Resource == "storageclusterops" && getStr(obj, "spec.action") == "node-rolling-restart" {
			// seeded in Running: the first tick is when the walker takes over
			s.onRunning(d, obj)
		}
		s.ticks[uid]++
		target := 3 + int(hash32(uid)%4)
		if len(subPhases) > 0 {
			idx := s.ticks[uid] * len(subPhases) / (target + 1)
			if idx >= len(subPhases) {
				idx = len(subPhases) - 1
			}
			setPath(obj, "status.subPhase", subPhases[idx])
		}
		if d.Resource == "storageclusterops" && getStr(obj, "spec.action") == "node-rolling-restart" {
			if s.rollingRestartStep(obj) {
				setPath(obj, "status.message", "restarting nodes one at a time")
				s.st.Update(d, ns, obj)
				return
			}
		} else if s.ticks[uid] < target {
			setPath(obj, "status.message", fmt.Sprintf("in progress (%d/%d)", s.ticks[uid], target))
			s.st.Update(d, ns, obj)
			return
		}
		delete(s.ticks, uid)
		if s.rng.Float64() < s.failRate {
			setPath(obj, "status.phase", "Failed")
			setPath(obj, "status.message", "simulated failure (mock failRate)")
			s.emitEvent(d.Kind, name, ns, "Warning", "Failed", d.Kind+" "+name+" failed (simulated)")
			s.onFinish(d, obj, false)
		} else {
			setPath(obj, "status.phase", terminalOK)
			setPath(obj, "status.message", "completed")
			s.emitEvent(d.Kind, name, ns, "Normal", "Completed", d.Kind+" "+name+" completed")
			s.onSuccess(d, obj)
			s.onFinish(d, obj, true)
		}
		setPath(obj, "status.completedAt", now)
		s.st.Update(d, ns, obj)
	}
}

// ---- status transitions the Ops drive ----------------------------------------------

// onRunning applies what an operation does to its target when it starts
// doing it: the node it restarts goes in_restart, the cluster it activates
// goes in_activation.
func (s *Simulator) onRunning(d ResourceDef, obj map[string]any) {
	ns := getStr(obj, "metadata.namespace")
	switch d.Resource {
	case "storagenodeops":
		node := s.node(ns, getStr(obj, "spec.storageNodeRef"))
		if node == nil {
			return
		}
		switch getStr(obj, "spec.action") {
		case "restart":
			s.setNodeStatus(node, "in_restart")
		case "shutdown":
			s.setNodeStatus(node, "in_shutdown")
		case "suspend":
			s.setNodeStatus(node, "suspended")
		case "remove", "migrate":
			s.setNodeStatus(node, "in_removal")
		}
	case "storageclusterops":
		cluster := s.cluster(ns, getStr(obj, "spec.clusterRef"))
		if cluster == nil {
			return
		}
		switch getStr(obj, "spec.action") {
		case "activate":
			s.setClusterStatus(cluster, "in_activation")
		case "expand":
			s.setClusterStatus(cluster, "in_expansion")
		case "shutdown":
			for _, n := range s.clusterNodes(cluster) {
				s.setNodeStatus(n, "in_shutdown")
			}
		case "node-rolling-restart":
			s.setClusterStatus(cluster, "degraded")
		}
	}
}

// onFinish lands the target where the operation leaves it: back online after
// a successful restart, offline after a failed one.
func (s *Simulator) onFinish(d ResourceDef, obj map[string]any, ok bool) {
	ns := getStr(obj, "metadata.namespace")
	switch d.Resource {
	case "storagenodeops":
		node := s.node(ns, getStr(obj, "spec.storageNodeRef"))
		if node == nil {
			return
		}
		switch action := getStr(obj, "spec.action"); {
		case !ok && (action == "restart" || action == "resume" || action == "migrate"):
			s.setNodeStatus(node, "offline")
		case !ok:
			// a failed shutdown, suspend, or removal leaves the node where it was
		case action == "restart", action == "resume", action == "migrate":
			s.setNodeStatus(node, "online")
		case action == "shutdown":
			s.setNodeStatus(node, "offline")
		case action == "suspend":
			s.setNodeStatus(node, "suspended")
		case action == "remove":
			s.setNodeStatus(node, "removed")
		}
	case "storageclusterops":
		cluster := s.cluster(ns, getStr(obj, "spec.clusterRef"))
		if cluster == nil {
			return
		}
		switch action := getStr(obj, "spec.action"); {
		case !ok && (action == "activate" || action == "expand" || action == "start"):
			s.setClusterStatus(cluster, "unready")
		case !ok:
			s.setClusterStatus(cluster, "degraded")
		case action == "shutdown":
			for _, n := range s.clusterNodes(cluster) {
				s.setNodeStatus(n, "offline")
			}
			s.setClusterStatus(cluster, "inactive")
		case action == "start":
			for _, n := range s.clusterNodes(cluster) {
				s.setNodeStatus(n, "online")
			}
			s.setClusterStatus(cluster, "active")
		default:
			s.setClusterStatus(cluster, "active")
		}
	}
}

// rollingRestartStep walks nodeRollingRestartStatus one node per tick: the
// node restarted last tick comes back online, the next pending one goes
// in_restart. It reports whether work remains.
func (s *Simulator) rollingRestartStep(obj map[string]any) bool {
	ns := getStr(obj, "metadata.namespace")
	status, _ := getPath(obj, "status.nodeRollingRestartStatus").(map[string]any)
	if status == nil {
		status = map[string]any{}
		setPath(obj, "status.nodeRollingRestartStatus", status)
	}
	if cur := getStr(status, "currentNode"); cur != "" {
		if node := s.node(ns, cur); node != nil {
			s.setNodeStatus(node, "online")
		}
		processed, _ := status["processedNodes"].([]any)
		status["processedNodes"] = append(processed, cur)
		delete(status, "currentNode")
	}
	pending, _ := status["pendingNodes"].([]any)
	if len(pending) == 0 {
		status["nodePhase"] = "Completed"
		return false
	}
	next, _ := pending[0].(string)
	status["pendingNodes"] = pending[1:]
	status["currentNode"] = next
	status["nodePhase"] = "Running"
	if node := s.node(ns, next); node != nil {
		s.setNodeStatus(node, "in_restart")
	}
	return true
}

func (s *Simulator) node(ns, name string) map[string]any {
	d, ok := s.st.Lookup(sbGroup, "v1alpha1", "storagenodes")
	if !ok {
		return nil
	}
	n, err := s.st.Get(d, ns, name)
	if err != nil {
		return nil
	}
	return n
}

func (s *Simulator) cluster(ns, name string) map[string]any {
	d, ok := s.st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	if !ok {
		return nil
	}
	c, err := s.st.Get(d, ns, name)
	if err != nil {
		return nil
	}
	return c
}

// clusterNodes lists the StorageNodes whose StorageNodeSet names the cluster.
func (s *Simulator) clusterNodes(cluster map[string]any) []map[string]any {
	ns, name := getStr(cluster, "metadata.namespace"), getStr(cluster, "metadata.name")
	setDef, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagenodesets")
	owned := map[string]bool{}
	sets, _ := s.st.List(setDef, ns, "", "")
	for _, set := range sets {
		if getStr(set, "spec.clusterName") == name {
			owned[getStr(set, "metadata.name")] = true
		}
	}
	nodeDef, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagenodes")
	nodes, _ := s.st.List(nodeDef, ns, "", "")
	var out []map[string]any
	for _, n := range nodes {
		if owned[getStr(n, "spec.storageNodeSetRef")] {
			out = append(out, n)
		}
	}
	return out
}

// clusterUUIDOfNode walks node -> StorageNodeSet -> StorageCluster.
func (s *Simulator) clusterUUIDOfNode(node map[string]any) string {
	ns := getStr(node, "metadata.namespace")
	setDef, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagenodesets")
	set, err := s.st.Get(setDef, ns, getStr(node, "spec.storageNodeSetRef"))
	if err != nil {
		return ""
	}
	if c := s.cluster(ns, getStr(set, "spec.clusterName")); c != nil {
		return getStr(c, "status.uuid")
	}
	return ""
}

// logLevel is the level the control plane logs a status with.
func logLevel(status string) string {
	switch status {
	case "online", "active", "in_creation", "in_activation", "in_expansion", "schedulable":
		return "Info"
	case "offline", "removed", "down", "unreachable", "inactive":
		return "Error"
	default:
		return "Warning"
	}
}

// setNodeStatus moves a StorageNode to a status, mirrors it into its
// StorageNodeSet, carries its devices along, and logs the transition.
func (s *Simulator) setNodeStatus(node map[string]any, to string) {
	from := getStr(node, "status.status")
	if from == to {
		return
	}
	ns, uuid := getStr(node, "metadata.namespace"), getStr(node, "status.uuid")
	cid := s.clusterUUIDOfNode(node)
	setPath(node, "status.status", to)
	setPath(node, "status.health", to == "online")
	setPath(node, "status.postedAt", time.Now().UTC().Format(time.RFC3339))
	d, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagenodes")
	s.st.Update(d, ns, node)
	s.appendLog(cid, uuid, "STATUS_CHANGE", logLevel(to), fmt.Sprintf("Storage node status changed from: %s to: %s", from, to), uuid, to)
	s.appendLog(cid, uuid, "node_status", logLevel(to), "Storage node is "+to, uuid, to)
	s.emitEvent("StorageNode", getStr(node, "metadata.name"), ns, map[bool]string{true: "Normal", false: "Warning"}[logLevel(to) == "Info"], "StatusChanged", "storage node "+from+" -> "+to)

	// the set's node table
	setDef, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagenodesets")
	if set, err := s.st.Get(setDef, ns, getStr(node, "spec.storageNodeSetRef")); err == nil {
		entries, _ := getPath(set, "status.nodes").([]any)
		online := 0
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if em == nil {
				continue
			}
			if em["uuid"] == uuid {
				em["status"], em["health"] = to, to == "online"
			}
			if em["status"] == "online" {
				online++
			}
		}
		setPath(set, "status.onlineNodes", float64(online))
		setPath(set, "status.offlineNodes", float64(len(entries)-online))
		s.st.Update(setDef, ns, set)
	}

	// the devices follow: unavailable while the node is away, online with it
	devStatus := ""
	switch to {
	case "online":
		devStatus = "online"
	case "removed":
		devStatus = "removed"
	case "in_restart", "in_shutdown", "offline", "in_removal", "down", "unreachable":
		devStatus = "unavailable"
	}
	if devStatus == "" {
		return
	}
	devDef, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagedevices")
	devs, _ := s.st.List(devDef, ns, "", "")
	for _, dev := range devs {
		if getStr(dev, "status.nodeID") == uuid {
			s.setDeviceStatus(dev, devStatus)
		}
	}
}

var devicePhases = map[string]string{
	"online": "Online", "unavailable": "Unavailable", "offline": "Offline",
	"removed": "Removed", "read_only": "ReadOnly", "failed": "Failed",
}

// setDeviceStatus moves a StorageDevice to a status and logs the transition.
func (s *Simulator) setDeviceStatus(dev map[string]any, to string) {
	from := getStr(dev, "status.deviceStatus")
	if from == to {
		return
	}
	ns := getStr(dev, "metadata.namespace")
	setPath(dev, "status.deviceStatus", to)
	if phase, ok := devicePhases[to]; ok {
		setPath(dev, "status.phase", phase)
	}
	d, _ := s.st.Lookup(sbGroup, "v1alpha1", "storagedevices")
	s.st.Update(d, ns, dev)
	cid, nid, did := getStr(dev, "status.clusterID"), getStr(dev, "status.nodeID"), getStr(dev, "spec.deviceID")
	s.appendLog(cid, nid, "STATUS_CHANGE", logLevel(to), fmt.Sprintf("Device status changed from: %s to: %s", from, to), did, to)
	s.appendLog(cid, nid, "device_status", logLevel(to), "Device is "+to, did, to)
}

// setClusterStatus moves a StorageCluster to a status and logs the transition.
func (s *Simulator) setClusterStatus(cluster map[string]any, to string) {
	from := getStr(cluster, "status.status")
	if from == to {
		return
	}
	ns, cid := getStr(cluster, "metadata.namespace"), getStr(cluster, "status.uuid")
	setPath(cluster, "status.status", to)
	setPath(cluster, "status.lastUpdated", time.Now().UTC().Format(time.RFC3339))
	d, _ := s.st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	s.st.Update(d, ns, cluster)
	s.appendLog(cid, "", "STATUS_CHANGE", logLevel(to), fmt.Sprintf("Cluster status changed from: %s to: %s", from, to), cid, to)
	s.emitEvent("StorageCluster", getStr(cluster, "metadata.name"), ns, map[bool]string{true: "Normal", false: "Warning"}[logLevel(to) == "Info"], "StatusChanged", "cluster "+from+" -> "+to)
}

func (s *Simulator) appendLog(clusterUUID, nodeID, event, level, msg, storageID, status string) {
	if clusterUUID == "" {
		return
	}
	s.st.AppendLog(clusterUUID, LogRecord{
		Date: logDate(time.Now()), NodeID: nodeID, Event: event, Level: level,
		Message: msg, StorageID: storageID, Status: status,
	})
}

// capacityNotice logs the control plane's periodic CAPACITY record for a
// cluster now and then, so the log keeps moving between operations.
func (s *Simulator) capacityNotice() {
	if s.rng.Float64() > 0.15 {
		return
	}
	clusters := userStorageClusters(s.st)
	if len(clusters) == 0 {
		return
	}
	cluster := clusters[s.rng.IntN(len(clusters))]
	total, used := 0.0, 0.0
	for _, n := range s.clusterNodes(cluster) {
		total += num(getPath(n, "status.resources.capacity.totalBytes"))
		used += num(getPath(n, "status.resources.capacity.usedBytes"))
	}
	if total == 0 {
		return
	}
	util := int(used / total * 100)
	level := "Info"
	if util >= int(numOr(getPath(cluster, "spec.warningThreshold.capacity"), 75)) {
		level = "Warning"
	}
	cid := getStr(cluster, "status.uuid")
	s.appendLog(cid, "", "CAPACITY", level, fmt.Sprintf("Cluster capacity utilization reached: %d%%", util), cid, "")
}

// ---- control plane tasks ---------------------------------------------------------

// advanceTasks plays the task runners: a new task starts, a running one
// finishes after a few ticks (failing at the fail rate, with retries), and a
// master task finishes once every sub-task of it is done.
func (s *Simulator) advanceTasks() {
	d := cpTaskDef()
	items, _ := s.st.List(d, "", "", "")
	subsOf := map[string][]map[string]any{}
	var masters []map[string]any
	for _, it := range items {
		t, _ := it["task"].(map[string]any)
		if t == nil {
			continue
		}
		if parent := getStr(t, "function_params.parent_task_id"); parent != "" {
			subsOf[parent] = append(subsOf[parent], t)
		} else {
			masters = append(masters, t)
		}
	}
	for _, m := range masters {
		cid := getStr(m, "cluster_id")
		subs := subsOf[getStr(m, "id")]
		if len(subs) == 0 {
			s.advanceTask(cid, m, true)
			continue
		}
		if getStr(m, "status") == "new" {
			m["status"] = "running"
			s.st.UpdateTask(cid, m)
		}
		// sub-tasks run one at a time, in order
		allDone := true
		for _, sub := range subs {
			if getStr(sub, "status") == "done" {
				continue
			}
			allDone = false
			s.advanceTask(cid, sub, true)
			break
		}
		if allDone && getStr(m, "status") != "done" {
			m["status"] = "done"
			if getStr(m, "function_result") == "" {
				m["function_result"] = "success"
			}
			s.st.UpdateTask(cid, m)
		}
	}
}

// advanceTask moves one task a step: new -> running, running -> done after
// its ticks, with a retry when the simulated run fails.
func (s *Simulator) advanceTask(cid string, t map[string]any, mayFail bool) {
	id := getStr(t, "id")
	switch getStr(t, "status") {
	case "new":
		t["status"] = "running"
	case "running":
		s.ticks[id]++
		if s.ticks[id] < 2+int(hash32(id)%3) {
			return
		}
		delete(s.ticks, id)
		if mayFail && s.rng.Float64() < s.failRate {
			retry := int(num(t["retry"])) + 1
			t["retry"] = float64(retry)
			if retry >= int(numOr(t["max_retry"], 3)) {
				t["status"] = "done"
				t["function_result"] = "failed: retries exhausted"
			} else {
				t["function_result"] = fmt.Sprintf("retrying (%d)", retry)
			}
			break
		}
		t["status"] = "done"
		t["function_result"] = "success"
	default:
		return
	}
	s.st.UpdateTask(cid, t)
}

// onSuccess propagates a finished operation into the objects it acted on.
func (s *Simulator) onSuccess(d ResourceDef, obj map[string]any) {
	if d.Resource != "replicationops" || getStr(obj, "spec.action") != "failover" {
		return
	}
	// a successful failover flips the policy's slots
	slotsDef, ok := s.st.Lookup(sbGroup, "v1alpha1", "replicationslots")
	if !ok {
		return
	}
	slots, _ := s.st.List(slotsDef, "", "", "")
	ref := getStr(obj, "spec.ref")
	for _, slot := range slots {
		if getStr(slot, "spec.policyRef") != ref {
			continue
		}
		setPath(slot, "status.state", "failed_over")
		setPath(slot, "status.direction", "target")
		s.st.Update(slotsDef, getStr(slot, "metadata.namespace"), slot)
	}
}

// advanceDRPCs walks Ramen placement actions: Failover -> FailingOver ->
// FailedOver, Relocate -> Relocating -> Relocated. Clearing spec.action (or
// none set) leaves the DRPC where it is, like the real operator.
func (s *Simulator) advanceDRPCs() {
	d, ok := s.st.Lookup(ramenGroup, "v1alpha1", "drplacementcontrols")
	if !ok {
		return
	}
	items, _ := s.st.List(d, "", "", "")
	for _, obj := range items {
		action := getStr(obj, "spec.action")
		phase := getStr(obj, "status.phase")
		uid := getStr(obj, "metadata.uid")
		ns := getStr(obj, "metadata.namespace")
		name := getStr(obj, "metadata.name")

		var running, done string
		switch action {
		case "Failover":
			running, done = "FailingOver", "FailedOver"
		case "Relocate":
			running, done = "Relocating", "Relocated"
		default:
			continue
		}
		switch phase {
		case done:
			delete(s.ticks, uid)
			continue
		case running:
			s.ticks[uid]++
			if s.ticks[uid] < 3+int(hash32(uid)%3) {
				continue
			}
			delete(s.ticks, uid)
			setPath(obj, "status.phase", done)
			setPath(obj, "status.lastUpdateTime", time.Now().UTC().Format(time.RFC3339))
			if done == "FailedOver" {
				setPath(obj, "status.preferredDecision.clusterName", getStr(obj, "spec.failoverCluster"))
			}
			s.st.Update(d, ns, obj)
			s.emitEvent("DRPlacementControl", name, ns, "Normal", done, "DR action "+action+" completed")
		default:
			setPath(obj, "status.phase", running)
			setPath(obj, "status.lastUpdateTime", time.Now().UTC().Format(time.RFC3339))
			s.st.Update(d, ns, obj)
			s.emitEvent("DRPlacementControl", name, ns, "Normal", running, "DR action "+action+" started")
		}
	}
}

// refreshTelemetry nudges a couple of nodes' heartbeat fields so lists look
// alive without flooding the watch streams.
func (s *Simulator) refreshTelemetry() {
	d, ok := s.st.Lookup(sbGroup, "v1alpha1", "storagenodes")
	if !ok {
		return
	}
	items, _ := s.st.List(d, "", "", "")
	if len(items) == 0 {
		return
	}
	for i := 0; i < 2; i++ {
		obj := items[s.rng.IntN(int(len(items)))]
		setPath(obj, "status.postedAt", time.Now().UTC().Format(time.RFC3339))
		if used, ok := getPath(obj, "status.resources.capacity.usedBytes").(float64); ok {
			jitter := (s.rng.Float64() - 0.45) * 0.002 * used
			setPath(obj, "status.resources.capacity.usedBytes", used+jitter)
			setPath(obj, "status.resources.capacity.sampledAt", time.Now().UTC().Format(time.RFC3339))
		}
		s.st.Update(d, getStr(obj, "metadata.namespace"), obj)
	}
}

func (s *Simulator) emitEvent(kind, name, ns, typ, reason, msg string) {
	d, ok := s.st.Lookup("", "v1", "events")
	if !ok || ns == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.st.Create(d, ns, map[string]any{
		"metadata": map[string]any{"generateName": name + "."},
		"type":     typ, "reason": reason, "message": msg,
		"involvedObject": map[string]any{"kind": kind, "name": name, "namespace": ns},
		"source":         map[string]any{"component": "sb-mock-simulator"},
		"firstTimestamp": now, "lastTimestamp": now, "count": float64(1),
	})
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
