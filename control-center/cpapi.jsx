// ---------------------------------------------------------------------------
// CONTROL PLANE API (simplyblock management API v2) — read-only
//
// Where the storage clusters are not CRDs in this Kubernetes cluster — a hub
// whose control plane manages storage clusters on other sites (the managed
// operators keep their StorageCluster objects there) — the control plane is
// the one place that knows every cluster, node, device, pool and volume. The
// console reads it through its own proxy (/controlplane/), which attaches the
// console's identity and scrubs credentials out of every response before it
// reaches the browser. Writes never go this way: actions stay Ops objects on
// the Kubernetes API.
//
// This file speaks the v2 wire format and turns it into the records the
// normalizers in api.jsx already read, so the screens render unchanged.
// ---------------------------------------------------------------------------
const CP_SB = window.SB_CONFIG;
// Off in the fixture backend and wherever the pod does not proxy the control
// plane: the screens then keep reading CRDs and the operator API as before.
const cpOn = () => !CP_SB.mock && !!CP_SB.cpBase && !!(CP_SB.upstreams && CP_SB.upstreams.controlPlane);
// Which optional upstream this pod proxies. A config without the table (the
// fixture backend, an older pod) is taken to have them all.
const upstreamOn = name => !CP_SB.upstreams || CP_SB.upstreams[name] !== false;

// A screen whose source is not part of this deployment. The console renders it
// as a neutral "not available" state, never as a failure.
const notInDeployment = (what, why, path) =>
  new ApiError(503, `${what} ${why || "is not part of this deployment"}`, path || "", "NotInDeployment");

const CP_TTL_MS = 3000;
const cpCache = new Map();
const cpErr = (status, message, path, reason) => {
  const e = new ApiError(status, message, path, reason);
  e.source = "control plane API";
  return e;
};
// GET only, deduplicated for a few seconds: a cluster view asks for the same
// node list from several panels at once.
function cpGet(path) {
  const now = Date.now();
  const hit = cpCache.get(path);
  if (hit && now - hit.t < CP_TTL_MS) return hit.p;
  const p = (async () => {
    let res;
    try {
      res = await fetch(CP_SB.cpBase + path, {headers: {Accept: "application/json"}});
    } catch (e) { throw cpErr(0, "Cannot reach the control plane API", path, "Unreachable"); }
    let body = null;
    try { body = await res.json(); } catch (e) {}
    if (!res.ok || (body && body.kind === "Status" && body.status === "Failure")) {
      if (res.status === 401 || res.status === 403)
        throw cpErr(res.status, "The control plane did not accept the console's identity. Its service account must be one of the control plane's admin accounts (the chart adds it when controlCenter.enabled), or mount an admin token (controlCenter.controlPlane.tokenSecret)", path, "Unauthorized");
      if (body && body.reason === "NotInDeployment") throw cpErr(503, body.message, path, "NotInDeployment");
      const detail = body && (body.message || (typeof body.detail === "string" ? body.detail : body.detail && JSON.stringify(body.detail)));
      throw cpErr((body && body.code) || res.status, detail || `The control plane answered HTTP ${res.status}`, path, (body && body.reason) || null);
    }
    return body;
  })();
  cpCache.set(path, {t: now, p});
  p.catch(() => cpCache.delete(path));
  return p;
}

// ---- small helpers ---------------------------------------------------------
const cpCap = o => (o && o.capacity) || {};
// a storage node's hostname carries its RPC port ("ip-10-70-2-22_4420"); the
// machine is the part before it
const hostKey = h => String(h || "").replace(/_\d+$/, "");
// v2 links related records by URL; the id is the last path segment
const idOfUrl = u => (String(u || "").replace(/\/+$/, "").split("/").pop() || null);
const SECRET_FIELD = /^(secret|password|token|access_key|secret_key)$|_(secret|password|token)$/i;
// Defense in depth: the proxy scrubs credentials, and nothing the console keeps
// may carry one either way.
const dropSecrets = o => {
  const out = {};
  Object.keys(o || {}).forEach(k => { if (!SECRET_FIELD.test(k)) out[k] = o[k]; });
  return out;
};

// ---- raw reads -------------------------------------------------------------
const cpRaw = {
  clusters: () => cpGet("/clusters/").then(r => (r || []).map(dropSecrets)),
  nodes: cid => cpGet(`/clusters/${cid}/storage-nodes/`),
  devices: (cid, nid) => cpGet(`/clusters/${cid}/storage-nodes/${nid}/devices/`),
  // the node's data NICs: [{"ID", "Device name", "Address", "Net type", "Status"}]
  nics: (cid, nid) => cpGet(`/clusters/${cid}/storage-nodes/${nid}/nics`),
  pools: cid => cpGet(`/clusters/${cid}/storage-pools/`),
  volumes: (cid, pid) => cpGet(`/clusters/${cid}/storage-pools/${pid}/volumes/`),
  snapshots: (cid, pid) => cpGet(`/clusters/${cid}/storage-pools/${pid}/snapshots/`),
  tasks: cid => cpGet(`/clusters/${cid}/tasks/`),
  logs: cid => cpGet(`/clusters/${cid}/logs`),
  alerts: cid => cpGet(`/clusters/${cid}/alerts/`),
  cgs: cid => cpGet(`/clusters/${cid}/consistency-groups/`)
};

