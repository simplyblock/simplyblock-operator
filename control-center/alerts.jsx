// ---------------------------------------------------------------------------
// ALERTS — cross-cluster alert state, derived in the console
//
// No API serves alerts. The chart's Grafana rules derive them from the control
// plane's event log and from metrics; this module derives the same conditions
// from what the console already reads: CR status, the event log, pod state,
// operations and tasks, and the round trips to each control plane and API
// server. Each rule names the condition that opens an alert and the one that
// resolves it; the store keeps open instances, promotes them after their
// hold time ("for"), and keeps resolved ones as history, so a resolution is
// shown rather than a row silently vanishing.
//
// A node shutdown an operator asked for is not an alert: the fold over the
// event log exempts planned transitions exactly as the chart's rules do. A
// device that went unavailable within two minutes of its node leaving online
// is folded into the node's alert as a cause, and so is an unhealthy pod on a
// node that is down, so one incident shows once.
//
// Silences live in the viewer's browser until a backend-for-frontend holds
// them for everyone (the export is the shape it will accept).
// ---------------------------------------------------------------------------
const SEVR = {critical: 3, warning: 2, info: 1};
const HEALTHY_NODE = ["online", "in_creation"];
const PLANNED_NODE = ["in_shutdown", "in_removal", "pending_removal", "removed"];
const CASCADE_MS = 120000;
const HOLD = {default: 60000, "node.health-false": 120000, none: 0};
const EVAL_MS = 10000, SAMPLE_MS = 15000, HISTORY_LOG = 2000, HISTORY_MAX = 500, HISTORY_DAYS = 7;

// ---- the fold over the event log ---------------------------------------------
// foldStatus(log, "Storage node status changed from: ") -> {[NodeId]: {state, since, at, planned, order, transitions[]}}
function foldStatus(log, prefix, planned) {
  const out = {};
  (log || []).slice().sort((a, b) => (a.tsMs || 0) - (b.tsMs || 0)).forEach(r => {
    if (r.event !== "STATUS_CHANGE" || !r.message || !r.message.startsWith(prefix)) return;
    const to = (r.message.split(" to: ")[1] || "").replace(/\s*\(\d+\)\s*$/, "").trim();
    const id = r.nodeId || r.clusterId || "cluster";
    const prev = out[id] || {state: "online", since: null, at: 0, planned: false, order: r.storageId, transitions: []};
    const isPlanned = !!planned && ((planned.includes(to)) || (prev.planned && to === "offline"));
    out[id] = {state: to, since: r.ts, at: r.tsMs || 0, planned: isPlanned, order: r.storageId || prev.order,
      transitions: prev.transitions.concat([{to, at: r.tsMs || 0, ts: r.ts, planned: isPlanned}])};
  });
  return out;
}

// ---- rules -----------------------------------------------------------------------
// evaluate(snap) returns the instances open right now; the store does the rest
const inst = (snap, rule, severity, object, title, detail, extra) => Object.assign({
  fingerprint: `${rule}|${snap.clusterId}|${object.kind}/${object.name || object.uuid}`,
  rule, severity, clusterId: snap.clusterId, clusterName: snap.clusterName, ctx: snap.ctx, ns: snap.ns,
  object, title, detail, remedy: (RULE_REMEDY[rule] || ""), causes: [], links: [], since: null
}, extra || {});
const RULE_REMEDY = {
  "cluster.degraded": "Bring the missing nodes back online; the cluster serves I/O with reduced redundancy until then.",
  "cluster.suspended": "More nodes are down than the fault budget allows. Restore nodes, then start the cluster.",
  "cluster.capacity": "Add devices or nodes, or move volumes. Provisioned capacity above the threshold means thin volumes may not get the space they were promised.",
  "node.left-online": "Open the node: if its pod restarted or the worker is down, fix the worker; otherwise restart the storage node.",
  "node.health-false": "The control plane's health check fails for this node. Check its SPDK pod and the fabric interfaces.",
  "device.unavailable": "Check the device's SMART report; if the drive is gone, remove it so its data migrates.",
  "device.read-only": "The device refuses writes. Replace it and remove it from the node.",
  "device.removed": "A device was removed; make sure its data migration finished.",
  "cp.latency": "The control plane answers slowly or not at all. Check the webappapi pod and its FoundationDB.",
  "cp.not-ready": "The control plane reports not ready, or its FoundationDB pods are not running.",
  "jm.backlog": "The journal manager holds more records than its threshold; compression is behind.",
  "jm.compression-error": "Journal compression failed on this node.",
  "pod.unhealthy": "The pod is not running or keeps restarting. Read its log.",
  "ops.failed": "The operation failed; read its message, then retry with a new operation.",
  "slot.error": "Replication of this volume is in error; check the slot's message and the pair's connectivity."
};
const nodeName = (snap, id) => (snap.nodeById[id] || {}).hostname || id;
const deviceName = (snap, id) => { const d = snap.deviceById[id]; return d ? (d.serial || d.name || id) : id; };

