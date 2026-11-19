// ---------------------------------------------------------------------------
// TASKS AND OPERATIONS — one monitor for everything long-running
//
// Long-running work lives in three places: the operator's Ops kinds
// (StorageClusterOps, StorageNodeOps, VolumeMigration, OperatorOps,
// ClusterDeploymentConfig, BackupRestore, BackupImport, StorageBackup), the
// control plane's tasks (node_restart, device_migration, lvol_migration, ...),
// and the control plane's subsystem migrations. This module reads all three
// into one WorkItem shape and joins them into a tree: a rolling restart to
// its per-node steps to each node's restart task, a node removal to its
// VolumeMigrations, a device failure to its migration tasks, a node add to
// the deployment document and the node_add task.
//
// Cancellation is offered exactly where an API exists: spec.abort on
// OperatorOps and VolumeMigration, DELETE on a v2 migration. Everything else
// says why it cannot be canceled.
// ---------------------------------------------------------------------------
const WORK_KINDS = ["StorageClusterOps", "StorageNodeOps", "VolumeMigration", "OperatorOps", "ClusterDeploymentConfig", "BackupRestore", "BackupImport", "StorageBackup"];
const PHASE_MAP = {Pending: "Pending", Running: "Running", Succeeded: "Succeeded", Failed: "Failed", Aborted: "Aborted", Validating: "Running", Completed: "Succeeded",
  Draft: "Pending", Expanding: "Running", Expanded: "Succeeded", InProgress: "Running", PVCBinding: "Running", Done: "Succeeded", Merging: "Running", Deleting: "Running",
  Exporting: "Running", Importing: "Running", Active: "Succeeded"};
const phaseOf = raw => PHASE_MAP[raw] || (raw ? "Unknown" : "Pending");
const TERMINAL = ["Succeeded", "Failed", "Aborted"];
const PHASE_COLOR = {Pending: "var(--dim2)", Running: "var(--info)", Suspended: "var(--warn)", Succeeded: "var(--ok)", Failed: "var(--bad)", Aborted: "var(--warn)", Unknown: "var(--idle)"};
const CR_STEPS = {
  StorageNodeOps: {remove: ["Validating", "Suspending", "Migrating", "Verifying", "Removing"], migrate: ["Preparing", "Restarting", "Promoting"]},
  StorageClusterOps: {"node-rolling-restart": ["snode-refresh", "snode-refresh-wait", "shutting-down", "restarting", "rebalancing"]},
  OperatorOps: {Discover: ["Inspecting", "Probing", "Writing"]},
  ClusterDeploymentConfig: {deploy: ["Validating", "CreatingCluster", "AwaitingCluster", "CreatingNodes"]},
  BackupRestore: {restore: ["Pending", "InProgress", "PVCBinding", "Done"]},
  BackupImport: {import: ["Pending", "Exporting", "Importing", "Done"]},
  StorageBackup: {backup: ["Pending", "InProgress", "Done"]}
};
const ACTION_LABEL = {activate: "activate cluster", expand: "expand cluster", shutdown: "shut down", start: "start", restart: "restart", "node-rolling-restart": "rolling restart",
  suspend: "suspend node", resume: "resume node", remove: "remove node", migrate: "migrate", Discover: "discover", deploy: "deploy", restore: "restore backup", import: "import backup", backup: "back up",
  node_restart: "node restart", device_restart: "device restart", device_migration: "device migration", failed_device_migration: "failed device migration", new_device_migration: "new device migration",
  node_add: "node add", lvol_migration: "volume migration", s3_backup: "S3 backup", s3_backup_restore: "S3 restore", s3_backup_merge: "S3 merge", snapshot_replication: "snapshot replication",
  port_allow: "port allow", balancing_on_restart: "rebalance after restart", balancing_on_dev_rem: "rebalance after device removal", balancing_on_dev_add: "rebalance after device add", jc_comp_resume: "journal compression", lvol_sync_del: "volume sync delete", "node-add": "node add"};

