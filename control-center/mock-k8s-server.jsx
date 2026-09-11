// ---------------------------------------------------------------------------
// MOCK API SERVER — routes the three real surfaces.
//   SB_CONFIG.k8sBase       Kubernetes API   (CRDs + core objects + pod logs)
//   SB_CONFIG.operatorBase  operator API     (releases, SSE, /proposed/*)
//   SB_CONFIG.helmBase      Helm             (release state)
// Everything else falls through to the network.
// ---------------------------------------------------------------------------
const SBM = window.SB_MOCK = Object.assign({
  latency: [90, 260], failRate: 0, forceEmpty: false, offline: false, failNext: false, requests: 0
}, window.SB_MOCK);

const jsonRes = (body, status) => new Response(JSON.stringify(body),
  {status: status === undefined ? 200 : status, headers: {"Content-Type": "application/json"}});
// A Kubernetes failure is a Status object, not a bare error string.
const status4 = (code, reason, message) => jsonRes({
  kind: "Status", apiVersion: "v1", status: "Failure", code, reason, message
}, code);
const listOf = (kind, items) => ({apiVersion: kind.startsWith("Storage") || kind.startsWith("Control")
  || kind.startsWith("Simplyblock") || kind.startsWith("Cluster") || kind.startsWith("Operator")
  ? window.API_GROUP : "v1", kind: kind + "List",
  metadata: {resourceVersion: String(Date.now() % 100000)}, items});

const DB2 = () => window.SB_DB;
const U2 = () => window.SB_UTIL;
const PLURALS = {};
Object.entries(window.RESOURCES).forEach(([k, r]) => { PLURALS[r.plural] = k; });

// ---- read: fixture collection -> k8s objects, with label-selector filtering -
function readList(kind, params) {
  if (SBM.forceEmpty) return [];
  let items = [];
  if (window.COLL[kind]) {
    const [coll] = window.COLL[kind];
    items = (DB2()[coll] || []).map(window.TO_K8S[kind]);
  } else if (window.RESOURCES[kind] && window.RESOURCES[kind].ops !== undefined) {
    items = (DB2().ops || []).filter(o => o.kind === kind).map(window.opsToK8s);
  } else if (kind === "PersistentVolumeClaim") {
    items = (DB2().pvcs || []).map(pvcToK8s);
  } else if (kind === "StorageClass") {
    items = (DB2().storage_classes || []).map(scToK8s);
  } else if (kind === "PersistentVolume") {
    items = (DB2().lvols || []).map(pvToK8s);
  } else if (kind === "Node") {
    items = (DB2().hosts || []).map(nodeToK8s);
  } else if (kind === "Pod") {
    items = (DB2().containers || []).map(podToK8s);
  } else if (kind === "ControlPlane") {
    items = (DB2().clusters || []).map(cpToK8s);
  } else if (kind === "SimplyblockDriver") {
    items = (DB2().k8s_clusters || []).map(driverToK8s);
  } else if (kind === "ClusterDeploymentConfig") {
    items = (DB2().deployment_configs || []).map(cdcToK8s);
  } else if (NS_KINDS_M[kind]) {
    items = nsResources(params.get("__ns")).filter(o => o.kind === kind);
  }
  const ls = params.get("labelSelector");
  if (ls) {
    const want = ls.split(",").map(p => p.split("="));
    items = items.filter(o => want.every(([k, v]) =>
      ((o.metadata.labels || {})[k] || "") === v));
  }
  const fs = params.get("fieldSelector");
  if (fs && fs.startsWith("metadata.name=")) {
    const n = fs.slice(14);
    items = items.filter(o => o.metadata.name === n);
  }
  return items;
}