const ALERT_RULES = {
  "cluster.degraded": {severity: "warning", scope: "cluster", evaluate: snap => {
    const planned = Object.values(snap.nodeFold).some(f => f.planned && !HEALTHY_NODE.includes(f.state));
    if (snap.cluster.status !== "degraded" || planned) return [];
    return [inst(snap, "cluster.degraded", "warning", {kind: "cluster", name: snap.clusterName, uuid: snap.clusterId},
      `Cluster ${snap.clusterName} is degraded`, `${snap.cluster.counts.nodes - snap.cluster.counts.nodesOnline} of ${snap.cluster.counts.nodes} storage nodes are not online; no planned shutdown explains it.`)];
  }},
  "cluster.suspended": {severity: "critical", scope: "cluster", evaluate: snap => snap.cluster.status !== "suspended" ? []
    : [inst(snap, "cluster.suspended", "critical", {kind: "cluster", name: snap.clusterName, uuid: snap.clusterId}, `Cluster ${snap.clusterName} is suspended`, "I/O is halted until enough nodes return.")]},
  "cluster.capacity": {severity: "warning", scope: "cluster", evaluate: snap => {
    const out = [], c = snap.cluster, th = c.thresholds || {};
    const warnCap = (th.warn || {}).capacity || 80, critCap = (th.crit || {}).capacity || 95;
    const warnProv = (th.warn || {}).provisionedCapacity || 100, critProv = (th.crit || {}).provisionedCapacity || 120;
    const util = c.capacity.total ? c.capacity.used / c.capacity.total * 100 : null;
    const prov = c.capacity.total && c.provisioned ? c.provisioned / c.capacity.total * 100 : null;
    const hyst = 2;
    const open = (kind, pct, warn, crit, wasOpen) => {
      const lvl = pct >= crit ? "critical" : pct >= warn - (wasOpen ? hyst : 0) ? "warning" : null;
      if (!lvl) return;
      out.push(inst(snap, "cluster.capacity", lvl, {kind: "capacity", name: kind, uuid: snap.clusterId},
        `${kind === "absolute" ? "Capacity" : "Provisioned capacity"} of ${snap.clusterName} at ${pct.toFixed(0)}%`,
        `${kind === "absolute" ? "Used" : "Provisioned"} capacity is ${pct.toFixed(1)}%, ${lvl === "critical" ? "past the critical" : "past the warning"} threshold of ${lvl === "critical" ? crit : warn}%.`, {value: pct}));
    };
    const wasOpen = kind => snap.wasOpen(`cluster.capacity|${snap.clusterId}|capacity/${kind}`);
    if (util !== null) open("absolute", util, warnCap, critCap, wasOpen("absolute"));
    if (prov !== null) open("provisioned", prov, warnProv, critProv, wasOpen("provisioned"));
    // the event-log fallback when no live figure exists: the newest CAPACITY record per kind, still fresh
    if (util === null) {
      const now = snap.now;
      const recs = snap.log.filter(r => r.event === "CAPACITY" && (r.level === "warning" || r.level === "critical"));
      ["absolute", "provisioned"].forEach(kind => {
        const r = recs.filter(x => (/provisioned/i.test(x.message)) === (kind === "provisioned")).sort((a, b) => b.tsMs - a.tsMs)[0];
        if (!r) return;
        const fresh = now - r.tsMs <= (kind === "absolute" && r.level === "critical" ? 1200000 : 180000);
        if (!fresh) return;
        const m = /reached:\s*([\d.]+)%/.exec(r.message);
        out.push(inst(snap, "cluster.capacity", r.level, {kind: "capacity", name: kind, uuid: snap.clusterId},
          `${kind === "absolute" ? "Capacity" : "Provisioned capacity"} of ${snap.clusterName}${m ? ` at ${m[1]}%` : ""}`, r.message, {value: m ? Number(m[1]) : null}));
      });
    }
    return out;
  }},
  "node.left-online": {severity: "critical", scope: "node", evaluate: snap => {
    const out = [];
    const seen = new Set();
    Object.entries(snap.nodeFold).forEach(([id, f]) => {
      seen.add(id);
      if (HEALTHY_NODE.includes(f.state) || f.planned) return;
      if (!snap.nodeById[id]) return;
      out.push(inst(snap, "node.left-online", "critical", {kind: "node", name: nodeName(snap, id), uuid: id},
        `Storage node ${nodeName(snap, id)} left the online state`, `The node is ${f.state} since ${f.since ? fmtAgo(f.since) : "an unknown time"}; nothing marks the transition as planned.`, {since: f.since}));
    });
    // no event log: the CR status decides, and an active operation marks it planned
    snap.nodes.forEach(n => {
      if (seen.has(n.id) || HEALTHY_NODE.includes(n.status) || PLANNED_NODE.includes(n.status) || n.activeOpsRef) return;
      out.push(inst(snap, "node.left-online", "critical", {kind: "node", name: n.hostname, uuid: n.id},
        `Storage node ${n.hostname} is ${n.status}`, `The StorageNode reports ${n.status} and no operation is running against it.`));
    });
    return out;
  }},
  "node.health-false": {severity: "warning", scope: "node", evaluate: snap => snap.nodes.filter(n => n.status === "online" && n.health === false)
    .map(n => inst(snap, "node.health-false", "warning", {kind: "node", name: n.hostname, uuid: n.id}, `Health check fails on ${n.hostname}`, "The control plane's health check reports false while the node is online."))},
  "device.unavailable": {severity: "warning", scope: "device", evaluate: snap => {
    const out = [];
    const nodeDown = Object.values(snap.nodeFold).filter(f => !HEALTHY_NODE.includes(f.state)).map(f => f.at);
    const seen = new Set();
    Object.entries(snap.deviceFold).forEach(([id, f]) => {
      seen.add(id);
      if (f.state !== "unavailable") return;
      const cascade = nodeDown.some(t => t <= f.at && f.at - t <= CASCADE_MS);
      if (cascade) return;
      out.push(inst(snap, "device.unavailable", "warning", {kind: "device", name: deviceName(snap, id), uuid: id, order: f.order},
        `Device ${deviceName(snap, id)} is unavailable`, `Device order ${f.order || "?"} went unavailable ${f.since ? fmtAgo(f.since) : ""} with its node online.`, {since: f.since}));
    });
    snap.devices.forEach(d => {
      if (seen.has(d.id) || d.status !== "unavailable") return;
      const node = snap.nodeById[d.nodeId];
      if (node && !HEALTHY_NODE.includes(node.status)) return;
      out.push(inst(snap, "device.unavailable", "warning", {kind: "device", name: deviceName(snap, d.id), uuid: d.id}, `Device ${deviceName(snap, d.id)} is unavailable`, "The StorageDevice reports unavailable while its node is online."));
    });
    return out;
  }},
  "device.read-only": {severity: "warning", scope: "device", evaluate: snap => snap.devices.filter(d => d.status === "read_only")
    .map(d => inst(snap, "device.read-only", "warning", {kind: "device", name: deviceName(snap, d.id), uuid: d.id}, `Device ${deviceName(snap, d.id)} is read-only`, "The device refuses writes."))},
  "device.removed": {severity: "critical", scope: "device", evaluate: snap => Object.entries(snap.deviceFold)
    .filter(([, f]) => f.state === "removed" && snap.now - f.at <= 86400000)
    .map(([id, f]) => inst(snap, "device.removed", "critical", {kind: "device", name: deviceName(snap, id), uuid: id, order: f.order}, `Device ${deviceName(snap, id)} was removed`, `Removed ${fmtAgo(f.since)}; the alert ages out after a day.`, {since: f.since}))},
  "cp.latency": {severity: "warning", scope: "cp", evaluate: snap => {
    const out = [];
    const judge = (samples, kind, name) => {
      const last = samples.slice(-3);
      if (last.length < 2) return;
      const fails = last.slice(-2).every(s => !s.ok);
      const slow = last.length === 3 && last.every(s => s.ok && s.ms > 1000);
      if (fails) out.push(inst(snap, "cp.latency", "critical", {kind, name}, `${kind === "apiserver" ? "API server" : "Control plane"} of ${name} is unreachable`, `${last.slice(-2).length} consecutive probes failed.`));
      else if (slow) out.push(inst(snap, "cp.latency", "warning", {kind, name}, `${kind === "apiserver" ? "API server" : "Control plane"} of ${name} answers slowly`, `The last three probes took ${last.map(s => s.ms + " ms").join(", ")}.`, {value: last[last.length - 1].ms}));
    };
    judge(snap.cpSamples, "controlplane", snap.clusterName);
    judge(snap.apiSamples, "apiserver", snap.ctx);
    return out;
  }},
  "cp.not-ready": {severity: "critical", scope: "cp", evaluate: snap => {
    const out = [];
    const cp = snap.controlPlane;
    if (cp && cp.phase && cp.phase !== "Ready") out.push(inst(snap, "cp.not-ready", "critical", {kind: "controlplane", name: cp.name}, `Control plane ${cp.name} is ${cp.phase}`, cp.message || "The ControlPlane reports a phase other than Ready."));
    const fdb = snap.pods.filter(p => p.group === "fdb" && !(p.phase === "Running" && p.ready));
    if (fdb.length) out.push(inst(snap, "cp.not-ready", "critical", {kind: "fdb", name: "foundationdb"}, "FoundationDB pods are not ready", `${fdb.length} FDB pod${fdb.length === 1 ? "" : "s"} not Running and Ready: ${fdb.map(p => p.name).slice(0, 3).join(", ")}.`, {causes: fdb.map(p => ({kind: "Pod", name: p.name, reason: p.phase}))}));
    return out;
  }},
  "jm.backlog": {severity: "critical", scope: "node", evaluate: snap => jmRule(snap, /JM_COMPRESSION_BACKLOG|records_threshold|JM_RECORDS/i, "jm.backlog", "Journal backlog on")},
  "jm.compression-error": {severity: "critical", scope: "node", evaluate: snap => jmRule(snap, /JM_COMPRESSION_ERROR|compression failed|JM_COMP_ERR/i, "jm.compression-error", "Journal compression failed on")},
  "pod.unhealthy": {severity: "warning", scope: "pod", evaluate: snap => snap.pods.filter(p => p.group !== "fdb").map(p => {
    const notReady = !(p.phase === "Running" && p.ready);
    const restarts = snap.restartBurst(p.name, p.restarts);
    if (!notReady && !restarts) return null;
    const node = p.nodeId ? snap.nodeById[p.nodeId] : null;
    const sev = node && node.status === "online" && (p.group === "spdk" || p.group === "node-api") ? "critical" : "warning";
    const reason = (p.raw.status.containerStatuses || []).map(c => c.state && (c.state.waiting || c.state.terminated) ? (c.state.waiting || c.state.terminated).reason : null).filter(Boolean)[0] || p.phase;
    return inst(snap, "pod.unhealthy", sev, {kind: "pod", name: p.name, ns: p.namespace, uuid: p.nodeId || null},
      `Pod ${p.name} is ${notReady ? "not ready" : "restarting"}`, notReady ? `Phase ${p.phase}, ${reason}.` : `${restarts} restarts in the last 30 minutes.`,
      {node: p.nodeId || null, causes: [{kind: "Pod", name: p.name, reason}]});
  }).filter(Boolean)},
  "ops.failed": {severity: "warning", scope: "ops", evaluate: snap => {
    const out = [];
    snap.ops.filter(o => o.phase === "Failed").forEach(o => out.push(inst(snap, "ops.failed", "warning", {kind: o.kind, name: o.name, ns: o.ns},
      `${o.kind} ${o.name} failed`, o.message || `Action ${o.action} on ${o.targetName || "the cluster"} ended in Failed.`, {since: o.completedAt || o.startedAt, opKey: o.id})));
    snap.tasks.forEach(t => {
      const exhausted = t.maxRetry > 0 && t.retry >= t.maxRetry && t.status !== "done";
      const failedDone = t.status === "done" && !t.canceled && /fail|error/i.test(t.result || "");
      if (!exhausted && !failedDone) return;
      out.push(inst(snap, "ops.failed", "warning", {kind: "task", name: t.id, uuid: t.nodeId || t.deviceId || null},
        `Task ${t.fn} ${exhausted ? "exhausted its retries" : "failed"}`, (t.result || "").slice(0, 200) || `${t.retry}/${t.maxRetry} retries`, {taskId: t.id}));
    });
    return out;
  }},
  "slot.error": {severity: "critical", scope: "slot", evaluate: snap => snap.slots.filter(s => (s.status || {}).state === "error")
    .map(s => inst(snap, "slot.error", "critical", {kind: "ReplicationSlot", name: s.metadata.name, ns: s.metadata.namespace}, `Replication slot ${s.metadata.name} is in error`, (s.status || {}).message || "The slot reports state error."))}
};
function jmRule(snap, re, rule, title) {
  const latest = {};
  snap.log.filter(r => re.test(r.event) || re.test(r.message)).forEach(r => { const id = r.nodeId || "cluster"; if (!latest[id] || latest[id].tsMs < r.tsMs) latest[id] = r; });
  return Object.entries(latest).filter(([, r]) => snap.now - r.tsMs <= 3600000)
    .map(([id, r]) => inst(snap, rule, "critical", {kind: "node", name: nodeName(snap, id), uuid: id}, `${title} ${nodeName(snap, id)}`, r.message, {since: r.ts}));
}

