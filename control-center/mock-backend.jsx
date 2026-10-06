// ---------------------------------------------------------------------------
// MOCK BACKEND — in-memory fixture store shaped like control plane API v2
// payloads (snake_case, flat collections, uuid foreign keys).
// Nothing in the UI reads this file: it is only served by mock-api.jsx.
// ---------------------------------------------------------------------------
const rnd = (s => () => (s = (s * 1664525 + 1013904223) >>> 0) / 4294967296)(20260808);
const pick = a => a[Math.floor(rnd() * a.length)];
const int = (a, b) => a + Math.floor(rnd() * (b - a + 1));
const hex = n => Array.from({length: n}, () => "0123456789abcdef"[Math.floor(rnd() * 16)]).join("");
const uuid = () => `${hex(8)}-${hex(4)}-4${hex(3)}-a${hex(3)}-${hex(12)}`;
const GB = 1e9, TB = 1e12;
const series = (base, jit) => Array.from({length: 28}, () => Math.max(0, Math.round(base * (1 + (rnd() - .5) * jit))));
// Single reference clock. ago() and every freshness/RPO check must use this and
// nothing else — two anchors is how the RPO metric silently broke before.
const NOW_MS = Date.parse("2026-09-01T09:00:00Z");
const ago = h => new Date(NOW_MS - h * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");

const DB = {clusters: [], hosts: [], storage_nodes: [], devices: [], pools: [], lvols: [], snapshots: [],
  backups: [], backup_policies: [], consistency_groups: [], cg_snapshots: [], migrations: [],
  k8s_clusters: [], storage_classes: [], pvcs: [], buckets: [],
  dr_clusters: [], protected_apps: []};

const FD = ["rack-a1", "rack-a2", "rack-b1", "rack-b2", "az-1", "az-2", "az-3"];
const PHYS = ["dell-r750-01", "dell-r750-02", "smc-2124us", "hpe-dl385", "gigabyte-r282", null];
const MODELS = [["KIOXIA CM7-R", 7.68 * TB], ["SAMSUNG PM9A3", 3.84 * TB], ["INTEL SSDPF2KX064T1", 6.4 * TB], ["MICRON 7450 PRO", 15.36 * TB]];
const POOL_NAMES = ["default", "gold-tier", "silver-tier", "bronze-tier", "analytics", "db-prod", "ci-scratch"];
const VOL_PREFIX = ["pg", "mysql", "kafka", "etcd", "minio", "redis", "elastic", "vm", "registry", "clickhouse", "grafana"];
const BUCKETS = ["s3://sb-backup-eu/", "s3://sb-backup-us/", "s3://sb-archive-cold/"];

// Provider-shaped KMS records — a Vault mount means nothing to AWS KMS.
function kmsFor(provider, name, i) {
  const common = {provider, key_name: `sb-${name}-dek`, key_type: "aes256-gcm96",
    verify_tls: true, status: rnd() > .12 ? "connected" : pick(["unreachable", "sealed"]),
    last_check_at: ago(rnd() * 2), keys_in_use: 0, rotation_days: pick([30, 90, 180, 0])};
  const region = pick(["eu-central-1", "us-east-2", "eu-north-1"]);
  if (provider === "aws_kms") return Object.assign(common, {
    address: `https://kms.${region}.amazonaws.com`, region,
    auth_method: pick(["irsa", "irsa", "access_key"]),
    auth_role: `arn:aws:iam::${int(100000000000, 999999999999)}:role/simplyblock-kms`});
  if (provider === "azure_key_vault") return Object.assign(common, {
    address: `https://sb-${name.split("-")[0]}-kv.vault.azure.net`,
    auth_method: pick(["workload_identity", "workload_identity", "client_secret"]),
    auth_role: `${hex(8)}-${hex(4)}-${hex(4)}-${hex(4)}-${hex(12)}`});
  if (provider === "gcp_kms") return Object.assign(common, {
    address: `projects/sb-${name.split("-")[0]}/locations/${region}/keyRings/simplyblock`,
    auth_method: "workload_identity"});
  if (provider === "kmip") return Object.assign(common, {
    address: `kmip-${i + 1}.simplyblock.internal:5696`,
    auth_method: "certificate", auth_role: "sb-kmip-client"});
  return Object.assign(common, {
    address: `https://vault-${i + 1}.simplyblock.internal:8200`,
    namespace: rnd() > .6 ? "admin/storage" : null,
    auth_method: pick(["kubernetes", "kubernetes", "approle", "token"]),
    auth_role: "simplyblock-storage", mount_path: "transit"});
}

// Data is striped across every device in a cluster, so devices fill at close to
// the same rate. One fill ratio per cluster, each device within a couple of
// percent of it — never independently random, which read as a broken cluster.
// A storage cluster is built from one hardware SKU: every host has the same
// socket count, CPU and memory, and carries identical drives. Heterogeneous
// nodes read as a broken or half-migrated cluster, and they make the capacity
// and IOPS columns look wrong even when they are right.
const CLUSTER_SKU = new Map();
const sku = cid => {
  if (!CLUSTER_SKU.has(cid)) {
    const [model, size] = pick(MODELS);
    const sockets = pick([1, 2, 2]);
    CLUSTER_SKU.set(cid, {
      sockets, model, size,
      nvmePerSocket: int(2, 5),
      blockCount: int(0, 3),
      vcpu: pick([32, 48, 64, 96, 128]),
      memory: pick([128, 256, 384, 512]) * GB,
      hugepages: pick([64, 96, 128]) * GB,
      vcpuReserved: pick([8, 12, 15, 16, 24]),
      maxSubsys: pick([100, 100, 128, 256]),
      memReservedFrac: pick([.5, .6, .75]),
      // one I/O profile for the whole cluster: work is spread evenly, so every
      // device answers at roughly the same rate
      devIops: int(9000, 145000), devBwPerIo: int(3800, 9200)
    });
  }
  return CLUSTER_SKU.get(cid);
};
// jitter one cluster-wide figure by a couple of percent
const near = (base, spread) => Math.round(base * (1 + (rnd() - .5) * (spread === undefined ? .06 : spread)));

const CLUSTER_FILL = new Map();
const devFill = cid => {
  if (!CLUSTER_FILL.has(cid)) CLUSTER_FILL.set(cid, .2 + rnd() * .62);
  const base = CLUSTER_FILL.get(cid);
  return Math.min(.97, Math.max(.02, base * (1 + (rnd() - .5) * .05)));
};

const ioStats = (io, bw) => ({read_io_ps: Math.round(io * .6), write_io_ps: Math.round(io * .4), read_bytes_ps: Math.round(bw * .6), write_bytes_ps: Math.round(bw * .4)});
const qosOf = force => (!force && rnd() > .55) ? null : {
  rw_ios_per_sec: pick([0, 25000, 50000, 100000, 200000]),
  rw_mbytes_per_sec: pick([0, 500, 1000, 2000]),
  r_mbytes_per_sec: pick([0, 0, 1500]),
  w_mbytes_per_sec: pick([0, 0, 800])
};

// ---- hosts -----------------------------------------------------------------
function seedHost(c, idx, unassigned) {
  const S = sku(c.uuid);
  const sockets = S.sockets;
  const status = unassigned ? "available" : (rnd() > .9 ? "unreachable" : "available");
  const h = {
    uuid: uuid(), cluster_id: c.uuid,
    hostname: `${c.name.split("-").slice(0, 2).join("-")}-host-${String(idx + 1).padStart(2, "0")}`,
    mgmt_ip: `172.20.${int(0, 40)}.${int(2, 250)}`,
    status,
    numa_sockets: sockets,
    control_plane: rnd() > .55,
    vcpu_count: S.vcpu,
    memory_total: S.memory,
    hugepages_reserved: S.hugepages,
    hugepages_allocated: 0,
    prepared_at: ago(int(2, 900)),
    zone_id: null, zone: null, region: null, k8s_cluster: null,
    // operators taint hosts to mark them as migration targets
    migration_taint: null,
    // worker nodes may or may not be tainted with zone / rack / cabinet
    rack_id: rnd() > .18 ? `r${String(int(1, 18)).padStart(2, "0")}` : null,
    cabinet_id: rnd() > .3 ? `c${String(int(1, 6)).padStart(2, "0")}` : null,
    labels: {"simplyblock.io/storage-node": "true", "topology.kubernetes.io/zone": pick(FD)},
    devices: [], storage_node_ids: []
  };
  const nvmePerSocket = S.nvmePerSocket;
  const model = S.model, size = S.size;
  for (let s = 0; s < sockets; s++) {
    for (let i = 0; i < nvmePerSocket; i++) {
      h.devices.push({id: uuid(), kind: "nvme", numa_socket: s,
        pcie_address: `0000:${hex(2)}:0${i}.0`, device_name: `/dev/nvme${s * nvmePerSocket + i}n1`,
        serial_number: `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        model_number: `${model} ${(size / TB).toFixed(2)}TB`, size,
        assigned_node_id: null});
    }
  }
  for (let i = 0; i < S.blockCount; i++) {
    h.devices.push({id: uuid(), kind: "block", numa_socket: 0, pcie_address: null,
      device_name: `/dev/sd${"bcdef"[i]}`, serial_number: null, model_number: "VIRTUAL-BLOCK",
      size: S.size, assigned_node_id: null});
  }
  DB.hosts.push(h);
  h.host_class = h.devices.some(d => d.kind === "nvme") ? "nvme" : "non-nvme";
  return h;
}

function seedCluster(name, device_class, status, location_type, i) {
  const edge = location_type === "edge";
  const c = {
    uuid: uuid(), name, device_class, status, location_type,
    // edge clusters neither rebalance nor run a task engine
    rebalancing: edge ? false : rnd() > .68,
    ha_type: pick(["ha", "ha", "single"]), distr_npcs: pick([1, 1, 2]), distr_ndcs: pick([2, 4]),
    blk_size: 4096, page_size_in_blocks: 2097152,
    cluster_version: pick(["26.2.1", "26.2.0", "26.1.2"]),
    capabilities: {
      snapshot_replication: true,
      async_replication: true,
      rebalancing: !edge,
      tasks: !edge
    },
    // not every cluster qualifies as a DR target (capacity, licence, siting)
    dr_target_eligible: location_type !== "edge" && status !== "unready",
    // backups require a complete S3 endpoint configuration
    backup_enabled: !edge,
    s3: edge ? null : {
      endpoint: `https://s3.${location_type === "edge" ? "ap-southeast-1" : pick(["eu-central-1", "us-east-2", "eu-north-1"])}.amazonaws.com`,
      region: pick(["eu-central-1", "us-east-2", "eu-north-1"]),
      bucket: `sb-backup-${name}`,
      path_prefix: `clusters/${name}/`,
      access_key_id: `AKIA${hex(8).toUpperCase()}`,
      secret_access_key: "••••••••••••••••••••",
      verify_tls: true, addressing: pick(["virtual-hosted", "path"])
    },
    // failure domains: a cluster picks one granularity for the whole cluster.
    // Both this and sync_replication_enabled are fixed at creation.
    failure_domain_enabled: !edge,
    failure_domain_scope: edge ? null : pick(["rack", "rack", "cabinet", "zone"]),
    sync_replication_enabled: false,
    // ---- file storage: pNFS over the cluster's block capacity -------------
    // Every worker becomes a pNFS data client; the kernel NFS server on one
    // control-plane worker serves metadata only and can restart elsewhere,
    // because its configuration and backend are shared.
    file_storage: (!edge && rnd() > .45) ? {
      enabled: true, nfs_version: "4.2", layout_type: "flexfile",
      export_root: "/export/simplyblock",
      mds_host: null, mds_candidates: [],
      mds_state: rnd() > .12 ? "active" : pick(["restarting", "electing"]),
      mds_restarted_at: rnd() > .6 ? ago(int(1, 400)) : null,
      lease_seconds: pick([10, 20, 30]),
      grace_seconds: pick([15, 45, 90]),
      failover_budget_seconds: pick([5, 8, 12]),
      // pNFS on the Linux kernel NFS server only supports XFS
      filesystem: "xfs", max_exports: pick([64, 128, 256])
    } : {enabled: false},
    // ---- S3: blobs on cluster capacity, metadata in FoundationDB -----------
    object_storage: (!edge && rnd() > .45) ? {
      enabled: true,
      endpoint: `https://s3.${name}.simplyblock.internal`,
      region: pick(["eu-central-1", "us-east-2", "eu-north-1"]),
      addressing: pick(["virtual-hosted", "path"]),
      metadata_backend: "foundationdb",
      versioning_default: rnd() > .5, max_buckets: pick([100, 500, 1000])
    } : {enabled: false},
    // Multipathing: with two data NICs per storage node the client gets two
    // paths and uses them automatically. Fixed at creation, because it decides
    // how many NICs every node must be given.
    multipathing_enabled: !edge && rnd() > .35,
    // Instant volume migration (26.3.0): the logical volume's primary moves
    // between nodes without copying data. Rebalancing uses the same mechanism.
    auto_rebalance: {enabled: !edge && rnd() > .4},
    // node affinity is chosen at cluster creation
    node_affinity: edge ? "none" : pick(["none", "soft", "soft", "strict"]),
    // front storage follows the workload pod when it is rescheduled
    pod_affinity_enabled: rnd() > .45,
    // External key management: the data encryption key of every encrypted
    // volume and bucket is wrapped by a key held in the operator's KMS. Each
    // provider carries its own connection shape.
    kms: edge ? null : kmsFor(pick(["hashicorp_vault", "hashicorp_vault", "aws_kms", "azure_key_vault", "kmip"]), name, i),
    // on the edge the control plane is the Kubernetes API itself
    mgmt_endpoint: edge ? `https://k8s-api.${name}.local:6443` : `https://cp-${i + 1}.simplyblock.internal:5000`,
    mgmt_endpoint_kind: edge ? "Kubernetes API" : "control plane API",
    created_at: `2026-0${int(1, 7)}-${int(10, 28)}T0${int(1, 9)}:${int(10, 59)}:00Z`
  };
  DB.clusters.push(c);

  const nodeCount = int(5, 9);
  const hostCount = nodeCount + int(1, 2);            // spare, prepared hosts
  const hosts = Array.from({length: hostCount}, (_, hi) => seedHost(c, hi, hi >= nodeCount));

  // Redundancy decides how many nodes may be lost: distr_npcs parity chunks
  // (1 or 2) — or, with failure domains, everything inside one domain. Within
  // that budget the cluster is degraded; beyond it, it suspends itself. The
  // fixture picks a node set that lands exactly on the intended state.
  const budget = c.distr_npcs;
  const downCount = status === "online" ? 0
    : status === "degraded" ? int(1, budget)
    : status === "suspended" ? nodeCount : 0;
  const downIdx = new Set();
  while (downIdx.size < downCount) downIdx.add(int(0, nodeCount - 1));
  for (let ni = 0; ni < nodeCount; ni++) {
    const host = hosts[ni];
    if (status === "online" || status === "degraded") host.status = "available";
    const nstatus = status === "in_activation" ? pick(["in_restart", "online", "offline"])
      : downIdx.has(ni) ? (status === "suspended" ? pick(["offline", "offline", "down"]) : pick(["down", "unreachable", "offline", "in_restart"]))
      : "online";
    const n = {
      uuid: uuid(), cluster_id: c.uuid, host_id: host.uuid,
      hostname: `${name}-stor-${String(ni + 1).padStart(2, "0")}`,
      data_nics: [],
      mgmt_ip: host.mgmt_ip,
      failure_domain: null,
      physical_label: pick(PHYS),
      status: nstatus,
      cpu_count: Math.round(host.vcpu_count / 2),
      vcpu_reserved: sku(c.uuid).vcpuReserved,
      max_subsystem_count: sku(c.uuid).maxSubsys,
      memory_total: host.memory_total / 2,
      memory_reserved: Math.round(host.memory_total / 2 * sku(c.uuid).memReservedFrac),
      memory_used: near(host.memory_total / 2 * .55, .18),
      hugepages_total: host.hugepages_reserved,
      hugepages_used: near(host.hugepages_reserved * .72, .1),
      spdk_version: `v24.0${int(1, 9)}`,
      // state the alert rules evaluate. metadata lives in its own arena, so it
      // has its own capacity and its own compaction job
      meta_size_total: 64e9,
      meta_size_util: near(64e9 * (.35 + (CLUSTER_FILL.get(c.uuid) || .4) * .4), .1),
      meta_compaction_at: ago(int(1, 40) / 24),
      meta_compaction_failed: false,
      objects_used: near(30000, .2), objects_max: 65536,
      latency_us: near(320, .18),
      // set when the node was stopped on purpose, so the rules can tell a
      // maintenance shutdown from a failure
      maintenance: false
    };
    n.data_nics = Array.from({length: c.multipathing_enabled ? 2 : 1}, (_, k) => ({
      name: k === 0 ? "ens1f0" : "ens1f1",
      ip: `10.${int(10, 60)}.${int(0, 40)}.${int(2, 250)}`, port: 4420 + k,
      numa_socket: k, state: "up"
    }));
    DB.storage_nodes.push(n);
    host.storage_node_ids.push(n.uuid);
    host.hugepages_allocated += host.hugepages_reserved;

    // claim a slice of the host's devices for this node
    const claimable = host.devices.filter(d => !d.assigned_node_id && (device_class === "nvme" ? d.kind === "nvme" : true));
    // every node in the cluster ends up with the same device count: the hosts
    // are identical, so the claim is a fixed fraction of an identical pool
    const take = Math.max(3, Math.floor(claimable.length * .75));
    claimable.slice(0, take).forEach((hd, di) => {
      hd.assigned_node_id = n.uuid;
      const dstatus = nstatus === "online"
        ? pick(["online", "online", "online", "online", "online", "online", "read_only", "unavailable", "new"])
        : pick(["unavailable", "unavailable", "online", "removed"]);
      const live = dstatus === "online" || dstatus === "read_only";
      const S = sku(c.uuid);
      const io = live ? near(S.devIops) : 0, bw = live ? Math.round(io * near(S.devBwPerIo, .04)) : 0;
      DB.devices.push({
        uuid: uuid(), node_id: n.uuid, cluster_id: c.uuid, host_id: host.uuid,
        cluster_device_class: device_class, numa_socket: hd.numa_socket,
        serial_number: hd.serial_number || `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        pcie_address: hd.pcie_address, device_name: hd.device_name,
        model_number: hd.model_number, firmware_revision: `GXA7${int(10, 99)}1`,
        status: dstatus, health_check: live ? pick(["good", "good", "good", "good", "warn", "critical"]) : null,
        size_total: hd.size, size_util: live ? Math.round(hd.size * devFill(c.uuid)) : 0,
        temperature_c: near(46, .3), percentage_used: near(14, .8), power_on_hours: near(16000, .25),
        io_stats: ioStats(io, bw), io_history: {iops: series(io, .3), bytes: series(bw, .3)}
      });
    });
  }

  // ---- backup policies ----
  // A schedule row is: interval · how many backup versions are retained ·
  // (optionally) how many snapshots of that tier stay online. The interval is
  // both the snapshot/backup periodicity AND, once the version count is
  // exceeded, the merge cadence for that tier.
  const POLICIES = [
    {policy_name: "5m-tiered", schedule: [{interval: "5m", versions: 12, online: 3}, {interval: "1h", versions: 11}, {interval: "1d", versions: 6}]},
    {policy_name: "hourly-24h", schedule: [{interval: "1h", versions: 24, online: 2}, {interval: "1d", versions: 7}]},
    {policy_name: "daily-30d", schedule: [{interval: "1d", versions: 30, online: 1}]},
    {policy_name: "15m-longhaul", schedule: [{interval: "15m", versions: 8, online: 4}, {interval: "6h", versions: 12}, {interval: "1d", versions: 14}, {interval: "7d", versions: 8}]},
    // group-consistent: every cycle snapshots all linked volumes as one consistency group,
    // and the whole retention is kept — the ransomware recovery basis
    {policy_name: "ransomware-cg-1h", consistency_group: true, schedule: [{interval: "1h", versions: 24, online: 2}, {interval: "1d", versions: 14}, {interval: "7d", versions: 8}]}
  ];
  const clusterPolicies = POLICIES.slice(0, int(2, 4)).concat([POLICIES[4]]).map(p => {
    const rec = Object.assign({uuid: uuid(), cluster_id: c.uuid, created_at: ago(int(200, 2000))}, p);
    DB.backup_policies.push(rec); return rec;
  });

  // ---- pools, volumes, snapshots, backups ----
  const poolCount = int(2, 5);
  for (let pi = 0; pi < poolCount; pi++) {
    // a pool is never offline; it is enabled or disabled. Disabled pools keep
    // serving their volumes but refuse new provisioning.
    const p = {uuid: uuid(), cluster_id: c.uuid, pool_name: POOL_NAMES[pi % POOL_NAMES.length],
      // bi-directional DH-CHAP is set when the pool is created and never changes
      dhchap_bidirectional: rnd() > .6, enabled: rnd() > .15, qos: qosOf(pi === 0)};
    DB.pools.push(p);
    const clusterNodes = DB.storage_nodes.filter(n => n.cluster_id === c.uuid);
    const volCount = int(3, 8);
    for (let vi = 0; vi < volCount; vi++) {
      const vstatus = rnd() > .1 ? "online" : "offline";
      const live = vstatus === "online";
      const prov = pick([100 * GB, 250 * GB, 500 * GB, 1 * TB, 2 * TB, 4 * TB]);
      const io = live ? int(1200, 68000) : 0, bw = live ? io * int(4000, 12000) : 0;
      // stride of 1 keeps placement even for any node count, and the primary
      // prefers an online node so front storage is where it can serve
      const onlineNodes = clusterNodes.filter(n => n.status === "online");
      const ref = k => {
        const pool2 = k === 0 && onlineNodes.length ? onlineNodes : clusterNodes;
        const n = pool2[(vi + k) % (pool2.length || 1)];
        return n ? {uuid: n.uuid, hostname: n.hostname} : null;
      };
      const v = {
        uuid: uuid(), pool_id: p.uuid, pool_name: p.pool_name, cluster_id: c.uuid,
        lvol_name: `${pick(VOL_PREFIX)}-${String(vi + 1).padStart(3, "0")}-${hex(3)}`,
        status: vstatus,
        nodes: {primary: ref(0), secondary: ref(1), tertiary: rnd() > .45 ? ref(2) : null},
        size_prov: prov, size_util: Math.round(prov * (.05 + rnd() * .8)),
        crypto_enabled: rnd() > .55, qos: qosOf(false),
        // data reduction is one per-volume switch: compression+dedup together
        compression_dedup_enabled: rnd() > .45,
        nqn: `nqn.2023-02.io.simplyblock:${hex(8)}`,
        base_snapshot: null,
        // a volume can belong to several consistency groups at once
        consistency_groups: [],
        affinity: null,
        pvc: null, bucket: null,
        backup_policy: rnd() > .5 ? {uuid: pick(clusterPolicies).uuid, policy_name: null} : null,
        replication: null,
        created_at: ago(int(20, 4000)),
        io_stats: ioStats(io, bw), io_history: {iops: series(io, .35), bytes: series(bw, .35)}
      };
      if (v.backup_policy) v.backup_policy.policy_name = (clusterPolicies.find(x => x.uuid === v.backup_policy.uuid) || {}).policy_name;
      v.logical_used = v.compression_dedup_enabled
        ? Math.round(v.size_util * (1.5 + rnd() * 1.6))
        : v.size_util;
      if (c.pod_affinity_enabled && rnd() > .55 && v.nodes.primary) {
        v.affinity = {mode: "pod", workload: `${v.lvol_name}-0`,
          workload_node: v.nodes.primary.hostname, satisfied: rnd() > .2};
      } else if (c.node_affinity === "strict" && v.nodes.primary && rnd() > .6) {
        v.affinity = {mode: "node", pinned_node_id: v.nodes.primary.uuid,
          pinned_node: v.nodes.primary.hostname, satisfied: true};
      }
      DB.lvols.push(v);

      // ---- snapshot chain -------------------------------------------------
      // Snapshots are chained: each one is a delta against its predecessor.
      const snapCount = int(0, 4);
      let prevSnap = null;
      const volSnaps = [];
      for (let si = 0; si < snapCount; si++) {
        const s = {uuid: uuid(), cluster_id: c.uuid, pool_id: p.uuid, pool_name: p.pool_name,
          lvol_id: v.uuid, lvol_name: v.lvol_name,
          snapshot_name: `${v.lvol_name}-snap-${String(si + 1).padStart(3, "0")}`,
          seq: si + 1, parent_id: prevSnap ? prevSnap.uuid : null,
          created_at: ago((snapCount - si) * int(1, 40)),
          size: Math.round(v.size_util * (.05 + rnd() * .3)),
          status: "online", backup_version_id: null};
        DB.snapshots.push(s); volSnaps.push(s); prevSnap = s;
      }

      // ---- backup chain ---------------------------------------------------
      // A backup is taken FROM a snapshot. Once taken, snapshot and backup
      // version are independent objects: deleting the online snapshot leaves
      // the backup version untouched. Versions form their own chain.
      if (v.backup_policy || rnd() > .65) {
        // a chain only claims a policy when the volume actually has one attached;
        // otherwise it is a manual backup chain
        const pol = v.backup_policy ? clusterPolicies.find(x => x.uuid === v.backup_policy.uuid) : null;
        const tiers = (pol || pick(clusterPolicies)).schedule;
        const versions = [];
        let seq = 1;
        // earliest version is always the full one; everything after it is a delta
        tiers.slice().reverse().forEach((tier, ti) => {
          const n = ti === 0 ? 1 : Math.min(tier.versions, int(2, tier.versions));
          for (let k = 0; k < n; k++) {
            const isFull = seq === 1;
            const src = volSnaps.length ? volSnaps[(seq - 1) % volSnaps.length] : null;
            versions.push({id: `v${String(seq).padStart(4, "0")}`, seq, tier: tier.interval,
              type: isFull ? "full" : "delta",
              created_at: ago(int(1, 800) / seq),
              size: Math.round(v.size_util * (isFull ? .82 + rnd() * .15 : .01 + rnd() * .06)),
              source_snapshot_id: src ? src.uuid : null,
              source_snapshot_name: src ? src.snapshot_name : null,
              merged_count: isFull ? int(0, 9) : 0});
            seq++;
          }
        });
        versions.sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at))
          .forEach((x, i) => { x.seq = i + 1; x.type = i === 0 ? "full" : "delta"; });
        // mark which snapshots have a backup version taken from them
        versions.forEach(x => {
          const s = volSnaps.find(y => y.uuid === x.source_snapshot_id);
          if (s) s.backup_version_id = x.id;
        });
        DB.backups.push({uuid: uuid(), cluster_id: c.uuid, pool_id: p.uuid, pool_name: p.pool_name,
          lvol_id: v.uuid, lvol_name: v.lvol_name,
          chain_id: `bk-${hex(6)}`,
          policy_id: pol ? pol.uuid : null, policy_name: pol ? pol.policy_name : null,
          bucket: pick(BUCKETS) + c.name + "/" + v.lvol_name + "/",
          status: rnd() > .06 ? "online" : "unavailable",
          created_at: versions[0] ? versions[0].created_at : ago(500),
          last_merge_at: rnd() > .5 ? ago(int(1, 30)) : null,
          versions});
      }
    }
  }
  // a few volumes are clones of an existing snapshot
  const snaps = DB.snapshots.filter(s => s.cluster_id === c.uuid);
  DB.lvols.filter(v => v.cluster_id === c.uuid).forEach(v => {
    if (snaps.length && rnd() > .72) {
      const s = pick(snaps);
      if (s.lvol_id !== v.uuid) v.base_snapshot = {uuid: s.uuid, snapshot_name: s.snapshot_name, lvol_name: s.lvol_name};
    }
  });
}

seedCluster("prod-eu-central-1", "nvme", "online", "datacenter", 0);
seedCluster("prod-us-east-2", "nvme", "degraded", "datacenter", 1);
seedCluster("prod-eu-north-1", "nvme", "online", "datacenter", 2);
seedCluster("stage-eu-west-1", "block", "in_activation", "datacenter", 3);
// edge clusters are block-device only — the NVMe device class is not offered there
seedCluster("edge-apac-1", "block", "suspended", "edge", 4);
seedCluster("dev-lab-01", "block", "unready", "edge", 5);

// Prepared, labelled hosts that belong to no cluster yet — the pool a new
// cluster or a migrated storage node can be built on.
for (let i = 0; i < 5; i++) {
  const h = seedHost({uuid: null, name: "pool"}, i, true);
  h.cluster_id = null;
  h.hostname = `pool-host-${String(i + 1).padStart(2, "0")}`;
  h.control_plane = false;
}

// ---- file storage: pick the metadata server and its failover candidates -----
DB.clusters.forEach(c => {
  if (!c.file_storage.enabled) return;
  const cp = DB.hosts.filter(h => h.cluster_id === c.uuid && h.control_plane && h.status === "available");
  const pool2 = cp.length ? cp : DB.hosts.filter(h => h.cluster_id === c.uuid && h.status === "available");
  if (!pool2.length) { c.file_storage.enabled = false; return; }
  c.file_storage.mds_host = {uuid: pool2[0].uuid, hostname: pool2[0].hostname};
  c.file_storage.mds_candidates = pool2.slice(1, 4).map(h => ({uuid: h.uuid, hostname: h.hostname}));
});

// ---- S3 buckets -------------------------------------------------------------
// One bucket is one filesystem is one logical volume: blobs live on cluster
// capacity, metadata in FoundationDB. So a bucket inherits everything a volume
// can do — snapshots, backups, synchronous and asynchronous replication.
const BUCKET_NAMES = ["media-assets", "app-logs", "ml-datasets", "invoices", "backups-tier2",
  "user-uploads", "telemetry", "artifacts", "warehouse"];
// S3 metadata a client can set and a console must be able to search by: bucket
// tags (up to 50 key/value pairs), the storage class default, the region the
// endpoint answers for, the owner, and lifecycle rules.
const BUCKET_TEAMS = ["platform", "data", "payments", "media", "ml"];
const BUCKET_ENVS = ["prod", "prod", "staging", "dev"];
const bucketTags = name => {
  const t = {env: pick(BUCKET_ENVS), team: pick(BUCKET_TEAMS), app: name.replace(/-\d+$/, "")};
  if (rnd() > .5) t["cost-center"] = `cc-${int(1000, 9999)}`;
  if (rnd() > .6) t.retention = pick(["30d", "90d", "1y", "7y"]);
  if (rnd() > .7) t.compliance = pick(["gdpr", "pci", "sox"]);
  return t;
};
const bucketLifecycle = () => rnd() > .5 ? [] : [
  {id: "expire-tmp", prefix: "tmp/", expire_days: pick([7, 14, 30]), status: "Enabled"},
  rnd() > .5 ? {id: "cold-archives", prefix: "archive/", transition_days: pick([30, 90]), transition_class: "infrequent-access", status: "Enabled"} : null,
  rnd() > .6 ? {id: "old-versions", noncurrent_expire_days: pick([30, 60]), status: pick(["Enabled", "Disabled"])} : null
].filter(Boolean);
DB.clusters.forEach(c => {
  if (!c.object_storage.enabled) return;
  const vols = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online" && !v.pvc && !v.bucket);
  vols.slice(0, int(2, 5)).forEach((v, i) => {
    const name = `${pick(BUCKET_NAMES)}-${String(i + 1).padStart(2, "0")}`;
    const b = {uuid: uuid(), cluster_id: c.uuid, name,
      lvol_id: v.uuid, lvol_name: v.lvol_name,
      pool_id: v.pool_id, pool_name: v.pool_name,
      status: "online",
      versioning: rnd() > .55,
      object_lock: rnd() > .8,
      quota_bytes: rnd() > .5 ? v.size_prov : 0,
      objects: int(1200, 4200000),
      size_bytes: v.size_util,
      region: c.object_storage.region,
      storage_class: pick(["standard", "standard", "standard", "infrequent-access"]),
      owner: pick(["platform-team", "data-eng", "payments", "media-svc"]),
      tags: bucketTags(name),
      lifecycle_rules: bucketLifecycle(),
      cors_enabled: rnd() > .7,
      // bucket security rides on Kubernetes: a service account and a secret
      // holding the access key, bound by an IAM-style policy document
      access: {
        service_account: `sb-s3-${name}`,
        namespace: pick(["default", "prod", "data", "team-a"]),
        secret_name: `${name}-s3-credentials`,
        access_key_id: `SB${hex(9).toUpperCase()}`,
        policy: pick(["read-write", "read-write", "read-only", "write-only"]),
        public: rnd() > .9
      },
      created_at: ago(int(20, 2600))};
    DB.buckets.push(b);
    v.bucket = {uuid: b.uuid, name: b.name};
  });
});

// ---- consistency groups ----------------------------------------------------
// A consistency group is just a named set of volumes. It exists on its own —
// asynchronous replication policies may reference one, but do not own it.
const CG_NAMES = ["sap-hana-prod", "oracle-fin", "mssql-erp", "postgres-cluster", "vmware-tier1", "ci-fleet"];
DB.clusters.forEach(c => {
  const vols = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online");
  if (vols.length < 2) return;
  for (let i = 0; i < int(1, 3); i++) {
    // groups may overlap: the same volume can be a member of several
    const members = vols.filter(() => rnd() > .62).slice(0, int(2, 5));
    if (members.length < 2) continue;
    // A group can carry a backup policy and/or a replication policy. When it
    // does, the group is the unit of protection: every member is snapshotted,
    // backed up and replicated together, and a volume joining the group
    // inherits the group's policies.
    const cg = {uuid: uuid(), cluster_id: c.uuid, name: `${pick(CG_NAMES)}-${i + 1}`,
      lvol_ids: members.map(v => v.uuid), created_at: ago(int(200, 2600)),
      backup_policy: null, replication_config: null};
    DB.consistency_groups.push(cg);
    members.forEach(v => { (v.consistency_groups = v.consistency_groups || []).push({uuid: cg.uuid, name: cg.name}); });
    for (let k = 0; k < int(0, 4); k++) {
      const at = ago(int(1, 400) / (k + 1));
      DB.cg_snapshots.push({uuid: uuid(), cluster_id: c.uuid, cg_id: cg.uuid, cg_name: cg.name,
        snapshot_name: `${cg.name}-cgsnap-${String(k + 1).padStart(3, "0")}`,
        created_at: at, status: "online",
        members: members.map(v => ({lvol_id: v.uuid, lvol_name: v.lvol_name,
          snapshot_id: uuid(), size: Math.round(v.size_util * (.04 + rnd() * .2))})),
        backup_version_id: rnd() > .55 ? `v${String(int(1, 9)).padStart(4, "0")}` : null,
        backup_bucket: null});
    }
  }
});
DB.cg_snapshots.forEach(s2 => {
  s2.size = s2.members.reduce((a, m) => a + m.size, 0);
  if (s2.backup_version_id) s2.backup_bucket = `s3://sb-backup-eu/cg/${s2.cg_name}/`;
});

// ---- zones and regions ------------------------------------------------------
// Topology comes from the Kubernetes node labels, not from a bespoke object:
//   topology.kubernetes.io/region  and  topology.kubernetes.io/zone
// Storage classes map them onto storage clusters through zone_cluster_map and
// region_cluster_map, so a pod scheduled in a zone gets local storage.
DB.zones = [];
const ZONE_DEFS = [
  ["eu-central-1a", "eu-central-1", "Frankfurt, Hetzner DC1"],
  ["eu-central-1b", "eu-central-1", "Frankfurt, Interxion FRA6"],
  ["us-east-2a", "us-east-2", "Ashburn, Equinix DC2"],
  ["us-east-2b", "us-east-2", "Ashburn, Equinix DC11"],
  ["eu-north-1a", "eu-north-1", "Stockholm, Digital Realty"],
  ["ap-southeast-1a", "ap-southeast-1", "Singapore, edge cage"]
];
ZONE_DEFS.forEach(([name, region, location]) => {
  DB.zones.push({uuid: uuid(), name, region, location,
    label: "topology.kubernetes.io/zone=" + name,
    region_label: "topology.kubernetes.io/region=" + region,
    k8s_cluster_ids: [], rtt_ms: null, created_at: ago(int(600, 4000)), cluster_ids: []});
});
// A Kubernetes cluster spans one or more zones and provides the worker nodes
// that become simplyblock hosts.
const K8S_DEFS = [
  ["k8s-prod-a", ["eu-central-1a", "eu-central-1b"], "1.31.4"],
  ["k8s-prod-b", ["us-east-2a", "us-east-2b"], "1.30.8"],
  ["k8s-stage", ["eu-north-1a"], "1.32.1"],
  ["k8s-edge", ["ap-southeast-1a"], "1.30.6"],
  ["k8s-analytics", ["eu-central-1a"], "1.31.2"]
];
K8S_DEFS.forEach(([name, zoneNames, version], i) => {
  const zoneIds = zoneNames.map(n => (DB.zones.find(x => x.name === n) || {}).uuid).filter(Boolean);
  const k = {uuid: uuid(), name, version,
    api_endpoint: `https://${name}.k8s.internal:6443`,
    environment: pick(["Vanilla", "OpenShift", "Rancher", "K3s", "Talos"]),
    csi_version: pick(["26.2.1", "26.2.0", "26.1.2", "26.1.0"]),
    csi_status: rnd() > .12 ? "online" : pick(["degraded", "unreachable"]),
    operator_namespace: pick(["simplyblock", "simplyblock", "sb-system"]),
    status: rnd() > .1 ? "online" : "degraded",
    zone_ids: zoneIds, created_at: ago(int(600, 4000))};
  DB.k8s_clusters.push(k);
  zoneIds.forEach(id => { const st = DB.zones.find(x => x.uuid === id); if (st) st.k8s_cluster_ids.push(k.uuid); });
});
const zoneBy = n => DB.zones.find(s => s.name === n);

// stretch the first two clusters across two zones each, the rest sit in one
DB.clusters.forEach((c, i) => {
  const zoneSpans = [["eu-central-1a", "eu-central-1b"], ["us-east-2a", "us-east-2b"], ["eu-north-1a"],
    ["eu-central-1a"], ["ap-southeast-1a"], ["eu-central-1b"]];
  const names = zoneSpans[i] || ["eu-central-1a"];
  c.zone_ids = names.map(n => zoneBy(n).uuid);
  c.stretched = c.zone_ids.length > 1;
  c.sync_replication_enabled = c.stretched;
  names.forEach(n => zoneBy(n).cluster_ids.push(c.uuid));
  // distribute the cluster's nodes across its zones
  const ns = DB.storage_nodes.filter(n2 => n2.cluster_id === c.uuid);
  ns.forEach((n2, k) => { n2.zone_id = c.zone_ids[k % c.zone_ids.length]; });
  // hosts belong to exactly one zone; a node inherits its host's zone
  const hs = DB.hosts.filter(h => h.cluster_id === c.uuid);
  hs.forEach((h, k) => {
    h.zone_id = c.zone_ids[k % c.zone_ids.length];
    const st = DB.zones.find(x => x.uuid === h.zone_id);
    if (st) { h.zone = st.name; h.region = st.region; }
    // a zone can host several Kubernetes clusters — spread the worker nodes over all of them
    const ids = st ? st.k8s_cluster_ids : [];
    const kc = ids.length ? DB.k8s_clusters.find(x => x.uuid === ids[k % ids.length]) : null;
    h.k8s_cluster_id = kc ? kc.uuid : null;
    h.k8s_cluster = kc ? kc.name : null;
  });
  ns.forEach(n2 => {
    const h = DB.hosts.find(x => x.uuid === n2.host_id);
    if (h) n2.zone_id = h.zone_id;
    if (!c.failure_domain_enabled) { n2.failure_domain = null; return; }
    const st = DB.zones.find(x => x.uuid === n2.zone_id);
    n2.failure_domain = c.failure_domain_scope === "zone" ? (st ? st.name : null)
      : c.failure_domain_scope === "cabinet" ? (h ? h.cabinet_id : null)
      : (h ? h.rack_id : null);
  });
  if (c.failure_domain_enabled) {
    // A real cluster is built domain by domain, in pairs: every domain holds at
    // least two nodes and no domain is more than one node ahead of another.
    // Taking whatever label each host happened to carry produced singleton
    // domains, which the balance rule forbids.
    const labels = [...new Set(ns.map(n2 => n2.failure_domain).filter(Boolean))];
    const domains = labels.length >= 2 ? labels
      : FD.slice(0, Math.max(2, Math.min(4, Math.floor(ns.length / 2))));
    const usable = Math.min(domains.length, Math.floor(ns.length / 2));
    const use = domains.slice(0, Math.max(2, usable));
    // fill pairwise: two nodes per domain, then round-robin the remainder
    const order = [];
    use.forEach(f => { order.push(f, f); });
    for (let i = order.length; i < ns.length; i++) order.push(use[i % use.length]);
    ns.forEach((n2, i) => { n2.failure_domain = order[i] || use[0]; });
  }
  // a degraded fixture cluster loses nodes inside ONE failure domain
  if (c.failure_domain_enabled && c.status === "degraded") {
    const down = ns.filter(n => n.status !== "online");
    const fd = down.length ? down[0].failure_domain : null;
    down.slice(1).forEach(n => { if (n.failure_domain !== fd) n.status = "online"; });
  }
});
// hosts in the unassigned pool are racked in a zone too
DB.hosts.filter(h => !h.cluster_id).forEach((h, k) => {
  const st = DB.zones[k % DB.zones.length];
  h.zone_id = st.uuid; h.zone = st.name; h.region = st.region;
  const ids = st.k8s_cluster_ids;
  const kc = ids.length ? DB.k8s_clusters.find(x => x.uuid === ids[k % ids.length]) : null;
  h.k8s_cluster_id = kc ? kc.uuid : null;
  h.k8s_cluster = kc ? kc.name : null;
});

// ---- storage classes and PVCs ----------------------------------------------
// The CSI driver provisions a logical volume per PVC. A PVC references its
// storage class; the volume carries a back-reference to the PVC.
const NS_NAMES = ["default", "prod", "staging", "data", "monitoring", "team-a", "team-b"];
// StorageClassParameters as declared on the StoragePool CRD (camelCase), written
// to the StorageClass under the CSI driver's parameter names.
const SCP_DEFS = [
  {encryption: true, compression: true, qosRwIops: 100000, qosRwMbytes: 0,
    filesystem: "ext4", fabric: "tcp", maxNamespacePerSubsys: 1, tune2fsReservedBlocks: 0},
  {encryption: false, compression: true, qosRwIops: 50000, qosRwMbytes: 1000,
    filesystem: "xfs", fabric: "tcp", maxNamespacePerSubsys: 8, tune2fsReservedBlocks: 0},
  {encryption: false, compression: false, qosRwIops: 0, qosRwMbytes: 0,
    filesystem: null, fabric: "tcp", maxNamespacePerSubsys: 1, tune2fsReservedBlocks: 0},
  {encryption: true, compression: false, qosRwIops: 0, qosRwMbytes: 2000, qosRMbytes: 1500,
    filesystem: "ext4", fabric: "rdma", maxNamespacePerSubsys: 1, tune2fsReservedBlocks: 5}
];
// CRD camelCase field -> CSI StorageClass parameter name
const SCP_MAP = {qosRwIops: "qos_rw_iops", qosRwMbytes: "qos_rw_mbytes", qosRMbytes: "qos_r_mbytes",
  qosWMbytes: "qos_w_mbytes", compression: "compression", encryption: "encryption",
  fabric: "fabric", maxNamespacePerSubsys: "max_namespace_per_subsys",
  tune2fsReservedBlocks: "tune2fs_reserved_blocks", filesystem: "csi.storage.k8s.io/fstype"};
DB.k8s_clusters.forEach((kc, ki) => {
  // storage classes point at a pool in a storage cluster reachable from this k8s cluster
  const storClusters = DB.clusters.filter(c => (c.zone_ids || []).some(id => kc.zone_ids.includes(id)));
  const target = storClusters.length ? storClusters[0] : DB.clusters[0];
  const pools = DB.pools.filter(p => target && p.cluster_id === target.uuid);
  // The operator generates a StorageClass per active StoragePool. A pool can back
  // several classes (different QoS, filesystem or encryption defaults over the
  // same capacity); a class always belongs to exactly one pool.
  const gen = [];
  pools.slice(0, int(2, 4)).forEach((pool, si) => { gen.push([pool, si]); if (rnd() > .6) gen.push([pool, si + 1]); });
  // one simplyblock operator per Kubernetes cluster, so one namespace for every
  // StorageClass it generates
  const ns = kc.operator_namespace;
  gen.forEach(([pool, si]) => {
    const spec = SCP_DEFS[si % SCP_DEFS.length];
    const sibling = DB.storage_classes.some(x => x.pool_id === pool.uuid && x.k8s_cluster_id === kc.uuid);
    // cluster_id and pool_name always come from the pool and cannot be overridden
    const params = {cluster_id: target.uuid, pool_name: pool.pool_name};
    Object.entries(spec).forEach(([k2, v2]) => {
      if (v2 === null || v2 === undefined) return;
      params[SCP_MAP[k2]] = typeof v2 === "boolean" ? (v2 ? "True" : "False") : String(v2);
    });
    // Topology-aware provisioning: map the pod's zone / region label onto the
    // storage cluster that serves it, so a PVC lands on local storage.
    // Per zone, prefer the most zone-local cluster (narrowest zone span) so the
    // map genuinely routes a pod to storage beside it. The region falls back to
    // the widest cluster in that region, which is the stretched one.
    const zoneMap = {}, regionMap = {};
    const byRegion = {};
    kc.zone_ids.forEach(zid => {
      const z = DB.zones.find(x => x.uuid === zid);
      if (!z) return;
      const here = DB.clusters.filter(x => (x.zone_ids || []).includes(zid));
      const local = here.slice().sort((p, q) =>
        (p.zone_ids.length - q.zone_ids.length) || (p.uuid === target.uuid ? -1 : 1))[0] || target;
      zoneMap[z.name] = local.uuid;
      (byRegion[z.region] = byRegion[z.region] || []).push(...here);
    });
    Object.entries(byRegion).forEach(([region, cs]) => {
      const wide = cs.slice().sort((p, q) => q.zone_ids.length - p.zone_ids.length)[0];
      if (wide) regionMap[region] = wide.uuid;
    });
    if (Object.keys(zoneMap).length > 1) params.zone_cluster_map = JSON.stringify(zoneMap);
    if (Object.keys(regionMap).length) params.region_cluster_map = JSON.stringify(regionMap);
    const dhchap = rnd() > .7;
    if (dhchap) params.dhchap_node_label = "simplyblock.io/dhchap-pool";
    const scId = uuid();
    DB.storage_classes.push({uuid: scId, k8s_cluster_id: kc.uuid,
      // A StorageClass is scoped to its Kubernetes cluster, so only a sibling in
      // the SAME cluster over the SAME pool needs disambiguating.
      name: `simplyblock-${ns}-${target.name}-${pool.pool_name}`
        + (sibling ? `-${spec.filesystem || "block"}` : ""),
      variant: sibling ? (spec.filesystem || "block") : "default",
      operator_namespace: ns,
      provisioner: "csi.simplyblock.io",
      cluster_id: target.uuid,
      pool_id: pool.uuid, pool_name: pool.pool_name,
      storage_pool_ref: `${ns}/${pool.pool_name}`,
      spec_parameters: spec,
      parameters: params,
      zone_cluster_map: zoneMap, region_cluster_map: regionMap,
      dhchap: dhchap,
      allowed_topology: dhchap ? "simplyblock.io/dhchap-pool" : null,
      reclaim_policy: pick(["Delete", "Delete", "Retain"]),
      volume_binding_mode: pick(["WaitForFirstConsumer", "WaitForFirstConsumer", "Immediate"]),
      allow_volume_expansion: true,
      is_default: si === 0 && ki === 0,
      created_at: ago(int(400, 3000))});
  });
});
// bind PVCs to real logical volumes
const PVC_WORKLOADS = ["postgres", "kafka", "mysql", "redis", "elastic", "minio", "clickhouse", "grafana", "registry"];
DB.k8s_clusters.forEach(kc => {
  const scs = DB.storage_classes.filter(x => x.k8s_cluster_id === kc.uuid);
  if (!scs.length) return;
  const clusterIds = [...new Set(scs.map(x => x.cluster_id))];
  // a volume can only be claimed through the StorageClass of its own pool
  const byPool = {};
  scs.forEach(x => { if (x.pool_id) (byPool[x.pool_id] = byPool[x.pool_id] || []).push(x); });
  // a bucket's volume is its filesystem and cannot also back a claim
  const free = DB.lvols.filter(v => clusterIds.includes(v.cluster_id) && !v.pvc && !v.bucket && byPool[v.pool_id]);
  free.forEach((v, i) => {
    if (rnd() < .35) return;
    const opts = byPool[v.pool_id];
    const sc = opts[i % opts.length];
    const ns = pick(NS_NAMES);
    const wl = pick(PVC_WORKLOADS);
    const name = `${wl}-data-${String(i % 9).padStart(2, "0")}`;
    const bound = v.status === "online" ? (rnd() > .05 ? "Bound" : "Lost") : "Pending";
    const srcCluster = DB.clusters.find(x => x.uuid === v.cluster_id);
    const fileCap = srcCluster && srcCluster.file_storage.enabled;
    const pvc = {uuid: uuid(), k8s_cluster_id: kc.uuid,
      namespace: ns, pvc_name: name,
      storage_class_id: sc.uuid, storage_class: sc.name,
      lvol_id: bound === "Pending" ? null : v.uuid,
      requested_bytes: v.size_prov, actual_bytes: v.size_prov,
      status: bound,
      // RWX is served by pNFS and is XFS-only, so it is offered only where the
      // storage cluster has file storage enabled
      access_mode: (fileCap && rnd() > .6) ? "ReadWriteMany"
        : pick(["ReadWriteOnce", "ReadWriteOnce", "ReadWriteOncePod"]),
      volume_mode: pick(["Filesystem", "Filesystem", "Block"]),
      workload: `${wl}-${int(0, 2)}`,
      workload_kind: pick(["StatefulSet", "StatefulSet", "Deployment"]),
      annotations: Object.assign(
        {"volume.kubernetes.io/storage-provisioner": "csi.simplyblock.io"},
        rnd() > .5 ? {"backup.simplyblock.io/policy": pick(["5m-tiered", "hourly-24h", "daily-30d"])} : {},
        rnd() > .6 ? {"simplyblock.io/tier": pick(["gold", "silver", "bronze"])} : {},
        rnd() > .75 ? {"app.kubernetes.io/part-of": wl} : {}),
      labels: {"app.kubernetes.io/name": wl, "app.kubernetes.io/instance": name},
      created_at: ago(int(10, 3000))};
    if (pvc.access_mode === "ReadWriteMany") {
      // pNFS on the Linux kernel NFS server supports XFS only
      pvc.volume_mode = "Filesystem";
      pvc.filesystem = "xfs";
      pvc.annotations["simplyblock.io/access"] = "pnfs";
    } else {
      pvc.filesystem = pvc.volume_mode === "Block" ? null : pick(["ext4", "xfs"]);
    }
    DB.pvcs.push(pvc);
    if (pvc.lvol_id) v.pvc = {uuid: pvc.uuid, name: pvc.pvc_name, namespace: ns,
      storage_class: sc.name, k8s_cluster_id: kc.uuid, k8s_cluster: kc.name, workload: pvc.workload};
  });
});

// ---- application-level DR (Ramen model) ------------------------------------
// DRCluster        — a managed Kubernetes cluster enrolled in DR, with its S3
//                    metadata profile and a fencing state
// DRPolicy         — a pair of DRClusters plus a scheduling interval
// DRPlacementControl — one protected application: which PVCs, where it runs,
//                    where it fails over to, and its current phase
// VolumeReplicationGroup — the PVC set of that application, primary/secondary
DB.k8s_clusters.forEach((kc, i) => {
  DB.dr_clusters.push({
    uuid: uuid(), k8s_cluster_id: kc.uuid, name: kc.name,
    region: (DB.zones.find(x => x.uuid === kc.zone_ids[0]) || {}).region || "unknown",
    s3_profile_name: `s3-${kc.name}`,
    s3_endpoint: `https://s3.${(DB.zones.find(x => x.uuid === kc.zone_ids[0]) || {}).region || "eu-central-1"}.amazonaws.com`,
    s3_bucket: `ramen-metadata-${kc.name}`,
    fencing_state: rnd() > .88 ? pick(["Fenced", "ManuallyFenced"]) : "Unfenced",
    status: kc.status === "online" ? (rnd() > .1 ? "online" : "degraded") : "degraded",
    ramen_version: `4.${int(14, 18)}`,
    last_heartbeat_at: ago(rnd()),
    created_at: ago(int(500, 3000))
  });
});
const APP_DEFS = [
  ["pacman", "pacman", "ApplicationSet"], ["busybox-sample", "busybox", "Subscription"],
  ["postgres-ha", "db-prod", "ApplicationSet"], ["kafka-stream", "data", "Subscription"],
  ["vm-win2022", "virt", "VirtualMachine"], ["vm-rhel9-erp", "virt", "VirtualMachine"],
  ["grafana-stack", "monitoring", "ApplicationSet"]
];
// The only valid progressions for each Ramen phase, in the order they occur.
// The seeder and the reconcile loop both read this, so they cannot drift apart.
// Node lifecycle operations. The phase list IS the progress tracker the UI
// draws, and the subtask list is what shows up under the cluster's tasks —
// one entry per phase, so the two can never disagree.
const NODE_OPS = {
  removal: {label: "Removal", status: "in_removal", every: 5000, done: "node removed",
    phases: ["data migration and rebalancing", "volume migration", "removed"],
    subtasks: ["migrate_data", "migrate_volumes", "deregister_node"],
    hint: "Data is rebalanced onto the remaining nodes, then the volumes whose primary sits here are moved off. The node and its device records are deleted at the end."},
  expansion: {label: "Expansion", status: "in_creation", every: 5000, done: "node added and data rebalanced",
    phases: ["adding node", "rebalancing data", "complete"],
    subtasks: ["add_node", "rebalance_data", "activate_node"],
    hint: "The storage node is deployed and joins the cluster, then existing data is rebalanced onto it."},
  migration: {label: "Migration", status: "in_migration", every: 5000, done: "node migrated to the new host",
    phases: ["restarting node", "rebalancing", "removing node", "migrated"],
    subtasks: ["restart_node", "rebalance_data", "remove_source_node", "finish_migration"],
    hint: "The node is restarted on the prepared target host, data is rebalanced, and the record on the old host is removed."}
};
const PROGRESSION_BY_PHASE = {
  Deployed: ["Completed"],
  FailedOver: ["Completed"],
  Relocated: ["Completed"],
  WaitForUser: ["WaitOnUserToCleanUp"],
  FailingOver: ["EnsuringVolumesAreSecondary", "WaitingForResourceRestore"],
  // backup-based protection: the volumes are rebuilt from backups first (Ramen's volume promotion), then handed out as new PVCs
  FailingOverFromBackup: ["RestoringBackups", "PromotingVolumes", "WaitingForResourceRestore"],
  Relocating: ["WaitingForResourceRestore", "UpdatedPlacement", "Cleaning Up"],
  // A restore is an ordinary Ramen failover with one addition: the generation
  // is pinned out of band immediately before the DRPC rebind, because
  // PromoteVolume carries no point-in-time argument.
  RestoringGeneration: ["PinningGeneration", "UpdatingPlacement", "MaterialisingVolumes",
    "RestoringKubeObjects", "WaitingForResourceRestore", "Completed"]
};
const progressionsFor = ph => PROGRESSION_BY_PHASE[ph] || ["Completed"];

// index 0 of every run gets a FailedOver app (so Relocate is reachable on load),
// one app parks in WaitForUser, the rest are steady.
// ---- cluster / volume migrations -------------------------------------------
// Two mechanisms: within a cluster the volumes' primaries are moved by instant
// migration; between clusters or zones the data is shipped by asynchronous
// replication and the last, smallest snapshot is applied during a brief IO
// freeze while the NVMe paths roll over to the target.
const MIG_STATE_INTRA = ["running", "completed", "completed"];
const MIG_STATE_CROSS = ["replicating", "converging", "cutover_pending", "completed", "paused"];
DB.clusters.filter(c => c.capabilities.rebalancing).slice(0, 3).forEach((c, ci) => {
  const vols = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online").slice(0, int(2, 6));
  if (!vols.length) return;
  const cross = ci % 2 === 1;
  const iterationLimit = 12;
  // intra-cluster targets must be one of the cluster's own zones
  const ownZones = DB.zones.filter(x => c.zone_ids.includes(x.uuid));
  const targetZone = cross
    ? pick(DB.zones.filter(x => !c.zone_ids.includes(x.uuid)).concat(DB.zones))
    : (ownZones.length > 1 ? ownZones[ownZones.length - 1] : ownZones[0]);
  const targetCluster = cross ? pick(DB.clusters.filter(x => x.uuid !== c.uuid && x.dr_target_eligible)) : null;
  const freezeThreshold = 256e6;
  // leave iterations to spare so convergence is still reachable
  const iterations = cross ? int(2, 7) : 0;
  const firstSnap = vols.reduce((a, v) => a + v.size_util, 0);
  const lastSnap = cross ? Math.max(freezeThreshold * 1.4, Math.round(firstSnap / Math.pow(2.1, iterations))) : 0;
  // a cross-cluster job that is not yet converged must not claim to be ready
  const state = cross
    ? (lastSnap > freezeThreshold ? pick(["replicating", "converging", "converging", "paused"]) : "cutover_pending")
    : pick(MIG_STATE_INTRA);
  // taint the destination hosts so an intra-cluster job has somewhere to go
  if (!cross && targetZone) {
    DB.hosts.filter(h => h.cluster_id === c.uuid && h.zone_id === targetZone.uuid
      && (h.storage_node_ids || []).length)
      .forEach(h => { h.migration_taint = "simplyblock.io/migration-target=true"; });
  }
  DB.migrations.push({
    uuid: uuid(), cluster_id: c.uuid,
    name: cross ? `move-${c.name.split("-").slice(0, 2).join("-")}-to-${(targetCluster || {name: "dr"}).name.split("-").slice(-2).join("-")}`
      : `follow-workload-${c.name.split("-").slice(0, 2).join("-")}-${ci + 1}`,
    mode: cross ? "cross_cluster" : "intra_cluster",
    scope: rnd() > .5 ? "volumes" : "cluster",
    source_cluster_id: c.uuid,
    target_cluster_id: targetCluster ? targetCluster.uuid : c.uuid,
    target_zone_id: targetZone ? targetZone.uuid : null,
    target_taint: "simplyblock.io/migration-target=true",
    follow_workload: rnd() > .35,
    state,
    lvol_ids: vols.map(v => v.uuid),
    moved_count: state === "completed" ? vols.length : int(0, Math.max(0, vols.length - 1)),
    // cross-cluster convergence
    iterations, iteration_limit: iterationLimit,
    first_snapshot_bytes: cross ? firstSnap : 0,
    last_snapshot_bytes: lastSnap,
    freeze_threshold_bytes: cross ? freezeThreshold : 0,
    estimated_freeze_ms: cross ? Math.max(120, Math.round(lastSnap / 1.2e6)) : 0,
    throughput_bytes_ps: state === "paused" ? 0 : int(60, 900) * 1e6,
    started_at: ago(int(2, 300)),
    completed_at: state === "completed" ? ago(int(1, 40)) : null,
    frozen_at: null, error: null
  });
});

// ---- cluster pairs ---------------------------------------------------------
// Pairing is directional. a→b and b→a are two pairs, which is how bidirectional
// and fan-out topologies (a→b, b→a, a→c, b→c) are expressed.
DB.cluster_pairs = [];
const PAIR_STATE = ["paired", "paired", "paired", "paired", "degraded", "unreachable"];
const eligible = DB.clusters.filter(c => c.dr_target_eligible);
function pairUp(src, tgt) {
  if (!src || !tgt || src.uuid === tgt.uuid) return null;
  if (DB.cluster_pairs.some(p => p.source_cluster_id === src.uuid && p.target_cluster_id === tgt.uuid)) return null;
  const state = pick(PAIR_STATE);
  const p = {uuid: uuid(), source_cluster_id: src.uuid, target_cluster_id: tgt.uuid, state,
    link: {endpoint: `nvmf://${tgt.name}.simplyblock.remote:4420`,
      nqn: `nqn.2023-02.io.simplyblock:repl:${hex(8)}`,
      rtt_ms: +(2 + rnd() * 38).toFixed(1),
      bandwidth_mbit: pick([1000, 2500, 10000]),
      throughput_bytes_ps: state === "unreachable" ? 0 : int(20, 900) * 1e6},
    last_handshake_at: ago(state === "unreachable" ? int(3, 40) : rnd()),
    created_at: ago(int(200, 3000))};
  DB.cluster_pairs.push(p);
  return p;
}
const dcK8s = DB.clusters.filter(c => c.capabilities.async_replication && c.location_type !== "edge");
if (dcK8s.length > 1) {
  pairUp(dcK8s[0], dcK8s[1]);          // a → b
  pairUp(dcK8s[1], dcK8s[0]);          // b → a  (bidirectional)
}
DB.clusters.filter(c => c.capabilities.async_replication).forEach((c, i) => {
  const t = eligible.filter(e => e.uuid !== c.uuid);
  if (t.length) pairUp(c, t[i % t.length]);
});

// ---- DR replication policies ----------------------------------------------
// Async policies hang off a cluster pair. Sync policies live inside one
// stretched cluster and name the zones they span.
DB.dr_policies = [];
const SCHEDULES = [
  [{interval: "5m", keep: 10}, {interval: "15m", keep: 4}, {interval: "1h", keep: 11}, {interval: "1d", keep: 6}],
  [{interval: "15m", keep: 8}, {interval: "1h", keep: 12}, {interval: "1d", keep: 7}],
  [{interval: "1h", keep: 24}, {interval: "1d", keep: 14}],
  [{interval: "5m", keep: 12}]
];
const POL_STATE = ["healthy", "healthy", "healthy", "healthy", "degraded", "unhealthy"];

DB.cluster_pairs.filter(p => p.state !== "unreachable").forEach((p, pi) => {
  const src = DB.clusters.find(c => c.uuid === p.source_cluster_id);
  const tgt = DB.clusters.find(c => c.uuid === p.target_cluster_id);
  for (let i = 0; i < int(1, 2); i++) {
    const state = p.state === "degraded" ? pick(["degraded", "unhealthy"]) : pick(POL_STATE);
    const freq = pick([5, 5, 15, 30, 60]);
    const pol = {
      uuid: uuid(), name: `dr-${src.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(-2).join("-")}${i ? "-tier2" : ""}`,
      mode: "asynchronous", pair_id: p.uuid,
      source_cluster_id: src.uuid, target_cluster_id: tgt.uuid, zone_ids: null,
      cg_id: null, cg_name: null,
      frequency_minutes: freq,
      retention: pick(SCHEDULES),
      failback: {mode: pick(["manual", "manual", "automatic"]), frequency_minutes: freq,
        reverse_on_failover: true, resync_full: rnd() > .7},
      state,
      last_replication_at: ago(state === "unhealthy" ? int(3, 26) : (freq / 60) * (rnd() * .9)),
      backlog_bytes: state === "healthy" ? int(50, 2600) * 1e6 : int(4000, 90000) * 1e6,
      generations_kept: 0,
      created_at: ago(int(100, 2600)), last_failover_at: null, last_test_at: rnd() > .6 ? ago(int(20, 700)) : null,
      lvol_ids: []
    };
    pol.generations_kept = pol.retention.reduce((a, r) => a + r.keep, 0);
    DB.dr_policies.push(pol);
  }
});

// synchronous policies on stretched clusters
DB.clusters.filter(c => c.stretched).forEach(c => {
  const pol = {
    uuid: uuid(), name: `sync-${c.name.split("-").slice(0, 2).join("-")}-stretch`,
    mode: "synchronous", pair_id: null,
    source_cluster_id: c.uuid, target_cluster_id: null, zone_ids: c.zone_ids.slice(),
    cg_id: null, cg_name: null, frequency_minutes: 0, retention: [],
    failback: {mode: "automatic", frequency_minutes: 0, reverse_on_failover: false, resync_full: false},
    state: pick(["healthy", "healthy", "healthy", "degraded"]),
    last_replication_at: ago(0), backlog_bytes: 0, generations_kept: 0,
    created_at: ago(int(200, 2000)), last_failover_at: null, last_test_at: null, lvol_ids: []
  };
  DB.dr_policies.push(pol);
});

// attach volumes
DB.dr_policies.forEach(pol => {
  DB.lvols.filter(v => v.cluster_id === pol.source_cluster_id && v.status === "online").forEach(v => {
    if (v.replication || rnd() < .68) return;
    pol.lvol_ids.push(v.uuid);
    const healthy = pol.state === "healthy" ? rnd() > .12 : rnd() > .55;
    v.replication = {
      policy_id: pol.uuid, policy_name: pol.name, mode: pol.mode,
      status: healthy ? "healthy" : "unhealthy",
      last_replication_at: pol.mode === "synchronous" ? ago(0)
        : ago(healthy ? (pol.frequency_minutes / 60) * (rnd() * .9) : int(2, 20)),
      backlog_bytes: pol.mode === "synchronous" ? 0
        : Math.round(v.size_util * (healthy ? .004 + rnd() * .02 : .08 + rnd() * .3)),
      consistency_group: pol.cg_name || null, frequency_minutes: pol.frequency_minutes,
      generations: pol.generations_kept
    };
  });
});

const recipeFor = (name, kind, ns) => {
  const app = name.split("-")[0];
  const sel = {matchLabels: {"app.kubernetes.io/name": app}};
  const vm = kind === "VirtualMachine";
  const groups = vm ? [
    {name: "config", type: "resource", includedResourceTypes: ["Secret", "ConfigMap"], labelSelector: sel},
    {name: "vms", type: "resource", includedResourceTypes: ["VirtualMachine", "DataVolume"], labelSelector: sel},
    {name: "expose", type: "resource", includedResourceTypes: ["Service"], labelSelector: sel}
  ] : [
    {name: "config", type: "resource", includedResourceTypes: ["Secret", "ConfigMap", "ServiceAccount"], labelSelector: sel},
    {name: "data", type: "resource", includedResourceTypes: ["StatefulSet"], labelSelector: sel},
    {name: "app", type: "resource", includedResourceTypes: ["Deployment"], labelSelector: sel},
    {name: "expose", type: "resource", includedResourceTypes: ["Service", "Ingress", "Route"], labelSelector: sel}
  ];
  const hooks = vm ? [
    {name: "vm-ready", type: "check", selectResource: "pod", labelSelector: {matchLabels: {"kubevirt.io/domain": app}}, ops: [],
      chks: [{name: "running", condition: "{$.status.phase} == 'Running'", timeout: 600, onError: "fail"}]}
  ] : [
    {name: "db", type: "exec", selectResource: "pod", labelSelector: {matchLabels: {"app.kubernetes.io/component": "db"}},
      ops: [{name: "checkpoint", container: "postgres", command: "psql -c CHECKPOINT", timeout: 60, onError: "fail"},
        {name: "isready", container: "postgres", command: "pg_isready -q", timeout: 180, onError: "fail"}], chks: []},
    {name: "data-ready", type: "check", selectResource: "statefulset", labelSelector: sel, ops: [],
      chks: [{name: "replicas", condition: "{$.status.readyReplicas} == {$.spec.replicas}", timeout: 900, onError: "fail"}]}
  ];
  return {
    name: `${app}-recipe`, namespace: ns, appType: app, groups, hooks,
    captureWorkflow: {failOn: "any-error", sequence: vm ? [{group: "config"}, {group: "vms"}, {group: "expose"}] : [{hook: "db/checkpoint"}, {group: "config"}, {group: "data"}, {group: "app"}, {group: "expose"}]},
    recoverWorkflow: {failOn: pick(["any-error", "any-error", "essential-error"]), sequence: vm
      ? [{group: "config"}, {group: "vms"}, {hook: "vm-ready/running"}, {group: "expose"}]
      : [{group: "config"}, {group: "data"}, {hook: "data-ready/replicas"}, {hook: "db/isready"}, {group: "app"}, {group: "expose"}]}
  };
};
// recovery points a backup-protected app can fail over to: one per retained
// backup version of its group chain, newest first
function recoveryPointsFor(pol) {
  const pts = []; let gen = 1;
  (pol.schedule || []).slice().reverse().forEach(t => {
    const n = parseInt(t.interval, 10), unit = t.interval.replace(/\d/g, "");
    const mins = n * (unit === "m" ? 1 : unit === "h" ? 60 : unit === "d" ? 1440 : 10080);
    for (let i = t.versions - 1; i >= 0; i--) pts.push({generation: gen++, at: ago((mins * (i + 1)) / 60), tier: t.interval, kind: "delta", size: int(200, 9000) * 1e6});
  });
  const kept = pts.reverse().slice(0, 40).map((p, i) => Object.assign(p, {generation: pts.length - i}));
  // the earliest retained version of a chain IS the full; merging raises it
  if (kept.length) { const base = kept[kept.length - 1]; base.kind = "full"; base.size = int(9000, 40000) * 1e6; }
  return kept;
}
const DR_PHASE_CYCLE = ["FailedOver", "Deployed", "Deployed", "WaitForUser", "Deployed", "Relocating", "Deployed"];
// Hand a few consistency groups their own protection, so the group-as-unit case
// is visible: a group-consistent backup policy on one, a replication policy on
// another, and one group carrying both.
DB.consistency_groups.forEach((g, i) => {
  if (i % 3 === 0) {
    const pol = DB.backup_policies.find(p => p.cluster_id === g.cluster_id && p.consistency_group)
      || DB.backup_policies.find(p => p.cluster_id === g.cluster_id);
    if (pol) { pol.consistency_group = true; g.backup_policy = {uuid: pol.uuid, policy_name: pol.policy_name}; }
  }
  if (i % 3 !== 1) {
    // the group's own replication cadence: how often the group snapshot is taken
    // and shipped, and how many older generations the target keeps
    g.replication_config = {frequency_minutes: pick([5, 15, 30, 60]), retention: pick(SCHEDULES)};
  }
});
// An asynchronous DR policy is a pair plus a consistency group. It has no
// schedule of its own — the group's replication config is the schedule.
DB.dr_policies.filter(p => p.mode === "asynchronous").forEach(p => {
  const g = DB.consistency_groups.find(x => x.cluster_id === p.source_cluster_id && x.replication_config)
    || DB.consistency_groups.find(x => x.cluster_id === p.source_cluster_id);
  if (!g) return;
  if (!g.replication_config) g.replication_config = {frequency_minutes: p.frequency_minutes, retention: p.retention};
  p.cg_id = g.uuid; p.cg_name = g.name;
  p.lvol_ids = g.lvol_ids.slice();
});

// The DR clusters a policy protects: the Kubernetes clusters whose storage
// classes consume its source (and, async, its target) storage cluster.
// Sync: both ends consume the same stretched cluster — two different DR
// clusters must consume it for the policy to carry applications.
const drEndsOf = pol => {
  const consumers = cid => [...new Set((DB.storage_classes || []).filter(sc => sc.cluster_id === cid).map(sc => sc.k8s_cluster_id))]
    .map(kid => DB.dr_clusters.find(d => d.k8s_cluster_id === kid)).filter(Boolean);
  if (pol.mode === "synchronous") {
    const both = consumers(pol.source_cluster_id);
    return both.length >= 2 ? [both[0], both[1]] : null;
  }
  const a = consumers(pol.source_cluster_id)[0], b = consumers(pol.target_cluster_id).find(d => !a || d.uuid !== a.uuid);
  return a && b ? [a, b] : null;
};
let appCursor = 0, appSeq = 0;
DB.dr_policies.forEach(pol => {
  const ends = drEndsOf(pol);
  if (!ends || appCursor >= APP_DEFS.length) return;
  const [a, b] = ends;
  const take = Math.min(int(2, 3), APP_DEFS.length - appCursor);
  const mine = APP_DEFS.slice(appCursor, appCursor + take);
  appCursor += take;
  mine.forEach(def => {
    const [name, ns, kind] = def;
    const phase = DR_PHASE_CYCLE[appSeq % DR_PHASE_CYCLE.length];
    const isStale = appSeq === 3;           // exactly one app is deliberately behind
    appSeq++;
    const failedOver = phase === "FailedOver";
    const ivMins = pol.mode === "synchronous" ? 0 : Math.max(1, pol.frequency_minutes || 5);
    const lagMins = pol.mode === "synchronous" ? 0
      : isStale ? ivMins * (3 + rnd() * 3)
      : ivMins * (.2 + rnd() * .6);
    const preferred = failedOver ? b : a;
    const failoverTo = failedOver ? a : b;
    const claims = DB.pvcs.filter(p => p.k8s_cluster_id === preferred.k8s_cluster_id && p.namespace === ns);
    const pvcs = (claims.length ? claims : DB.pvcs.filter(p => p.k8s_cluster_id === preferred.k8s_cluster_id)).slice(0, int(1, 4));
    // the PVCs' volumes join the policy — that is what replicates the blocks
    pvcs.forEach(p => {
      const v = DB.lvols.find(x => x.uuid === p.lvol_id);
      if (!v || v.replication) return;
      pol.lvol_ids.push(v.uuid);
      v.replication = {policy_id: pol.uuid, policy_name: pol.name, mode: pol.mode, status: isStale ? "unhealthy" : "healthy",
        last_replication_at: ago(lagMins / 60), backlog_bytes: pol.mode === "synchronous" ? 0 : Math.round(v.size_util * (isStale ? .1 : .01)),
        consistency_group: pol.cg_name || null, frequency_minutes: pol.frequency_minutes, generations: pol.generations_kept};
    });
    DB.protected_apps.push({
      uuid: uuid(), app_name: name, namespace: ns, app_kind: kind,
      policy_id: pol.uuid, policy_name: pol.name,
      preferred_cluster_id: preferred.uuid, failover_cluster_id: failoverTo.uuid,
      pvc_selector: {"app.kubernetes.io/name": name.split("-")[0]},
      phase,
      progression: progressionsFor(phase)[0],
      action: phase === "FailingOver" ? "Failover" : phase === "Relocating" ? "Relocate" : null,
      pvc_ids: pvcs.map(p => p.uuid),
      vrg_state: failedOver || phase === "Deployed" ? "primary" : "secondary",
      last_group_sync_at: ago(lagMins / 60),
      last_group_sync_duration_s: Math.max(1, Math.round(Math.max(1, ivMins) * 60 * (.05 + rnd() * .3))),
      last_group_sync_bytes: int(20, 4000) * 1e6,
      // ransomware protection: recovery from group-consistent backups instead of replication
      protection_mode: appSeq % 3 === 0 ? "backup" : "replication",
      backup_policy_id: null, backup_policy_name: null, recovery_points: [], restore_point: null,
      // An application's PVC set can be pinned to a consistency group instead of
      // a label selector: the group then defines the crash-consistent boundary,
      // and every member is failed over together.
      cg_id: null, cg_name: null,
      kube_object_protection: rnd() > .4,
      // Ramen Recipe: the boot sequence. Ordered groups of Kubernetes objects
      // restored one after the other, each gated on a readiness condition, with
      // optional hooks run before/after a group.
      recipe: rnd() > .25 ? recipeFor(name, kind, ns) : null,
      created_at: ago(int(50, 2000))
    });
  });
});

(DB.protected_apps || []).forEach(a => {
  if (a.protection_mode !== "backup") return;
  const pvc = DB.pvcs.find(p => a.pvc_ids.includes(p.uuid));
  const v = pvc && DB.lvols.find(x => x.uuid === pvc.lvol_id);
  const cid = v ? v.cluster_id : (DB.clusters[0] || {}).uuid;
  const pol = DB.backup_policies.find(p => p.cluster_id === cid && p.consistency_group) || DB.backup_policies.find(p => p.consistency_group);
  if (!pol) { a.protection_mode = "replication"; return; }
  a.backup_policy_id = pol.uuid; a.backup_policy_name = pol.policy_name;
  a.recovery_points = recoveryPointsFor(pol);
  a.pvc_ids.forEach(id => { const p = DB.pvcs.find(x => x.uuid === id); const lv = p && DB.lvols.find(x => x.uuid === p.lvol_id); if (lv) { lv.backup_policy = {uuid: pol.uuid, policy_name: pol.policy_name}; lv.replication = null; } });
  if (a.phase === "FailedOver") a.restore_point = a.recovery_points[2] || a.recovery_points[0] || null;
});

// Base roughly half the protected applications on a consistency group: the
// group's members are the application's volumes, so the app inherits the
// group's crash-consistent boundary rather than matching PVCs by label.
// An application picks a DR policy, and the policy's consistency group defines
// which volumes it protects. The group is never chosen on the application.
(DB.protected_apps || []).forEach(a => {
  const pol = DB.dr_policies.find(p => p.uuid === a.policy_id);
  const cg = pol && pol.cg_id ? DB.consistency_groups.find(g => g.uuid === pol.cg_id) : null;
  if (!cg) return;
  a.cg_id = cg.uuid; a.cg_name = cg.name;
  const pvcs = cg.lvol_ids.map(vid => DB.pvcs.find(p => p.lvol_id === vid)).filter(Boolean);
  if (pvcs.length) a.pvc_ids = pvcs.map(p => p.uuid);
  if (a.protection_mode === "backup" && cg.backup_policy) {
    a.backup_policy_id = cg.backup_policy.uuid; a.backup_policy_name = cg.backup_policy.policy_name;
  }
});

// Buckets replicate like any volume. Make sure at least one cluster has both
// object storage and an async policy sourced from it, and that one of its
// buckets is attached — otherwise the bucket → policy path has no example.
(function seedReplicatedBucket() {
  const pol = DB.dr_policies.find(p => p.mode === "asynchronous");
  if (!pol) return;
  const c = DB.clusters.find(x => x.uuid === pol.source_cluster_id);
  if (!c) return;
  if (!c.object_storage.enabled) c.object_storage = {
    enabled: true, endpoint: `https://s3.${c.name}.simplyblock.internal`,
    region: pick(["eu-central-1", "us-east-2"]), addressing: "virtual-hosted",
    metadata_backend: "foundationdb", versioning_default: true, max_buckets: 500
  };
  let mine = DB.buckets.filter(b => b.cluster_id === c.uuid);
  if (!mine.length) {
    const free = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online" && !v.pvc && !v.bucket).slice(0, 3);
    free.forEach((v, i) => {
      const name = `${pick(BUCKET_NAMES)}-${String(i + 1).padStart(2, "0")}`;
      const b = {uuid: uuid(), cluster_id: c.uuid, name, lvol_id: v.uuid, lvol_name: v.lvol_name,
        pool_id: v.pool_id, pool_name: v.pool_name, status: "online", versioning: true, object_lock: false,
        quota_bytes: 0, objects: int(1200, 900000), size_bytes: v.size_util,
        region: c.object_storage.region, storage_class: "standard", owner: pick(["platform-team", "data-eng"]),
        tags: bucketTags(name), lifecycle_rules: bucketLifecycle(), cors_enabled: false,
        access: {service_account: `sb-s3-${name}`, namespace: "prod", secret_name: `${name}-s3-credentials`,
          access_key_id: `SB${hex(9).toUpperCase()}`, policy: "read-write", public: false},
        created_at: ago(int(20, 900))};
      DB.buckets.push(b); v.bucket = {uuid: b.uuid, name: b.name};
    });
    mine = DB.buckets.filter(b => b.cluster_id === c.uuid);
  }
  const b = mine.find(x => !DB.lvols.find(v => v.uuid === x.lvol_id).replication);
  if (!b) return;
  const v = DB.lvols.find(x => x.uuid === b.lvol_id);
  pol.lvol_ids.push(v.uuid);
  v.replication = {policy_id: pol.uuid, policy_name: pol.name, mode: pol.mode, status: "healthy",
    last_replication_at: ago((pol.frequency_minutes / 60) * .4), backlog_bytes: Math.round(v.size_util * .01),
    consistency_group: null, generations: pol.generations_kept};
})();

const NIC_NAMES = ["eno1", "eno2", "ens1f0", "ens1f1", "enp94s0f0", "enp94s0f1", "bond0"];
function mkNics(mgmtIp) {
  const n = int(2, 4);
  return Array.from({length: n}, (_, i) => ({
    name: NIC_NAMES[i % NIC_NAMES.length],
    mac: Array.from({length: 6}, () => hex(2)).join(":"),
    speed_gbps: pick([10, 25, 25, 100]),
    address: i === 0 ? mgmtIp : `10.${int(10, 60)}.${int(0, 40)}.${int(2, 250)}`,
    numa_socket: i < 2 ? 0 : 1, state: rnd() > .1 ? "up" : "down"
  }));
}

// Populate a discovered worker node with the inventory the inspection pod finds.
function inspectHost(h) {
  const sockets = pick([1, 2, 2]);
  h.numa_sockets = sockets;
  h.devices = [];
  const per = int(2, 5);
  for (let s = 0; s < sockets; s++) {
    for (let i = 0; i < per; i++) {
      const [model, size] = pick(MODELS);
      h.devices.push({id: uuid(), kind: "nvme", numa_socket: s,
        pcie_address: `0000:${hex(2)}:0${i}.0`, device_name: `/dev/nvme${s * per + i}n1`,
        serial_number: `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        model_number: `${model} ${(size / TB).toFixed(2)}TB`, size, assigned_node_id: null});
    }
  }
  for (let i = 0; i < int(0, 2); i++) {
    h.devices.push({id: uuid(), kind: "block", numa_socket: 0, pcie_address: null,
      device_name: `/dev/sd${"bcd"[i]}`, serial_number: null, model_number: "VIRTUAL-BLOCK",
      size: pick([1 * TB, 2 * TB]), assigned_node_id: null});
  }
  h.nics = mkNics(h.mgmt_ip);
  h.status = "inspected";
  h.inspection = {state: "complete", finished_at: ago(0)};
}

// ---- Kubernetes worker nodes that are not prepared yet ---------------------
DB.clusters.forEach(c => {
  const prefix = c.name.split("-").slice(0, 2).join("-");
  const start = DB.hosts.filter(h => h.cluster_id === c.uuid).length;
  for (let i = 0; i < int(2, 5); i++) {
    DB.hosts.push({
      uuid: uuid(), cluster_id: c.uuid,
      hostname: `${prefix}-worker-${String(start + i + 1).padStart(2, "0")}`,
      mgmt_ip: `172.20.${int(0, 40)}.${int(2, 250)}`,
      status: "discovered", source: "kubernetes",
      kubelet_version: `v1.3${int(0, 3)}.${int(1, 9)}`,
      roles: ["worker"],
      k8s_labels: {"kubernetes.io/os": "linux",
        "node.kubernetes.io/instance-type": pick(["m6id.8xlarge", "i4i.4xlarge", "bare-metal"])},
      numa_sockets: null, control_plane: false,
      zone_id: null, zone: null, region: null, k8s_cluster: null,
      rack_id: rnd() > .35 ? `r${String(int(1, 18)).padStart(2, "0")}` : null,
      cabinet_id: rnd() > .5 ? `c${String(int(1, 6)).padStart(2, "0")}` : null,
      host_class: null,
      vcpu_count: pick([32, 48, 64, 96]), memory_total: pick([128, 256, 384]) * GB,
      hugepages_reserved: 0, hugepages_allocated: 0,
      prepared_at: null, labels: {}, devices: [], nics: [], storage_node_ids: [], inspection: null
    });
  }
});
DB.hosts.filter(h => h.status !== "discovered" && !h.nics).forEach(h => { h.nics = mkNics(h.mgmt_ip); });

// A worker node belongs to a Kubernetes cluster, not to a storage cluster —
// it is a candidate precisely because it carries no storage node yet. The
// zone/cluster assignment above ran before these existed, so inherit the
// placement of a prepared sibling on the same machine pool.
DB.hosts.filter(h => h.status === "discovered" && !h.k8s_cluster_id).forEach(h => {
  const sib = DB.hosts.find(x => x.cluster_id === h.cluster_id && x.k8s_cluster_id);
  if (!sib) return;
  h.k8s_cluster_id = sib.k8s_cluster_id;
  h.k8s_cluster = sib.k8s_cluster;
  h.zone_id = sib.zone_id;
  h.zone = sib.zone;
  h.region = sib.region;
});

// ---- derived rollups (the real control plane aggregates these server-side) --
const sum = (arr, f) => arr.reduce((a, x) => a + f(x), 0);
const zeroIo = {read_io_ps: 0, write_io_ps: 0, read_bytes_ps: 0, write_bytes_ps: 0};
const addIo = (a, b) => ({read_io_ps: a.read_io_ps + b.read_io_ps, write_io_ps: a.write_io_ps + b.write_io_ps,
  read_bytes_ps: a.read_bytes_ps + b.read_bytes_ps, write_bytes_ps: a.write_bytes_ps + b.write_bytes_ps});

function rollup() {
  DB.storage_nodes.forEach(n => {
    const devs = DB.devices.filter(d => d.node_id === n.uuid);
    const live = n.status === "online" || n.status === "in_restart";
    n.devices_count = devs.length;
    n.devices_online = devs.filter(d => d.status === "online").length;
    n.size_total = sum(devs, d => d.size_total);
    n.size_util = sum(devs, d => d.size_util);
    n.io_stats = live ? devs.reduce((a, d) => addIo(a, d.io_stats), zeroIo) : zeroIo;
    n.io_history = {
      iops: devs.length && live ? devs[0].io_history.iops.map((_, i) => sum(devs, d => d.io_history.iops[i])) : series(0, 0),
      bytes: devs.length && live ? devs[0].io_history.bytes.map((_, i) => sum(devs, d => d.io_history.bytes[i])) : series(0, 0)
    };
  });
  DB.hosts.forEach(h => {
    h.storage_node_ids = DB.storage_nodes.filter(n => n.host_id === h.uuid).map(n => n.uuid);
    if (h.status !== "discovered") h.host_class = h.devices.some(d => d.kind === "nvme") ? "nvme" : "non-nvme";    h.devices_assigned = h.devices.filter(d => d.assigned_node_id).length;
    h.devices_free = h.devices.filter(d => !d.assigned_node_id).length;
    h.nvme_count = h.devices.filter(d => d.kind === "nvme").length;
    h.block_free_count = h.devices.filter(d => d.kind === "block" && !d.assigned_node_id).length;
    h.size_total = sum(h.devices, d => d.size);
    h.size_assigned = sum(h.devices.filter(d => d.assigned_node_id), d => d.size);
  });
  DB.pools.forEach(p => {
    const vs = DB.lvols.filter(v => v.pool_id === p.uuid);
    const snaps = DB.snapshots.filter(x => x.pool_id === p.uuid);
    p.snapshots_bytes = sum(snaps, x => x.size);
    p.lvols_bytes = sum(vs, v => v.size_util);
    p.lvols_count = vs.length;
    p.lvols_online = vs.filter(v => v.status === "online").length;
    p.snapshots_count = DB.snapshots.filter(s => s.pool_id === p.uuid).length;
    p.backups_count = DB.backups.filter(b => b.pool_id === p.uuid).length;
    p.size_prov = sum(vs, v => v.size_prov);
    // pool utilization is what the pool actually consumes: volume data + snapshot deltas
    p.size_util = p.lvols_bytes + p.snapshots_bytes;
  });
  DB.backups.forEach(b => {
    const vs = b.versions || [];
    b.versions_count = vs.length;
    b.full_bytes = vs.filter(x => x.type === "full").reduce((a, x) => a + x.size, 0);
    b.delta_bytes = vs.filter(x => x.type === "delta").reduce((a, x) => a + x.size, 0);
    b.size = b.full_bytes + b.delta_bytes;
    b.latest_at = vs.length ? vs[vs.length - 1].created_at : b.created_at;
    b.earliest_at = vs.length ? vs[0].created_at : b.created_at;
    b.merged_total = vs.reduce((a, x) => a + (x.merged_count || 0), 0);
  });
  DB.lvols.forEach(v => {
    const snaps = DB.snapshots.filter(s => s.lvol_id === v.uuid);
    v.snapshots_count = snaps.length;
    v.snapshots_backed_up = snaps.filter(s => s.backup_version_id).length;
    const chain = DB.backups.find(b => b.lvol_id === v.uuid);
    v.backups_count = chain ? 1 : 0;
    v.backup_versions_count = chain ? chain.versions_count : 0;
    v.backup_chain_id = chain ? chain.uuid : null;
  });
  DB.clusters.forEach(c => {
    const ns = DB.storage_nodes.filter(n => n.cluster_id === c.uuid);
    c.regions = [...new Set((c.zone_ids || []).map(id => (DB.zones.find(z => z.uuid === id) || {}).region).filter(Boolean))];
    const ds = DB.devices.filter(d => d.cluster_id === c.uuid);
    const hs = DB.hosts.filter(h => h.cluster_id === c.uuid);
    const live = c.status === "online" || c.status === "degraded";
    c.hosts_count = hs.length;
    c.hosts_available = hs.filter(h => h.status === "available").length;
    c.storage_nodes_count = ns.length;
    c.storage_nodes_online = ns.filter(n => n.status === "online").length;
    // Health is a function of the node set. Failure domains: one whole domain
    // may be down. Otherwise distr_npcs nodes may be down. Either way the
    // cluster is degraded while inside the budget and suspends beyond it.
    // unready / in_activation are lifecycle states and are not derived.
    const down = ns.filter(n => n.status !== "online");
    const downFds = [...new Set(down.map(n => n.failure_domain || n.uuid))];
    const allFds = [...new Set(ns.map(n => n.failure_domain).filter(Boolean))];
    // a domain budget only means something with at least two domains
    c.fault_budget = c.failure_domain_enabled && allFds.length >= 2
      ? {kind: "failure_domain", tolerated: 1, lost: downFds.length}
      : {kind: "nodes", tolerated: c.distr_npcs, lost: down.length};
    if (["online", "degraded", "suspended"].includes(c.status) && ns.length) {
      c.status = down.length === 0 ? "online"
        : down.length === ns.length ? "suspended"
        : c.fault_budget.lost <= c.fault_budget.tolerated ? "degraded" : "suspended";
    }
    c.devices_count = ds.length;
    c.devices_online = ds.filter(d => d.status === "online").length;
    c.pools_count = DB.pools.filter(p => p.cluster_id === c.uuid).length;
    c.lvols_count = DB.lvols.filter(v => v.cluster_id === c.uuid).length;
    c.snapshots_count = DB.snapshots.filter(s => s.cluster_id === c.uuid).length;
    c.backups_count = DB.backups.filter(b => b.cluster_id === c.uuid).length;
    c.backup_policies_count = DB.backup_policies.filter(b => b.cluster_id === c.uuid).length;
    c.replication_policies_count = (DB.dr_policies || []).filter(p => p.source_cluster_id === c.uuid).length;
    c.pairs_out_count = (DB.cluster_pairs || []).filter(p => p.source_cluster_id === c.uuid).length;
    c.pairs_in_count = (DB.cluster_pairs || []).filter(p => p.target_cluster_id === c.uuid).length;
    c.replicated_lvols_count = DB.lvols.filter(v => v.cluster_id === c.uuid && v.replication).length;
    c.consistency_groups_count = (DB.consistency_groups || []).filter(g => g.cluster_id === c.uuid).length;
    c.multipath_nodes_count = ns.filter(n => (n.data_nics || []).length > 1).length;
    c.migrations_count = (DB.migrations || []).filter(x => x.source_cluster_id === c.uuid && x.state !== "completed").length;
    c.migration_targets_count = DB.hosts.filter(h => h.cluster_id === c.uuid && h.migration_taint).length;
    // and a storage cluster can serve several Kubernetes clusters
    c.k8s_cluster_ids = [...new Set((DB.storage_classes || []).filter(x => x.cluster_id === c.uuid)
      .map(x => x.k8s_cluster_id))];
    c.pvcs_count = (DB.pvcs || []).filter(p => {
      const v = p.lvol_id ? DB.lvols.find(x => x.uuid === p.lvol_id) : null;
      return v && v.cluster_id === c.uuid;
    }).length;
    // How many volumes moved, counted from the migration tasks. This is the
    // only rebalancing telemetry there is — the rebalancer does not publish a
    // spread, a trigger, a concurrency limit or a window.
    const since = h => NOW_MS - h * 3600e3;
    const moves = (DB.tasks || []).filter(t => t.cluster_id === c.uuid && t.function_name === "lvol_migration");
    c.auto_rebalance = Object.assign({}, c.auto_rebalance, {
      moved_1h: moves.filter(t => Date.parse(t.created_at) >= since(1)).length,
      moved_24h: moves.filter(t => Date.parse(t.created_at) >= since(24)).length
    });
    c.encrypted_lvols_count = DB.lvols.filter(v => v.cluster_id === c.uuid && v.crypto_enabled).length;
    c.buckets_count = (DB.buckets || []).filter(b => b.cluster_id === c.uuid).length;
    c.buckets_bytes = (DB.buckets || []).filter(b => b.cluster_id === c.uuid).reduce((a, b) => a + (b.size_bytes || 0), 0);
    if (c.file_storage.enabled) {
      // every worker in the cluster is a pNFS data client
      c.file_storage.client_count = DB.hosts.filter(h => h.cluster_id === c.uuid && h.status === "available").length;
      c.file_storage.export_count = (DB.pvcs || []).filter(p => p.access_mode === "ReadWriteMany"
        && DB.lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
    }
    c.rwx_pvcs_count = (DB.pvcs || []).filter(p => p.access_mode === "ReadWriteMany"
      && DB.lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
    const cvs = DB.lvols.filter(v => v.cluster_id === c.uuid);
    c.reduced_lvols_count = cvs.filter(v => v.compression_dedup_enabled).length;
    c.logical_used = sum(cvs, v => v.logical_used || v.size_util);
    if (c.kms) c.kms.keys_in_use = c.encrypted_lvols_count;
    // failure domains: node counts may differ by at most one across domains
    if (c.failure_domain_enabled) {
      const byFd = {};
      ns.forEach(n => { const k = n.failure_domain || "unassigned"; byFd[k] = (byFd[k] || 0) + 1; });
      const counts = Object.values(byFd);
      c.failure_domains = Object.keys(byFd).sort().map(k => ({name: k, nodes: byFd[k]}));
      c.fd_unassigned = byFd.unassigned || 0;
      c.fd_min = counts.length ? Math.min(...counts) : 0;
      c.fd_max = counts.length ? Math.max(...counts) : 0;
      c.fd_balanced = c.fd_max - c.fd_min <= 1 && c.fd_min >= 2 && counts.length >= 2 && !c.fd_unassigned;
      c.fd_thin = c.failure_domains.filter(f => f.name !== "unassigned" && f.nodes < 2).map(f => f.name);
    } else {
      c.failure_domains = []; c.fd_unassigned = 0; c.fd_min = 0; c.fd_max = 0; c.fd_balanced = true;
    }
    c.size_total = sum(ns, n => n.size_total);
    c.size_util = sum(ns, n => n.size_util);
    c.io_stats = live ? ns.reduce((a, n) => addIo(a, n.io_stats), zeroIo) : zeroIo;
    c.io_history = {
      iops: ns.length && live ? ns[0].io_history.iops.map((_, i) => sum(ns, n => n.io_history.iops[i])) : series(0, 0),
      bytes: ns.length && live ? ns[0].io_history.bytes.map((_, i) => sum(ns, n => n.io_history.bytes[i])) : series(0, 0)
    };
  });
  (DB.protected_apps || []).forEach(ap => {
    ap.pvc_ids = ap.pvc_ids.filter(id => (DB.pvcs || []).some(p => p.uuid === id));
    ap.pvcs_count = ap.pvc_ids.length;
    const vols = ap.pvc_ids.map(id => DB.pvcs.find(p => p.uuid === id)).filter(Boolean)
      .map(p => DB.lvols.find(v => v.uuid === p.lvol_id)).filter(Boolean);
    ap.volumes = vols.map(v => ({
      lvol_id: v.uuid, lvol_name: v.lvol_name, size: v.size_util,
      last_at: ap.protection_mode === "backup"
        ? (ap.recovery_points[0] || {}).at || null
        : (v.replication || {}).last_replication_at || null,
      written_since: ap.protection_mode === "backup"
        ? Math.round(v.size_util * (.002 + Math.random() * .02))
        : (v.replication || {}).backlog_bytes || 0,
      status: (v.replication || {}).status || (ap.protection_mode === "backup" ? "healthy" : "unhealthy")
    }));
    // the group is only as current as its most stale member
    ap.group_last_at = ap.volumes.reduce((t, v) => v.last_at && (!t || Date.parse(v.last_at) < Date.parse(t)) ? v.last_at : t, null);
    ap.group_written_since = ap.volumes.reduce((n, v) => n + v.written_since, 0);
    ap.group_lag_seconds = ap.group_last_at ? Math.max(0, Math.round((NOW_MS - Date.parse(ap.group_last_at)) / 1000)) : null;
    const pol = (DB.dr_policies || []).find(x => x.uuid === ap.policy_id);
    const mins = pol ? (pol.frequency_minutes || 0) : 5;
    const lag = (NOW_MS - Date.parse(ap.last_group_sync_at)) / 60000;
    // a healthy app has synced inside its scheduling interval
    if (!(ap.legs || []).length) {
      ap.rpo_met = pol && pol.mode === "synchronous" ? true : lag <= Math.max(1, mins) * 2;
      ap.health = ap.phase === "WaitForUser" ? "unhealthy"
        : !ap.rpo_met ? "degraded"
        : ap.progression === "Completed" ? "healthy" : "degraded";
    }
  });
  // The plan layer derives its own rollup from the legs, and it has to run last
  // so the per-leg health is what the application reports.
  if (window.SB_DR) window.SB_DR.drRollup();
  // One DR policy: the storage replication policy also carries the
  // applications. Its DR clusters are the Kubernetes clusters consuming the
  // storage clusters it links; the replication class is how Kubernetes names it.
  (DB.dr_policies || []).forEach(pol => {
    const consumers = cid => [...new Set((DB.storage_classes || []).filter(sc => sc.cluster_id === cid).map(sc => sc.k8s_cluster_id))]
      .map(kid => (DB.dr_clusters || []).find(d => d.k8s_cluster_id === kid)).filter(Boolean).map(d => d.uuid);
    const ends = pol.mode === "synchronous" ? consumers(pol.source_cluster_id)
      : [...new Set(consumers(pol.source_cluster_id).concat(consumers(pol.target_cluster_id)))];
    pol.dr_cluster_ids = ends;
    pol.replication_class = `simplyblock-${pol.mode === "synchronous" ? "sync" : "async"}-${pol.name}`;
    const apps = (DB.protected_apps || []).filter(a => a.policy_id === pol.uuid);
    pol.apps_count = apps.length;
    pol.pvcs_count = apps.reduce((a, x) => a + (x.pvcs_count || 0), 0);
    pol.unhealthy_apps = apps.filter(a => a.health !== "healthy").length;
    // Ramen can only validate a policy that reaches two DR clusters
    pol.app_dr_status = ends.length >= 2 ? "Validated" : "Unavailable";
  });
  (DB.dr_clusters || []).forEach(dc => {
    dc.apps_count = (DB.protected_apps || []).filter(a => a.preferred_cluster_id === dc.uuid).length;
    dc.standby_apps_count = (DB.protected_apps || []).filter(a => a.failover_cluster_id === dc.uuid).length;
    dc.policies_count = (DB.dr_policies || []).filter(p => (p.dr_cluster_ids || []).includes(dc.uuid)).length;
  });
  (DB.k8s_clusters || []).forEach(kc => {
    const dc = (DB.dr_clusters || []).find(x => x.k8s_cluster_id === kc.uuid);
    kc.dr_cluster_id = dc ? dc.uuid : null;
    kc.protected_apps_count = dc ? dc.apps_count : 0;
  });
  DB.pools.forEach(p => {
    const scs2 = (DB.storage_classes || []).filter(x => x.pool_id === p.uuid);
    p.storage_classes = scs2.map(x => ({uuid: x.uuid, name: x.name, variant: x.variant || "default",
      k8s_cluster_id: x.k8s_cluster_id,
      k8s_cluster: (DB.k8s_clusters.find(y => y.uuid === x.k8s_cluster_id) || {}).name || null}));
    p.storage_classes_count = scs2.length;
    p.k8s_cluster_id = scs2.length ? scs2[0].k8s_cluster_id : null;
  });
  (DB.storage_classes || []).forEach(sc => {
    sc.pvcs_count = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid).length;
    sc.bound_count = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid && p.status === "Bound").length;
    sc.provisioned_bytes = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid)
      .reduce((a, p) => a + (p.actual_bytes || 0), 0);
  });
  (DB.k8s_clusters || []).forEach(kc => {
    const mySc = (DB.storage_classes || []).filter(x => x.k8s_cluster_id === kc.uuid);
    // a Kubernetes cluster can consume several storage clusters, via its storage classes
    kc.storage_cluster_ids = [...new Set(mySc.map(x => x.cluster_id).filter(Boolean))];
    kc.storage_classes_count = mySc.length;
    kc.pvcs_count = (DB.pvcs || []).filter(x => x.k8s_cluster_id === kc.uuid).length;
    kc.pvcs_bound = (DB.pvcs || []).filter(x => x.k8s_cluster_id === kc.uuid && x.status === "Bound").length;
    kc.worker_nodes_count = DB.hosts.filter(h => h.k8s_cluster_id === kc.uuid).length;
    kc.prepared_hosts_count = DB.hosts.filter(h => h.k8s_cluster_id === kc.uuid && h.status === "available").length;
    kc.namespaces = [...new Set((DB.pvcs || []).filter(x => x.k8s_cluster_id === kc.uuid).map(x => x.namespace))].sort();
    kc.provisioned_bytes = (DB.pvcs || []).filter(x => x.k8s_cluster_id === kc.uuid)
      .reduce((a, p) => a + (p.actual_bytes || 0), 0);
  });
  (DB.pvcs || []).forEach(p => {
    if (p.lvol_id && !DB.lvols.some(v => v.uuid === p.lvol_id)) { p.lvol_id = null; p.status = "Lost"; }
  });
  (DB.buckets || []).forEach(b => {
    const v = DB.lvols.find(x => x.uuid === b.lvol_id);
    if (!v) { b.status = "unavailable"; return; }
    b.size_bytes = v.size_util;
    b.provisioned_bytes = v.size_prov;
    b.status = v.status === "online" ? "online" : "unavailable";
    b.snapshots_count = DB.snapshots.filter(x => x.lvol_id === v.uuid).length;
    b.backups_count = DB.backups.filter(x => x.lvol_id === v.uuid).length;
    b.replication = v.replication || null;
    b.encrypted = !!v.crypto_enabled;
  });
  (DB.zones || []).forEach(st => {
    st.k8s_cluster_ids = (st.k8s_cluster_ids || []).filter(id => DB.k8s_clusters.some(k => k.uuid === id));
    st.k8s_clusters = st.k8s_cluster_ids.map(id => {
      const k = DB.k8s_clusters.find(x => x.uuid === id);
      return {uuid: id, name: k ? k.name : null};
    });
  });
  DB.hosts.forEach(h => {
    const z = DB.zones.find(x => x.uuid === h.zone_id);
    h.zone = z ? z.name : null;
    h.region = z ? z.region : null;
    if (z) {
      h.k8s_labels = Object.assign({}, h.k8s_labels,
        {"topology.kubernetes.io/zone": z.name, "topology.kubernetes.io/region": z.region});
      h.labels = Object.assign({}, h.labels,
        h.status === "available" ? {"topology.kubernetes.io/zone": z.name, "topology.kubernetes.io/region": z.region} : {});
    }
  });
  (DB.consistency_groups || []).forEach(g => {
    g.lvol_ids = g.lvol_ids.filter(id => DB.lvols.some(v => v.uuid === id));
    const vs = DB.lvols.filter(v => g.lvol_ids.includes(v.uuid));
    g.lvols_count = vs.length;
    g.size_prov = sum(vs, v => v.size_prov);
    g.size_util = sum(vs, v => v.size_util);
    g.snapshots_count = (DB.cg_snapshots || []).filter(x => x.cg_id === g.uuid).length;
    g.backed_up_count = (DB.cg_snapshots || []).filter(x => x.cg_id === g.uuid && x.backup_version_id).length;
    g.status = vs.length < 2 ? "degraded" : vs.every(v => v.status === "online") ? "online" : "degraded";
    const pol = (DB.dr_policies || []).find(p => p.consistency_group && p.lvol_ids.some(id => g.lvol_ids.includes(id)));
    g.replication_policy = pol ? {uuid: pol.uuid, name: pol.name} : null;
  });
  (DB.protected_apps || []).forEach(a => {
    if (!a.action_started_ms) return;
    const age = Date.now() - a.action_started_ms;
    if (a.phase === "FailingOver") {
      const backup = a.progression === "RestoringGeneration" || a.pinned_generation != null;
      const steps = progressionsFor(backup ? "RestoringGeneration" : "FailingOver");
      const per = backup ? 4500 : 4000;
      a.progression = steps[Math.min(steps.length - 1, Math.floor(age / per))];
      if (age > per * steps.length) {
        const t = a.preferred_cluster_id;
        a.preferred_cluster_id = a.failover_cluster_id; a.failover_cluster_id = t;
        a.phase = a.kube_object_protection ? "FailedOver" : "WaitForUser";
        a.progression = progressionsFor(a.phase)[0];
        a.action = null; a.vrg_state = "primary";
        a.last_group_sync_at = ago(0); a.action_started_ms = null;
        if (a.pinned_generation != null) {
          // Payload 2 is cleared once the promote has resolved: a stale pin
          // would make the next ordinary failover resolve to an old generation.
          a.restored_from_generation = a.pinned_generation;
          a.pinned_generation = null;
          a.active_site = a.restore_target || a.active_site;
          // The original site's pre-compromise state cannot be reconstructed:
          // the source volume is gone or untrusted and the vault holds
          // generations, not a live peer. The application is re-protected as a
          // new plan with a full baseline instead.
          a.failover_targets = [];
          a.needs_reprotect = true;
        } else {
          const leg = (a.legs || []).find(l => l.orchestrated);
          if (leg && leg.target) a.active_site = leg.target;
        }
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    } else if (a.phase === "Relocating") {
      const steps = progressionsFor("Relocating");
      if (age > 4000 && a.progression === steps[0]) a.progression = steps[1];
      if (age > 8000) {
        const t = a.preferred_cluster_id;
        a.preferred_cluster_id = a.failover_cluster_id; a.failover_cluster_id = t;
        a.phase = "Deployed"; a.progression = progressionsFor("Deployed")[0]; a.action = null;
        a.vrg_state = "primary"; a.last_group_sync_at = ago(0); a.action_started_ms = null;
        if (a.preferred_site) a.active_site = a.preferred_site;
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    }
  });
  (DB.migrations || []).forEach(mg => {
    mg.lvol_ids = mg.lvol_ids.filter(id => DB.lvols.some(v => v.uuid === id));
    mg.lvols_count = mg.lvol_ids.length;
    mg.progress_pct = mg.state === "completed" ? 100
      : mg.mode === "intra_cluster"
        ? (mg.lvols_count ? Math.round((mg.moved_count / mg.lvols_count) * 100) : 0)
        : Math.min(99, Math.round((1 - Math.log(Math.max(1, mg.last_snapshot_bytes)) / Math.log(Math.max(2, mg.first_snapshot_bytes))) * 100));
    mg.ready_to_cutover = mg.mode === "cross_cluster" && mg.last_snapshot_bytes <= mg.freeze_threshold_bytes
      && ["converging", "cutover_pending", "replicating"].includes(mg.state);
  });
  // A group that owns a policy owns it for every member: the group's policy is
  // pushed down onto the member volumes, so a volume can never sit in a
  // group-consistent policy while pointing at a different one of its own.
  DB.lvols.forEach(v => { v.consistency_groups = []; });
  DB.consistency_groups.forEach(g => {
    g.lvol_ids = g.lvol_ids.filter(id => DB.lvols.some(v => v.uuid === id));
    const members = DB.lvols.filter(v => g.lvol_ids.includes(v.uuid));
    members.forEach(v => v.consistency_groups.push({uuid: g.uuid, name: g.name}));
    // A group with active protection is frozen: its membership defines the
    // crash-consistent set the policy operates on, and changing it would make
    // the retained versions and the replica stream disagree about what the set
    // is. Overlapping groups are how a different set gets protected instead.
    g.locked = !!(g.backup_policy || g.replication_config);
    g.dr_policy_ids = DB.dr_policies.filter(p => p.cg_id === g.uuid).map(p => p.uuid);
    if (g.backup_policy) {
      const pol = DB.backup_policies.find(p => p.uuid === g.backup_policy.uuid);
      if (!pol) g.backup_policy = null;
      else { g.backup_policy.policy_name = pol.policy_name;
        members.forEach(v => { v.backup_policy = {uuid: pol.uuid, policy_name: pol.policy_name, via_cg: g.uuid}; }); }
    }
    if (g.replication_config) {
      // every DR policy that names this group replicates exactly its members
      const pols = DB.dr_policies.filter(p => p.cg_id === g.uuid);
      pols.forEach(p => {
        p.lvol_ids = g.lvol_ids.slice();
        p.frequency_minutes = g.replication_config.frequency_minutes;
        p.retention = g.replication_config.retention;
        p.cg_name = g.name;
      });
      const p0 = pols[0];
      members.forEach(v => {
        v.replication = Object.assign({}, v.replication || {}, {
          policy_id: p0 ? p0.uuid : null, policy_name: p0 ? p0.name : null,
          mode: p0 ? p0.mode : "asynchronous",
          consistency_group: g.name, via_cg: g.uuid,
          frequency_minutes: g.replication_config.frequency_minutes,
          status: v.replication && v.replication.status ? v.replication.status : "healthy",
          last_replication_at: (v.replication && v.replication.last_replication_at) || ago(g.replication_config.frequency_minutes / 120),
          backlog_bytes: (v.replication && v.replication.backlog_bytes) || 0});
      });
    }
    g.replication_status = g.replication_config
      ? (members.some(v => v.replication && v.replication.status === "unhealthy") ? "unhealthy" : "healthy") : null;
    g.replication_backlog_bytes = members.reduce((n, v) => n + ((v.replication && v.replication.backlog_bytes) || 0), 0);
    g.replication_last_at = members.reduce((t, v) => {
      const at = v.replication && v.replication.last_replication_at;
      return at && (!t || Date.parse(at) < Date.parse(t)) ? at : t;
    }, null);
    g.apps_count = (DB.protected_apps || []).filter(a => a.cg_id === g.uuid).length;
  });
  DB.backup_policies.forEach(p => {
    p.cgroups_count = DB.consistency_groups.filter(g => g.backup_policy && g.backup_policy.uuid === p.uuid).length;
    p.lvols_count = DB.lvols.filter(v => v.backup_policy && v.backup_policy.uuid === p.uuid).length;
    p.chains_count = DB.backups.filter(b => b.policy_id === p.uuid).length;
    p.versions_total = p.schedule.reduce((a, r) => a + r.versions, 0);
    p.online_snapshots = p.schedule.reduce((a, r) => a + (r.online || 0), 0);
    p.finest_interval = p.schedule.length ? p.schedule[0].interval : null;
  });
  (DB.zones || []).forEach(s => {
    const hs = DB.hosts.filter(h => h.zone_id === s.uuid);
    s.hosts_count = hs.length;
    s.hosts_prepared = hs.filter(h => h.status === "available").length;
    s.nvme_hosts = hs.filter(h => h.host_class === "nvme").length;
    s.nodes_count = DB.storage_nodes.filter(n => n.zone_id === s.uuid).length;
    s.cluster_ids = [...new Set(hs.map(h => h.cluster_id).filter(Boolean))];
    s.racks = [...new Set(hs.map(h => h.rack_id).filter(Boolean))].sort();
    s.untainted_hosts = hs.filter(h => !h.rack_id).length;
    s.size_total = sum(hs, h => h.size_total || 0);
  });
}
rollup();

// Jitter leaf-level counters so polled requests return moving numbers.
function jitter() {
  // the NFS metadata server comes back on its new worker within a few seconds
  DB.clusters.forEach(c => {
    const f = c.file_storage;
    if (f && f.enabled && f.mds_state === "restarting" && f.mds_restart_ms
        && Date.now() - f.mds_restart_ms > (f.failover_budget_seconds || 8) * 1000) {
      f.mds_state = "active";
      f.mds_restart_ms = null;
    }
  });
  // an inspection pod finishes a few seconds after it is scheduled
  DB.hosts.forEach(h => {
    if (h.inspection && h.inspection.state === "running" && Date.now() - h.inspection.started_ms > 4500) inspectHost(h);
  });
  // Node lifecycle operations are asynchronous and phased. Every phase is a
  // named subtask; the last phase applies the effect on the cluster.
  DB.storage_nodes.slice().forEach(n => {
    if (!n.op || typeof n.op.started_ms !== "number") return;   // operator-driven ops advance in mock-k8s
    const spec = NODE_OPS[n.op.kind]; if (!spec) return;
    const idx = Math.min(spec.phases.length - 1, Math.floor((Date.now() - n.op.started_ms) / spec.every));
    const task = (DB.tasks || []).find(t => t.uuid === n.op.task_id);
    const subs = (DB.tasks || []).filter(t => t.parent_id === n.op.task_id);
    for (let i = 0; i < idx; i++) {
      const t2 = subs.find(x => x.function_name === spec.subtasks[i]);
      if (t2 && t2.status !== "done") { t2.status = "done"; t2.result = "done"; t2.updated_at = ago(0); }
    }
    if (spec.phases[idx] === n.op.phase) return;
    n.op.phase = spec.phases[idx]; n.op.phase_index = idx;
    if (idx < spec.phases.length - 1) return;
    // terminal phase: apply the effect
    const host = DB.hosts.find(x => x.uuid === n.host_id);
    if (n.op.kind === "removal") {
      if (host) { host.devices.forEach(d => { if (d.assigned_node_id === n.uuid) d.assigned_node_id = null; }); host.storage_node_ids = host.storage_node_ids.filter(x => x !== n.uuid); }
      DB.devices = DB.devices.filter(d => d.node_id !== n.uuid);
      DB.storage_nodes = DB.storage_nodes.filter(x => x.uuid !== n.uuid);
      if (task) { task.status = "done"; task.result = `Node removed, ${n.op.volumes_moved} volume(s) moved`; task.updated_at = ago(0); }
      rollup(); return;
    }
    if (n.op.kind === "migration") {
      const tgt = DB.hosts.find(x => x.uuid === n.op.target_host_id);
      if (host) { host.devices.forEach(d => { if (d.assigned_node_id === n.uuid) d.assigned_node_id = null; }); host.storage_node_ids = host.storage_node_ids.filter(x => x !== n.uuid); }
      if (tgt) {
        n.host_id = tgt.uuid; n.mgmt_ip = tgt.mgmt_ip; n.hostname = tgt.hostname;
        if (!tgt.storage_node_ids.includes(n.uuid)) tgt.storage_node_ids.push(n.uuid);
        tgt.devices.filter(d => !d.assigned_node_id).forEach(d => d.assigned_node_id = n.uuid);
        DB.devices.filter(d => d.node_id === n.uuid).forEach(d => { d.host_id = tgt.uuid; d.status = "online"; });
      }
    }
    n.status = "online";
    DB.devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status === "unavailable" || d.status === "new") d.status = "online"; });
    if (task) { task.status = "done"; task.result = spec.done; task.updated_at = ago(0); }
    n.op = null; rollup();
  });
  const drift = o => {
    if (!o.io_stats) return;
    const live = ["online", "read_only"].includes(o.status);
    const f = live ? 1 + (Math.random() - .5) * .14 : 0;
    o.io_stats = {read_io_ps: Math.round(o.io_stats.read_io_ps * f), write_io_ps: Math.round(o.io_stats.write_io_ps * f),
      read_bytes_ps: Math.round(o.io_stats.read_bytes_ps * f), write_bytes_ps: Math.round(o.io_stats.write_bytes_ps * f)};
    o.io_history.iops.push(o.io_stats.read_io_ps + o.io_stats.write_io_ps); o.io_history.iops.shift();
    o.io_history.bytes.push(o.io_stats.read_bytes_ps + o.io_stats.write_bytes_ps); o.io_history.bytes.shift();
  };
  DB.devices.forEach(drift);
  DB.lvols.forEach(drift);
  (DB.protected_apps || []).forEach(a => {
    if (!a.action_started_ms) return;
    const age = Date.now() - a.action_started_ms;
    if (a.phase === "FailingOver") {
      const backup = a.progression === "RestoringGeneration" || a.pinned_generation != null;
      const steps = progressionsFor(backup ? "RestoringGeneration" : "FailingOver");
      const per = backup ? 4500 : 4000;
      a.progression = steps[Math.min(steps.length - 1, Math.floor(age / per))];
      if (age > per * steps.length) {
        const t = a.preferred_cluster_id;
        a.preferred_cluster_id = a.failover_cluster_id; a.failover_cluster_id = t;
        a.phase = a.kube_object_protection ? "FailedOver" : "WaitForUser";
        a.progression = progressionsFor(a.phase)[0];
        a.action = null; a.vrg_state = "primary";
        a.last_group_sync_at = ago(0); a.action_started_ms = null;
        if (a.pinned_generation != null) {
          // Payload 2 is cleared once the promote has resolved: a stale pin
          // would make the next ordinary failover resolve to an old generation.
          a.restored_from_generation = a.pinned_generation;
          a.pinned_generation = null;
          a.active_site = a.restore_target || a.active_site;
          // The original site's pre-compromise state cannot be reconstructed:
          // the source volume is gone or untrusted and the vault holds
          // generations, not a live peer. The application is re-protected as a
          // new plan with a full baseline instead.
          a.failover_targets = [];
          a.needs_reprotect = true;
        } else {
          const leg = (a.legs || []).find(l => l.orchestrated);
          if (leg && leg.target) a.active_site = leg.target;
        }
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    } else if (a.phase === "Relocating") {
      const steps = progressionsFor("Relocating");
      if (age > 4000 && a.progression === steps[0]) a.progression = steps[1];
      if (age > 8000) {
        const t = a.preferred_cluster_id;
        a.preferred_cluster_id = a.failover_cluster_id; a.failover_cluster_id = t;
        a.phase = "Deployed"; a.progression = progressionsFor("Deployed")[0]; a.action = null;
        a.vrg_state = "primary"; a.last_group_sync_at = ago(0); a.action_started_ms = null;
        if (a.preferred_site) a.active_site = a.preferred_site;
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    }
  });
  (DB.migrations || []).forEach(mg => {
    if (["completed", "paused", "failed"].includes(mg.state)) return;
    if (mg.mode === "intra_cluster") {
      // instant migration: move one volume's primary per tick onto a tainted target
      if (mg.moved_count >= mg.lvols_count) { mg.state = "completed"; mg.completed_at = ago(0); return; }
      const targets = DB.storage_nodes.filter(n => {
        if (n.cluster_id !== mg.target_cluster_id || n.status !== "online") return false;
        const h = DB.hosts.find(x => x.uuid === n.host_id);
        return h && (h.migration_taint || (mg.target_zone_id && h.zone_id === mg.target_zone_id));
      });
      if (!targets.length) return;
      const v = DB.lvols.find(x => mg.lvol_ids.includes(x.uuid) && x.nodes && x.nodes.primary
        && !targets.some(t => t.uuid === x.nodes.primary.uuid));
      if (!v) { mg.state = "completed"; mg.completed_at = ago(0); return; }
      const t = targets[mg.moved_count % targets.length];
      const from = v.nodes.primary.hostname;
      v.nodes = Object.assign({}, v.nodes, {primary: {uuid: t.uuid, hostname: t.hostname}});
      v.migration = {state: "completed", instant: true, from, target: t.hostname,
        reason: "cluster_migration", queued_at: ago(0), completed_at: ago(0)};
      mg.moved_count++;
      mg.state = mg.moved_count >= mg.lvols_count ? "completed" : "running";
      if (mg.state === "completed") mg.completed_at = ago(0);
      return;
    }
    // cross-cluster: iterate snapshots, each smaller than the last
    if (mg.state === "replicating" || mg.state === "converging") {
      mg.iterations++;
      mg.last_snapshot_bytes = Math.max(20e6, Math.round(mg.last_snapshot_bytes / (1.6 + Math.random())));
      mg.state = mg.last_snapshot_bytes <= mg.freeze_threshold_bytes ? "cutover_pending" : "converging";
    }
    mg.estimated_freeze_ms = Math.max(90, Math.round(mg.last_snapshot_bytes / 1.2e6));
    mg.throughput_bytes_ps = Math.max(2e7, Math.round(mg.throughput_bytes_ps * (1 + (Math.random() - .5) * .3)));
  });
  (DB.dr_policies || []).forEach(p => {
    p.lvols_count = p.lvol_ids.length;
    p.generations_kept = p.retention.reduce((a, r) => a + r.keep, 0);
    if (p.mode === "synchronous") { p.backlog_bytes = 0; p.last_replication_at = ago(0); return; }
    const pair = (DB.cluster_pairs || []).find(x => x.uuid === p.pair_id);
    if (pair && pair.state === "unreachable") { p.state = "unhealthy"; p.backlog_bytes += 4e8; return; }
    p.backlog_bytes = Math.max(0, Math.round(p.backlog_bytes * (1 + (Math.random() - .58) * .3)));
    const budget = p.frequency_minutes * 60 * 8e6;
    p.state = p.backlog_bytes > budget * 4 ? "unhealthy" : p.backlog_bytes > budget ? "degraded" : "healthy";
    if (Math.random() > .7) p.last_replication_at = ago(Math.random() * (p.frequency_minutes / 60));
    p.lvol_ids.forEach(id => {
      const v = DB.lvols.find(x => x.uuid === id);
      if (!v || !v.replication) return;
      v.replication.backlog_bytes = Math.max(0, Math.round(v.replication.backlog_bytes * (1 + (Math.random() - .58) * .35)));
      v.replication.status = v.replication.backlog_bytes > v.size_util * .06 ? "unhealthy" : "healthy";
      if (Math.random() > .75) v.replication.last_replication_at = ago(Math.random() * (p.frequency_minutes / 60));
    });
  });
  (DB.cluster_pairs || []).forEach(p => {
    p.policies_count = (DB.dr_policies || []).filter(x => x.pair_id === p.uuid).length;
    if (p.state === "unreachable") { p.link.throughput_bytes_ps = 0; return; }
    p.link.throughput_bytes_ps = Math.max(1e6, Math.round(p.link.throughput_bytes_ps * (1 + (Math.random() - .5) * .25)));
    p.link.rtt_ms = +Math.max(1, p.link.rtt_ms + (Math.random() - .5) * 2).toFixed(1);
  });
  rollup();
}

window.SB_NOW = NOW_MS;
window.SB_DB = DB;
window.SB_JITTER = jitter;
window.SB_UTIL = {uuid, hex, int, pick, ago, series, ioStats, rollup, inspectHost, mkNics, GB, TB, MODELS, NODE_OPS};
