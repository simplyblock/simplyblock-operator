// ---------------------------------------------------------------------------
// FLEET DASHBOARD: the root of the Clusters section
//
// One view for every deployment shape: a few sites with a cluster each, a
// lifecycle ladder of contexts, tenants in namespaces, a dozen edge clusters
// in one Kubernetes cluster, or one context per edge site. The grouping
// switch picks the dimension, and every dimension is something the objects
// or the console's configuration actually carry: the Kubernetes context, the
// namespace, the site (a stretched cluster's spec.stretched.site, else the
// topology.kubernetes.io/zone label of the workers the nodes run on, which is
// what "sites are tagged nodes" means), the region label, any label on the
// StorageCluster, or any label the administrator configured on a context.
// Each group says where its value came from. Counts and capacity are the
// derived values api.jsx documents; alerts and operations come from their
// stores; the control planes are the ControlPlane objects of each context.
// ---------------------------------------------------------------------------
const SM = s => STATUS_META[s] || {c: "var(--idle)", rank: 1, label: s || "unknown"};
const FLEET_POLL = 10000;
const NONE = "none";

const BASE_GROUPINGS = {
  context: {label: "Context", keysOf: c => [c.ctx], from: () => "the Kubernetes context the object was read from"},
  namespace: {label: "Namespace", keysOf: c => [c.namespace], from: () => "metadata.namespace"},
  site: {label: "Site", keysOf: c => (c.sites.length ? c.sites.map(s => s.value) : [NONE]), from: c => (c.sites[0] || {}).from || "no site: the cluster is not stretched and no worker carries a zone label"},
  region: {label: "Region", keysOf: c => (c.regions.length ? c.regions : [NONE]), from: () => "worker Node label topology.kubernetes.io/region"},
  flat: {label: "Flat", keysOf: () => [""], from: () => null}
};
// "label:<key>" groups by a StorageCluster label, "ctxlabel:<key>" by a label the console's configuration gives the context
function groupingOf(key) {
  if (BASE_GROUPINGS[key]) return BASE_GROUPINGS[key];
  if (key.startsWith("label:")) { const k = key.slice(6); return {label: k, keysOf: c => [(c.labels || {})[k] || NONE], from: () => `metadata.labels[${k}]`}; }
  if (key.startsWith("ctxlabel:")) { const k = key.slice(9); return {label: k, keysOf: c => [(ctxLabels(c.ctx) || {})[k] || NONE], from: () => `the console's configuration of context ${k}`}; }
  return BASE_GROUPINGS.flat;
}
const groupBy = (clusters, key) => {
  const g = groupingOf(key);
  const map = new Map();
  clusters.forEach(c => g.keysOf(c).forEach(k => { if (!map.has(k)) map.set(k, {key: k, from: g.from(c), rows: []}); map.get(k).rows.push(c); }));
  const groups = Array.from(map.values());
  groups.sort((a, b) => (a.key === NONE) - (b.key === NONE) || a.key.localeCompare(b.key));
  return groups;
};
// the grouping keys on offer: the fixed ones, every label key a cluster carries, every label key a context declares
const groupingKeys = clusters => {
  const labelKeys = Array.from(new Set(clusters.flatMap(c => Object.keys(c.labels || {})))).sort();
  const ctxKeys = window.ctxLabelKeys ? ctxLabelKeys() : [];
  return [{k: "context", l: "Context", on: window.MULTI_CTX}, {k: "namespace", l: "Namespace", on: true}, {k: "site", l: "Site", on: true}, {k: "region", l: "Region", on: clusters.some(c => c.regions.length)}]
    .filter(x => x.on).map(x => ({k: x.k, l: x.l}))
    .concat(ctxKeys.map(k => ({k: "ctxlabel:" + k, l: `context ${k}`})), labelKeys.map(k => ({k: "label:" + k, l: `label ${k}`})), [{k: "flat", l: "Flat"}]);
};

