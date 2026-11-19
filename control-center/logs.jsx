// ---------------------------------------------------------------------------
// LOGS — one consolidated log per cluster, from four sources
//
//   storage plane   the SPDK pods (spdk, spdk-proxy) and the node API pods of
//                   the cluster's nodes, through the pod-log subresource
//   control plane   the control plane's pods in the cluster's namespace
//   event log       the control plane's own cluster event log (v2)
//   kubernetes      Events about the cluster's objects, pods, and workers
//
// Lines are merged by time with the level parsed from each line, filtered by
// source, pod, container, level, text or regex, and time window, and rendered
// in a windowed list. A deep link from an alert or an operation carries the
// window and the object it is about. Follow mode re-reads each stream since
// the newest line it has seen.
// ---------------------------------------------------------------------------
// an ISO timestamp as the log panels print it
const fmtTs = s => String(s || "").replace("T", " ").replace(/(\.\d+)?Z$/, "");
const LOG_SOURCES = {splane: "storage plane", cp: "control plane", eventlog: "event log", k8s: "kubernetes"};
const LOG_RANGES = [["15m", 900], ["1h", 3600], ["6h", 21600], ["24h", 86400]];
const LOG_TAILS = [200, 500, 2000, 10000];
const LOG_CAP = 20000;
const CONTAINER_ORDER = ["spdk-container", "spdk-proxy-container", "s-node-api-container"];
const LEVEL_NORM = {DEBUG: "debug", INFO: "info", NOTICE: "info", WARN: "warn", WARNING: "warn", ERROR: "error", CRITICAL: "error", FATAL: "error"};
const normLevel = l => LEVEL_NORM[String(l || "").toUpperCase()] || (l ? String(l).toLowerCase() : null);

// Kubernetes Events for the cluster's objects, pods, and workers
async function clusterEvents(ctx, cluster, pods, nodes) {
  const ns = cluster.ns;
  const names = new Set([cluster.name].concat(nodes.map(n => n.name), nodes.map(n => n.hostname), pods.map(p => p.name)));
  const workers = new Set(nodes.map(n => n.worker).filter(Boolean));
  const [nsEvents, nodeEvents] = await Promise.all([
    k8s.list("Event", {ctx, namespace: ns}).catch(() => []),
    k8s.list("Event", {ctx, namespace: "default", fieldSelector: "involvedObject.kind=Node"}).catch(() => [])
  ]);
  const mine = e => { const o = e.involvedObject || {}; return names.has(o.name) || (o.kind === "Node" && workers.has(o.name)) || (o.kind === "StorageDevice" || o.kind === "StoragePool" || /Ops$/.test(o.kind || "")) && (e.metadata || {}).namespace === ns; };
  return nsEvents.concat(nodeEvents).filter(mine).map(e => {
    const t = e.lastTimestamp || e.eventTime || (e.metadata || {}).creationTimestamp;
    const o = e.involvedObject || {};
    return {id: "ev:" + (e.metadata || {}).uid, tsMs: Date.parse(t) || 0, ts: t, src: "k8s", pod: null, container: null, node: o.kind === "Node" ? o.name : null, snode: null,
      level: e.type === "Warning" ? "warn" : "info", msg: `${e.reason || ""} ${o.kind}/${o.name}${e.count > 1 ? ` (×${e.count})` : ""}: ${e.message || ""}`,
      raw: JSON.stringify(e), event: e.reason, objectRef: {kind: o.kind, name: o.name, namespace: o.namespace}};
  });
}