// ---- snapshot of one cluster ----------------------------------------------------------
async function snapshotOf(c, store_) {
  const ctx = c.ctx;
  const [w, nodes, devices, pods, ops, tasks, log, slots] = await Promise.all([
    sources.world(ctx), api.nodes(c.id).catch(() => []), sources.devices(ctx).then(ds => ds.filter(d => d.cluster_id === c.id).map(normDevice)).catch(() => []),
    sources.clusterPods(ctx, c).catch(() => []), api.operations(c.id).catch(() => []),
    hasControlPlane(ctx) ? cp.tasks(ctx, c.id).catch(() => []) : Promise.resolve([]),
    hasControlPlane(ctx) ? cp.logs(ctx, c.id, HISTORY_LOG).catch(() => null) : Promise.resolve(null),
    store.read(ctx, "ReplicationSlot").catch(() => [])
  ]);
  const cpCR = w.controlPlanes[0] || null;
  const nodeById = {}; nodes.forEach(n => { nodeById[n.id] = n; });
  const deviceById = {}; devices.forEach(d => { deviceById[d.id] = d; });
  const logRecords = log || [];
  return {
    clusterId: c.id, clusterName: c.name, ctx, ns: c.ns, cluster: c, nodes, devices, nodeById, deviceById, pods, ops, tasks, log: logRecords, logMissing: log === null,
    slots: slots.filter(s => w.clusterByUUID.has(String((s.spec || {}).volumeID || "").split(":")[0]) ? String((s.spec || {}).volumeID || "").split(":")[0] === c.id : true),
    nodeFold: foldStatus(logRecords, "Storage node status changed from: ", PLANNED_NODE),
    deviceFold: foldStatus(logRecords, "Device status changed from: "),
    clusterFold: foldStatus(logRecords, "Cluster status changed from: "),
    controlPlane: cpCR ? {name: cpCR.metadata.name, phase: (cpCR.status || {}).phase, message: (cpCR.status || {}).message} : null,
    cpSamples: store_.samples.cp[c.id] || [], apiSamples: store_.samples.api[ctx] || [],
    now: Date.now(), wasOpen: fp => store_.instances.has(fp), restartBurst: (pod, n) => store_.restartBurst(pod, n)
  };
}