// ---- the fleet -----------------------------------------------------------------
async function loadFleet() {
  const clusters = await api.clusters();
  const cps = (await Promise.all(CONTEXTS.map(c => k8s.list("ControlPlane", {allNamespaces: true, ctx: c.name}).then(r => r.map(o => Object.assign(o, {__ctx: c.name}))).catch(() => [])))).flat();
  // the nodes at each site: a StorageNode belongs to the zone of its worker (or its stretched site)
  const nodes = (await Promise.all(clusters.map(c => api.nodes(c.id).catch(() => [])))).flat();
  const nodesAt = {};
  nodes.forEach(n => { const site = n.zone || (n.config || {}).site || NONE; const a = nodesAt[site] || (nodesAt[site] = {total: 0, online: 0}); a.total++; if (n.phase === "Online") a.online++; });
  const rows = clusters.map(c => {
    const th = c.thresholds || {};
    const cap = c.capacity;
    return Object.assign({}, c, {
      utilPct: cap.total ? cap.used / cap.total * 100 : 0,
      warnCap: (th.warn || {}).capacity, critCap: (th.crit || {}).capacity,
      controlPlane: cps.find(cp => cp.__ctx === c.ctx) || null
    });
  });
  return {rows, controlPlanes: cps, nodesAt};
}
const NO_ALERTS = {critical: 0, warning: 0, silenced: 0}, NO_OPS = {running: 0, failed: 0};
function useFleet(rev) {
  const r = useResource("fleet|" + rev, loadFleet, FLEET_POLL);
  // the stores re-render this view as they evaluate, so the counts are read at render time
  const al = window.useAlerts ? useAlerts() : null;
  const wk = window.useWork ? useWork() : null;
  const alertRev = al ? al.lastEval : 0, workRev = wk ? wk.version : 0;
  const clusters = React.useMemo(() => ((r.data || {}).rows || []).map(c => Object.assign({}, c, {
    alerts: al && al.ready ? al.countsFor(c.id) : NO_ALERTS,
    ops: wk && wk.ready ? wk.countsFor(c.id) : NO_OPS
  })), [r.data, alertRev, workRev]);
  const controlPlanes = (r.data || {}).controlPlanes || [];
  const nodesAt = (r.data || {}).nodesAt || {};
  const sites = React.useMemo(() => groupBy(clusters, "site").map(g => ({
    id: g.key, from: g.from, clusters: g.rows,
    nodes: (nodesAt[g.key] || {}).total || 0, nodesOnline: (nodesAt[g.key] || {}).online || 0,
    contexts: Array.from(new Set(g.rows.map(c => c.ctx))), stretched: g.rows.filter(c => c.stretchedSpec),
    capacity: {used: g.rows.reduce((n, c) => n + (c.capacity.used || 0), 0), total: g.rows.reduce((n, c) => n + (c.capacity.total || 0), 0)},
    alerts: g.rows.reduce((a, c) => ({critical: a.critical + c.alerts.critical, warning: a.warning + c.alerts.warning}), {critical: 0, warning: 0}),
    worst: g.rows.reduce((w, c) => Math.max(w, SM(c.phase).rank), 0)
  })), [clusters]);
  return Object.assign({}, r, {clusters, controlPlanes, sites, errors: window.worldErrors ? worldErrors() : {}});
}

// ---- primitives ----------------------------------------------------------------
// a bar with the cluster's own threshold ticks when its spec states them
function Meter({pct, warn, crit, mini, title}) {
  const p = Math.max(0, Number(pct) || 0);
  const c = crit && p >= crit ? "var(--bad)" : warn && p >= warn ? "var(--warn)" : "var(--accent)";
  return (
    <div className={"meter" + (mini ? " mini" : "")} title={title || `${p.toFixed(0)}%${warn ? ` · spec.warningThreshold.capacity ${warn}%` : ""}${crit ? ` · spec.criticalThreshold.capacity ${crit}%` : ""}`}>
      <div className="bar"><i style={{width: Math.min(100, p) + "%", "--bc": c}}></i>
        {warn && <span className="tick" style={{left: Math.min(100, warn) + "%"}}></span>}{crit && <span className="tick" style={{left: Math.min(100, crit) + "%"}}></span>}</div>
      <span className="mono mp" style={{color: c}}>{p.toFixed(0)}%</span>
    </div>
  );
}
const SevCount = ({critical, warning, onClick}) => (
  <span className={"sev" + (onClick ? " link" : "")} onClick={onClick}>
    {critical ? <b className="crit">{critical}</b> : null}{warning ? <b className="warn">{warning}</b> : null}
    {!critical && !warning ? <span className="none">0</span> : null}
  </span>
);
const OpsCount = ({running, failed, onClick}) => (
  <span className={"sev" + (onClick ? " link" : "")} onClick={onClick}>
    {running ? <b className="run">{running}</b> : null}{failed ? <b className="crit">{failed}</b> : null}
    {!running && !failed ? <span className="none">0</span> : null}
  </span>
);
function VirtualList({rows, rowHeight, render, height, overscan = 12}) {
  const [top, setTop] = React.useState(0);
  const h = height || 560;
  const start = Math.max(0, Math.floor(top / rowHeight) - overscan);
  const end = Math.min(rows.length, Math.ceil((top + h) / rowHeight) + overscan);
  return (
    <div className="vlist" style={{height: h}} onScroll={e => setTop(e.target.scrollTop)}>
      <div style={{height: rows.length * rowHeight, position: "relative"}}>
        <div style={{position: "absolute", top: start * rowHeight, left: 0, right: 0}}>{rows.slice(start, end).map((r, i) => render(r, start + i))}</div>
      </div>
    </div>
  );
}
const Provenance = ({from}) => from ? <span className="prov" title={from}>{from.length > 38 ? from.slice(0, 36) + "…" : from}</span> : null;
const cpPhase = cp => cp ? ((cp.status || {}).phase || "Pending") : null;

