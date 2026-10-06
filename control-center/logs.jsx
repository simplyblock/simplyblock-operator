// ---------------------------------------------------------------------------
// LOG EXPLORER
//
// The shipped logs of every pod fluent-bit collects (the hub's control plane
// and, where the sites ship to the hub, the sites' simplyblock namespaces):
// Graylog's search API behind the console's proxy (/graylog/api/, GET only,
// credentials injected server-side, responses scrubbed by redact.js).
//
// Oldest line first. A query is a keyword filter (simple mode: words AND-ed,
// "quoted phrases", -word or NOT word; or raw Graylog/Lucene syntax), a time
// range (relative preset or absolute from/to) and source filters (site,
// namespace, pod, container, level). The whole query lives in the URL
// (#logs?...), so a link reproduces it.
//
// Without Graylog the view falls back to the live tail of a pod straight from
// the Kubernetes API; without either it says why logs are not available.
// ---------------------------------------------------------------------------
const LGC = window.SB_CONFIG;
const graylogOn = () => !!LGC.mock || (!!LGC.graylogBase && !!(LGC.upstreams && LGC.upstreams.graylog));
const graylogOffReason = () => LGC.graylogOff || "no log store (Graylog) is configured for this console";

const LOG_PAGE = 200;
// OpenSearch serves offset + limit up to its max_result_window (10,000 by
// default); a page beyond it is refused, so the explorer asks for a narrower
// range instead.
const LOG_WINDOW_MAX = 10000;
const LOG_FIELDS = ["timestamp", "source", "message", "kubernetes_namespace_name", "kubernetes_pod_name",
  "kubernetes_container_name", "kubernetes_host"];
const LOG_RANGES = [["15m", 900, "15 min"], ["1h", 3600, "1 hour"], ["6h", 21600, "6 hours"], ["24h", 86400, "24 hours"], ["7d", 604800, "7 days"]];
const rangeSecs = k => (LOG_RANGES.find(r => r[0] === k) || LOG_RANGES[1])[1];
const LEVEL_TERMS = {ERROR: ["ERROR", "FATAL", "CRITICAL"], WARN: ["WARN", "WARNING"], INFO: ["INFO"], DEBUG: ["DEBUG"]};