// ---- normalizers -------------------------------------------------------------------
function normOpsItem(o, ctx) {
  const sp = o.spec || {}, st = o.status || {}, kind = o.kind;
  const action = sp.action || (kind === "VolumeMigration" ? "migrate" : kind === "ClusterDeploymentConfig" ? "deploy" : kind === "BackupRestore" ? "restore" : kind === "BackupImport" ? "import" : kind === "StorageBackup" ? "backup" : "run");
  const targetName = sp.clusterRef || sp.storageNodeRef || sp.pvName || sp.clusterName || (sp.pvcRef || {}).name || (sp.backupRef || {}).name || (sp.cluster || {}).name || sp.sourceClusterName || null;
  const targetKind = sp.clusterRef || sp.clusterName || (sp.cluster || {}).name || sp.sourceClusterName ? "cluster" : sp.storageNodeRef ? "node" : sp.pvName ? "volume" : sp.pvcRef ? "pvc" : sp.backupRef ? "backup" : null;
  const vm = Object.values(REG).find(v => v.ctx === ctx && v.k8s && ((targetKind === "cluster" && v.kind === "cluster" && v.k8s.name === targetName) || (targetKind === "node" && v.kind === "node" && v.k8s.name === targetName) || (targetKind === "volume" && v.kind === "volume" && v.pvName === targetName))) || null;
  const clusterId = st.clusterUUID || (vm ? (vm.kind === "cluster" ? vm.id : vm.clusterId) : null) || (REG[Object.keys(REG).find(k => REG[k].kind === "cluster" && REG[k].ctx === ctx && (REG[k].k8s || {}).name === sp.clusterName)] || {}).id || null;
  const rr = st.nodeRollingRestartStatus || null;
  const rawPhase = st.phase || "Pending";
  const phase = phaseOf(rawPhase);
  const step = (st.step && st.step.state) || st.subPhase || (rr && rr.nodePhase) || (kind === "BackupRestore" || kind === "BackupImport" || kind === "StorageBackup" ? rawPhase : null);
  const steps = (CR_STEPS[kind] || {})[action] || [];
  const progress = rr ? {done: (rr.processedNodes || []).length, total: (rr.processedNodes || []).length + (rr.pendingNodes || []).length, unit: "nodes"}
    : (st.volumesMigrated !== undefined || st.volumesPending !== undefined) ? {done: st.volumesMigrated || 0, total: (st.volumesMigrated || 0) + (st.volumesPending || 0), unit: "volumes"}
    : kind === "VolumeMigration" && (st.validationJobs || []).length ? {done: st.validationJobs.filter(j => j.succeeded).length, total: st.validationJobs.length, unit: "validation jobs"} : null;
  const canAbort = ABORTABLE_KINDS.includes(kind) && !TERMINAL.includes(phase);
  return {
    key: `ops:${objKey(ctx, o.metadata.namespace, kind, o.metadata.name)}`, src: "ops", crKind: kind, name: o.metadata.name, ns: o.metadata.namespace, ctx, uid: o.metadata.uid,
    clusterId, clusterName: clusterId && REG[clusterId] ? REG[clusterId].name : sp.clusterRef || sp.clusterName || null,
    action, actionLabel: ACTION_LABEL[action] || action,
    target: {kind: targetKind, name: targetName, uuid: vm ? vm.id : (sp.targetNodeUUID || null)},
    phase, rawPhase, step, stepIndex: Math.max(0, steps.indexOf(step)), steps,
    startedAt: st.startedAt || o.metadata.creationTimestamp, completedAt: st.completedAt || null, message: st.message || st.errorMessage || "",
    retries: null, progress, aborting: !!sp.abort,
    cancel: canAbort ? {can: true, how: "abort-patch", reason: null} : TERMINAL.includes(phase) ? {can: false, how: null, reason: `nothing to cancel: ${phase}`}
      : {can: false, how: null, reason: `${kind} has no abort field; it runs to completion`},
    parentKey: null, children: [], links: [], raw: o,
    rr, drainNode: (o.metadata.labels || {})[LABELS.drainNode] || null, migrationUUID: st.migrationUUID || null, nqn: st.subsystemNQN || null, configRef: st.configRef || null, backupRef: st.storageBackupRef || null
  };
}
function normTaskItem(t) {
  const phase = t.status === "new" ? "Pending" : t.status === "running" ? "Running" : t.status === "suspended" ? "Suspended" : t.canceled ? "Aborted" : /fail|error/i.test(t.result || "") ? "Failed" : "Succeeded";
  const node = t.nodeId ? REG[t.nodeId] : null, dev = t.deviceId ? REG[t.deviceId] : null;
  return {
    key: `task:${t.id}`, src: "task", crKind: null, name: t.id, ns: null, ctx: t.ctx, clusterId: t.clusterId, clusterName: (REG[t.clusterId] || {}).name || null,
    action: t.fn, actionLabel: ACTION_LABEL[t.fn] || t.fn,
    target: dev ? {kind: "device", name: dev.serial || dev.name, uuid: dev.id} : node ? {kind: "node", name: node.hostname, uuid: node.id} : {kind: t.deviceId ? "device" : t.nodeId ? "node" : null, name: t.deviceId || t.nodeId || null, uuid: t.deviceId || t.nodeId || null},
    phase, rawPhase: t.status, step: null, stepIndex: 0, steps: [], startedAt: t.createdAt, completedAt: null, message: (t.result || "").slice(0, 300),
    retries: t.maxRetry !== null && t.maxRetry !== undefined ? {n: t.retry, max: t.maxRetry} : {n: t.retry, max: null}, progress: null, aborting: false,
    cancel: TERMINAL.includes(phase) ? {can: false, how: null, reason: `nothing to cancel: ${phase}`} : {can: false, how: null, reason: "the control plane exposes no cancel route for tasks"},
    parentKey: t.parentId ? `task:${t.parentId}` : null, derived: !!t.derived, children: [], links: [], raw: t.raw, params: t.params
  };
}
function normMigrationItem(m) {
  const phase = /fail|error/i.test(m.status || "") ? "Failed" : /cancel|abort/i.test(m.status || "") ? "Aborted" : /done|complete|success/i.test(m.status || m.phase || "") ? "Succeeded" : /pend|new/i.test(m.status || "") ? "Pending" : "Running";
  return {
    key: `mig:${m.clusterId}/${m.nqn}/${m.id}`, src: "migration", crKind: null, name: m.id, ns: null, ctx: m.ctx, clusterId: m.clusterId, clusterName: (REG[m.clusterId] || {}).name || null,
    action: "migration", actionLabel: "subsystem migration", target: {kind: "node", name: (REG[m.targetNodeId] || {}).hostname || m.targetNodeId, uuid: m.targetNodeId},
    phase, rawPhase: m.status, step: m.phase, stepIndex: 0, steps: [], startedAt: m.startedAt, completedAt: m.completedAt, message: m.error || "",
    retries: {n: m.retries.n, max: m.retries.max}, progress: m.snaps.total ? {done: m.snaps.migrated || 0, total: m.snaps.total, unit: "snapshots"} : null, aborting: false,
    cancel: TERMINAL.includes(phase) ? {can: false, how: null, reason: `nothing to cancel: ${phase}`} : {can: true, how: "v2-delete", reason: null},
    parentKey: null, children: [], links: [], raw: m.raw, nqn: m.nqn, migrationUUID: m.id
  };
}
function normNodeAddItem(set, ctx, worker, since) {
  const cluster = Object.values(REG).find(v => v.kind === "cluster" && v.ctx === ctx && (v.k8s || {}).name === (set.spec || {}).clusterName) || null;
  const failed = ((set.status || {}).schedulingFailedWorkers || {})[worker];
  return {
    key: `nodeadd:${objKey(ctx, set.metadata.namespace, "StorageNodeSet", set.metadata.name)}/${worker}`, src: "nodeadd", crKind: "StorageNodeSet", name: `${set.metadata.name}/${worker}`, ns: set.metadata.namespace, ctx,
    clusterId: cluster ? cluster.id : null, clusterName: cluster ? cluster.name : (set.spec || {}).clusterName, action: "node-add", actionLabel: "node add",
    target: {kind: "worker", name: worker, uuid: null}, phase: failed ? "Failed" : "Running", rawPhase: failed ? "scheduling failed" : "pending", step: failed ? "scheduling" : "waiting for the node pod", stepIndex: 0, steps: [],
    startedAt: since, completedAt: null, message: failed ? "the storage node pod could not be scheduled on this worker" : "", retries: null, progress: null, aborting: false,
    cancel: {can: false, how: null, reason: "remove the worker from StorageNodeSet.spec.workerNodes instead"}, parentKey: null, children: [], links: [], raw: set
  };
}