// ---- site mapping ----------------------------------------------------------
// Which managed cluster (site) a storage cluster runs on. The DR hub's
// SiteProfiles carry each site's node inventory as its dr-agent reported it;
// a storage node runs on one of those machines. A StorageSiteDeployment names
// its cluster directly when the storage was deployed from the hub.
async function cpSites() {
  const [profiles, deploys] = await Promise.all([
    k8s.list("SiteProfile").catch(() => []),
    k8s.list("StorageSiteDeployment", {allNamespaces: true}).catch(() => [])]);
  const byHost = {}, invByHost = {};
  profiles.forEach(p => ((((p.status || {}).inventory) || {}).nodes || []).forEach(n => {
    byHost[n.name] = p.metadata.name; invByHost[n.name] = {node: n, site: p.metadata.name};
  }));
  const byCluster = {};
  deploys.forEach(d => {
    const sc = (d.status || {}).storageCluster || {};
    const id = sc.uuid || sc.clusterId || sc.id;
    if (id && d.spec && d.spec.cluster) byCluster[id] = d.spec.cluster;
  });
  return {byHost, byCluster, profiles, invByHost, deploys};
}
const siteOf = (sites, clusterId, nodes) => sites.byCluster[clusterId]
  || (nodes || []).map(n => sites.byHost[hostKey(n.hostname)]).find(Boolean) || "";

// ---- v2 record -> console wire record ---------------------------------------
// The tiles color by STATUS_META; a state the console has no entry for (a new
// control plane state) is folded into the nearest one rather than crashing it.
const knownStatus = st => {
  const x = String(st || "");
  if (window.STATUS_META && STATUS_META[x]) return x;
  if (/fail|error/.test(x)) return "error";
  if (/read_only/.test(x)) return "degraded";
  if (/^in_|ing$/.test(x)) return "in_activation";
  if (/new/.test(x)) return "in_creation";
  return "offline";
};
// A storage node's data NICs from the control plane's nics list; the port is
// the node's volume subsystem port.
const wireNics = (nics, n) => (nics || []).map(x => ({name: x["Device name"] || x.if_name || "", ip: x.Address || x.ip4_address || "",
  port: n.lvol_subsys_port, state: String(x.Status || x.status || "").toLowerCase() === "online" ? "up" : (x.Status || x.status || null),
  trtype: x["Net type"] || x.trtype || null}));
// ANA multipath: a volume of this node is also reachable through its
// secondary (and tertiary) node -- that is simplyblock's multipathing.
const anaPaths = n => 1 + (n.secondary_node_id ? 1 : 0) + (n.tertiary_node_id ? 1 : 0);
const wireNode = (n, site, nics) => ({
  uuid: n.id, cluster_id: n.cluster_id, host_id: `${n.cluster_id}:${hostKey(n.hostname)}`,
  hostname: hostKey(n.hostname), mgmt_ip: n.mgmt_ip, status: knownStatus(n.status),
  data_nics: nics ? wireNics(nics, n) : null,
  ana_paths: anaPaths(n), secondary_node_id: n.secondary_node_id || null, tertiary_node_id: n.tertiary_node_id || null,
  failure_domain: n.failure_domain >= 0 ? n.failure_domain : null,
  size_total: cpCap(n).size_total || 0, size_util: cpCap(n).size_used || 0,
  devices_count: n.device_count || 0, devices_online: n.online_device_count || 0,
  cpu_count: n.cpu_total_count, vcpu_reserved: n.cpu_spdk_count,
  // in-use memory/hugepages and the SPDK version are not part of the record
  memory_total: n.memory || null, memory_reserved: n.spdk_mem || null, hugepages_total: n.hugepage_memory || null,
  memory_used: null, hugepages_used: null, spdk_version: null,
  max_subsystem_count: n.lvols_max || null, lvols: n.lvols || 0, site: site || ""
});
const wireDevice = (d, node) => ({
  uuid: d.id, node_id: d.storage_node_id, cluster_id: d.cluster_id,
  host_id: node ? `${d.cluster_id}:${hostKey(node.hostname)}` : null,
  cluster_device_class: d.bdev_type === "nvme" ? "nvme" : "blockdev",
  serial_number: d.serial_number, pcie_address: d.pcie_address,
  device_name: d.device_path || d.nvme_controller, model_number: d.model,
  status: knownStatus(d.status),
  // the console's health vocabulary is good / warn / critical
  health_check: d.health_check === false ? "critical" : d.health_check === null || d.health_check === undefined ? "warn" : "good",
  size_total: cpCap(d).size_total || d.size || 0, size_util: cpCap(d).size_used || 0
});
const qosOf = o => (o.max_rw_iops || o.max_rw_mbytes || o.max_r_mbytes || o.max_w_mbytes)
  ? {rw_ios_per_sec: o.max_rw_iops || 0, rw_mbytes_per_sec: o.max_rw_mbytes || 0,
    r_mbytes_per_sec: o.max_r_mbytes || 0, w_mbytes_per_sec: o.max_w_mbytes || 0} : null;
