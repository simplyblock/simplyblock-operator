package main

import (
	"testing"
)

// tickUntil runs sim ticks until pred is true or n ticks passed.
func tickUntil(t *testing.T, sim *Simulator, n int, pred func() bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		if pred() {
			return
		}
		sim.Tick()
	}
	if !pred() {
		t.Fatalf("condition not reached after %d ticks", n)
	}
}

func TestOpProgressesToSucceeded(t *testing.T) {
	st := newTestStore()
	sim := NewSimulator(st, "sb", 0 /* never fail */, 1)
	d := testDef()

	st.Create(d, "sb", map[string]any{
		"metadata": map[string]any{"name": "op-restart"},
		"spec":     map[string]any{"action": "restart", "storageNodeRef": "sn-01"},
	})

	phase := func() string {
		obj, _ := st.Get(d, "sb", "op-restart")
		return getStr(obj, "status.phase")
	}
	sawRunning := false
	tickUntil(t, sim, 20, func() bool {
		if phase() == "Running" {
			sawRunning = true
		}
		return phase() == "Succeeded"
	})
	if !sawRunning {
		t.Fatal("op never passed through Running")
	}
	obj, _ := st.Get(d, "sb", "op-restart")
	if getStr(obj, "status.startedAt") == "" || getStr(obj, "status.completedAt") == "" {
		t.Fatalf("timestamps missing: %v", obj["status"])
	}
	if getStr(obj, "status.subPhase") == "" {
		t.Fatalf("storagenodeops should carry a subPhase: %v", obj["status"])
	}
	// the run left an audit trail of events
	evDef, _ := st.Lookup("", "v1", "events")
	evs, _ := st.List(evDef, "sb", "", "fieldSelector")
	evs, _ = st.List(evDef, "sb", "", "involvedObject.name=op-restart")
	if len(evs) == 0 {
		t.Fatal("no events emitted for the op")
	}
}

func TestOpFailsAtFailRateOne(t *testing.T) {
	st := newTestStore()
	sim := NewSimulator(st, "sb", 1 /* always fail */, 1)
	d := testDef()
	st.Create(d, "sb", map[string]any{
		"metadata": map[string]any{"name": "op-doomed"},
		"spec":     map[string]any{"action": "restart"},
	})
	tickUntil(t, sim, 20, func() bool {
		obj, _ := st.Get(d, "sb", "op-doomed")
		return getStr(obj, "status.phase") == "Failed"
	})
}

func TestAbortWins(t *testing.T) {
	st := newTestStore()
	sim := NewSimulator(st, "sb", 0, 1)
	d := testDef()
	st.Create(d, "sb", map[string]any{
		"metadata": map[string]any{"name": "op-abort"},
		"spec":     map[string]any{"action": "restart"},
	})
	sim.Tick() // Pending
	sim.Tick() // Running

	obj, _ := st.Get(d, "sb", "op-abort")
	setPath(obj, "spec.abort", true)
	st.Update(d, "sb", obj)

	sim.Tick()
	obj, _ = st.Get(d, "sb", "op-abort")
	if getStr(obj, "status.phase") != "Aborted" {
		t.Fatalf("want Aborted, got %s", getStr(obj, "status.phase"))
	}
}

func TestDRPCFailover(t *testing.T) {
	st := newTestStore()
	sim := NewSimulator(st, "sb", 0, 1)
	d, _ := st.Lookup(ramenGroup, "v1alpha1", "drplacementcontrols")
	st.Create(d, "prod-db", map[string]any{
		"metadata": map[string]any{"name": "prod-db-drpc"},
		"spec": map[string]any{
			"action":           "Failover",
			"preferredCluster": "sb-primary",
			"failoverCluster":  "sb-dr",
		},
		"status": map[string]any{"phase": "Deployed"},
	})
	sawFailingOver := false
	tickUntil(t, sim, 20, func() bool {
		obj, _ := st.Get(d, "prod-db", "prod-db-drpc")
		if getStr(obj, "status.phase") == "FailingOver" {
			sawFailingOver = true
		}
		return getStr(obj, "status.phase") == "FailedOver"
	})
	if !sawFailingOver {
		t.Fatal("DRPC never passed through FailingOver")
	}
	obj, _ := st.Get(d, "prod-db", "prod-db-drpc")
	if getStr(obj, "status.preferredDecision.clusterName") != "sb-dr" {
		t.Fatalf("failover did not move the decision: %v", obj["status"])
	}
}