// ---- the tree ----------------------------------------------------------------------------
function buildTree(items) {
  const byKey = new Map(items.map(i => [i.key, i]));
  items.forEach(i => { i.children = []; });
  const attach = (child, parentKey) => { const p = byKey.get(parentKey); if (p && p !== child) { child.parentKey = parentKey; p.children.push(child); } };
  const tasks = items.filter(i => i.src === "task");
  // tasks that say who their master is
  tasks.forEach(t => { if (t.parentKey && byKey.has(t.parentKey)) attach(t, t.parentKey); });
  items.filter(i => i.src === "ops").forEach(o => {
    // a rolling restart: one synthetic child per node, each pointing at that node's restart task
    if (o.crKind === "StorageClusterOps" && o.rr) {
      const done = o.rr.processedNodes || [], pend = o.rr.pendingNodes || [];
      done.concat(pend).forEach((uuid, i) => {
        const node = REG[uuid];
        const isDone = i < done.length, isCur = i === done.length && o.phase === "Running";
        const child = {key: `${o.key}/node/${uuid}`, src: "step", crKind: null, name: uuid, ctx: o.ctx, clusterId: o.clusterId, clusterName: o.clusterName,
          action: "restart", actionLabel: "node restart", target: {kind: "node", name: node ? node.hostname : shortId(uuid), uuid},
          phase: isDone ? "Succeeded" : isCur ? "Running" : o.phase === "Running" ? "Pending" : o.phase, rawPhase: isCur ? o.rr.nodePhase : "", step: isCur ? o.rr.nodePhase : null, stepIndex: 0, steps: [],
          startedAt: o.startedAt, completedAt: null, message: "", retries: null, progress: null, aborting: false,
          cancel: {can: false, how: null, reason: "part of the rolling restart"}, parentKey: o.key, children: [], links: [], raw: null};
        items.push(child); byKey.set(child.key, child); o.children.push(child);
        tasks.filter(t => t.action === "node_restart" && t.target.uuid === uuid && !t.parentKey && Date.parse(t.startedAt || 0) >= Date.parse(o.startedAt || 0) - 60000).forEach(t => attach(t, child.key));
      });
    }
    // a node removal or migration: its VolumeMigrations, each to its subsystem migration
    if (o.crKind === "StorageNodeOps" && (o.action === "remove" || o.action === "migrate") && o.target.uuid) {
      items.filter(v => v.crKind === "VolumeMigration" && v.drainNode === o.target.uuid).forEach(v => attach(v, o.key));
      tasks.filter(t => (t.action === "node_restart" || t.action === "balancing_on_restart") && t.target.uuid === o.target.uuid && !t.parentKey && Date.parse(t.startedAt || 0) >= Date.parse(o.startedAt || 0) - 60000).forEach(t => attach(t, o.key));
    }
    if (o.crKind === "VolumeMigration" && o.migrationUUID) items.filter(m => m.src === "migration" && m.migrationUUID === o.migrationUUID).forEach(m => attach(m, o.key));
    if (o.crKind === "OperatorOps" && o.configRef) items.filter(d => d.crKind === "ClusterDeploymentConfig" && d.name === o.configRef && d.ctx === o.ctx).forEach(d => attach(d, o.key));
    if (o.crKind === "BackupImport" && o.backupRef) items.filter(b => b.crKind === "StorageBackup" && b.name === o.backupRef && b.ctx === o.ctx).forEach(b => attach(b, o.key));
  });
  // a node add: the deployment document's node creation, the set's pending add, the node_add task
  items.filter(i => i.src === "nodeadd").forEach(n => {
    tasks.filter(t => t.action === "node_add" && t.clusterId === n.clusterId && !t.parentKey).forEach(t => attach(t, n.key));
  });
  // device tasks without a master: group them under a synthetic device item
  const byDevice = new Map();
  tasks.filter(t => !t.parentKey && t.target.kind === "device" && /device_migration|device_restart/.test(t.action)).forEach(t => {
    const k = `dev:${t.clusterId}/${t.target.uuid}`;
    if (!byDevice.has(k)) {
      const g = {key: k, src: "group", crKind: null, name: t.target.name, ctx: t.ctx, clusterId: t.clusterId, clusterName: t.clusterName, action: "device-recovery", actionLabel: "device recovery",
        target: t.target, phase: "Running", rawPhase: "", step: null, stepIndex: 0, steps: [], startedAt: t.startedAt, completedAt: null, message: "", retries: null, progress: null, aborting: false,
        cancel: {can: false, how: null, reason: "a group of tasks"}, parentKey: null, children: [], links: [], raw: null};
      byDevice.set(k, g); items.push(g); byKey.set(k, g);
    }
    attach(t, k);
  });
  byDevice.forEach(g => {
    const ph = g.children.map(c => c.phase);
    g.phase = ph.includes("Running") ? "Running" : ph.includes("Failed") ? "Failed" : ph.every(p => p === "Succeeded") ? "Succeeded" : ph.includes("Pending") ? "Pending" : "Running";
    g.startedAt = g.children.map(c => c.startedAt).filter(Boolean).sort()[0] || g.startedAt;
  });
  return items.filter(i => !i.parentKey);
}

