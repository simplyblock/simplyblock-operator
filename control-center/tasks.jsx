// ---------------------------------------------------------------------------
// TASKS AND OPERATIONS: one monitor for everything long-running
//
// What it shows is what the operator records (OPERATIONS.md): the Ops
// objects of every kind with their phase, their step on the action's graph
// (ops-model.jsx), and the progress fields their CRD carries; a StorageNode
// in its provisioning machine (a node add); a ClusterDeploymentConfig in its
// expansion machine; and the control plane's task window on each
// StorageCluster (status.tasks). Rows nest only where an object names its
// parent: a PersistentVolumeOps through spec.creatorRef or the drain-node
// label, a StorageClusterOps through its controller owner, a node add's task
// through status.nodeAddTaskID, a relocation's tasks through
// status.relocate.volumes[].taskID. Nothing is inferred from timing.
//
// Cancel is offered exactly where the operator has it: spec.abort while the
// operation stands at a step its graph marks abortable, and a
// StorageClusterOps CancelTask for a task in the window. Everything else
// says why it cannot be canceled.
// ---------------------------------------------------------------------------
const WORK_KINDS = ["StorageClusterOps", "StorageNodeOps", "StorageDeviceOps", "StoragePoolOps", "PersistentVolumeOps", "ControlPlaneOps", "OperatorOps", "StorageBackupOps", "ClusterDeploymentConfig"];
const TERMINAL = ["Succeeded", "Failed", "Aborted"];
const PHASE_COLOR = {Pending: "var(--dim2)", Running: "var(--info)", Suspended: "var(--warn)", Succeeded: "var(--ok)", Failed: "var(--bad)", Aborted: "var(--warn)", Provisioning: "var(--info)", Expanding: "var(--info)", Draft: "var(--dim2)", Expanded: "var(--ok)", Unknown: "var(--idle)"};
const TASK_PHASE = {new: "Pending", running: "Running", suspended: "Suspended", done: "Succeeded"};
const wkey = (kind, ns, name, ctx) => `${ctx || LOCAL_CTX}/${kind}/${ns || ""}/${name}`;

// ---- the objects the operation points at ---------------------------------------
const vmByName = (kind, name, ns, ctx) => Object.values(REG).find(v => v.k8s && v.k8s.kind === kind && v.k8s.name === name && (!ns || v.k8s.namespace === ns) && (!ctx || (v.ctx || LOCAL_CTX) === ctx)) || null;
const clusterByUUID = uuid => Object.values(REG).find(v => v.kind === "cluster" && v.id === uuid) || null;
function targetOf(o) {
  const r = RESOURCES[o.kind] || {}, sp = o.spec || {};
  if (!r.ref) return {kind: null, name: null, vm: null};
  const raw = sp[r.ref];
  const name = raw && typeof raw === "object" ? raw.name : raw;
  const vm = r.ops ? vmByName(r.ops, name, r.ops === "PersistentVolume" ? undefined : o.metadata.namespace, o.__ctx || LOCAL_CTX) : null;
  return {kind: r.ops, name: name || null, vm};
}
function clusterIdOf(o, target) {
  const st = o.status || {};
  if (target.vm) return target.vm.kind === "cluster" ? target.vm.id : target.vm.clusterId || null;
  if (st.migration && st.migration.clusterUUID) return clusterByUUID(st.migration.clusterUUID) ? st.migration.clusterUUID : null;
  if (st.relocate && st.relocate.sourceClusterUUID) return st.relocate.sourceClusterUUID;
  if (o.kind === "ClusterDeploymentConfig" && st.clusterRef) { const c = vmByName("StorageCluster", st.clusterRef, o.metadata.namespace, o.__ctx || LOCAL_CTX); return c ? c.id : null; }
  if (o.kind === "StorageNode") { const c = vmByName("StorageCluster", (o.spec || {}).clusterRef, o.metadata.namespace, o.__ctx || LOCAL_CTX); return c ? c.id : null; }
  return null;
}