// ---- the roll-up -----------------------------------------------------------------
function FleetRollup({clusters, sites, controlPlanes, nav}) {
  const n = clusters.length;
  const byPhase = {};
  clusters.forEach(c => { byPhase[c.phase] = (byPhase[c.phase] || 0) + 1; });
  const online = byPhase.Online || 0;
  const worst = clusters.reduce((w, c) => Math.max(w, SM(c.phase).rank), 0);
  const nodes = clusters.reduce((a, c) => ({t: a.t + c.counts.nodes, o: a.o + c.counts.nodesOnline}), {t: 0, o: 0});
  const devs = clusters.reduce((a, c) => ({t: a.t + c.counts.devices, o: a.o + c.counts.devicesOnline}), {t: 0, o: 0});
  const cap = clusters.reduce((a, c) => ({u: a.u + (c.capacity.used || 0), t: a.t + (c.capacity.total || 0)}), {u: 0, t: 0});
  const alerts = clusters.reduce((a, c) => ({c: a.c + c.alerts.critical, w: a.w + c.alerts.warning}), {c: 0, w: 0});
  const ops = clusters.reduce((a, c) => ({r: a.r + c.ops.running, f: a.f + c.ops.failed}), {r: 0, f: 0});
  const cpAvail = controlPlanes.filter(cp => cpPhase(cp) === "Available").length;
  const others = Object.entries(byPhase).filter(([p]) => p !== "Online").map(([p, k]) => `${k} ${p}`).join(" · ");
  return (
    <div className="stats fleet">
      <Stat k="Clusters Online" v={`${online}/${n}`} c={worst >= 3 ? "var(--bad)" : worst >= 2 ? "var(--warn)" : undefined} s={others || "status.phase Online on every cluster"} />
      <Stat k="Nodes Online" v={`${nodes.o}/${nodes.t}`} c={nodes.o < nodes.t ? "var(--warn)" : undefined} s={`${sites.filter(s => s.id !== NONE).length} site${sites.filter(s => s.id !== NONE).length === 1 ? "" : "s"}`} />
      <Stat k="Devices Online" v={`${devs.o}/${devs.t}`} c={devs.o < devs.t ? "var(--warn)" : undefined} s={devs.t - devs.o ? `${devs.t - devs.o} not Online` : "all Online"} />
      <div className="stat"><div className="k">Capacity used <Derived formula="sum of StorageNode.status.resources.capacity over every cluster" /></div>
        <Meter pct={cap.t ? cap.u / cap.t * 100 : 0} /><div className="s">{fmtBytes(cap.u)} of {fmtBytes(cap.t)}</div></div>
      <div className="stat link" onClick={() => nav.alerts && nav.alerts()}><div className="k">Open alerts</div>
        <div className="v" style={{color: alerts.c ? "var(--bad)" : alerts.w ? "var(--warn)" : "var(--ok)"}}>{alerts.c}</div><div className="s">{alerts.w} warning</div></div>
      <div className="stat link" onClick={() => nav.monitor && nav.monitor({state: "running"})}><div className="k">Operations</div>
        <div className="v" style={{color: ops.f ? "var(--bad)" : ops.r ? "var(--info)" : undefined}}>{ops.r}</div><div className="s">running · {ops.f} failed</div></div>
      <div className="stat link" onClick={() => { const el = document.getElementById("cplanes"); if (el) el.scrollIntoView({behavior: "smooth"}); }}><div className="k">Control planes Available</div>
        <div className="v" style={{color: cpAvail < controlPlanes.length ? "var(--warn)" : undefined}}>{cpAvail}/{controlPlanes.length}</div><div className="s">ControlPlane.status.phase per context</div></div>
    </div>
  );
}

