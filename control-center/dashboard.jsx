// ---------------------------------------------------------------------------
// FLEET DASHBOARD — the root of the Clusters section
//
// One view that fits every deployment shape: two sites with a cluster each,
// a lifecycle ladder (dev, uat, staging, prd), tenants or namespaces, a dozen
// edge clusters inside one Kubernetes cluster, or one Kubernetes context per
// edge site. The grouping switch picks the dimension (site, stage, tenant,
// context, or flat); the matrix stays readable at two rows and at two
// hundred. Sites are derived from tagged nodes (fleet-model.jsx) unless a
// cluster or its namespace says otherwise, and every grouping value shows
// where it came from.
//
// Alert and operation counts come from the alert and work stores when those
// modules are loaded; until then the stubs in fleet-model.jsx answer zeros.
// ---------------------------------------------------------------------------
const SM = s => STATUS_META[s] || {c: "var(--idle)", rank: 1, label: s || "unknown"};
const FLEET_POLL = 10000;
const DEFAULT_THRESHOLDS = {capacity: {warn: 80, crit: 95}, provisioned: {warn: 100, crit: 120}};

const GROUPINGS = {
  site: {label: "Site", keysOf: c => (c.sites.length ? c.sites.map(s => s.value) : ["unassigned"]), from: c => (c.sites[0] || {}).from, multi: true},
  stage: {label: "Stage", keysOf: c => [c.stage ? c.stage.value.toUpperCase() : "unassigned"], from: c => (c.stage || {}).from, order: (a, b) => stageRank(a) - stageRank(b) || a.localeCompare(b)},
  tenant: {label: "Tenant", keysOf: c => [c.tenant ? c.tenant.value : (c.ns || "unassigned")], from: c => (c.tenant || {}).from},
  context: {label: "Kubernetes context", keysOf: c => [c.ctx], from: () => "context"},
  flat: {label: "Flat", keysOf: () => [""], from: () => null}
};
const groupBy = (clusters, key) => {
  const g = GROUPINGS[key] || GROUPINGS.flat;
  const map = new Map();
  clusters.forEach(c => g.keysOf(c).forEach(k => { if (!map.has(k)) map.set(k, {key: k, from: g.from(c), rows: []}); map.get(k).rows.push(c); }));
  const groups = Array.from(map.values());
  groups.sort(g.order ? (a, b) => g.order(a.key, b.key) : (a, b) => (a.key === "unassigned") - (b.key === "unassigned") || a.key.localeCompare(b.key));
  return groups;
};