// ---- history from the log ------------------------------------------------------------
// Each transition into a bad state opens an episode; the return to online
// closes it. Only closed episodes are history; the open ones are live alerts.
function historyFromLog(snap) {
  const out = [];
  const walk = (fold, kind, name, badOf, rule, sev) => Object.entries(fold).forEach(([id, f]) => {
    let open = null;
    f.transitions.forEach(t => {
      const bad = badOf(t);
      if (bad && !open) open = t;
      else if (!bad && open) {
        out.push(Object.assign(inst(snap, rule, sev, {kind, name: name(id), uuid: id}, `${kind === "node" ? "Storage node" : kind === "device" ? "Device" : "Cluster"} ${name(id)} was ${open.to}`, `From ${open.ts} until ${t.ts}.`),
          {firstSeen: open.at, lastSeen: t.at, resolvedAt: t.at, resolvedBy: "condition", source: "history", silence: null}));
        open = null;
      }
    });
  });
  walk(snap.nodeFold, "node", id => nodeName(snap, id), t => !HEALTHY_NODE.includes(t.to) && !t.planned, "node.left-online", "critical");
  walk(snap.deviceFold, "device", id => deviceName(snap, id), t => t.to === "unavailable" || t.to === "read_only", "device.unavailable", "warning");
  walk(snap.clusterFold, "cluster", () => snap.clusterName, t => t.to === "degraded" || t.to === "suspended", "cluster.degraded", "warning");
  return out;
}