const wirePool = (p, vols, snaps) => ({
  uuid: p.id, cluster_id: p.cluster_id, pool_name: p.name, enabled: p.status === "active",
  dhchap_bidirectional: !!p.dhchap, qos: qosOf(p),
  size_prov: cpCap(p).size_total || p.max_size || 0, size_util: cpCap(p).size_used || 0,
  lvols_count: (vols || []).length, lvols_online: (vols || []).filter(v => v.status === "online").length,
  snapshots_count: (snaps || []).length, storage_classes: []
});
const wireVolume = (v, nodesById, cgsById) => {
  const ids = (v.nodes || []).map(idOfUrl).filter(Boolean);
  const ref = id => id ? {uuid: id, hostname: nodesById && nodesById[id] ? hostKey(nodesById[id].hostname) : id} : null;
  return {
    uuid: v.id, pool_id: v.pool_uuid, pool_name: v.pool_name, cluster_id: v.cluster_id,
    lvol_name: v.name, status: knownStatus(v.status), nqn: v.nqn,
    nodes: {primary: ref(ids[0] || v.storage_node_id), secondary: ref(ids[1]), tertiary: ref(ids[2])},
    size_prov: v.size || 0, size_util: cpCap(v).size_used || 0, qos: qosOf(v),
    pvc: null, pvc_name: v.pvc_name || "", pvc_namespace: v.namespace || "",
    replication: v.do_replicate ? {status: "replicating", mode: "async", consistency_group: v.group_id || null} : null,
    consistency_groups: v.group_id ? [{uuid: idOfUrl(v.group_id), name: ((cgsById || {})[idOfUrl(v.group_id)] || {}).name || idOfUrl(v.group_id)}] : []
  };
};
const wireSnapshot = (s, pool) => ({
  uuid: s.id, cluster_id: pool.cluster_id, pool_id: pool.id, pool_name: pool.name,
  lvol_id: idOfUrl(s.lvol), lvol_name: "", snapshot_name: s.name, status: knownStatus(s.status),
  seq: s.group_seq || null, created_at: s.created_at, size: s.size || 0
});
const wireTask = t => ({
  uuid: t.id, cluster_id: t.cluster_id, function_name: t.function_name,
  target_id: t.device_id || t.storage_node_id || null, node_id: t.storage_node_id,
  status: t.status, result: t.function_result, retry: t.retry, max_retry: t.max_retry,
  canceled: t.canceled
});
const wireLog = l => ({
  uuid: l.id, ts: l.date, level: l.level, event: l.event, message: l.message,
  node_id: l.node_id, storage_id: l.storage_id, vuid: l.vuid, record_status: l.status
});
const wireAlert = (a, c) => ({
  uuid: a.id, cluster_id: a.cluster_id, cluster_name: c ? c.name : "",
  rule: a.kind, severity: a.severity, scope: a.device_id ? "device" : a.node_id ? "node" : "cluster",
  title: a.message, detail: Object.keys(a.details || {}).length ? JSON.stringify(a.details) : "",
  node_id: a.node_id, device_ids: a.device_id ? [a.device_id] : [],
  since: a.since || a.first_seen, silenced: false
});
const wireCg = g => ({uuid: g.id, cluster_id: g.cluster_id, name: g.name, status: "online",
  lvols_count: g.member_count || 0});