// ---- application resources per namespace ----------------------------------
// Generated once per namespace from the PVCs that live there — what a real API
// server would return for the app's Deployments, StatefulSets, Services, ...
const NS_KINDS_M = {Deployment: 1, StatefulSet: 1, Service: 1, ConfigMap: 1, Secret: 1, Ingress: 1, VirtualMachine: 1};
const NS_CACHE = {};
function nsResources(ns) {
  if (!ns) return [];
  if (NS_CACHE[ns]) return NS_CACHE[ns];
  const U = U2();
  const pvcs = (DB2().pvcs || []).filter(p => p.namespace === ns);
  const apps = [...new Set(pvcs.map(p => (p.pvc_name || "data").replace(/^(data|pvc|vol)-/, "").split("-")[0]))].slice(0, 3);
  if (!apps.length) apps.push(ns.split("-")[0]);
  const mk = (kind, name, app, extra) => Object.assign({apiVersion: kind === "Deployment" || kind === "StatefulSet" ? "apps/v1" : kind === "Ingress" ? "networking.k8s.io/v1" : kind === "VirtualMachine" ? "kubevirt.io/v1" : "v1",
    kind, metadata: {name, namespace: ns, uid: U.uuid(), creationTimestamp: U.ago(U.int(100, 3000)),
      labels: Object.assign({"app.kubernetes.io/name": app, "app.kubernetes.io/instance": app + "-" + ns}, extra || {})}}, {});
  const out = [];
  apps.forEach((app, i) => {
    const vm = /vm|win|desktop/.test(app);
    if (vm) out.push(mk("VirtualMachine", app, app, {"kubevirt.io/domain": app}));
    else {
      out.push(mk("StatefulSet", app, app, {"app.kubernetes.io/component": "db"}));
      if (i === 0) out.push(mk("Deployment", app + "-api", app, {"app.kubernetes.io/component": "api"}), mk("Deployment", app + "-worker", app, {"app.kubernetes.io/component": "worker"}));
      out.push(mk("Ingress", app, app));
    }
    out.push(mk("Service", app, app), mk("ConfigMap", app + "-config", app), mk("Secret", app + "-credentials", app));
  });
  out.push(mk("ConfigMap", "kube-root-ca.crt", ns, {}), mk("Secret", "default-token", ns, {}));
  return (NS_CACHE[ns] = out);
}

// ---- core object mappers ---------------------------------------------------
const d2 = s => window.dnsName(s);
const cName = id => d2((DB2().clusters.find(c => c.uuid === id) || {}).name);

const pvToK8s = v => ({
  apiVersion: "v1", kind: "PersistentVolume",
  metadata: {name: d2(v.lvol_name), uid: v.uuid, creationTimestamp: v.created_at,
    labels: {[window.GROUP + "/cluster"]: cName(v.cluster_id),
      [window.GROUP + "/pool"]: d2(v.pool_name)},
    annotations: Object.assign({"pv.kubernetes.io/provisioned-by": "csi.simplyblock.io"},
      v.bucket ? {[window.GROUP + "/bucket"]: v.bucket.name} : {})},
  spec: {
    capacity: {storage: Math.round(v.size_prov / 1e9) + "Gi"},
    accessModes: [v.pvc && v.pvc.access_mode ? v.pvc.access_mode : "ReadWriteOnce"],
    persistentVolumeReclaimPolicy: "Delete",
    storageClassName: v.pvc ? d2(v.pvc.storage_class) : null,
    claimRef: v.pvc ? {kind: "PersistentVolumeClaim", namespace: v.pvc.namespace, name: v.pvc.name} : null,
    csi: {driver: "csi.simplyblock.io", volumeHandle: v.uuid, fsType: (v.pvc && v.pvc.filesystem) || "ext4",
      volumeAttributes: {
        // everything simplyblock-specific about a volume travels here
        pool_name: v.pool_name, cluster_id: v.cluster_id,
        encryption: String(!!v.crypto_enabled), compression: String(!!v.compression_dedup_enabled),
        nqn: v.nqn, qos_rw_iops: String((v.qos || {}).rw_ios_per_sec || 0)
      }}
  },
  status: {phase: v.status === "online" ? "Bound" : "Available",
    // status the CSI driver reports back from the control plane
    simplyblock: {
      volumeId: v.uuid, used: String(v.size_util), logicalUsed: String(v.logical_used || v.size_util),
      nodes: v.nodes || {}, snapshots: v.snapshots_count || 0,
      backupVersions: v.backup_versions_count || 0,
      health: v.status === "online" ? "Healthy" : "Unavailable"
    }}
});