// ---- the store -----------------------------------------------------------------------------
const workStore = {
  ready: false, items: [], roots: [], version: 0, listeners: new Set(), timer: null, loading: false, ring: new Map(), error: null,
  subscribe(f) { this.listeners.add(f); return () => this.listeners.delete(f); },
  emit() { this.version++; this.listeners.forEach(f => { try { f(); } catch (e) { console.error(e); } }); },
  async load() {
    if (this.loading) return;
    this.loading = true;
    try {
      const clusters = await api.clusters().catch(() => []);
      const perCtx = await Promise.all(CONTEXTS.map(async c => {
        const lists = await Promise.all(WORK_KINDS.map(k => store.read(c.name, k).catch(() => [])));
        const sets = await store.read(c.name, "StorageNodeSet").catch(() => []);
        const ops = lists.flat().map(o => normOpsItem(o, c.name));
        const adds = sets.flatMap(s => Object.entries((s.status || {}).pendingNodeAdds || {}).map(([w, since]) => normNodeAddItem(s, c.name, w, since)));
        return ops.concat(adds);
      }));
      const taskLists = await Promise.all(clusters.filter(c => hasControlPlane(c.ctx) && isUuid(c.id)).map(c => api.tasks(c.id).then(ts => ts.map(normTaskItem)).catch(() => [])));
      const tasks = taskLists.flat();
      // the control plane forgets done tasks quickly; a day's ring keeps them visible
      const now = Date.now();
      tasks.forEach(t => this.ring.set(t.key, {item: t, at: now}));
      this.ring.forEach((v, k) => { if (now - v.at > 86400000) this.ring.delete(k); });
      const liveKeys = new Set(tasks.map(t => t.key));
      const remembered = Array.from(this.ring.values()).filter(v => !liveKeys.has(v.item.key)).map(v => Object.assign({}, v.item, {phase: TERMINAL.includes(v.item.phase) ? v.item.phase : "Succeeded", remembered: true}));
      // subsystem migrations behind VolumeMigrations that are running
      const migs = (await Promise.all(perCtx.flat().filter(o => o.crKind === "VolumeMigration" && o.nqn && !TERMINAL.includes(o.phase) && o.clusterId && hasControlPlane(o.ctx))
        .map(o => cp.migrations(o.ctx, o.clusterId, o.nqn).then(ms => ms.map(normMigrationItem)).catch(() => [])))).flat();
      const items = perCtx.flat().concat(tasks, remembered, migs);
      this.items = items;
      this.roots = buildTree(items);
      this.ready = true; this.error = null;
    } catch (e) { this.error = e; }
    this.loading = false;
    this.emit();
  },
  start() { if (this.timer) return; const tick = () => this.load().finally(() => { this.timer = setTimeout(tick, document.hidden ? 15000 : 5000); }); tick(); },
  // ---- what the rest of the console reads ----
  all() { return this.items; },
  flat() { const out = []; const walk = (it, d) => { out.push(Object.assign(it, {depth: d})); it.children.forEach(c => walk(c, d + 1)); }; this.roots.forEach(r => walk(r, 0)); return out; },
  countsFor(clusterId) { const mine = this.items.filter(i => i.clusterId === clusterId && i.src !== "step"); return {running: mine.filter(i => i.phase === "Running" || i.phase === "Pending").length, failed: mine.filter(i => i.phase === "Failed" && !i.remembered).length}; },
  forObject(ref) { const r = ref || {}; return this.items.filter(i => (r.kind === "cluster" && i.clusterId === r.id && (i.target.kind === "cluster" || !i.target.kind)) || (i.target.uuid && i.target.uuid === r.id) || (i.target.name && r.k8s && i.target.name === r.k8s.name && i.ctx === r.ctx)); },
  byKey(key) { return this.items.find(i => i.key === key) || null; }
};
window.work = workStore;
if ((window.SB_CONFIG || {}).mode !== "dr") setTimeout(() => workStore.start(), 2500);
function useWork() { const [, f] = React.useState(0); React.useEffect(() => workStore.subscribe(() => f(n => n + 1)), []); return workStore; }