// ---- the matrix --------------------------------------------------------------------
const matrixCols = labelKey => ["", "Cluster", "Sites", labelKey ? labelKey.replace(/^(ctx)?label:/, "") : "Phase", "Namespace", "Alerts", "Ops", "Nodes", "Devices", "Capacity", "Step", "Control plane", ""];
function ClusterRow({r, nav, labelKey, style}) {
  const sm = SM(r.phase);
  const bad = sm.rank >= 3 ? " bad" : sm.rank >= 2 ? " warn" : "";
  const g = labelKey ? groupingOf(labelKey) : null;
  const cp = r.controlPlane;
  return (
    <div className={"cmxr" + bad} style={style} onDoubleClick={() => nav.openCluster(r.id)}>
      <span className="c-st"><TrafficLight status={r.phase} sm label={false} /></span>
      <span className="c-name"><button className="tlink" onClick={() => nav.openCluster(r.id)}>{r.name}</button>
        <span className="c-sub mono">{window.MULTI_CTX ? r.ctx + " · " : ""}{r.status || r.phase}</span></span>
      <span className="c-sites">{r.sites.length ? r.sites.slice(0, 2).map(s => <span key={s.value} className="lab sitelab" title={s.from} onClick={() => nav.openSite(s.value)}>{s.value}</span>) : <span className="none">{NONE}</span>}
        {r.sites.length > 2 && <span className="lab" title={r.sites.slice(2).map(s => s.value).join(", ")}>+{r.sites.length - 2}</span>}
        {r.stretchedSpec && <span className="badge" title="spec.stretched">stretched</span>}</span>
      <span className="c-stage">{g ? <span className="badge" title={g.from(r)}>{g.keysOf(r)[0]}</span> : <span className="badge" title="status.phase">{r.phase}</span>}</span>
      <span className="c-tenant mono" title="metadata.namespace">{r.namespace}</span>
      <span className="c-alerts"><SevCount critical={r.alerts.critical} warning={r.alerts.warning} onClick={nav.alerts ? () => nav.alerts({cluster: r.id}) : null} /></span>
      <span className="c-ops"><OpsCount running={r.ops.running} failed={r.ops.failed} onClick={nav.monitor ? () => nav.monitor({cluster: r.id}) : null} /></span>
      <span className="c-nodes mono" title="StorageNode.status.phase Online / all" style={{color: r.counts.nodesOnline < r.counts.nodes ? "var(--warn)" : undefined}}>{r.counts.nodesOnline}/{r.counts.nodes}</span>
      <span className="c-devs mono" title="StorageDevice.status.phase Online / all" style={{color: r.counts.devicesOnline < r.counts.devices ? "var(--warn)" : undefined}}>{r.counts.devicesOnline}/{r.counts.devices}</span>
      <span className="c-cap">{r.capacity.total ? <Meter mini pct={r.utilPct} warn={r.warnCap} crit={r.critCap} title={`${fmtBytes(r.capacity.used)} of ${fmtBytes(r.capacity.total)} · ${r.capacity.derived}`} /> : <span className="none">unset</span>}</span>
      <span className="c-iops mono" style={{fontSize: 10.5}} title={r.activeOpsRef ? "status.activeOpsRef" : "status.step.state"}>{r.activeOpsRef ? r.activeOpsRef : r.step || "unset"}</span>
      <span className="c-cp" title={cp ? `ControlPlane ${cp.metadata.name} status.phase` : "no ControlPlane object in this context"}><Dot c={cp ? SM(cpPhase(cp)).c : "var(--dim2)"} />{cp ? cpPhase(cp) : NONE}</span>
      <span className="c-act"><ActionBtn obj={REG[r.id] || r} /></span>
    </div>
  );
}
function ClusterMatrix({groups, nav, grouping}) {
  const [collapsed, setCollapsed] = useLocal("sb.fleet.collapsed", {});
  const labelKey = grouping.startsWith("label:") || grouping.startsWith("ctxlabel:") ? grouping : null;
  const flat = [];
  groups.forEach(g => {
    if (grouping !== "flat") flat.push({head: g});
    if (!collapsed[g.key]) g.rows.forEach(r => flat.push({row: r}));
  });
  const render = (it, i) => it.head
    ? <GroupRow key={"g" + it.head.key} g={it.head} grouping={grouping} collapsed={!!collapsed[it.head.key]} onToggle={() => setCollapsed(Object.assign({}, collapsed, {[it.head.key]: !collapsed[it.head.key]}))} nav={nav} />
    : <ClusterRow key={it.row.id + "|" + i} r={it.row} nav={nav} labelKey={labelKey} />;
  return (
    <div className="cmx">
      <div className="cmxh">{matrixCols(labelKey).map((c, i) => <span key={i}>{c}</span>)}</div>
      {flat.length > 80 ? <VirtualList rows={flat} rowHeight={36} render={render} /> : flat.map(render)}
    </div>
  );
}
function GroupRow({g, grouping, collapsed, onToggle, nav}) {
  const worst = g.rows.reduce((w, c) => Math.max(w, SM(c.phase).rank), 0);
  const alerts = g.rows.reduce((a, c) => ({c: a.c + c.alerts.critical, w: a.w + c.alerts.warning}), {c: 0, w: 0});
  const label = g.key === NONE ? `no ${groupingOf(grouping).label}` : g.key;
  return (
    <div className="cmxg" onClick={onToggle}>
      <span className="gt"><Icon n={collapsed ? "chev" : "chevd"} s={10} />{label}<Provenance from={g.from} /></span>
      <span className="gc"><Dot c={worst >= 3 ? "var(--bad)" : worst >= 2 ? "var(--warn)" : "var(--ok)"} />{g.rows.length} cluster{g.rows.length === 1 ? "" : "s"}</span>
      <span className="gc"><SevCount critical={alerts.c} warning={alerts.w} /></span>
      {grouping === "site" && g.key !== NONE && <button className="chip" onClick={e => { e.stopPropagation(); nav.openSite(g.key); }}>Open site</button>}
    </div>
  );
}