const pvcToK8s = p => ({
  apiVersion: "v1", kind: "PersistentVolumeClaim",
  metadata: {name: p.pvc_name, namespace: p.namespace, uid: p.uuid,
    creationTimestamp: p.created_at, labels: p.labels || {}, annotations: p.annotations || {}},
  spec: {accessModes: [p.access_mode], volumeMode: p.volume_mode,
    storageClassName: d2(p.storage_class),
    resources: {requests: {storage: Math.round(p.requested_bytes / 1e9) + "Gi"}},
    volumeName: p.lvol_id ? d2((DB2().lvols.find(v => v.uuid === p.lvol_id) || {}).lvol_name) : null},
  status: {phase: p.status,
    capacity: p.actual_bytes ? {storage: Math.round(p.actual_bytes / 1e9) + "Gi"} : {},
    accessModes: [p.access_mode]}
});

const scToK8s = s => ({
  apiVersion: "storage.k8s.io/v1", kind: "StorageClass",
  metadata: {name: d2(s.name), uid: s.uuid, creationTimestamp: s.created_at,
    labels: {[window.GROUP + "/pool"]: d2(s.pool_name)},
    annotations: Object.assign({[window.GROUP + "/storage-pool"]: s.storage_pool_ref || ""},
      s.is_default ? {"storageclass.kubernetes.io/is-default-class": "true"} : {})},
  provisioner: s.provisioner, parameters: s.parameters || {},
  reclaimPolicy: s.reclaim_policy, volumeBindingMode: s.volume_binding_mode,
  allowVolumeExpansion: s.allow_volume_expansion !== false
});

const nodeToK8s = h => ({
  apiVersion: "v1", kind: "Node",
  metadata: {name: h.hostname, uid: h.uuid, creationTimestamp: h.prepared_at,
    labels: Object.assign({"kubernetes.io/os": "linux"}, h.k8s_labels || {}, h.labels || {}),
    annotations: h.migration_taint ? {[window.GROUP + "/migration-target"]: "true"} : {}},
  spec: {taints: h.migration_taint ? [{key: window.GROUP + "/migration-target", value: "true", effect: "NoSchedule"}] : []},
  status: {
    conditions: [{type: "Ready", status: h.status === "unreachable" ? "Unknown" : "True",
      reason: "KubeletReady", lastTransitionTime: h.prepared_at}],
    capacity: {cpu: String(h.vcpu_count), memory: Math.round(h.memory_total / 1e9) + "Gi",
      "hugepages-2Mi": Math.round((h.hugepages_reserved || 0) / 1e9) + "Gi"},
    nodeInfo: {kubeletVersion: h.kubelet_version || null, operatingSystem: "linux"},
    addresses: [{type: "InternalIP", address: h.mgmt_ip}, {type: "Hostname", address: h.hostname}]
  }
});

const podToK8s = c => ({
  apiVersion: "v1", kind: "Pod",
  metadata: {name: c.name, namespace: KNSx(), uid: c.name,
    labels: {"app.kubernetes.io/component": c.group.replace(/\s+/g, "-"),
      "app.kubernetes.io/part-of": "simplyblock",
      [window.GROUP + "/cluster"]: cName(c.cluster_id)}},
  spec: {containers: [{name: c.name, image: c.image,
    resources: {requests: {cpu: String(c.cpu_cores_alloc), memory: Math.round(c.mem_limit / 1e9) + "Gi"},
      limits: {cpu: String(c.cpu_cores_alloc), memory: Math.round(c.mem_limit / 1e9) + "Gi"}}}]},
  status: {phase: c.state === "running" ? "Running" : "Succeeded",
    containerStatuses: [{name: c.name, ready: c.state === "running", restartCount: c.restarts}],
    // usage the metrics API would serve; carried here so one read answers the panel
    usage: {cpu: c.cpu_pct / 100, memory: String(c.mem_used), disk: String(c.disk_used),
      diskLimit: String(c.disk_limit)}}
});
const KNSx = () => window.SB_CONFIG.namespace || "simplyblock";

const cpToK8s = c => ({
  apiVersion: window.API_GROUP, kind: "ControlPlane",
  metadata: {name: d2(c.name) + "-cp", namespace: KNSx(), uid: c.uuid + "-cp",
    labels: {[window.GROUP + "/cluster"]: d2(c.name)}},
  spec: {clusterRef: {name: d2(c.name)}, version: c.cluster_version},
  status: {
    phase: c.status === "suspended" ? "Suspended" : "Ready",
    observedGeneration: 1,
    activeOpsRef: null,
    endpoint: c.mgmt_endpoint,
    stateDatabase: {backend: "FoundationDB",
      backups: (DB2().fdb_backups || []).filter(b => b.cluster_id === c.uuid).length},
    conditions: [window.__cond ? window.__cond() : {type: "Ready", status: "True", reason: "Ready"}]
  }
});