// ---- normalizers -------------------------------------------------------------------
// progress as the CRD records it, per kind and action
function progressOf(o) {
  const st = o.status || {};
  if (st.rollingRestart) return {done: st.rollingRestart.nodeIndex || 0, total: (st.rollingRestart.nodes || []).length, unit: "nodes", field: "status.rollingRestart"};
  if (st.drain) return {done: st.drain.volumesMigrated || 0, total: st.drain.volumesTotal || 0, unit: "volumes", field: "status.drain"};
  if (st.relocate && (st.relocate.volumes || []).length) return {done: st.relocate.volumes.filter(v => v.cutOver).length, total: st.relocate.volumes.length, unit: "volumes cut over", field: "status.relocate.volumes"};
  if (st.migration && (st.migration.validationJobs || []).length) return {done: st.migration.validationJobs.filter(j => j.succeeded).length, total: st.migration.validationJobs.length, unit: "validation jobs", field: "status.migration.validationJobs"};
  if (o.kind === "ClusterDeploymentConfig" && (st.nodeRefs || []).length) return {done: st.nodeRefs.length, total: st.nodeRefs.length, unit: "nodes created", field: "status.nodeRefs"};
  return null;
}
function normOpsItem(o) {
  const sp = o.spec || {}, st = o.status || {};
  const target = targetOf(o);
  const pos = stepPosition(o.kind, sp.action, (st.step || {}).state);
  const phase = st.phase || "Pending";
  const canAbort = ABORTABLE_KINDS.includes(o.kind);
  const cancel = TERMINAL.includes(phase) ? {can: false, how: null, reason: `nothing to cancel: the operation is ${phase}`}
    : sp.abort ? {can: false, how: null, reason: "spec.abort is already set"}
    : !canAbort ? {can: false, how: null, reason: `${o.kind} has no spec.abort`}
    : phase === "Pending" ? {can: true, how: "abort-patch", reason: null}
    : pos.abortable ? {can: true, how: "abort-patch", reason: null}
    : {can: false, how: null, reason: `step ${(st.step || {}).state || "unknown"} does not honor spec.abort; the graph for ${sp.action} aborts only at ${((graphOf(o.kind, sp.action) || {}).abortable || []).join(", ") || "no step"}`};
  return {
    key: wkey(o.kind, o.metadata.namespace, o.metadata.name, o.__ctx), src: "ops", crKind: o.kind, name: o.metadata.name, ns: o.metadata.namespace, ctx: o.__ctx || LOCAL_CTX, uid: o.metadata.uid,
    clusterId: clusterIdOf(o, target), action: sp.action || null, target,
    phase, step: (st.step || {}).state || null, stepClaim: (st.step || {}).claim || null, stepDeadline: (st.step || {}).deadline || null,
    steps: pos.steps, stepIndex: pos.index, abortable: pos.abortable, perNode: pos.perNode,
    startedAt: st.startedAt || o.metadata.creationTimestamp, completedAt: st.completedAt || null, message: st.message || "",
    progress: progressOf(o), aborting: !!sp.abort, cancel,
    creatorRef: sp.creatorRef || null, owner: (o.metadata.ownerReferences || []).find(r => r.controller) || null, labels: o.metadata.labels || {},
    rollingRestart: st.rollingRestart || null, drain: st.drain || null, removal: st.removal || null, migration: st.migration || null, relocate: st.relocate || null, configRef: st.configRef || null,
    deferredSince: st.deferredSince || null, taskIds: (st.relocate && (st.relocate.volumes || []).map(v => v.taskID).filter(Boolean)) || (sp.cancelTask ? [sp.cancelTask.taskID] : []),
    parentKey: null, children: [], raw: o
  };
}
// a StorageNode being added: the entity's own provisioning machine
function normNodeAddItem(n) {
  const st = n.status || {};
  const pos = stepPosition("StorageNode", null, (st.step || {}).state);
  const vm = vmByName("StorageNode", n.metadata.name, n.metadata.namespace, n.__ctx || LOCAL_CTX);
  return {
    key: wkey("StorageNode", n.metadata.namespace, n.metadata.name, n.__ctx), src: "nodeadd", crKind: "StorageNode", name: n.metadata.name, ns: n.metadata.namespace, ctx: n.__ctx || LOCAL_CTX, uid: n.metadata.uid,
    clusterId: clusterIdOf(n, {vm: null}), action: "provisioning", target: {kind: "StorageNode", name: n.metadata.name, vm},
    phase: st.phase === "Failed" ? "Failed" : "Running", step: (st.step || {}).state || null, stepClaim: (st.step || {}).claim || null, stepDeadline: (st.step || {}).deadline || null,
    steps: pos.steps, stepIndex: pos.index, abortable: false,
    startedAt: n.metadata.creationTimestamp, completedAt: null, message: st.message || "", progress: null, aborting: false,
    cancel: {can: false, how: null, reason: "a node add has no abort; deleting the StorageNode creates its Remove operation"},
    nodeAddTaskID: st.nodeAddTaskID || null, taskIds: st.nodeAddTaskID ? [st.nodeAddTaskID] : [], parentKey: null, children: [], raw: n
  };
}
// one entry of a cluster's status.tasks window
function normTaskItem(t, cluster) {
  const phase = TASK_PHASE[t.status] || "Unknown";
  return {
    key: `task/${cluster.id}/${t.id}`, src: "task", crKind: null, name: t.id, ns: cluster.namespace, ctx: cluster.ctx, clusterId: cluster.id, action: t.type || null, target: {kind: "StorageCluster", name: cluster.name, vm: cluster},
    phase, step: null, steps: [], stepIndex: -1, abortable: false, startedAt: null, completedAt: null, message: "", progress: null, aborting: false,
    retry: t.retry || 0, cancel: TERMINAL.includes(phase) ? {can: false, how: null, reason: "the task is done"} : {can: true, how: "cancel-task", reason: null},
    parentKey: null, children: [], raw: t
  };
}