const wireCluster = (c, nodes, pools, site) => {
  const hosts = new Set(nodes.map(n => hostKey(n.hostname)));
  const online = nodes.filter(n => n.status === "online");
  return {
    uuid: c.id, name: c.name || c.id, status: knownStatus(c.status), rebalancing: !!c.is_re_balancing,
    device_class: c.device_mode === "nvme" ? "nvme" : "blockdev",
    size_total: cpCap(c).size_total || 0, size_util: cpCap(c).size_used || 0,
    hosts_count: hosts.size, hosts_available: new Set(online.map(n => hostKey(n.hostname))).size,
    storage_nodes_count: nodes.length, storage_nodes_online: online.length,
    devices_count: nodes.reduce((s, n) => s + (n.device_count || 0), 0),
    devices_online: nodes.reduce((s, n) => s + (n.online_device_count || 0), 0),
    pools_count: pools.length, lvols_count: nodes.reduce((s, n) => s + (n.lvols || 0), 0),
    distr_ndcs: c.distr_ndcs, distr_npcs: c.distr_npcs, ha_type: c.ha ? "ha" : "single",
    backup_enabled: !!c.backup_enabled, failure_domain_enabled: !!c.enable_failure_domain,
    node_affinity: c.node_affinity ? "node" : "none",
    mgmt_endpoint: site ? `hub control plane · site ${site}` : "hub control plane",
    mgmt_endpoint_kind: "control plane API", site: site || ""
  };
};

// ---- the read model ----------------------------------------------------------
// Indexes from the last listing, so a detail link (/storage-nodes/{id}) finds
// its cluster without walking every cluster again.
const cpIdx = {node: {}, device: {}, pool: {}, volume: {}, snapshot: {}};

async function cpClusterBundle(c, sites) {
  const [nodes, pools] = await Promise.all([cpRaw.nodes(c.id).catch(() => []), cpRaw.pools(c.id).catch(() => [])]);
  nodes.forEach(n => { cpIdx.node[n.id] = c.id; });
  pools.forEach(p => { cpIdx.pool[p.id] = c.id; });
  return {c, nodes, pools, site: siteOf(sites, c.id, nodes)};
}
async function cpBundles() {
  const [cs, sites] = await Promise.all([cpRaw.clusters(), cpSites()]);
  return Promise.all(cs.map(c => cpClusterBundle(c, sites)));
}
async function cpBundle(cid) {
  const cs = await cpRaw.clusters();
  const c = cs.find(x => x.id === cid);
  if (!c) throw new ApiError(404, `cluster ${cid} not found in the control plane`, `/clusters/${cid}`, "NotFound");
  return cpClusterBundle(c, await cpSites());
}
const clusterOfNode = async nid => {
  if (!cpIdx.node[nid]) await cpBundles();
  const cid = cpIdx.node[nid];
  if (!cid) throw new ApiError(404, `storage node ${nid} not found in the control plane`, `/storage-nodes/${nid}`, "NotFound");
  return cid;
};
const clusterOfPool = async pid => {
  if (!cpIdx.pool[pid]) await cpBundles();
  const cid = cpIdx.pool[pid];
  if (!cid) throw new ApiError(404, `pool ${pid} not found in the control plane`, `/pools/${pid}`, "NotFound");
  return cid;
};

// ---- hosts: one machine, every source that knows something about it --------
// Kubernetes quantities ("16", "15500m", "32Gi", "8589934592") as numbers.
const QTY = {Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50, k: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, m: 1e-3};
const qty = q => {
  const m = /^([0-9.]+)([a-zA-Z]*)$/.exec(String(q === undefined || q === null ? "" : q).trim());
  if (!m) return null;
  const v = parseFloat(m[1]) * (m[2] ? QTY[m[2]] || NaN : 1);
  return Number.isFinite(v) ? v : null;
};
const hugepagesOf = rl => {
  const ks = Object.keys(rl || {}).filter(k => k.startsWith("hugepages-"));
  return ks.length ? ks.reduce((t, k) => t + (qty(rl[k]) || 0), 0) : null;
};
const hostDevice = d => ({id: d.id, kind: d.bdev_type === "nvme" ? "nvme" : "block", numa_socket: null,
  pcie_address: d.pcie_address || null, device_name: d.device_path || d.nvme_controller || null,
  serial_number: d.serial_number || null, model_number: d.model || null, size: d.size || 0,
  assigned_node_id: d.storage_node_id || null, status: d.status});