const driverToK8s = k => ({
  apiVersion: window.API_GROUP, kind: "SimplyblockDriver",
  metadata: {name: d2(k.name) + "-driver", namespace: KNSx(), uid: k.uuid + "-drv",
    labels: {[window.GROUP + "/kubernetes-cluster"]: d2(k.name)}},
  spec: {version: k.csi_version, namespace: k.operator_namespace},
  status: {
    phase: k.csi_status === "online" ? "Ready" : "Degraded",
    observedGeneration: 1,
    driverVersion: k.csi_version,
    operatorVersion: k.csi_version,
    controlPlaneVersions: [...new Set((k.storage_cluster_ids || [])
      .map(id => (DB2().clusters.find(c => c.uuid === id) || {}).cluster_version).filter(Boolean))],
    conditions: [{type: "Ready", status: k.csi_status === "online" ? "True" : "False",
      reason: k.csi_status === "online" ? "Ready" : "DriverDegraded"}]
  }
});

const cdcToK8s = c => ({
  apiVersion: window.API_GROUP, kind: "ClusterDeploymentConfig",
  metadata: {name: c.name, namespace: KNSx(), uid: c.uuid, creationTimestamp: c.created_at},
  spec: c.spec,
  status: c.status || {phase: "Draft", observedGeneration: 1}
});