// ---- the tree ----------------------------------------------------------------------------
function buildTree(items) {
  const byKey = new Map(items.map(i => [i.key, i]));
  items.forEach(i => { i.children = []; i.parentKey = null; });
  const attach = (child, parent) => { if (parent && parent !== child && !child.parentKey) { child.parentKey = parent.key; parent.children.push(child); } };
  const ops = items.filter(i => i.src === "ops");
  ops.forEach(o => {
    // spec.creatorRef names the operation that created this one (a drain's migrations, the rebalancer's)
    if (o.creatorRef && o.creatorRef.kind !== "StorageCluster") attach(o, byKey.get(wkey(o.creatorRef.kind, o.creatorRef.namespace, o.creatorRef.name, o.ctx)));
    // the drain-node label, for a mover that wrote no creatorRef
    else if (o.crKind === "PersistentVolumeOps" && o.labels["storage.simplyblock.io/drain-node"]) {
      const drain = ops.find(p => p.crKind === "StorageNodeOps" && p.action === "Remove" && p.target.vm && p.target.vm.id === o.labels["storage.simplyblock.io/drain-node"]);
      attach(o, drain);
    }
    // a controller owner that is itself an operation (the activation a deployment creates)
    if (o.owner && WORK_KINDS.includes(o.owner.kind)) attach(o, byKey.get(wkey(o.owner.kind, o.ns, o.owner.name, o.ctx)));
    // a discovery's document
    if (o.crKind === "OperatorOps" && o.configRef) attach(byKey.get(wkey("ClusterDeploymentConfig", o.ns, o.configRef, o.ctx)), o);
  });
  // a node add under the document that created it (status.nodeRefs)
  items.filter(i => i.src === "nodeadd").forEach(n => {
    const doc = ops.find(d => d.crKind === "ClusterDeploymentConfig" && d.ns === n.ns && d.ctx === n.ctx && ((d.raw.status || {}).nodeRefs || []).includes(n.name));
    attach(n, doc);
  });
  // a child without a cluster takes its parent's; a creatorRef naming a StorageCluster is that cluster
  items.forEach(i => { if (!i.clusterId && i.creatorRef && i.creatorRef.kind === "StorageCluster") { const c = vmByName("StorageCluster", i.creatorRef.name, i.creatorRef.namespace, i.ctx); if (c) i.clusterId = c.id; } });
  const inherit = it => it.children.forEach(c => { if (!c.clusterId) c.clusterId = it.clusterId; inherit(c); });
  items.filter(i => !i.parentKey).forEach(inherit);
  // a task under the object that recorded its id
  const tasks = items.filter(i => i.src === "task");
  items.filter(i => i.src !== "task" && (i.taskIds || []).length).forEach(o => o.taskIds.forEach(id => tasks.filter(t => t.name === id).forEach(t => attach(t, o))));
  return items.filter(i => !i.parentKey);
}