// ---- control planes ---------------------------------------------------------------------
function ControlPlanesCard({controlPlanes, nav}) {
  return (
    <div className="card" id="cplanes" style={{marginTop: 12}}><h3>Control planes · the ControlPlane object of each context</h3><div className="bd" style={{padding: 0}}>
      {!CONTEXTS.length ? null : <table className="dt"><thead><tr><th>Context</th><th>ControlPlane</th><th>status.phase</th><th>status.version</th><th>status.endpoint</th><th>status.components</th><th>activeOpsRef</th><th></th></tr></thead><tbody>
        {CONTEXTS.map(c => {
          const cps = controlPlanes.filter(cp => cp.__ctx === c.name);
          if (!cps.length) return <tr key={c.name}><td className="mono">{c.name}</td><td colSpan={6} style={{color: "var(--dim2)"}}>no ControlPlane object</td><td></td></tr>;
          return cps.map(cp => { const st = cp.status || {}; const comps = st.components || []; const notReady = comps.filter(x => (x.ready || 0) < (x.desired || 0)); return (
            <tr key={c.name + cp.metadata.name}>
              <td className="mono">{c.name}</td><td className="mono">{cp.metadata.namespace}/{cp.metadata.name}</td>
              <td><TrafficLight status={cpPhase(cp)} sm /></td><td className="mono">{st.version || "unset"}</td><td className="mono" style={{fontSize: 11}}>{st.endpoint || "unset"}</td>
              <td className="mono" title={comps.map(x => `${x.name} ${x.ready || 0}/${x.desired}`).join("\n")} style={{color: notReady.length ? "var(--warn)" : undefined}}>{comps.length ? `${comps.length - notReady.length}/${comps.length} ready` : "unset"}</td>
              <td className="mono">{st.activeOpsRef || ""}</td>
              <td style={{textAlign: "right", whiteSpace: "nowrap"}}>
                <button className="chip" onClick={() => nav.openCR({ctx: c.name, ns: cp.metadata.namespace, kind: "ControlPlane", name: cp.metadata.name})}>Resource</button>
                {c.local && nav.cp && <button className="chip" onClick={() => nav.cp()}>Pods and logs</button>}
              </td>
            </tr>); });
        })}
      </tbody></table>}
    </div></div>
  );
}