// ---- the store ---------------------------------------------------------------------------
const SILENCE_KEY = "sb.silences";
const alertStore = {
  ready: false, instances: new Map(), history: [], silences: [], samples: {cp: {}, api: {}}, restarts: {}, lastEval: {}, listeners: new Set(),
  pending: new Map(), clusters: [], timer: null, sampler: null, historyDone: new Set(), version: 0,
  load() { try { this.silences = JSON.parse(localStorage.getItem(SILENCE_KEY) || "[]"); } catch (e) { this.silences = []; } },
  save() { try { localStorage.setItem(SILENCE_KEY, JSON.stringify(this.silences)); } catch (e) {} },
  emit() { this.version++; this.listeners.forEach(f => { try { f(); } catch (e) { console.error(e); } }); },
  subscribe(f) { this.listeners.add(f); return () => this.listeners.delete(f); },
  restartBurst(pod, n) {
    const ring = this.restarts[pod] || [];
    const now = Date.now();
    const last = ring[ring.length - 1];
    if (!last || last.n !== n) ring.push({n, at: now});
    this.restarts[pod] = ring.filter(x => now - x.at <= 1800000).slice(-10);
    const r = this.restarts[pod];
    return r.length >= 2 ? r[r.length - 1].n - r[0].n : 0;
  },
  isSilenced(a) {
    const now = Date.now();
    return this.silences.find(s => (!s.until || Date.parse(s.until) > now) && (
      (s.matcher.fingerprint && s.matcher.fingerprint === a.fingerprint)
      || (s.matcher.rule && s.matcher.rule === a.rule && (!s.matcher.clusterId || s.matcher.clusterId === a.clusterId)
        && (!s.matcher.objectName || (s.matcher.objectName === a.object.name && s.matcher.objectKind === a.object.kind))))) || null;
  },
  silence(matcher, opts) {
    const o = opts || {};
    const s = {id: Date.now().toString(36) + Math.random().toString(36).slice(2, 6), matcher, until: o.until || null, by: ((window.access || {}).state || {}).user || "viewer", note: o.note || "", createdAt: new Date().toISOString()};
    this.silences.push(s); this.save(); this.emit(); return s;
  },
  unsilence(id) {
    this.silences = this.silences.filter(s => s.id !== id && s.matcher.fingerprint !== id); this.save(); this.emit();
  },
  export() { return JSON.stringify(this.silences); },
  apply(snap, found) {
    const now = Date.now();
    const seen = new Set();
    found.forEach(f => {
      seen.add(f.fingerprint);
      const cur = this.instances.get(f.fingerprint);
      if (cur) { Object.assign(cur, {severity: f.severity, title: f.title, detail: f.detail, value: f.value, causes: f.causes, lastSeen: now}); return; }
      const hold = HOLD[f.rule] !== undefined ? HOLD[f.rule] : (f.rule.startsWith("cp.") || f.rule.startsWith("ops.") || f.rule.startsWith("jm.") || f.rule === "device.removed" ? 0 : HOLD.default);
      const p = this.pending.get(f.fingerprint);
      if (!p) { this.pending.set(f.fingerprint, {since: now, inst: f}); if (hold > 0) return; }
      else if (now - p.since < hold) return;
      this.pending.delete(f.fingerprint);
      this.instances.set(f.fingerprint, Object.assign(f, {firstSeen: p ? p.since : now, lastSeen: now, resolvedAt: null, resolvedBy: null, source: "live"}));
    });
    // whatever this cluster's rules no longer report has resolved
    Array.from(this.pending.keys()).filter(fp => fp.split("|")[1] === snap.clusterId && !seen.has(fp)).forEach(fp => this.pending.delete(fp));
    Array.from(this.instances.values()).filter(a => a.clusterId === snap.clusterId && !seen.has(a.fingerprint)).forEach(a => {
      this.instances.delete(a.fingerprint);
      this.history.unshift(Object.assign(a, {resolvedAt: now, resolvedBy: a.rule === "device.removed" ? "aged" : "condition"}));
    });
    this.trimHistory();
  },
  trimHistory() {
    const cutoff = Date.now() - HISTORY_DAYS * 86400000;
    this.history = this.history.filter(a => (a.resolvedAt || 0) >= cutoff).slice(0, HISTORY_MAX);
  },
  // the cascade: a device or pod that went bad while its node is down is the node's cause
  reconcile() {
    const nodeAlerts = Array.from(this.instances.values()).filter(a => a.rule === "node.left-online");
    nodeAlerts.forEach(n => { n.causes = n.causes.filter(c => c.kind !== "Pod" && c.kind !== "device"); });
    Array.from(this.instances.values()).forEach(a => {
      if (a.rule !== "pod.unhealthy" || !a.node) { a.folded = false; return; }
      const n = nodeAlerts.find(x => x.clusterId === a.clusterId && x.object.uuid === a.node);
      if (n) { n.causes.push(a.causes[0] || {kind: "Pod", name: a.object.name}); a.folded = true; } else a.folded = false;
    });
  },
  async evaluateCluster(c) {
    try {
      const snap = await snapshotOf(c, this);
      const found = [];
      Object.values(ALERT_RULES).forEach(r => { try { found.push(...r.evaluate(snap)); } catch (e) { console.error("alert rule", e); } });
      this.apply(snap, found);
      if (!this.historyDone.has(c.id) && snap.log.length) {
        this.historyDone.add(c.id);
        this.history = this.history.concat(historyFromLog(snap)).sort((a, b) => (b.resolvedAt || 0) - (a.resolvedAt || 0));
        this.trimHistory();
      }
      this.lastEval[c.id] = {at: Date.now(), ok: true, logMissing: snap.logMissing};
    } catch (e) {
      // a failed read resolves nothing (the chart's rules do the same on an execution error)
      this.lastEval[c.id] = {at: Date.now(), ok: false, error: e};
    }
  },
  async evaluate() {
    if (this.evaluating) return;
    this.evaluating = true;
    try {
      this.clusters = await api.clusters().catch(() => this.clusters);
      for (let i = 0; i < this.clusters.length; i += 4) await Promise.all(this.clusters.slice(i, i + 4).map(c => this.evaluateCluster(c)));
      this.reconcile();
      this.ready = true;
      this.emit();
    } finally { this.evaluating = false; }
  },
  async sample() {
    const push = (map, key, s) => { map[key] = (map[key] || []).concat([s]).slice(-5); };
    await Promise.all(CONTEXTS.map(async c => {
      const t0 = performance.now();
      try { await k8s.version(c.name); push(this.samples.api, c.name, {ok: true, ms: Math.round(performance.now() - t0), at: Date.now()}); }
      catch (e) { push(this.samples.api, c.name, {ok: false, ms: Math.round(performance.now() - t0), at: Date.now()}); }
    }));
    const live = this.clusters.filter(c => hasControlPlane(c.ctx));
    for (let i = 0; i < live.length; i += 8) await Promise.all(live.slice(i, i + 8).map(async c => push(this.samples.cp, c.id, await cp.health(c.ctx))));
  },
  start() {
    if (this.timer) return;
    this.load();
    const tick = () => this.evaluate().finally(() => { this.timer = setTimeout(tick, document.hidden ? EVAL_MS * 3 : EVAL_MS); });
    tick();
    const sampleTick = () => this.sample().finally(() => { this.sampler = setTimeout(sampleTick, SAMPLE_MS); });
    sampleTick();
  },
  // ---- what the rest of the console reads ----
  all() { return Array.from(this.instances.values()).filter(a => !a.folded).map(a => Object.assign(a, {silence: this.isSilenced(a), silenced: !!this.isSilenced(a), scope: (ALERT_RULES[a.rule] || {}).scope, id: a.fingerprint})); },
  forCluster(id) { return this.all().filter(a => a.clusterId === id); },
  forObject(ref) {
    const r = ref || {};
    return this.all().filter(a => (r.kind === "cluster" && a.clusterId === r.id)
      || (a.object.uuid && a.object.uuid === r.id) || (a.node && a.node === r.id)
      || (a.object.name && a.object.name === r.name && a.ctx === r.ctx));
  },
  countsFor(clusterId) {
    const as = this.forCluster(clusterId);
    return {critical: as.filter(a => a.severity === "critical" && !a.silenced).length, warning: as.filter(a => a.severity === "warning" && !a.silenced).length, silenced: as.filter(a => a.silenced).length};
  },
  latencyOf(clusterId) { const s = this.samples.cp[clusterId]; return s && s.length ? s[s.length - 1].ms : null; },
  recent(hours) { const cutoff = Date.now() - (hours || 24) * 3600000; return this.history.filter(a => (a.resolvedAt || 0) >= cutoff); }
};
window.alerts = alertStore;
if (!(window.SB_CONFIG || {}).mode || window.SB_CONFIG.mode !== "dr") setTimeout(() => alertStore.start(), 1500);

