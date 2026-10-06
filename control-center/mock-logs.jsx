// Fixture backend for the log explorer: Graylog 5.0's universal search
// (GET /api/search/universal/absolute and /relative), over a generated set of
// log lines from the control plane, the DR stack and two sites' storage nodes.
// Understands the subset of Lucene the explorer writes: clauses joined by
// AND, NOT, "phrases", bare words, field:"value", field:(a OR b).
(function () {
  const PODS = [
    {ns: "simplyblock", pod: "simplyblock-webappapi-7c9d4-abcde", container: "webappapi", source: "ip-10-70-1-10"},
    {ns: "simplyblock", pod: "simplyblock-tasks-76fd5-vjnds", container: "tasks-runner-migration", source: "ip-10-70-1-11"},
    {ns: "simplyblock", pod: "simplyblock-tasks-76fd5-vjnds", container: "tasks-runner-backup-merge", source: "ip-10-70-1-11"},
    {ns: "dr-simplyblock", pod: "dr-hub-cc96bdf-bvscz", container: "dr-hub", source: "ip-10-70-1-12"},
    {ns: "simplyblock", pod: "snode-spdk-pod-4420-1102ec", container: "spdk-container", source: "ip-10-70-2-21"},
    {ns: "simplyblock", pod: "snode-spdk-pod-4420-f5fa82", container: "spdk-container", source: "ip-10-70-3-21"}
  ];
  const MSGS = [
    ["INFO", "lvol LVOL_103 created on node 1102ec3c"], ["INFO", "replication snapshot shipped for group wordpress"],
    ["WARN", "retrying connection to hublvol, attempt 2"], ["ERROR", "task failed: migration of lvol LVOL_7 aborted"],
    ["DEBUG", "polling cluster status"], ["INFO", "relocate wordpress site-a -> site-b: step ramen-move"],
    ["ERROR", "python3: can't open file backup_merge_service.py"], ["INFO", "health probe http passed"]
  ];
  const START = Date.now() - 7 * 86400e3;
  const N = 3000;
  const LINES = [];
  for (let i = 0; i < N; i++) {
    const p = PODS[(i * 7) % PODS.length], m = MSGS[(i * 5 + (i >> 3)) % MSGS.length];
    const ts = new Date(START + Math.floor(i * (7 * 86400e3 - 120e3) / N));
    LINES.push({_id: "m" + i, timestamp: ts.toISOString(), source: p.source, message: `${m[0]} ${m[1]} (#${i})`,
      kubernetes_namespace_name: p.ns, kubernetes_pod_name: p.pod, kubernetes_container_name: p.container, kubernetes_host: p.source});
  }
  // lines that keep arriving, so follow mode has something to append
  let tick = 0;
  const fresh = () => {
    const now = Date.now();
    while (tick < Math.floor((now - (START + 7 * 86400e3 - 120e3)) / 3000)) {
      const p = PODS[tick % PODS.length];
      LINES.push({_id: "f" + tick, timestamp: new Date(START + 7 * 86400e3 - 120e3 + tick * 3000).toISOString(), source: p.source,
        message: `INFO live line ${tick}`, kubernetes_namespace_name: p.ns, kubernetes_pod_name: p.pod, kubernetes_container_name: p.container, kubernetes_host: p.source});
      tick++;
    }
  };
  window.SB_MOCK_LOGS = {LINES, PODS};

  // top-level split on " AND " outside quotes and parentheses
  const splitAnd = q => {
    const out = []; let depth = 0, inQ = false, cur = "";
    for (let i = 0; i < q.length; i++) {
      const c = q[i];
      if (c === '"' && q[i - 1] !== "\\") inQ = !inQ;
      if (!inQ && c === "(") depth++;
      if (!inQ && c === ")") depth--;
      if (!inQ && depth === 0 && q.startsWith(" AND ", i)) { out.push(cur); cur = ""; i += 4; continue; }
      cur += c;
    }
    out.push(cur);
    return out.map(x => x.trim()).filter(Boolean);
  };
  const unq = v => { v = v.trim(); return v.startsWith('"') ? v.slice(1, -1).replace(/\\(["\\])/g, "$1") : v.replace(/\\(.)/g, "$1"); };
  const clause = (c, l) => {
    if (c.startsWith("NOT ")) return !clause(c.slice(4), l);
    if (c.startsWith("(") && c.endsWith(")")) return splitAnd(c.slice(1, -1)).every(x => clause(x, l));
    const fm = /^([a-z_]+):(.*)$/.exec(c);
    if (fm) {
      const f = fm[1], v = fm[2].trim();
      const vals = v.startsWith("(") ? v.slice(1, -1).split(/\s+OR\s+/).map(unq) : [unq(v)];
      const have = String(l[f] || "");
      return f === "message" ? vals.some(x => have.toLowerCase().includes(x.toLowerCase())) : vals.includes(have);
    }
    if (c === "*") return true;
    return l.message.toLowerCase().includes(unq(c).toLowerCase());
  };
  const match = (q, l) => !q || q === "*" || splitAnd(q).every(c => clause(c, l));

  const prev = window.fetch.bind(window);
  window.fetch = async function (input, init) {
    const cfg = window.SB_CONFIG;
    const url = typeof input === "string" ? input : input.url;
    const base = cfg.graylogBase || "/graylog/api";
    if (!cfg.mock || !url.startsWith(base)) return prev(input, init);
    const json = (b, s) => new Response(JSON.stringify(b), {status: s || 200, headers: {"Content-Type": "application/json"}});
    if (((init && init.method) || "GET").toUpperCase() !== "GET") return json({type: "ApiError", message: "GET only"}, 405);
    const u = new URL(url, "http://localhost/");
    const kind = u.pathname.slice(base.length);
    if (!/^\/search\/universal\/(absolute|relative)$/.test(kind)) return json({type: "ApiError", message: "not found"}, 404);
    fresh();
    const p = u.searchParams;
    let from, to;
    if (kind.endsWith("relative")) { to = Date.now(); from = to - Number(p.get("range") || 300) * 1000; }
    else { from = Date.parse(p.get("from")); to = Date.parse(p.get("to")); if (isNaN(from) || isNaN(to)) return json({type: "ApiError", message: "from/to required"}, 400); }
    const q = p.get("query") || "*";
    const hit = LINES.filter(l => { const t = Date.parse(l.timestamp); return t >= from && t <= to && match(q, l); });
    hit.sort((a, b) => a.timestamp < b.timestamp ? -1 : a.timestamp > b.timestamp ? 1 : 0);
    if (p.get("sort") === "timestamp:desc") hit.reverse();
    const offset = Number(p.get("offset") || 0), limit = Number(p.get("limit") || 150);
    if (offset + limit > 10000) return json({type: "ApiError", message: "Result window is too large, from + size must be less than or equal to: [10000]"}, 500);
    window.SB_MOCK_LOGS.lastQuery = {query: q, from: new Date(from).toISOString(), to: new Date(to).toISOString(), offset, limit, sort: p.get("sort")};
    await new Promise(r => setTimeout(r, 30));
    return json({query: q, built_query: "{}", used_indices: [], time: 3, total_results: hit.length,
      from: new Date(from).toISOString(), to: new Date(to).toISOString(), fields: ["source", "message", "timestamp"],
      messages: hit.slice(offset, offset + limit).map(m => ({message: m, index: "graylog_0", highlight_ranges: {}, decoration_stats: null}))});
  };
})();
