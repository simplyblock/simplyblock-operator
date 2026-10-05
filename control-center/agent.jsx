// ---------------------------------------------------------------------------
// OPERATOR-AGENT / PROMETHEUS CLIENT
// These are served by the operator, not by the control plane. Container logs
// should move to the Kubernetes pod-log subresource (k8s.logs); the rest —
// SMART, SPDK streams, FoundationDB backups — have no CRD and no core object,
// so they stay on the operator API until the model covers them. — data that the control plane API v2 does NOT
// expose. Read straight from the control plane containers, the node agent and
// Prometheus. Kept apart from api.jsx on purpose: different base URL,
// different auth, different availability guarantees.
// ---------------------------------------------------------------------------
const AGENT_BASE = window.SB_CONFIG.agentBase;
const PROM_BASE = window.SB_CONFIG.promBase;

async function areq(base, path) {
  if (base === AGENT_BASE && !upstreamOn("operator"))
    throw notInDeployment("The operator's agent API", "that serves this view is not part of this deployment", path);
  if (base === PROM_BASE && !upstreamOn("prometheus"))
    throw notInDeployment("Prometheus", "is not part of this deployment", path);
  let res;
  try { res = await fetch(base + path, {headers: {Accept: "application/json", Authorization: `Bearer ${window.SB_CONFIG.token}`}}); }
  catch (e) { throw new ApiError(0, "Agent unreachable — this data is read directly from the containers", path); }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  if (!res.ok || (body && body.status === false)) throw new ApiError(res.status, (body && body.error) || "Request failed", path);
  return body && body.results !== undefined ? body.results : body;
}
const asend = (base, method, path, payload) => fetch(base + path, {
  method, headers: {"Content-Type": "application/json", Authorization: `Bearer ${window.SB_CONFIG.token}`},
  body: JSON.stringify(payload || {})
}).then(async res => {
  let b = null; try { b = await res.json(); } catch (e) {}
  if (!res.ok || (b && b.status === false)) throw new ApiError(res.status, (b && b.error) || "Request failed", path);
  return b;
});

const normContainer = c => ({
  name: c.name, group: c.group, image: c.image, state: c.state,
  cpu: {alloc: c.cpu_cores_alloc, pct: c.cpu_pct},
  mem: {used: c.mem_used, limit: c.mem_limit},
  disk: {used: c.disk_used, limit: c.disk_limit},
  restarts: c.restarts, uptimeH: c.uptime_h
});
const normFdb = b => ({
  id: b.id, version: b.version, createdAt: b.created_at, size: b.size, type: b.type,
  status: b.status, restoreRequestedAt: b.restore_requested_at || null
});