// the top bar's pill: how many ControlPlane objects report status.phase Available, across the contexts
function ControlPlanePill({nav}) {
  const r = useResource("cp-pill", () => Promise.all(CONTEXTS.map(c => k8s.list("ControlPlane", {allNamespaces: true, ctx: c.name}).catch(() => []))).then(rs => rs.flat()), 15000);
  const cps = r.data || [];
  if (!cps.length) return <span className="env" title="no ControlPlane object in any context"><span className="pulse" style={{background: "var(--dim2)"}}></span>no control plane</span>;
  const avail = cps.filter(cp => cpPhase(cp) === "Available").length;
  const ok = avail === cps.length;
  return (
    <span className="env" style={{cursor: "pointer"}} title={cps.map(cp => `${cp.metadata.namespace}/${cp.metadata.name}: ${cpPhase(cp)}`).join("\n")}
      onClick={() => { nav.dashboard(); setTimeout(() => { const el = document.getElementById("cplanes"); if (el) el.scrollIntoView({behavior: "smooth"}); }, 300); }}>
      <span className="pulse" style={ok ? undefined : {background: "var(--warn)"}}></span>{avail}/{cps.length} control plane{cps.length === 1 ? "" : "s"} Available
    </span>
  );
}

// ---- sites ---------------------------------------------------------------------------
function SiteCard({s, nav}) {
  const c = s.worst >= 3 ? "var(--bad)" : s.worst >= 2 ? "var(--warn)" : "var(--ok)";
  return (
    <div className="tile sitecard" style={{"--sc": c}} onDoubleClick={() => nav.openSite(s.id)}>
      <div className="th"><div style={{minWidth: 0, flex: 1}}><TrafficLight status={s.worst >= 3 ? "Degraded" : s.worst >= 2 ? "Degraded" : "Online"} label={s.worst >= 2 ? "a cluster is not Online" : "every cluster Online"} />
        <div className="tname">{s.id === NONE ? "No site" : s.id}</div><div className="tsub"><Provenance from={s.from} /></div></div></div>
      <div className="labels">
        <span className="lab"><i>clusters</i>{s.clusters.length}</span>
        <span className="lab" title="StorageNodes whose worker carries this zone"><i>nodes Online</i>{s.nodesOnline}/{s.nodes}</span>
        <span className="lab"><i>contexts</i>{s.contexts.join(", ")}</span>
        {s.stretched.length > 0 && <span className="lab"><i>stretched</i>{s.stretched.length}</span>}
      </div>
      {s.capacity.total > 0 && <Capacity total={s.capacity.total} used={s.capacity.used} />}
      <div className="kv" style={{marginTop: 8}}><div><span>Alerts</span><b><SevCount critical={s.alerts.critical} warning={s.alerts.warning} /></b></div></div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.openSite(s.id)}]} />
    </div>
  );
}
function SiteBoard({sites, nav}) {
  if (!sites.length) return <div className="empty"><Icon n="zone" s={24} /><b>No sites</b><span>A site is a stretched cluster's spec.stretched.site or the topology.kubernetes.io/zone label of the workers the storage nodes run on.</span></div>;
  return <div className="grid">{sites.map(s => <SiteCard key={s.id} s={s} nav={nav} />)}</div>;
}
function FleetSiteDetail({seg, nav, rev}) {
  const fleet = useFleet(rev);
  const site = fleet.sites.find(s => s.id === seg.id) || null;
  const {data: nodes} = useResource("sitenodes|" + seg.id + "|" + (site ? site.clusters.map(c => c.id).join(",") : ""),
    () => site ? Promise.all(site.clusters.map(c => api.nodes(c.id).catch(() => []))).then(rs => rs.flat()) : Promise.resolve([]), 15000);
  if (fleet.loading && !fleet.data) return <div className="scroll"><div className="lmsg">loading…</div></div>;
  if (!site) return <div className="scroll"><div className="empty"><Icon n="zone" s={24} /><b>Site {seg.id} is not known</b><span>No stretched cluster names it and no worker carries it as a zone.</span></div></div>;
  const nodeRows = (nodes || []).filter(n => site.id === NONE ? !n.zone && !(n.config || {}).site : n.zone === site.id || (n.config || {}).site === site.id);
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>{site.id === NONE ? "No site" : site.id}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Site <Provenance from={site.from} /> · {site.clusters.length} cluster{site.clusters.length === 1 ? "" : "s"} across {site.contexts.length} context{site.contexts.length === 1 ? "" : "s"}</div></div></div>
      <div className="stats">
        <Stat k="Clusters" v={site.clusters.length} s={site.stretched.length ? `${site.stretched.length} stretched` : ""} />
        <Stat k="Nodes Online" v={`${site.nodesOnline}/${site.nodes}`} c={site.nodesOnline < site.nodes ? "var(--warn)" : undefined} s="StorageNodes at this site" />
        <div className="stat"><div className="k">Capacity used <Derived formula="sum of StorageNode.status.resources.capacity over the whole of every cluster with a node at this site" /></div><Meter pct={site.capacity.total ? site.capacity.used / site.capacity.total * 100 : 0} /><div className="s">{fmtBytes(site.capacity.used)} of {fmtBytes(site.capacity.total)}</div></div>
        <div className="stat"><div className="k">Open alerts</div><div className="v"><SevCount critical={site.alerts.critical} warning={site.alerts.warning} /></div></div>
      </div>
      <div className="sech"><h2>Clusters at this site</h2><span className="ln"></span></div>
      <ClusterMatrix groups={[{key: site.id, from: site.from, rows: site.clusters}]} nav={nav} grouping="flat" />
      <div className="sech" style={{marginTop: 16}}><h2>Storage nodes at this site</h2><span className="ln"></span><span className="count">{nodeRows.length}</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>StorageNode</th><th>spec.clusterRef</th><th>status.phase</th><th>zone</th><th>spec.workerNode</th><th>Context</th></tr></thead><tbody>
          {nodeRows.map(n => <tr key={n.id}>
            <td><button className="tlink" onClick={() => nav.openNode(n.clusterId, n.id)}>{n.name}</button></td>
            <td className="mono">{n.clusterRef}</td>
            <td><TrafficLight status={n.phase} sm /></td>
            <td className="mono" title="topology.kubernetes.io/zone of the worker Node">{n.zone || (n.config || {}).site || "unset"}</td>
            <td className="mono">{n.workerNode}</td>
            <td className="mono">{n.ctx}</td>
          </tr>)}
        </tbody></table>
      </div></div>
    </div>
  );
}
function SitesView({nav, rev}) {
  const fleet = useFleet(rev);
  return (
    <div className="scroll">
      <div className="sech"><h2>Sites</h2><span className="ln"></span><span className="count">{fleet.sites.length}</span></div>
      {fleet.loading && !fleet.data ? <div className="grid">{Array.from({length: 4}).map((_, i) => <div className="skel" key={i}></div>)}</div> : <SiteBoard sites={fleet.sites} nav={nav} />}
    </div>
  );
}