function useAlerts() {
  const [, force] = React.useState(0);
  React.useEffect(() => alertStore.subscribe(() => force(n => n + 1)), []);
  return alertStore;
}

// ---- views ---------------------------------------------------------------------------------
const SEV_C = {critical: "var(--bad)", warning: "var(--warn)", info: "var(--info)"};
function alertLinks(a, nav) {
  const links = [];
  const o = a.object || {};
  if (o.kind === "node" && o.uuid) links.push({label: "Open node", icon: "node", go: () => nav.openNode(a.clusterId, o.uuid)});
  if (o.kind === "device" && o.uuid) { const d = REG[o.uuid]; links.push({label: "Open device", icon: "device", go: () => d ? nav.openDevice(a.clusterId, d.nodeId, o.uuid) : nav.openCluster(a.clusterId)}); }
  if (o.kind === "pod" && a.node) links.push({label: "Open node", icon: "node", go: () => nav.openNode(a.clusterId, a.node)});
  if (["cluster", "capacity", "controlplane", "fdb"].includes(o.kind)) links.push({label: "Open cluster", icon: "cluster", go: () => nav.openCluster(a.clusterId)});
  if (nav.logs && a.clusterId) {
    const from = (a.firstSeen || Date.now()) - 300000, to = (a.resolvedAt || Date.now()) + 300000;
    links.push({label: "Logs ±5 min", icon: "list", go: () => nav.logs(a.clusterId, Object.assign({from, to}, o.kind === "node" ? {snode: o.uuid} : o.kind === "pod" ? {pod: [o.name]} : {}))});
  }
  if (nav.monitor && (a.opKey || a.taskId)) links.push({label: "Operation", icon: "clock", go: () => nav.monitor({cluster: a.clusterId, q: a.object.name})});
  return links;
}
function silenceDialog(a, store_) {
  return {
    title: `Silence "${a.title}"?`,
    desc: "A silenced alert keeps evaluating and still resolves on its own; it is only left out of the counts and the bell. The silence is kept in this browser.",
    fields: [
      {k: "duration", label: "For", type: "select", def: "resolved", options: [{v: "resolved", l: "until it resolves"}, {v: "1", l: "1 hour"}, {v: "8", l: "8 hours"}, {v: "24", l: "24 hours"}, {v: "168", l: "7 days"}]},
      {k: "scope", label: "Scope", type: "select", def: "instance", options: [{v: "instance", l: "this alert"}, {v: "object", l: `every ${a.rule} alert on ${a.object.name}`}, {v: "cluster", l: `every ${a.rule} alert on ${a.clusterName}`}]},
      {k: "note", label: "Note", placeholder: "why, and who to ask"}
    ],
    confirm: "Silence",
    run: v => Promise.resolve(store_.silence(v.scope === "instance" ? {fingerprint: a.fingerprint} : v.scope === "object" ? {rule: a.rule, clusterId: a.clusterId, objectKind: a.object.kind, objectName: a.object.name} : {rule: a.rule, clusterId: a.clusterId},
      {until: v.duration === "resolved" ? null : new Date(Date.now() + Number(v.duration) * 3600000).toISOString(), note: v.note}))
  };
}
function AlertRow({a, nav, showCluster}) {
  const store_ = alertStore;
  const resolved = !!a.resolvedAt;
  const links = alertLinks(a, nav);
  return (
    <div className={"alertrow " + a.severity + (a.silenced ? " ack" : "") + (resolved ? " resolved" : "")}>
      <span className="asev" title={a.severity}><i></i></span>
      <div style={{minWidth: 0, flex: 1}}>
        <div className="atitle">{a.title}
          {a.silenced && <span className="badge" title={a.silence.note || ""}>silenced{a.silence.until ? ` until ${fmtDate(a.silence.until)}` : ""}</span>}
          {resolved && <span className="badge" style={{color: "var(--ok)"}}>resolved {fmtAgo(new Date(a.resolvedAt).toISOString())}</span>}
          {a.source === "history" && <span className="badge">from the event log</span>}
        </div>
        <div className="adetail">{a.detail}</div>
        <div className="ameta">
          {showCluster && <button className="lab link" onClick={() => nav.openCluster(a.clusterId)}><Icon n="cluster" s={10} />{a.clusterName}<i style={{marginLeft: 4}}>{a.ctx}</i></button>}
          <span className="lab"><i>rule</i>{a.rule}</span>
          <span className="lab"><i>{resolved ? "opened" : "since"}</i>{fmtAgo(new Date(a.firstSeen || Date.now()).toISOString())}</span>
          {(a.causes || []).slice(0, 4).map((c, i) => <span key={i} className="lab warn" title="folded into this alert as a cause"><i>{c.kind}</i>{c.name}{c.reason ? ` · ${c.reason}` : ""}</span>)}
          {(a.causes || []).length > 4 && <span className="lab"><i>+</i>{a.causes.length - 4} more</span>}
          {links.map(l => <button key={l.label} className="lab link" onClick={l.go}><Icon n={l.icon} s={10} />{l.label}</button>)}
        </div>
        {a.remedy && !resolved && <div className="aremedy"><Icon n="chev" s={10} />{a.remedy}</div>}
      </div>
      {!resolved && <div className="aacts">
        {a.silenced
          ? <button className="chip" onClick={() => { store_.unsilence(a.silence.id); window.__toast("Alert unsilenced"); }}>Unsilence</button>
          : <button className="chip" onClick={() => window.__ui.dialog(silenceDialog(a, store_), {kind: "alert", id: a.fingerprint})}><Icon n="check" s={11} />Silence</button>}
      </div>}
    </div>
  );
}
function AlertList({items, groupBy, nav, showCluster}) {
  if (!items.length) return <div className="lmsg">Nothing to show.</div>;
  if (!groupBy) return <div className="alertlist">{items.map(a => <AlertRow key={a.fingerprint + (a.resolvedAt || "")} a={a} nav={nav} showCluster={showCluster} />)}</div>;
  const keyOf = {cluster: a => a.clusterName, site: a => ((REG[a.clusterId] || {}).sites || []).map(s => s.value).join(", ") || "no site", severity: a => a.severity, rule: a => a.rule}[groupBy];
  const groups = new Map();
  items.forEach(a => { const k = keyOf(a); if (!groups.has(k)) groups.set(k, []); groups.get(k).push(a); });
  return <>{Array.from(groups.entries()).map(([k, as]) => <div key={k}>
    <div className="grouplbl">{k}<span className="count" style={{marginLeft: 8}}>{as.length}</span></div>
    <div className="card"><div className="alertlist">{as.map(a => <AlertRow key={a.fingerprint + (a.resolvedAt || "")} a={a} nav={nav} showCluster={showCluster && groupBy !== "cluster"} />)}</div></div>
  </div>)}</>;
}
const sortAlerts = (x, y) => (x.silenced - y.silenced) || SEVR[y.severity] - SEVR[x.severity] || (y.firstSeen || 0) - (x.firstSeen || 0);