// The draft group that names this host as a worker, when the storage was
// deployed from the hub (StorageSiteDeployment): its NICs, sockets, devices.
const draftGroupOf = (deploys, host) => {
  for (const d of deploys || []) {
    const dr = (d.status || {}).draft || {};
    for (const set of dr.nodeSets || []) for (const g of set.groups || []) {
      if ((g.workers || []).includes(host)) return {group: g, cluster: dr.cluster || {}, name: d.metadata.name};
    }
  }
  return null;
};
const CPS = "control plane API", INV = "dr-agent inventory";
// Every field a source knows is set with the source it came from (h.sources);
// a field no source knows stays unset, and the view says so.
function mergeHost(h, sites) {
  const src = {};
  const set = (field, v, from) => {
    if (v === null || v === undefined || v === "" || (Array.isArray(v) && !v.length) || (typeof v === "number" && !Number.isFinite(v))) return;
    h[field] = v; src[field] = from;
  };
  const ns = h._nodes;
  const max = f => { const v = ns.map(f).filter(x => typeof x === "number" && x > 0); return v.length ? Math.max(...v) : null; };
  const sum = f => { const v = ns.map(f).filter(x => typeof x === "number" && x > 0); return v.length ? v.reduce((a, b) => a + b, 0) : null; };
  // the control plane: what its storage nodes recorded of the machine
  set("vcpu_count", max(n => n.cpu_total_count), CPS);
  set("memory_total", max(n => n.memory), CPS);
  set("hugepages_reserved", max(n => n.hugepage_memory), CPS);
  set("hugepages_allocated", sum(n => n.spdk_mem), CPS);
  set("memory_per_pod", max(n => n.spdk_mem), CPS);
  if (h._devsKnown) src.devices = CPS;
  if (h._nicsKnown) { src.nics = CPS; set("data_nics", h.nics.map(x => x.name).filter(Boolean), CPS); }
  // dr-agent: the Kubernetes node (zone, region, rack, cabinet, capacity)
  const inv = (sites.invByHost || {})[h.hostname];
  if (inv) {
    const n = inv.node, cap = n.capacity || {};
    set("zone", n.zone, INV); set("zone_id", n.zone ? zoneId(inv.site, n.zone) : null, INV); set("region", n.region, INV);
    set("rack_id", n.rack, INV); set("cabinet_id", n.cabinet, INV);
    set("k8s_cluster", inv.site, INV);
    // the node's own view of the machine wins over what a storage node recorded
    set("vcpu_count", qty(cap.cpu), INV); set("memory_total", qty(cap.memory), INV); set("hugepages_reserved", hugepagesOf(cap), INV);
    if (n.ready === false && h.status === "available") h.status = "unavailable";
  }
  // a hub-driven deployment: the draft that placed storage on this host
  const dg = draftGroupOf(sites.deploys, h.hostname);
  if (dg) {
    const g = dg.group, from = `StorageSiteDeployment ${dg.name}`;
    set("mgmt_nic", g.mgmtInterface, from);
    if (!src.data_nics) set("data_nics", g.dataInterfaces, from);
    set("numa_sockets_used", (dg.cluster.socketsToUse || []).map(x => parseInt(x, 10)).filter(x => !isNaN(x)), from);
    if (!src.memory_per_pod) set("memory_per_pod", qty(g.spdkSystemMemory), from);
    ((g.devices || {}).nvme || []).forEach(pcie => {
      if (!h.devices.some(d => d.pcie_address === pcie)) {
        h.devices.push({id: `draft:${pcie}`, kind: "nvme", numa_socket: null, pcie_address: pcie, size: 0, assigned_node_id: null, selected: true});
      }
    });
    if (!src.devices && h.devices.length) src.devices = from;
  }
  const kinds = [...new Set(h.devices.map(d => d.kind))];
  if (src.devices) set("host_class", kinds.length ? kinds.join(" + ") : null, src.devices);
  h.devices_assigned = h.devices.filter(d => d.assigned_node_id).length;
  h.devices_free = h.devices.length - h.devices_assigned;
  h.nvme_count = h.devices.filter(d => d.kind === "nvme").length;
  h.sources = src;
  delete h._nodes; delete h._devsKnown; delete h._nicsKnown;
  return h;
}