// ---- the view ----------------------------------------------------------------------------
function DashboardView({nav, seg, rev}) {
  const p = seg.p || {};
  const [group, setGroup] = useLocal("sb.fleet.group", window.MULTI_CTX ? "context" : "namespace");
  const [view, setView] = useLocal("sb.fleet.view", "matrix");
  const [q, setQ] = React.useState(p.q || "");
  const [phases, setPhases] = React.useState(p.phase || []);
  const [sort, setSort] = React.useState(p.sort || "health");
  React.useEffect(() => { if (p.group) setGroup(p.group); if (p.view) setView(p.view); }, [p.group, p.view]);
  const fleet = useFleet(rev);
  const all = fleet.clusters;
  const counts = {};
  all.forEach(c => { counts[c.phase] = (counts[c.phase] || 0) + 1; });
  const keys = groupingKeys(all);
  const groupKey = keys.some(k => k.k === group) ? group : (keys[0] || {k: "flat"}).k;
  const filtered = React.useMemo(() => {
    const s = q.trim().toLowerCase();
    const cmp = {
      health: (a, b) => SM(b.phase).rank - SM(a.phase).rank || b.alerts.critical - a.alerts.critical || a.name.localeCompare(b.name),
      name: (a, b) => a.name.localeCompare(b.name),
      util: (a, b) => b.utilPct - a.utilPct, capacity: (a, b) => (b.capacity.total || 0) - (a.capacity.total || 0),
      alerts: (a, b) => b.alerts.critical - a.alerts.critical || b.alerts.warning - a.alerts.warning
    }[sort] || ((a, b) => a.name.localeCompare(b.name));
    return all.filter(c => (!phases.length || phases.includes(c.phase))
      && (!s || [c.name, c.id, c.ctx, c.namespace, c.phase, c.status].concat(c.sites.map(x => x.value), Object.values(c.labels || {})).filter(Boolean).join(" ").toLowerCase().includes(s)))
      .sort(cmp);
  }, [all, q, phases, sort]);
  const groups = React.useMemo(() => groupBy(filtered, groupKey), [filtered, groupKey]);
  const ctxErrors = Object.entries(fleet.errors || {}).filter(([, e]) => e && Object.keys(e).length);
  const phaseOrder = Object.keys(counts).sort((a, b) => SM(b).rank - SM(a).rank);
  return (
    <>
      <div className="toolbar">
        <div className="search"><Icon n="search" s={13} c="var(--dim2)" />
          <input value={q} placeholder="Search name, namespace, context, site, label…" onChange={e => setQ(e.target.value)} />
          {q && <button onClick={() => setQ("")} style={{display: "flex", color: "var(--dim2)"}}><Icon n="x" s={11} /></button>}</div>
        <div className="chips">{phaseOrder.map(s => <button key={s} className={"chip" + (phases.includes(s) ? " on" : "")} title="status.phase"
          onClick={() => setPhases(phases.includes(s) ? phases.filter(x => x !== s) : phases.concat([s]))}><Dot c={SM(s).c} />{s}<span className="n">{counts[s]}</span></button>)}</div>
        <div className="spacer"></div>
        <span className="count">{filtered.length}</span>
        <select className="sel" value={groupKey} onChange={e => setGroup(e.target.value)} title="Group clusters by">
          {keys.map(k => <option key={k.k} value={k.k}>by {k.l}</option>)}
        </select>
        <select className="sel" value={sort} onChange={e => setSort(e.target.value)}>
          <option value="health">Unhealthy first</option><option value="alerts">Most alerts</option><option value="name">Name (A–Z)</option>
          <option value="util">Utilization ↓</option><option value="capacity">Capacity ↓</option>
        </select>
        <div className="seg" title="View">
          {[["matrix", "list"], ["tiles", "cluster"], ["sites", "zone"]].map(([k, ic]) => <button key={k} className={view === k ? "on" : ""} onClick={() => setView(k)} title={k}><Icon n={ic} s={12} /></button>)}
        </div>
        <button className="chip" onClick={fleet.reload} title="Refresh"><Icon n="refresh" s={12} /></button>
      </div>
      <div className="scroll">
        {ctxErrors.map(([ctx, errs]) => <div className="banner" key={ctx}><Icon n="alert" s={15} />
          <span><b>Context {ctx} is partly unreachable.</b> {Object.entries(errs).map(([k, e]) => `${k}: ${e.message || e}`).join(" · ")}</span></div>)}
        {fleet.error && <ErrorState error={fleet.error} onRetry={fleet.reload} />}
        {fleet.loading && !all.length ? <div className="stats">{Array.from({length: 7}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div>
          : <FleetRollup clusters={filtered} sites={fleet.sites} controlPlanes={fleet.controlPlanes} nav={nav} />}
        {!fleet.loading && !all.length && !fleet.error && <div className="empty"><Icon n="cluster" s={24} /><b>No clusters</b><span>No StorageCluster in any configured context.</span></div>}
        {all.length > 0 && !filtered.length && <div className="empty"><Icon n="search" s={22} /><b>No clusters match</b><button className="chip" style={{marginTop: 6}} onClick={() => { setQ(""); setPhases([]); }}>Reset filters</button></div>}
        {filtered.length > 0 && view === "matrix" && <ClusterMatrix groups={groups} nav={nav} grouping={groupKey} />}
        {filtered.length > 0 && view === "tiles" && groups.map(g => <div key={g.key}>
          {groupKey !== "flat" && <div className="grouplbl">{g.key === NONE ? `no ${groupingOf(groupKey).label}` : g.key}<Provenance from={g.from} /></div>}
          <div className="grid">{g.rows.map(c => <ClusterTile key={c.id} c={REG[c.id] || c} nav={nav} />)}</div></div>)}
        {filtered.length > 0 && view === "sites" && <SiteBoard sites={fleet.sites.filter(s => s.clusters.some(c => filtered.includes(c)))} nav={nav} />}
        {!fleet.loading && <ControlPlanesCard controlPlanes={fleet.controlPlanes} nav={nav} />}
      </div>
    </>
  );
}

Object.assign(window, {ControlPlanePill, DashboardView, SitesView, SiteBoard, FleetSiteDetail, ClusterMatrix, FleetRollup, ControlPlanesCard, Meter, SevCount, OpsCount, VirtualList, groupBy, groupingOf, groupingKeys, useFleet, loadFleet, Provenance, SM});