// ---- the store -----------------------------------------------------------------------------
const workStore = {
  ready: false, items: [], roots: [], version: 0, listeners: new Set(), timer: null, loading: false, error: null,
  subscribe(f) { this.listeners.add(f); return () => this.listeners.delete(f); },
  emit() { this.version++; this.listeners.forEach(f => { try { f(); } catch (e) { console.error(e); } }); },
  async load() {
    if (this.loading) return;
    this.loading = true;
    try {
      const clusters = await api.clusters();
      // the nodes, devices, and pools go through the registry so an operation's target resolves
      await Promise.all(clusters.flatMap(c => [api.nodes(c.id), api.clusterDevices(c.id), api.pools(c.id)]));
      const w = await world();
      const lists = await Promise.all(CONTEXTS.flatMap(c => WORK_KINDS.map(k => k8s.list(k, {allNamespaces: true, ctx: c.name}).then(r => r.map(o => Object.assign(o, {__ctx: c.name}))).catch(() => []))));
      const ops = lists.flat().map(normOpsItem);
      const adds = w.nodes.filter(n => (n.status || {}).phase === "Provisioning" || (n.status || {}).phase === "Pending").map(normNodeAddItem);
      const tasks = clusters.flatMap(c => (c.tasks || []).map(t => normTaskItem(t, c)));
      const items = ops.concat(adds, tasks);
      this.items = items;
      this.roots = buildTree(items);
      this.ready = true; this.error = null;
    } catch (e) { this.error = e; }
    this.loading = false;
    this.emit();
  },
  start() { if (this.timer) return; const tick = () => this.load().finally(() => { this.timer = setTimeout(tick, document.hidden ? 15000 : 5000); }); tick(); },
  all() { return this.items; },
  countsFor(clusterId) { const mine = this.items.filter(i => i.clusterId === clusterId); return {running: mine.filter(i => i.phase === "Running" || i.phase === "Pending").length, failed: mine.filter(i => i.phase === "Failed").length}; },
  forObject(ref) {
    const r = ref || {};
    return this.items.filter(i => (r.kind === "cluster" && i.clusterId === r.id && (!i.target.vm || i.target.vm.kind === "cluster")) || (i.target.vm && i.target.vm.id === r.id) || (i.src === "nodeadd" && i.name === (r.k8s || {}).name));
  },
  byKey(key) { return this.items.find(i => i.key === key) || null; }
};
window.work = workStore;
if ((window.SB_CONFIG || {}).mode !== "dr") setTimeout(() => workStore.start(), 2500);
function useWork() { const [, f] = React.useState(0); React.useEffect(() => workStore.subscribe(() => f(n => n + 1)), []); return workStore; }

// ---- cancellation -------------------------------------------------------------------------------
function cancelDialog(item) {
  if (item.cancel.how === "cancel-task") {
    const c = item.target.vm;
    return {
      title: `StorageClusterOps CancelTask for ${item.action} ${item.name}?`, danger: true,
      desc: "Creates a StorageClusterOps with action CancelTask and spec.cancelTask.taskID set to this task. The operator asks the control plane to cancel it; Awaiting completes when the task leaves status.tasks.",
      confirm: "Create StorageClusterOps",
      run: () => submitOps("StorageCluster", c.name, "CancelTask", {taskID: item.name}, {namespace: c.namespace, ctx: c.ctx}).then(() => workStore.load()),
      done: "CancelTask created"
    };
  }
  return {
    title: `Abort ${item.crKind} ${item.name}?`, danger: true,
    desc: `Sets spec.abort. The operator honors it at step ${item.step || "Pending"} and the operation lands in Aborted; past the abortable steps it emits AbortRefused and runs on.`,
    confirm: "Set spec.abort",
    run: () => abortOps(item.crKind, item.name, {namespace: item.ns, ctx: item.ctx}).then(() => workStore.load()),
    done: "spec.abort set"
  };
}

