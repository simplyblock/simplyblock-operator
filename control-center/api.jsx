// ---------------------------------------------------------------------------
// API CLIENT — the view models the UI renders, and how each is read
//
// Storage objects come from k8s-sources.jsx: records assembled from the CRDs,
// the core objects, and, where a context has a control plane, the v2 DTOs.
// The normalizers below turn those records into view models; `REG` keeps
// every view model by id so breadcrumbs and cross-links resolve without a
// fetch. Actions are Ops objects (k8s-client.jsx submitOps) or edits of the
// core object that owns the thing (a PVC for a volume, a VolumeSnapshot for a
// snapshot).
//
// What is left of the `/proposed` operator surface serves only the views no
// API exists for yet (zones, buckets, consistency groups, migration paths,
// the legacy DR layer, access roles). Each is behind a feature flag and
// answers "not part of this deployment" unless the flag is on, which it is
// in the design preview with the fixture backend.
// ---------------------------------------------------------------------------
const API = window.SB_CONFIG.k8sBase;

// first path segment of a /proposed collection -> the feature that gates it
const PROPOSED_FEATURE = {
  zones: "zones", buckets: "buckets", "consistency-groups": "cgroups", "cg-snapshots": "cgroups",
  "migration-paths": "migrationPaths", "app-groups": "migrationPaths", migrations: "migrationPaths",
  "access-roles": "accessRoles", "access-grants": "accessRoles", access: "accessRoles",
  "protection-plans": "drLegacy", "dr-sites": "drLegacy", "dr-clusters": "drLegacy", "protected-apps": "drLegacy",
  "dr-arbitration": "drLegacy", "replication-policies": "drLegacy", "cluster-pairs": "drLegacy"
};
const proposedFeature = path => {
  const seg = path.split("?")[0].split("/").filter(Boolean);
  return PROPOSED_FEATURE[seg[0]] || (seg[2] && PROPOSED_FEATURE[seg[2]]) || "proposed";
};
function guardProposed(path) {
  const f = proposedFeature(path);
  if (window.SB_CONFIG.mock || feature(f)) return;
  throw new ApiError(503, `${f} is not part of this deployment: no API serves it`, path, REASON_FEATURE_DISABLED);
}
// proposed: the fixture-only surface, gated per feature
async function preq(path) {
  guardProposed(path);
  let res;
  try {
    res = await fetch(window.SB_CONFIG.operatorBase + "/proposed" + path,
      {headers: {Accept: "application/json", Authorization: `Bearer ${window.SB_CONFIG.token}`}});
  } catch (e) { throw new ApiError(0, "Cannot reach the operator API", path, "Unreachable"); }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  if (!res.ok) throw new ApiError(res.status,
    (body && (body.message || body.error)) || "Request failed", path, (body && body.reason) || null);
  return body && body.results !== undefined ? body.results : body;
}

// req(path): a GET on a proposed collection, always answered as an array; any
// other method goes to mutate()
async function req(path, opts) {
  const method = ((opts || {}).method || "GET").toUpperCase();
  if (method !== "GET") return mutate(method, path, opts && opts.body ? JSON.parse(opts.body) : {});
  const r = await preq(path);
  return Array.isArray(r) ? r : (r === null || r === undefined ? [] : [r]);
}
const send = (method, path, payload) => req(path, {method, body: JSON.stringify(payload || {})});

// the context and namespace an entity lives in, from the registry
const whereIs = id => { const vm = REG[id] || {}; return {ctx: vm.ctx || LOCAL_CTX, namespace: vm.ns || undefined}; };
// after a write, the kind's collections in that context are stale
const touched = (ctx, kind) => r => { store.invalidate(ctx, kind); return r; };

async function mutate(method, path, payload) {
  // a proposed collection: fixture-only, gated
  guardProposed(path);
  const res = await fetch(window.SB_CONFIG.operatorBase + "/proposed" + path, {
    method, headers: {"Content-Type": "application/json",
      Authorization: `Bearer ${window.SB_CONFIG.token}`},
    body: method === "DELETE" ? undefined : JSON.stringify(payload || {})
  });
  let b = null; try { b = await res.json(); } catch (e) {}
  if (!res.ok) throw new ApiError(res.status, (b && (b.message || b.error)) || "Request failed", path, (b && b.reason) || null);
  return b && b.results !== undefined ? b.results : b;
}

// ---- status vocabulary -----------------------------------------------------
const STATUS_META = {
  online: {c: "var(--ok)", rank: 0, label: "online"},
  active: {c: "var(--ok)", rank: 0, label: "active"},
  paired: {c: "var(--ok)", rank: 0, label: "paired"},
  pairing: {c: "var(--info)", rank: 1, label: "pairing", blink: true},
  healthy: {c: "var(--ok)", rank: 0, label: "healthy"},
  unhealthy: {c: "var(--bad)", rank: 4, label: "unhealthy"},
  available: {c: "var(--ok)", rank: 0, label: "available"},
  enabled: {c: "var(--ok)", rank: 0, label: "enabled"},
  disabled: {c: "var(--warn)", rank: 3, label: "disabled"},
  discovered: {c: "var(--dim2)", rank: 2, label: "discovered"},
  inspecting: {c: "var(--info)", rank: 1, label: "inspecting", blink: true},
  inspected: {c: "var(--accent)", rank: 3, label: "ready to configure"},
  degraded: {c: "var(--warn)", rank: 3, label: "degraded"},
  suspended: {c: "var(--bad)", rank: 4, label: "suspended"},
  unready: {c: "var(--idle)", rank: 2, label: "unready"},
  in_activation: {c: "var(--info)", rank: 1, label: "in activation", blink: true},
  offline: {c: "var(--bad)", rank: 4, label: "offline"},
  unreachable: {c: "var(--alert)", rank: 5, label: "unreachable"},
  down: {c: "var(--bad)", rank: 4, label: "down"},
  in_restart: {c: "var(--info)", rank: 1, label: "in restart", blink: true},
  in_removal: {c: "var(--alert)", rank: 2, label: "in removal", blink: true},
  in_shutdown: {c: "var(--warn)", rank: 3, label: "in shutdown", blink: true},
  in_creation: {c: "var(--info)", rank: 1, label: "in creation", blink: true},
  in_migration: {c: "var(--info)", rank: 1, label: "in migration", blink: true},
  // ReplicationSlot.status.state — a slot that is replicating is in its steady
  // state, so it reads green rather than as a transient.
  replicating: {c: "var(--ok)", rank: 0, label: "replicating"},
  converging: {c: "var(--info)", rank: 1, label: "converging", blink: true},
  cutover_pending: {c: "var(--warn)", rank: 5, label: "ready to cut over"},
  cutover_done: {c: "var(--info)", rank: 1, label: "cutover done"},
  attaching: {c: "var(--idle)", rank: 4, label: "attaching"},
  detaching: {c: "var(--idle)", rank: 4, label: "detaching"},
  failed_over: {c: "var(--ro)", rank: 6, label: "failed over"},
  error: {c: "var(--bad)", rank: 9, label: "error"},
  frozen: {c: "var(--alert)", rank: 3, label: "io frozen", blink: true},
  running: {c: "var(--info)", rank: 1, label: "running", blink: true},
  paused: {c: "var(--warn)", rank: 3, label: "paused"},
  completed: {c: "var(--ok)", rank: 0, label: "completed"},
  Bound: {c: "var(--ok)", rank: 0, label: "Bound"},
  Running: {c: "var(--info)", rank: 3, label: "running", blink: true},
  Succeeded: {c: "var(--ok)", rank: 0, label: "succeeded"},
  Pending: {c: "var(--warn)", rank: 3, label: "Pending"},
  Lost: {c: "var(--bad)", rank: 4, label: "Lost"},
  Draft: {c: "var(--dim2)", rank: 2, label: "draft — not approved"},
  Approved: {c: "var(--accent)", rank: 1, label: "approved"},
  Deploying: {c: "var(--info)", rank: 1, label: "deploying", blink: true},
  undiscovered: {c: "var(--dim2)", rank: 3, label: "not discovered"},
  Queued: {c: "var(--idle)", rank: 2, label: "queued"}, RestoringBackups: {c: "var(--info)", rank: 1, label: "restoring backups", blink: true}, PromotingVolumes: {c: "var(--info)", rank: 1, label: "promoting volumes", blink: true}, Replicating: {c: "var(--info)", rank: 1, label: "replicating", blink: true},
  Converged: {c: "var(--accent)", rank: 1, label: "converged"}, MovingWorkloads: {c: "var(--info)", rank: 1, label: "moving workloads", blink: true},
  MigratingVolumes: {c: "var(--info)", rank: 1, label: "migrating volumes", blink: true}, Cleanup: {c: "var(--info)", rank: 1, label: "cleanup", blink: true},
  Completed: {c: "var(--ok)", rank: 0, label: "completed"}, Paused: {c: "var(--warn)", rank: 3, label: "paused"},
  LiveMigrating: {c: "var(--info)", rank: 1, label: "live migrating", blink: true}, Restarting: {c: "var(--info)", rank: 1, label: "restarting", blink: true}, Moved: {c: "var(--ok)", rank: 0, label: "moved"},
  Configured: {c: "var(--ok)", rank: 0, label: "configured"}, Added: {c: "var(--ok)", rank: 0, label: "added"},
  Applying: {c: "var(--info)", rank: 1, label: "applying", blink: true}, Isolating: {c: "var(--info)", rank: 1, label: "isolating cores", blink: true},
  Rebooting: {c: "var(--warn)", rank: 1, label: "rebooting", blink: true}, Verifying: {c: "var(--info)", rank: 1, label: "verifying", blink: true},
  Scheduling: {c: "var(--info)", rank: 1, label: "scheduling pod", blink: true}, Joining: {c: "var(--info)", rank: 1, label: "joining cluster", blink: true},
  "Starting SPDK": {c: "var(--info)", rank: 1, label: "starting SPDK", blink: true},
  Failed: {c: "var(--bad)", rank: 4, label: "failed"},
  complete: {c: "var(--ok)", rank: 0, label: "complete"},
  Validated: {c: "var(--ok)", rank: 0, label: "Validated"},
  Validating: {c: "var(--info)", rank: 1, label: "Validating", blink: true},
  Deployed: {c: "var(--ok)", rank: 0, label: "Deployed"},
  FailedOver: {c: "var(--warn)", rank: 3, label: "Failed over"},
  FailingOver: {c: "var(--alert)", rank: 3, label: "Failing over", blink: true},
  Relocating: {c: "var(--info)", rank: 2, label: "Relocating", blink: true},
  Relocated: {c: "var(--ok)", rank: 0, label: "Relocated"},
  WaitForUser: {c: "var(--bad)", rank: 4, label: "Waiting for operator"},
  Fenced: {c: "var(--bad)", rank: 4, label: "Fenced"},
  ManuallyFenced: {c: "var(--bad)", rank: 4, label: "Manually fenced"},
  Unfenced: {c: "var(--ok)", rank: 0, label: "Unfenced"},
  failed: {c: "var(--bad)", rank: 4, label: "failed"},
  unavailable: {c: "var(--bad)", rank: 4, label: "unavailable"},
  new: {c: "var(--idle)", rank: 2, label: "new"},
  removed: {c: "var(--dim2)", rank: 3, label: "removed"},
  in_removal: {c: "var(--alert)", rank: 2, label: "in removal", blink: true},
  in_shutdown: {c: "var(--warn)", rank: 3, label: "in shutdown", blink: true},
  in_failure: {c: "var(--bad)", rank: 3, label: "failing", blink: true},
  read_only: {c: "var(--ro)", rank: 3, label: "read-only"},
  in_sync: {c: "var(--ok)", rank: 0, label: "in sync"},
  catching_up: {c: "var(--info)", rank: 1, label: "catching up", blink: true},
  paused: {c: "var(--warn)", rank: 3, label: "paused"},
  broken: {c: "var(--bad)", rank: 5, label: "broken"},
  good: {c: "var(--ok)", rank: 0, label: "healthy"},
  warn: {c: "var(--warn)", rank: 3, label: "warning"},
  critical: {c: "var(--bad)", rank: 4, label: "critical"}
};

// ---- normalizers: wire record -> view model --------------------------------
const REG = {};                       // uuid -> view model, warms breadcrumbs
const reg = vm => { REG[vm.id] = vm; return vm; };
const ioOf = s => ({r: (s || {}).read_io_ps || 0, w: (s || {}).write_io_ps || 0});
const bwOf = s => ({r: (s || {}).read_bytes_ps || 0, w: (s || {}).write_bytes_ps || 0});
const histOf = h => ({iops: (h || {}).iops || [], bw: (h || {}).bytes || []});

