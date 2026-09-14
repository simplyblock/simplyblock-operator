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

const agent = {
  // The control plane is one deployment across every cluster, so none of these
  // are scoped by cluster.
  containers: () => areq(AGENT_BASE, "/control-plane/containers").then(r => r.map(normContainer)),
  containerLogs: name => areq(AGENT_BASE, `/control-plane/containers/${name}/logs`),
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

Object.assign(window, {agent, AGENT_BASE, PROM_BASE, SourceTag, normContainer, normFdb});