// ---- the fleet -----------------------------------------------------------------
// Clusters from every context, enriched with the live I/O history of the ones
// whose control plane is reachable (eight requests in flight at a time, so a
// large fleet does not fan out hundreds of calls per tick).
async function loadFleet() {
  const clusters = await api.clusters();
  const live = clusters.filter(c => hasControlPlane(c.ctx) && isUuid(c.id));
  const hist = new Map();
  for (let i = 0; i < live.length; i += 8) {
    await Promise.all(live.slice(i, i + 8).map(c => cp.iostats(c.ctx, c.id, "1h").then(s => hist.set(c.id, s.points)).catch(() => {})));
  }
  const counts = contextErrors();
  return clusters.map(c => {
    const pts = hist.get(c.id) || [];
    const iopsHist = pts.map(p => (Number(p.read_io_ps) || 0) + (Number(p.write_io_ps) || 0)).slice(-28);
    const last = pts[pts.length - 1] || {};
    const th = c.thresholds || {};
    const used = c.capacity.used || 0, total = c.capacity.total || 0, prov = c.provisioned || 0;
    return Object.assign({}, c, {
      iopsHist, iopsNow: (Number(last.read_io_ps) || 0) + (Number(last.write_io_ps) || 0),
      utilPct: total ? used / total * 100 : 0, provPct: total && prov ? prov / total * 100 : 0,
      warn: {cap: (th.warn || {}).capacity || DEFAULT_THRESHOLDS.capacity.warn, prov: (th.warn || {}).provisionedCapacity || DEFAULT_THRESHOLDS.provisioned.warn},
      crit: {cap: (th.crit || {}).capacity || DEFAULT_THRESHOLDS.capacity.crit, prov: (th.crit || {}).provisionedCapacity || DEFAULT_THRESHOLDS.provisioned.crit},
      alerts: window.alerts.countsFor(c.id), ops: window.work.countsFor(c.id),
      cp: {reachable: hasControlPlane(c.ctx), latencyMs: (window.alerts.latencyOf ? window.alerts.latencyOf(c.id) : null)},
      ctxError: counts[c.ctx] && Object.keys(counts[c.ctx]).length ? counts[c.ctx] : null
    });
  });
}
function useFleet(rev) {
  const r = useResource("fleet|" + rev, loadFleet, FLEET_POLL);
  const clusters = r.data || [];
  const sites = React.useMemo(() => groupBy(clusters, "site").map(g => ({
    id: g.key, from: g.from, clusters: g.rows,
    nodes: g.rows.reduce((n, c) => n + c.counts.nodes, 0), nodesOnline: g.rows.reduce((n, c) => n + c.counts.nodesOnline, 0),
    contexts: Array.from(new Set(g.rows.map(c => c.ctx))), stretched: g.rows.filter(c => c.stretched),
    capacity: {used: g.rows.reduce((n, c) => n + (c.capacity.used || 0), 0), total: g.rows.reduce((n, c) => n + (c.capacity.total || 0), 0)},
    alerts: g.rows.reduce((a, c) => ({critical: a.critical + c.alerts.critical, warning: a.warning + c.alerts.warning}), {critical: 0, warning: 0}),
    worst: g.rows.reduce((w, c) => Math.max(w, SM(c.status).rank), 0)
  })), [r.data]);
  return Object.assign({}, r, {clusters, sites, errors: contextErrors()});
}

// ---- primitives ----------------------------------------------------------------
// a bar with threshold ticks; the color follows the thresholds
function Meter({pct, warn, crit, label, mini, title}) {
  const p = Math.max(0, Number(pct) || 0);
  const c = p >= crit ? "var(--bad)" : p >= warn ? "var(--warn)" : "var(--accent)";
  return (
    <div className={"meter" + (mini ? " mini" : "")} title={title || `${p.toFixed(0)}% · warning at ${warn}%, critical at ${crit}%`}>
      {label && <span className="ml">{label}</span>}
      <div className="bar"><i style={{width: Math.min(100, p) + "%", "--bc": c}}></i>
        <span className="tick" style={{left: Math.min(100, warn) + "%"}}></span><span className="tick" style={{left: Math.min(100, crit) + "%"}}></span></div>
      <span className="mono mp" style={{color: c}}>{p.toFixed(0)}%</span>
    </div>
  );
}
const SevCount = ({critical, warning, info, onClick}) => (
  <span className={"sev" + (onClick ? " link" : "")} onClick={onClick}>
    {critical ? <b className="crit">{critical}</b> : null}
    {warning ? <b className="warn">{warning}</b> : null}
    {info ? <b className="info">{info}</b> : null}
    {!critical && !warning && !info ? <span className="none">—</span> : null}
  </span>
);
const OpsCount = ({running, failed, onClick}) => (
  <span className={"sev" + (onClick ? " link" : "")} onClick={onClick}>
    {running ? <b className="run">{running}</b> : null}
    {failed ? <b className="crit">{failed}</b> : null}
    {!running && !failed ? <span className="none">—</span> : null}
  </span>
);
// windowed rendering for long lists; rows must be fixed height
function VirtualList({rows, rowHeight, render, height, overscan = 12}) {
  const [top, setTop] = React.useState(0);
  const ref = React.useRef(null);
  const h = height || 560;
  const start = Math.max(0, Math.floor(top / rowHeight) - overscan);
  const end = Math.min(rows.length, Math.ceil((top + h) / rowHeight) + overscan);
  return (
    <div className="vlist" style={{height: h}} ref={ref} onScroll={e => setTop(e.target.scrollTop)}>
      <div style={{height: rows.length * rowHeight, position: "relative"}}>
        <div style={{position: "absolute", top: start * rowHeight, left: 0, right: 0}}>
          {rows.slice(start, end).map((r, i) => render(r, start + i))}
        </div>
      </div>
    </div>
  );
}
const Provenance = ({from}) => from ? <span className="prov" title={`derived from the ${from.replace("-", " ")}`}>{from.replace("-", " ")}</span> : null;