// the section-level view
function AlertsView({seg, nav}) {
  const store_ = useAlerts();
  const p = seg.p || {};
  const [group, setGroup] = useLocal("sb.alerts.group", p.group || "cluster");
  const [q, setQ] = React.useState(p.q || "");
  const [sev, setSev] = React.useState(p.sev ? [p.sev] : []);
  const [showSilenced, setShowSilenced] = React.useState(false);
  const [showResolved, setShowResolved] = React.useState(!!p.show);
  const live = store_.all().filter(a => !p.cluster || a.clusterId === p.cluster);
  const resolved = showResolved ? store_.recent(24).filter(a => !p.cluster || a.clusterId === p.cluster) : [];
  const match = a => (showSilenced || !a.silenced) && (!sev.length || sev.includes(a.severity))
    && (!q || `${a.title} ${a.detail} ${a.rule} ${a.clusterName} ${a.object.name}`.toLowerCase().includes(q.toLowerCase()));
  const rows = live.filter(match).sort(sortAlerts);
  const hist = resolved.filter(match);
  const counts = {critical: live.filter(a => a.severity === "critical" && !a.silenced).length, warning: live.filter(a => a.severity === "warning" && !a.silenced).length, silenced: live.filter(a => a.silenced).length};
  const clustersHit = new Set(live.filter(a => !a.silenced).map(a => a.clusterId)).size;
  const cpDown = live.filter(a => a.rule === "cp.latency" && a.severity === "critical").length;
  const stale = Object.entries(store_.lastEval).filter(([, e]) => !e.ok);
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>Alerts{p.cluster && REG[p.cluster] ? ` · ${REG[p.cluster].name}` : ""}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Derived every {EVAL_MS / 1000} s from cluster, node, device, pod, operation, and control plane state across every context. A row resolves when its condition clears and stays visible as history for a day.</div></div>
        {p.cluster && <button className="chip" onClick={() => nav.alerts()}>All clusters</button>}</div>
      {!store_.ready && <div className="banner"><Icon n="refresh" s={15} /><span>First evaluation in progress…</span></div>}
      {stale.length > 0 && <div className="banner"><Icon n="alert" s={15} /><span><b>Source error.</b> {stale.map(([id, e]) => `${(REG[id] || {}).name || id}: ${e.error && e.error.message}`).join(" · ")} — alerts of these clusters are not resolved until the read works again.</span></div>}
      <div className="stats">
        <Stat k="Critical" v={counts.critical} c={counts.critical ? "var(--bad)" : "var(--ok)"} s={counts.critical ? "open now" : "none open"} />
        <Stat k="Warning" v={counts.warning} c={counts.warning ? "var(--warn)" : "var(--ok)"} s={counts.warning ? "open now" : "none open"} />
        <Stat k="Silenced" v={counts.silenced} s="left out of the bell" />
        <Stat k="Clusters affected" v={clustersHit} s={`of ${store_.clusters.length}`} />
        <Stat k="Control planes down" v={cpDown} c={cpDown ? "var(--bad)" : undefined} s="unreachable from the console" />
        <Stat k="Resolved, 24 h" v={store_.recent(24).length} s="including the event log's history" />
      </div>
      <div className="card" style={{marginBottom: 12}}><div className="ptools">
        <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" /><input value={q} placeholder="Search title, rule, cluster, object…" onChange={e => setQ(e.target.value)} /></div>
        <div className="chips">
          {["critical", "warning"].map(s => <button key={s} className={"chip" + (sev.includes(s) ? " on" : "")} onClick={() => setSev(sev.includes(s) ? sev.filter(v => v !== s) : [...sev, s])}><Dot c={SEV_C[s]} />{s}<span className="n">{counts[s]}</span></button>)}
          <button className={"chip" + (showSilenced ? " on" : "")} onClick={() => setShowSilenced(!showSilenced)}><Icon n="check" s={10} />include silenced</button>
          <button className={"chip" + (showResolved ? " on" : "")} onClick={() => setShowResolved(!showResolved)}><Icon n="clock" s={10} />recently resolved</button>
        </div>
        <div className="spacer"></div>
        <div className="seg">{["cluster", "site", "severity", "rule"].map(k => <button key={k} className={group === k ? "on" : ""} onClick={() => setGroup(k)}>{k}</button>)}</div>
        <span className="count">{rows.length}</span><span className="live"><i></i>live</span>
      </div></div>
      {rows.length ? <AlertList items={rows} groupBy={group} nav={nav} showCluster /> : <div className="empty"><Icon n="bell" s={24} c="var(--ok)" /><b>Nothing open</b><span>{store_.ready ? "Every watched condition is false right now." : "Waiting for the first evaluation."}</span></div>}
      {showResolved && <>
        <div className="sech" style={{marginTop: 16}}><h2>Resolved in the last 24 hours</h2><span className="ln"></span><span className="count">{hist.length}</span></div>
        {hist.length ? <div className="card"><AlertList items={hist} nav={nav} showCluster /></div> : <div className="lmsg">No resolution in the last 24 hours.</div>}
      </>}
    </div>
  );
}
// the cluster-scoped panel used by ClusterDetail and the control plane view
function AlertsScoped({cluster, nav}) {
  const store_ = useAlerts();
  const items = (cluster ? store_.forCluster(cluster.id) : store_.all().filter(a => a.scope === "cp")).sort(sortAlerts);
  const hist = store_.recent(24).filter(a => !cluster || a.clusterId === cluster.id);
  return (
    <>
      <div className="stats" style={{marginBottom: 12}}>
        <Stat k="Critical" v={items.filter(a => a.severity === "critical" && !a.silenced).length} c={items.some(a => a.severity === "critical" && !a.silenced) ? "var(--bad)" : "var(--ok)"} />
        <Stat k="Warning" v={items.filter(a => a.severity === "warning" && !a.silenced).length} c={items.some(a => a.severity === "warning" && !a.silenced) ? "var(--warn)" : "var(--ok)"} />
        <Stat k="Silenced" v={items.filter(a => a.silenced).length} />
        <Stat k="Resolved, 24 h" v={hist.length} />
      </div>
      <div className="card">{items.length ? <AlertList items={items} nav={nav} /> : <div className="lmsg">{store_.ready ? "Nothing open for this scope." : "Waiting for the first evaluation."}</div>}</div>
      {hist.length > 0 && <>
        <div className="sech" style={{marginTop: 14}}><h2>Resolved in the last 24 hours</h2><span className="ln"></span></div>
        <div className="card"><AlertList items={hist} nav={nav} /></div>
      </>}
      {cluster && nav.alerts && <p className="mdesc" style={{marginTop: 10}}><button className="chip" onClick={() => nav.alerts({cluster: cluster.id})}>Open in the Alerts section</button></p>}
    </>
  );
}
// the top-bar bell: open critical alerts across the fleet
function AlertBell({nav, clusterId}) {
  const store_ = useAlerts();
  const open = store_.all().filter(a => !a.silenced);
  const crit = open.filter(a => a.severity === "critical").length;
  const warn = open.filter(a => a.severity === "warning").length;
  return (
    <button className="tbtn bellwrap" title={crit ? `${crit} critical, ${warn} warning` : warn ? `${warn} warning` : "No open alerts"}
      onClick={() => nav.alerts(clusterId ? {cluster: clusterId} : undefined)}>
      <Icon n="bell" s={14} />{crit > 0 && <span className="bellbadge">{crit > 99 ? "99+" : crit}</span>}
      {!crit && warn > 0 && <span className="bellbadge" style={{background: "var(--warn)"}}>{warn > 99 ? "99+" : warn}</span>}
    </button>
  );
}
// a severity count on tiles and detail heads
function AlertBadge({obj, nav}) {
  const store_ = useAlerts();
  if (!obj) return null;
  const as = store_.forObject(obj).filter(a => !a.silenced);
  if (!as.length) return null;
  const c = as.filter(a => a.severity === "critical").length, w = as.filter(a => a.severity === "warning").length;
  return <span className="abadge" title={as.map(a => a.title).join("\n")} onClick={e => { e.stopPropagation(); if (nav && nav.alerts) nav.alerts({cluster: obj.kind === "cluster" ? obj.id : obj.clusterId, q: obj.name || obj.hostname}); }}>
    <SevCount critical={c} warning={w} /></span>;
}

Object.assign(window, {ALERT_RULES, foldStatus, snapshotOf, historyFromLog, alertStore, useAlerts, AlertsView, AlertsScoped, AlertList, AlertRow, AlertBell, AlertBadge, silenceDialog});