const normCluster = c => reg({
  kind: "cluster", id: c.uuid, name: c.name,
  ctx: c.ctx || LOCAL_CTX, ns: c.ns || null, k8s: c.__k8s || null, labels: c.labels || {},
  sites: c.sites || [], stage: c.stage || null, tenant: c.tenant || null, stretched: !!c.stretched,
  phase: c.phase || null, subPhase: c.sub_phase || null, configured: c.configured !== false,
  activeOpsRef: c.active_ops_ref || null, activeOps: c.active_ops || [],
  thresholds: c.thresholds || null, capacityApprox: !!c.capacity_approx, provisioned: c.size_prov || null,
  workerNames: c.worker_names || [], nodeUuids: c.node_uuids || [], nqn: c.nqn || null, live: c.live || null,
  mode: c.device_class === "nvme" ? "nvme" : "blockdev",
  siting: c.location_type === "edge" ? "edge" : "datacenter",
  status: c.status, rebalancing: !!c.rebalancing,
  capacity: {total: c.size_total, used: c.size_util},
  iops: ioOf(c.io_stats), bw: bwOf(c.io_stats), hist: histOf(c.io_history),
  counts: {hosts: c.hosts_count, hostsAvailable: c.hosts_available, nodes: c.storage_nodes_count,
    nodesOnline: c.storage_nodes_online, devices: c.devices_count, devicesOnline: c.devices_online,
    pools: c.pools_count, volumes: c.lvols_count, snapshots: c.snapshots_count,
    backups: c.backups_count, policies: c.backup_policies_count,
    cgroups: c.consistency_groups_count || 0, encrypted: c.encrypted_lvols_count || 0,
    migrations: c.migrations_count || 0, migrationTargets: c.migration_targets_count || 0,
    buckets: c.buckets_count || 0, rwxPvcs: c.rwx_pvcs_count || 0,
    k8sClusters: (c.k8s_cluster_ids || []).length, pvcs: c.pvcs_count || 0,
    reduced: c.reduced_lvols_count || 0,
    rpolicies: c.replication_policies_count || 0, pairsOut: c.pairs_out_count || 0,
    pairsIn: c.pairs_in_count || 0, replicated: c.replicated_lvols_count || 0,
    zones: (c.zone_ids || []).length},
  zoneIds: c.zone_ids || [], regions: c.regions || [], stretched: !!c.stretched, drEligible: !!c.dr_target_eligible,
  k8sClusterIds: c.k8s_cluster_ids || [],
  backupEnabled: !!c.backup_enabled, s3: c.s3 || null,
  syncReplication: !!c.sync_replication_enabled,
  kms: c.kms || null, logicalUsed: c.logical_used || 0,
  fileStorage: c.file_storage || {enabled: false},
  objectStorage: c.object_storage || {enabled: false},
  multipathing: !!c.multipathing_enabled, multipathNodes: c.multipath_nodes_count || 0,
  autoRebalance: {enabled: !!(c.auto_rebalance || {}).enabled,
    moved1h: (c.auto_rebalance || {}).moved_1h || 0, moved24h: (c.auto_rebalance || {}).moved_24h || 0},
  nodeAffinity: c.node_affinity || "none", podAffinity: !!c.pod_affinity_enabled,

  fd: {enabled: !!c.failure_domain_enabled, scope: c.failure_domain_scope || null,
    domains: c.failure_domains || [], unassigned: c.fd_unassigned || 0,
    min: c.fd_min || 0, max: c.fd_max || 0, balanced: c.fd_balanced !== false, thin: c.fd_thin || []},
  caps: c.capabilities || {},
  faultBudget: c.fault_budget || {kind: "nodes", tolerated: c.distr_npcs || 1, lost: (c.storage_nodes_count || 0) - (c.storage_nodes_online || 0)},
  haType: c.ha_type, distrNpcs: c.distr_npcs, distrNdcs: c.distr_ndcs, version: c.cluster_version, mgmt: c.mgmt_endpoint,
  mgmtKind: c.mgmt_endpoint_kind || "control plane API", createdAt: c.created_at
});
const normHost = h => reg({
  kind: "host", id: h.uuid, clusterId: h.cluster_id, hostname: h.hostname, mgmtIp: h.mgmt_ip,
  ctx: h.ctx || LOCAL_CTX, ns: null, k8s: h.__k8s || null, clusterIds: h.cluster_ids || [],
  kubeletVersion: h.kubelet_version || null, osImage: h.os_image || null,
  zoneId: h.zone_id || null, zone: h.zone || null, region: h.region || null,
  rack: h.rack_id || null, cabinet: h.cabinet_id || null, k8sCluster: h.k8s_cluster || null,
  migrationTaint: h.migration_taint || null,
  hostClass: h.host_class || null,
  status: h.status, source: h.source || "manual", sockets: h.numa_sockets, controlPlane: !!h.control_plane,
  kubelet: h.kubelet_version, roles: h.roles || [], k8sLabels: h.k8s_labels || {},
  inspection: h.inspection || null,
  vcpu: h.vcpu_count, memory: h.memory_total,
  memoryPerPod: h.memory_per_pod || null, socketsUsed: h.numa_sockets_used || null,
  mgmtNic: h.mgmt_nic || null, dataNics: h.data_nics || [],
  nics: (h.nics || []).map(n => ({name: n.name, mac: n.mac, speed: n.speed_gbps, address: n.address, socket: n.numa_socket, state: n.state})),
  hugepages: {reserved: h.hugepages_reserved, allocated: h.hugepages_allocated},
  devices: (h.devices || []).map(d => ({id: d.id, kind: d.kind, socket: d.numa_socket, pcie: d.pcie_address,
    blockdev: d.device_name, serial: d.serial_number, model: d.model_number, size: d.size,
    assignedNodeId: d.assigned_node_id, reserved: !!d.reserved, reservedFor: d.reserved_for_node_id})),
  nodeIds: h.storage_node_ids || [],
  counts: {devices: (h.devices || []).length, assigned: h.devices_assigned, free: h.devices_free,
    nvme: h.nvme_count, blockFree: h.block_free_count, nodes: (h.storage_node_ids || []).length},
  capacity: {total: h.size_total, used: h.size_assigned},
  preparedAt: h.prepared_at, labels: h.labels || {}
});
// A discovery run: the inventory pass, and the filter it ran with. The filter
// belongs here rather than in the wizard because it decides what was even
// reported — a boot device excluded at discovery never reaches a node set.
const normDiscovery = d => ({
  kind: "discovery", id: d.uuid, k8sClusterId: d.k8s_cluster_id,
  status: d.status, step: d.step || null, opName: d.op_name || null,
  startedAt: d.started_at, finishedAt: d.finished_at,
  nodeSelector: (d.node_selector || {}).matchLabels || {},
  filter: d.device_filter || {},
  hostIds: d.host_ids || [],
  counts: {nodes: d.node_count, devices: d.device_count, filtered: d.filtered_count}
});