// ---- the roll-up -----------------------------------------------------------------
function FleetRollup({clusters, sites, nav, onPick}) {
  const n = clusters.length;
  const by = s => clusters.filter(c => c.status === s).length;
  const healthy = clusters.filter(c => ["active", "online"].includes(c.status)).length;
  const worst = clusters.reduce((w, c) => Math.max(w, SM(c.status).rank), 0);
  const nodes = clusters.reduce((a, c) => ({t: a.t + c.counts.nodes, o: a.o + c.counts.nodesOnline, p: a.p + (c.live ? 0 : 0)}), {t: 0, o: 0, p: 0});
  const planned = clusters.reduce((a, c) => a + ((c.raw && c.raw.nodes_planned_offline) || 0), 0);
  const devs = clusters.reduce((a, c) => ({t: a.t + c.counts.devices, o: a.o + c.counts.devicesOnline}), {t: 0, o: 0});
  const cap = clusters.reduce((a, c) => ({u: a.u + (c.capacity.used || 0), t: a.t + (c.capacity.total || 0), p: a.p + (c.provisioned || 0)}), {u: 0, t: 0, p: 0});
  const warnCap = clusters.length ? Math.min(...clusters.map(c => c.warn.cap)) : DEFAULT_THRESHOLDS.capacity.warn;
  const critCap = clusters.length ? Math.min(...clusters.map(c => c.crit.cap)) : DEFAULT_THRESHOLDS.capacity.crit;
  const warnProv = clusters.length ? Math.min(...clusters.map(c => c.warn.prov)) : DEFAULT_THRESHOLDS.provisioned.warn;
  const critProv = clusters.length ? Math.min(...clusters.map(c => c.crit.prov)) : DEFAULT_THRESHOLDS.provisioned.crit;
  const alerts = clusters.reduce((a, c) => ({c: a.c + c.alerts.critical, w: a.w + c.alerts.warning}), {c: 0, w: 0});
  const ops = clusters.reduce((a, c) => ({r: a.r + c.ops.running, f: a.f + c.ops.failed}), {r: 0, f: 0});
  const cps = clusters.filter(c => c.cp.reachable).length;
  const approx = clusters.some(c => c.capacityApprox);
  return (
    <div className="stats fleet">
      <Stat k="Clusters" v={`${healthy}/${n}`} c={worst >= 3 ? "var(--bad)" : worst >= 2 ? "var(--warn)" : undefined}
        s={[by("degraded") && `${by("degraded")} degraded`, by("suspended") && `${by("suspended")} suspended`, by("unready") && `${by("unready")} unready`].filter(Boolean).join(" · ") || "all healthy"} />
      <Stat k="Nodes online" v={`${nodes.o}/${nodes.t}`} c={nodes.o < nodes.t ? "var(--warn)" : undefined} s={planned ? `${planned} planned offline` : `${sites.length} site${sites.length === 1 ? "" : "s"}`} />
      <Stat k="Devices online" v={`${devs.o}/${devs.t}`} c={devs.o < devs.t ? "var(--warn)" : undefined} s={devs.t - devs.o ? `${devs.t - devs.o} not online` : "all online"} />
      <div className="stat"><div className="k">Capacity used{approx && <span title="summed from node status; no control plane reachable for a live figure"> · approx.</span>}</div>
        <Meter pct={cap.t ? cap.u / cap.t * 100 : 0} warn={warnCap} crit={critCap} /><div className="s">{fmtBytes(cap.u)} of {fmtBytes(cap.t)}</div></div>
      <div className="stat"><div className="k">Provisioned</div>
        {cap.p ? <><Meter pct={cap.t ? cap.p / cap.t * 100 : 0} warn={warnProv} crit={critProv} /><div className="s">{fmtBytes(cap.p)} provisioned</div></>
          : <><div className="v sm" style={{color: "var(--dim2)"}}>—</div><div className="s">needs a reachable control plane</div></>}</div>
      <div className="stat link" onClick={() => nav.alerts && nav.alerts()}><div className="k">Open alerts</div>
        <div className="v" style={{color: alerts.c ? "var(--bad)" : alerts.w ? "var(--warn)" : "var(--ok)"}}>{alerts.c}</div><div className="s">{alerts.w} warning{window.alerts.ready ? "" : " · alert engine not loaded"}</div></div>
      <div className="stat link" onClick={() => nav.monitor && nav.monitor({state: "running"})}><div className="k">Operations</div>
        <div className="v" style={{color: ops.f ? "var(--bad)" : ops.r ? "var(--info)" : undefined}}>{ops.r}</div><div className="s">running · {ops.f} failed</div></div>
      <Stat k="Control planes" v={`${cps}/${n}`} c={cps < n ? "var(--warn)" : undefined} s={cps < n ? `${n - cps} not reachable from the console` : "all reachable"} />
    </div>
  );
}