// ---- the interceptor -------------------------------------------------------
const passThrough = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfg = window.SB_CONFIG;
  if (!cfg.mock) return passThrough(input, init);
  const url = typeof input === "string" ? input : input.url;
  const isK8s = url.startsWith(cfg.k8sBase);
  const isOp = url.startsWith(cfg.operatorBase);
  const isHelm = url.startsWith(cfg.helmBase);
  if (!isK8s && !isOp && !isHelm) return passThrough(input, init);

  const method = ((init && init.method) || "GET").toUpperCase();
  const base = isK8s ? cfg.k8sBase : isOp ? cfg.operatorBase : cfg.helmBase;
  const [rawPath, rawQuery] = url.slice(base.length).split("?");
  const params = new URLSearchParams(rawQuery || "");
  let body = {};
  try { if (init && init.body) body = JSON.parse(init.body); } catch (e) {}

  SBM.requests++;
  await new Promise(r => setTimeout(r, SBM.latency[0] + Math.random() * (SBM.latency[1] - SBM.latency[0])));
  if (SBM.offline) return status4(503, "ServiceUnavailable", "Cannot reach the Kubernetes API");
  if (SBM.failNext) { SBM.failNext = false; return status4(503, "ServiceUnavailable", "Injected failure"); }
  if (SBM.failRate && Math.random() < SBM.failRate) return status4(503, "ServiceUnavailable", "API server unavailable");

  if (window.__tickErr) return status4(500, "InternalError", "fixture tick failed: " + window.__tickErr);

  if (isHelm) return jsonRes(helmMock(rawPath));
  if (isOp) return operatorMock(rawPath, method, body, params);

  // /apis/<group>/<version>/namespaces/<ns>/<plural>[/<name>[/<sub>]]
  // /api/v1[/namespaces/<ns>]/<plural>[/<name>[/<sub>]]
  const parts = rawPath.split("/").filter(Boolean);
  const nsIdx = parts.indexOf("namespaces");
  const tail = nsIdx >= 0 ? parts.slice(nsIdx + 2) : parts.slice(parts[0] === "api" ? 2 : 3);
  const plural = tail[0], name = tail[1], sub = tail[2];
  const kind = PLURALS[plural];
  if (!kind) return status4(404, "NotFound", `the server could not find the resource: ${plural}`);

  if (sub === "log") {
    const lines = (DB2().container_logs[Object.keys(DB2().container_logs)
      .find(k => k.endsWith("/" + name)) || ""] || []);
    return new Response(lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n"),
      {status: 200, headers: {"Content-Type": "text/plain"}});
  }

  if (method === "GET") {
    if (nsIdx >= 0) params.set("__ns", parts[nsIdx + 1]);
    if (kind === "PersistentVolumeClaim" && nsIdx >= 0) { const ns = parts[nsIdx + 1]; const its = readList(kind, params).filter(o => o.metadata.namespace === ns); return jsonRes(listOf(kind, its)); }
    const items = readList(kind, params);
    if (!name) return jsonRes(listOf(kind, items));
    const hit = items.find(o => o.metadata.name === name);
    return hit ? jsonRes(hit)
      : status4(404, "NotFound", `${plural} "${name}" not found`);
  }

  if (method === "POST") {
    // kinds with their own create rules (replication) rather than the Ops shape
    const mk = (window.CRD_CREATE || {})[kind];
    if (mk) {
      try {
        const r = mk(body);
        if (r.err) return status4(r.reason === "Conflict" ? 409 : r.reason === "NotFound" ? 404
          : r.reason === "AlreadyExists" ? 409 : 422, r.reason, r.err);
        return jsonRes(r.obj, 201);
      } catch (e) { return status4(500, "InternalError", e.message); }
    }
    if (!window.RESOURCES[kind].ops && kind !== "OperatorOps")
      return status4(405, "MethodNotAllowed", `${kind} is not created through this console`);
    try {
      const r = window.runOps(kind, body);
      if (r.err) return status4(r.reason === "Conflict" ? 409 : 422, r.reason, r.err);
      return jsonRes(window.opsToK8s(r.rec), 201);
    } catch (e) { return status4(500, "InternalError", e.message); }
  }

  if (method === "PATCH") {
    // The only membership control replication has: the PVC annotation. Writing
    // it attaches, clearing it detaches; the operator owns the slot either way.
    if (kind === "PersistentVolumeClaim") {
      const anns = ((body.metadata || {}).annotations) || {};
      const key = window.SB_REPL.REPL_ANN;
      if (!Object.prototype.hasOwnProperty.call(anns, key))
        return status4(422, "Invalid", `this console only patches the ${key} annotation on a PVC`);
      const r = window.SB_REPL.setPvcPolicy(name, anns[key]);
      if (r.err) return status4(r.reason === "Conflict" ? 409 : r.reason === "NotFound" ? 404 : 422, r.reason, r.err);
      return jsonRes(r.obj);
    }
    const rec = (DB2().ops || []).find(o => o.kind === kind && o.name === name);
    if (!rec) return status4(404, "NotFound", `${plural} "${name}" not found`);
    // spec.abort is the one mutable field on an Ops spec
    const keys = Object.keys((body.spec) || {});
    if (keys.some(k => k !== "abort"))
      return status4(422, "Invalid", `spec is immutable except for spec.abort; refused: ${keys.filter(k => k !== "abort").join(", ")}`);
    if (["Succeeded", "Failed", "Aborted"].includes(rec.phase))
      return status4(409, "Conflict", `operation is already ${rec.phase}`);
    rec.abort = !!body.spec.abort;
    return jsonRes(window.opsToK8s(rec));
  }

  if (method === "DELETE") {
    // the operator enforces the deletion order: a pair cannot go while a policy
    // references it, a policy cannot go while a slot references it
    const rm = (window.CRD_DELETE || {})[kind];
    if (rm) {
      const r = rm(name);
      if (r.err) return status4(r.reason === "Conflict" ? 409 : r.reason === "NotFound" ? 404 : 422, r.reason, r.err);
      return jsonRes(r.obj);
    }
    const rec = (DB2().ops || []).find(o => o.kind === kind && o.name === name);
    if (!rec) return status4(404, "NotFound", `${plural} "${name}" not found`);
    // The DELETE webhook: terminal is admitted, an abortable step is unwound,
    // a step with no abort edge is refused rather than stranding the entity.
    if (["Succeeded", "Failed", "Aborted"].includes(rec.phase)) {
      DB2().ops = DB2().ops.filter(o => o !== rec);
      return jsonRes({kind: "Status", status: "Success"});
    }
    const abortable = (window.OPS_ABORTABLE[rec.action] || []).includes(rec.step);
    if (!abortable)
      return status4(403, "Forbidden",
        `admission webhook denied the request: ${rec.action} cannot be aborted from step ${rec.step}; deleting now would strand ${rec.target_kind}/${rec.target_name}`);
    rec.abort = true;
    return jsonRes({kind: "Status", status: "Success",
      message: `abort requested; ${rec.action} will unwind from ${rec.step}`});
  }
  return status4(405, "MethodNotAllowed", method + " not supported");
};