// ---- views -----------------------------------------------------------------------------------------
const Phase = ({p}) => <span className="tstat" style={{color: PHASE_COLOR[p] || "var(--dim)"}}><Dot c={PHASE_COLOR[p] || "var(--dim)"} />{p}</span>;
const Elapsed = ({item}) => {
  const [, tick] = React.useState(0);
  React.useEffect(() => { if (!item.startedAt || item.completedAt || TERMINAL.includes(item.phase)) return; const t = setInterval(() => tick(n => n + 1), 1000); return () => clearInterval(t); }, [item.key, item.phase]);
  if (!item.startedAt) return <span className="mono" style={{color: "var(--dim2)"}}>unset</span>;
  const end = item.completedAt ? Date.parse(item.completedAt) : TERMINAL.includes(item.phase) ? null : Date.now();
  if (!end) return <span className="mono" style={{color: "var(--dim2)"}}>unset</span>;
  const secs = (end - Date.parse(item.startedAt)) / 1000;
  if (!(secs >= 0)) return <span className="mono" style={{color: "var(--dim2)"}} title="completedAt precedes startedAt">unset</span>;
  return <span className="mono">{fmtDur(secs)}</span>;
};
const StepCell = ({it}) => it.src === "task" ? <span className="mono" style={{fontSize: 11}} title="status.tasks[].retry">retry {it.retry}</span>
  : it.step ? <span className="mono" style={{fontSize: 11}} title={it.stepDeadline ? `deadline ${fmtDate(it.stepDeadline)}` : "status.step.state"}>{it.stepIndex >= 0 && it.steps.length ? `${Math.min(it.stepIndex + 1, it.steps.length)}/${it.steps.length} ` : ""}{it.step}{it.perNode && it.rollingRestart && it.step !== "Completed" ? ` (node ${(it.rollingRestart.nodeIndex || 0) + 1}/${(it.rollingRestart.nodes || []).length})` : ""}</span>
  : <span style={{color: "var(--dim2)"}}>unset</span>;