// the merged read for one cluster and one filter; `cursor` holds the newest
// time per stream so a follow re-reads only what is new
async function readLogs(cluster, nodes, pods, filter, cursor) {
  const ctx = cluster.ctx;
  const srcs = filter.src && filter.src.length ? filter.src : Object.keys(LOG_SOURCES);
  const wantPod = p => !filter.pod || !filter.pod.length || filter.pod.includes(p.name);
  const wantGroup = p => (p.group === "spdk" || p.group === "node-api") ? srcs.includes("splane") : srcs.includes("cp");
  const wantNode = p => !filter.snode || p.nodeId === filter.snode;
  const chosen = pods.filter(p => wantGroup(p) && wantPod(p) && wantNode(p));
  const since = filter.from ? null : (filter.since || 3600);
  const tail = filter.tail || 2000;
  const jobs = [];
  chosen.forEach(p => (p.containers || []).filter(c => !filter.container || !filter.container.length || filter.container.includes(c)).forEach(c => {
    const key = `${p.name}/${c}`;
    const opts = Object.assign({container: c, tailLines: tail, timestamps: true},
      cursor && cursor[key] ? {sinceTime: new Date(cursor[key] + 1).toISOString()} : filter.from ? {sinceTime: new Date(filter.from).toISOString()} : since ? {sinceSeconds: since} : {});
    jobs.push(sources.podLog(ctx, p.namespace, p.name, opts)
      .then(text => parsePodLog(text, {pod: p.name, container: c, node: p.worker || null, snode: p.nodeId || null, src: (p.group === "spdk" || p.group === "node-api") ? "splane" : "cp"}))
      .then(lines => lines.map(l => Object.assign(l, {id: `${key}|${l.tsMs}|${hashKey(l.msg)}`, level: normLevel(l.level)})))
      .catch(e => [{id: `${key}|err`, tsMs: Date.now(), ts: new Date().toISOString(), src: "cp", pod: p.name, container: c, level: "error", msg: `cannot read ${key}: ${e.message}`, raw: "", unreadable: true}]));
  }));
  if (srcs.includes("eventlog") && hasControlPlane(ctx) && isUuid(cluster.id)) {
    jobs.push(cp.logs(ctx, cluster.id, Math.min(tail, 2000)).then(rs => rs.map(r => ({
      id: "el:" + r.id, tsMs: r.tsMs || 0, ts: r.ts, src: "eventlog", pod: null, container: null, node: null, snode: r.nodeId || null,
      level: normLevel(r.level), msg: `${r.event} ${r.message}`, raw: JSON.stringify(r.raw), event: r.event, objectRef: r.nodeId ? {kind: "node", uuid: r.nodeId} : null
    }))).catch(() => []));
  }
  if (srcs.includes("k8s")) jobs.push(clusterEvents(ctx, cluster, pods, nodes).catch(() => []));
  const lines = (await Promise.all(jobs)).flat();
  return lines;
}

function applyFilter(lines, filter) {
  const q = (filter.q || "").trim();
  let re = null;
  if (q && filter.regex) { try { re = new RegExp(q, "i"); } catch (e) { re = null; } }
  const ql = q.toLowerCase();
  const from = filter.from || (filter.since ? Date.now() - filter.since * 1000 : 0), to = filter.to || Infinity;
  return lines.filter(l => (!filter.level || !filter.level.length || filter.level.includes(l.level || "none"))
    && (l.src === "eventlog" || l.src === "k8s" || !l.tsMs || (l.tsMs >= from && l.tsMs <= to))
    && (!(l.src === "eventlog" || l.src === "k8s") || !l.tsMs || !filter.from || (l.tsMs >= from && l.tsMs <= to))
    && (!filter.snode || l.snode === filter.snode || l.src === "eventlog" && l.snode === filter.snode || (!l.snode && l.src === "k8s"))
    && (!q || (re ? re.test(l.msg) : l.msg.toLowerCase().includes(ql))));
}

function useLogMerge(cluster, nodes, pods, filter, follow) {
  const [state, setState] = React.useState({lines: [], loading: true, error: null, errors: [], at: null});
  const cursor = React.useRef({});
  const all = React.useRef(new Map());
  const key = JSON.stringify([cluster && cluster.id, (pods || []).map(p => p.name), filter.src, filter.pod, filter.container, filter.snode, filter.since, filter.from, filter.to, filter.tail]);
  React.useEffect(() => { cursor.current = {}; all.current = new Map(); setState(s => Object.assign({}, s, {lines: [], loading: true})); }, [key]);
  const load = React.useCallback(async quiet => {
    if (!cluster || !pods) return;
    if (!quiet) setState(s => Object.assign({}, s, {loading: true}));
    try {
      const fresh = await readLogs(cluster, nodes || [], pods, filter, quiet ? cursor.current : null);
      fresh.forEach(l => { all.current.set(l.id, l); if (l.pod && l.tsMs) { const k = `${l.pod}/${l.container}`; cursor.current[k] = Math.max(cursor.current[k] || 0, l.tsMs); } });
      let lines = Array.from(all.current.values()).sort((a, b) => (a.tsMs || 0) - (b.tsMs || 0) || (a.src < b.src ? -1 : 1));
      let capped = false;
      if (lines.length > LOG_CAP) { lines = lines.slice(lines.length - LOG_CAP); capped = true; all.current = new Map(lines.map(l => [l.id, l])); }
      setState({lines, loading: false, error: null, errors: lines.filter(l => l.unreadable), at: Date.now(), capped});
    } catch (e) { setState(s => Object.assign({}, s, {loading: false, error: e})); }
  }, [key]);
  React.useEffect(() => { load(false); }, [key]);
  React.useEffect(() => {
    if (!follow) return;
    const t = setInterval(() => load(true), 3000);
    return () => clearInterval(t);
  }, [key, follow, load]);
  return Object.assign({}, state, {reload: () => load(false)});
}