// ---- query building --------------------------------------------------------
// Lucene's reserved characters in a bare word; a quoted phrase only needs its
// quotes and backslashes escaped.
const luceneWord = w => w.replace(/([+\-&|!(){}[\]^"~*?:\\/])/g, "\\$1");
const lucenePhrase = p => '"' + p.replace(/(["\\])/g, "\\$1") + '"';
// simple mode: words AND-ed, "quoted phrases", -word / NOT word excluded
function simpleQuery(text) {
  const out = [];
  const re = /(?:(-|NOT\s+))?(?:"([^"]*)"|(\S+))/g;
  let m;
  while ((m = re.exec(String(text || "")))) {
    const neg = !!m[1];
    if (m[3] !== undefined && /^(AND|OR|NOT)$/.test(m[3])) continue;
    const term = m[2] !== undefined ? (m[2].trim() ? lucenePhrase(m[2]) : "") : luceneWord(m[3]);
    if (!term) continue;
    out.push(neg ? `NOT ${term}` : term);
  }
  return out.join(" AND ");
}
const fieldIs = (f, v) => `${f}:${lucenePhrase(v)}`;
const anyOf = (f, vs) => vs.length === 1 ? fieldIs(f, vs[0]) : `${f}:(${vs.map(lucenePhrase).join(" OR ")})`;
// The effective Graylog query of a filter state, and its parts for display.
function buildLogQuery(f, siteNodes) {
  const parts = [];
  const kw = f.adv ? String(f.q || "").trim() : simpleQuery(f.q);
  if (kw) parts.push(f.adv ? `(${kw})` : kw);
  if (f.ns) parts.push(fieldIs("kubernetes_namespace_name", f.ns));
  if (f.pod) parts.push(fieldIs("kubernetes_pod_name", f.pod));
  if (f.container) parts.push(fieldIs("kubernetes_container_name", f.container));
  if (f.source) parts.push(fieldIs("source", f.source));
  else if (f.site) {
    const ns = (siteNodes || {})[f.site] || [];
    // a site whose nodes are not known yet matches nothing rather than everything
    parts.push(ns.length ? anyOf("source", ns) : fieldIs("source", `__no_known_node_of_${f.site}`));
  }
  if (f.level) parts.push(`message:(${(LEVEL_TERMS[f.level] || [f.level]).join(" OR ")})`);
  return parts.length ? parts.join(" AND ") : "*";
}

// The time window a filter state means right now: relative presets end now.
function logWindow(f, now) {
  const t = now || Date.now();
  if (f.from || f.to) {
    const from = f.from ? Date.parse(f.from) : t - rangeSecs("1h") * 1000;
    const to = f.to ? Date.parse(f.to) : t;
    return {from: new Date(Math.min(from, to)), to: new Date(Math.max(from, to)), live: !f.to};
  }
  return {from: new Date(t - rangeSecs(f.range) * 1000), to: new Date(t), live: true};
}

// ---- permalink -------------------------------------------------------------
const LOG_KEYS = ["q", "adv", "range", "from", "to", "site", "ns", "pod", "container", "source", "level", "follow"];
const DEFAULT_LOG_FILTER = {q: "", adv: false, range: "1h", from: "", to: "", site: "", ns: "", pod: "", container: "", source: "", level: "", follow: false};
function filterFromHash(hash) {
  const h = String(hash || "");
  const i = h.indexOf("?");
  if (!/^#logs\b/.test(h)) return null;
  const p = new URLSearchParams(i >= 0 ? h.slice(i + 1) : "");
  const f = Object.assign({}, DEFAULT_LOG_FILTER);
  LOG_KEYS.forEach(k => { if (p.has(k)) f[k] = p.get(k); });
  f.adv = f.adv === true || f.adv === "1";
  f.follow = f.follow === true || f.follow === "1";
  if (!LOG_RANGES.some(r => r[0] === f.range)) f.range = "1h";
  return f;
}
function hashOfFilter(f) {
  const p = new URLSearchParams();
  LOG_KEYS.forEach(k => {
    const v = f[k];
    if (v === true) p.set(k, "1");
    else if (v && v !== DEFAULT_LOG_FILTER[k]) p.set(k, v);
  });
  const s = p.toString();
  return "#logs" + (s ? "?" + s : "");
}
// A link to #logs opens the explorer: the shell restores its location from
// localStorage, so point it at the logs layer before the shell mounts.
if (filterFromHash(window.location.hash)) {
  try { localStorage.setItem(LGC.mode === "dr" ? "sb.drpath" : "sb.path", JSON.stringify([{t: "logs"}])); } catch (e) {}
}

// ---- transport -------------------------------------------------------------
const glMsg = m => {
  const x = m.message || m;
  const msg = String(x.message || x.full_message || "");
  return {id: x._id || `${x.timestamp}|${x.source}|${msg.slice(0, 40)}`, ts: x.timestamp || "", source: x.source || x.kubernetes_host || "",
    ns: x.kubernetes_namespace_name || "", pod: x.kubernetes_pod_name || "", container: x.kubernetes_container_name || "",
    level: levelOf(msg), msg};
};
async function glGet(path) {
  if (!graylogOn()) throw notInDeployment("The log store (Graylog)", "is not part of this deployment: " + graylogOffReason(), path);
  let res;
  try { res = await fetch((LGC.graylogBase || "/graylog/api") + path, {headers: {Accept: "application/json"}}); }
  catch (e) { throw new ApiError(0, "Cannot reach the log store (Graylog)", path, "Unreachable"); }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  const failed = body && body.kind === "Status" && body.status === "Failure";
  if (!res.ok || failed) {
    const msg = (body && (body.message || body.type)) || res.statusText || "Request failed";
    const e = new ApiError((body && body.code) || res.status, msg, path, (body && body.reason) || null);
    e.source = "log store (Graylog)";
    throw e;
  }
  return body || {};
}
const logs = {
  // GET /search/universal/absolute — oldest first, one page
  search: async ({query, from, to, offset, limit}) => {
    const p = new URLSearchParams({query: query || "*", from: from.toISOString(), to: to.toISOString(),
      offset: String(offset || 0), limit: String(limit || LOG_PAGE), sort: "timestamp:asc", fields: LOG_FIELDS.join(","), decorate: "false"});
    const r = await glGet("/search/universal/absolute?" + p.toString());
    return {total: Number(r.total_results) || 0, lines: (r.messages || []).map(glMsg), builtQuery: r.built_query || null};
  }
};

// Values to filter by: the hub's own pods and nodes, the sites' nodes from
// their dr-agent inventory, and whatever the loaded lines carry.
async function logSourcesKnown() {
  const [pods, hubNodes, sites] = await Promise.all([
    k8s.list("Pod").catch(() => []),
    k8s.list("Node").catch(() => []),
    (typeof hubSites === "function" ? hubSites() : Promise.resolve([])).catch(() => [])]);
  const siteNodes = {hub: hubNodes.map(n => n.metadata.name)};
  sites.forEach(s => { siteNodes[s.name] = (s.nodes || []).map(n => n.name); });
  return {siteNodes, pods: pods.map(p => ({name: p.metadata.name, ns: p.metadata.namespace || NS(),
    containers: ((p.spec || {}).containers || []).map(c => c.name)}))};
}

// ---- view ------------------------------------------------------------------
const toLocalInput = d => { if (!d || isNaN(d)) return ""; const z = new Date(d.getTime() - d.getTimezoneOffset() * 60000); return z.toISOString().slice(0, 16); };
const fromLocalInput = v => { if (!v) return ""; const d = new Date(v); return isNaN(d) ? "" : d.toISOString(); };
const fmtLineTs = s => (s || "").replace("T", " ").replace(/Z$/, "");

function LogsView({nav}) {
  const [f, setF] = useState(() => filterFromHash(window.location.hash) || Object.assign({}, DEFAULT_LOG_FILTER));
  const set = patch => setF(p => Object.assign({}, p, patch));
  useEffect(() => {
    const h = e => e.detail && setF(Object.assign({}, DEFAULT_LOG_FILTER, e.detail));
    window.addEventListener("sb-logs-query", h);
    return () => window.removeEventListener("sb-logs-query", h);
  }, []);
  // keep the URL in step: the address bar is the permalink of what is shown
  useEffect(() => {
    try { window.history.replaceState(null, "", window.location.pathname + window.location.search + hashOfFilter(f)); } catch (e) {}
  }, [f]);
  useEffect(() => () => {
    try { if (/^#logs\b/.test(window.location.hash)) window.history.replaceState(null, "", window.location.pathname + window.location.search); } catch (e) {}
  }, []);
  const store = graylogOn();
  return (
    <div className="scroll">
      <div className="sech"><h2>Logs</h2><span className="ln"></span>
        <SourceTag what={store ? "log store (Graylog), shipped by fluent-bit" : "live tail from the Kubernetes API"} /></div>
      {store ? <LogExplorer f={f} set={set} setF={setF} /> : <LiveTail f={f} set={set} />}
    </div>
  );
}

function LogExplorer({f, set, setF}) {
  const [qDraft, setQDraft] = useState(f.q);
  useEffect(() => setQDraft(f.q), [f.q]);
  const {data: known} = useResource("logsrc", () => logSourcesKnown());
  const siteNodes = (known || {}).siteNodes || {};
  const query = buildLogQuery(f, siteNodes);
  const [run, setRun] = useState(0);
  const [st, setSt] = useState({loading: true, error: null, lines: [], total: 0, win: null, offset: 0, capped: false});
  const listRef = useRef(null);
  const atEnd = useRef(true);
  const key = JSON.stringify([query, f.range, f.from, f.to, f.follow, run]);

  // a new query: the first page, or with follow the newest page
  useEffect(() => {
    let dead = false;
    const win = logWindow(f);
    setSt(p => Object.assign({}, p, {loading: true, error: null}));
    (async () => {
      try {
        let r = await logs.search({query, from: win.from, to: win.to, offset: 0, limit: LOG_PAGE});
        let offset = 0;
        if (f.follow && r.total > LOG_PAGE) {
          offset = Math.max(0, Math.min(r.total, LOG_WINDOW_MAX) - LOG_PAGE);
          r = Object.assign(await logs.search({query, from: win.from, to: win.to, offset, limit: LOG_PAGE}), {total: r.total});
        }
        if (!dead) setSt({loading: false, error: null, lines: r.lines, total: r.total, win, offset, capped: r.total > LOG_WINDOW_MAX});
      } catch (e) { if (!dead) setSt({loading: false, error: e, lines: [], total: 0, win, offset: 0, capped: false}); }
    })();
    return () => { dead = true; };
  }, [key]);

  // follow: append what arrived since the last line, every few seconds
  useEffect(() => {
    if (!f.follow || !st.win || !st.win.live || st.error) return;
    const i = setInterval(async () => {
      const last = st.lines.length ? st.lines[st.lines.length - 1].ts : st.win.from.toISOString();
      try {
        const r = await logs.search({query, from: new Date(last), to: new Date(), offset: 0, limit: 500});
        setSt(p => {
          const seen = new Set(p.lines.map(l => l.id));
          const add = r.lines.filter(l => !seen.has(l.id));
          return add.length ? Object.assign({}, p, {lines: p.lines.concat(add), total: p.total + add.length}) : p;
        });
      } catch (e) {}
    }, 5000);
    return () => clearInterval(i);
  }, [f.follow, st.win, st.error, st.lines.length && st.lines[st.lines.length - 1].id, query]);

  useEffect(() => {
    const el = listRef.current;
    if (el && f.follow && atEnd.current) el.scrollTop = el.scrollHeight;
  }, [st.lines.length, f.follow]);

  const loaded = st.offset + st.lines.length;
  const more = !f.follow && loaded < Math.min(st.total, LOG_WINDOW_MAX);
  const loadMore = async () => {
    if (!more || st.loading) return;
    setSt(p => Object.assign({}, p, {loading: true}));
    try {
      const r = await logs.search({query, from: st.win.from, to: st.win.to, offset: loaded, limit: LOG_PAGE});
      setSt(p => Object.assign({}, p, {loading: false, lines: p.lines.concat(r.lines)}));
    } catch (e) { setSt(p => Object.assign({}, p, {loading: false, error: e})); }
  };
  const onScroll = e => {
    const el = e.target;
    atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    if (atEnd.current && more) loadMore();
  };

  // filter values: known sources plus what the loaded lines carry
  const uniq = xs => [...new Set(xs.filter(Boolean))].sort();
  const pods = (known || {}).pods || [];
  const nsOpts = uniq(pods.map(p => p.ns).concat(st.lines.map(l => l.ns), [f.ns]));
  const podOpts = uniq(pods.filter(p => !f.ns || p.ns === f.ns).map(p => p.name).concat(st.lines.filter(l => !f.ns || l.ns === f.ns).map(l => l.pod), [f.pod]));
  const ctrOpts = uniq(pods.filter(p => !f.pod || p.name === f.pod).flatMap(p => p.containers).concat(st.lines.filter(l => !f.pod || l.pod === f.pod).map(l => l.container), [f.container]));
  const siteOpts = uniq(Object.keys(siteNodes).concat([f.site]));
  const srcOpts = uniq((f.site ? siteNodes[f.site] || [] : Object.values(siteNodes).flat()).concat(st.lines.map(l => l.source), [f.source]));

  const text = () => st.lines.map(l => `${l.ts} ${l.source} ${l.ns}/${l.pod}/${l.container} ${l.level} ${l.msg}`).join("\n");
  const download = () => {
    try {
      const a = document.createElement("a");
      a.href = URL.createObjectURL(new Blob([text() + "\n"], {type: "text/plain"}));
      a.download = `logs-${st.win ? st.win.from.toISOString() : "now"}.log`.replace(/:/g, "-");
      document.body.appendChild(a); a.click(); a.remove();
    } catch (e) {}
  };
  const win = st.win || logWindow(f);
  const sel = (label, k, opts, all) => (
    <select className={"sel lx-" + k} value={f[k] || ""} title={label} onChange={e => set({[k]: e.target.value, ...(k === "ns" ? {pod: "", container: ""} : k === "pod" ? {container: ""} : k === "site" ? {source: ""} : {})})}>
      <option value="">{all}</option>{opts.map(o => <option key={o} value={o}>{o}</option>)}
    </select>
  );
  return (
    <div className="card lx">
      <div className="ptools" style={{flexWrap: "wrap", gap: 6}}>
        <div className="search" style={{minWidth: 280, flex: 1}}><Icon n="search" s={13} c="var(--dim2)" />
          <input className="lx-q" value={qDraft} placeholder={f.adv ? 'Graylog query, e.g. message:"lvol" AND NOT level:debug' : 'Keywords, "a phrase", -exclude'}
            onChange={e => setQDraft(e.target.value)} onKeyDown={e => { if (e.key === "Enter") set({q: e.target.value}); }} onBlur={e => e.target.value !== f.q && set({q: e.target.value})} /></div>
        <label className="chip lx-adv" title="Write the query in Graylog (Lucene) syntax instead"><input type="checkbox" checked={f.adv} onChange={e => set({adv: e.target.checked})} /> Query syntax</label>
        {LOG_RANGES.map(([k, , l]) => <button key={k} className={"chip lx-range" + (!f.from && !f.to && f.range === k ? " on" : "")} title={`last ${l}`}
          style={!f.from && !f.to && f.range === k ? {borderColor: "var(--info)", color: "var(--info)"} : null} onClick={() => set({range: k, from: "", to: ""})}>{k}</button>)}
        <span style={{fontSize: 11, color: "var(--dim)"}}>from</span>
        <input type="datetime-local" className="sel lx-from" value={toLocalInput(f.from ? new Date(f.from) : null)} onChange={e => set({from: fromLocalInput(e.target.value)})} />
        <span style={{fontSize: 11, color: "var(--dim)"}}>to</span>
        <input type="datetime-local" className="sel lx-to" value={toLocalInput(f.to ? new Date(f.to) : null)} onChange={e => set({to: fromLocalInput(e.target.value), follow: false})} title="empty: now" />
      </div>
      <div className="ptools" style={{flexWrap: "wrap", gap: 6, marginTop: 6}}>
        {sel("Site or cluster", "site", siteOpts, "All sites")}
        {sel("Node", "source", srcOpts, "All nodes")}
        {sel("Namespace", "ns", nsOpts, "All namespaces")}
        {sel("Pod", "pod", podOpts, "All pods")}
        {sel("Container", "container", ctrOpts, "All containers")}
        <select className="sel lx-level" value={f.level} onChange={e => set({level: e.target.value})}>
          <option value="">All levels</option>{["ERROR", "WARN", "INFO", "DEBUG"].map(l => <option key={l} value={l}>{l}</option>)}
        </select>
        <label className="chip lx-follow" title="Append new lines as they arrive (relative ranges and open-ended absolute ranges)">
          <input type="checkbox" checked={f.follow} disabled={!!f.to} onChange={e => set({follow: e.target.checked})} /> Follow</label>
        <button className="chip lx-clear" onClick={() => setF(Object.assign({}, DEFAULT_LOG_FILTER))}>Clear</button>
        <div className="spacer"></div>
        <span className="count lx-count">{st.lines.length}{st.total > st.lines.length ? ` of ${st.total}` : ""}</span>
        <CopyBtn get={() => window.location.href} label="Copy link" />
        <CopyBtn get={text} label="Copy lines" />
        <button className="chip lx-download" onClick={download}><Icon n="cloud" s={12} />Download</button>
        <button className="chip" onClick={() => setRun(r => r + 1)}><Icon n="refresh" s={12} />Run</button>
      </div>
      <div className="lx-effective" style={{fontSize: 11, color: "var(--dim)", margin: "6px 0", overflowWrap: "anywhere"}}>
        <span className="mono lx-query">{query}</span> · <span className="lx-window">{fmtLineTs(win.from.toISOString())} → {f.to ? fmtLineTs(win.to.toISOString()) : "now"}</span> · oldest first · Graylog
      </div>
      {st.capped && <div className="banner lx-capped"><Icon n="alert" s={14} /><span>{st.total} lines match. The log store serves the first {LOG_WINDOW_MAX.toLocaleString()} of a query: narrow the time range or the filter to see the rest.</span></div>}
      {st.error ? <div className="bd"><ErrorState error={st.error} onRetry={() => setRun(r => r + 1)} /></div> :
      <div className="logstream lx-lines" ref={listRef} onScroll={onScroll} style={{maxHeight: 560}}>
        {st.loading && !st.lines.length ? <div className="lmsg">loading…</div>
          : !st.lines.length ? <div className="lmsg lx-empty">No log lines match this query in this time range.</div>
          : st.lines.map(l => (
            <div className="lrow lx-row" key={l.id}>
              <span className="lts">{fmtLineTs(l.ts)}</span>
              <span className="lts" style={{minWidth: 0, color: "var(--dim2)"}} title={`${l.source} · ${l.ns}/${l.pod}/${l.container}`}>{l.pod || l.source}{l.container ? "/" + l.container : ""}</span>
              <span className="llvl" style={{color: LEVEL_C[l.level] || "var(--dim)"}}>{l.level}</span>
              <span className="lmsgtxt">{l.msg}</span>
            </div>))}
        {more && <div className="lmsg"><button className="chip lx-more" onClick={loadMore}>{st.loading ? "loading…" : `Load ${Math.min(LOG_PAGE, Math.min(st.total, LOG_WINDOW_MAX) - loaded)} more`}</button></div>}
        {f.follow && <div className="lmsg lx-following"><span className="live"><i></i>following</span></div>}
      </div>}
    </div>
  );
}

// Without a log store: the live tail of one pod from the Kubernetes API, the
// keyword and time filters applied to those lines.
function LiveTail({f, set}) {
  const {data: pods, error: pErr} = useResource("ltpods", () => k8s.list("Pod"));
  const list = (pods || []).map(p => ({name: p.metadata.name, containers: ((p.spec || {}).containers || []).map(c => c.name)}));
  const pod = f.pod && list.some(p => p.name === f.pod) ? f.pod : (list[0] || {}).name || "";
  const cs = (list.find(p => p.name === pod) || {}).containers || [];
  const container = cs.length > 1 ? (cs.includes(f.container) ? f.container : cs[0]) : "";
  const [tail, setTail] = useState(POD_LOG_TAIL);
  const {data, loading, error, reload} = useResource("lt|" + pod + "|" + container + "|" + tail, () => pod ? podLogs(pod, container, tail) : Promise.resolve([]), f.follow ? 4000 : 0);
  const win = logWindow(f);
  const words = [];
  const re = /(?:(-|NOT\s+))?(?:"([^"]*)"|(\S+))/g; let m;
  while ((m = re.exec(f.q || ""))) { const t = (m[2] !== undefined ? m[2] : m[3]).toLowerCase(); if (t && !/^(and|or|not)$/.test(t)) words.push({neg: !!m[1], t}); }
  const inWin = l => { const t = Date.parse(l.ts); return isNaN(t) || (t >= win.from.getTime() && t <= win.to.getTime()); };
  const lines = (data || []).filter(l => inWin(l) && (!f.level || l.level === f.level)
    && words.every(w => l.msg.toLowerCase().includes(w.t) !== w.neg));
  if (pErr) return <div className="card"><div className="bd"><ErrorState error={notInDeployment("Logs", `are not available in this deployment: ${graylogOffReason()}, and the console may not read pods here (${pErr.message})`)} /></div></div>;
  return (
    <>
      <div className="banner lx-livetail" style={{marginBottom: 10}}><Icon n="alert" s={14} /><span><b>Live tail, last {tail} lines.</b> No log store is configured ({graylogOffReason()}), so this reads the pod's log from the Kubernetes API: no history beyond those lines.</span></div>
      <div className="card">
        <LogStream lines={lines} loading={loading} error={error} onRetry={reload} height={560} empty={pod ? "No lines match the current filter." : "No pod to read."} tools={
          <div className="ptools" style={{flexWrap: "wrap", gap: 6}}>
            <select className="sel lt-pod" value={pod} onChange={e => set({pod: e.target.value, container: ""})}>{list.map(p => <option key={p.name} value={p.name}>{p.name}</option>)}</select>
            {cs.length > 1 && <select className="sel lt-container" value={container} onChange={e => set({container: e.target.value})}>{cs.map(c => <option key={c} value={c}>{c}</option>)}</select>}
            <select className="sel lt-tail" value={tail} onChange={e => setTail(Number(e.target.value))}>{[200, 500, 2000, 5000].map(n => <option key={n} value={n}>last {n} lines</option>)}</select>
            <select className="sel" value={f.level} onChange={e => set({level: e.target.value})}>
              <option value="">All levels</option>{["ERROR", "WARN", "INFO", "DEBUG"].map(l => <option key={l} value={l}>{l}</option>)}</select>
            <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" />
              <input className="lt-q" value={f.q} placeholder='Keywords, "a phrase", -exclude' onChange={e => set({q: e.target.value})} /></div>
            {LOG_RANGES.map(([k]) => <button key={k} className={"chip" + (!f.from && !f.to && f.range === k ? " on" : "")} onClick={() => set({range: k, from: "", to: ""})}>{k}</button>)}
            <label className="chip"><input type="checkbox" checked={f.follow} onChange={e => set({follow: e.target.checked})} /> Follow</label>
            <div className="spacer"></div>
            <span className="count lt-count">{lines.length}</span>
            <CopyBtn get={() => lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n")} />
          </div>} />
      </div>
    </>
  );
}

// A chip that opens the explorer on a query; entry points from the control
// plane, nodes, clusters and DR runs.
const LogsLink = ({params, label}) => window.__nav && window.__nav.logs
  ? <button className="chip lx-link" onClick={() => window.__nav.logs(params)} title="Shipped logs: any time range, keyword search, oldest first"><Icon n="search" s={11} />{label || "Search logs"}</button>
  : null;

Object.assign(window, {LogsView, LogsLink, logs, graylogOn, buildLogQuery, simpleQuery, filterFromHash, hashOfFilter, logWindow, DEFAULT_LOG_FILTER});