function WorkRow({it, nav, expanded, onToggle, onCancel}) {
  const hasKids = it.children.length > 0;
  const t = it.target || {};
  const openTarget = () => {
    const vm = t.vm;
    if (vm && vm.kind === "node") return nav.openNode(vm.clusterId, vm.id);
    if (vm && vm.kind === "device") return nav.openDevice(vm.clusterId, vm.nodeId, vm.id);
    if (vm && vm.kind === "pool") return nav.openPool(vm.clusterId, vm.id);
    if (vm && vm.kind === "cluster") return nav.openCluster(vm.id);
    if (t.kind && t.name) return nav.openCR({ctx: it.ctx, ns: t.kind === "PersistentVolume" ? undefined : it.ns, kind: t.kind, name: t.name});
  };
  const label = it.src === "task" ? it.action : it.src === "nodeadd" ? "StorageNode provisioning" : `${it.crKind} ${it.action || ""}`;
  return (
    <tr className={"wrow" + (it.phase === "Failed" ? " failed" : "") + ` tdepth-${Math.min(3, it.depth || 0)}`}>
      <td style={{width: 24}}>{hasKids && <button className="xpand" onClick={onToggle}><Icon n={expanded ? "chevd" : "chev"} s={11} /></button>}</td>
      <td><div style={{display: "flex", flexDirection: "column", lineHeight: 1.25, paddingLeft: (it.depth || 0) * 16}}>
        <span><b>{label}</b>{t.name && <> on <button className="tlink" onClick={openTarget}>{t.name}</button></>}</span>
        <span className="mono" style={{fontSize: 10, color: "var(--dim2)"}}>{it.src === "task" ? `status.tasks[] id ${it.name}` : `${it.ns ? it.ns + "/" : ""}${it.name}`}{window.MULTI_CTX ? ` · ${it.ctx}` : ""}</span>
      </div></td>
      <td><span className="badge">{it.src === "task" ? "task" : it.crKind}</span></td>
      <td><Phase p={it.phase} /></td>
      <td><StepCell it={it} /></td>
      <td style={{minWidth: 70}}>{it.progress && it.progress.total ? <div className="prog" title={`${it.progress.field}: ${it.progress.done} of ${it.progress.total} ${it.progress.unit}`}><div className="bar"><i style={{width: Math.round(it.progress.done / it.progress.total * 100) + "%", "--bc": "var(--info)"}}></i></div><span className="mono" style={{fontSize: 10}}>{it.progress.done}/{it.progress.total}</span></div> : <span style={{color: "var(--dim2)"}}>unset</span>}</td>
      <td className="mono" style={{fontSize: 11}}>{it.startedAt ? fmtAgo(it.startedAt) : "unset"}</td>
      <td><Elapsed item={it} /></td>
      <td style={{maxWidth: 260, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap"}} title={it.message}>{it.message || ""}</td>
      <td style={{textAlign: "right", whiteSpace: "nowrap"}}>
        {it.cancel.can ? <button className="chip" onClick={() => onCancel(it)}><Icon n="x" s={10} />{it.cancel.how === "cancel-task" ? "CancelTask" : "Abort"}</button>
          : <span className="chip" style={{opacity: .55, cursor: "help"}} title={it.cancel.reason}>{it.aborting ? "aborting" : "no abort"}</span>}
        {nav.openWork && <button className="chip" onClick={() => nav.openWork(it.clusterId, it.key)}>Open</button>}
      </td>
    </tr>
  );
}
function WorkTree({roots, nav, filterFn, onCancel}) {
  const [open, setOpen] = useLocal("sb.work.open", {});
  const isOpen = it => open[it.key] !== false && (open[it.key] || it.phase === "Running" || it.phase === "Failed" || it.depth === 0);
  const rows = [];
  const walk = (it, d) => { it.depth = d; rows.push(it); if (isOpen(it)) it.children.forEach(c => walk(c, d + 1)); };
  roots.filter(filterFn).forEach(r => walk(r, 0));
  if (!rows.length) return <div className="lmsg">Nothing matches.</div>;
  return (
    <table className="dt tasks"><thead><tr><th></th><th>Operation</th><th>Kind</th><th>status.phase</th><th>status.step</th><th>Progress</th><th>startedAt</th><th>Elapsed</th><th>status.message</th><th></th></tr></thead>
      <tbody>{rows.map(it => <WorkRow key={it.key} it={it} nav={nav} expanded={isOpen(it)} onToggle={() => setOpen(Object.assign({}, open, {[it.key]: !isOpen(it)}))} onCancel={onCancel} />)}</tbody></table>
  );
}
function MonitorView({seg, nav, scope}) {
  const w = useWork();
  const p = (seg && seg.p) || {};
  const clusterId = (scope && scope.id) || p.cluster || null;
  const [state, setState] = React.useState(p.state || "running");
  const [kind, setKind] = React.useState(p.kind || "");
  const [q, setQ] = React.useState(p.q || "");
  const all = w.items.filter(i => !clusterId || i.clusterId === clusterId);
  const kinds = Array.from(new Set(all.map(i => i.crKind || i.src))).sort();
  const roots = w.roots.filter(r => !clusterId || r.clusterId === clusterId);
  const matchesSelf = i => (!kind || (i.crKind || i.src) === kind) && (!q || `${i.crKind || ""} ${i.action || ""} ${i.name} ${(i.target || {}).name || ""} ${i.message}`.toLowerCase().includes(q.toLowerCase()));
  const stateOk = i => state === "all" || (state === "running" ? ["Running", "Pending", "Suspended"].includes(i.phase) : state === "failed" ? i.phase === "Failed" : true);
  const matchesTree = i => (matchesSelf(i) && stateOk(i)) || i.children.some(matchesTree);
  const day = Date.now() - 86400000;
  const counts = {running: all.filter(i => i.phase === "Running" || i.phase === "Pending").length, pending: all.filter(i => i.phase === "Pending").length,
    failed: all.filter(i => i.phase === "Failed" && Date.parse(i.completedAt || i.startedAt || 0) > day).length,
    done: all.filter(i => i.phase === "Succeeded" && Date.parse(i.completedAt || i.startedAt || 0) > day).length, cancelable: all.filter(i => i.cancel.can).length,
    tasks: all.filter(i => i.src === "task").length};
  const cancel = it => window.__ui.dialog(cancelDialog(it), {kind: "operation", id: it.key});
  const body = (
    <>
      {w.error && <ErrorState error={w.error} onRetry={() => w.load()} />}
      <div className="stats">
        <Stat k="Running" v={counts.running} c={counts.running ? "var(--info)" : undefined} s={`${counts.pending} Pending`} />
        <Stat k="Failed, 24 h" v={counts.failed} c={counts.failed ? "var(--bad)" : "var(--ok)"} />
        <Stat k="Succeeded, 24 h" v={counts.done} />
        <Stat k="status.tasks" v={counts.tasks} s="the control plane's window" />
        <Stat k="Cancelable" v={counts.cancelable} s="at an abortable step, or a task" />
      </div>
      <div className="card">
        <div className="ptools">
          <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" /><input value={q} placeholder="Search kind, action, target, message…" onChange={e => setQ(e.target.value)} /></div>
          <select className="sel" value={kind} onChange={e => setKind(e.target.value)}><option value="">all kinds</option>{kinds.map(k => <option key={k} value={k}>{k}</option>)}</select>
          <div className="seg">{[["running", "running"], ["failed", "failed"], ["all", "all"]].map(([k, l]) => <button key={k} className={state === k ? "on" : ""} onClick={() => setState(k)}>{l}</button>)}</div>
          <div className="spacer"></div>
          <span className="count">{roots.filter(matchesTree).length}</span><span className="live"><i></i>live</span>
          <button className="chip" onClick={() => w.load()} title="Refresh"><Icon n="refresh" s={12} /></button>
        </div>
        {!w.ready ? <div className="lmsg">reading operations…</div> : <WorkTree roots={roots} nav={nav} filterFn={matchesTree} onCancel={cancel} />}
        <p className="mdesc" style={{margin: "10px 12px 12px"}}>Every Ops object, every StorageNode in its provisioning machine, and each cluster's status.tasks window. A row nests under the object that names it (spec.creatorRef, the drain-node label, a controller owner, a recorded task id) and nowhere else. Abort appears while the operation stands at a step its graph honors; the tooltip says why not elsewhere.</p>
      </div>
    </>
  );
  if (scope) return <div style={{marginTop: 4}}>{body}</div>;
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>Tasks and operations{clusterId && REG[clusterId] ? ` · ${REG[clusterId].name}` : ""}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Long-running work as the operator records it: Ops objects on their step graphs, node adds, and the control plane's task window.</div></div>
        {clusterId && <button className="chip" onClick={() => nav.monitor()}>All clusters</button>}</div>
      {body}
    </div>
  );
}
function WorkItemDetail({seg, nav}) {
  const w = useWork();
  const it = w.byKey(seg.id);
  if (!w.ready) return <div className="scroll"><div className="lmsg">reading operations…</div></div>;
  if (!it) return <div className="scroll"><div className="empty"><Icon n="clock" s={24} /><b>Not listed any more</b><span>A finished task leaves status.tasks; an operation stays until it is deleted.</span></div></div>;
  const cancel = x => window.__ui.dialog(cancelDialog(x), {kind: "operation", id: x.key});
  const label = it.src === "task" ? `${it.action} task` : it.src === "nodeadd" ? `StorageNode ${it.name}` : `${it.crKind} ${it.action || ""}`;
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>{label}{it.target.name && it.src !== "nodeadd" ? ` on ${it.target.name}` : ""}</h1>
        <div style={{display: "flex", gap: 14, marginTop: 5, alignItems: "center", flexWrap: "wrap"}}><Phase p={it.phase} /><span className="mono" style={{fontSize: 11, color: "var(--dim)"}}>{it.src === "task" ? `status.tasks[] id ${it.name}` : `${it.ns ? it.ns + "/" : ""}${it.name}`}</span></div></div>
        {it.cancel.can && <button className="btn" onClick={() => cancel(it)}><Icon n="x" s={11} />{it.cancel.how === "cancel-task" ? "CancelTask" : "Abort"}</button>}</div>
      <div className="stats">
        <Stat k="status.startedAt" v={it.startedAt ? fmtAgo(it.startedAt) : "unset"} s={it.startedAt ? fmtDate(it.startedAt) : ""} />
        <Stat k="Elapsed" v={<Elapsed item={it} />} s={it.completedAt ? `completedAt ${fmtDate(it.completedAt)}` : ""} />
        <Stat k="status.step.state" v={it.step || "unset"} s={it.stepDeadline ? `deadline ${fmtDate(it.stepDeadline)}` : it.src === "task" ? `retry ${it.retry}` : ""} />
        <Stat k="Progress" v={it.progress ? `${it.progress.done}/${it.progress.total}` : "unset"} s={it.progress ? `${it.progress.unit} (${it.progress.field})` : ""} />
        <Stat k="Abort" v={it.cancel.can ? "possible" : "refused"} s={it.cancel.reason || "at this step the graph honors spec.abort"} />
      </div>
      {it.message && <div className="banner"><Icon n={it.phase === "Failed" ? "alert" : "list"} s={15} /><span>{it.message}</span></div>}
      {it.steps.length > 0 && <div className="card"><h3>Steps of {it.crKind} {it.action || "provisioning"}</h3><div className="bd"><ul className="opsteps">{it.steps.map((s, i) => <li key={s} className={it.phase === "Succeeded" || i < it.stepIndex ? "done" : i === it.stepIndex && (it.phase === "Running" || it.src === "nodeadd") ? "on" : i === it.stepIndex && it.phase === "Failed" ? "failed" : i === it.stepIndex && it.phase === "Aborted" ? "aborted" : ""}><i></i>{s}{(graphOf(it.crKind, it.action) || {abortable: []}).abortable.includes(s) ? <span className="lab" style={{marginLeft: 6}}>abortable</span> : null}</li>)}</ul></div></div>}
      {it.children.length > 0 && <><div className="sech" style={{marginTop: 14}}><h2>Objects that name this one</h2><span className="ln"></span></div><div className="card"><WorkTree roots={it.children} nav={nav} filterFn={() => true} onCancel={cancel} /></div></>}
      <div className="sech" style={{marginTop: 14}}><h2>Links</h2><span className="ln"></span></div>
      <div className="labels">
        {it.clusterId && REG[it.clusterId] && <button className="lab link" onClick={() => nav.openCluster(it.clusterId)}><Icon n="cluster" s={10} />{REG[it.clusterId].name}</button>}
        {it.crKind && <button className="lab link" onClick={() => nav.openCR({ctx: it.ctx, ns: it.ns, kind: it.crKind, name: it.name})}><Icon n="list" s={10} />{it.crKind} {it.name}</button>}
        {it.creatorRef && <button className="lab link" onClick={() => nav.openCR({ctx: it.ctx, ns: it.creatorRef.namespace, kind: it.creatorRef.kind, name: it.creatorRef.name})}><Icon n="chev" s={10} />spec.creatorRef {it.creatorRef.kind}/{it.creatorRef.name}</button>}
      </div>
      {it.raw && <div className="card" style={{marginTop: 12}}><h3>{it.src === "task" ? "status.tasks[] entry" : "spec and status"}</h3><div className="bd"><pre className="code" style={{maxHeight: 360, overflow: "auto"}}>{JSON.stringify(it.src === "task" ? it.raw : {spec: it.raw.spec, status: it.raw.status}, null, 2)}</pre></div></div>}
    </div>
  );
}
// the top-bar indicator and the chip on a detail head
function OpsIndicator({nav}) {
  const w = useWork();
  const running = w.items.filter(i => i.phase === "Running" || i.phase === "Pending").length;
  const failed = w.items.filter(i => i.phase === "Failed" && Date.parse(i.completedAt || i.startedAt || 0) > Date.now() - 86400000).length;
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
  return <button className="lab link" onClick={() => nav.monitor({cluster: obj.kind === "cluster" ? obj.id : obj.clusterId, q: obj.name, state: running ? "running" : "failed"})}><Icon n="clock" s={10} />{running ? `${running} running` : ""}{running && failed ? " · " : ""}{failed ? `${failed} failed` : ""}</button>;
}

Object.assign(window, {workStore, useWork, MonitorView, WorkItemDetail, WorkTree, OpsIndicator, OpsLink, normOpsItem, normTaskItem, normNodeAddItem, buildTree, cancelDialog, WORK_KINDS});