func TestReplicationFailoverFlipsSlots(t *testing.T) {
	st := newTestStore()
	sim := NewSimulator(st, "sb", 0, 1)
	slotDef, _ := st.Lookup(sbGroup, "v1alpha1", "replicationslots")
	opDef, _ := st.Lookup(sbGroup, "v1alpha1", "replicationops")

	st.Create(slotDef, "sb", map[string]any{
		"metadata": map[string]any{"name": "slot-a"},
		"spec":     map[string]any{"policyRef": "policy-prod", "pvcRef": "prod-db/data-postgres-0"},
		"status":   map[string]any{"state": "replicating", "direction": "source"},
	})
	st.Create(opDef, "sb", map[string]any{
		"metadata": map[string]any{"name": "failover-1"},
		"spec":     map[string]any{"action": "failover", "scope": "policy", "ref": "policy-prod"},
	})
	tickUntil(t, sim, 20, func() bool {
		op, _ := st.Get(opDef, "sb", "failover-1")
		return getStr(op, "status.phase") == "Succeeded"
	})
	slot, _ := st.Get(slotDef, "sb", "slot-a")
	if getStr(slot, "status.state") != "failed_over" || getStr(slot, "status.direction") != "target" {
		t.Fatalf("slot did not follow the failover: %v", slot["status"])
	}
}

func TestGenerateAllDatasets(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			st := NewStore(99)
			st.Register(allResourceDefs()...)
			Generate(st, sc, 99, "simplyblock")

			nodeDef, _ := st.Lookup(sbGroup, "v1alpha1", "storagenodes")
			nodes, _ := st.List(nodeDef, "", "", "")
			if len(nodes) != sc.Workers {
				t.Fatalf("want %d StorageNodes, got %d", sc.Workers, len(nodes))
			}
			devDef, _ := st.Lookup(sbGroup, "v1alpha1", "storagedevices")
			devs, _ := st.List(devDef, "", "", "")
			if len(devs) != sc.Workers*sc.DevicesPerNode {
				t.Fatalf("want %d devices, got %d", sc.Workers*sc.DevicesPerNode, len(devs))
			}
			// every PV has a PVC and vice versa
			pvcDef, _ := st.Lookup("", "v1", "persistentvolumeclaims")
			pvDef, _ := st.Lookup("", "v1", "persistentvolumes")
			pvcs, _ := st.List(pvcDef, "", "", "")
			pvs, _ := st.List(pvDef, "", "", "")
			if len(pvcs) != sc.Volumes || len(pvs) != sc.Volumes {
				t.Fatalf("want %d PVC/PV, got %d/%d", sc.Volumes, len(pvcs), len(pvs))
			}
			for _, pv := range pvs {
				claimNs, claimName := getStr(pv, "spec.claimRef.namespace"), getStr(pv, "spec.claimRef.name")
				if _, err := st.Get(pvcDef, claimNs, claimName); err != nil {
					t.Fatalf("PV %s claims missing PVC %s/%s", getStr(pv, "metadata.name"), claimNs, claimName)
				}
			}
			if sc.WithDR {
				slotDef, _ := st.Lookup(sbGroup, "v1alpha1", "replicationslots")
				slots, _ := st.List(slotDef, "", "", "")
				if len(slots) == 0 {
					t.Fatal("DR scenario generated no replication slots")
				}
				for _, slot := range slots {
					ref := getStr(slot, "spec.pvcRef")
					parts := []string{ref[:len(ref)-len(ref[len(ref)-1:])], ""}
					_ = parts
					nsName := ref
					sep := -1
					for i, c := range nsName {
						if c == '/' {
							sep = i
						}
					}
					if _, err := st.Get(pvcDef, nsName[:sep], nsName[sep+1:]); err != nil {
						t.Fatalf("slot %s references missing PVC %s", getStr(slot, "metadata.name"), ref)
					}
				}
				drpcDef, _ := st.Lookup(ramenGroup, "v1alpha1", "drplacementcontrols")
				drpcs, _ := st.List(drpcDef, "", "", "")
				if len(drpcs) != len(sc.AppNamespaces) {
					t.Fatalf("want a DRPC per app namespace (%d), got %d", len(sc.AppNamespaces), len(drpcs))
				}
			}
		})
	}
}

// Determinism: the same dataset+seed must produce the identical world.
func TestGenerateDeterministic(t *testing.T) {
	build := func() map[string]int {
		st := NewStore(7)
		st.Register(allResourceDefs()...)
		sc, _ := scenarioByName("medium-degraded")
		Generate(st, sc, 7, "simplyblock")
		out := map[string]int{}
		nodeDef, _ := st.Lookup(sbGroup, "v1alpha1", "storagenodes")
		nodes, _ := st.List(nodeDef, "", "", "")
		for _, n := range nodes {
			out[getStr(n, "metadata.name")+"|"+getStr(n, "status.uuid")+"|"+getStr(n, "status.status")]++
		}
		return out
	}
	a, b := build(), build()
	if len(a) == 0 {
		t.Fatal("no nodes")
	}
	for k := range a {
		if b[k] != a[k] {
			t.Fatalf("worlds differ at %q", k)
		}
	}
}