// Without the operator's agent the control plane's services are read where
// they run: the pods in the console's namespace, from the Kubernetes API.
// Pods report no live CPU/memory use; the allocation shown is their limits.
const memQty = q => {
  const m = /^([0-9.]+)\s*([KMGTP]i?)?$/.exec(String(q || "").trim());
  if (!m) return 0;
  const mul = {K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}[m[2]] || 1;
  return Number(m[1]) * mul;
};
const cpuQty = q => { const s = String(q || ""); return s.endsWith("m") ? Number(s.slice(0, -1)) / 1000 : Number(s) || 0; };
const podGroup = p => {
  const l = p.metadata.labels || {};
  const app = l.app || l["app.kubernetes.io/name"] || l["app.kubernetes.io/component"] || "";
  if (/graylog|opensearch|mongo|grafana|prometheus|thanos|fluent/.test(app + p.metadata.name)) return "observability";
  if (/fdb|foundationdb/.test(app + p.metadata.name)) return "state database";
  if (/control-center|operator|reloader/.test(app + p.metadata.name)) return "operator & console";
  return "control plane";
};
const podContainer = p => {
  const cs = (p.spec || {}).containers || [];
  const st = (p.status || {}).containerStatuses || [];
  const lim = k => cs.reduce((s, c) => s + (k === "cpu" ? cpuQty : memQty)((((c.resources || {}).limits) || {})[k]), 0);
  const started = (p.status || {}).startTime ? Date.parse(p.status.startTime) : null;
  const ready = st.length > 0 && st.every(c => c.ready);
  return {name: p.metadata.name, group: podGroup(p), image: (cs[0] || {}).image || "",
    state: (p.status || {}).phase === "Running" && ready ? "running" : String((p.status || {}).phase || "unknown").toLowerCase(),
    cpu_cores_alloc: lim("cpu"), cpu_pct: 0, mem_used: 0, mem_limit: lim("memory"), disk_used: 0, disk_limit: 0,
    restarts: st.reduce((s, c) => s + (c.restartCount || 0), 0),
    uptime_h: started ? Math.max(0, Math.round((Date.now() - started) / 36e5)) : 0, metrics: false};
};
const LOG_LEVEL = /\b(DEBUG|INFO|WARN(?:ING)?|ERROR|FATAL|CRITICAL)\b/i;
async function podLogs(name) {
  const path = pathFor("Pod", {name, subresource: "log"}) + "?tailLines=300&timestamps=true";
  let res;
  try { res = await fetch(window.SB_CONFIG.k8sBase + path, {headers: {Accept: "text/plain", Authorization: `Bearer ${window.SB_CONFIG.token}`}}); }
  catch (e) { throw new ApiError(0, "Cannot reach the Kubernetes API", path, "Unreachable"); }
  const text = await res.text();
  if (!res.ok) { let b = null; try { b = JSON.parse(text); } catch (e) {} throw new ApiError(res.status, (b && b.message) || "Request failed", path, (b && b.reason) || null); }
  return text.split("\n").filter(Boolean).map(line => {
    const sp = line.indexOf(" ");
    const ts = sp > 0 ? line.slice(0, sp) : "", msg = sp > 0 ? line.slice(sp + 1) : line;
    const lv = (LOG_LEVEL.exec(msg) || [])[1] || "INFO";
    return {ts, level: lv.toUpperCase().replace("WARNING", "WARN").replace("CRITICAL", "ERROR").replace("FATAL", "ERROR"), msg};
  });
}

const agent = {
  // The control plane is one deployment across every cluster, so none of these
  // are scoped by cluster.
  containers: () => upstreamOn("operator")
    ? areq(AGENT_BASE, "/control-plane/containers").then(r => r.map(normContainer))
    : k8s.list("Pod").then(ps => ps.map(p => Object.assign(normContainer(podContainer(p)), {metrics: false}))),
  containerLogs: name => upstreamOn("operator") ? areq(AGENT_BASE, `/control-plane/containers/${name}/logs`) : podLogs(name),
  fdbBackups: () => areq(AGENT_BASE, "/control-plane/fdb/backups").then(r => r.map(normFdb)),
  fdbRestore: id => asend(AGENT_BASE, "POST", `/control-plane/fdb/backups/${id}/restore`),
  nodeLogs: (nid, stream) => areq(AGENT_BASE, `/nodes/${nid}/logs/${stream}`),
  smart: did => areq(AGENT_BASE, `/devices/${did}/smart`).then(r => r[0]),
  smartRefresh: did => asend(AGENT_BASE, "POST", `/devices/${did}/smart/refresh`),
  // Prometheus: spdk_thread_busy_percent{node="<uuid>"}
  spdkThreads: nid => areq(PROM_BASE, `/query?query=${encodeURIComponent(`spdk_thread_busy_percent{node="${nid}"}`)}`)
    .then(d => (d.data.result || []).map(r => ({
      name: r.metric.thread, core: Number(r.metric.core), busy: Number(r.value[1])
    })).sort((a, b) => a.core - b.core || a.name.localeCompare(b.name)))
};

const SourceTag = ({what}) => (
  <span className="srctag" title={`Not part of control plane API v2 — read from ${what}`}><Icon n="alert" s={10} />{what}</span>
);

Object.assign(window, {agent, AGENT_BASE, PROM_BASE, SourceTag, normContainer, normFdb, podContainer, memQty});