const cp = {
  on: cpOn,
  clusters: () => cpBundles().then(bs => bs.map(b => wireCluster(b.c, b.nodes, b.pools, b.site))),
  cluster: cid => cpBundle(cid).then(b => wireCluster(b.c, b.nodes, b.pools, b.site)),
  nodes: async cid => {
    const b = await cpBundle(cid);
    const nics = await Promise.all(b.nodes.map(n => cpRaw.nics(cid, n.id).catch(() => null)));
    return b.nodes.map((n, i) => wireNode(n, b.site, nics[i]));
  },
  node: async nid => (await cp.nodes(await clusterOfNode(nid))).find(n => n.uuid === nid),
  devices: async nid => {
    const cid = await clusterOfNode(nid);
    const [ds, ns] = await Promise.all([cpRaw.devices(cid, nid), cpRaw.nodes(cid)]);
    const node = ns.find(n => n.id === nid);
    ds.forEach(d => { cpIdx.device[d.id] = [cid, nid]; });
    return ds.map(d => wireDevice(d, node));
  },
  device: async did => {
    if (!cpIdx.device[did]) {
      const bs = await cpBundles();
      await Promise.all(bs.flatMap(b => b.nodes.map(n => cpRaw.devices(b.c.id, n.id).then(ds => ds.forEach(d => { cpIdx.device[d.id] = [b.c.id, n.id]; })).catch(() => {}))));
    }
    const at = cpIdx.device[did];
    if (!at) throw new ApiError(404, `device ${did} not found in the control plane`, `/devices/${did}`, "NotFound");
    return (await cp.devices(at[1])).find(d => d.uuid === did);
  },
  pools: async cid => {
    const b = await cpBundle(cid);
    return Promise.all(b.pools.map(async p => {
      const [vols, snaps] = await Promise.all([cpRaw.volumes(cid, p.id).catch(() => []), cpRaw.snapshots(cid, p.id).catch(() => [])]);
      return wirePool(p, vols, snaps);
    }));
  },
  pool: async pid => (await cp.pools(await clusterOfPool(pid))).find(p => p.uuid === pid),
  poolVolumes: async pid => {
    const cid = await clusterOfPool(pid);
    const [vols, nodes, cgs] = await Promise.all([cpRaw.volumes(cid, pid), cpRaw.nodes(cid), cpRaw.cgs(cid).catch(() => [])]);
    const byId = Object.fromEntries(nodes.map(n => [n.id, n]));
    const cgById = Object.fromEntries(cgs.map(g => [g.id, g]));
    vols.forEach(v => { cpIdx.volume[v.id] = [cid, pid]; });
    return vols.map(v => wireVolume(v, byId, cgById));
  },
  clusterVolumes: async cid => {
    const b = await cpBundle(cid);
    const lists = await Promise.all(b.pools.map(p => cp.poolVolumes(p.id).catch(() => [])));
    return lists.flat();
  },
  volume: async vid => {
    if (!cpIdx.volume[vid]) {
      const bs = await cpBundles();
      await Promise.all(bs.map(b => cp.clusterVolumes(b.c.id).catch(() => [])));
    }
    const at = cpIdx.volume[vid];
    if (!at) throw new ApiError(404, `volume ${vid} not found in the control plane`, `/lvols/${vid}`, "NotFound");
    return (await cp.poolVolumes(at[1])).find(v => v.uuid === vid);
  },
  // snapshots are listed per pool; a volume's are the pool's filtered by it
  snapshots: async (scope, id) => {
    let cid, pools;
    if (scope === "clusters") { const b = await cpBundle(id); cid = id; pools = b.pools; }
    else if (scope === "pools") { cid = await clusterOfPool(id); pools = (await cpRaw.pools(cid)).filter(p => p.id === id); }
    else {
      if (!cpIdx.volume[id]) await cp.volume(id);
      const [c2, pid] = cpIdx.volume[id]; cid = c2; pools = (await cpRaw.pools(cid)).filter(p => p.id === pid);
    }
    const lists = await Promise.all(pools.map(p => cpRaw.snapshots(cid, p.id).then(ss => ss.map(s => wireSnapshot(s, p))).catch(() => [])));
    const all = lists.flat();
    all.forEach(s => { cpIdx.snapshot[s.uuid] = s; });
    return scope === "lvols" ? all.filter(s => s.lvol_id === id) : all;
  },
  snapshot: async sid => {
    if (!cpIdx.snapshot[sid]) { const bs = await cpBundles(); await Promise.all(bs.map(b => cp.snapshots("clusters", b.c.id).catch(() => []))); }
    const s = cpIdx.snapshot[sid];
    if (!s) throw new ApiError(404, `snapshot ${sid} not found in the control plane`, `/snapshots/${sid}`, "NotFound");
    return s;
  },
  // a storage node runs on one machine; the host view is that machine
  hosts: async cid => {
    const [b, sites] = await Promise.all([cpBundle(cid), cpSites()]);
    const [devs, nics] = await Promise.all([
      Promise.all(b.nodes.map(n => cpRaw.devices(cid, n.id).catch(() => null))),
      Promise.all(b.nodes.map(n => cpRaw.nics(cid, n.id).catch(() => null)))]);
    const by = {};
    b.nodes.forEach((n, i) => {
      const k = hostKey(n.hostname);
      const h = by[k] = by[k] || {uuid: `${cid}:${k}`, cluster_id: cid, hostname: k, mgmt_ip: n.mgmt_ip, source: "kubernetes",
        status: "unavailable", storage_node_ids: [], devices: [], nics: [], size_total: 0, size_assigned: 0, k8s_cluster: b.site || null,
        _nodes: [], _devsKnown: true, _nicsKnown: true};
      h.storage_node_ids.push(n.id); h._nodes.push(n);
      if (n.status === "online") h.status = "available";
      h.size_total += cpCap(n).size_total || 0; h.size_assigned += cpCap(n).size_total || 0;
      if (devs[i]) devs[i].forEach(d => h.devices.push(hostDevice(d)));
      else h._devsKnown = false;
      if (nics[i]) wireNics(nics[i], n).forEach(x => { if (!h.nics.some(y => y.name === x.name)) h.nics.push(x); });
      else h._nicsKnown = false;
    });
    return Object.values(by).map(h => mergeHost(h, sites));
  },
  host: async hid => {
    const cid = String(hid).split(":")[0];
    const h = (await cp.hosts(cid)).find(x => x.uuid === hid);
    if (!h) throw new ApiError(404, `host ${hid} not found`, `/hosts/${hid}`, "NotFound");
    return h;
  },
  tasks: cid => cpRaw.tasks(cid).then(ts => ts.map(wireTask)),
  logs: cid => cpRaw.logs(cid).then(ls => ls.map(wireLog).sort((x, y) => Date.parse(y.ts) - Date.parse(x.ts))),
  cgroups: cid => cpRaw.cgs(cid).then(gs => gs.map(wireCg)),
  cgroup: async gid => {
    const cs = await cpRaw.clusters();
    const lists = await Promise.all(cs.map(c => cpRaw.cgs(c.id).catch(() => [])));
    const g = lists.flat().find(x => x.id === gid);
    if (!g) throw new ApiError(404, `consistency group ${gid} not found in the control plane`, `/consistency-groups/${gid}`, "NotFound");
    return wireCg(g);
  },
  alerts: async cid => {
    const cs = await cpRaw.clusters();
    const c = cs.find(x => x.id === cid);
    return (await cpRaw.alerts(cid)).map(a => wireAlert(a, c));
  },
  // every cluster's alerts: the control plane raises them per cluster
  allAlerts: async () => {
    const cs = await cpRaw.clusters();
    const lists = await Promise.all(cs.map(c => cpRaw.alerts(c.id).then(as => as.map(a => wireAlert(a, c))).catch(() => [])));
    return lists.flat();
  },
  sites: cpSites
};