// ---- operator API ----------------------------------------------------------
// /releases is the compatibility matrix. /proposed/* are the collections the UI
// needs that have no CRD in v1alpha1 yet — replication, DR, consistency groups,
// migrations, buckets, zones. They are namespaced under /proposed on purpose.
function operatorMock(path, method, body, params) {
  if (path === "/healthz") return jsonRes({status: "ok"});
  if (path === "/releases") return jsonRes(RELEASES);
  // access control: roles, bindings, and who-am-I. Its own handler because it
  // enforces authority (403) rather than just serving a collection.
  if (path.startsWith("/proposed/access") && window.SB_ACCESS_ROUTE) {
    const r = window.SB_ACCESS_ROUTE(method, path + (params.toString() ? "?" + params.toString() : ""), body);
    if (r) return r;
  }
  const m = path.match(/^\/proposed\/([\w-]+)(?:\/([\w-]+))?(?:\/([\w-]+))?$/);
  if (!m) return status4(404, "NotFound", "no operator route " + path);
  // mutations: the control-plane behaviour table (validation + state change)
  if (method !== "GET" && window.SB_CP_ROUTES) {
    const scope0 = params.get("scope"), scopeId0 = params.get("scopeId");
    const route = (scope0 && scopeId0 ? `/${scope0}/${scopeId0}${path.slice(9)}` : path.slice(9)).replace(/\/$/, "");
    for (const [mm, re, h] of window.SB_CP_ROUTES.MUT_ROUTES) {
      if (mm !== method) continue;
      const mt = route.match(re);
      if (!mt) continue;
      const r = h(mt, body);
      if (r.__404) return status4(404, "NotFound", "Resource not found");
      if (r.__err) return status4(409, "Conflict", r.__err);
      if (r.__cap) return status4(501, "NotImplemented", r.__cap);
      return jsonRes(Object.assign({proposed: true}, r.results !== undefined ? r : {results: [r]}));
    }
  }
  const coll = PROPOSED_COLL[m[1]];
  if (!coll) return status4(404, "NotFound", "no proposed collection " + m[1]);
  let items = DB2()[coll] || [];

  // /proposed/<coll>/<id>[/<verb>]
  if (m[2]) {
    if (m[3] || method !== "GET") return jsonRes({status: "ok", accepted: true});
    const hit = items.find(x => x.uuid === m[2] || x.id === m[2]);
    return hit ? jsonRes({results: [hit], proposed: true})
      : status4(404, "NotFound", `${m[1]}/${m[2]} not found`);
  }
  if (method !== "GET") return jsonRes({status: "ok", accepted: true});

  // ?scope=<parent collection>&scopeId=<uuid>
  const scope = params.get("scope"), scopeId = params.get("scopeId");
  if (scope && scopeId) {
    const field = SCOPE_FIELD_M[scope];
    // a policy or pair "belongs" to the cluster it replicates from
    const alt = scope === "clusters" && items.length && items.some(x => "source_cluster_id" in x) ? "source_cluster_id" : null;
    if (alt) {
      items = items.filter(x => x.source_cluster_id === scopeId);
    } else if (field && items.length && items.some(x => field in x)) {
      items = items.filter(x => x[field] === scopeId);
    } else {
      // membership: the parent names its children in an <child>_ids array.
      // Match the key to THIS child collection — a parent often has several.
      const pColl = PROPOSED_COLL[scope] || CORE_COLL[scope];
      const parent = ((DB2()[pColl] || []).find(x => x.uuid === scopeId)) || null;
      const singular = m[1].replace(/-/g, "_").replace(/s$/, "");
      const idsKey = parent && [IDS_ALIAS[m[1]], `${singular}_ids`, `${singular}s_ids`,
        `${m[1].replace(/-/g, "_")}_ids`].filter(Boolean).find(k => Array.isArray(parent[k]));
      items = parent && idsKey ? items.filter(x => (parent[idsKey] || []).includes(x.uuid)) : [];
    }
  }
  return jsonRes({results: SBM.forceEmpty ? [] : items, proposed: true});
}