// ---- the matrix --------------------------------------------------------------------
const MATRIX_COLS = ["", "Cluster", "Sites", "Stage", "Tenant", "Alerts", "Ops", "Nodes", "Devices", "Capacity", "IOPS", "CP", ""];
function ClusterRow({r, nav, style}) {
  const sm = SM(r.status);
  const bad = sm.rank >= 3 ? " bad" : sm.rank >= 2 ? " warn" : "";
  return (
    <div className={"cmxr" + bad} style={style} onDoubleClick={() => nav.openCluster(r.id)}>
      <span className="c-st"><TrafficLight status={r.status} sm /></span>
      <span className="c-name"><button className="tlink" onClick={() => nav.openCluster(r.id)}>{r.name}</button>
        <span className="c-sub mono">{r.ctx}{r.ns ? " · " + r.ns : ""}</span></span>
      <span className="c-sites">{r.sites.length ? r.sites.map(s => <span key={s.value} className="lab sitelab" title={`site from the ${s.from.replace("-", " ")}`} onClick={() => nav.openSite(s.value)}>{s.value}</span>) : <span className="none">—</span>}
        {r.stretched && <span className="badge" title="workers span several sites">stretched</span>}</span>
      <span className="c-stage">{r.stage ? <span className="badge stage" title={`stage from the ${r.stage.from.replace("-", " ")}`}>{r.stage.value.toUpperCase()}</span> : <span className="none">—</span>}</span>
      <span className="c-tenant">{r.tenant ? <span title={`tenant from the ${r.tenant.from.replace("-", " ")}`}>{r.tenant.value}</span> : <span className="none">—</span>}</span>
      <span className="c-alerts"><SevCount critical={r.alerts.critical} warning={r.alerts.warning} onClick={nav.alerts ? () => nav.alerts({cluster: r.id}) : null} /></span>
      <span className="c-ops"><OpsCount running={r.ops.running} failed={r.ops.failed} onClick={nav.monitor ? () => nav.monitor({cluster: r.id}) : null} /></span>
      <span className="c-nodes mono" style={{color: r.counts.nodesOnline < r.counts.nodes ? "var(--warn)" : undefined}}>{r.counts.nodesOnline}/{r.counts.nodes}</span>
      <span className="c-devs mono" style={{color: r.counts.devicesOnline < r.counts.devices ? "var(--warn)" : undefined}}>{r.counts.devicesOnline}/{r.counts.devices}</span>
      <span className="c-cap"><Meter mini pct={r.utilPct} warn={r.warn.cap} crit={r.crit.cap} title={`${fmtBytes(r.capacity.used)} of ${fmtBytes(r.capacity.total)}${r.capacityApprox ? " (approx., summed from node status)" : ""}`} /></span>
      <span className="c-iops">{r.iopsHist.length ? <><Sparkline data={r.iopsHist} w={64} h={16} /><span className="mono">{fmtNum(r.iopsNow)}</span></> : <span className="none" title="no control plane reachable for live I/O">—</span>}</span>
      <span className="c-cp"><Dot c={r.cp.reachable ? "var(--ok)" : "var(--dim2)"} />{r.cp.reachable ? (r.cp.latencyMs !== null && r.cp.latencyMs !== undefined ? `${r.cp.latencyMs} ms` : "ok") : "n/a"}</span>
      <span className="c-act"><ActionBtn obj={REG[r.id] || r} /></span>
    </div>
  );
}
function ClusterMatrix({groups, nav, grouping}) {
  const [collapsed, setCollapsed] = useLocal("sb.fleet.collapsed", {});
  const flat = [];
  groups.forEach(g => {
    if (grouping !== "flat") flat.push({head: g});
    if (!collapsed[g.key]) g.rows.forEach(r => flat.push({row: r}));
  });
  const render = (it, i) => it.head
    ? <GroupRow key={"g" + it.head.key} g={it.head} grouping={grouping} collapsed={!!collapsed[it.head.key]} onToggle={() => setCollapsed(Object.assign({}, collapsed, {[it.head.key]: !collapsed[it.head.key]}))} nav={nav} />
    : <ClusterRow key={it.row.id + "|" + i} r={it.row} nav={nav} />;
  return (
    <div className="cmx">
      <div className="cmxh">{MATRIX_COLS.map((c, i) => <span key={i}>{c}</span>)}</div>
      {flat.length > 80 ? <VirtualList rows={flat} rowHeight={36} render={render} /> : flat.map(render)}
    </div>
  );
}
function GroupRow({g, grouping, collapsed, onToggle, nav}) {
  const worst = g.rows.reduce((w, c) => Math.max(w, SM(c.status).rank), 0);
  const alerts = g.rows.reduce((a, c) => ({c: a.c + c.alerts.critical, w: a.w + c.alerts.warning}), {c: 0, w: 0});
  const label = g.key === "unassigned" ? `no ${GROUPINGS[grouping].label.toLowerCase()}` : g.key;
  return (
    <div className="cmxg" onClick={onToggle}>
      <span className="gt"><Icon n={collapsed ? "chev" : "chevd"} s={10} />{label}<Provenance from={g.from} /></span>
      <span className="gc"><Dot c={worst >= 3 ? "var(--bad)" : worst >= 2 ? "var(--warn)" : "var(--ok)"} />{g.rows.length} cluster{g.rows.length === 1 ? "" : "s"}</span>
      <span className="gc"><SevCount critical={alerts.c} warning={alerts.w} /></span>
      {grouping === "site" && g.key !== "unassigned" && <button className="chip" onClick={e => { e.stopPropagation(); nav.openSite(g.key); }}>Open site</button>}
    </div>
  );
}