// Resolve one console path onto the control plane, or null when the control
// plane does not model it (the caller then says it is not available here).
cp.route = path => {
  const seg = path.split("?")[0].split("/").filter(Boolean);
  const [a, id, child] = seg;
  const one = p => p.then(x => [x]);
  if (seg.length === 1) {
    if (a === "clusters") return cp.clusters();
    if (a === "alerts") return cp.allAlerts();
    return null;
  }
  if (seg.length === 2) {
    if (a === "control-plane" && id === "alerts") return cp.allAlerts();
    const one1 = {clusters: cp.cluster, "storage-nodes": cp.node, devices: cp.device, pools: cp.pool, lvols: cp.volume,
      snapshots: cp.snapshot, hosts: cp.host, "consistency-groups": cp.cgroup}[a];
    return one1 ? one(one1(id)) : null;
  }
  if (seg.length === 3) {
    if (a === "clusters") {
      const f = {"storage-nodes": cp.nodes, pools: cp.pools, lvols: cp.clusterVolumes, tasks: cp.tasks, logs: cp.logs,
        alerts: cp.alerts, hosts: cp.hosts, "consistency-groups": cp.cgroups}[child];
      if (f) return f(id);
      if (child === "snapshots") return cp.snapshots("clusters", id);
      return null;
    }
    if (a === "storage-nodes" && child === "devices") return cp.devices(id);
    if (a === "pools" && child === "lvols") return cp.poolVolumes(id);
    if ((a === "pools" || a === "lvols") && child === "snapshots") return cp.snapshots(a, id);
  }
  return null;
};

// ---------------------------------------------------------------------------
// KUBERNETES CLUSTERS ON A HUB — Open Cluster Management + dr-agent inventory
//
// Without the operator API there is no "kubernetes-clusters" collection. On a
// hub the clusters are OCM ManagedClusters, and what each one has (nodes,
// zones, storage classes) is the inventory its dr-agent reports into the
// site's SiteProfile. Storage clusters are matched to sites through the
// control plane (see siteOf).
// ---------------------------------------------------------------------------
const condTrue = (o, type) => ((((o || {}).status || {}).conditions) || []).some(c => c.type === type && c.status === "True");
const condKnown = (o, type) => ((((o || {}).status || {}).conditions) || []).some(c => c.type === type);
const zoneId = (site, z) => `zone:${site}:${z}`;