// The kinds v1alpha1 does not model. Served here until they are CRDs.
const PROPOSED_COLL = {
  lvols: "lvols", snapshots: "snapshots", backups: "backups",
  "backup-policies": "backup_policies", "consistency-groups": "consistency_groups",
  "cg-snapshots": "cg_snapshots", "replication-policies": "dr_policies",
  "cluster-pairs": "cluster_pairs", pairs: "cluster_pairs",
  migrations: "migrations", buckets: "buckets", zones: "zones",
  "dr-clusters": "dr_clusters",
  "protected-apps": "protected_apps", alerts: "alerts", tasks: "tasks", logs: "logs",
  "migration-paths": "migration_paths", "app-groups": "app_groups",
  hosts: "hosts", "storage-classes": "storage_classes", pvcs: "pvcs",
  "kubernetes-clusters": "k8s_clusters", "k8s-clusters": "k8s_clusters",
  "fdb-backups": "fdb_backups", containers: "containers",
  "storage-clusters": "clusters", clusters: "clusters", pools: "pools",
  "storage-nodes": "storage_nodes", devices: "devices"
};
const CORE_COLL = {clusters: "clusters", pools: "pools", "storage-nodes": "storage_nodes",
  devices: "devices"};
// where the fixture's membership array is not the plural of the collection name
const IDS_ALIAS = {"kubernetes-clusters": "k8s_cluster_ids", "k8s-clusters": "k8s_cluster_ids",
  "storage-clusters": "storage_cluster_ids", clusters: "cluster_ids",
  "storage-nodes": "storage_node_ids", "storage-classes": "storage_class_ids"};
const SCOPE_FIELD_M = {
  clusters: "cluster_id", pools: "pool_id", lvols: "lvol_id",
  "storage-nodes": "node_id", hosts: "host_id", devices: "device_id",
  "backup-policies": "policy_id", "replication-policies": "policy_id",
  tasks: "parent_id", pairs: "pair_id",
  "kubernetes-clusters": "k8s_cluster_id", "k8s-clusters": "k8s_cluster_id",
  zones: "zone_id", "storage-classes": "storage_class_id",
  "consistency-groups": "cg_id", "dr-clusters": "dr_cluster_id", snapshots: "snapshot_id",
  "migration-paths": "path_id", "app-groups": "group_id"
};

// Mirrors install.simplyblock.io/releases.yaml
const RELEASES = {schema: 1, components: [
  {name: "controlplane", releases: [
    {version: "26.2.1", released: "2026-09-01"}, {version: "26.2", released: "2026-08-08"},
    {version: "26.1"}, {version: "0.2.0"}, {version: "0.1.0"}]},
  {name: "csi-driver", releases: [
    {version: "26.2.1", released: "2026-07-02", compatible: {controlplane: ["26.2.x", "26.1.x"]}},
    {version: "26.2.0", released: "2026-06-01", compatible: {controlplane: ["26.2.x", "26.1.x"]}},
    {version: "26.1.2", released: "2026-06-01", compatible: {controlplane: ["26.1.x", "0.2.0"]}},
    {version: "26.1.0", released: "2026-01-01", compatible: {controlplane: ["26.1.x", "0.2.0"]}}]},
  {name: "operator", releases: [
    {version: "26.2.1", released: "2026-07-02", compatible: {controlplane: ["26.2.x", "26.1.x"]}},
    {version: "26.2.0", released: "2026-06-01", compatible: {controlplane: ["26.2.x", "26.1.x"]}},
    {version: "26.1.2", released: "2026-06-01", compatible: {controlplane: ["26.1.x", "0.2.0"]}}]}
]};

function helmMock(path) {
  const rel = {name: "simplyblock", namespace: KNSx(), revision: 7,
    chart: "simplyblock-operator-26.2.1", appVersion: "26.2.1",
    status: "deployed", updated: U2().ago(72)};
  if (path === "/releases") return {releases: [rel]};
  if (path.endsWith("/values")) return {values: {
    operator: {logLevel: "info", replicas: 1},
    csi: {enableSnapshots: true, enableExpansion: true},
    controlPlane: {version: "26.2.1"}}};
  return rel;
}

if (!window.__fixtureClock) window.__fixtureClock = setInterval(() => {
  try { window.SB_JITTER(); window.tickOps(); window.__tickErr = null; }
  catch (e) { window.__tickErr = e.message; }
}, 1000);

Object.assign(window, {RELEASES, readList});
