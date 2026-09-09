package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"
)

// Simulator is the mock's stand-in for every reconciler: it moves Ops objects
// through their phases, honors spec.abort, walks DRPC failovers and volume
// migrations forward, cuts replication slots over when their failover
// finishes and emits Events for each transition. It performs no underlying
// change — status is the only thing it writes, which is exactly what a UI
// needs in order to be tested against live transitions.
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
	s.advanceDRPCs()
	s.refreshTelemetry()
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
	default:
		// Running: burn ticks, then land on a terminal phase
		s.ticks[uid]++
		target := 3 + int(hash32(uid)%4)
		if len(subPhases) > 0 {
			idx := s.ticks[uid] * len(subPhases) / (target + 1)
			if idx >= len(subPhases) {
				idx = len(subPhases) - 1
			}
			setPath(obj, "status.subPhase", subPhases[idx])
		}
		if s.ticks[uid] < target {
			setPath(obj, "status.message", fmt.Sprintf("in progress (%d/%d)", s.ticks[uid], target))
			s.st.Update(d, ns, obj)
			return
		}
		delete(s.ticks, uid)
		if s.rng.Float64() < s.failRate {
			setPath(obj, "status.phase", "Failed")
			setPath(obj, "status.message", "simulated failure (mock failRate)")
			s.emitEvent(d.Kind, name, ns, "Warning", "Failed", d.Kind+" "+name+" failed (simulated)")
		} else {
			setPath(obj, "status.phase", terminalOK)
			setPath(obj, "status.message", "completed")
			s.emitEvent(d.Kind, name, ns, "Normal", "Completed", d.Kind+" "+name+" completed")
			s.onSuccess(d, obj)
		}
		setPath(obj, "status.completedAt", now)
		s.st.Update(d, ns, obj)
	}
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