async function hubSites() {
  const [mcs, profiles, storage] = await Promise.all([
    k8s.list("ManagedCluster").catch(e => {
      if (e.status === 404) throw notInDeployment("The Kubernetes clusters view",
        "reads the hub's Open Cluster Management clusters, and OCM is not installed here (nor is the operator API)", "/apis/cluster.open-cluster-management.io/v1/managedclusters");
      throw e;
    }),
    k8s.list("SiteProfile").catch(() => []),
    cpOn() ? cp.clusters().catch(() => []) : Promise.resolve([])]);
  const prof = Object.fromEntries(profiles.map(p => [p.metadata.name, p]));
  return mcs.map(mc => {
    const name = mc.metadata.name;
    const p = prof[name];
    const inv = ((p || {}).status || {}).inventory || {};
    const nodes = inv.nodes || [];
    const zones = inv.zones && inv.zones.length ? inv.zones : [...new Set(nodes.map(n => n.zone).filter(Boolean))];
    const avail = condTrue(mc, "ManagedClusterConditionAvailable");
    const labels = mc.metadata.labels || {};
    const csi = (inv.storageClasses || []).find(c => /simplyblock/.test(c.driver || ""));
    return {mc, name, profile: p || null, inv, nodes, zones,
      storage: storage.filter(c => c.site === name),
      wire: {
        uuid: mc.metadata.uid || name, name,
        version: ((mc.status || {}).version || {}).kubernetes || "",
        status: avail ? "online" : condKnown(mc, "ManagedClusterConditionAvailable") ? "offline" : "unreachable",
        // the CSI driver is visible as the provisioner of a storage class
        csi_version: csi ? csi.driver : p ? "not seen" : "not reported", csi_status: csi || !p ? "online" : "unreachable",
        api_endpoint: (((mc.spec || {}).managedClusterClientConfigs || [])[0] || {}).url || "",
        environment: [labels.vendor, labels.cloud].filter(x => x && x !== "auto-detect").join(" · ") || "Kubernetes",
        discovered: false, operator_namespace: "simplyblock",
        zone_ids: zones.map(z => zoneId(name, z)),
        storage_cluster_ids: storage.filter(c => c.site === name).map(c => c.uuid),
        worker_nodes_count: nodes.length, storage_classes_count: (inv.storageClasses || []).length,
        created_at: mc.metadata.creationTimestamp,
        // where the numbers come from, for the detail view
        source: "Open Cluster Management + dr-agent inventory",
        inventory_reported_at: ((p || {}).status || {}).reportedAt || null
      }};
  });
}
const hubSite = async id => {
  const s = (await hubSites()).find(x => x.wire.uuid === id || x.name === id);
  if (!s) throw new ApiError(404, `Kubernetes cluster ${id} is not a managed cluster of this hub`, `/kubernetes-clusters/${id}`, "NotFound");
  return s;
};
const hubK8s = {
  clusters: () => hubSites().then(ss => ss.map(s => s.wire)),
  cluster: id => hubSite(id).then(s => s.wire),
  hosts: async id => {
    const s = await hubSite(id);
    const storageNodes = (await Promise.all(s.storage.map(c => cp.nodes(c.uuid).catch(() => [])))).flat();
    return s.nodes.map(n => {
      const cap = n.capacity || {}, h = {
        uuid: `node:${s.name}:${n.name}`, hostname: n.name, status: n.ready ? "available" : "unavailable",
        zone: n.zone || null, zone_id: n.zone ? zoneId(s.name, n.zone) : null, region: n.region || null,
        rack_id: n.rack || null, cabinet_id: n.cabinet || null, k8s_cluster: s.name, source: "kubernetes",
        vcpu_count: qty(cap.cpu), memory_total: qty(cap.memory), hugepages_reserved: hugepagesOf(cap),
        storage_node_ids: storageNodes.filter(x => x.hostname === n.name).map(x => x.uuid), devices: []};
      h.sources = {};
      ["zone", "region", "rack_id", "cabinet_id", "vcpu_count", "memory_total", "hugepages_reserved"].forEach(f => { if (h[f]) h.sources[f] = INV; });
      return h;
    });
  },
  zones: async id => {
    const ss = id ? [await hubSite(id)] : await hubSites();
    return ss.flatMap(s => s.zones.map(z => ({
      uuid: zoneId(s.name, z), name: z, region: "", cluster_ids: s.storage.map(c => c.uuid),
      k8s_cluster_ids: [s.wire.uuid], k8s_clusters: [{uuid: s.wire.uuid, name: s.name}],
      hosts_count: s.nodes.filter(n => n.zone === z).length
    })));
  },
  storageClasses: async id => {
    const s = await hubSite(id);
    return (s.inv.storageClasses || []).map(c => ({
      uuid: `sc:${s.name}:${c.name}`, k8s_cluster_id: s.wire.uuid, name: c.name, provisioner: c.driver || "",
      parameters: {}, spec_parameters: c.labels || {}}));
  },
  storageClusters: id => hubSite(id).then(s => s.storage)
};

Object.assign(window, {cp, cpOn, upstreamOn, notInDeployment, cpGet, hostKey, idOfUrl, hubK8s, hubSites});
