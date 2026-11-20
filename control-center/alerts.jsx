// ---------------------------------------------------------------------------
// ALERTS: conditions derived in the console from what the operator writes
//
// Every rule reads fields of the custom resources (storage.simplyblock.io/
// v1alpha2) and names the field it read: a StorageCluster whose status.phase
// is Degraded, a StorageNode whose status.health is false, a StorageDevice
// whose status.health.smart.verdict is Failing, an Ops object whose
// status.phase is Failed, a ControlPlane component with fewer ready replicas
// than desired. Capacity alerts compare the derived cluster capacity (the sum
// of its nodes') against the thresholds in the cluster's own spec. An alert
// opens when its condition holds, is kept with its first-seen time while it
// holds, and resolves when the condition clears or the object is gone; the
// resolved ones stay in a history for a day. Silences live in this browser.
// Nothing here reads a source the operator does not have.
// ---------------------------------------------------------------------------
const SEV_RANK = {critical: 2, warning: 1, info: 0};
const SEV_COLOR = {critical: "var(--bad)", warning: "var(--warn)", info: "var(--info)"};

// each rule returns alert instances for one world snapshot; `field` says what was read
const ALERT_RULES = {
  "cluster.phase": {scope: "cluster", evaluate: w => w.clusters.filter(c => ["Degraded", "Unavailable", "Suspended"].includes(c.phase)).map(c =>
    inst("cluster.phase", c.phase === "Degraded" ? "warning" : "critical", c, c, `StorageCluster ${c.name} is ${c.phase}`, c.message || `status.phase is ${c.phase}; status.status is ${c.status || "unset"}`, "status.phase"))},
  "cluster.capacity": {scope: "cluster", evaluate: w => w.clusters.flatMap(c => {
    const t = c.thresholds || {}, warn = (t.warn || {}).capacity, crit = (t.crit || {}).capacity;
    if (!c.capacity.total || (!warn && !crit)) return [];
    const pct = c.capacity.used / c.capacity.total * 100;
    if (crit && pct >= crit) return [inst("cluster.capacity", "critical", c, c, `StorageCluster ${c.name} is ${Math.round(pct)}% full`, `used capacity is at or above spec.criticalThreshold.capacity (${crit}%); the figure is derived: ${c.capacity.derived}`, "spec.criticalThreshold.capacity")];
    if (warn && pct >= warn) return [inst("cluster.capacity", "warning", c, c, `StorageCluster ${c.name} is ${Math.round(pct)}% full`, `used capacity is at or above spec.warningThreshold.capacity (${warn}%); the figure is derived: ${c.capacity.derived}`, "spec.warningThreshold.capacity")];
    return [];
  })},
  "cluster.stretched": {scope: "cluster", evaluate: w => w.clusters.flatMap(c => {
    const s = c.stretched || {}, out = [];
    if (s.siteLink === "Unreachable") out.push(inst("cluster.stretched", "critical", c, c, `StorageCluster ${c.name}: the other site is unreachable`, "status.stretched.siteLink is Unreachable", "status.stretched.siteLink"));
    if (s.lostSite) out.push(inst("cluster.stretched", "critical", c, c, `StorageCluster ${c.name}: site ${s.lostSite} is out of service`, `status.stretched.lostSite is ${s.lostSite}; siteRecord ${s.siteRecord || "unset"}`, "status.stretched.lostSite"));
    const a = c.arbitration || {};
    if (a.state && a.state !== "steady") out.push(inst("cluster.stretched", a.state === "partitioned" ? "critical" : "warning", c, c, `StorageCluster ${c.name}: arbitration is ${a.state}`, a.message || `status.arbitration.state is ${a.state}`, "status.arbitration.state"));
    return out;
  })},
  "node.phase": {scope: "node", evaluate: w => w.nodes.filter(n => ["Offline", "Degraded", "Failed"].includes(n.phase)).map(n =>
    inst("node.phase", n.phase === "Degraded" ? "warning" : "critical", n, REG[n.clusterId], `StorageNode ${n.name} is ${n.phase}`, n.message || `status.phase is ${n.phase}; status.status is ${n.status || "unset"}`, "status.phase"))},
  "node.health": {scope: "node", evaluate: w => w.nodes.filter(n => n.health === false && n.phase === "Online").map(n =>
    inst("node.health", "warning", n, REG[n.clusterId], `StorageNode ${n.name} reports health false while Online`, "status.health is false", "status.health"))},
  "node.network": {scope: "node", evaluate: w => w.nodes.flatMap(n => (n.network || []).filter(x => x.phase === "Mismatched" || x.phase === "Unavailable" || x.hostReachable === false).map(x =>
    inst("node.network", "warning", n, REG[n.clusterId], `StorageNode ${n.name}: network ${x.network} is ${x.phase}${x.hostReachable === false ? ", not host-reachable" : ""}`, `status.network[].phase is ${x.phase}; hostReachable ${x.hostReachable === undefined ? "unset" : x.hostReachable}`, "status.network[]")))},
  "node.remediation": {scope: "node", evaluate: w => w.nodes.flatMap(n => { const r = n.remediation || {}, flags = ["nodeNotReady", "outOfService", "reportedFenced"].filter(k => r[k]); return flags.length ? [inst("node.remediation", "warning", n, REG[n.clusterId], `StorageNode ${n.name}: ${flags.join(", ")}`, `status.remediation reports ${flags.join(", ")}; observed ${r.observedAt ? fmtAgo(r.observedAt) : "unset"}`, "status.remediation")] : []; })},
  "device.phase": {scope: "device", evaluate: w => w.devices.filter(d => ["Degraded", "Failed"].includes(d.phase)).map(d =>
    inst("device.phase", d.phase === "Degraded" ? "warning" : "critical", d, REG[d.clusterId], `StorageDevice ${d.name} is ${d.phase}`, d.message || `status.phase is ${d.phase}; status.deviceStatus is ${d.deviceStatus || "unset"}`, "status.phase"))},
  "device.smart": {scope: "device", evaluate: w => w.devices.filter(d => ["Warning", "Failing"].includes((d.health.smart || {}).verdict)).map(d =>
    inst("device.smart", d.health.smart.verdict === "Failing" ? "critical" : "warning", d, REG[d.clusterId], `StorageDevice ${d.name}: SMART verdict ${d.health.smart.verdict}`, d.health.smart.reason || "status.health.smart.verdict", "status.health.smart.verdict"))},
  "device.performance": {scope: "device", evaluate: w => w.devices.filter(d => (d.health.performance || {}).verdict === "Slow").map(d =>
    inst("device.performance", "warning", d, REG[d.clusterId], `StorageDevice ${d.name} is Slow`, d.health.performance.reason || "status.health.performance.verdict is Slow", "status.health.performance.verdict"))},
  "pool.phase": {scope: "pool", evaluate: w => w.pools.filter(p => p.phase !== "Ready").map(p =>
    inst("pool.phase", "warning", p, REG[p.clusterId], `StoragePool ${p.name} is ${p.phase}`, p.message || `status.phase is ${p.phase}`, "status.phase"))},
  "ops.failed": {scope: "ops", evaluate: w => w.ops.filter(o => o.phase === "Failed" && Date.parse(o.completedAt || o.startedAt || 0) > Date.now() - 86400000).map(o =>
    inst("ops.failed", "warning", {kind: o.crKind, id: o.key, name: o.name, namespace: o.ns}, o.clusterId ? REG[o.clusterId] : null, `${o.crKind} ${o.action || ""} ${o.name} failed`, `${o.message || "status.phase is Failed"}${o.step ? ` (at step ${o.step})` : ""}`, "status.phase"))},
  "controlplane.phase": {scope: "controlplane", evaluate: w => w.controlplanes.flatMap(cp => {
    const st = cp.status || {}, out = [];
    if (["Degraded", "Unavailable"].includes(st.phase)) out.push(inst("controlplane.phase", st.phase === "Unavailable" ? "critical" : "warning", {kind: "ControlPlane", id: cp.metadata.uid, name: cp.metadata.name, namespace: cp.metadata.namespace, ctx: cp.__ctx}, null, `ControlPlane ${cp.metadata.name} is ${st.phase}`, st.message || `status.phase is ${st.phase}`, "status.phase"));
    (st.components || []).filter(c => (c.ready || 0) < (c.desired || 0)).forEach(c => out.push(inst("controlplane.phase", c.essential ? "critical" : "warning", {kind: "ControlPlane", id: cp.metadata.uid + "/" + c.name, name: cp.metadata.name, namespace: cp.metadata.namespace, ctx: cp.__ctx}, null, `ControlPlane component ${c.name}: ${c.ready || 0} of ${c.desired} ready`, `status.components[] ${c.name} ready ${c.ready || 0}, desired ${c.desired}${c.essential ? ", essential" : ""}`, "status.components[]")));
    return out;
  })},
  "driver.phase": {scope: "driver", evaluate: w => w.drivers.filter(d => ["Degraded", "Unavailable"].includes((d.status || {}).phase)).map(d =>
    inst("driver.phase", d.status.phase === "Unavailable" ? "critical" : "warning", {kind: "SimplyblockDriver", id: d.metadata.uid, name: d.metadata.name, namespace: d.metadata.namespace, ctx: d.__ctx}, null, `SimplyblockDriver ${d.metadata.name} is ${d.status.phase}`, d.status.message || `nodesReady ${d.status.nodesReady} of ${d.status.nodesTotal}`, "status.phase"))}
};
function inst(rule, severity, obj, cluster, title, detail, field) {
  return {fingerprint: `${rule}|${obj.kind}|${obj.namespace || ""}|${obj.id || obj.name}`, rule, severity, title, detail, field,
    object: {kind: obj.kind, id: obj.id, name: obj.name, namespace: obj.namespace, ctx: obj.ctx || (obj.metadata ? obj.__ctx : null) || LOCAL_CTX, clusterId: obj.clusterId || (cluster ? cluster.id : null), nodeId: obj.nodeId || null},
    clusterId: cluster ? cluster.id : (obj.clusterId || null), clusterName: cluster ? cluster.name : null};
}