// ClusterDeploymentConfig — the document that is reviewed and approved. One
// group per NUMA socket, because one storage node runs per socket.
const normDeployConfig = o => {
  const sp = o.spec || {}, st = o.status || {};
  const set = (sp.nodeSets || [])[0] || {};
  const groups = (set.groups || []).map(g => ({
    node: g.node, socket: g.numaSocket,
    nvme: (g.devices || {}).nvme || [], block: (g.devices || {}).block || [],
    mgmtNic: g.mgmtInterface, dataNics: g.dataInterfaces || [],
    maxSubsystems: (g.sizing || {}).maxSubsystemCount,
    vcpu: (g.sizing || {}).vcpuCount,
    hugepages: (g.sizing || {}).minHugePagesSize,
    systemMemory: (g.sizing || {}).systemMemory,
    coreIsolation: !!g.coreIsolation, failureDomain: g.failureDomain
  }));
  const steps = (st.steps || []).map(s => ({
    name: s.name, label: s.label, phase: s.phase, message: s.message,
    startedAt: s.startedAt, finishedAt: s.finishedAt, progress: s.progress
  }));
  const cl = sp.cluster || {};
  return reg({
    kind: "deployconfig", id: o.metadata.uid, name: o.metadata.name,
    ctx: o.__ctx || LOCAL_CTX, ns: o.metadata.namespace, k8s: {name: o.metadata.name, namespace: o.metadata.namespace, kind: o.kind, uid: o.metadata.uid},
    createdAt: o.metadata.creationTimestamp,
    approved: !!sp.approved, status: st.phase || "Draft", message: st.message || null,
    environment: sp.environment || null,
    k8sClusterId: o.__ctx || st.kubernetesClusterId || LOCAL_CTX, k8sClusterName: sp.kubernetesClusterRef || o.__ctx || null,
    clusterId: st.clusterId || null, discoveryId: st.discoveryRef || null,
    hostIds: st.hostIds || [],
    nodeSelector: (sp.nodeSelector || {}).matchLabels || {},
    filter: cl.deviceFilter || sp.deviceFilter || {},
    cluster: sp.cluster || null, hosts: set.nodes || [], groups, steps,
    nodes: (st.nodes || []).map(n => ({host: n.host, hostId: n.hostId, phase: n.phase, message: n.message, progress: n.progress || 0, storageNodeIds: n.storageNodeIds || []})),
    log: (st.log || []).map(l => ({ts: l.ts, level: l.level, step: l.step, node: l.node, msg: l.msg})),
    counts: {hosts: (set.nodes || []).length, nodes: groups.length,
      devices: groups.reduce((n, g) => n + g.nvme.length + g.block.length, 0)},
    sizing: {hugepages: groups.reduce((n, g) => n + (g.hugepages || 0), 0),
      vcpu: groups.reduce((n, g) => n + (g.vcpu || 0), 0),
      coreIsolation: groups.some(g => g.coreIsolation),
      maxSubsystems: groups.length ? groups[0].maxSubsystems : null}
  });
};
const normNode = n => reg({
  kind: "node", id: n.uuid, clusterId: n.cluster_id, hostId: n.host_id, zoneId: n.zone_id || null, hostname: n.hostname,
  ctx: n.ctx || LOCAL_CTX, ns: n.ns || null, k8s: n.__k8s || null, name: n.name || null, clusterName: n.cluster_name || null,
  worker: n.worker_node || null, setName: n.set_name || null, socket: n.socket, nodeIndex: n.node_index,
  zone: n.zone || null, rpcPort: n.rpc_port || null, nvmfPort: n.nvmf_port || null, lvolPort: n.lvol_port || null,
  activeOpsRef: n.active_ops_ref || null, health: n.health_check, uptime: n.uptime || null, latency: n.latency || null,
  postedAt: n.posted_at || null, live: n.live || null,
  ip: (n.data_nics && n.data_nics[0] ? n.data_nics[0].ip : null), port: (n.data_nics && n.data_nics[0] ? n.data_nics[0].port : 4420),
  dataNics: (n.data_nics || []).map(x => ({name: x.name, ip: x.ip, port: x.port, socket: x.numa_socket, state: x.state})),
  multipath: (n.data_nics || []).length > 1,
  op: n.op ? {kind: n.op.kind, phase: n.op.phase, phaseIndex: n.op.phase_index || 0, phases: n.op.phases || [],
    volumesMoved: n.op.volumes_moved || 0, targetHostname: n.op.target_hostname || null, sourceHostname: n.op.source_hostname || null,
    startedAt: n.op.started_at, taskId: n.op.task_id} : null,
  mgmtIp: n.mgmt_ip, failureDomain: n.failure_domain, physicalLabel: n.physical_label, status: n.status,
  capacity: {total: n.size_total, used: n.size_util},
  iops: ioOf(n.io_stats), bw: bwOf(n.io_stats), hist: histOf(n.io_history),
  counts: {devices: n.devices_count, devicesOnline: n.devices_online},
  cpuCount: n.cpu_count, cpuReserved: n.vcpu_reserved, maxSubsystems: n.max_subsystem_count || null,
  memory: {total: n.memory_total, reserved: n.memory_reserved, used: n.memory_used},
  hugepages: {total: n.hugepages_total, used: n.hugepages_used},
  spdk: n.spdk_version
});
const normDevice = d => reg({
  kind: "device", id: d.uuid, nodeId: d.node_id, clusterId: d.cluster_id, hostId: d.host_id,
  ctx: d.ctx || LOCAL_CTX, ns: d.ns || null, k8s: d.__k8s || null, name: d.name || null, nodeName: d.node_name || null,
  role: d.role || null, phase: d.phase || null, message: d.message || null, ioError: d.io_error, retriesExhausted: d.retries_exhausted,
  live: d.live || null,
  mode: d.cluster_device_class === "nvme" ? "nvme" : "blockdev", socket: d.numa_socket,
  serial: d.serial_number, pcie: d.pcie_address, blockdev: d.device_name,
  model: d.model_number, firmware: d.firmware_revision,
  status: d.status, health: d.health_check, lastHealthCheck: d.last_health_check,
  capacity: {total: d.size_total, used: d.size_util},
  iops: ioOf(d.io_stats), bw: bwOf(d.io_stats), hist: histOf(d.io_history),
  temp: d.temperature_c, wear: d.percentage_used, poweronHours: d.power_on_hours
});
const normPool = p => reg({
  kind: "pool", id: p.uuid, clusterId: p.cluster_id, name: p.pool_name, dhchap: !!p.dhchap_bidirectional,
  ctx: p.ctx || LOCAL_CTX, ns: p.ns || null, k8s: p.__k8s || null, clusterName: p.cluster_name || null,
  allowedNodes: p.allowed_nodes || [], storageClassParameters: p.storage_class_parameters || {},
  lvolMaxSize: p.lvol_max_size || null, live: p.live || null,
  // a pool is never offline: it is enabled or disabled. Disabled keeps serving I/O
  // but refuses new volume provisioning.
  status: p.enabled === false ? "disabled" : "enabled", enabled: p.enabled !== false, qos: p.qos,
  capacity: {total: p.size_prov, used: p.size_util},
  lvolBytes: p.lvols_bytes || 0, snapshotBytes: p.snapshots_bytes || 0,
  storageClasses: p.storage_classes || [], k8sClusterId: p.k8s_cluster_id || null,
  counts: {volumes: p.lvols_count, volumesOnline: p.lvols_online, snapshots: p.snapshots_count,
    backups: p.backups_count, storageClasses: p.storage_classes_count || 0}
});
const normVolume = v => reg({
  kind: "volume", id: v.uuid, poolId: v.pool_id, poolName: v.pool_name, clusterId: v.cluster_id,
  ctx: v.ctx || LOCAL_CTX, ns: v.ns || null, k8s: v.__k8s || null, pvName: v.pv_name || null, handle: v.handle || null,
  clusterName: v.cluster_name || null, storageClass: v.storage_class || null, storageClassId: v.storage_class_id || null,
  fsType: v.fs_type || null, replicationPolicy: v.replication_policy || null, migrating: !!v.migrating, live: v.live || null,
  name: v.lvol_name, status: v.status, nodes: v.nodes || {},
  capacity: {total: v.size_prov, used: v.size_util},
  iops: ioOf(v.io_stats), bw: bwOf(v.io_stats), hist: histOf(v.io_history),
  crypto: !!v.crypto_enabled, qos: v.qos, nqn: v.nqn,
  dataReduction: !!v.compression_dedup_enabled,
  logicalUsed: v.logical_used || v.size_util,
  baseSnapshot: v.base_snapshot || null,
  // a volume can belong to several consistency groups at once
  consistencyGroups: v.consistency_groups || [],
  pvc: v.pvc || null, bucket: v.bucket || null,
  affinity: v.affinity || null,
  backupPolicy: v.backup_policy || null,
  replication: v.replication ? {
    policyId: v.replication.policy_id, policyName: v.replication.policy_name, mode: v.replication.mode,
    status: v.replication.status, lastAt: v.replication.last_replication_at,
    backlog: v.replication.backlog_bytes, consistencyGroup: v.replication.consistency_group,
    generations: v.replication.generations
  } : null,
  migration: v.migration || null,
  counts: {snapshots: v.snapshots_count || 0, backups: v.backups_count || 0,
    snapshotsBackedUp: v.snapshots_backed_up || 0, backupVersions: v.backup_versions_count || 0},
  backupChainId: v.backup_chain_id || null,
  createdAt: v.created_at
});
const normSnapshot = s => reg({
  kind: "snapshot", id: s.uuid, clusterId: s.cluster_id, poolId: s.pool_id, poolName: s.pool_name,
  ctx: s.ctx || LOCAL_CTX, ns: s.ns || null, k8s: s.__k8s || null, pvcName: s.pvc_name || null,
  contentName: s.content_name || null, handle: s.handle || null, snapshotClass: s.snapshot_class || null, error: s.error || null,
  volumeId: s.lvol_id, volumeName: s.lvol_name, name: s.snapshot_name, status: s.status || "online",
  seq: s.seq, parentId: s.parent_id || null, backupVersionId: s.backup_version_id || null,
  createdAt: s.created_at, capacity: {total: s.size, used: s.size}
});
const normBackup = b => reg({
  kind: "backup", id: b.uuid, clusterId: b.cluster_id, poolId: b.pool_id, poolName: b.pool_name,
  ctx: b.ctx || LOCAL_CTX, ns: b.ns || null, k8s: b.__k8s || null, crName: b.name || null, phase: b.phase || null,
  message: b.message || null, pvc: b.pvc || null, prevBackupId: b.prev_backup_id || null, clusterName: b.cluster_name || null,
  volumeId: b.lvol_id, volumeName: b.lvol_name, name: b.lvol_name, chainId: b.chain_id,
  policyId: b.policy_id || null, policyName: b.policy_name || null,
  status: b.status || "online", bucket: b.bucket,
  exportedTo: b.exported_to || null, exportedVersion: b.exported_version || null,
  createdAt: b.earliest_at || b.created_at, latestAt: b.latest_at, lastMergeAt: b.last_merge_at || null,
  versions: (b.versions || []).map(v => ({
    id: v.id, seq: v.seq, tier: v.tier, type: v.type, createdAt: v.created_at, size: v.size,
    snapshotId: v.source_snapshot_id, snapshotName: v.source_snapshot_name, merged: v.merged_count || 0
  })),
  counts: {versions: b.versions_count || 0, merged: b.merged_total || 0},
  fullBytes: b.full_bytes || 0, deltaBytes: b.delta_bytes || 0,
  capacity: {total: b.size || 0, used: b.size || 0}
});
const normPolicy = p => reg({
  kind: "policy", id: p.uuid, clusterId: p.cluster_id, name: p.policy_name, status: p.status || "active", consistencyGroup: !!p.consistency_group,
  ctx: p.ctx || LOCAL_CTX, ns: p.ns || null, k8s: p.__k8s || null, clusterName: p.cluster_name || null, phase: p.phase || null,
  message: p.message || null, maxVersions: p.max_versions, maxAge: p.max_age || null, attached: p.attached || [],
  schedule: (p.schedule || []).map(r => ({interval: r.interval, versions: r.versions, online: r.online || 0})),
  counts: {volumes: p.lvols_count || 0, chains: p.chains_count || 0,
    versions: p.versions_total || 0, online: p.online_snapshots || 0},
  finest: p.finest_interval || null,
  createdAt: p.created_at, capacity: {total: 0, used: 0}
});
// ReplicationPair — reusable {sourceCluster, targetCluster}. targetCluster is
// immutable after creation, so a pair is never edited, only replaced.
const normReplPair = (o, pols, slots) => {
  const s = o.spec || {}, st = o.status || {};
  const mine = (pols || []).filter(p => (p.spec || {}).pairRef === o.metadata.name);
  const names = mine.map(p => p.metadata.name);
  const mySlots = (slots || []).filter(x => names.includes((x.spec || {}).policyRef));
  return reg({
    kind: "pair", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationPair", shortName: "relpair",
    sourceCluster: s.sourceCluster, targetCluster: s.targetCluster,
    ready: !!st.ready, backendTargetId: st.backendTargetID || null,
    message: st.message || "", activeOpsRef: st.activeOpsRef || null,
    conditions: st.conditions || [],
    status: st.ready ? "online" : "degraded",
    counts: {policies: mine.length, slots: mySlots.length,
      failedOver: mySlots.filter(x => (x.status || {}).state === "failed_over").length,
      errored: mySlots.filter(x => (x.status || {}).state === "error").length},
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationPolicy — one interval, one snapshot count. mode is exactly
// failover | migration; there is no synchronous mode and no tiered retention.
const normReplPolicy = (o, slots, pairs) => {
  const s = o.spec || {}, st = o.status || {};
  const mine = (slots || []).filter(x => (x.spec || {}).policyRef === o.metadata.name)
    .map(x => x.status || {});
  const pair = (pairs || []).find(p => p.metadata.name === s.pairRef);
  // the fixtures' clock, not the wall clock — otherwise every slot reads stale
  const nowMs = window.SB_NOW || Date.now();
  const late = mine.filter(x => {
    if (x.state !== "replicating" || !x.lastReplicatedAt) return false;
    return (nowMs - Date.parse(x.lastReplicatedAt)) / 60000 > ivMinutes(s.interval) * 2;
  }).length;
  return reg({
    kind: "rpolicy", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationPolicy", shortName: "repl",
    pairRef: s.pairRef, pairId: pair ? pair.metadata.uid : null,
    sourceCluster: pair ? (pair.spec || {}).sourceCluster : null,
    targetCluster: pair ? (pair.spec || {}).targetCluster : null,
    mode: s.mode || "failover", interval: s.interval || "5m",
    snapshotRetention: s.snapshotRetention == null ? 3 : s.snapshotRetention,
    ready: !!st.ready, backendPolicyId: st.backendPolicyID || null,
    activeOpsRef: st.activeOpsRef || null, conditions: st.conditions || [],
    message: (st.conditions || []).map(c => c.message).filter(Boolean)[0] || "",
    counts: {slots: st.slotCount != null ? st.slotCount : mine.length,
      replicating: mine.filter(x => x.state === "replicating").length,
      failedOver: mine.filter(x => x.state === "failed_over").length,
      errored: mine.filter(x => x.state === "error").length,
      cutoverPending: mine.filter(x => x.state === "cutover_pending").length,
      late},
    lastAt: mine.map(x => x.lastReplicatedAt).filter(Boolean).sort().slice(-1)[0] || null,
    status: !st.ready ? "degraded"
      : mine.some(x => x.state === "error") ? "unhealthy"
      : late ? "degraded" : "online",
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationSlot — one per PVC, created by the operator from the annotation and
// owned by the PVC. Never created or deleted from here.
const normReplSlot = o => {
  const s = o.spec || {}, st = o.status || {};
  const own = (o.metadata.ownerReferences || [])[0] || {};
  return reg({
    kind: "slot", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationSlot", shortName: "relslot",
    policyRef: s.policyRef, pvcRef: s.pvcRef, volumeId: s.volumeID,
    // <clusterUUID>:<poolUUID>:<volumeUUID>
    volumeParts: String(s.volumeID || "").split(":"),
    state: st.state || "replicating", status: st.state || "replicating",
    direction: st.direction || "source",
    sourceLvolId: st.sourceLvolID || null, targetLvolId: st.targetLvolID || null,
    targetNqn: st.targetNQN || null, lastAt: st.lastReplicatedAt || null,
    message: st.message || "", conditions: st.conditions || [],
    ownedBy: own.kind ? `${own.kind}/${own.name}` : null,
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationOps — one-shot. A terminal op is never re-run; a correction needs
// a new one.
const normReplOps = o => {
  const s = o.spec || {}, st = o.status || {};
  return reg({
    kind: "replops", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationOps", shortName: "replops",
    action: s.action, scope: s.scope, ref: s.ref,
    sourceClusterId: s.sourceClusterID || null, deleteSource: !!s.deleteSource,
    phase: st.phase || "Pending", subphase: st.subphase || null,
    message: st.message || "", startedAt: st.startedAt || null, completedAt: st.completedAt || null,
    results: (st.results || []).map(r => ({slotRef: r.slotRef, status: r.status,
      detail: r.detail || null, targetLvolId: r.targetLvolID || null})),
    terminal: ["Succeeded", "Failed"].includes(st.phase),
    status: st.phase === "Succeeded" ? "online" : st.phase === "Failed" ? "unhealthy"
      : st.phase === "Running" ? "activating" : "idle",
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
const ivMinutes = iv => {
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(String(iv || "").trim());
  if (!m) return 5;
  const n = parseFloat(m[1]), u = m[2] || "m";
  return Math.max(1, Math.round(u === "s" ? n / 60 : u === "h" ? n * 60 : u === "d" ? n * 1440 : u === "w" ? n * 10080 : n));
};

const normPair = p => reg({
  kind: "pair", id: p.uuid, sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id,
  status: p.state, link: p.link || {},
  lastHandshakeAt: p.last_handshake_at, createdAt: p.created_at,
  counts: {policies: p.policies_count || 0}, capacity: {total: 0, used: 0},
  name: null
});
const normDrPolicy = p => reg({
  kind: "rpolicy", id: p.uuid, name: p.name, mode: p.mode, pairId: p.pair_id,
  sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id, zoneIds: p.zone_ids || null,
  frequency: p.frequency_minutes, retention: p.retention || [],
  // An asynchronous DR policy names a consistency group and carries no schedule
  // of its own: the frequency and retention below are the group's.
  cgId: p.cg_id || null, cgName: p.cg_name || null,
  consistencyGroup: !!p.cg_id, failback: p.failback || {},
  status: p.state, lastAt: p.last_replication_at, backlog: p.backlog_bytes,
  generations: p.generations_kept, createdAt: p.created_at,
  lastFailoverAt: p.last_failover_at, failoverMode: p.failover_mode || null, lastTestAt: p.last_test_at,
  drClusterIds: p.dr_cluster_ids || [], replicationClass: p.replication_class || null,
  appDrStatus: p.app_dr_status || "Unavailable",
  counts: {volumes: p.lvols_count !== undefined ? p.lvols_count : (p.lvol_ids || []).length,
    apps: p.apps_count || 0, pvcs: p.pvcs_count || 0, unhealthy: p.unhealthy_apps || 0},
  capacity: {total: 0, used: 0}
});
const normCg = g => reg({
  kind: "cgroup", id: g.uuid, clusterId: g.cluster_id, name: g.name,
  status: g.status || "online", memberIds: g.lvol_ids || [],
  capacity: {total: g.size_prov || 0, used: g.size_util || 0},
  counts: {volumes: g.lvols_count || 0, snapshots: g.snapshots_count || 0, backedUp: g.backed_up_count || 0,
    apps: g.apps_count || 0},
  // A group can own its protection: attaching a policy here applies it to every
  // member, and members added later inherit it.
  // Once a group carries protection its membership is fixed: the retained
  // versions and the replica stream are defined against exactly this set.
  locked: !!g.locked,
  backupPolicy: g.backup_policy || null,
  // The group owns the replication cadence; DR policies just name the group.
  replicationConfig: g.replication_config
    ? {frequency: g.replication_config.frequency_minutes, retention: g.replication_config.retention || []} : null,
  drPolicyIds: g.dr_policy_ids || [],
  replication: g.replication_config ? {status: g.replication_status || "healthy",
    lastAt: g.replication_last_at, backlog: g.replication_backlog_bytes || 0} : null,
  createdAt: g.created_at
});
const normCgSnap = s => reg({
  kind: "cgsnapshot", id: s.uuid, clusterId: s.cluster_id, cgId: s.cg_id, cgName: s.cg_name,
  name: s.snapshot_name, status: s.status || "online", createdAt: s.created_at,
  members: (s.members || []).map(m => ({volumeId: m.lvol_id, volumeName: m.lvol_name, snapshotId: m.snapshot_id, size: m.size})),
  backupVersionId: s.backup_version_id || null, bucket: s.backup_bucket || null,
  capacity: {total: s.size || 0, used: s.size || 0},
  counts: {volumes: (s.members || []).length}
});
const normMigration = m => reg({
  kind: "migration", id: m.uuid, clusterId: m.cluster_id, name: m.name,
  mode: m.mode, scope: m.scope, status: m.state,
  sourceClusterId: m.source_cluster_id, targetClusterId: m.target_cluster_id, targetZoneId: m.target_zone_id,
  targetTaint: m.target_taint, followWorkload: !!m.follow_workload,
  memberIds: m.lvol_ids || [],
  counts: {volumes: m.lvols_count || 0, moved: m.moved_count || 0},
  progress: m.progress_pct || 0, readyToCutover: !!m.ready_to_cutover,
  iterations: m.iterations || 0, iterationLimit: m.iteration_limit || 0,
  firstSnapshot: m.first_snapshot_bytes || 0, lastSnapshot: m.last_snapshot_bytes || 0,
  freezeThreshold: m.freeze_threshold_bytes || 0, freezeMs: m.estimated_freeze_ms || 0,
  throughput: m.throughput_bytes_ps || 0,
  startedAt: m.started_at, completedAt: m.completed_at, frozenAt: m.frozen_at, error: m.error || null,
  capacity: {total: 0, used: 0}, createdAt: m.started_at
});
const normK8s = k => reg({
  kind: "k8sc", id: k.uuid, name: k.name, version: k.version, status: k.status,
  ctx: k.ctx || k.uuid, labels: k.labels || {}, local: !!k.local, controlPlane: k.control_plane || null,
  discovered: !!k.discovered, discoveredAt: k.discovered_at || null,
  endpoint: k.api_endpoint, environment: k.environment,
  csi: {version: k.csi_version, status: k.csi_status}, operatorNamespace: k.operator_namespace || null,
  zoneIds: k.zone_ids || [], storageClusterIds: k.storage_cluster_ids || [], namespaces: k.namespaces || [],
  counts: {storageClasses: k.storage_classes_count || 0, pvcs: k.pvcs_count || 0,
    bound: k.pvcs_bound || 0, workers: k.worker_nodes_count || 0, prepared: k.prepared_hosts_count || 0,
    zones: (k.zone_ids || []).length, storageClusters: (k.storage_cluster_ids || []).length,
    protectedApps: k.protected_apps_count || 0},
  drClusterId: k.dr_cluster_id || null,
  capacity: {total: k.provisioned_bytes || 0, used: k.provisioned_bytes || 0},
  createdAt: k.created_at
});
const normSc = s => reg({
  kind: "storageclass", id: s.uuid, k8sClusterId: s.k8s_cluster_id, name: s.name,
  ctx: s.ctx || LOCAL_CTX, ns: null, k8s: s.__k8s || null, clusterName: s.cluster_name || null,
  provisioner: s.provisioner, clusterId: s.cluster_id, poolId: s.pool_id, poolName: s.pool_name,
  operatorNamespace: s.operator_namespace || null, storagePoolRef: s.storage_pool_ref || null,
  variant: s.variant || "default",
  parameters: s.parameters || {}, specParameters: s.spec_parameters || {},
  zoneClusterMap: s.zone_cluster_map || {}, regionClusterMap: s.region_cluster_map || {},
  dhchap: !!s.dhchap, allowedTopology: s.allowed_topology || null,
  reclaim: s.reclaim_policy, binding: s.volume_binding_mode,
  expansion: !!s.allow_volume_expansion, isDefault: !!s.is_default, status: "active",
  counts: {pvcs: s.pvcs_count || 0, bound: s.bound_count || 0},
  capacity: {total: s.provisioned_bytes || 0, used: s.provisioned_bytes || 0},
  createdAt: s.created_at
});
const normBucket = b => reg({
  kind: "bucket", id: b.uuid, clusterId: b.cluster_id, name: b.name,
  volumeId: b.lvol_id, volumeName: b.lvol_name, poolId: b.pool_id, poolName: b.pool_name,
  status: b.status || "online",
  versioning: !!b.versioning, objectLock: !!b.object_lock, encrypted: !!b.encrypted,
  quota: b.quota_bytes || 0, objects: b.objects || 0,
  access: b.access || {}, replication: b.replication || null,
  // S3 metadata — what a client sets, what the console searches by
  region: b.region || null, storageClass: b.storage_class || "standard", owner: b.owner || null,
  tags: b.tags || {}, lifecycle: b.lifecycle_rules || [], cors: !!b.cors_enabled,
  counts: {snapshots: b.snapshots_count || 0, backups: b.backups_count || 0, tags: Object.keys(b.tags || {}).length},
  capacity: {total: b.provisioned_bytes || 0, used: b.size_bytes || 0},
  createdAt: b.created_at
});
const normMPath = p => reg({
  kind: "mpath", id: p.uuid, name: p.name, status: p.status, k8sClusterId: p.k8s_cluster_id,
  sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id,
  sourceZoneId: p.source_zone_id || null, targetZoneId: p.target_zone_id || null,
  queue: p.queue || [], log: (p.log || []).map(l => ({ts: l.ts, level: l.level, groupId: l.group_id, msg: l.msg})),
  createdAt: p.created_at, capacity: {total: 0, used: 0}, counts: {groups: (p.queue || []).length}
});
const normAppGroup = g => reg({
  kind: "appgroup", id: g.uuid, pathId: g.path_id, name: g.name, namespace: g.namespace,
  status: g.phase, phase: g.phase, message: g.message || null, approval: g.approval || "manual", order: g.order || 0,
  members: (g.members || []).map(m => ({kind: m.kind, name: m.name, state: m.state || "Pending", progress: m.progress || 0})),
  pvcIds: g.pvc_ids || [], memberIds: g.lvol_ids || [], rpolicyId: g.rpolicy_id || null,
  volumes: (g.volumes || []).map(v => ({id: v.lvol_id, name: v.lvol_name, size: v.size || 0, backlog: v.backlog_bytes || 0, lastAt: v.last_replication_at, progress: v.progress || 0, migrated: !!v.migrated})),
  backlog: (g.volumes || []).reduce((n, v) => n + (v.backlog_bytes || 0), 0),
  counts: {members: (g.members || []).length, vms: (g.members || []).filter(m => m.kind === "VirtualMachine").length, volumes: (g.lvol_ids || []).length,
    moved: (g.members || []).filter(m => m.state === "Moved").length, migrated: (g.volumes || []).filter(v => v.migrated).length},
  capacity: {total: (g.volumes || []).reduce((n, v) => n + (v.size || 0), 0), used: 0},
  createdAt: g.created_at, startedAt: g.started_at, finishedAt: g.finished_at
});
const normPvc = p => reg({
  kind: "pvc", id: p.uuid, k8sClusterId: p.k8s_cluster_id,
  ctx: p.ctx || LOCAL_CTX, ns: p.namespace, k8s: p.__k8s || null, pvName: p.pv_name || null,
  clusterId: p.cluster_id || null, poolId: p.pool_id || null, dataSource: p.data_source || null,
  backupPolicy: p.backup_policy || null, replicationPolicy: p.replication_policy || null,
  namespace: p.namespace, name: p.pvc_name,
  storageClassId: p.storage_class_id, storageClass: p.storage_class,
  volumeId: p.lvol_id || null,
  status: p.status, accessMode: p.access_mode, volumeMode: p.volume_mode, filesystem: p.filesystem || null,
  workload: p.workload, workloadKind: p.workload_kind,
  annotations: p.annotations || {}, labels: p.labels || {},
  capacity: {total: p.actual_bytes || p.requested_bytes || 0, used: p.requested_bytes || 0},
  requested: p.requested_bytes || 0, createdAt: p.created_at
});
const normDrCluster = d => reg({
  kind: "drcluster", id: d.uuid, k8sClusterId: d.k8s_cluster_id, name: d.name, region: d.region,
  status: d.status, fencing: d.fencing_state, ramen: d.ramen_version,
  s3: {profile: d.s3_profile_name, endpoint: d.s3_endpoint, bucket: d.s3_bucket},
  lastHeartbeatAt: d.last_heartbeat_at, createdAt: d.created_at,
  counts: {apps: d.apps_count || 0, standby: d.standby_apps_count || 0, policies: d.policies_count || 0},
  capacity: {total: 0, used: 0}
});
// A protection plan: the sites it spans, the storage profile, and the methods
// it declares. Everything below it — DRCluster, DRPolicy, DRPC, the class
// matrix — is derived, never authored.
const CLS_NAME = m => m.type === "sync" ? "sb-sync-" + m.name
  : m.type === "async" ? "sb-async-" + m.interval : "sb-vault-" + m.interval;
const normMethod = (m, plan) => ({
  name: m.name, type: m.type, target: m.target,
  interval: m.interval, classInterval: m.class_interval,
  // one top-layer field written to two places: DRPolicy.spec.schedulingInterval
  // and the class's parameters.schedulingInterval. If they differ by even
  // formatting, no class resolves, the policy still validates cleanly, and the
  // application is protected by nothing.
  intervalConsistent: m.type === "sync" || m.class_interval === m.interval,
  retention: m.retention || null, immutable: !!m.immutable, bucket: m.bucket || null,
  cls: (plan.classes || []).find(c => c.name === CLS_NAME(m)) || null,
  rpo: m.type === "sync" ? "0s" : m.interval,
  failback: m.type !== "snapshot-s3",
  generationSelect: m.type === "snapshot-s3"
});
const normPlan = p => reg({
  kind: "plan", id: p.uuid, name: p.name, storageProfile: p.storage_profile,
  siteNames: p.site_names || [], siteIds: p.site_ids || [],
  methods: (p.methods || []).map(m => normMethod(m, p)),
  classes: (p.classes || []).map(c => ({
    name: c.name, crdKind: c.kind, replicationId: c.replication_id, storageId: c.storage_id,
    provisioner: c.provisioner, labels: c.labels || {}, parameters: c.parameters || {}
  })),
  policies: (p.policies || []).map(x => ({
    name: x.name, drClusters: x.dr_clusters, schedulingInterval: x.scheduling_interval,
    selector: x.replication_class_selector || {}, methodName: x.method_name,
    peerClass: x.peer_class || null, validated: x.validated
  })),
  status: p.health || "healthy",
  protectionGap: !!p.protection_gap,
  worstLagSeconds: p.worst_lag_seconds || 0,
  counts: {apps: p.apps_count || 0, pvcs: p.pvcs_count || 0, methods: p.methods_count || 0,
    sites: p.sites_count || 0, policies: p.policies_count || 0,
    unvalidated: p.unvalidated_count || 0, generations: p.generations_total || 0},
  capacity: {total: 0, used: 0}, createdAt: p.created_at
});
// A site is one managed cluster. Its name must equal the OCM ManagedCluster
// name, and its region is how sync versus async is declared: equal region means
// the pair can mirror synchronously, distinct region cannot.
const normSite = s => reg({
  kind: "site", id: s.uuid, name: s.name, k8sClusterId: s.k8s_cluster_id,
  region: s.region, s3Profile: s.s3_profile_name, s3Endpoint: s.s3_endpoint, s3Bucket: s.s3_bucket,
  cidrs: s.cidrs || [], fencing: s.fencing_state, status: s.status,
  ramen: s.ramen_version, discoveredClasses: s.discovered_classes || [],
  counts: {activeApps: s.active_apps_count || 0, standbyApps: s.standby_apps_count || 0,
    plans: s.plans_count || 0, classes: (s.discovered_classes || []).length},
  capacity: {total: 0, used: 0}, createdAt: s.onboarded_at
});
const normGeneration = g => ({
  generation: g.generation, tier: g.tier, at: g.at, size: g.size_bytes,
  integrity: g.integrity_state, lockedUntil: g.locked_until || null,
  consistencyGroup: g.consistency_group || null, kind: g.kind, verifiedAt: g.verified_at || null
});
const normLeg = l => ({
  id: l.leg_id, method: l.method, type: l.type, target: l.target,
  state: l.state, lastAt: l.last_sync_at, lagSeconds: l.lag_seconds,
  epoch: l.epoch, bytesLastCycle: l.bytes_last_cycle,
  status: l.health, orchestrated: !!l.orchestrated, note: l.note || null,
  generations: l.generations || 0,
  oldestAt: l.oldest_generation_at || null, newestAt: l.newest_generation_at || null,
  lockedUntil: l.locked_until || null
});
const normArbitration = t2 => ({
  holder: t2.token_holder, generation: t2.generation,
  quorumAck: (t2.quorum_ack || []).map(q => ({site: q.site, ackedAt: q.acked_at})),
  fencedSites: t2.fenced_sites || [], quorumSize: t2.quorum_size, updatedAt: t2.updated_at
});

const normApp = a2 => reg({
  kind: "protectedapp", id: a2.uuid, name: a2.app_name, namespace: a2.namespace, appKind: a2.app_kind,
  policyId: a2.policy_id, policyName: a2.policy_name,
  preferredClusterId: a2.preferred_cluster_id, failoverClusterId: a2.failover_cluster_id,
  selector: a2.pvc_selector || {}, phase: a2.phase, progression: a2.progression, action: a2.action,
  status: a2.health || "healthy", rpoMet: a2.rpo_met !== false, vrgState: a2.vrg_state,
  lastSyncAt: a2.last_group_sync_at, lastSyncDuration: a2.last_group_sync_duration_s,
  lastSyncBytes: a2.last_group_sync_bytes,
  kubeObjectProtection: !!a2.kube_object_protection,
  recipe: a2.recipe || null,
  planId: a2.plan_id || null, planName: a2.plan_name || null, storageProfile: a2.storage_profile || null,
  preferredSite: a2.preferred_site || null, activeSite: a2.active_site || null,
  preferredSiteId: a2.preferred_site_id || null, activeSiteId: a2.active_site_id || null,
  // Ramen drives exactly one method: a DRPC selects PVCs by label, so two over
  // the same PVCs would both claim them. The other methods run with identical
  // parameters in the data plane but report their lag out of band.
  orchestratedMethod: a2.orchestrated_method || null,
  legs: (a2.legs || []).map(normLeg),
  generations: (a2.generations || []).map(normGeneration),
  vaultMethod: a2.vault_method || null,
  pinnedGeneration: a2.pinned_generation != null ? a2.pinned_generation : null,
  restoredFromGeneration: a2.restored_from_generation != null ? a2.restored_from_generation : null,
  restoreTarget: a2.restore_target || null,
  needsReprotect: !!a2.needs_reprotect,
  failoverTargets: a2.failover_targets || [], restoreTargets: a2.restore_targets || [],
  backupPolicyId: a2.backup_policy_id || null, backupPolicyName: a2.backup_policy_name || null,
  cgId: a2.cg_id || null, cgName: a2.cg_name || null,
  // Protection lag rolled up over the group: the oldest member's last successful
  // cycle, and the data written since then.
  group: {lastAt: a2.group_last_at || null, writtenSince: a2.group_written_since || 0,
    lagSeconds: a2.group_lag_seconds, volumes: (a2.volumes || []).map(v => ({
      id: v.lvol_id, name: v.lvol_name, size: v.size, lastAt: v.last_at,
      writtenSince: v.written_since, status: v.status}))},
  counts: {pvcs: a2.pvcs_count || 0}, memberIds: a2.pvc_ids || [],
  capacity: {total: 0, used: 0}, createdAt: a2.created_at
});
const normZone = s => reg({
  kind: "zone", id: s.uuid, name: s.name, location: s.location, region: s.region,
  status: "active", region: s.region, label: s.label, regionLabel: s.region_label, createdAt: s.created_at,
  clusterIds: s.cluster_ids || [], racks: s.racks || [],
  k8sClusterIds: s.k8s_cluster_ids || [], k8sClusters: s.k8s_clusters || [],
  untaintedHosts: s.untainted_hosts || 0,
  counts: {hosts: s.hosts_count || 0, hostsPrepared: s.hosts_prepared || 0, nvme: s.nvme_hosts || 0,
    nodes: s.nodes_count || 0, clusters: (s.cluster_ids || []).length},
  capacity: {total: s.size_total || 0, used: 0}
});
// a backend task, from the v2 client's normalized form; the index remembers
// which cluster and context a task id belongs to so sub-task reads resolve
const TASK_INDEX = {};
const normTask = (t, all) => {
  TASK_INDEX[t.id] = {ctx: t.ctx, clusterId: t.clusterId};
  return {
    kind: "task", id: t.id, ctx: t.ctx, clusterId: t.clusterId, parentId: t.parentId || null, derived: !!t.derived,
    fn: t.fn, target: t.deviceId || t.nodeId || null, nodeId: t.nodeId || null, deviceId: t.deviceId || null, distrib: null,
    params: t.params || {}, retry: t.retry, maxRetry: t.maxRetry, status: t.status, result: t.result,
    canceled: !!t.canceled, subtaskTotal: (all || []).filter(x => x.parentId === t.id).length,
    createdAt: t.createdAt, updatedAt: t.updatedAt, raw: t.raw
  };
};
// the event log arrives normalized from cp-client.jsx
const normLog = l => l;
// An alert is an indicator, not an event: it exists while its condition holds
// and vanishes when the condition clears. There is no acknowledged/closed
// lifecycle — only a silence, which dies with the condition.
const normAlert = a => ({
  id: a.uuid, clusterId: a.cluster_id, clusterName: a.cluster_name,
  rule: a.rule, severity: a.severity, scope: a.scope,
  title: a.title, detail: a.detail, remedy: a.remedy,
  nodeId: a.node_id, nodeName: a.node_name,
  nodeIds: a.node_ids || [], nodeNames: a.node_names || [],
  deviceIds: a.device_ids || [], deviceNames: a.device_names || [],
  container: a.container || null,
  since: a.since, silenced: !!a.silenced, silencedBy: a.silenced_by || null
});

// An operation is an Ops CRD: a named action on one object that lands in
// Succeeded, Failed or Aborted. Each kind reports progress its own way (a
// sub-phase, a per-node walk, a step state machine), so the step list here is
// read from the kind rather than invented.
const OPS_KINDS = ["StorageClusterOps", "StorageNodeOps", "VolumeMigration", "OperatorOps"];
const OPS_TARGET_LABEL = {StorageCluster: "cluster", StorageNode: "node", StorageDevice: "device",
  StoragePool: "pool", PersistentVolume: "volume", StorageBackup: "backup", ControlPlane: "control plane", Operator: "operator"};
const OPS_STEPS = {
  StorageNodeOps: {remove: ["Validating", "Suspending", "Migrating", "Verifying", "Removing"], migrate: ["Preparing", "Restarting", "Promoting"]},
  StorageClusterOps: {"node-rolling-restart": ["snode-refresh", "snode-refresh-wait", "shutting-down", "restarting", "rebalancing"]},
  OperatorOps: {Discover: ["Inspecting", "Probing", "Writing"]},
  VolumeMigration: {migrate: ["Pending", "Validating", "Running", "Completed"]},
  ClusterDeploymentConfig: {deploy: ["Validating", "CreatingCluster", "AwaitingCluster", "CreatingNodes"]}
};
const normOperation = (o, ctx) => {
  const sp = o.spec || {}, st = o.status || {}, kind = o.kind;
  const action = sp.action || (kind === "VolumeMigration" ? "migrate" : kind === "ClusterDeploymentConfig" ? "deploy" : null);
  const targetName = sp.clusterRef || sp.storageNodeRef || sp.pvName || null;
  const targetKind = sp.clusterRef ? "StorageCluster" : sp.storageNodeRef ? "StorageNode" : sp.pvName ? "PersistentVolume" : (window.RESOURCES[kind] || {}).ops || null;
  const rr = st.nodeRollingRestartStatus || null;
  const stepKey = (st.step && st.step.state) || st.subPhase || (rr && rr.nodePhase) || null;
  const steps = (OPS_STEPS[kind] || {})[action] || [];
  const phase = st.phase || "Pending";
  // the cluster an op belongs to, through the registry when the target is known
  const target = Object.values(REG).find(v => v.ctx === (ctx || LOCAL_CTX) && v.k8s && v.k8s.kind === targetKind && v.k8s.name === targetName) || null;
  const clusterId = st.clusterUUID || (target ? (target.kind === "cluster" ? target.id : target.clusterId) : null);
  const progress = rr ? {done: (rr.processedNodes || []).length, total: (rr.processedNodes || []).length + (rr.pendingNodes || []).length, unit: "nodes"}
    : st.volumesMigrated !== undefined || st.volumesPending !== undefined ? {done: st.volumesMigrated || 0, total: (st.volumesMigrated || 0) + (st.volumesPending || 0), unit: "volumes"} : null;
  return {
    id: o.metadata.uid, name: o.metadata.name, kind, ctx: ctx || LOCAL_CTX, ns: o.metadata.namespace,
    action, clusterId, targetKind, targetName, targetId: target ? target.id : null,
    phase, message: st.message || st.errorMessage || "",
    step: stepKey, stepKey, steps, stepIndex: Math.max(0, steps.indexOf(stepKey)),
    abortable: ABORTABLE_KINDS.includes(kind) && !["Succeeded", "Failed", "Aborted"].includes(phase), aborting: !!sp.abort,
    progress, startedAt: st.startedAt || o.metadata.creationTimestamp, completedAt: st.completedAt || null,
    events: st.events || [], running: !["Succeeded", "Failed", "Aborted"].includes(phase), raw: o
  };
};

const normRole = r => ({kind: "role", id: r.uuid || r.name, name: r.name, boundAt: r.boundAt || "", description: r.description || "",
  parts: r.parts || [], rules: r.rules || (r.parts || []).flatMap(p => p.rules)});

// ---- endpoints -------------------------------------------------------------
// ---- reading storage objects -------------------------------------------------
// The registry says which context an id lives in. When it does not know yet
// (a cold deep link), every context is asked and the first answer wins.
const ctxOfId = id => (REG[id] || {}).ctx || LOCAL_CTX;
const nsOfCtx = ctx => { const n = ctxNamespaces(ctx); return Array.isArray(n) ? n[0] : NS(); };
const one = (fn, id, norm, label) => {
  const known = REG[id];
  const ctxs = known ? [known.ctx] : CONTEXTS.map(c => c.name);
  return Promise.all(ctxs.map(ctx => fn(ctx, id).catch(() => null))).then(rs => {
    const hit = rs.find(Boolean);
    if (!hit) throw new ApiError(404, `${label} ${id} not found`, "/" + id, "NotFound");
    return norm(hit);
  });
};
const everywhere = (fn, norm) => Promise.all(CONTEXTS.map(c => fn(c.name).catch(() => []))).then(rs => rs.flat().map(norm));
const SNAP_SCOPE = {clusters: "clusterId", pools: "poolId", lvols: "volumeId", volumes: "volumeId"};

const api = {
  clusters: () => everywhere(sources.clusters, normCluster),
  cluster: id => one(sources.cluster, id, normCluster, "cluster"),
  hosts: cid => sources.hosts(ctxOfId(cid), cid).then(r => r.map(normHost)),
  host: id => one(sources.host, id, normHost, "host"),
  nodes: cid => sources.nodes(ctxOfId(cid), cid).then(r => r.map(normNode)),
  node: id => one(sources.node, id, normNode, "storage node"),
  devices: nid => sources.devices(ctxOfId(nid), nid).then(r => r.map(normDevice)),
  device: id => one(sources.device, id, normDevice, "device"),
  pools: cid => sources.pools(ctxOfId(cid), cid).then(r => r.map(normPool)),
  pool: id => one(sources.pool, id, normPool, "pool"),
  poolVolumes: pid => sources.volumes(ctxOfId(pid), {poolId: pid}).then(r => r.map(normVolume)),
  poolStorageClasses: pid => sources.storageClasses(ctxOfId(pid), pid).then(r => r.map(normSc)),
  clusterVolumes: cid => sources.volumes(ctxOfId(cid), {clusterId: cid}).then(r => r.map(normVolume)),
  nodeVolumes: nid => sources.volumes(ctxOfId(nid), {nodeId: nid}).then(r => r.map(normVolume)),
  volume: id => one(sources.volume, id, normVolume, "volume"),
  snapshots: (scope, id) => sources.snapshots(ctxOfId(id), {[SNAP_SCOPE[scope] || "clusterId"]: id}).then(r => r.map(normSnapshot)),
  snapshot: id => one(sources.snapshot, id, normSnapshot, "snapshot"),
  backups: (scope, id) => sources.backups(ctxOfId(id), {[SNAP_SCOPE[scope] || "clusterId"]: id}).then(r => r.map(normBackup)),
  backup: id => one(sources.backup, id, normBackup, "backup"),
  policies: cid => sources.policies(ctxOfId(cid), cid).then(r => r.map(normPolicy)),
  policy: id => one(sources.policy, id, normPolicy, "backup policy"),
  cgroups: cid => req(`/clusters/${cid}/consistency-groups`).then(r => r.map(normCg)),
  cgroup: id => req(`/consistency-groups/${id}`).then(r => normCg(r[0])),
  cgroupVolumes: id => req(`/consistency-groups/${id}/lvols`).then(r => r.map(normVolume)),
  cgSnapshots: id => req(`/consistency-groups/${id}/snapshots`).then(r => r.map(normCgSnap)),
  cgSnapshot: id => req(`/cg-snapshots/${id}`).then(r => normCgSnap(r[0])),
  cgroupCreate: (cid, p) => send("POST", `/clusters/${cid}/consistency-groups`, p),
  cgroupAddVolumes: (id, ids) => send("POST", `/consistency-groups/${id}/lvols`, {lvol_ids: ids}),
  cgroupRemoveVolume: (id, vid) => send("DELETE", `/consistency-groups/${id}/lvols/${vid}`),
  cgroupSnapshot: (id, name) => send("POST", `/consistency-groups/${id}/snapshot`, {name}),
  cgroupDelete: id => send("DELETE", `/consistency-groups/${id}`),
  cgroupSetPolicy: (id, policyId) => send("PUT", `/consistency-groups/${id}/backup-policy`, {policy_id: policyId}),
  cgroupReplicate: (id, cfg) => send("POST", `/consistency-groups/${id}/replicate`, cfg),
  cgroupUnreplicate: id => send("POST", `/consistency-groups/${id}/unreplicate`),
  cgroupApps: id => req("/protected-apps").then(r => r.map(normApp).filter(a2 => a2.cgId === id)),
  cgSnapBackup: (id, bucket) => send("POST", `/cg-snapshots/${id}/backup`, {bucket}),
  cgSnapRestore: (id, p) => send("POST", `/cg-snapshots/${id}/restore`, p),
  cgSnapDelete: id => send("DELETE", `/cg-snapshots/${id}`),
  snapshotRestore: (id, p) => send("POST", `/snapshots/${id}/restore`, p),
  clusterSetKms: (cid, p) => send("PUT", `/clusters/${cid}/kms`, p),
  clusterTestKms: cid => send("POST", `/clusters/${cid}/kms/test`),
  // ---- tasks, the event log, operations: the control plane and the Ops kinds ----
  tasks: cid => cp.tasks(ctxOfId(cid), cid).then(ts => ts.map(t => normTask(t, ts))),
  subtasks: tid => {
    const ix = TASK_INDEX[tid];
    if (!ix) return Promise.resolve([]);
    return cp.tasks(ix.ctx, ix.clusterId).then(ts => ts.filter(t => t.parentId === tid).map(t => normTask(t, ts)));
  },
  // the control plane exposes no route to cancel a task (backend gap)
  taskCancel: () => Promise.reject(new ApiError(501, "the control plane exposes no route to cancel a task", "/tasks", "NotImplemented")),
  clusterLogs: (cid, limit) => cp.logs(ctxOfId(cid), cid, limit || 400),
  // alerts are derived in alerts.jsx; until it loads these answer empty
  alerts: cid => Promise.resolve(window.alerts.ready ? window.alerts.forCluster(cid) : []),
  allAlerts: () => Promise.resolve(window.alerts.ready ? window.alerts.all() : []),
  cpAlerts: () => Promise.resolve(window.alerts.ready ? window.alerts.all().filter(a => a.scope === "cp") : []),
  operations: cid => Promise.all(CONTEXTS.map(c => Promise.all(OPS_KINDS.map(k2 => store.read(c.name, k2).catch(() => [])))
      .then(rs => rs.flat().map(o => normOperation(o, c.name)))))
    .then(rs => rs.flat().filter(o => !cid || o.clusterId === cid)
      .sort((x, y) => (y.running - x.running) || Date.parse(y.startedAt) - Date.parse(x.startedAt))),
  operationAbort: (kind, name, opts) => abortOps(kind, name, opts).then(touched((opts || {}).ctx, kind)),
  alertRuleCount: () => null,
  alertSilence: id => Promise.resolve(window.alerts.ready ? window.alerts.silence({fingerprint: id}) : null),
  alertUnsilence: id => Promise.resolve(window.alerts.ready ? window.alerts.unsilence(id) : null),
  unassignedHosts: () => everywhere(sources.unassignedHosts, normHost),
  // ---- replication: the real v1alpha1 kinds, on the Kubernetes API ----
  // Pairs, policies and slots are three lists that have to be joined, because
  // the CRDs reference each other by name and carry no rollups.
  replTree: () => Promise.all([
    k8s.list("ReplicationPair"), k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")
  ]).then(([pairs, pols, slots]) => ({pairs, pols, slots})),
  pairs: () => api.replTree().then(t => t.pairs.map(p => normReplPair(p, t.pols, t.slots))),
  pair: id => api.replTree().then(t => {
    const p = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!p) throw new ApiError(404, `ReplicationPair ${id} not found`, "", "NotFound");
    return normReplPair(p, t.pols, t.slots);
  }),
  allRPolicies: () => api.replTree().then(t => t.pols.map(p => normReplPolicy(p, t.slots, t.pairs))),
  // a policy belongs to a pair, and a pair to a source cluster — so a
  // cluster-scoped list is a filter on the pair's sourceCluster
  rpolicies: cid => api.replTree().then(t => {
    const name = (REG[cid] || {}).name;
    const mine = t.pairs.filter(p => (p.spec || {}).sourceCluster === name).map(p => p.metadata.name);
    return t.pols.filter(p => mine.includes((p.spec || {}).pairRef))
      .map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  rpolicy: id => api.replTree().then(t => {
    const p = t.pols.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!p) throw new ApiError(404, `ReplicationPolicy ${id} not found`, "", "NotFound");
    return normReplPolicy(p, t.slots, t.pairs);
  }),
  pairRPolicies: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    return t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name)
      .map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  slots: () => k8s.list("ReplicationSlot").then(r => r.map(normReplSlot)),
  slot: id => k8s.list("ReplicationSlot").then(r => {
    const s = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!s) throw new ApiError(404, `ReplicationSlot ${id} not found`, "", "NotFound");
    return normReplSlot(s);
  }),
  policySlots: id => Promise.all([k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")])
    .then(([pols, slots]) => {
      const p = pols.find(x => x.metadata.uid === id || x.metadata.name === id);
      return slots.filter(s => p && (s.spec || {}).policyRef === p.metadata.name).map(normReplSlot);
    }),
  pairSlots: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    const names = t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name)
      .map(p => p.metadata.name);
    return t.slots.filter(s => names.includes((s.spec || {}).policyRef)).map(normReplSlot);
  }),
  replOps: () => k8s.list("ReplicationOps").then(r => r.map(normReplOps)),
  replOp: id => k8s.list("ReplicationOps").then(r => {
    const o = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!o) throw new ApiError(404, `ReplicationOps ${id} not found`, "", "NotFound");
    return normReplOps(o);
  }),
  // scope-filtered history, so a policy or pair can show its own operations
  refReplOps: ref => k8s.list("ReplicationOps").then(r =>
    r.filter(o => (o.spec || {}).ref === ref).map(normReplOps)),
  pairCreateCrd: (name, sourceCluster, targetCluster) => k8s.create("ReplicationPair", {
    apiVersion: window.API_GROUP, kind: "ReplicationPair",
    metadata: {name}, spec: {sourceCluster, targetCluster}
  }),
  rpolicyCreateCrd: (name, spec) => k8s.create("ReplicationPolicy", {
    apiVersion: window.API_GROUP, kind: "ReplicationPolicy", metadata: {name}, spec
  }),
  // every failover, failback and planned cutover is a ReplicationOps
  replOpsCreate: spec => k8s.create("ReplicationOps", {
    apiVersion: window.API_GROUP, kind: "ReplicationOps",
    metadata: {name: `${spec.action}-${spec.ref}-${Date.now().toString(36)}`.slice(0, 253)},
    spec
  }),
  pairDeleteCrd: name => k8s.remove("ReplicationPair", name),
  rpolicyDeleteCrd: name => k8s.remove("ReplicationPolicy", name),
  // membership is a PVC annotation, nothing else
  pvcSetReplPolicy: (pvcName, namespace, policyName) => k8s.patch("PersistentVolumeClaim", pvcName,
    {metadata: {annotations: {"storage.simplyblock.io/replication-policy": policyName || null}}},
    {namespace}),
  // ---- migration paths: A → B inside a stretched Kubernetes cluster ----
  mpaths: () => req("/migration-paths").then(r => r.map(normMPath)),
  mpath: id => req(`/migration-paths/${id}`).then(r => normMPath(r[0])),
  mpathGroups: id => req(`/migration-paths/${id}/app-groups`).then(r => r.map(normAppGroup).sort((x, y) => x.order - y.order)),
  mpathCreate: p => send("POST", "/migration-paths", p),
  mpathPause: id => send("POST", `/migration-paths/${id}/pause`),
  mpathResume: id => send("POST", `/migration-paths/${id}/resume`),
  mpathDelete: id => send("DELETE", `/migration-paths/${id}`),
  mpathReorder: (id, order) => send("PUT", `/migration-paths/${id}/queue`, {order}),
  appGroupCreate: (pathId, p) => send("POST", `/migration-paths/${pathId}/app-groups`, p),
  appGroup: id => req(`/app-groups/${id}`).then(r => normAppGroup(r[0])),
  appGroupVolumes: id => req(`/app-groups/${id}/lvols`).then(r => r.map(normVolume)),
  appGroupMove: id => send("POST", `/app-groups/${id}/move`),
  appGroupPause: id => send("POST", `/app-groups/${id}/pause`),
  appGroupResume: id => send("POST", `/app-groups/${id}/resume`),
  appGroupApproval: (id, approval) => send("PUT", `/app-groups/${id}/approval`, {approval}),
  appGroupDelete: id => send("DELETE", `/app-groups/${id}`),
  migrations: cid => req(`/clusters/${cid}/migrations`).then(r => r.map(normMigration)),
  allMigrations: () => req("/migrations").then(r => r.map(normMigration)),
  migration: id => req(`/migrations/${id}`).then(r => normMigration(r[0])),
  migrationVolumes: id => req(`/migrations/${id}/lvols`).then(r => r.map(normVolume)),
  migrationCreate: (cid, p) => send("POST", `/clusters/${cid}/migrations`, p),
  migrationCutover: id => send("POST", `/migrations/${id}/cutover`),
  migrationPause: id => send("POST", `/migrations/${id}/pause`),
  migrationResume: id => send("POST", `/migrations/${id}/resume`),
  migrationCancel: id => send("DELETE", `/migrations/${id}`),
  hostSetTaint: (id, taint) => send("PUT", `/hosts/${id}/migration-taint`, {taint}),
  // ---- DR top layer: plans and sites ----
  plans: () => req("/protection-plans").then(r => r.map(normPlan)),
  plan: id => req("/protection-plans/" + id).then(r => normPlan(r[0])),
  planApps: id => req("/protection-plans/" + id + "/protected-apps").then(r => r.map(normApp)),
  planSites: id => req("/protection-plans/" + id + "/sites").then(r => r.map(normSite)),
  planCreate: p => send("POST", "/protection-plans", p),
  planDelete: id => send("DELETE", "/protection-plans/" + id),
  planAddMethod: (id, m) => send("POST", "/protection-plans/" + id + "/methods", m),
  planRemoveMethod: (id, name) => send("DELETE", "/protection-plans/" + id + "/methods/" + name),
  planSetInterval: (id, name, interval) => send("PUT", "/protection-plans/" + id + "/methods/" + name + "/interval", {interval}),
  sites: () => req("/dr-sites").then(r => r.map(normSite)),
  site: id => req("/dr-sites/" + id).then(r => normSite(r[0])),
  siteApps: id => req("/dr-sites/" + id + "/protected-apps").then(r => r.map(normApp)),
  siteFence: id => send("POST", "/dr-sites/" + id + "/fence"),
  siteUnfence: id => send("POST", "/dr-sites/" + id + "/unfence"),
  sitePlans: id => req("/dr-sites/" + id + "/protection-plans").then(r => r.map(normPlan)),
  // bypass payload 4: no Kubernetes object represents a generation
  appGenerations: id => req("/protected-apps/" + id + "/generations").then(r => r.map(normGeneration)),
  appSetOrchestrated: (id, method, allowVault) => send("PUT", "/protected-apps/" + id + "/orchestrated-method", {method, allow_vault: !!allowVault}),
  // bypass payload 2: pin the generation, then rebind the DRPC
  appRestore: (id, generation) => send("POST", "/protected-apps/" + id + "/restore", {generation}),
  appVerifyGeneration: (id, generation) => send("POST", "/protected-apps/" + id + "/verify-generation", {generation}),
  // bypass payload 6: three sites need a quorum decision Ramen does not model
  arbitration: () => req("/dr-arbitration").then(r => normArbitration(r[0])),
  drClusters: () => req("/dr-clusters").then(r => r.map(normDrCluster)),
  drCluster: id => req(`/dr-clusters/${id}`).then(r => normDrCluster(r[0])),
  drClusterApps: id => req(`/dr-clusters/${id}/protected-apps`).then(r => r.map(normApp)),
  rpolicyApps: id => req(`/replication-policies/${id}/protected-apps`).then(r => r.map(normApp)),
  protectedApps: () => req("/protected-apps").then(r => r.map(normApp)),
  protectedApp: id => req(`/protected-apps/${id}`).then(r => normApp(r[0])),
  protectedAppPvcs: id => req(`/protected-apps/${id}/pvcs`).then(r => r.map(normPvc)),
  appProtect: p => send("POST", "/protected-apps", p),
  appUpdate: (id, p) => send("PUT", `/protected-apps/${id}`, p),
  appSetRecipe: (id, p) => send("PUT", `/protected-apps/${id}/recipe`, p),
  // application resources in a namespace, one Kubernetes list per kind — the
  // callback fires per kind as each list lands, so the form fills in progressively
  nsResourcesEach: (ns, cb) => Promise.all(["Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "Ingress", "PersistentVolumeClaim", "VirtualMachine"]
    .map(kind => k8s.list(kind, {namespace: ns}).then(items => cb(kind, items, null)).catch(e => cb(kind, [], e)))),
  appFailover: (id, p) => send("POST", `/protected-apps/${id}/failover`, p || {}),
  appSetProtection: (id, p) => send("PUT", `/protected-apps/${id}/protection`, p),
  appRelocate: id => send("POST", `/protected-apps/${id}/relocate`),
  appCleanup: id => send("POST", `/protected-apps/${id}/cleanup`),
  appUnprotect: id => send("DELETE", `/protected-apps/${id}`),
  drClusterFence: id => send("POST", `/dr-clusters/${id}/fence`),
  drClusterUnfence: id => send("POST", `/dr-clusters/${id}/unfence`),
  // access control — RBAC-DESIGN.md. Kubernetes RBAC is the store; the operator
  // serves the aggregated view: one SelfSubjectRulesReview per namespace (/self),
  // the permitted scope tree (/scopes), AccessGrants, and SubjectAccessReview.
  // Who the caller is and what they may do, from the Kubernetes API itself:
  // identity and rules from the access reviews, and the namespace each
  // cluster and pool lives in so a rule can be judged in the right place.
  // Rules are read in the local context; a remote context enforces its own
  // RBAC on every call regardless.
  accessSelf: async () => {
    const [who, rules, clusters, pools] = await Promise.all([
      review.whoami().catch(() => ({})), review.rules(NS()).catch(() => ({resourceRules: [], incomplete: true})),
      api.clusters().catch(() => []), everywhere(sources.pools, normPool).catch(() => [])]);
    const norm = (rules.resourceRules || []).map(r => ({apiGroups: r.apiGroups || [""], resources: r.resources || [], verbs: r.verbs || [], resourceNames: r.resourceNames}));
    const ns = {clusters: {}, pools: {}, managed: {}, apps: {}, dr: DR_NS()};
    clusters.forEach(c => { ns.clusters[c.id] = c.ns; });
    pools.forEach(p => { ns.pools[p.id] = p.ns; });
    CONTEXTS.forEach(c => { ns.managed[c.name] = nsOfCtx(c.name); });
    const user = who.username || null;
    const initials = String(user || "?").replace(/^system:serviceaccount:[^:]+:/, "").split(/[^a-z0-9]+/i).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join("") || "??";
    return {user, groups: who.groups || [], initials, label: user, rules: {cluster: norm, ns: {}}, incomplete: !!rules.incomplete,
      // no server-side visibility verdicts: the rules decide what is readable
      grants: [], scopes: {ns, clusters: [], managed: [], pools: [], apps: [],
        contexts: CONTEXTS.map(c => ({name: c.name, labels: c.labels, namespaces: c.namespaces}))}, demo_users: []};
  },
  accessScopes: () => api.accessSelf().then(r => r.scopes),
  accessRoles: () => req("/access-roles").then(r => r.map(normRole)),
  accessGrants: () => req("/access-grants"),
  accessGrantCreate: b => send("POST", "/access-grants", b),
  accessGrantDelete: id => send("DELETE", `/access-grants/${id}`),
  accessSubjects: () => req("/access/subjects"),
  accessEffective: subject => req("/access/effective?subject=" + encodeURIComponent(subject)).then(r => r[0]),
  accessReview: b => send("POST", "/access/review", b),
  accessInvariants: () => req("/access/invariants"),
  // a "Kubernetes cluster" is a configured context
  k8sClusters: () => sources.k8sClusters().then(r => r.map(normK8s)),
  k8sCluster: id => sources.k8sCluster(id).then(normK8s),
  k8sStorageClasses: id => sources.storageClasses(id).then(r => r.map(normSc)),
  k8sPvcs: id => sources.pvcs(id).then(r => r.map(normPvc)),
  k8sHosts: id => sources.hosts(id).then(r => r.map(normHost)),
  k8sZones: id => req(`/kubernetes-clusters/${id}/zones`).then(r => r.map(normZone)),
  k8sStorageClusters: id => sources.clusters(id).then(r => r.map(normCluster)),
  clusterK8s: id => sources.k8sCluster(ctxOfId(id)).then(k => [normK8s(k)]),
  storageClass: id => one(sources.storageClass, id, normSc, "StorageClass"),
  storageClassPvcs: id => sources.pvcs(ctxOfId(id), {storageClassId: id}).then(r => r.map(normPvc)),
  pvcs: () => everywhere(sources.pvcs, normPvc),
  pvcResize: (id, size) => pvcGrow(id, size),
  buckets: cid => req(`/clusters/${cid}/buckets`).then(r => r.map(normBucket)),
  bucket: id => req(`/buckets/${id}`).then(r => normBucket(r[0])),
  bucketCreate: (cid, p) => send("POST", `/clusters/${cid}/buckets`, p),
  bucketUpdate: (id, p) => send("PUT", `/buckets/${id}`, p),
  bucketSetAccess: (id, p) => send("PUT", `/buckets/${id}/access`, p),
  bucketResize: (id, size) => send("POST", `/buckets/${id}/resize`, {size}),
  bucketDelete: id => send("DELETE", `/buckets/${id}`),
  bucketSetTags: (id, tags) => send("PUT", `/buckets/${id}/tags`, {tags}),
  bucketReplicate: (id, policyId) => send("POST", `/buckets/${id}/replicate`, {policy_id: policyId}),
  bucketUnreplicate: id => send("POST", `/buckets/${id}/unreplicate`),
  clusterSetFileStorage: (cid, p) => send("PUT", `/clusters/${cid}/file-storage`, p),
  clusterFailoverMds: cid => send("POST", `/clusters/${cid}/file-storage/failover`),
  clusterSetObjectStorage: (cid, p) => send("PUT", `/clusters/${cid}/object-storage`, p),
  pvc: id => one(sources.pvc, id, normPvc, "PVC"),
  zoneK8sClusters: id => req(`/zones/${id}/kubernetes-clusters`).then(r => r.map(normK8s)),
  zones: () => req("/zones").then(r => r.map(normZone)),
  zone: id => req(`/zones/${id}`).then(r => normZone(r[0])),
  zoneHosts: id => req(`/zones/${id}/hosts`).then(r => r.map(normHost)),
  zoneClusters: id => req(`/zones/${id}/clusters`).then(r => r.map(normCluster)),
  clusterZones: cid => req(`/clusters/${cid}/zones`).then(r => r.map(normZone)),

  pairCreate: p => send("POST", "/cluster-pairs", p),
  pairTest: id => send("POST", `/cluster-pairs/${id}/test`),
  pairDelete: id => send("DELETE", `/cluster-pairs/${id}`),
  rpolicyCreate: p => send("POST", "/replication-policies", p),
  rpolicyUpdate: (id, p) => send("PUT", `/replication-policies/${id}`, p),
  rpolicyAddVolumes: (id, ids) => send("POST", `/replication-policies/${id}/lvols`, {lvol_ids: ids}),
  rpolicyRemoveVolume: (id, vid) => send("DELETE", `/replication-policies/${id}/lvols/${vid}`),
  rpolicyFailover: (id, planned) => send("POST", `/replication-policies/${id}/failover`, {planned}),
  rpolicyFailback: id => send("POST", `/replication-policies/${id}/failback`),
  rpolicyTest: id => send("POST", `/replication-policies/${id}/test-failover`),
  rpolicyResync: id => send("POST", `/replication-policies/${id}/resync`),
  rpolicyDelete: id => send("DELETE", `/replication-policies/${id}`),
  clusterSetZones: (cid, ids) => send("PUT", `/clusters/${cid}/zones`, {zone_ids: ids}),
  hostSetPlacement: (id, p) => send("PUT", `/hosts/${id}/placement`, p),
  // ---- discovery + deployment ----
  // A cluster is not created by POSTing a cluster: discovery writes an
  // inventory, a ClusterDeploymentConfig is drafted against it, and approving
  // that document is what deploys.
  // a discovery is an OperatorOps of action Discover in that context
  discoveries: kid => store.read(kid, "OperatorOps")
    .then(r => r.filter(o => (o.spec || {}).action === "Discover").map(o => normDiscovery(discoveryRecord(o, kid)))),
  discoveryRun: (kid, name, filter, nodeSelector) => k8s.create("OperatorOps", {
    apiVersion: apiVersionOf("OperatorOps"), kind: "OperatorOps",
    metadata: {name: `discover-${(name || "cluster").slice(0, 30)}-${Date.now().toString(36)}`, namespace: nsOfCtx(kid)},
    spec: {action: "Discover", discover: Object.assign({},
      filter && Object.keys(filter).length ? {deviceFilter: filter} : {},
      nodeSelector && Object.keys((nodeSelector.matchLabels || nodeSelector) || {}).length ? {nodeSelector: nodeSelector.matchLabels || nodeSelector} : {},
      name ? {configName: name} : {})}
  }, {ctx: kid, namespace: nsOfCtx(kid)}).then(touched(kid, "OperatorOps")),
  deployConfigs: () => Promise.all(CONTEXTS.map(c => store.read(c.name, "ClusterDeploymentConfig").catch(() => [])
    .then(r => r.map(o => normDeployConfig(Object.assign({}, o, {__ctx: c.name})))))).then(rs => rs.flat()),
  k8sDeployConfigs: kid => store.read(kid, "ClusterDeploymentConfig").then(r => r.map(o => normDeployConfig(Object.assign({}, o, {__ctx: kid})))),
  deployConfig: id => api.deployConfigs().then(r => {
    const hit = r.find(c => c.id === id || c.name === id);
    if (!hit) throw new ApiError(404, "deployment config not found", "/" + id, "NotFound");
    return hit;
  }),
  deployConfigCreate: (name, spec, ctx) => k8s.create("ClusterDeploymentConfig", {
    apiVersion: apiVersionOf("ClusterDeploymentConfig"), kind: "ClusterDeploymentConfig",
    metadata: {name, namespace: nsOfCtx(ctx)},
    spec: Object.assign({approved: false}, spec)
  }, {ctx, namespace: nsOfCtx(ctx)}).then(touched(ctx, "ClusterDeploymentConfig")),
  // approval is the only mutable field, and it is one-way
  deployConfigApprove: (name, ctx) => k8s.patch("ClusterDeploymentConfig", name, {spec: {approved: true}}, {ctx, namespace: nsOfCtx(ctx)}).then(touched(ctx, "ClusterDeploymentConfig")),
  // step / node logs: the probe Job pods a discovery run labels with its name
  deployLog: (id, name) => deployLogRead(id, name),
  deployConfigDelete: (name, ctx) => k8s.remove("ClusterDeploymentConfig", name, {ctx, namespace: nsOfCtx(ctx)}).then(touched(ctx, "ClusterDeploymentConfig")),
  clusterCreate: payload => send("POST", "/clusters", payload),

  // ---- mutations ----
  // Every action is an object: an Ops CR for a cluster or node, an edit of
  // the Kubernetes object that owns a volume or a policy. What no API models
  // yet answers "no API action exists" and is listed as a backend gap.
  clusterSuspend: noAction("suspend a cluster"),
  clusterActivate: id => clusterOps(id, "activate"),
  clusterShutdown: id => clusterOps(id, "shutdown"),
  clusterStart: id => clusterOps(id, "start"),
  clusterRestart: id => clusterOps(id, "restart"),
  clusterRollingRestart: (id, refresh) => clusterOps(id, "node-rolling-restart", refresh ? {nodeRollingRestart: {refreshSNodeAPI: true}} : null),
  clusterExpand: id => clusterOps(id, "expand"),
  // a node joins by being listed on the cluster's node set
  clusterAddNode: (id, hostId, failureDomain) => nodeSetAddWorker(id, hostId, failureDomain),
  nodeShutdown: (id, force) => nodeOps(id, "shutdown", force ? {force: true} : null),
  nodeRestart: id => nodeOps(id, "restart"),
  nodeSuspend: id => nodeOps(id, "suspend"),
  nodeResume: id => nodeOps(id, "resume"),
  nodeRemove: id => nodeOps(id, "remove"),
  nodeMigrate: (id, hostId) => nodeOps(id, "migrate", {targetWorkerNode: (REG[hostId] || {}).hostname || hostId}),
  nodeAddDevice: noAction("add a device to a node"),
  deviceRestart: noAction("restart a device"),
  deviceFail: noAction("fail a device"),
  deviceHealthCheck: noAction("run a device health check"),
  deviceRemove: noAction("remove a device"),
  hostReserveDevice: noAction("reserve a device"),
  hostsPrepare: noAction("prepare a host"),
  hostConfigure: noAction("configure a host"),
  // a volume is its claim: deleting the PVC releases the volume
  volumeDelete: id => withVolume(id, v => k8s.remove("PersistentVolumeClaim", v.pvc.name, {ctx: v.ctx, namespace: v.pvc.namespace})
    .then(touched(v.ctx, "PersistentVolumeClaim"))),
  volumeResize: (id, size) => withVolume(id, v => pvcGrow(v.pvc.uuid, size, v)),
  volumeSnapshot: (id, name) => withVolume(id, v => snapshotCreate(v, name)),
  volumeClone: (id, name) => withVolume(id, v => pvcClone(v, name, {kind: "PersistentVolumeClaim", name: v.pvc.name})),
  // a migration moves the volume's backing logical volume to another node
  volumeMigrate: (id, p) => withVolume(id, v => submitOps("PersistentVolume", v.pvName, "migrate",
    {targetNodeUUID: p.targetNodeUUID || p.target_node_id || p.node_id || p.host_id}, {ctx: v.ctx, namespace: v.k8s && REG[v.clusterId] ? REG[v.clusterId].ns : undefined})
    .then(touched(v.ctx, "VolumeMigration"))),
  volumeRebalance: noAction("rebalance a volume"),
  migrationTasks: cid => api.tasks(cid).then(ts => ts.filter(t => t.fn === "lvol_migration")),
  volumeSetAffinity: noAction("set volume affinity"),
  clusterRebalance: noAction("trigger a cluster rebalance"),
  clusterSetRebalance: (id, enabled) => withCluster(id, c => k8s.patch("StorageCluster", c.k8s.name, {spec: {volumeAutoPlacement: {enabled: !!enabled}}}, {ctx: c.ctx, namespace: c.ns})
    .then(touched(c.ctx, "StorageCluster"))),
  volumeBackup: (id, payload) => withVolume(id, v => backupCreate(v, payload)),
  snapshotBackup: noAction("back up a snapshot"),
  backupMerge: noAction("merge a backup"),
  backupMergeVersion: noAction("merge a backup version"),
  policyCreate: (cid, p) => withCluster(cid, c => policyCreate(c, p)),
  policyUpdate: noAction("change a backup policy (its fields are immutable; create a new one)"),
  policyDelete: id => withVm(id, p => k8s.remove("BackupPolicy", p.k8s.name, {ctx: p.ctx, namespace: p.ns}).then(touched(p.ctx, "BackupPolicy"))),
  // membership is a PVC annotation
  volumeSetPolicy: (id, policyId) => withVolume(id, v => k8s.patch("PersistentVolumeClaim", v.pvc.name,
    {metadata: {annotations: {[LABELS.backupPolicy]: policyId ? ((REG[policyId] || {}).name || policyId) : null}}}, {ctx: v.ctx, namespace: v.pvc.namespace})
    .then(touched(v.ctx, "PersistentVolumeClaim"))),
  volumeSetQos: noAction("change a volume's QoS"),
  volumeSetDataReduction: noAction("change data reduction"),
  poolSetQos: (id, qos) => withVm(id, p => k8s.patch("StoragePool", p.k8s.name, {spec: {qos: poolQosSpec(qos)}}, {ctx: p.ctx, namespace: p.ns}).then(touched(p.ctx, "StoragePool"))),
  poolCreate: (cid, p) => withCluster(cid, c => poolCreate(c, p)),
  poolEnable: noAction("enable a pool"),
  poolDisable: noAction("disable a pool"),
  snapshotDelete: id => withVm(id, s => k8s.remove("VolumeSnapshot", s.k8s.name, {ctx: s.ctx, namespace: s.ns}).then(touched(s.ctx, "VolumeSnapshot"))),
  snapshotClone: (id, name) => withVm(id, s => api.volume(s.volumeId).then(v => pvcClone(v, name, {kind: "VolumeSnapshot", apiGroup: "snapshot.storage.k8s.io", name: s.k8s.name}))),
  backupRestore: (id, name) => withVm(id, b => restoreCreate(b, name)),
  backupExport: noAction("export a backup"),
  backupDelete: id => withVm(id, b => k8s.remove("StorageBackup", b.k8s.name, {ctx: b.ctx, namespace: b.ns}).then(touched(b.ctx, "StorageBackup")))
};

// ---- action helpers ----------------------------------------------------------
// a function declaration, because the api object calls it while it is built
function noAction(what) { return () => Promise.reject(new ApiError(501, `${what}: no API action exists for this yet`, "", "NotImplemented")); }
const notFound = (what, id) => new ApiError(404, `${what} ${id} is not known to the console`, "/" + id, "NotFound");
const withVm = (id, fn) => { const vm = REG[id]; return vm && vm.k8s ? Promise.resolve(fn(vm)) : Promise.reject(notFound("object", id)); };
const withCluster = (id, fn) => (REG[id] && REG[id].k8s ? Promise.resolve(REG[id]) : api.cluster(id)).then(fn);
const withVolume = (id, fn) => (REG[id] && REG[id].k8s ? Promise.resolve(REG[id]) : api.volume(id)).then(v => {
  if (!v.pvc) throw new ApiError(409, "this volume has no claim; manage it where it was created", "/" + id, "Conflict");
  return fn(v);
});
const clusterOps = (id, action, payload) => withCluster(id, c => submitOps("StorageCluster", c.k8s.name, action, payload, {ctx: c.ctx, namespace: c.ns})
  .then(touched(c.ctx, "StorageClusterOps")));
const nodeOps = (id, action, payload) => (REG[id] && REG[id].k8s ? Promise.resolve(REG[id]) : api.node(id))
  .then(n => submitOps("StorageNode", n.k8s.name, action, payload, {ctx: n.ctx, namespace: n.ns}).then(touched(n.ctx, "StorageNodeOps")));
const pvcGrow = (pvcId, size, vol) => {
  const pvc = REG[pvcId] || null;
  const ns = pvc ? pvc.namespace : vol.pvc.namespace, name = pvc ? pvc.name : vol.pvc.name, ctx = pvc ? pvc.ctx : vol.ctx;
  return k8s.patch("PersistentVolumeClaim", name, {spec: {resources: {requests: {storage: String(Math.round(Number(size)))}}}}, {ctx, namespace: ns})
    .then(touched(ctx, "PersistentVolumeClaim"));
};
const snapshotClassFor = ctx => store.read(ctx, "VolumeSnapshotClass").catch(() => [])
  .then(cs => (cs.find(c => c.driver === "csi.simplyblock.io") || {}).metadata).then(m => (m ? m.name : null));
const snapshotCreate = (v, name) => snapshotClassFor(v.ctx).then(cls => k8s.create("VolumeSnapshot", {
  apiVersion: "snapshot.storage.k8s.io/v1", kind: "VolumeSnapshot",
  metadata: {name: name || `${v.pvc.name}-${Date.now().toString(36)}`, namespace: v.pvc.namespace},
  spec: Object.assign({source: {persistentVolumeClaimName: v.pvc.name}}, cls ? {volumeSnapshotClassName: cls} : {})
}, {ctx: v.ctx, namespace: v.pvc.namespace})).then(touched(v.ctx, "VolumeSnapshot"));
const pvcClone = (v, name, dataSource) => k8s.create("PersistentVolumeClaim", {
  apiVersion: "v1", kind: "PersistentVolumeClaim",
  metadata: {name: name || `${v.pvc.name}-clone-${Date.now().toString(36)}`, namespace: v.pvc.namespace},
  spec: {storageClassName: v.storageClass, accessModes: ["ReadWriteOnce"], resources: {requests: {storage: String(v.capacity.total)}}, dataSource}
}, {ctx: v.ctx, namespace: v.pvc.namespace}).then(touched(v.ctx, "PersistentVolumeClaim"));
const backupCreate = (v, payload) => k8s.create("StorageBackup", {
  apiVersion: apiVersionOf("StorageBackup"), kind: "StorageBackup",
  metadata: {name: `${v.pvc.name}-${Date.now().toString(36)}`.slice(0, 63), namespace: v.pvc.namespace},
  spec: Object.assign({clusterName: v.clusterName || (REG[v.clusterId] || {}).name, pvcRef: {name: v.pvc.name, namespace: v.pvc.namespace}},
    payload && payload.snapshot_name ? {snapshotName: payload.snapshot_name} : {})
}, {ctx: v.ctx, namespace: v.pvc.namespace}).then(touched(v.ctx, "StorageBackup"));
const restoreCreate = (b, name) => k8s.create("BackupRestore", {
  apiVersion: apiVersionOf("BackupRestore"), kind: "BackupRestore",
  metadata: {name: `restore-${(name || b.crName).slice(0, 40)}-${Date.now().toString(36)}`.slice(0, 63), namespace: b.ns},
  spec: {clusterName: b.clusterName, backupRef: {name: b.crName},
    pvcTemplate: {metadata: {name: name || `${b.pvc.name}-restored`}, spec: {accessModes: ["ReadWriteOnce"], resources: {requests: {storage: String(b.capacity.total || b.size || 0)}}}}}
}, {ctx: b.ctx, namespace: b.ns}).then(touched(b.ctx, "BackupRestore"));
const scheduleString = s => Array.isArray(s) ? s.map(r => `${r.interval},${r.versions}`).join(" ") : String(s || "");
const policyCreate = (c, p) => k8s.create("BackupPolicy", {
  apiVersion: apiVersionOf("BackupPolicy"), kind: "BackupPolicy",
  metadata: {name: p.name || p.policy_name, namespace: c.ns},
  spec: Object.assign({clusterName: c.k8s.name, schedule: scheduleString(p.schedule)},
    p.max_versions || p.maxVersions ? {maxVersions: Number(p.max_versions || p.maxVersions)} : {},
    p.max_age || p.maxAge ? {maxAge: p.max_age || p.maxAge} : {})
}, {ctx: c.ctx, namespace: c.ns}).then(touched(c.ctx, "BackupPolicy"));
const poolQosSpec = q => Object.assign({}, q.rw_iops || q.iops ? {iops: Number(q.rw_iops || q.iops)} : {},
  {throughput: Object.assign({}, q.rw_mbytes ? {readWrite: Number(q.rw_mbytes)} : {}, q.r_mbytes ? {read: Number(q.r_mbytes)} : {}, q.w_mbytes ? {write: Number(q.w_mbytes)} : {})});
const poolCreate = (c, p) => k8s.create("StoragePool", {
  apiVersion: apiVersionOf("StoragePool"), kind: "StoragePool",
  metadata: {name: p.name || p.pool_name, namespace: c.ns},
  spec: Object.assign({clusterName: c.k8s.name},
    p.size_prov || p.capacity_limit ? {capacityLimit: String(p.size_prov || p.capacity_limit)} : {},
    p.qos ? {qos: poolQosSpec(p.qos)} : {},
    p.storage_class_parameters ? {storageClassParameters: p.storage_class_parameters} : {})
}, {ctx: c.ctx, namespace: c.ns}).then(touched(c.ctx, "StoragePool"));
// adding a worker to the cluster's first node set is how a node is added
const nodeSetAddWorker = (clusterId, hostId, failureDomain) => withCluster(clusterId, async c => {
  const sets = (await store.read(c.ctx, "StorageNodeSet")).filter(s => (s.spec || {}).clusterName === c.k8s.name);
  if (!sets.length) throw new ApiError(409, "the cluster has no StorageNodeSet to add the worker to", "/" + clusterId, "Conflict");
  const set = sets[0], worker = (REG[hostId] || {}).hostname || hostId;
  const workers = Array.from(new Set(((set.spec || {}).workerNodes || []).concat([worker])));
  const patch = {spec: Object.assign({workerNodes: workers}, failureDomain !== undefined && failureDomain !== null && failureDomain !== ""
    ? {nodeFailureDomains: Object.assign({}, (set.spec || {}).nodeFailureDomains || {}, {[worker]: Number(failureDomain)})} : {})};
  return k8s.patch("StorageNodeSet", set.metadata.name, patch, {ctx: c.ctx, namespace: set.metadata.namespace}).then(touched(c.ctx, "StorageNodeSet"));
});
// a discovery run, in the record shape normDiscovery reads
const discoveryRecord = (o, ctx) => {
  const sp = (o.spec || {}).discover || {}, st = o.status || {}, step = st.step || {};
  return {uuid: o.metadata.uid, k8s_cluster_id: ctx, status: String(st.phase || "Pending").toLowerCase(), step: step.state || null, op_name: o.metadata.name,
    started_at: st.startedAt || o.metadata.creationTimestamp, finished_at: st.completedAt || null,
    node_selector: {matchLabels: sp.nodeSelector || {}}, device_filter: sp.deviceFilter || {}, host_ids: st.workers || [],
    node_count: (st.workers || []).length, device_count: null, filtered_count: null, config_ref: st.configRef || null, message: st.message || null};
};
// the probe Job pods a discovery run labels with its name, read as one log
const deployLogRead = async (id, name) => {
  const d = REG[id] || {};
  const ctx = d.ctx || LOCAL_CTX, ns = d.ns || nsOfCtx(ctx);
  const pods = await k8s.list("Pod", {ctx, namespace: ns, labelSelector: `storage.simplyblock.io/nodeprobe-run=${name}`}).catch(() => []);
  const logs = await Promise.all(pods.map(p => k8s.logs(p.metadata.name, {ctx, namespace: ns, tailLines: 500, timestamps: true}).catch(() => "")));
  return logs.flatMap((text, i) => String(text || "").split("\n").filter(Boolean).map(line => {
    const sp = line.indexOf(" ");
    return {ts: sp > 0 ? line.slice(0, sp) : null, level: /error|fail/i.test(line) ? "error" : "info", node: pods[i].spec.nodeName || null, msg: sp > 0 ? line.slice(sp + 1) : line};
  }));
};
const GETTER = {plan: api.plan, site: api.site, cluster: api.cluster, host: api.host, node: api.node, device: api.device, pool: api.pool,
  volume: api.volume, snapshot: api.snapshot, backup: api.backup, policy: api.policy,
  pair: api.pair, rpolicy: api.rpolicy, slot: api.slot, replops: api.replOp, zone: api.zone, cgroup: api.cgroup, cgsnapshot: api.cgSnapshot,
  migration: api.migration, k8sc: api.k8sCluster, storageclass: api.storageClass, pvc: api.pvc,
  drcluster: api.drCluster, protectedapp: api.protectedApp,
  bucket: api.bucket, deployconfig: api.deployConfig, mpath: api.mpath, appgroup: api.appGroup};
const resolve = (kind, id) => REG[id] ? Promise.resolve(REG[id]) : (GETTER[kind] ? GETTER[kind](id).catch(() => null) : Promise.resolve(null));
// the state of every context's reads, for the "context unreachable" banners
const contextErrors = () => CTX_ERRORS;

// ---- data hooks ------------------------------------------------------------
function useResource(key, loader, pollMs) {
  const ref = React.useRef(loader); ref.current = loader;
  const [s, setS] = React.useState({loading: true, data: null, error: null});
  const load = React.useCallback(async quiet => {
    if (!quiet) setS(p => ({loading: true, data: null, error: null}));
    try { const d = await ref.current(); setS({loading: false, data: d, error: null}); }
    catch (e) { setS(p => quiet ? p : {loading: false, data: null, error: e}); }
  }, [key]);
  React.useEffect(() => { load(false); }, [key]);
  React.useEffect(() => {
    if (!pollMs) return;
    const i = setInterval(() => load(true), pollMs);
    return () => clearInterval(i);
  }, [key, pollMs]);
  return {...s, reload: () => load(false)};
}

Object.assign(window, {api, API, ApiError, STATUS_META, REG, GETTER, resolve, useResource, contextErrors, ctxOfId, nsOfCtx, OPS_KINDS, OPS_STEPS, TASK_INDEX,
  normCluster, normHost, normNode, normDevice, normPool, normVolume, normSnapshot, normBackup, normPolicy,
  normTask, normLog, normAlert, normOperation, OPS_TARGET_LABEL, normPlan, normSite, normMethod, normLeg, normGeneration,
  normReplPair, normReplPolicy, normReplSlot, normReplOps, ivMinutes, normPair, normDrPolicy, normZone, normCg, normCgSnap, normMigration, normK8s, normSc, normPvc, normBucket, normDrCluster, normApp});