// ---- sites ---------------------------------------------------------------------------
function SiteCard({s, nav}) {
  const c = s.worst >= 3 ? "var(--bad)" : s.worst >= 2 ? "var(--warn)" : "var(--ok)";
  return (
    <div className="tile sitecard" style={{"--sc": c}} onDoubleClick={() => nav.openSite(s.id)}>
      <div className="th"><div style={{minWidth: 0, flex: 1}}><TrafficLight status={s.worst >= 3 ? "degraded" : s.worst >= 2 ? "degraded" : "online"} label={s.worst >= 3 ? "needs attention" : s.worst >= 2 ? "degraded" : "healthy"} />
        <div className="tname">{s.id === "unassigned" ? "No site" : s.id}</div><div className="tsub"><Provenance from={s.from} /></div></div></div>
      <div className="labels">
        <span className="lab"><i>clusters</i>{s.clusters.length}</span>
        <span className="lab"><i>nodes</i>{s.nodesOnline}/{s.nodes}</span>
        <span className="lab"><i>contexts</i>{s.contexts.join(", ")}</span>
        {s.stretched.length > 0 && <span className="lab"><i>stretched</i>{s.stretched.length}</span>}
      </div>
      <Capacity total={s.capacity.total} used={s.capacity.used} />
      <div className="kv" style={{marginTop: 8}}><div><span>Alerts</span><b><SevCount critical={s.alerts.critical} warning={s.alerts.warning} /></b></div></div>
      <Foot items={[{label: "Clusters", count: s.clusters.length, icon: "cluster", onClick: () => nav.openSite(s.id)}, {label: "Details", right: true, onClick: () => nav.openSite(s.id)}]} />
    </div>
  );
}
function SiteBoard({sites, nav}) {
  if (!sites.length) return <div className="empty"><Icon n="zone" s={24} /><b>No sites yet</b><span>Label a StorageCluster, its Namespace, a context, or the worker Nodes' zone to place clusters on a site.</span></div>;
  return <div className="grid">{sites.map(s => <SiteCard key={s.id} s={s} nav={nav} />)}</div>;
}
function FleetSiteDetail({seg, nav, rev}) {
  const fleet = useFleet(rev);
  const site = fleet.sites.find(s => s.id === seg.id) || null;
  const {data: nodes} = useResource("sitenodes|" + seg.id + "|" + (site ? site.clusters.map(c => c.id).join(",") : ""),
    () => site ? Promise.all(site.clusters.map(c => api.nodes(c.id).catch(() => []))).then(rs => rs.flat()) : Promise.resolve([]), 15000);
  if (fleet.loading && !fleet.data) return <div className="scroll"><div className="lmsg">loading…</div></div>;
  if (!site) return <div className="scroll"><div className="empty"><Icon n="zone" s={24} /><b>Site {seg.id} is not known</b><span>No cluster, namespace, context, or node zone names it.</span></div></div>;
  const nodeRows = (nodes || []).filter(n => !n.zone || site.from !== "node-zone" || n.zone === site.id);
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>{site.id}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Site <Provenance from={site.from} /> · {site.clusters.length} cluster{site.clusters.length === 1 ? "" : "s"} across {site.contexts.length} context{site.contexts.length === 1 ? "" : "s"}</div></div></div>
      <div className="stats">
        <Stat k="Clusters" v={site.clusters.length} s={site.stretched.length ? `${site.stretched.length} stretched` : ""} />
        <Stat k="Nodes online" v={`${site.nodesOnline}/${site.nodes}`} c={site.nodesOnline < site.nodes ? "var(--warn)" : undefined} />
        <div className="stat"><div className="k">Capacity used</div><Meter pct={site.capacity.total ? site.capacity.used / site.capacity.total * 100 : 0} warn={80} crit={95} /><div className="s">{fmtBytes(site.capacity.used)} of {fmtBytes(site.capacity.total)}</div></div>
        <div className="stat"><div className="k">Open alerts</div><div className="v"><SevCount critical={site.alerts.critical} warning={site.alerts.warning} /></div></div>
      </div>
      <div className="sech"><h2>Clusters at this site</h2><span className="ln"></span></div>
      <ClusterMatrix groups={[{key: site.id, from: site.from, rows: site.clusters}]} nav={nav} grouping="flat" />
      <div className="sech" style={{marginTop: 16}}><h2>Storage nodes at this site</h2><span className="ln"></span><span className="count">{nodeRows.length}</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Node</th><th>Cluster</th><th>Status</th><th>Zone</th><th>Worker</th><th>Context</th></tr></thead><tbody>
          {nodeRows.map(n => <tr key={n.id}>
            <td><button className="tlink" onClick={() => nav.openNode(n.clusterId, n.id)}>{n.hostname}</button></td>
            <td>{regName(n.clusterId, "cluster")}</td>
            <td><TrafficLight status={n.status} sm /></td>
            <td className="mono">{n.zone || "—"}</td>
            <td className="mono">{n.worker || "—"}</td>
            <td className="mono">{n.ctx}</td>
          </tr>)}
        </tbody></table>
      </div></div>
      <div className="sech" style={{marginTop: 16}}><h2>Kubernetes contexts</h2><span className="ln"></span></div>
      <div className="labels">{site.contexts.map(c => <span key={c} className="lab link" onClick={() => nav.openK8s(c)}><i>context</i>{c}</span>)}</div>
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
  const [group, setGroup] = useLocal("sb.fleet.group", "site");
  const [view, setView] = useLocal("sb.fleet.view", "matrix");
  const [q, setQ] = React.useState(p.q || "");
  const [status, setStatus] = React.useState(p.status || []);
  const [sort, setSort] = React.useState(p.sort || "health");
  React.useEffect(() => { if (p.group) setGroup(p.group); if (p.view) setView(p.view); }, [p.group, p.view]);
  // the alert and work stores land after the first fleet read; re-read when they move
  const [tick, setTick] = React.useState(0);
  React.useEffect(() => {
    const bump = () => setTick(t => t + 1);
    const subs = [window.alerts, window.work].filter(s => s && s.subscribe).map(s => s.subscribe(bump));
    return () => subs.forEach(u => u && u());
  }, []);
  const fleet = useFleet(`${rev}|${tick}`);
  const all = fleet.clusters;
  const counts = {};
  all.forEach(c => { counts[c.status] = (counts[c.status] || 0) + 1; });
  const filtered = React.useMemo(() => {
    const s = q.trim().toLowerCase();
    const cmp = {
      health: (a, b) => SM(b.status).rank - SM(a.status).rank || b.alerts.critical - a.alerts.critical || a.name.localeCompare(b.name),
      name: (a, b) => a.name.localeCompare(b.name),
      util: (a, b) => b.utilPct - a.utilPct, capacity: (a, b) => (b.capacity.total || 0) - (a.capacity.total || 0),
      iops: (a, b) => b.iopsNow - a.iopsNow, alerts: (a, b) => b.alerts.critical - a.alerts.critical || b.alerts.warning - a.alerts.warning
    }[sort] || ((a, b) => a.name.localeCompare(b.name));
    return all.filter(c => (!status.length || status.includes(c.status))
      && (!s || [c.name, c.id, c.ctx, c.ns, c.tenant && c.tenant.value, c.stage && c.stage.value].concat(c.sites.map(x => x.value)).filter(Boolean).join(" ").toLowerCase().includes(s)))
      .sort(cmp);
  }, [all, q, status, sort]);
  const groups = React.useMemo(() => groupBy(filtered, group), [filtered, group]);
  const ctxErrors = Object.entries(fleet.errors || {}).filter(([, e]) => e && Object.keys(e).length);
  const statusOrder = Object.keys(counts).sort((a, b) => SM(b).rank - SM(a).rank);
  return (
    <>
      <div className="toolbar">
        <div className="search"><Icon n="search" s={13} c="var(--dim2)" />
          <input value={q} placeholder="Search cluster, site, stage, tenant, context…  ( / )" onChange={e => setQ(e.target.value)} />
          {q && <button onClick={() => setQ("")} style={{display: "flex", color: "var(--dim2)"}}><Icon n="x" s={11} /></button>}</div>
        <div className="chips">{statusOrder.map(s => <button key={s} className={"chip" + (status.includes(s) ? " on" : "")}
          onClick={() => setStatus(status.includes(s) ? status.filter(x => x !== s) : status.concat([s]))}><Dot c={SM(s).c} />{SM(s).label}<span className="n">{counts[s]}</span></button>)}</div>
        <div className="spacer"></div>
        <span className="count">{filtered.length}</span>
        <div className="seg grpsw" title="Group clusters by">
          {Object.keys(GROUPINGS).map(k => <button key={k} className={group === k ? "on" : ""} onClick={() => setGroup(k)}>{GROUPINGS[k].label}</button>)}
        </div>
        <select className="sel" value={sort} onChange={e => setSort(e.target.value)}>
          <option value="health">Unhealthy first</option><option value="alerts">Most alerts</option><option value="name">Name (A–Z)</option>
          <option value="util">Utilization ↓</option><option value="capacity">Capacity ↓</option><option value="iops">IOPS ↓</option>
        </select>
        <div className="seg" title="View">
          {[["matrix", "list"], ["tiles", "cluster"], ["sites", "zone"]].map(([k, ic]) => <button key={k} className={view === k ? "on" : ""} onClick={() => setView(k)} title={k}><Icon n={ic} s={12} /></button>)}
        </div>
        <button className="chip" onClick={fleet.reload} title="Refresh"><Icon n="refresh" s={12} /></button>
        {nav.discovery && <button className="btn primary" onClick={() => nav.discovery(LOCAL_CTX)} title="Discover the local cluster's workers and draft a deployment"><Icon n="plus" s={12} />Deploy cluster</button>}
      </div>
      <div className="scroll">
        {ctxErrors.map(([ctx, errs]) => <div className="banner" key={ctx}><Icon n="alert" s={15} />
          <span><b>Context {ctx} is partly unreachable.</b> {Object.entries(errs).map(([k, e]) => `${k}: ${e.message || e}`).join(" · ")}</span></div>)}
        {fleet.error && <ErrorState error={fleet.error} onRetry={fleet.reload} />}
        {fleet.loading && !all.length ? <div className="stats">{Array.from({length: 8}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div>
          : <FleetRollup clusters={filtered} sites={fleet.sites} nav={nav} />}
        {!fleet.loading && !all.length && !fleet.error && <div className="empty"><Icon n="cluster" s={24} /><b>No clusters yet</b><span>No StorageCluster in any configured context.</span>
          {nav.discovery && <button className="btn primary" style={{marginTop: 8}} onClick={() => nav.discovery(LOCAL_CTX)}><Icon n="plus" s={12} />Deploy a cluster</button>}</div>}
        {all.length > 0 && !filtered.length && <div className="empty"><Icon n="search" s={22} /><b>No clusters match</b><button className="chip" style={{marginTop: 6}} onClick={() => { setQ(""); setStatus([]); }}>Reset filters</button></div>}
        {filtered.length > 0 && view === "matrix" && <ClusterMatrix groups={groups} nav={nav} grouping={group} />}
        {filtered.length > 0 && view === "tiles" && groups.map(g => <div key={g.key}>
          {group !== "flat" && <div className="grouplbl">{g.key === "unassigned" ? `no ${GROUPINGS[group].label.toLowerCase()}` : g.key}<Provenance from={g.from} /></div>}
          <div className="grid">{g.rows.map(c => <ClusterTile key={c.id} c={REG[c.id] || c} nav={nav} />)}</div></div>)}
        {filtered.length > 0 && view === "sites" && <SiteBoard sites={fleet.sites.filter(s => s.clusters.some(c => filtered.includes(c)))} nav={nav} />}
      </div>
    </>
  );
}

Object.assign(window, {DashboardView, SitesView, SiteBoard, FleetSiteDetail, ClusterMatrix, FleetRollup, Meter, SevCount, OpsCount, VirtualList, GROUPINGS, groupBy, useFleet, loadFleet, Provenance, SM});
