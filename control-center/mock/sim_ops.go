package main

// The simulator's stand-in for the Ops reconcilers: an operation is walked
// through the step graph the operator declares for its action
// (operator/internal/controllers/*/graphs.go, transcribed in
// control-center/ops-model.jsx and OPERATIONS.md), one step every few ticks,
// honoring spec.abort only at the steps the graph marks abortable, and
// finishing with the side effect the real reconciler leaves on the target.
// It writes status only.

import (
	"fmt"
	"time"
)

type opsGraph struct {
	steps     []string
	abortable []string
}

// the operator's graphs, per resource and action
var opsGraphs = map[string]map[string]opsGraph{
	"storageclusterops": {
		"Activate":       {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Expand":         {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Shutdown":       {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Start":          {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"CancelTask":     {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Restart":        {[]string{"ShuttingDown", "Starting"}, nil},
		"RollingRestart": {[]string{"CheckingPeers", "ShuttingDownNode", "RestartingNode", "Rebalancing"}, []string{"CheckingPeers", "Rebalancing"}},
	},
	"storagenodeops": {
		"Shutdown":        {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Suspend":         {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Resume":          {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Restart":         {[]string{"Requesting", "Departing", "Awaiting"}, []string{"Requesting"}},
		"Remove":          {[]string{"Validating", "ShuttingDown", "MigratingDevices", "MigratingVolumes", "Verifying", "Removing", "AwaitingRemoval"}, []string{"Validating"}},
		"Migrate":         {[]string{"Preparing", "Relocating", "AwaitingNode", "Promoting"}, []string{"Preparing"}},
		"HostMaintenance": {[]string{"Holding", "ShuttingDown", "Releasing", "AwaitingHost", "Restarting", "Cleanup"}, []string{"Holding"}},
	},
	"storagedeviceops": {
		"Restart":     {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"Fail":        {[]string{"Removing", "Failing", "Awaiting"}, []string{"Removing"}},
		"CheckHealth": {[]string{"Requesting"}, []string{"Requesting"}},
	},
	"storagepoolops": {
		"Rebalance": {[]string{"Validating", "Migrating"}, nil},
	},
	"persistentvolumeops": {
		"Migrate":              {[]string{"Validating", "Migrating", "Verifying"}, []string{"Validating", "Migrating"}},
		"Relocate":             {[]string{"Validating", "Replicating", "Committing", "AwaitingCutover", "AttachingPaths", "Proceeding", "Verifying"}, []string{"Validating", "Replicating"}},
		"JoinConsistencyGroup": {[]string{"Planning", "SelectingSubsystem", "PreparingTarget", "AttachingPaths", "Suspending", "MovingNamespaces", "Resuming", "Committing", "ReleasingPaths"}, []string{"Planning", "SelectingSubsystem", "PreparingTarget", "AttachingPaths"}},
	},
	"controlplaneops": {
		"Restart":         {[]string{"Draining", "Restarting", "Awaiting"}, []string{"Draining"}},
		"Upgrade":         {[]string{"Preflight", "Draining", "Applying", "Awaiting", "Verifying"}, []string{"Preflight", "Draining"}},
		"Backup":          {[]string{"Requesting", "Awaiting"}, []string{"Requesting"}},
		"HostMaintenance": {[]string{"Holding", "EnteringMaintenance", "MovingCoordinators", "ExcludingProcesses", "Releasing", "AwaitingHost", "IncludingProcesses", "LeavingMaintenance", "Cleanup"}, []string{"Holding"}},
	},
	"operatorops": {
		"Discover": {[]string{"Inspecting", "Probing", "Writing"}, nil},
	},
	"storagebackupops": {
		"Restore": {[]string{"Validating", "Restoring", "AwaitingVolume", "Binding"}, nil},
	},
}

// the spec field that names the target, per resource
var opsTargetRef = map[string][2]string{
	"storageclusterops":   {"clusterRef", "storageclusters"},
	"storagenodeops":      {"nodeRef", "storagenodes"},
	"storagedeviceops":    {"deviceRef", "storagedevices"},
	"storagepoolops":      {"poolRef", "storagepools"},
	"controlplaneops":     {"controlPlaneRef", "controlplanes"},
	"persistentvolumeops": {"persistentVolumeName", ""},
}

const ticksPerStep = 2

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// advanceGraphOp moves one operation along its graph.
func (s *Simulator) advanceGraphOp(d ResourceDef, obj map[string]any) {
	phase := getStr(obj, "status.phase")
	uid := getStr(obj, "metadata.uid")
	name := getStr(obj, "metadata.name")
	ns := getStr(obj, "metadata.namespace")
	now := time.Now().UTC()
	nowS := now.Format(time.RFC3339)
	action := getStr(obj, "spec.action")
	graph, known := opsGraphs[d.Resource][action]

	switch phase {
	case "Succeeded", "Failed", "Aborted":
		delete(s.ticks, uid)
		return
	}
	if !known {
		setPath(obj, "status.phase", "Failed")
		setPath(obj, "status.message", fmt.Sprintf("%s has no action %s", d.Kind, action))
		setPath(obj, "status.completedAt", nowS)
		s.st.Update(d, ns, obj)
		return
	}
	step := getStr(obj, "status.step.state")
	abort, _ := getPath(obj, "spec.abort").(bool)

	finish := func(phase, message string) {
		setPath(obj, "status.phase", phase)
		setPath(obj, "status.message", message)
		setPath(obj, "status.completedAt", nowS)
		if phase == "Succeeded" {
			setPath(obj, "status.step", map[string]any{"state": "Completed"})
		}
		s.st.Update(d, ns, obj)
		s.releaseTarget(d, obj)
		delete(s.ticks, uid)
	}

	if abort && phase != "" {
		if contains(graph.abortable, step) {
			finish("Aborted", "aborted at step "+step+" on request")
			s.emitEvent(d.Kind, name, ns, "Normal", "Aborted", d.Kind+" "+name+" aborted at "+step)
			return
		}
		if !s.refused[uid] {
			s.refused[uid] = true
			s.emitEvent(d.Kind, name, ns, "Warning", "AbortRefused", fmt.Sprintf("%s %s cannot be aborted at step %s", d.Kind, name, step))
		}
	}

	switch phase {
	case "":
		setPath(obj, "status.phase", "Pending")
		setPath(obj, "status.message", "waiting for the target's lock")
		s.st.Update(d, ns, obj)
	case "Pending":
		if !s.takeTarget(d, obj) {
			return
		}
		setPath(obj, "status.phase", "Running")
		setPath(obj, "status.startedAt", nowS)
		s.enterStep(obj, graph.steps[0], now)
		setPath(obj, "status.message", "step "+graph.steps[0])
		s.st.Update(d, ns, obj)
		s.emitEvent(d.Kind, name, ns, "Normal", "Started", d.Kind+" "+name+" started at "+graph.steps[0])
	case "Running":
		s.ticks[uid]++
		if s.ticks[uid] < ticksPerStep {
			return
		}
		s.ticks[uid] = 0
		idx := -1
		for i, st := range graph.steps {
			if st == step {
				idx = i
			}
		}
		if s.rng.Float64() < s.failRate {
			finish("Failed", fmt.Sprintf("step %s failed (simulated)", step))
			s.emitEvent(d.Kind, name, ns, "Warning", "Failed", d.Kind+" "+name+" failed at "+step)
			return
		}
		if idx+1 < len(graph.steps) {
			next := graph.steps[idx+1]
			s.enterStep(obj, next, now)
			setPath(obj, "status.message", "step "+next)
			s.st.Update(d, ns, obj)
			return
		}
		finish("Succeeded", "completed")
		s.emitEvent(d.Kind, name, ns, "Normal", "Completed", d.Kind+" "+name+" completed")
		s.onGraphSuccess(d, obj)
	}
}

func (s *Simulator) enterStep(obj map[string]any, state string, now time.Time) {
	setPath(obj, "status.step", map[string]any{
		"state":    state,
		"claim":    map[string]any{"state": state, "attempt": float64(1), "leaseUntil": now.Add(time.Minute).Format(time.RFC3339)},
		"deadline": now.Add(30 * time.Minute).Format(time.RFC3339),
	})
}

// takeTarget takes the target's activeOpsRef lock, which is how a second
// operation on one object stays Pending.
func (s *Simulator) takeTarget(d ResourceDef, obj map[string]any) bool {
	ref, ok := opsTargetRef[d.Resource]
	if !ok || ref[1] == "" {
		return true
	}
	td, ok := s.st.LookupResource(sbGroup, ref[1])
	if !ok {
		return true
	}
	target, err := s.st.Get(td, getStr(obj, "metadata.namespace"), getStr(obj, "spec."+ref[0]))
	if err != nil {
		setPath(obj, "status.phase", "Failed")
		setPath(obj, "status.message", fmt.Sprintf("%s %q not found", td.Kind, getStr(obj, "spec."+ref[0])))
		setPath(obj, "status.completedAt", time.Now().UTC().Format(time.RFC3339))
		s.st.Update(d, getStr(obj, "metadata.namespace"), obj)
		return false
	}
	holder := getStr(target, "status.activeOpsRef")
	if holder != "" && holder != getStr(obj, "metadata.name") {
		setPath(obj, "status.message", "waiting for operation "+holder+" to release "+td.Kind+" "+getStr(target, "metadata.name"))
		s.st.Update(d, getStr(obj, "metadata.namespace"), obj)
		if !s.refused["queued:"+getStr(obj, "metadata.uid")] {
			s.refused["queued:"+getStr(obj, "metadata.uid")] = true
			s.emitEvent(d.Kind, getStr(obj, "metadata.name"), getStr(obj, "metadata.namespace"), "Normal", "OperationQueued", "queued behind "+holder)
		}
		return false
	}
	setPath(target, "status.activeOpsRef", getStr(obj, "metadata.name"))
	s.st.Update(td, getStr(target, "metadata.namespace"), target)
	return true
}

func (s *Simulator) releaseTarget(d ResourceDef, obj map[string]any) {
	ref, ok := opsTargetRef[d.Resource]
	if !ok || ref[1] == "" {
		return
	}
	td, ok := s.st.LookupResource(sbGroup, ref[1])
	if !ok {
		return
	}
	target, err := s.st.Get(td, getStr(obj, "metadata.namespace"), getStr(obj, "spec."+ref[0]))
	if err != nil || getStr(target, "status.activeOpsRef") != getStr(obj, "metadata.name") {
		return
	}
	setPath(target, "status.activeOpsRef", "")
	delete(target["status"].(map[string]any), "activeOpsRef")
	s.st.Update(td, getStr(target, "metadata.namespace"), target)
}

// onGraphSuccess leaves on the target what the real reconciler leaves: the
// node's control plane status and phase, the device's, the cluster's task
// window and volume move generation.
func (s *Simulator) onGraphSuccess(d ResourceDef, obj map[string]any) {
	ns := getStr(obj, "metadata.namespace")
	action := getStr(obj, "spec.action")
	switch d.Resource {
	case "storagenodeops":
		nd, _ := s.st.LookupResource(sbGroup, "storagenodes")
		node, err := s.st.Get(nd, ns, getStr(obj, "spec.nodeRef"))
		if err != nil {
			return
		}
		switch action {
		case "Restart", "Resume", "HostMaintenance":
			setPath(node, "status.status", "online")
			setPath(node, "status.phase", "Online")
			setPath(node, "status.health", true)
			delete(node["status"].(map[string]any), "message")
			s.dropTask(ns, "node_restart")
		case "Shutdown":
			setPath(node, "status.status", "offline")
			setPath(node, "status.phase", "Offline")
		case "Suspend":
			setPath(node, "status.status", "suspended")
			setPath(node, "status.phase", "Degraded")
		case "Remove":
			setPath(node, "status.status", "removed")
			setPath(node, "status.phase", "Removed")
			s.dropTask(ns, "lvol_migration")
			s.setClusterPhase(ns, getStr(node, "spec.clusterRef"))
		case "Migrate":
			setPath(node, "spec.workerNode", getStr(obj, "spec.migrate.targetWorkerNode"))
			setPath(node, "status.hostname", getStr(obj, "spec.migrate.targetWorkerNode"))
		}
		s.st.Update(nd, ns, node)
	case "storagedeviceops":
		dd, _ := s.st.LookupResource(sbGroup, "storagedevices")
		dev, err := s.st.Get(dd, ns, getStr(obj, "spec.deviceRef"))
		if err != nil {
			return
		}
		switch action {
		case "Fail":
			setPath(dev, "status.phase", "Migrated")
			setPath(dev, "status.deviceStatus", "failed_and_migrated")
		case "Restart":
			setPath(dev, "status.phase", "Online")
			setPath(dev, "status.deviceStatus", "online")
		case "CheckHealth":
			setPath(dev, "status.health.smart.checkedAt", time.Now().UTC().Format(time.RFC3339))
		}
		s.st.Update(dd, ns, dev)
	case "storageclusterops":
		cd, _ := s.st.LookupResource(sbGroup, "storageclusters")
		cluster, err := s.st.Get(cd, ns, getStr(obj, "spec.clusterRef"))
		if err != nil {
			return
		}
		switch action {
		case "Activate", "Start", "Restart", "Expand":
			setPath(cluster, "status.status", "active")
			setPath(cluster, "status.phase", "Online")
		case "Shutdown":
			setPath(cluster, "status.status", "offline")
			setPath(cluster, "status.phase", "Unavailable")
		case "RollingRestart":
			s.dropTask(ns, "balancing_on_restart")
		case "CancelTask":
			id := getStr(obj, "spec.cancelTask.taskID")
			tasks, _ := getPath(cluster, "status.tasks").([]any)
			var kept []any
			for _, t := range tasks {
				if getStr(t, "id") != id {
					kept = append(kept, t)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			setPath(cluster, "status.tasks", kept)
		}
		s.st.Update(cd, ns, cluster)
	case "persistentvolumeops":
		if action != "Migrate" {
			return
		}
		cd, _ := s.st.LookupResource(sbGroup, "storageclusters")
		for _, c := range mustList(s.st, cd) {
			if getStr(c, "status.uuid") != getStr(obj, "status.migration.clusterUUID") {
				continue
			}
			gen, _ := getPath(c, "status.volumeMoveGeneration").(float64)
			setPath(c, "status.volumeMoveGeneration", gen+1)
			s.st.Update(cd, getStr(c, "metadata.namespace"), c)
		}
	}
}

// dropTask removes the first task of a type from every cluster's window in a
// namespace: the control plane finished it.
func (s *Simulator) dropTask(ns, typ string) {
	cd, _ := s.st.LookupResource(sbGroup, "storageclusters")
	for _, c := range mustList(s.st, cd) {
		if getStr(c, "metadata.namespace") != ns {
			continue
		}
		tasks, _ := getPath(c, "status.tasks").([]any)
		kept := []any{}
		dropped := false
		for _, t := range tasks {
			if !dropped && getStr(t, "type") == typ {
				dropped = true
				continue
			}
			kept = append(kept, t)
		}
		setPath(c, "status.tasks", kept)
		s.st.Update(cd, ns, c)
	}
}

// setClusterPhase recomputes a cluster's phase from its nodes once a removal
// is through: Online when every remaining node is, Degraded otherwise.
func (s *Simulator) setClusterPhase(ns, cluster string) {
	cd, _ := s.st.LookupResource(sbGroup, "storageclusters")
	nd, _ := s.st.LookupResource(sbGroup, "storagenodes")
	c, err := s.st.Get(cd, ns, cluster)
	if err != nil {
		return
	}
	phase, status := "Online", "active"
	for _, n := range mustList(s.st, nd) {
		if getStr(n, "spec.clusterRef") != cluster || getStr(n, "status.phase") == "Removed" {
			continue
		}
		if getStr(n, "status.phase") != "Online" {
			phase, status = "Degraded", "degraded"
		}
	}
	setPath(c, "status.phase", phase)
	setPath(c, "status.status", status)
	s.st.Update(cd, ns, c)
}