// ---- the store -----------------------------------------------------------------------------
const alertStore = {
  ready: false, instances: new Map(), history: [], silences: [], listeners: new Set(), timer: null, error: null, lastEval: null,
  subscribe(f) { this.listeners.add(f); return () => this.listeners.delete(f); },
  emit() { this.listeners.forEach(f => { try { f(); } catch (e) { console.error(e); } }); },
  loadSilences() { try { this.silences = JSON.parse(localStorage.getItem("sb.silences") || "[]").filter(s => !s.until || Date.parse(s.until) > Date.now()); } catch (e) { this.silences = []; } },
  saveSilences() { try { localStorage.setItem("sb.silences", JSON.stringify(this.silences)); } catch (e) {} },
  async evaluate() {
    try {
      const perCtx = async kind => (await Promise.all(CONTEXTS.map(c => k8s.list(kind, {allNamespaces: true, ctx: c.name}).then(r => r.map(o => Object.assign(o, {__ctx: c.name}))).catch(() => [])))).flat();
      const [clusters, cps, drivers] = await Promise.all([api.clusters(), perCtx("ControlPlane"), perCtx("SimplyblockDriver")]);
      const nodes = (await Promise.all(clusters.map(c => api.nodes(c.id)))).flat();
      const devices = (await Promise.all(clusters.map(c => api.clusterDevices(c.id)))).flat();
      const pools = (await Promise.all(clusters.map(c => api.pools(c.id)))).flat();
      const ops = window.work && window.work.ready ? window.work.items.filter(i => i.src === "ops") : [];
      const w = {clusters, nodes, devices, pools, ops, controlplanes: cps, drivers};
      const now = new Date().toISOString();
      const fresh = new Map();
      Object.entries(ALERT_RULES).forEach(([id, r]) => { try { r.evaluate(w).forEach(a => fresh.set(a.fingerprint, a)); } catch (e) { console.error("alert rule " + id, e); } });
      fresh.forEach((a, k) => { const old = this.instances.get(k); this.instances.set(k, Object.assign(a, {firstSeen: old ? old.firstSeen : now, lastSeen: now})); });
      this.instances.forEach((a, k) => { if (!fresh.has(k)) { this.instances.delete(k); this.history.unshift(Object.assign({}, a, {resolvedAt: now})); } });
      this.history = this.history.filter(h => Date.parse(h.resolvedAt) > Date.now() - 86400000).slice(0, 500);
      this.ready = true; this.error = null; this.lastEval = now;
    } catch (e) { this.error = e; }
    this.emit();
  },
  start() { if (this.timer) return; this.loadSilences(); const tick = () => this.evaluate().finally(() => { this.timer = setTimeout(tick, document.hidden ? 30000 : 10000); }); tick(); },
  isSilenced(a) { return this.silences.find(s => s.fingerprint === a.fingerprint && (!s.until || Date.parse(s.until) > Date.now())) || null; },
  silence(a, hours, note) { this.silences = this.silences.filter(s => s.fingerprint !== a.fingerprint).concat([{fingerprint: a.fingerprint, until: hours ? new Date(Date.now() + hours * 3600000).toISOString() : null, note: note || "", at: new Date().toISOString()}]); this.saveSilences(); this.emit(); },
  unsilence(a) { this.silences = this.silences.filter(s => s.fingerprint !== a.fingerprint); this.saveSilences(); this.emit(); },
  all() { return Array.from(this.instances.values()).map(a => Object.assign(a, {silence: this.isSilenced(a), silenced: !!this.isSilenced(a), id: a.fingerprint})); },
  forCluster(id) { return this.all().filter(a => a.clusterId === id); },
  forObject(ref) { const r = ref || {}; return this.all().filter(a => (r.kind === "cluster" && a.clusterId === r.id && a.object.kind === "cluster") || (a.object.id && a.object.id === r.id)); },
  countsFor(clusterId) { const as = this.forCluster(clusterId); return {critical: as.filter(a => a.severity === "critical" && !a.silenced).length, warning: as.filter(a => a.severity === "warning" && !a.silenced).length, silenced: as.filter(a => a.silenced).length}; },
  recent(hours) { const cutoff = Date.now() - (hours || 24) * 3600000; return this.history.filter(a => Date.parse(a.resolvedAt) >= cutoff); }
};
window.alerts = alertStore;
if ((window.SB_CONFIG || {}).mode !== "dr") setTimeout(() => alertStore.start(), 3000);
function useAlerts() { const [, f] = React.useState(0); React.useEffect(() => alertStore.subscribe(() => f(n => n + 1)), []); return alertStore; }