const LEVEL_COLOR = {error: "var(--bad)", warn: "var(--warn)", info: "var(--dim)", debug: "var(--dim2)"};
const SRC_CLASS = {splane: "splane", cp: "cp", eventlog: "eventlog", k8s: "k8s"};
function LogRow({l, open, onToggle, q, nav, cluster}) {
  const hl = (text) => {
    if (!q) return text;
    const i = text.toLowerCase().indexOf(q.toLowerCase());
    if (i < 0) return text;
    return <>{text.slice(0, i)}<mark className="lhl">{text.slice(i, i + q.length)}</mark>{text.slice(i + q.length)}</>;
  };
  const who = l.pod ? `${l.pod}` : l.src === "eventlog" ? (l.snode ? (REG[l.snode] || {}).hostname || shortId(l.snode) : "cluster") : l.node || (l.objectRef ? `${l.objectRef.kind}/${l.objectRef.name}` : "");
  return (
    <div className={"lgrow" + (open ? " open" : "")} style={{height: open ? "auto" : 22}} onClick={onToggle}>
      <span className="lts mono">{l.ts ? fmtTs(l.ts) : "—"}</span>
      <span className={"lsrc " + SRC_CLASS[l.src]}>{LOG_SOURCES[l.src]}</span>
      <span className="lpod mono" title={who}>{who}</span>
      <span className="lctr mono">{l.container || (l.event ? l.event : "")}</span>
      <span className="llvl" style={{color: LEVEL_COLOR[l.level] || "var(--dim2)"}}>{l.level || ""}</span>
      <span className="lmsgtxt">{hl(l.msg)}</span>
      {open && <div className="lgx">
        <pre className="code">{l.raw || l.msg}</pre>
        <div className="labels">
          {l.snode && nav && cluster && <button className="lab link" onClick={e => { e.stopPropagation(); nav.openNode(cluster.id, l.snode); }}><Icon n="node" s={10} />open node</button>}
          {l.pod && <span className="lab"><i>pod</i>{l.pod}</span>}
          {l.node && <span className="lab"><i>worker</i>{l.node}</span>}
          <button className="lab link" onClick={e => { e.stopPropagation(); navigator.clipboard && navigator.clipboard.writeText(l.raw || l.msg); window.__toast("Line copied"); }}><Icon n="copy" s={10} />copy</button>
        </div>
      </div>}
    </div>
  );
}

function downloadText(name, text) {
  const blob = new Blob([text], {type: "text/plain"});
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob); a.download = name; document.body.appendChild(a); a.click();
  setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 500);
}
const fmtLine = l => `${l.ts || ""} ${l.src} ${l.pod || l.node || ""}${l.container ? "/" + l.container : ""} ${l.level || ""} ${l.msg}`;

