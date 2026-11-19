package main

// Simulator event-log tests: every node, device, and cluster status
// transition the simulator makes lands in the control plane event log as a
// STATUS_CHANGE record, and control plane tasks advance through their phases.

import (
	"strings"
	"testing"
)

func logMessages(st *Store, clusterUUID string) []string {
	var out []string
	for _, r := range st.Logs(clusterUUID, 0) {
		out = append(out, r.Event+"|"+r.Message)
	}
	return out
}

func hasMessage(msgs []string, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

func TestNodeRestartLogsStatusChanges(t *testing.T) {
	st := genWorld(t, "small-healthy")
	cluster := userClusters(t, st)[0]
	cid, ns := getStr(cluster, "status.uuid"), getStr(cluster, "metadata.namespace")
	sim := NewSimulator(st, "simplyblock", 0, 1)
	before := len(st.Logs(cid, 0))

	opDef, _ := st.Lookup(sbGroup, "v1alpha1", "storagenodeops")
	st.Create(opDef, ns, map[string]any{
		"metadata": map[string]any{"name": "restart-sn-01-test"},
		"spec":     map[string]any{"action": "restart", "storageNodeRef": "sn-01"},
	})
	nodeDef, _ := st.Lookup(sbGroup, "v1alpha1", "storagenodes")
	status := func() string {
		n, _ := st.Get(nodeDef, ns, "sn-01")
		return getStr(n, "status.status")
	}
	sawRestart := false
	tickUntil(t, sim, 30, func() bool {
		if status() == "in_restart" {
			sawRestart = true
		}
		op, _ := st.Get(opDef, ns, "restart-sn-01-test")
		return getStr(op, "status.phase") == "Succeeded"
	})
	if !sawRestart || status() != "online" {
		t.Fatalf("node should pass through in_restart and end online (saw %v, now %s)", sawRestart, status())
	}
	msgs := logMessages(st, cid)
	if len(msgs) <= before {
		t.Fatal("the restart appended nothing to the event log")
	}
	for _, want := range []string{
		"STATUS_CHANGE|Storage node status changed from: online to: in_restart",
		"STATUS_CHANGE|Storage node status changed from: in_restart to: online",
		"STATUS_CHANGE|Device status changed from: online to: unavailable",
		"STATUS_CHANGE|Device status changed from: unavailable to: online",
	} {
		if !hasMessage(msgs, want) {
			t.Errorf("event log lacks %q", want)
		}
	}
	// the record names the node
	n, _ := st.Get(nodeDef, ns, "sn-01")
	found := false
	for _, r := range st.Logs(cid, 0) {
		if r.NodeID == getStr(n, "status.uuid") && strings.Contains(r.Message, "to: in_restart") {
			found = true
		}
	}
	if !found {
		t.Error("no STATUS_CHANGE record carries the node uuid as NodeId")
	}
}

func TestClusterShutdownLogsStatusChange(t *testing.T) {
	st := genWorld(t, "small-healthy")
	cluster := userClusters(t, st)[0]
	cid, ns, name := getStr(cluster, "status.uuid"), getStr(cluster, "metadata.namespace"), getStr(cluster, "metadata.name")
	sim := NewSimulator(st, "simplyblock", 0, 1)
	opDef, _ := st.Lookup(sbGroup, "v1alpha1", "storageclusterops")
	st.Create(opDef, ns, map[string]any{
		"metadata": map[string]any{"name": "shutdown-test"},
		"spec":     map[string]any{"action": "shutdown", "clusterRef": name},
	})
	scDef, _ := st.Lookup(sbGroup, "v1alpha1", "storageclusters")
	tickUntil(t, sim, 30, func() bool {
		op, _ := st.Get(opDef, ns, "shutdown-test")
		return getStr(op, "status.phase") == "Succeeded"
	})
	c, _ := st.Get(scDef, ns, name)
	if getStr(c, "status.status") != "inactive" {
		t.Fatalf("cluster status after shutdown: %s", getStr(c, "status.status"))
	}
	if !hasMessage(logMessages(st, cid), "STATUS_CHANGE|Cluster status changed from: active to: inactive") {
		t.Fatal("event log lacks the cluster status change")
	}
}

func TestTasksAdvanceToDone(t *testing.T) {
	st := genWorld(t, "medium-degraded")
	cid := getStr(userClusters(t, st)[0], "status.uuid")
	sim := NewSimulator(st, "simplyblock", 0, 1)
	open := func() int {
		n := 0
		for _, tk := range st.Tasks(cid) {
			if getStr(tk, "status") != "done" {
				n++
			}
		}
		return n
	}
	if open() == 0 {
		t.Fatal("the dataset should seed open tasks")
	}
	tickUntil(t, sim, 60, func() bool { return open() == 0 })
	// a master finishes only after its sub-tasks
	for _, tk := range st.Tasks(cid) {
		if getStr(tk, "function_result") == "" {
			t.Errorf("done task %s has no function_result", getStr(tk, "id"))
		}
	}
}