// ---- views ------------------------------------------------------------------------------------
const sortAlerts = (a, b) => (b.silenced ? 0 : 1) - (a.silenced ? 0 : 1) || SEV_RANK[b.severity] - SEV_RANK[a.severity] || Date.parse(b.firstSeen) - Date.parse(a.firstSeen);
function silenceDialog(a) {
  return {title: `Silence "${a.title}"?`, desc: "The alert stays evaluated but is counted as silenced in this browser until the silence expires. Silences are not shared.",
    fields: [{k: "hours", label: "Hours", type: "number", def: 4}, {k: "note", label: "Note", type: "text"}], confirm: "Silence",
    run: v => { alertStore.silence(a, Number(v.hours) || 0, v.note); return Promise.resolve(); }, done: "Silenced"};
}
function openAlertObject(a, nav) {
  const o = a.object, vm = o.id ? REG[o.id] : null;
  if (vm && vm.kind === "cluster") return nav.openCluster(vm.id);
  if (vm && vm.kind === "node") return nav.openNode(vm.clusterId, vm.id);
  if (vm && vm.kind === "device") return nav.openDevice(vm.clusterId, vm.nodeId, vm.id);
  if (vm && vm.kind === "pool") return nav.openPool(vm.clusterId, vm.id);
  if (o.kind && o.name && nav.openCR) return nav.openCR({ctx: o.ctx, ns: o.namespace, kind: o.kind, name: o.name});
  if (a.clusterId) return nav.openCluster(a.clusterId);
}
function AlertRow({a, nav, resolved}) {
  return (
    <tr className={resolved ? "" : a.silenced ? "" : a.severity === "critical" ? "wrow failed" : ""} style={a.silenced ? {opacity: .55} : undefined}>
      <td><span className="tstat" style={{color: SEV_COLOR[a.severity]}}><Dot c={SEV_COLOR[a.severity]} />{a.severity}</span></td>
      <td><div style={{display: "flex", flexDirection: "column", lineHeight: 1.25}}><b>{a.title}</b><span style={{fontSize: 11.5, color: "var(--dim)"}}>{a.detail}</span></div></td>
      <td className="mono" style={{fontSize: 10.5, color: "var(--dim2)"}} title="the field the rule read">{a.field}</td>
      <td>{a.clusterName ? <button className="tlink" onClick={() => nav.openCluster(a.clusterId)}>{a.clusterName}</button> : <span style={{color: "var(--dim2)"}}>none</span>}</td>
      <td className="mono" style={{fontSize: 11}} title={resolved ? `open ${fmtDate(a.firstSeen)}, resolved ${fmtDate(a.resolvedAt)}` : `first seen ${fmtDate(a.firstSeen)}`}>{resolved ? `resolved ${fmtAgo(a.resolvedAt)}` : fmtAgo(a.firstSeen)}</td>
      <td style={{textAlign: "right", whiteSpace: "nowrap"}}>
        <button className="chip" onClick={() => openAlertObject(a, nav)}>Open</button>
        {!resolved && (a.silenced ? <button className="chip" onClick={() => alertStore.unsilence(a)} title={a.silence.note || ""}>Unsilence</button> : <button className="chip" onClick={() => window.__ui.dialog(silenceDialog(a), {kind: "alert", id: a.fingerprint})}>Silence</button>)}
      </td>
    </tr>
  );
}
function AlertList({items, nav, resolved, empty}) {
  if (!items.length) return <div className="lmsg">{empty || "No alert."}</div>;
  return <table className="dt tasks"><thead><tr><th>Severity</th><th>Alert</th><th>Field</th><th>Cluster</th><th>{resolved ? "Resolved" : "Since"}</th><th></th></tr></thead><tbody>{items.map(a => <AlertRow key={a.fingerprint + (resolved ? a.resolvedAt : "")} a={a} nav={nav} resolved={resolved} />)}</tbody></table>;
}
function AlertsView({seg, nav}) {
  const s = useAlerts();
  const p = (seg && seg.p) || {};
  const [sev, setSev] = React.useState(p.sev || "");
  const [cluster, setCluster] = React.useState(p.cluster || "");
  const [showSilenced, setShowSilenced] = React.useState(false);
  const [q, setQ] = React.useState("");
  const live = s.all().filter(a => (!sev || a.severity === sev) && (!cluster || a.clusterId === cluster) && (showSilenced || !a.silenced) && (!q || `${a.title} ${a.detail} ${a.field}`.toLowerCase().includes(q.toLowerCase()))).sort(sortAlerts);
  const hist = s.recent(24).filter(a => (!cluster || a.clusterId === cluster));
  const clusters = Array.from(new Set(s.all().map(a => a.clusterId).filter(Boolean))).map(id => REG[id]).filter(Boolean);
  const counts = {critical: s.all().filter(a => a.severity === "critical" && !a.silenced).length, warning: s.all().filter(a => a.severity === "warning" && !a.silenced).length, silenced: s.all().filter(a => a.silenced).length};
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>Alerts</h1>
        <div className="mdesc" style={{marginTop: 4}}>Conditions derived from the operator's objects: phases, health verdicts, thresholds in the cluster's own spec, failed operations, control plane components. Each row names the field it read.{s.lastEval ? ` Evaluated ${fmtAgo(s.lastEval)}.` : ""}</div></div></div>
      {s.error && <ErrorState error={s.error} onRetry={() => s.evaluate()} />}
      <div className="stats">
        <Stat k="Critical" v={counts.critical} c={counts.critical ? "var(--bad)" : "var(--ok)"} />
        <Stat k="Warning" v={counts.warning} c={counts.warning ? "var(--warn)" : undefined} />
        <Stat k="Silenced" v={counts.silenced} s="in this browser" />
        <Stat k="Resolved, 24 h" v={hist.length} />
        <Stat k="Rules" v={Object.keys(ALERT_RULES).length} s="all on CR fields" />
      </div>
      <div className="card">
        <div className="ptools">
          <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" /><input value={q} placeholder="Search title, detail, field…" onChange={e => setQ(e.target.value)} /></div>
          <div className="seg">{[["", "all"], ["critical", "critical"], ["warning", "warning"]].map(([k, l]) => <button key={k} className={sev === k ? "on" : ""} onClick={() => setSev(k)}>{l}</button>)}</div>
          <select className="sel" value={cluster} onChange={e => setCluster(e.target.value)}><option value="">all clusters</option>{clusters.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}</select>
          <button className={"chip" + (showSilenced ? " on" : "")} onClick={() => setShowSilenced(!showSilenced)}>silenced</button>
          <div className="spacer"></div><span className="count">{live.length}</span><span className="live"><i></i>live</span>
          <button className="chip" onClick={() => s.evaluate()} title="Evaluate now"><Icon n="refresh" s={12} /></button>
        </div>
        {!s.ready ? <div className="lmsg">evaluating…</div> : <AlertList items={live} nav={nav} empty="No open alert." />}
      </div>
      <div className="sech" style={{marginTop: 14}}><h2>Resolved in the last 24 hours</h2><span className="ln"></span></div>
      <div className="card"><AlertList items={hist} nav={nav} resolved empty="Nothing resolved since this page was opened; the history lives in this browser session." /></div>
    </div>
  );
}
function AlertsScoped({cluster, nav}) {
  const s = useAlerts();
  const items = s.forCluster(cluster.id).sort(sortAlerts);
  const hist = s.recent(24).filter(a => a.clusterId === cluster.id);
  return (
    <>
      <div className="stats" style={{marginBottom: 12}}>
        <Stat k="Critical" v={items.filter(a => a.severity === "critical" && !a.silenced).length} c={items.some(a => a.severity === "critical" && !a.silenced) ? "var(--bad)" : "var(--ok)"} />
        <Stat k="Warning" v={items.filter(a => a.severity === "warning" && !a.silenced).length} />
        <Stat k="Resolved, 24 h" v={hist.length} />
      </div>
      <div className="card">{!s.ready ? <div className="lmsg">evaluating…</div> : <AlertList items={items} nav={nav} empty="No open alert on this cluster." />}</div>
      {hist.length > 0 && <div className="card" style={{marginTop: 12}}><h3>Resolved in the last 24 hours</h3><AlertList items={hist} nav={nav} resolved /></div>}
    </>
  );
}
function AlertBell({nav, clusterId}) {
  const s = useAlerts();
  const mine = clusterId ? s.forCluster(clusterId) : s.all();
  const crit = mine.filter(a => a.severity === "critical" && !a.silenced).length, warn = mine.filter(a => a.severity === "warning" && !a.silenced).length;
  return (
    <button className="tbtn bellwrap" title={`${crit} critical · ${warn} warning${clusterId ? " on this cluster" : ""}`} onClick={() => nav.alerts(clusterId ? {cluster: clusterId} : undefined)}>
      <Icon n="bell" s={14} />{crit > 0 && <span className="bellbadge">{crit > 99 ? "99+" : crit}</span>}{!crit && warn > 0 && <span className="bellbadge" style={{background: "var(--warn)"}}>{warn > 99 ? "99+" : warn}</span>}
    </button>
  );
}
function AlertBadge({obj, nav}) {
  const s = useAlerts();
  const mine = obj.kind === "cluster" ? s.forCluster(obj.id) : s.forObject(obj);
  const crit = mine.filter(a => a.severity === "critical" && !a.silenced).length, warn = mine.filter(a => a.severity === "warning" && !a.silenced).length;
  if (!crit && !warn) return null;
  return <button className="lab link" style={{color: crit ? "var(--bad)" : "var(--warn)"}} title={mine.map(a => a.title).join("\n")} onClick={e => { e.stopPropagation(); nav.alerts({cluster: obj.kind === "cluster" ? obj.id : obj.clusterId}); }}><Icon n="bell" s={10} />{crit ? `${crit} critical` : ""}{crit && warn ? " · " : ""}{warn ? `${warn} warning` : ""}</button>;
}

Object.assign(window, {ALERT_RULES, alertStore, useAlerts, AlertsView, AlertsScoped, AlertList, AlertRow, AlertBell, AlertBadge, silenceDialog});