// ---- cancellation -------------------------------------------------------------------------------
function cancelDialog(item) {
  const how = item.cancel.how;
  return {
    title: `Cancel ${item.actionLabel} on ${item.target.name || item.clusterName || "the cluster"}?`, danger: true,
    desc: how === "abort-patch" ? `Sets spec.abort on ${item.crKind} ${item.name}. The controller stops at its next step, unwinds what it can, and the operation lands in Aborted.`
      : "DELETE on the control plane's migration: the copy stops and the target is cleaned up; the source stays authoritative.",
    fields: how === "v2-delete" ? [{k: "confirm", label: "Type CANCEL to confirm", type: "text", required: true, match: "CANCEL"}] : [],
    confirm: "Cancel operation",
    run: () => how === "abort-patch" ? api.operationAbort(item.crKind, item.name, {ctx: item.ctx, namespace: item.ns}) : cp.cancelMigration(item.ctx, item.clusterId, item.nqn, item.migrationUUID),
    done: "Cancellation requested"
  };
}

// ---- views -----------------------------------------------------------------------------------------
const Phase = ({p, raw}) => <span className="tstat" style={{color: PHASE_COLOR[p] || "var(--dim)"}} title={raw || p}><Dot c={PHASE_COLOR[p] || "var(--dim)"} />{p}</span>;
const Elapsed = ({item}) => {
  const [, tick] = React.useState(0);
  React.useEffect(() => { if (!item.startedAt || item.completedAt || TERMINAL.includes(item.phase)) return; const t = setInterval(() => tick(n => n + 1), 1000); return () => clearInterval(t); }, [item.key, item.phase]);
  if (!item.startedAt) return <span className="mono" style={{color: "var(--dim2)"}}>—</span>;
  const end = item.completedAt ? Date.parse(item.completedAt) : TERMINAL.includes(item.phase) ? null : Date.now();
  if (!end) return <span className="mono" style={{color: "var(--dim2)"}}>—</span>;
  const secs = (end - Date.parse(item.startedAt)) / 1000;
  // a completion stamped before its start is a clock problem, not a duration
  if (!(secs >= 0)) return <span className="mono" style={{color: "var(--dim2)"}} title="completed before it started; the clocks disagree">—</span>;
  return <span className="mono">{fmtDur(secs)}</span>;
};
function WorkRow({it, nav, expanded, onToggle, onCancel}) {
  const hasKids = it.children.length > 0;
  const target = it.target || {};
  const openTarget = () => {
    if (target.kind === "node" && target.uuid && it.clusterId) return nav.openNode(it.clusterId, target.uuid);
    if (target.kind === "device" && target.uuid && REG[target.uuid]) return nav.openDevice(it.clusterId, REG[target.uuid].nodeId, target.uuid);
    if (target.kind === "volume" && target.uuid && REG[target.uuid]) return nav.openVolumeById(target.uuid);
    if (it.clusterId) return nav.openCluster(it.clusterId);
  };
  return (
    <tr className={"wrow" + (it.phase === "Failed" ? " failed" : "") + ` tdepth-${Math.min(3, it.depth || 0)}`}>
      <td style={{width: 24}}>{hasKids && <button className="xpand" onClick={onToggle}><Icon n={expanded ? "chevd" : "chev"} s={11} /></button>}</td>
      <td><div style={{display: "flex", flexDirection: "column", lineHeight: 1.25, paddingLeft: (it.depth || 0) * 16}}>
        <span><b>{it.actionLabel}</b>{target.name && <> on <button className="tlink" onClick={openTarget}>{target.name}</button></>}{it.derived && <span className="badge" title="parent inferred from timing, the control plane records no parent id">inferred</span>}{it.remembered && <span className="badge" title="no longer listed by the control plane; remembered for a day">remembered</span>}</span>
        <span className="mono" style={{fontSize: 10, color: "var(--dim2)"}}>{it.crKind ? `${it.crKind} ${it.name}` : it.src === "task" ? `task ${shortId(it.name)}` : it.src === "migration" ? `migration ${shortId(it.name)}` : it.name}{it.clusterName && !it.parentKey ? ` · ${it.clusterName}` : ""}</span>
      </div></td>
      <td><span className="badge">{it.src === "ops" ? it.crKind : it.src}</span></td>
      <td><Phase p={it.phase} raw={it.rawPhase} /></td>
      <td className="mono" style={{fontSize: 11}}>{it.step ? (it.steps.length ? `${it.stepIndex + 1}/${it.steps.length} ` : "") + it.step : "—"}</td>
      <td style={{minWidth: 70}}>{it.progress && it.progress.total ? <div className="prog" title={`${it.progress.done} of ${it.progress.total} ${it.progress.unit}`}><div className="bar"><i style={{width: Math.round(it.progress.done / it.progress.total * 100) + "%", "--bc": "var(--info)"}}></i></div><span className="mono" style={{fontSize: 10}}>{it.progress.done}/{it.progress.total}</span></div> : <span style={{color: "var(--dim2)"}}>—</span>}</td>
      <td className="mono" style={{fontSize: 11}}>{it.startedAt ? fmtAgo(it.startedAt) : "—"}</td>
      <td><Elapsed item={it} /></td>
      <td className="mono" style={{fontSize: 11, color: it.retries && it.retries.n ? "var(--warn)" : undefined}}>{it.retries ? `${it.retries.n}${it.retries.max !== null && it.retries.max !== undefined ? "/" + it.retries.max : ""}` : "—"}</td>
      <td style={{maxWidth: 260, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap"}} title={it.message}>{it.message || ""}</td>
      <td style={{textAlign: "right", whiteSpace: "nowrap"}}>
        {it.cancel.can ? <button className="chip" onClick={() => onCancel(it)}><Icon n="x" s={10} />{it.aborting ? "Aborting…" : "Cancel"}</button>
          : <span className="chip" style={{opacity: .55, cursor: "help"}} title={it.cancel.reason}>no cancel</span>}
      </td>
    </tr>
  );
}
function WorkTree({roots, nav, filterFn, onCancel}) {
  const [open, setOpen] = useLocal("sb.work.open", {});
  const rows = [];
  const walk = (it, d) => { it.depth = d; rows.push(it); if (open[it.key] !== false && (open[it.key] || it.phase === "Running" || it.phase === "Failed" || d === 0)) it.children.forEach(c => walk(c, d + 1)); };
  roots.filter(filterFn).forEach(r => walk(r, 0));
  if (!rows.length) return <div className="lmsg">Nothing matches.</div>;
  return (
    <table className="dt tasks"><thead><tr><th></th><th>Operation</th><th>Kind</th><th>Phase</th><th>Step</th><th>Progress</th><th>Started</th><th>Elapsed</th><th>Retries</th><th>Message</th><th></th></tr></thead>
      <tbody>{rows.map(it => <WorkRow key={it.key} it={it} nav={nav} expanded={open[it.key] !== false && (open[it.key] || it.phase === "Running" || it.phase === "Failed" || it.depth === 0)} onToggle={() => setOpen(Object.assign({}, open, {[it.key]: !(open[it.key] !== false && (open[it.key] || it.phase === "Running" || it.phase === "Failed" || it.depth === 0))}))} onCancel={onCancel} />)}</tbody></table>
  );
}
function MonitorView({seg, nav, scope}) {
  const w = useWork();
  const p = (seg && seg.p) || {};
  const clusterId = (scope && scope.id) || p.cluster || null;
  const [state, setState] = React.useState(p.state || "running");
  const [kind, setKind] = React.useState(p.kind || "");
  const [q, setQ] = React.useState(p.q || "");
  const all = w.items.filter(i => (!clusterId || i.clusterId === clusterId) && i.src !== "step");
  const kinds = Array.from(new Set(all.map(i => i.crKind || i.src))).sort();
  const roots = w.roots.filter(r => !clusterId || r.clusterId === clusterId);
  const matchesSelf = i => (!kind || (i.crKind || i.src) === kind) && (!q || `${i.actionLabel} ${i.name} ${(i.target || {}).name || ""} ${i.message} ${i.clusterName || ""}`.toLowerCase().includes(q.toLowerCase()));
  const stateOk = i => state === "all" || (state === "running" ? (i.phase === "Running" || i.phase === "Pending" || i.phase === "Suspended") : state === "failed" ? i.phase === "Failed" : true);
  const matchesTree = i => (matchesSelf(i) && stateOk(i)) || i.children.some(matchesTree);
  const counts = {running: all.filter(i => i.phase === "Running" || i.phase === "Pending").length, pending: all.filter(i => i.phase === "Pending").length,
    failed: all.filter(i => i.phase === "Failed" && Date.parse(i.completedAt || i.startedAt || 0) > Date.now() - 86400000).length,
    done: all.filter(i => i.phase === "Succeeded" && Date.parse(i.completedAt || i.startedAt || 0) > Date.now() - 86400000).length, cancelable: all.filter(i => i.cancel.can).length};
  const cancel = it => window.__ui.dialog(cancelDialog(it), {kind: "operation", id: it.key});
  const body = (
    <>
      {w.error && <ErrorState error={w.error} onRetry={() => w.load()} />}
      <div className="stats">
        <Stat k="Running" v={counts.running} c={counts.running ? "var(--info)" : undefined} s={`${counts.pending} pending`} />
        <Stat k="Failed, 24 h" v={counts.failed} c={counts.failed ? "var(--bad)" : "var(--ok)"} />
        <Stat k="Succeeded, 24 h" v={counts.done} />
        <Stat k="Cancelable" v={counts.cancelable} s="abort field or migration cancel" />
      </div>
      <div className="card">
        <div className="ptools">
          <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" /><input value={q} placeholder="Search action, target, message…" onChange={e => setQ(e.target.value)} /></div>
          <select className="sel" value={kind} onChange={e => setKind(e.target.value)}><option value="">all kinds</option>{kinds.map(k => <option key={k} value={k}>{k}</option>)}</select>
          <div className="seg">{[["running", "running"], ["failed", "failed"], ["all", "all"]].map(([k, l]) => <button key={k} className={state === k ? "on" : ""} onClick={() => setState(k)}>{l}</button>)}</div>
          <div className="spacer"></div>
          <span className="count">{roots.filter(matchesTree).length}</span><span className="live"><i></i>live</span>
          <button className="chip" onClick={() => w.load()} title="Refresh"><Icon n="refresh" s={12} /></button>
        </div>
        {!w.ready ? <div className="lmsg">reading operations…</div> : <WorkTree roots={roots} nav={nav} filterFn={matchesTree} onCancel={cancel} />}
        <p className="mdesc" style={{margin: "10px 12px 12px"}}>Ops objects from every context, the control plane's tasks where a control plane is reachable, and subsystem migrations behind running volume migrations. Rows nest: a rolling restart under its per-node steps, a node removal under its volume migrations. Cancel appears only where an API exists; the tooltip says why elsewhere.</p>
      </div>
    </>
  );
  if (scope) return <div style={{marginTop: 4}}>{body}</div>;
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>Tasks and operations{clusterId && REG[clusterId] ? ` · ${REG[clusterId].name}` : ""}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Every long-running operation across the fleet, with its steps, progress, and where possible a cancel.</div></div>
        {clusterId && <button className="chip" onClick={() => nav.monitor()}>All clusters</button>}</div>
      {body}
    </div>
  );
}
function WorkItemDetail({seg, nav}) {
  const w = useWork();
  const it = w.byKey(seg.id);
  if (!w.ready) return <div className="scroll"><div className="lmsg">reading operations…</div></div>;
  if (!it) return <div className="scroll"><div className="empty"><Icon n="clock" s={24} /><b>Operation not found</b><span>It is no longer listed. Done tasks leave the control plane's list quickly.</span></div></div>;
  const cancel = x => window.__ui.dialog(cancelDialog(x), {kind: "operation", id: x.key});
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>{it.actionLabel}{it.target.name ? ` on ${it.target.name}` : ""}</h1>
        <div style={{display: "flex", gap: 14, marginTop: 5, alignItems: "center"}}><Phase p={it.phase} raw={it.rawPhase} /><span className="mono" style={{fontSize: 11, color: "var(--dim)"}}>{it.crKind ? `${it.crKind} ${it.name}` : it.key}</span></div></div>
        {it.cancel.can && <button className="btn" onClick={() => cancel(it)}><Icon n="x" s={11} />Cancel</button>}</div>
      <div className="stats">
        <Stat k="Started" v={it.startedAt ? fmtAgo(it.startedAt) : "—"} s={it.startedAt ? fmtDate(it.startedAt) : ""} />
        <Stat k="Elapsed" v={<Elapsed item={it} />} />
        <Stat k="Step" v={it.step || "—"} s={it.steps.length ? `${it.stepIndex + 1} of ${it.steps.length}` : ""} />
        <Stat k="Progress" v={it.progress ? `${it.progress.done}/${it.progress.total}` : "—"} s={it.progress ? it.progress.unit : ""} />
        <Stat k="Retries" v={it.retries ? `${it.retries.n}${it.retries.max !== null && it.retries.max !== undefined ? "/" + it.retries.max : ""}` : "—"} />
      </div>
      {it.message && <div className={"banner"}><Icon n={it.phase === "Failed" ? "alert" : "info"} s={15} /><span>{it.message}</span></div>}
      {it.steps.length > 0 && <div className="card"><h3>Steps</h3><div className="bd"><ul className="opsteps">{it.steps.map((s, i) => <li key={s} className={i < it.stepIndex || TERMINAL.includes(it.phase) && it.phase === "Succeeded" ? "done" : i === it.stepIndex && it.phase === "Running" ? "on" : i === it.stepIndex && it.phase === "Failed" ? "failed" : ""}><i></i>{s}</li>)}</ul></div></div>}
      {it.children.length > 0 && <><div className="sech" style={{marginTop: 14}}><h2>Children</h2><span className="ln"></span></div><div className="card"><WorkTree roots={it.children} nav={nav} filterFn={() => true} onCancel={cancel} /></div></>}
      <div className="sech" style={{marginTop: 14}}><h2>Links</h2><span className="ln"></span></div>
      <div className="labels">
        {it.clusterId && <button className="lab link" onClick={() => nav.openCluster(it.clusterId)}><Icon n="cluster" s={10} />{it.clusterName || "cluster"}</button>}
        {it.clusterId && nav.logs && <button className="lab link" onClick={() => nav.logs(it.clusterId, Object.assign({from: Date.parse(it.startedAt || Date.now()) - 60000, to: it.completedAt ? Date.parse(it.completedAt) + 60000 : Date.now()}, it.target.kind === "node" ? {snode: it.target.uuid} : {}))}><Icon n="list" s={10} />logs of this window</button>}
        {it.crKind && nav.openCR && <button className="lab link" onClick={() => nav.openCR({ctx: it.ctx, ns: it.ns, kind: it.crKind, name: it.name})}><Icon n="list" s={10} />the object</button>}
      </div>
      {it.raw && <div className="card" style={{marginTop: 12}}><h3>Raw</h3><div className="bd"><pre className="code" style={{maxHeight: 360, overflow: "auto"}}>{JSON.stringify(it.src === "ops" ? {spec: it.raw.spec, status: it.raw.status} : it.raw, null, 2)}</pre></div></div>}
    </div>
  );
}
// the top-bar indicator and the chip on a detail head
function OpsIndicator({nav}) {
  const w = useWork();
  const running = w.items.filter(i => (i.phase === "Running" || i.phase === "Pending") && i.src !== "step").length;
  const failed = w.items.filter(i => i.phase === "Failed" && !i.remembered && i.src !== "step" && Date.parse(i.completedAt || i.startedAt || 0) > Date.now() - 86400000).length;
  return (
    <button className="tbtn bellwrap" title={`${running} running · ${failed} failed in 24 h`} onClick={() => nav.monitor({state: running ? "running" : "all"})}>
      <Icon n="clock" s={14} />{running > 0 && <span className="bellbadge" style={{background: "var(--info)"}}>{running > 99 ? "99+" : running}</span>}
      {!running && failed > 0 && <span className="bellbadge">{failed > 99 ? "99+" : failed}</span>}
    </button>
  );
}
function OpsLink({obj, nav}) {
  const w = useWork();
  const mine = w.forObject(obj);
  const running = mine.filter(i => i.phase === "Running" || i.phase === "Pending").length, failed = mine.filter(i => i.phase === "Failed").length;
  if (!running && !failed) return null;
  return <button className="lab link" onClick={() => nav.monitor({cluster: obj.kind === "cluster" ? obj.id : obj.clusterId, q: obj.name || obj.hostname, state: running ? "running" : "failed"})}><Icon n="clock" s={10} />{running ? `${running} running` : ""}{running && failed ? " · " : ""}{failed ? `${failed} failed` : ""}</button>;
}

Object.assign(window, {workStore, useWork, MonitorView, WorkItemDetail, WorkTree, OpsIndicator, OpsLink, normOpsItem, normTaskItem, normMigrationItem, buildTree, cancelDialog, WORK_KINDS, PHASE_MAP});