// LogsView: section-level (route logs under a cluster or a node) or embedded
function LogsView({seg, nav, clusterSeg, nodeSeg, cluster: clusterProp, node: nodeProp, embedded}) {
  const cid = clusterProp ? clusterProp.id : clusterSeg ? clusterSeg.id : null;
  const nid = nodeProp ? nodeProp.id : nodeSeg ? nodeSeg.id : null;
  const p = (seg && seg.p) || {};
  const {data: cluster} = useResource("logc|" + cid, () => cid ? api.cluster(cid) : Promise.resolve(null));
  const {data: nodes} = useResource("logn|" + cid, () => cid ? api.nodes(cid) : Promise.resolve([]), 30000);
  // keyed on the cluster's arrival too, or the loader would run once with no cluster and never again
  const {data: pods, error: podErr, reload: reloadPods} = useResource("logp|" + cid + "|" + (cluster ? cluster.id : "-"), () => cluster ? sources.clusterPods(cluster.ctx, cluster) : Promise.resolve(null), 30000);
  const [filter, setFilter] = React.useState(() => Object.assign({src: [], pod: [], container: [], level: [], q: "", regex: false, since: 3600, tail: 2000, snode: nid || null}, p));
  const [follow, setFollow] = React.useState(!p.from);
  const [open, setOpen] = React.useState(null);
  React.useEffect(() => { if (nid) setFilter(f => Object.assign({}, f, {snode: nid})); }, [nid]);
  // without a chosen pod, the view reads the node you came from, else the first five pods
  const podList = React.useMemo(() => {
    const ps = pods || [];
    if (filter.pod.length) return ps;
    if (filter.snode) return ps.filter(x => x.nodeId === filter.snode || x.group === "control-plane" || x.group === "fdb").slice(0, 12);
    return ps.slice(0, 5).concat(ps.filter(x => x.group === "fdb" || x.group === "control-plane").slice(0, 3));
  }, [pods, filter.pod, filter.snode]);
  const effective = React.useMemo(() => Object.assign({}, filter, {pod: filter.pod.length ? filter.pod : podList.map(x => x.name)}), [filter, podList]);
  const m = useLogMerge(cluster, nodes, pods, effective, follow);
  const lines = React.useMemo(() => applyFilter(m.lines, filter), [m.lines, filter.level, filter.q, filter.regex, filter.from, filter.to, filter.since, filter.snode]);
  const counts = React.useMemo(() => { const c = {src: {}, level: {}}; m.lines.forEach(l => { c.src[l.src] = (c.src[l.src] || 0) + 1; c.level[l.level || "none"] = (c.level[l.level || "none"] || 0) + 1; }); return c; }, [m.lines]);
  const toggle = (k, v) => setFilter(f => Object.assign({}, f, {[k]: f[k].includes(v) ? f[k].filter(x => x !== v) : f[k].concat([v])}));
  const containers = Array.from(new Set((pods || []).flatMap(x => x.containers))).sort((a, b) => (CONTAINER_ORDER.indexOf(a) + 100) % 100 - (CONTAINER_ORDER.indexOf(b) + 100) % 100 || a.localeCompare(b));
  const listRef = React.useRef(null);
  React.useEffect(() => { const el = listRef.current; if (follow && el && !open) el.scrollTop = el.scrollHeight; }, [lines.length, follow]);
  if (!cid) return <div className="scroll"><div className="empty"><Icon n="list" s={24} /><b>Pick a cluster</b><span>Logs are read per cluster.</span></div></div>;
  const body = (
    <>
      {podErr && <ErrorState error={podErr} onRetry={reloadPods} />}
      {filter.from && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}><Icon n="clock" s={15} />
        <span>Showing {fmtTs(new Date(filter.from).toISOString())} to {fmtTs(new Date(filter.to || Date.now()).toISOString())}{filter.snode && REG[filter.snode] ? ` around ${REG[filter.snode].hostname}` : ""}. </span>
        <button className="chip" onClick={() => { setFilter(f => Object.assign({}, f, {from: null, to: null, since: 3600})); setFollow(true); }}>Clear window</button></div>}
      {m.capped && <div className="banner"><Icon n="alert" s={15} /><span>More than {LOG_CAP} lines; the oldest were dropped. Narrow the window or the pods.</span></div>}
      <div className="card">
        <div className="ptools" style={{flexWrap: "wrap"}}>
          <div className="chips">{Object.keys(LOG_SOURCES).map(s => <button key={s} className={"chip" + (filter.src.includes(s) ? " on" : "")} onClick={() => toggle("src", s)} title={s === "eventlog" && cluster && !hasControlPlane(cluster.ctx) ? "needs a reachable control plane" : ""}>{LOG_SOURCES[s]}<span className="n">{counts.src[s] || 0}</span></button>)}</div>
          <select className="sel" multiple={false} value={filter.pod[0] || ""} onChange={e => setFilter(f => Object.assign({}, f, {pod: e.target.value ? [e.target.value] : []}))} style={{maxWidth: 260}} title="pod">
            <option value="">{filter.snode && REG[filter.snode] ? `pods of ${REG[filter.snode].hostname}` : `first pods (${podList.length})`}</option>
            {["spdk", "node-api", "control-plane", "fdb"].map(g => (pods || []).filter(x => x.group === g).length ? <optgroup key={g} label={g}>{(pods || []).filter(x => x.group === g).map(x => <option key={x.name} value={x.name}>{x.name}{x.worker ? ` · ${x.worker}` : ""}</option>)}</optgroup> : null)}
          </select>
          <select className="sel" value={filter.container[0] || ""} onChange={e => setFilter(f => Object.assign({}, f, {container: e.target.value ? [e.target.value] : []}))} title="container">
            <option value="">all containers</option>{containers.map(c => <option key={c} value={c}>{c}</option>)}
          </select>
          {nodes && nodes.length > 0 && <select className="sel" value={filter.snode || ""} onChange={e => setFilter(f => Object.assign({}, f, {snode: e.target.value || null, pod: []}))} title="storage node">
            <option value="">all nodes</option>{nodes.map(n => <option key={n.id} value={n.id}>{n.hostname}</option>)}
          </select>}
          <div className="chips">{["error", "warn", "info", "debug"].map(l => <button key={l} className={"chip" + (filter.level.includes(l) ? " on" : "")} onClick={() => toggle("level", l)}><Dot c={LEVEL_COLOR[l]} />{l}<span className="n">{counts.level[l] || 0}</span></button>)}</div>
          <div className="search" style={{minWidth: 200}}><Icon n="search" s={13} c="var(--dim2)" />
            <input value={filter.q} placeholder={filter.regex ? "regular expression…" : "filter lines…"} onChange={e => setFilter(f => Object.assign({}, f, {q: e.target.value}))} />
            <button className={"chip" + (filter.regex ? " on" : "")} style={{padding: "0 5px"}} title="regular expression" onClick={() => setFilter(f => Object.assign({}, f, {regex: !f.regex}))}>.*</button></div>
          <div className="seg">{LOG_RANGES.map(([l, s]) => <button key={l} className={!filter.from && filter.since === s ? "on" : ""} onClick={() => setFilter(f => Object.assign({}, f, {since: s, from: null, to: null}))}>{l}</button>)}</div>
          <select className="sel" value={filter.tail} onChange={e => setFilter(f => Object.assign({}, f, {tail: Number(e.target.value)}))} title="lines per stream">{LOG_TAILS.map(t => <option key={t} value={t}>{t} lines</option>)}</select>
          <div className="spacer"></div>
          <span className="count">{lines.length}</span>
          <button className={"chip" + (follow ? " on" : "")} onClick={() => setFollow(!follow)}><span className="live"><i></i>follow</span></button>
          <button className="chip" onClick={m.reload} title="Re-read"><Icon n="refresh" s={12} /></button>
          <CopyBtn get={() => lines.map(fmtLine).join("\n")} label="Copy" />
          <button className="chip" onClick={() => downloadText(`${cluster ? cluster.name : "cluster"}-logs-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-")}.txt`, lines.map(fmtLine).join("\n"))}><Icon n="cloud" s={11} />Download</button>
        </div>
        <div className="lglist" ref={listRef} style={{maxHeight: embedded ? 520 : "calc(100vh - 330px)"}}>
          {m.loading && !lines.length ? <div className="lmsg">reading {podList.length} pod{podList.length === 1 ? "" : "s"}…</div>
            : m.error ? <ErrorState error={m.error} onRetry={m.reload} />
            : !lines.length ? <div className="lmsg">{m.lines.length ? "No line matches the filter." : pods && !pods.length ? "No pod of this cluster is running in its namespace." : "No lines in this window."}</div>
            : lines.length > 400 && !open ? <VirtualList rows={lines} rowHeight={22} height={embedded ? 520 : Math.max(300, window.innerHeight - 340)} render={(l, i) => <LogRow key={l.id} l={l} q={filter.q} nav={nav} cluster={cluster} open={false} onToggle={() => setOpen(l.id)} />} />
            : lines.map(l => <LogRow key={l.id} l={l} q={filter.regex ? "" : filter.q} nav={nav} cluster={cluster} open={open === l.id} onToggle={() => setOpen(open === l.id ? null : l.id)} />)}
        </div>
        {m.errors && m.errors.length > 0 && <p className="mdesc" style={{margin: "8px 12px"}}>{m.errors.length} stream{m.errors.length === 1 ? "" : "s"} could not be read; the error is shown as a line.</p>}
      </div>
    </>
  );
  if (embedded) return body;
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}><h1>Logs{cluster ? ` · ${cluster.name}` : ""}{filter.snode && REG[filter.snode] ? ` · ${REG[filter.snode].hostname}` : ""}</h1>
        <div className="mdesc" style={{marginTop: 4}}>Storage plane and control plane pod logs, the control plane's event log, and Kubernetes events, merged by time.</div></div></div>
      {body}
    </div>
  );
}

Object.assign(window, {LogsView, readLogs, applyFilter, useLogMerge, LOG_SOURCES, downloadText, clusterEvents});
