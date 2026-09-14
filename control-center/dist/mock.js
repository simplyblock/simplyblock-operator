// generated from the mock-*.jsx + k8s-client.jsx sources — do not edit; rebuild with build.md
// ---- mock-backend.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK BACKEND — in-memory fixture store shaped like control plane API v2
// payloads (snake_case, flat collections, uuid foreign keys).
// Nothing in the UI reads this file: it is only served by mock-api.jsx.
// ---------------------------------------------------------------------------
const rnd = (s => () => (s = s * 1664525 + 1013904223 >>> 0) / 4294967296)(20260808);
const pick = a => a[Math.floor(rnd() * a.length)];
const int = (a, b) => a + Math.floor(rnd() * (b - a + 1));
const hex = n => Array.from({
  length: n
}, () => "0123456789abcdef"[Math.floor(rnd() * 16)]).join("");
const uuid = () => `${hex(8)}-${hex(4)}-4${hex(3)}-a${hex(3)}-${hex(12)}`;
const GB = 1e9,
  TB = 1e12;
const series = (base, jit) => Array.from({
  length: 28
}, () => Math.max(0, Math.round(base * (1 + (rnd() - .5) * jit))));
// Single reference clock. ago() and every freshness/RPO check must use this and
// nothing else — two anchors is how the RPO metric silently broke before.
const NOW_MS = Date.parse("2026-09-01T09:00:00Z");
const ago = h => new Date(NOW_MS - h * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");
const DB = {
  clusters: [],
  hosts: [],
  storage_nodes: [],
  devices: [],
  pools: [],
  lvols: [],
  snapshots: [],
  backups: [],
  backup_policies: [],
  consistency_groups: [],
  cg_snapshots: [],
  migrations: [],
  k8s_clusters: [],
  storage_classes: [],
  pvcs: [],
  buckets: [],
  dr_clusters: [],
  protected_apps: []
};
const FD = ["rack-a1", "rack-a2", "rack-b1", "rack-b2", "az-1", "az-2", "az-3"];
const PHYS = ["dell-r750-01", "dell-r750-02", "smc-2124us", "hpe-dl385", "gigabyte-r282", null];
const MODELS = [["KIOXIA CM7-R", 7.68 * TB], ["SAMSUNG PM9A3", 3.84 * TB], ["INTEL SSDPF2KX064T1", 6.4 * TB], ["MICRON 7450 PRO", 15.36 * TB]];
const POOL_NAMES = ["default", "gold-tier", "silver-tier", "bronze-tier", "analytics", "db-prod", "ci-scratch"];
const VOL_PREFIX = ["pg", "mysql", "kafka", "etcd", "minio", "redis", "elastic", "vm", "registry", "clickhouse", "grafana"];
const BUCKETS = ["s3://sb-backup-eu/", "s3://sb-backup-us/", "s3://sb-archive-cold/"];

// Provider-shaped KMS records — a Vault mount means nothing to AWS KMS.
function kmsFor(provider, name, i) {
  const common = {
    provider,
    key_name: `sb-${name}-dek`,
    key_type: "aes256-gcm96",
    verify_tls: true,
    status: rnd() > .12 ? "connected" : pick(["unreachable", "sealed"]),
    last_check_at: ago(rnd() * 2),
    keys_in_use: 0,
    rotation_days: pick([30, 90, 180, 0])
  };
  const region = pick(["eu-central-1", "us-east-2", "eu-north-1"]);
  if (provider === "aws_kms") return Object.assign(common, {
    address: `https://kms.${region}.amazonaws.com`,
    region,
    auth_method: pick(["irsa", "irsa", "access_key"]),
    auth_role: `arn:aws:iam::${int(100000000000, 999999999999)}:role/simplyblock-kms`
  });
  if (provider === "azure_key_vault") return Object.assign(common, {
    address: `https://sb-${name.split("-")[0]}-kv.vault.azure.net`,
    auth_method: pick(["workload_identity", "workload_identity", "client_secret"]),
    auth_role: `${hex(8)}-${hex(4)}-${hex(4)}-${hex(4)}-${hex(12)}`
  });
  if (provider === "gcp_kms") return Object.assign(common, {
    address: `projects/sb-${name.split("-")[0]}/locations/${region}/keyRings/simplyblock`,
    auth_method: "workload_identity"
  });
  if (provider === "kmip") return Object.assign(common, {
    address: `kmip-${i + 1}.simplyblock.internal:5696`,
    auth_method: "certificate",
    auth_role: "sb-kmip-client"
  });
  return Object.assign(common, {
    address: `https://vault-${i + 1}.simplyblock.internal:8200`,
    namespace: rnd() > .6 ? "admin/storage" : null,
    auth_method: pick(["kubernetes", "kubernetes", "approle", "token"]),
    auth_role: "simplyblock-storage",
    mount_path: "transit"
  });
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
      sockets,
      model,
      size,
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
      devIops: int(9000, 145000),
      devBwPerIo: int(3800, 9200)
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
const ioStats = (io, bw) => ({
  read_io_ps: Math.round(io * .6),
  write_io_ps: Math.round(io * .4),
  read_bytes_ps: Math.round(bw * .6),
  write_bytes_ps: Math.round(bw * .4)
});
const qosOf = force => !force && rnd() > .55 ? null : {
  rw_ios_per_sec: pick([0, 25000, 50000, 100000, 200000]),
  rw_mbytes_per_sec: pick([0, 500, 1000, 2000]),
  r_mbytes_per_sec: pick([0, 0, 1500]),
  w_mbytes_per_sec: pick([0, 0, 800])
};

// ---- hosts -----------------------------------------------------------------
function seedHost(c, idx, unassigned) {
  const S = sku(c.uuid);
  const sockets = S.sockets;
  const status = unassigned ? "available" : rnd() > .9 ? "unreachable" : "available";
  const h = {
    uuid: uuid(),
    cluster_id: c.uuid,
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
    zone_id: null,
    zone: null,
    region: null,
    k8s_cluster: null,
    // operators taint hosts to mark them as migration targets
    migration_taint: null,
    // worker nodes may or may not be tainted with zone / rack / cabinet
    rack_id: rnd() > .18 ? `r${String(int(1, 18)).padStart(2, "0")}` : null,
    cabinet_id: rnd() > .3 ? `c${String(int(1, 6)).padStart(2, "0")}` : null,
    labels: {
      "simplyblock.io/storage-node": "true",
      "topology.kubernetes.io/zone": pick(FD)
    },
    devices: [],
    storage_node_ids: []
  };
  const nvmePerSocket = S.nvmePerSocket;
  const model = S.model,
    size = S.size;
  for (let s = 0; s < sockets; s++) {
    for (let i = 0; i < nvmePerSocket; i++) {
      h.devices.push({
        id: uuid(),
        kind: "nvme",
        numa_socket: s,
        pcie_address: `0000:${hex(2)}:0${i}.0`,
        device_name: `/dev/nvme${s * nvmePerSocket + i}n1`,
        serial_number: `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        model_number: `${model} ${(size / TB).toFixed(2)}TB`,
        size,
        assigned_node_id: null
      });
    }
  }
  for (let i = 0; i < S.blockCount; i++) {
    h.devices.push({
      id: uuid(),
      kind: "block",
      numa_socket: 0,
      pcie_address: null,
      device_name: `/dev/sd${"bcdef"[i]}`,
      serial_number: null,
      model_number: "VIRTUAL-BLOCK",
      size: S.size,
      assigned_node_id: null
    });
  }
  DB.hosts.push(h);
  h.host_class = h.devices.some(d => d.kind === "nvme") ? "nvme" : "non-nvme";
  return h;
}
function seedCluster(name, device_class, status, location_type, i) {
  const edge = location_type === "edge";
  const c = {
    uuid: uuid(),
    name,
    device_class,
    status,
    location_type,
    // edge clusters neither rebalance nor run a task engine
    rebalancing: edge ? false : rnd() > .68,
    ha_type: pick(["ha", "ha", "single"]),
    distr_npcs: pick([1, 1, 2]),
    distr_ndcs: pick([2, 4]),
    blk_size: 4096,
    page_size_in_blocks: 2097152,
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
      verify_tls: true,
      addressing: pick(["virtual-hosted", "path"])
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
    file_storage: !edge && rnd() > .45 ? {
      enabled: true,
      nfs_version: "4.2",
      layout_type: "flexfile",
      export_root: "/export/simplyblock",
      mds_host: null,
      mds_candidates: [],
      mds_state: rnd() > .12 ? "active" : pick(["restarting", "electing"]),
      mds_restarted_at: rnd() > .6 ? ago(int(1, 400)) : null,
      lease_seconds: pick([10, 20, 30]),
      grace_seconds: pick([15, 45, 90]),
      failover_budget_seconds: pick([5, 8, 12]),
      // pNFS on the Linux kernel NFS server only supports XFS
      filesystem: "xfs",
      max_exports: pick([64, 128, 256])
    } : {
      enabled: false
    },
    // ---- S3: blobs on cluster capacity, metadata in FoundationDB -----------
    object_storage: !edge && rnd() > .45 ? {
      enabled: true,
      endpoint: `https://s3.${name}.simplyblock.internal`,
      region: pick(["eu-central-1", "us-east-2", "eu-north-1"]),
      addressing: pick(["virtual-hosted", "path"]),
      metadata_backend: "foundationdb",
      versioning_default: rnd() > .5,
      max_buckets: pick([100, 500, 1000])
    } : {
      enabled: false
    },
    // Multipathing: with two data NICs per storage node the client gets two
    // paths and uses them automatically. Fixed at creation, because it decides
    // how many NICs every node must be given.
    multipathing_enabled: !edge && rnd() > .35,
    // Instant volume migration (26.3.0): the logical volume's primary moves
    // between nodes without copying data. Rebalancing uses the same mechanism.
    auto_rebalance: {
      enabled: !edge && rnd() > .4
    },
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
  const hostCount = nodeCount + int(1, 2); // spare, prepared hosts
  const hosts = Array.from({
    length: hostCount
  }, (_, hi) => seedHost(c, hi, hi >= nodeCount));

  // Redundancy decides how many nodes may be lost: distr_npcs parity chunks
  // (1 or 2) — or, with failure domains, everything inside one domain. Within
  // that budget the cluster is degraded; beyond it, it suspends itself. The
  // fixture picks a node set that lands exactly on the intended state.
  const budget = c.distr_npcs;
  const downCount = status === "online" ? 0 : status === "degraded" ? int(1, budget) : status === "suspended" ? nodeCount : 0;
  const downIdx = new Set();
  while (downIdx.size < downCount) downIdx.add(int(0, nodeCount - 1));
  for (let ni = 0; ni < nodeCount; ni++) {
    const host = hosts[ni];
    if (status === "online" || status === "degraded") host.status = "available";
    const nstatus = status === "in_activation" ? pick(["in_restart", "online", "offline"]) : downIdx.has(ni) ? status === "suspended" ? pick(["offline", "offline", "down"]) : pick(["down", "unreachable", "offline", "in_restart"]) : "online";
    const n = {
      uuid: uuid(),
      cluster_id: c.uuid,
      host_id: host.uuid,
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
      objects_used: near(30000, .2),
      objects_max: 65536,
      latency_us: near(320, .18),
      // set when the node was stopped on purpose, so the rules can tell a
      // maintenance shutdown from a failure
      maintenance: false
    };
    n.data_nics = Array.from({
      length: c.multipathing_enabled ? 2 : 1
    }, (_, k) => ({
      name: k === 0 ? "ens1f0" : "ens1f1",
      ip: `10.${int(10, 60)}.${int(0, 40)}.${int(2, 250)}`,
      port: 4420 + k,
      numa_socket: k,
      state: "up"
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
      const dstatus = nstatus === "online" ? pick(["online", "online", "online", "online", "online", "online", "read_only", "unavailable", "new"]) : pick(["unavailable", "unavailable", "online", "removed"]);
      const live = dstatus === "online" || dstatus === "read_only";
      const S = sku(c.uuid);
      const io = live ? near(S.devIops) : 0,
        bw = live ? Math.round(io * near(S.devBwPerIo, .04)) : 0;
      DB.devices.push({
        uuid: uuid(),
        node_id: n.uuid,
        cluster_id: c.uuid,
        host_id: host.uuid,
        cluster_device_class: device_class,
        numa_socket: hd.numa_socket,
        serial_number: hd.serial_number || `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        pcie_address: hd.pcie_address,
        device_name: hd.device_name,
        model_number: hd.model_number,
        firmware_revision: `GXA7${int(10, 99)}1`,
        status: dstatus,
        health_check: live ? pick(["good", "good", "good", "good", "warn", "critical"]) : null,
        size_total: hd.size,
        size_util: live ? Math.round(hd.size * devFill(c.uuid)) : 0,
        temperature_c: near(46, .3),
        percentage_used: near(14, .8),
        power_on_hours: near(16000, .25),
        io_stats: ioStats(io, bw),
        io_history: {
          iops: series(io, .3),
          bytes: series(bw, .3)
        }
      });
    });
  }

  // ---- backup policies ----
  // A schedule row is: interval · how many backup versions are retained ·
  // (optionally) how many snapshots of that tier stay online. The interval is
  // both the snapshot/backup periodicity AND, once the version count is
  // exceeded, the merge cadence for that tier.
  const POLICIES = [{
    policy_name: "5m-tiered",
    schedule: [{
      interval: "5m",
      versions: 12,
      online: 3
    }, {
      interval: "1h",
      versions: 11
    }, {
      interval: "1d",
      versions: 6
    }]
  }, {
    policy_name: "hourly-24h",
    schedule: [{
      interval: "1h",
      versions: 24,
      online: 2
    }, {
      interval: "1d",
      versions: 7
    }]
  }, {
    policy_name: "daily-30d",
    schedule: [{
      interval: "1d",
      versions: 30,
      online: 1
    }]
  }, {
    policy_name: "15m-longhaul",
    schedule: [{
      interval: "15m",
      versions: 8,
      online: 4
    }, {
      interval: "6h",
      versions: 12
    }, {
      interval: "1d",
      versions: 14
    }, {
      interval: "7d",
      versions: 8
    }]
  },
  // group-consistent: every cycle snapshots all linked volumes as one consistency group,
  // and the whole retention is kept — the ransomware recovery basis
  {
    policy_name: "ransomware-cg-1h",
    consistency_group: true,
    schedule: [{
      interval: "1h",
      versions: 24,
      online: 2
    }, {
      interval: "1d",
      versions: 14
    }, {
      interval: "7d",
      versions: 8
    }]
  }];
  const clusterPolicies = POLICIES.slice(0, int(2, 4)).concat([POLICIES[4]]).map(p => {
    const rec = Object.assign({
      uuid: uuid(),
      cluster_id: c.uuid,
      created_at: ago(int(200, 2000))
    }, p);
    DB.backup_policies.push(rec);
    return rec;
  });

  // ---- pools, volumes, snapshots, backups ----
  const poolCount = int(2, 5);
  for (let pi = 0; pi < poolCount; pi++) {
    // a pool is never offline; it is enabled or disabled. Disabled pools keep
    // serving their volumes but refuse new provisioning.
    const p = {
      uuid: uuid(),
      cluster_id: c.uuid,
      pool_name: POOL_NAMES[pi % POOL_NAMES.length],
      // bi-directional DH-CHAP is set when the pool is created and never changes
      dhchap_bidirectional: rnd() > .6,
      enabled: rnd() > .15,
      qos: qosOf(pi === 0)
    };
    DB.pools.push(p);
    const clusterNodes = DB.storage_nodes.filter(n => n.cluster_id === c.uuid);
    const volCount = int(3, 8);
    for (let vi = 0; vi < volCount; vi++) {
      const vstatus = rnd() > .1 ? "online" : "offline";
      const live = vstatus === "online";
      const prov = pick([100 * GB, 250 * GB, 500 * GB, 1 * TB, 2 * TB, 4 * TB]);
      const io = live ? int(1200, 68000) : 0,
        bw = live ? io * int(4000, 12000) : 0;
      // stride of 1 keeps placement even for any node count, and the primary
      // prefers an online node so front storage is where it can serve
      const onlineNodes = clusterNodes.filter(n => n.status === "online");
      const ref = k => {
        const pool2 = k === 0 && onlineNodes.length ? onlineNodes : clusterNodes;
        const n = pool2[(vi + k) % (pool2.length || 1)];
        return n ? {
          uuid: n.uuid,
          hostname: n.hostname
        } : null;
      };
      const v = {
        uuid: uuid(),
        pool_id: p.uuid,
        pool_name: p.pool_name,
        cluster_id: c.uuid,
        lvol_name: `${pick(VOL_PREFIX)}-${String(vi + 1).padStart(3, "0")}-${hex(3)}`,
        status: vstatus,
        nodes: {
          primary: ref(0),
          secondary: ref(1),
          tertiary: rnd() > .45 ? ref(2) : null
        },
        size_prov: prov,
        size_util: Math.round(prov * (.05 + rnd() * .8)),
        crypto_enabled: rnd() > .55,
        qos: qosOf(false),
        // data reduction is one per-volume switch: compression+dedup together
        compression_dedup_enabled: rnd() > .45,
        nqn: `nqn.2023-02.io.simplyblock:${hex(8)}`,
        base_snapshot: null,
        // a volume can belong to several consistency groups at once
        consistency_groups: [],
        affinity: null,
        pvc: null,
        bucket: null,
        backup_policy: rnd() > .5 ? {
          uuid: pick(clusterPolicies).uuid,
          policy_name: null
        } : null,
        replication: null,
        created_at: ago(int(20, 4000)),
        io_stats: ioStats(io, bw),
        io_history: {
          iops: series(io, .35),
          bytes: series(bw, .35)
        }
      };
      if (v.backup_policy) v.backup_policy.policy_name = (clusterPolicies.find(x => x.uuid === v.backup_policy.uuid) || {}).policy_name;
      v.logical_used = v.compression_dedup_enabled ? Math.round(v.size_util * (1.5 + rnd() * 1.6)) : v.size_util;
      if (c.pod_affinity_enabled && rnd() > .55 && v.nodes.primary) {
        v.affinity = {
          mode: "pod",
          workload: `${v.lvol_name}-0`,
          workload_node: v.nodes.primary.hostname,
          satisfied: rnd() > .2
        };
      } else if (c.node_affinity === "strict" && v.nodes.primary && rnd() > .6) {
        v.affinity = {
          mode: "node",
          pinned_node_id: v.nodes.primary.uuid,
          pinned_node: v.nodes.primary.hostname,
          satisfied: true
        };
      }
      DB.lvols.push(v);

      // ---- snapshot chain -------------------------------------------------
      // Snapshots are chained: each one is a delta against its predecessor.
      const snapCount = int(0, 4);
      let prevSnap = null;
      const volSnaps = [];
      for (let si = 0; si < snapCount; si++) {
        const s = {
          uuid: uuid(),
          cluster_id: c.uuid,
          pool_id: p.uuid,
          pool_name: p.pool_name,
          lvol_id: v.uuid,
          lvol_name: v.lvol_name,
          snapshot_name: `${v.lvol_name}-snap-${String(si + 1).padStart(3, "0")}`,
          seq: si + 1,
          parent_id: prevSnap ? prevSnap.uuid : null,
          created_at: ago((snapCount - si) * int(1, 40)),
          size: Math.round(v.size_util * (.05 + rnd() * .3)),
          status: "online",
          backup_version_id: null
        };
        DB.snapshots.push(s);
        volSnaps.push(s);
        prevSnap = s;
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
            versions.push({
              id: `v${String(seq).padStart(4, "0")}`,
              seq,
              tier: tier.interval,
              type: isFull ? "full" : "delta",
              created_at: ago(int(1, 800) / seq),
              size: Math.round(v.size_util * (isFull ? .82 + rnd() * .15 : .01 + rnd() * .06)),
              source_snapshot_id: src ? src.uuid : null,
              source_snapshot_name: src ? src.snapshot_name : null,
              merged_count: isFull ? int(0, 9) : 0
            });
            seq++;
          }
        });
        versions.sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at)).forEach((x, i) => {
          x.seq = i + 1;
          x.type = i === 0 ? "full" : "delta";
        });
        // mark which snapshots have a backup version taken from them
        versions.forEach(x => {
          const s = volSnaps.find(y => y.uuid === x.source_snapshot_id);
          if (s) s.backup_version_id = x.id;
        });
        DB.backups.push({
          uuid: uuid(),
          cluster_id: c.uuid,
          pool_id: p.uuid,
          pool_name: p.pool_name,
          lvol_id: v.uuid,
          lvol_name: v.lvol_name,
          chain_id: `bk-${hex(6)}`,
          policy_id: pol ? pol.uuid : null,
          policy_name: pol ? pol.policy_name : null,
          bucket: pick(BUCKETS) + c.name + "/" + v.lvol_name + "/",
          status: rnd() > .06 ? "online" : "unavailable",
          created_at: versions[0] ? versions[0].created_at : ago(500),
          last_merge_at: rnd() > .5 ? ago(int(1, 30)) : null,
          versions
        });
      }
    }
  }
  // a few volumes are clones of an existing snapshot
  const snaps = DB.snapshots.filter(s => s.cluster_id === c.uuid);
  DB.lvols.filter(v => v.cluster_id === c.uuid).forEach(v => {
    if (snaps.length && rnd() > .72) {
      const s = pick(snaps);
      if (s.lvol_id !== v.uuid) v.base_snapshot = {
        uuid: s.uuid,
        snapshot_name: s.snapshot_name,
        lvol_name: s.lvol_name
      };
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
  const h = seedHost({
    uuid: null,
    name: "pool"
  }, i, true);
  h.cluster_id = null;
  h.hostname = `pool-host-${String(i + 1).padStart(2, "0")}`;
  h.control_plane = false;
}

// ---- file storage: pick the metadata server and its failover candidates -----
DB.clusters.forEach(c => {
  if (!c.file_storage.enabled) return;
  const cp = DB.hosts.filter(h => h.cluster_id === c.uuid && h.control_plane && h.status === "available");
  const pool2 = cp.length ? cp : DB.hosts.filter(h => h.cluster_id === c.uuid && h.status === "available");
  if (!pool2.length) {
    c.file_storage.enabled = false;
    return;
  }
  c.file_storage.mds_host = {
    uuid: pool2[0].uuid,
    hostname: pool2[0].hostname
  };
  c.file_storage.mds_candidates = pool2.slice(1, 4).map(h => ({
    uuid: h.uuid,
    hostname: h.hostname
  }));
});

// ---- S3 buckets -------------------------------------------------------------
// One bucket is one filesystem is one logical volume: blobs live on cluster
// capacity, metadata in FoundationDB. So a bucket inherits everything a volume
// can do — snapshots, backups, synchronous and asynchronous replication.
const BUCKET_NAMES = ["media-assets", "app-logs", "ml-datasets", "invoices", "backups-tier2", "user-uploads", "telemetry", "artifacts", "warehouse"];
// S3 metadata a client can set and a console must be able to search by: bucket
// tags (up to 50 key/value pairs), the storage class default, the region the
// endpoint answers for, the owner, and lifecycle rules.
const BUCKET_TEAMS = ["platform", "data", "payments", "media", "ml"];
const BUCKET_ENVS = ["prod", "prod", "staging", "dev"];
const bucketTags = name => {
  const t = {
    env: pick(BUCKET_ENVS),
    team: pick(BUCKET_TEAMS),
    app: name.replace(/-\d+$/, "")
  };
  if (rnd() > .5) t["cost-center"] = `cc-${int(1000, 9999)}`;
  if (rnd() > .6) t.retention = pick(["30d", "90d", "1y", "7y"]);
  if (rnd() > .7) t.compliance = pick(["gdpr", "pci", "sox"]);
  return t;
};
const bucketLifecycle = () => rnd() > .5 ? [] : [{
  id: "expire-tmp",
  prefix: "tmp/",
  expire_days: pick([7, 14, 30]),
  status: "Enabled"
}, rnd() > .5 ? {
  id: "cold-archives",
  prefix: "archive/",
  transition_days: pick([30, 90]),
  transition_class: "infrequent-access",
  status: "Enabled"
} : null, rnd() > .6 ? {
  id: "old-versions",
  noncurrent_expire_days: pick([30, 60]),
  status: pick(["Enabled", "Disabled"])
} : null].filter(Boolean);
DB.clusters.forEach(c => {
  if (!c.object_storage.enabled) return;
  const vols = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online" && !v.pvc && !v.bucket);
  vols.slice(0, int(2, 5)).forEach((v, i) => {
    const name = `${pick(BUCKET_NAMES)}-${String(i + 1).padStart(2, "0")}`;
    const b = {
      uuid: uuid(),
      cluster_id: c.uuid,
      name,
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      pool_id: v.pool_id,
      pool_name: v.pool_name,
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
      created_at: ago(int(20, 2600))
    };
    DB.buckets.push(b);
    v.bucket = {
      uuid: b.uuid,
      name: b.name
    };
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
    const cg = {
      uuid: uuid(),
      cluster_id: c.uuid,
      name: `${pick(CG_NAMES)}-${i + 1}`,
      lvol_ids: members.map(v => v.uuid),
      created_at: ago(int(200, 2600)),
      backup_policy: null,
      replication_config: null
    };
    DB.consistency_groups.push(cg);
    members.forEach(v => {
      (v.consistency_groups = v.consistency_groups || []).push({
        uuid: cg.uuid,
        name: cg.name
      });
    });
    for (let k = 0; k < int(0, 4); k++) {
      const at = ago(int(1, 400) / (k + 1));
      DB.cg_snapshots.push({
        uuid: uuid(),
        cluster_id: c.uuid,
        cg_id: cg.uuid,
        cg_name: cg.name,
        snapshot_name: `${cg.name}-cgsnap-${String(k + 1).padStart(3, "0")}`,
        created_at: at,
        status: "online",
        members: members.map(v => ({
          lvol_id: v.uuid,
          lvol_name: v.lvol_name,
          snapshot_id: uuid(),
          size: Math.round(v.size_util * (.04 + rnd() * .2))
        })),
        backup_version_id: rnd() > .55 ? `v${String(int(1, 9)).padStart(4, "0")}` : null,
        backup_bucket: null
      });
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
const ZONE_DEFS = [["eu-central-1a", "eu-central-1", "Frankfurt, Hetzner DC1"], ["eu-central-1b", "eu-central-1", "Frankfurt, Interxion FRA6"], ["us-east-2a", "us-east-2", "Ashburn, Equinix DC2"], ["us-east-2b", "us-east-2", "Ashburn, Equinix DC11"], ["eu-north-1a", "eu-north-1", "Stockholm, Digital Realty"], ["ap-southeast-1a", "ap-southeast-1", "Singapore, edge cage"]];
ZONE_DEFS.forEach(([name, region, location]) => {
  DB.zones.push({
    uuid: uuid(),
    name,
    region,
    location,
    label: "topology.kubernetes.io/zone=" + name,
    region_label: "topology.kubernetes.io/region=" + region,
    k8s_cluster_ids: [],
    rtt_ms: null,
    created_at: ago(int(600, 4000)),
    cluster_ids: []
  });
});
// A Kubernetes cluster spans one or more zones and provides the worker nodes
// that become simplyblock hosts.
const K8S_DEFS = [["k8s-prod-a", ["eu-central-1a", "eu-central-1b"], "1.31.4"], ["k8s-prod-b", ["us-east-2a", "us-east-2b"], "1.30.8"], ["k8s-stage", ["eu-north-1a"], "1.32.1"], ["k8s-edge", ["ap-southeast-1a"], "1.30.6"], ["k8s-analytics", ["eu-central-1a"], "1.31.2"]];
K8S_DEFS.forEach(([name, zoneNames, version], i) => {
  const zoneIds = zoneNames.map(n => (DB.zones.find(x => x.name === n) || {}).uuid).filter(Boolean);
  const k = {
    uuid: uuid(),
    name,
    version,
    api_endpoint: `https://${name}.k8s.internal:6443`,
    environment: pick(["Vanilla", "OpenShift", "Rancher", "K3s", "Talos"]),
    csi_version: pick(["26.2.1", "26.2.0", "26.1.2", "26.1.0"]),
    csi_status: rnd() > .12 ? "online" : pick(["degraded", "unreachable"]),
    operator_namespace: pick(["simplyblock", "simplyblock", "sb-system"]),
    status: rnd() > .1 ? "online" : "degraded",
    zone_ids: zoneIds,
    created_at: ago(int(600, 4000))
  };
  DB.k8s_clusters.push(k);
  zoneIds.forEach(id => {
    const st = DB.zones.find(x => x.uuid === id);
    if (st) st.k8s_cluster_ids.push(k.uuid);
  });
});
const zoneBy = n => DB.zones.find(s => s.name === n);

// stretch the first two clusters across two zones each, the rest sit in one
DB.clusters.forEach((c, i) => {
  const zoneSpans = [["eu-central-1a", "eu-central-1b"], ["us-east-2a", "us-east-2b"], ["eu-north-1a"], ["eu-central-1a"], ["ap-southeast-1a"], ["eu-central-1b"]];
  const names = zoneSpans[i] || ["eu-central-1a"];
  c.zone_ids = names.map(n => zoneBy(n).uuid);
  c.stretched = c.zone_ids.length > 1;
  c.sync_replication_enabled = c.stretched;
  names.forEach(n => zoneBy(n).cluster_ids.push(c.uuid));
  // distribute the cluster's nodes across its zones
  const ns = DB.storage_nodes.filter(n2 => n2.cluster_id === c.uuid);
  ns.forEach((n2, k) => {
    n2.zone_id = c.zone_ids[k % c.zone_ids.length];
  });
  // hosts belong to exactly one zone; a node inherits its host's zone
  const hs = DB.hosts.filter(h => h.cluster_id === c.uuid);
  hs.forEach((h, k) => {
    h.zone_id = c.zone_ids[k % c.zone_ids.length];
    const st = DB.zones.find(x => x.uuid === h.zone_id);
    if (st) {
      h.zone = st.name;
      h.region = st.region;
    }
    // a zone can host several Kubernetes clusters — spread the worker nodes over all of them
    const ids = st ? st.k8s_cluster_ids : [];
    const kc = ids.length ? DB.k8s_clusters.find(x => x.uuid === ids[k % ids.length]) : null;
    h.k8s_cluster_id = kc ? kc.uuid : null;
    h.k8s_cluster = kc ? kc.name : null;
  });
  ns.forEach(n2 => {
    const h = DB.hosts.find(x => x.uuid === n2.host_id);
    if (h) n2.zone_id = h.zone_id;
    if (!c.failure_domain_enabled) {
      n2.failure_domain = null;
      return;
    }
    const st = DB.zones.find(x => x.uuid === n2.zone_id);
    n2.failure_domain = c.failure_domain_scope === "zone" ? st ? st.name : null : c.failure_domain_scope === "cabinet" ? h ? h.cabinet_id : null : h ? h.rack_id : null;
  });
  if (c.failure_domain_enabled) {
    // A real cluster is built domain by domain, in pairs: every domain holds at
    // least two nodes and no domain is more than one node ahead of another.
    // Taking whatever label each host happened to carry produced singleton
    // domains, which the balance rule forbids.
    const labels = [...new Set(ns.map(n2 => n2.failure_domain).filter(Boolean))];
    const domains = labels.length >= 2 ? labels : FD.slice(0, Math.max(2, Math.min(4, Math.floor(ns.length / 2))));
    const usable = Math.min(domains.length, Math.floor(ns.length / 2));
    const use = domains.slice(0, Math.max(2, usable));
    // fill pairwise: two nodes per domain, then round-robin the remainder
    const order = [];
    use.forEach(f => {
      order.push(f, f);
    });
    for (let i = order.length; i < ns.length; i++) order.push(use[i % use.length]);
    ns.forEach((n2, i) => {
      n2.failure_domain = order[i] || use[0];
    });
  }
  // a degraded fixture cluster loses nodes inside ONE failure domain
  if (c.failure_domain_enabled && c.status === "degraded") {
    const down = ns.filter(n => n.status !== "online");
    const fd = down.length ? down[0].failure_domain : null;
    down.slice(1).forEach(n => {
      if (n.failure_domain !== fd) n.status = "online";
    });
  }
});
// hosts in the unassigned pool are racked in a zone too
DB.hosts.filter(h => !h.cluster_id).forEach((h, k) => {
  const st = DB.zones[k % DB.zones.length];
  h.zone_id = st.uuid;
  h.zone = st.name;
  h.region = st.region;
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
const SCP_DEFS = [{
  encryption: true,
  compression: true,
  qosRwIops: 100000,
  qosRwMbytes: 0,
  filesystem: "ext4",
  fabric: "tcp",
  maxNamespacePerSubsys: 1,
  tune2fsReservedBlocks: 0
}, {
  encryption: false,
  compression: true,
  qosRwIops: 50000,
  qosRwMbytes: 1000,
  filesystem: "xfs",
  fabric: "tcp",
  maxNamespacePerSubsys: 8,
  tune2fsReservedBlocks: 0
}, {
  encryption: false,
  compression: false,
  qosRwIops: 0,
  qosRwMbytes: 0,
  filesystem: null,
  fabric: "tcp",
  maxNamespacePerSubsys: 1,
  tune2fsReservedBlocks: 0
}, {
  encryption: true,
  compression: false,
  qosRwIops: 0,
  qosRwMbytes: 2000,
  qosRMbytes: 1500,
  filesystem: "ext4",
  fabric: "rdma",
  maxNamespacePerSubsys: 1,
  tune2fsReservedBlocks: 5
}];
// CRD camelCase field -> CSI StorageClass parameter name
const SCP_MAP = {
  qosRwIops: "qos_rw_iops",
  qosRwMbytes: "qos_rw_mbytes",
  qosRMbytes: "qos_r_mbytes",
  qosWMbytes: "qos_w_mbytes",
  compression: "compression",
  encryption: "encryption",
  fabric: "fabric",
  maxNamespacePerSubsys: "max_namespace_per_subsys",
  tune2fsReservedBlocks: "tune2fs_reserved_blocks",
  filesystem: "csi.storage.k8s.io/fstype"
};
DB.k8s_clusters.forEach((kc, ki) => {
  // storage classes point at a pool in a storage cluster reachable from this k8s cluster
  const storClusters = DB.clusters.filter(c => (c.zone_ids || []).some(id => kc.zone_ids.includes(id)));
  const target = storClusters.length ? storClusters[0] : DB.clusters[0];
  const pools = DB.pools.filter(p => target && p.cluster_id === target.uuid);
  // The operator generates a StorageClass per active StoragePool. A pool can back
  // several classes (different QoS, filesystem or encryption defaults over the
  // same capacity); a class always belongs to exactly one pool.
  const gen = [];
  pools.slice(0, int(2, 4)).forEach((pool, si) => {
    gen.push([pool, si]);
    if (rnd() > .6) gen.push([pool, si + 1]);
  });
  // one simplyblock operator per Kubernetes cluster, so one namespace for every
  // StorageClass it generates
  const ns = kc.operator_namespace;
  gen.forEach(([pool, si]) => {
    const spec = SCP_DEFS[si % SCP_DEFS.length];
    const sibling = DB.storage_classes.some(x => x.pool_id === pool.uuid && x.k8s_cluster_id === kc.uuid);
    // cluster_id and pool_name always come from the pool and cannot be overridden
    const params = {
      cluster_id: target.uuid,
      pool_name: pool.pool_name
    };
    Object.entries(spec).forEach(([k2, v2]) => {
      if (v2 === null || v2 === undefined) return;
      params[SCP_MAP[k2]] = typeof v2 === "boolean" ? v2 ? "True" : "False" : String(v2);
    });
    // Topology-aware provisioning: map the pod's zone / region label onto the
    // storage cluster that serves it, so a PVC lands on local storage.
    // Per zone, prefer the most zone-local cluster (narrowest zone span) so the
    // map genuinely routes a pod to storage beside it. The region falls back to
    // the widest cluster in that region, which is the stretched one.
    const zoneMap = {},
      regionMap = {};
    const byRegion = {};
    kc.zone_ids.forEach(zid => {
      const z = DB.zones.find(x => x.uuid === zid);
      if (!z) return;
      const here = DB.clusters.filter(x => (x.zone_ids || []).includes(zid));
      const local = here.slice().sort((p, q) => p.zone_ids.length - q.zone_ids.length || (p.uuid === target.uuid ? -1 : 1))[0] || target;
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
    DB.storage_classes.push({
      uuid: scId,
      k8s_cluster_id: kc.uuid,
      // A StorageClass is scoped to its Kubernetes cluster, so only a sibling in
      // the SAME cluster over the SAME pool needs disambiguating.
      name: `simplyblock-${ns}-${target.name}-${pool.pool_name}` + (sibling ? `-${spec.filesystem || "block"}` : ""),
      variant: sibling ? spec.filesystem || "block" : "default",
      operator_namespace: ns,
      provisioner: "csi.simplyblock.io",
      cluster_id: target.uuid,
      pool_id: pool.uuid,
      pool_name: pool.pool_name,
      storage_pool_ref: `${ns}/${pool.pool_name}`,
      spec_parameters: spec,
      parameters: params,
      zone_cluster_map: zoneMap,
      region_cluster_map: regionMap,
      dhchap: dhchap,
      allowed_topology: dhchap ? "simplyblock.io/dhchap-pool" : null,
      reclaim_policy: pick(["Delete", "Delete", "Retain"]),
      volume_binding_mode: pick(["WaitForFirstConsumer", "WaitForFirstConsumer", "Immediate"]),
      allow_volume_expansion: true,
      is_default: si === 0 && ki === 0,
      created_at: ago(int(400, 3000))
    });
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
  scs.forEach(x => {
    if (x.pool_id) (byPool[x.pool_id] = byPool[x.pool_id] || []).push(x);
  });
  // a bucket's volume is its filesystem and cannot also back a claim
  const free = DB.lvols.filter(v => clusterIds.includes(v.cluster_id) && !v.pvc && !v.bucket && byPool[v.pool_id]);
  free.forEach((v, i) => {
    if (rnd() < .35) return;
    const opts = byPool[v.pool_id];
    const sc = opts[i % opts.length];
    const ns = pick(NS_NAMES);
    const wl = pick(PVC_WORKLOADS);
    const name = `${wl}-data-${String(i % 9).padStart(2, "0")}`;
    const bound = v.status === "online" ? rnd() > .05 ? "Bound" : "Lost" : "Pending";
    const srcCluster = DB.clusters.find(x => x.uuid === v.cluster_id);
    const fileCap = srcCluster && srcCluster.file_storage.enabled;
    const pvc = {
      uuid: uuid(),
      k8s_cluster_id: kc.uuid,
      namespace: ns,
      pvc_name: name,
      storage_class_id: sc.uuid,
      storage_class: sc.name,
      lvol_id: bound === "Pending" ? null : v.uuid,
      requested_bytes: v.size_prov,
      actual_bytes: v.size_prov,
      status: bound,
      // RWX is served by pNFS and is XFS-only, so it is offered only where the
      // storage cluster has file storage enabled
      access_mode: fileCap && rnd() > .6 ? "ReadWriteMany" : pick(["ReadWriteOnce", "ReadWriteOnce", "ReadWriteOncePod"]),
      volume_mode: pick(["Filesystem", "Filesystem", "Block"]),
      workload: `${wl}-${int(0, 2)}`,
      workload_kind: pick(["StatefulSet", "StatefulSet", "Deployment"]),
      annotations: Object.assign({
        "volume.kubernetes.io/storage-provisioner": "csi.simplyblock.io"
      }, rnd() > .5 ? {
        "backup.simplyblock.io/policy": pick(["5m-tiered", "hourly-24h", "daily-30d"])
      } : {}, rnd() > .6 ? {
        "simplyblock.io/tier": pick(["gold", "silver", "bronze"])
      } : {}, rnd() > .75 ? {
        "app.kubernetes.io/part-of": wl
      } : {}),
      labels: {
        "app.kubernetes.io/name": wl,
        "app.kubernetes.io/instance": name
      },
      created_at: ago(int(10, 3000))
    };
    if (pvc.access_mode === "ReadWriteMany") {
      // pNFS on the Linux kernel NFS server supports XFS only
      pvc.volume_mode = "Filesystem";
      pvc.filesystem = "xfs";
      pvc.annotations["simplyblock.io/access"] = "pnfs";
    } else {
      pvc.filesystem = pvc.volume_mode === "Block" ? null : pick(["ext4", "xfs"]);
    }
    DB.pvcs.push(pvc);
    if (pvc.lvol_id) v.pvc = {
      uuid: pvc.uuid,
      name: pvc.pvc_name,
      namespace: ns,
      storage_class: sc.name,
      k8s_cluster_id: kc.uuid,
      k8s_cluster: kc.name,
      workload: pvc.workload
    };
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
    uuid: uuid(),
    k8s_cluster_id: kc.uuid,
    name: kc.name,
    region: (DB.zones.find(x => x.uuid === kc.zone_ids[0]) || {}).region || "unknown",
    s3_profile_name: `s3-${kc.name}`,
    s3_endpoint: `https://s3.${(DB.zones.find(x => x.uuid === kc.zone_ids[0]) || {}).region || "eu-central-1"}.amazonaws.com`,
    s3_bucket: `ramen-metadata-${kc.name}`,
    fencing_state: rnd() > .88 ? pick(["Fenced", "ManuallyFenced"]) : "Unfenced",
    status: kc.status === "online" ? rnd() > .1 ? "online" : "degraded" : "degraded",
    ramen_version: `4.${int(14, 18)}`,
    last_heartbeat_at: ago(rnd()),
    created_at: ago(int(500, 3000))
  });
});
const APP_DEFS = [["pacman", "pacman", "ApplicationSet"], ["busybox-sample", "busybox", "Subscription"], ["postgres-ha", "db-prod", "ApplicationSet"], ["kafka-stream", "data", "Subscription"], ["vm-win2022", "virt", "VirtualMachine"], ["vm-rhel9-erp", "virt", "VirtualMachine"], ["grafana-stack", "monitoring", "ApplicationSet"]];
// The only valid progressions for each Ramen phase, in the order they occur.
// The seeder and the reconcile loop both read this, so they cannot drift apart.
// Node lifecycle operations. The phase list IS the progress tracker the UI
// draws, and the subtask list is what shows up under the cluster's tasks —
// one entry per phase, so the two can never disagree.
const NODE_OPS = {
  removal: {
    label: "Removal",
    status: "in_removal",
    every: 5000,
    done: "node removed",
    phases: ["data migration and rebalancing", "volume migration", "removed"],
    subtasks: ["migrate_data", "migrate_volumes", "deregister_node"],
    hint: "Data is rebalanced onto the remaining nodes, then the volumes whose primary sits here are moved off. The node and its device records are deleted at the end."
  },
  expansion: {
    label: "Expansion",
    status: "in_creation",
    every: 5000,
    done: "node added and data rebalanced",
    phases: ["adding node", "rebalancing data", "complete"],
    subtasks: ["add_node", "rebalance_data", "activate_node"],
    hint: "The storage node is deployed and joins the cluster, then existing data is rebalanced onto it."
  },
  migration: {
    label: "Migration",
    status: "in_migration",
    every: 5000,
    done: "node migrated to the new host",
    phases: ["restarting node", "rebalancing", "removing node", "migrated"],
    subtasks: ["restart_node", "rebalance_data", "remove_source_node", "finish_migration"],
    hint: "The node is restarted on the prepared target host, data is rebalanced, and the record on the old host is removed."
  }
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
  RestoringGeneration: ["PinningGeneration", "UpdatingPlacement", "MaterialisingVolumes", "RestoringKubeObjects", "WaitingForResourceRestore", "Completed"]
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
  const targetZone = cross ? pick(DB.zones.filter(x => !c.zone_ids.includes(x.uuid)).concat(DB.zones)) : ownZones.length > 1 ? ownZones[ownZones.length - 1] : ownZones[0];
  const targetCluster = cross ? pick(DB.clusters.filter(x => x.uuid !== c.uuid && x.dr_target_eligible)) : null;
  const freezeThreshold = 256e6;
  // leave iterations to spare so convergence is still reachable
  const iterations = cross ? int(2, 7) : 0;
  const firstSnap = vols.reduce((a, v) => a + v.size_util, 0);
  const lastSnap = cross ? Math.max(freezeThreshold * 1.4, Math.round(firstSnap / Math.pow(2.1, iterations))) : 0;
  // a cross-cluster job that is not yet converged must not claim to be ready
  const state = cross ? lastSnap > freezeThreshold ? pick(["replicating", "converging", "converging", "paused"]) : "cutover_pending" : pick(MIG_STATE_INTRA);
  // taint the destination hosts so an intra-cluster job has somewhere to go
  if (!cross && targetZone) {
    DB.hosts.filter(h => h.cluster_id === c.uuid && h.zone_id === targetZone.uuid && (h.storage_node_ids || []).length).forEach(h => {
      h.migration_taint = "simplyblock.io/migration-target=true";
    });
  }
  DB.migrations.push({
    uuid: uuid(),
    cluster_id: c.uuid,
    name: cross ? `move-${c.name.split("-").slice(0, 2).join("-")}-to-${(targetCluster || {
      name: "dr"
    }).name.split("-").slice(-2).join("-")}` : `follow-workload-${c.name.split("-").slice(0, 2).join("-")}-${ci + 1}`,
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
    iterations,
    iteration_limit: iterationLimit,
    first_snapshot_bytes: cross ? firstSnap : 0,
    last_snapshot_bytes: lastSnap,
    freeze_threshold_bytes: cross ? freezeThreshold : 0,
    estimated_freeze_ms: cross ? Math.max(120, Math.round(lastSnap / 1.2e6)) : 0,
    throughput_bytes_ps: state === "paused" ? 0 : int(60, 900) * 1e6,
    started_at: ago(int(2, 300)),
    completed_at: state === "completed" ? ago(int(1, 40)) : null,
    frozen_at: null,
    error: null
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
  const p = {
    uuid: uuid(),
    source_cluster_id: src.uuid,
    target_cluster_id: tgt.uuid,
    state,
    link: {
      endpoint: `nvmf://${tgt.name}.simplyblock.remote:4420`,
      nqn: `nqn.2023-02.io.simplyblock:repl:${hex(8)}`,
      rtt_ms: +(2 + rnd() * 38).toFixed(1),
      bandwidth_mbit: pick([1000, 2500, 10000]),
      throughput_bytes_ps: state === "unreachable" ? 0 : int(20, 900) * 1e6
    },
    last_handshake_at: ago(state === "unreachable" ? int(3, 40) : rnd()),
    created_at: ago(int(200, 3000))
  };
  DB.cluster_pairs.push(p);
  return p;
}
const dcK8s = DB.clusters.filter(c => c.capabilities.async_replication && c.location_type !== "edge");
if (dcK8s.length > 1) {
  pairUp(dcK8s[0], dcK8s[1]); // a → b
  pairUp(dcK8s[1], dcK8s[0]); // b → a  (bidirectional)
}
DB.clusters.filter(c => c.capabilities.async_replication).forEach((c, i) => {
  const t = eligible.filter(e => e.uuid !== c.uuid);
  if (t.length) pairUp(c, t[i % t.length]);
});

// ---- DR replication policies ----------------------------------------------
// Async policies hang off a cluster pair. Sync policies live inside one
// stretched cluster and name the zones they span.
DB.dr_policies = [];
const SCHEDULES = [[{
  interval: "5m",
  keep: 10
}, {
  interval: "15m",
  keep: 4
}, {
  interval: "1h",
  keep: 11
}, {
  interval: "1d",
  keep: 6
}], [{
  interval: "15m",
  keep: 8
}, {
  interval: "1h",
  keep: 12
}, {
  interval: "1d",
  keep: 7
}], [{
  interval: "1h",
  keep: 24
}, {
  interval: "1d",
  keep: 14
}], [{
  interval: "5m",
  keep: 12
}]];
const POL_STATE = ["healthy", "healthy", "healthy", "healthy", "degraded", "unhealthy"];
DB.cluster_pairs.filter(p => p.state !== "unreachable").forEach((p, pi) => {
  const src = DB.clusters.find(c => c.uuid === p.source_cluster_id);
  const tgt = DB.clusters.find(c => c.uuid === p.target_cluster_id);
  for (let i = 0; i < int(1, 2); i++) {
    const state = p.state === "degraded" ? pick(["degraded", "unhealthy"]) : pick(POL_STATE);
    const freq = pick([5, 5, 15, 30, 60]);
    const pol = {
      uuid: uuid(),
      name: `dr-${src.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(-2).join("-")}${i ? "-tier2" : ""}`,
      mode: "asynchronous",
      pair_id: p.uuid,
      source_cluster_id: src.uuid,
      target_cluster_id: tgt.uuid,
      zone_ids: null,
      cg_id: null,
      cg_name: null,
      frequency_minutes: freq,
      retention: pick(SCHEDULES),
      failback: {
        mode: pick(["manual", "manual", "automatic"]),
        frequency_minutes: freq,
        reverse_on_failover: true,
        resync_full: rnd() > .7
      },
      state,
      last_replication_at: ago(state === "unhealthy" ? int(3, 26) : freq / 60 * (rnd() * .9)),
      backlog_bytes: state === "healthy" ? int(50, 2600) * 1e6 : int(4000, 90000) * 1e6,
      generations_kept: 0,
      created_at: ago(int(100, 2600)),
      last_failover_at: null,
      last_test_at: rnd() > .6 ? ago(int(20, 700)) : null,
      lvol_ids: []
    };
    pol.generations_kept = pol.retention.reduce((a, r) => a + r.keep, 0);
    DB.dr_policies.push(pol);
  }
});

// synchronous policies on stretched clusters
DB.clusters.filter(c => c.stretched).forEach(c => {
  const pol = {
    uuid: uuid(),
    name: `sync-${c.name.split("-").slice(0, 2).join("-")}-stretch`,
    mode: "synchronous",
    pair_id: null,
    source_cluster_id: c.uuid,
    target_cluster_id: null,
    zone_ids: c.zone_ids.slice(),
    cg_id: null,
    cg_name: null,
    frequency_minutes: 0,
    retention: [],
    failback: {
      mode: "automatic",
      frequency_minutes: 0,
      reverse_on_failover: false,
      resync_full: false
    },
    state: pick(["healthy", "healthy", "healthy", "degraded"]),
    last_replication_at: ago(0),
    backlog_bytes: 0,
    generations_kept: 0,
    created_at: ago(int(200, 2000)),
    last_failover_at: null,
    last_test_at: null,
    lvol_ids: []
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
      policy_id: pol.uuid,
      policy_name: pol.name,
      mode: pol.mode,
      status: healthy ? "healthy" : "unhealthy",
      last_replication_at: pol.mode === "synchronous" ? ago(0) : ago(healthy ? pol.frequency_minutes / 60 * (rnd() * .9) : int(2, 20)),
      backlog_bytes: pol.mode === "synchronous" ? 0 : Math.round(v.size_util * (healthy ? .004 + rnd() * .02 : .08 + rnd() * .3)),
      consistency_group: pol.cg_name || null,
      frequency_minutes: pol.frequency_minutes,
      generations: pol.generations_kept
    };
  });
});
const recipeFor = (name, kind, ns) => {
  const app = name.split("-")[0];
  const sel = {
    matchLabels: {
      "app.kubernetes.io/name": app
    }
  };
  const vm = kind === "VirtualMachine";
  const groups = vm ? [{
    name: "config",
    type: "resource",
    includedResourceTypes: ["Secret", "ConfigMap"],
    labelSelector: sel
  }, {
    name: "vms",
    type: "resource",
    includedResourceTypes: ["VirtualMachine", "DataVolume"],
    labelSelector: sel
  }, {
    name: "expose",
    type: "resource",
    includedResourceTypes: ["Service"],
    labelSelector: sel
  }] : [{
    name: "config",
    type: "resource",
    includedResourceTypes: ["Secret", "ConfigMap", "ServiceAccount"],
    labelSelector: sel
  }, {
    name: "data",
    type: "resource",
    includedResourceTypes: ["StatefulSet"],
    labelSelector: sel
  }, {
    name: "app",
    type: "resource",
    includedResourceTypes: ["Deployment"],
    labelSelector: sel
  }, {
    name: "expose",
    type: "resource",
    includedResourceTypes: ["Service", "Ingress", "Route"],
    labelSelector: sel
  }];
  const hooks = vm ? [{
    name: "vm-ready",
    type: "check",
    selectResource: "pod",
    labelSelector: {
      matchLabels: {
        "kubevirt.io/domain": app
      }
    },
    ops: [],
    chks: [{
      name: "running",
      condition: "{$.status.phase} == 'Running'",
      timeout: 600,
      onError: "fail"
    }]
  }] : [{
    name: "db",
    type: "exec",
    selectResource: "pod",
    labelSelector: {
      matchLabels: {
        "app.kubernetes.io/component": "db"
      }
    },
    ops: [{
      name: "checkpoint",
      container: "postgres",
      command: "psql -c CHECKPOINT",
      timeout: 60,
      onError: "fail"
    }, {
      name: "isready",
      container: "postgres",
      command: "pg_isready -q",
      timeout: 180,
      onError: "fail"
    }],
    chks: []
  }, {
    name: "data-ready",
    type: "check",
    selectResource: "statefulset",
    labelSelector: sel,
    ops: [],
    chks: [{
      name: "replicas",
      condition: "{$.status.readyReplicas} == {$.spec.replicas}",
      timeout: 900,
      onError: "fail"
    }]
  }];
  return {
    name: `${app}-recipe`,
    namespace: ns,
    appType: app,
    groups,
    hooks,
    captureWorkflow: {
      failOn: "any-error",
      sequence: vm ? [{
        group: "config"
      }, {
        group: "vms"
      }, {
        group: "expose"
      }] : [{
        hook: "db/checkpoint"
      }, {
        group: "config"
      }, {
        group: "data"
      }, {
        group: "app"
      }, {
        group: "expose"
      }]
    },
    recoverWorkflow: {
      failOn: pick(["any-error", "any-error", "essential-error"]),
      sequence: vm ? [{
        group: "config"
      }, {
        group: "vms"
      }, {
        hook: "vm-ready/running"
      }, {
        group: "expose"
      }] : [{
        group: "config"
      }, {
        group: "data"
      }, {
        hook: "data-ready/replicas"
      }, {
        hook: "db/isready"
      }, {
        group: "app"
      }, {
        group: "expose"
      }]
    }
  };
};
// recovery points a backup-protected app can fail over to: one per retained
// backup version of its group chain, newest first
function recoveryPointsFor(pol) {
  const pts = [];
  let gen = 1;
  (pol.schedule || []).slice().reverse().forEach(t => {
    const n = parseInt(t.interval, 10),
      unit = t.interval.replace(/\d/g, "");
    const mins = n * (unit === "m" ? 1 : unit === "h" ? 60 : unit === "d" ? 1440 : 10080);
    for (let i = t.versions - 1; i >= 0; i--) pts.push({
      generation: gen++,
      at: ago(mins * (i + 1) / 60),
      tier: t.interval,
      kind: "delta",
      size: int(200, 9000) * 1e6
    });
  });
  const kept = pts.reverse().slice(0, 40).map((p, i) => Object.assign(p, {
    generation: pts.length - i
  }));
  // the earliest retained version of a chain IS the full; merging raises it
  if (kept.length) {
    const base = kept[kept.length - 1];
    base.kind = "full";
    base.size = int(9000, 40000) * 1e6;
  }
  return kept;
}
const DR_PHASE_CYCLE = ["FailedOver", "Deployed", "Deployed", "WaitForUser", "Deployed", "Relocating", "Deployed"];
// Hand a few consistency groups their own protection, so the group-as-unit case
// is visible: a group-consistent backup policy on one, a replication policy on
// another, and one group carrying both.
DB.consistency_groups.forEach((g, i) => {
  if (i % 3 === 0) {
    const pol = DB.backup_policies.find(p => p.cluster_id === g.cluster_id && p.consistency_group) || DB.backup_policies.find(p => p.cluster_id === g.cluster_id);
    if (pol) {
      pol.consistency_group = true;
      g.backup_policy = {
        uuid: pol.uuid,
        policy_name: pol.policy_name
      };
    }
  }
  if (i % 3 !== 1) {
    // the group's own replication cadence: how often the group snapshot is taken
    // and shipped, and how many older generations the target keeps
    g.replication_config = {
      frequency_minutes: pick([5, 15, 30, 60]),
      retention: pick(SCHEDULES)
    };
  }
});
// An asynchronous DR policy is a pair plus a consistency group. It has no
// schedule of its own — the group's replication config is the schedule.
DB.dr_policies.filter(p => p.mode === "asynchronous").forEach(p => {
  const g = DB.consistency_groups.find(x => x.cluster_id === p.source_cluster_id && x.replication_config) || DB.consistency_groups.find(x => x.cluster_id === p.source_cluster_id);
  if (!g) return;
  if (!g.replication_config) g.replication_config = {
    frequency_minutes: p.frequency_minutes,
    retention: p.retention
  };
  p.cg_id = g.uuid;
  p.cg_name = g.name;
  p.lvol_ids = g.lvol_ids.slice();
});

// The DR clusters a policy protects: the Kubernetes clusters whose storage
// classes consume its source (and, async, its target) storage cluster.
// Sync: both ends consume the same stretched cluster — two different DR
// clusters must consume it for the policy to carry applications.
const drEndsOf = pol => {
  const consumers = cid => [...new Set((DB.storage_classes || []).filter(sc => sc.cluster_id === cid).map(sc => sc.k8s_cluster_id))].map(kid => DB.dr_clusters.find(d => d.k8s_cluster_id === kid)).filter(Boolean);
  if (pol.mode === "synchronous") {
    const both = consumers(pol.source_cluster_id);
    return both.length >= 2 ? [both[0], both[1]] : null;
  }
  const a = consumers(pol.source_cluster_id)[0],
    b = consumers(pol.target_cluster_id).find(d => !a || d.uuid !== a.uuid);
  return a && b ? [a, b] : null;
};
let appCursor = 0,
  appSeq = 0;
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
    const isStale = appSeq === 3; // exactly one app is deliberately behind
    appSeq++;
    const failedOver = phase === "FailedOver";
    const ivMins = pol.mode === "synchronous" ? 0 : Math.max(1, pol.frequency_minutes || 5);
    const lagMins = pol.mode === "synchronous" ? 0 : isStale ? ivMins * (3 + rnd() * 3) : ivMins * (.2 + rnd() * .6);
    const preferred = failedOver ? b : a;
    const failoverTo = failedOver ? a : b;
    const claims = DB.pvcs.filter(p => p.k8s_cluster_id === preferred.k8s_cluster_id && p.namespace === ns);
    const pvcs = (claims.length ? claims : DB.pvcs.filter(p => p.k8s_cluster_id === preferred.k8s_cluster_id)).slice(0, int(1, 4));
    // the PVCs' volumes join the policy — that is what replicates the blocks
    pvcs.forEach(p => {
      const v = DB.lvols.find(x => x.uuid === p.lvol_id);
      if (!v || v.replication) return;
      pol.lvol_ids.push(v.uuid);
      v.replication = {
        policy_id: pol.uuid,
        policy_name: pol.name,
        mode: pol.mode,
        status: isStale ? "unhealthy" : "healthy",
        last_replication_at: ago(lagMins / 60),
        backlog_bytes: pol.mode === "synchronous" ? 0 : Math.round(v.size_util * (isStale ? .1 : .01)),
        consistency_group: pol.cg_name || null,
        frequency_minutes: pol.frequency_minutes,
        generations: pol.generations_kept
      };
    });
    DB.protected_apps.push({
      uuid: uuid(),
      app_name: name,
      namespace: ns,
      app_kind: kind,
      policy_id: pol.uuid,
      policy_name: pol.name,
      preferred_cluster_id: preferred.uuid,
      failover_cluster_id: failoverTo.uuid,
      pvc_selector: {
        "app.kubernetes.io/name": name.split("-")[0]
      },
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
      backup_policy_id: null,
      backup_policy_name: null,
      recovery_points: [],
      restore_point: null,
      // An application's PVC set can be pinned to a consistency group instead of
      // a label selector: the group then defines the crash-consistent boundary,
      // and every member is failed over together.
      cg_id: null,
      cg_name: null,
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
  if (!pol) {
    a.protection_mode = "replication";
    return;
  }
  a.backup_policy_id = pol.uuid;
  a.backup_policy_name = pol.policy_name;
  a.recovery_points = recoveryPointsFor(pol);
  a.pvc_ids.forEach(id => {
    const p = DB.pvcs.find(x => x.uuid === id);
    const lv = p && DB.lvols.find(x => x.uuid === p.lvol_id);
    if (lv) {
      lv.backup_policy = {
        uuid: pol.uuid,
        policy_name: pol.policy_name
      };
      lv.replication = null;
    }
  });
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
  a.cg_id = cg.uuid;
  a.cg_name = cg.name;
  const pvcs = cg.lvol_ids.map(vid => DB.pvcs.find(p => p.lvol_id === vid)).filter(Boolean);
  if (pvcs.length) a.pvc_ids = pvcs.map(p => p.uuid);
  if (a.protection_mode === "backup" && cg.backup_policy) {
    a.backup_policy_id = cg.backup_policy.uuid;
    a.backup_policy_name = cg.backup_policy.policy_name;
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
    enabled: true,
    endpoint: `https://s3.${c.name}.simplyblock.internal`,
    region: pick(["eu-central-1", "us-east-2"]),
    addressing: "virtual-hosted",
    metadata_backend: "foundationdb",
    versioning_default: true,
    max_buckets: 500
  };
  let mine = DB.buckets.filter(b => b.cluster_id === c.uuid);
  if (!mine.length) {
    const free = DB.lvols.filter(v => v.cluster_id === c.uuid && v.status === "online" && !v.pvc && !v.bucket).slice(0, 3);
    free.forEach((v, i) => {
      const name = `${pick(BUCKET_NAMES)}-${String(i + 1).padStart(2, "0")}`;
      const b = {
        uuid: uuid(),
        cluster_id: c.uuid,
        name,
        lvol_id: v.uuid,
        lvol_name: v.lvol_name,
        pool_id: v.pool_id,
        pool_name: v.pool_name,
        status: "online",
        versioning: true,
        object_lock: false,
        quota_bytes: 0,
        objects: int(1200, 900000),
        size_bytes: v.size_util,
        region: c.object_storage.region,
        storage_class: "standard",
        owner: pick(["platform-team", "data-eng"]),
        tags: bucketTags(name),
        lifecycle_rules: bucketLifecycle(),
        cors_enabled: false,
        access: {
          service_account: `sb-s3-${name}`,
          namespace: "prod",
          secret_name: `${name}-s3-credentials`,
          access_key_id: `SB${hex(9).toUpperCase()}`,
          policy: "read-write",
          public: false
        },
        created_at: ago(int(20, 900))
      };
      DB.buckets.push(b);
      v.bucket = {
        uuid: b.uuid,
        name: b.name
      };
    });
    mine = DB.buckets.filter(b => b.cluster_id === c.uuid);
  }
  const b = mine.find(x => !DB.lvols.find(v => v.uuid === x.lvol_id).replication);
  if (!b) return;
  const v = DB.lvols.find(x => x.uuid === b.lvol_id);
  pol.lvol_ids.push(v.uuid);
  v.replication = {
    policy_id: pol.uuid,
    policy_name: pol.name,
    mode: pol.mode,
    status: "healthy",
    last_replication_at: ago(pol.frequency_minutes / 60 * .4),
    backlog_bytes: Math.round(v.size_util * .01),
    consistency_group: null,
    generations: pol.generations_kept
  };
})();
const NIC_NAMES = ["eno1", "eno2", "ens1f0", "ens1f1", "enp94s0f0", "enp94s0f1", "bond0"];
function mkNics(mgmtIp) {
  const n = int(2, 4);
  return Array.from({
    length: n
  }, (_, i) => ({
    name: NIC_NAMES[i % NIC_NAMES.length],
    mac: Array.from({
      length: 6
    }, () => hex(2)).join(":"),
    speed_gbps: pick([10, 25, 25, 100]),
    address: i === 0 ? mgmtIp : `10.${int(10, 60)}.${int(0, 40)}.${int(2, 250)}`,
    numa_socket: i < 2 ? 0 : 1,
    state: rnd() > .1 ? "up" : "down"
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
      h.devices.push({
        id: uuid(),
        kind: "nvme",
        numa_socket: s,
        pcie_address: `0000:${hex(2)}:0${i}.0`,
        device_name: `/dev/nvme${s * per + i}n1`,
        serial_number: `S${hex(3).toUpperCase()}NY0${int(100000, 999999)}`,
        model_number: `${model} ${(size / TB).toFixed(2)}TB`,
        size,
        assigned_node_id: null
      });
    }
  }
  for (let i = 0; i < int(0, 2); i++) {
    h.devices.push({
      id: uuid(),
      kind: "block",
      numa_socket: 0,
      pcie_address: null,
      device_name: `/dev/sd${"bcd"[i]}`,
      serial_number: null,
      model_number: "VIRTUAL-BLOCK",
      size: pick([1 * TB, 2 * TB]),
      assigned_node_id: null
    });
  }
  h.nics = mkNics(h.mgmt_ip);
  h.status = "inspected";
  h.inspection = {
    state: "complete",
    finished_at: ago(0)
  };
}

// ---- Kubernetes worker nodes that are not prepared yet ---------------------
DB.clusters.forEach(c => {
  const prefix = c.name.split("-").slice(0, 2).join("-");
  const start = DB.hosts.filter(h => h.cluster_id === c.uuid).length;
  for (let i = 0; i < int(2, 5); i++) {
    DB.hosts.push({
      uuid: uuid(),
      cluster_id: c.uuid,
      hostname: `${prefix}-worker-${String(start + i + 1).padStart(2, "0")}`,
      mgmt_ip: `172.20.${int(0, 40)}.${int(2, 250)}`,
      status: "discovered",
      source: "kubernetes",
      kubelet_version: `v1.3${int(0, 3)}.${int(1, 9)}`,
      roles: ["worker"],
      k8s_labels: {
        "kubernetes.io/os": "linux",
        "node.kubernetes.io/instance-type": pick(["m6id.8xlarge", "i4i.4xlarge", "bare-metal"])
      },
      numa_sockets: null,
      control_plane: false,
      zone_id: null,
      zone: null,
      region: null,
      k8s_cluster: null,
      rack_id: rnd() > .35 ? `r${String(int(1, 18)).padStart(2, "0")}` : null,
      cabinet_id: rnd() > .5 ? `c${String(int(1, 6)).padStart(2, "0")}` : null,
      host_class: null,
      vcpu_count: pick([32, 48, 64, 96]),
      memory_total: pick([128, 256, 384]) * GB,
      hugepages_reserved: 0,
      hugepages_allocated: 0,
      prepared_at: null,
      labels: {},
      devices: [],
      nics: [],
      storage_node_ids: [],
      inspection: null
    });
  }
});
DB.hosts.filter(h => h.status !== "discovered" && !h.nics).forEach(h => {
  h.nics = mkNics(h.mgmt_ip);
});

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
const zeroIo = {
  read_io_ps: 0,
  write_io_ps: 0,
  read_bytes_ps: 0,
  write_bytes_ps: 0
};
const addIo = (a, b) => ({
  read_io_ps: a.read_io_ps + b.read_io_ps,
  write_io_ps: a.write_io_ps + b.write_io_ps,
  read_bytes_ps: a.read_bytes_ps + b.read_bytes_ps,
  write_bytes_ps: a.write_bytes_ps + b.write_bytes_ps
});
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
    if (h.status !== "discovered") h.host_class = h.devices.some(d => d.kind === "nvme") ? "nvme" : "non-nvme";
    h.devices_assigned = h.devices.filter(d => d.assigned_node_id).length;
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
    c.fault_budget = c.failure_domain_enabled && allFds.length >= 2 ? {
      kind: "failure_domain",
      tolerated: 1,
      lost: downFds.length
    } : {
      kind: "nodes",
      tolerated: c.distr_npcs,
      lost: down.length
    };
    if (["online", "degraded", "suspended"].includes(c.status) && ns.length) {
      c.status = down.length === 0 ? "online" : down.length === ns.length ? "suspended" : c.fault_budget.lost <= c.fault_budget.tolerated ? "degraded" : "suspended";
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
    c.k8s_cluster_ids = [...new Set((DB.storage_classes || []).filter(x => x.cluster_id === c.uuid).map(x => x.k8s_cluster_id))];
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
      c.file_storage.export_count = (DB.pvcs || []).filter(p => p.access_mode === "ReadWriteMany" && DB.lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
    }
    c.rwx_pvcs_count = (DB.pvcs || []).filter(p => p.access_mode === "ReadWriteMany" && DB.lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
    const cvs = DB.lvols.filter(v => v.cluster_id === c.uuid);
    c.reduced_lvols_count = cvs.filter(v => v.compression_dedup_enabled).length;
    c.logical_used = sum(cvs, v => v.logical_used || v.size_util);
    if (c.kms) c.kms.keys_in_use = c.encrypted_lvols_count;
    // failure domains: node counts may differ by at most one across domains
    if (c.failure_domain_enabled) {
      const byFd = {};
      ns.forEach(n => {
        const k = n.failure_domain || "unassigned";
        byFd[k] = (byFd[k] || 0) + 1;
      });
      const counts = Object.values(byFd);
      c.failure_domains = Object.keys(byFd).sort().map(k => ({
        name: k,
        nodes: byFd[k]
      }));
      c.fd_unassigned = byFd.unassigned || 0;
      c.fd_min = counts.length ? Math.min(...counts) : 0;
      c.fd_max = counts.length ? Math.max(...counts) : 0;
      c.fd_balanced = c.fd_max - c.fd_min <= 1 && c.fd_min >= 2 && counts.length >= 2 && !c.fd_unassigned;
      c.fd_thin = c.failure_domains.filter(f => f.name !== "unassigned" && f.nodes < 2).map(f => f.name);
    } else {
      c.failure_domains = [];
      c.fd_unassigned = 0;
      c.fd_min = 0;
      c.fd_max = 0;
      c.fd_balanced = true;
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
    const vols = ap.pvc_ids.map(id => DB.pvcs.find(p => p.uuid === id)).filter(Boolean).map(p => DB.lvols.find(v => v.uuid === p.lvol_id)).filter(Boolean);
    ap.volumes = vols.map(v => ({
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      size: v.size_util,
      last_at: ap.protection_mode === "backup" ? (ap.recovery_points[0] || {}).at || null : (v.replication || {}).last_replication_at || null,
      written_since: ap.protection_mode === "backup" ? Math.round(v.size_util * (.002 + Math.random() * .02)) : (v.replication || {}).backlog_bytes || 0,
      status: (v.replication || {}).status || (ap.protection_mode === "backup" ? "healthy" : "unhealthy")
    }));
    // the group is only as current as its most stale member
    ap.group_last_at = ap.volumes.reduce((t, v) => v.last_at && (!t || Date.parse(v.last_at) < Date.parse(t)) ? v.last_at : t, null);
    ap.group_written_since = ap.volumes.reduce((n, v) => n + v.written_since, 0);
    ap.group_lag_seconds = ap.group_last_at ? Math.max(0, Math.round((NOW_MS - Date.parse(ap.group_last_at)) / 1000)) : null;
    const pol = (DB.dr_policies || []).find(x => x.uuid === ap.policy_id);
    const mins = pol ? pol.frequency_minutes || 0 : 5;
    const lag = (NOW_MS - Date.parse(ap.last_group_sync_at)) / 60000;
    // a healthy app has synced inside its scheduling interval
    if (!(ap.legs || []).length) {
      ap.rpo_met = pol && pol.mode === "synchronous" ? true : lag <= Math.max(1, mins) * 2;
      ap.health = ap.phase === "WaitForUser" ? "unhealthy" : !ap.rpo_met ? "degraded" : ap.progression === "Completed" ? "healthy" : "degraded";
    }
  });
  // The plan layer derives its own rollup from the legs, and it has to run last
  // so the per-leg health is what the application reports.
  if (window.SB_DR) window.SB_DR.drRollup();
  // One DR policy: the storage replication policy also carries the
  // applications. Its DR clusters are the Kubernetes clusters consuming the
  // storage clusters it links; the replication class is how Kubernetes names it.
  (DB.dr_policies || []).forEach(pol => {
    const consumers = cid => [...new Set((DB.storage_classes || []).filter(sc => sc.cluster_id === cid).map(sc => sc.k8s_cluster_id))].map(kid => (DB.dr_clusters || []).find(d => d.k8s_cluster_id === kid)).filter(Boolean).map(d => d.uuid);
    const ends = pol.mode === "synchronous" ? consumers(pol.source_cluster_id) : [...new Set(consumers(pol.source_cluster_id).concat(consumers(pol.target_cluster_id)))];
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
    p.storage_classes = scs2.map(x => ({
      uuid: x.uuid,
      name: x.name,
      variant: x.variant || "default",
      k8s_cluster_id: x.k8s_cluster_id,
      k8s_cluster: (DB.k8s_clusters.find(y => y.uuid === x.k8s_cluster_id) || {}).name || null
    }));
    p.storage_classes_count = scs2.length;
    p.k8s_cluster_id = scs2.length ? scs2[0].k8s_cluster_id : null;
  });
  (DB.storage_classes || []).forEach(sc => {
    sc.pvcs_count = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid).length;
    sc.bound_count = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid && p.status === "Bound").length;
    sc.provisioned_bytes = (DB.pvcs || []).filter(p => p.storage_class_id === sc.uuid).reduce((a, p) => a + (p.actual_bytes || 0), 0);
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
    kc.provisioned_bytes = (DB.pvcs || []).filter(x => x.k8s_cluster_id === kc.uuid).reduce((a, p) => a + (p.actual_bytes || 0), 0);
  });
  (DB.pvcs || []).forEach(p => {
    if (p.lvol_id && !DB.lvols.some(v => v.uuid === p.lvol_id)) {
      p.lvol_id = null;
      p.status = "Lost";
    }
  });
  (DB.buckets || []).forEach(b => {
    const v = DB.lvols.find(x => x.uuid === b.lvol_id);
    if (!v) {
      b.status = "unavailable";
      return;
    }
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
      return {
        uuid: id,
        name: k ? k.name : null
      };
    });
  });
  DB.hosts.forEach(h => {
    const z = DB.zones.find(x => x.uuid === h.zone_id);
    h.zone = z ? z.name : null;
    h.region = z ? z.region : null;
    if (z) {
      h.k8s_labels = Object.assign({}, h.k8s_labels, {
        "topology.kubernetes.io/zone": z.name,
        "topology.kubernetes.io/region": z.region
      });
      h.labels = Object.assign({}, h.labels, h.status === "available" ? {
        "topology.kubernetes.io/zone": z.name,
        "topology.kubernetes.io/region": z.region
      } : {});
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
    g.replication_policy = pol ? {
      uuid: pol.uuid,
      name: pol.name
    } : null;
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
        a.preferred_cluster_id = a.failover_cluster_id;
        a.failover_cluster_id = t;
        a.phase = a.kube_object_protection ? "FailedOver" : "WaitForUser";
        a.progression = progressionsFor(a.phase)[0];
        a.action = null;
        a.vrg_state = "primary";
        a.last_group_sync_at = ago(0);
        a.action_started_ms = null;
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
        a.preferred_cluster_id = a.failover_cluster_id;
        a.failover_cluster_id = t;
        a.phase = "Deployed";
        a.progression = progressionsFor("Deployed")[0];
        a.action = null;
        a.vrg_state = "primary";
        a.last_group_sync_at = ago(0);
        a.action_started_ms = null;
        if (a.preferred_site) a.active_site = a.preferred_site;
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    }
  });
  (DB.migrations || []).forEach(mg => {
    mg.lvol_ids = mg.lvol_ids.filter(id => DB.lvols.some(v => v.uuid === id));
    mg.lvols_count = mg.lvol_ids.length;
    mg.progress_pct = mg.state === "completed" ? 100 : mg.mode === "intra_cluster" ? mg.lvols_count ? Math.round(mg.moved_count / mg.lvols_count * 100) : 0 : Math.min(99, Math.round((1 - Math.log(Math.max(1, mg.last_snapshot_bytes)) / Math.log(Math.max(2, mg.first_snapshot_bytes))) * 100));
    mg.ready_to_cutover = mg.mode === "cross_cluster" && mg.last_snapshot_bytes <= mg.freeze_threshold_bytes && ["converging", "cutover_pending", "replicating"].includes(mg.state);
  });
  // A group that owns a policy owns it for every member: the group's policy is
  // pushed down onto the member volumes, so a volume can never sit in a
  // group-consistent policy while pointing at a different one of its own.
  DB.lvols.forEach(v => {
    v.consistency_groups = [];
  });
  DB.consistency_groups.forEach(g => {
    g.lvol_ids = g.lvol_ids.filter(id => DB.lvols.some(v => v.uuid === id));
    const members = DB.lvols.filter(v => g.lvol_ids.includes(v.uuid));
    members.forEach(v => v.consistency_groups.push({
      uuid: g.uuid,
      name: g.name
    }));
    // A group with active protection is frozen: its membership defines the
    // crash-consistent set the policy operates on, and changing it would make
    // the retained versions and the replica stream disagree about what the set
    // is. Overlapping groups are how a different set gets protected instead.
    g.locked = !!(g.backup_policy || g.replication_config);
    g.dr_policy_ids = DB.dr_policies.filter(p => p.cg_id === g.uuid).map(p => p.uuid);
    if (g.backup_policy) {
      const pol = DB.backup_policies.find(p => p.uuid === g.backup_policy.uuid);
      if (!pol) g.backup_policy = null;else {
        g.backup_policy.policy_name = pol.policy_name;
        members.forEach(v => {
          v.backup_policy = {
            uuid: pol.uuid,
            policy_name: pol.policy_name,
            via_cg: g.uuid
          };
        });
      }
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
          policy_id: p0 ? p0.uuid : null,
          policy_name: p0 ? p0.name : null,
          mode: p0 ? p0.mode : "asynchronous",
          consistency_group: g.name,
          via_cg: g.uuid,
          frequency_minutes: g.replication_config.frequency_minutes,
          status: v.replication && v.replication.status ? v.replication.status : "healthy",
          last_replication_at: v.replication && v.replication.last_replication_at || ago(g.replication_config.frequency_minutes / 120),
          backlog_bytes: v.replication && v.replication.backlog_bytes || 0
        });
      });
    }
    g.replication_status = g.replication_config ? members.some(v => v.replication && v.replication.status === "unhealthy") ? "unhealthy" : "healthy" : null;
    g.replication_backlog_bytes = members.reduce((n, v) => n + (v.replication && v.replication.backlog_bytes || 0), 0);
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
    if (f && f.enabled && f.mds_state === "restarting" && f.mds_restart_ms && Date.now() - f.mds_restart_ms > (f.failover_budget_seconds || 8) * 1000) {
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
    if (!n.op || typeof n.op.started_ms !== "number") return; // operator-driven ops advance in mock-k8s
    const spec = NODE_OPS[n.op.kind];
    if (!spec) return;
    const idx = Math.min(spec.phases.length - 1, Math.floor((Date.now() - n.op.started_ms) / spec.every));
    const task = (DB.tasks || []).find(t => t.uuid === n.op.task_id);
    const subs = (DB.tasks || []).filter(t => t.parent_id === n.op.task_id);
    for (let i = 0; i < idx; i++) {
      const t2 = subs.find(x => x.function_name === spec.subtasks[i]);
      if (t2 && t2.status !== "done") {
        t2.status = "done";
        t2.result = "done";
        t2.updated_at = ago(0);
      }
    }
    if (spec.phases[idx] === n.op.phase) return;
    n.op.phase = spec.phases[idx];
    n.op.phase_index = idx;
    if (idx < spec.phases.length - 1) return;
    // terminal phase: apply the effect
    const host = DB.hosts.find(x => x.uuid === n.host_id);
    if (n.op.kind === "removal") {
      if (host) {
        host.devices.forEach(d => {
          if (d.assigned_node_id === n.uuid) d.assigned_node_id = null;
        });
        host.storage_node_ids = host.storage_node_ids.filter(x => x !== n.uuid);
      }
      DB.devices = DB.devices.filter(d => d.node_id !== n.uuid);
      DB.storage_nodes = DB.storage_nodes.filter(x => x.uuid !== n.uuid);
      if (task) {
        task.status = "done";
        task.result = `Node removed, ${n.op.volumes_moved} volume(s) moved`;
        task.updated_at = ago(0);
      }
      rollup();
      return;
    }
    if (n.op.kind === "migration") {
      const tgt = DB.hosts.find(x => x.uuid === n.op.target_host_id);
      if (host) {
        host.devices.forEach(d => {
          if (d.assigned_node_id === n.uuid) d.assigned_node_id = null;
        });
        host.storage_node_ids = host.storage_node_ids.filter(x => x !== n.uuid);
      }
      if (tgt) {
        n.host_id = tgt.uuid;
        n.mgmt_ip = tgt.mgmt_ip;
        n.hostname = tgt.hostname;
        if (!tgt.storage_node_ids.includes(n.uuid)) tgt.storage_node_ids.push(n.uuid);
        tgt.devices.filter(d => !d.assigned_node_id).forEach(d => d.assigned_node_id = n.uuid);
        DB.devices.filter(d => d.node_id === n.uuid).forEach(d => {
          d.host_id = tgt.uuid;
          d.status = "online";
        });
      }
    }
    n.status = "online";
    DB.devices.filter(d => d.node_id === n.uuid).forEach(d => {
      if (d.status === "unavailable" || d.status === "new") d.status = "online";
    });
    if (task) {
      task.status = "done";
      task.result = spec.done;
      task.updated_at = ago(0);
    }
    n.op = null;
    rollup();
  });
  const drift = o => {
    if (!o.io_stats) return;
    const live = ["online", "read_only"].includes(o.status);
    const f = live ? 1 + (Math.random() - .5) * .14 : 0;
    o.io_stats = {
      read_io_ps: Math.round(o.io_stats.read_io_ps * f),
      write_io_ps: Math.round(o.io_stats.write_io_ps * f),
      read_bytes_ps: Math.round(o.io_stats.read_bytes_ps * f),
      write_bytes_ps: Math.round(o.io_stats.write_bytes_ps * f)
    };
    o.io_history.iops.push(o.io_stats.read_io_ps + o.io_stats.write_io_ps);
    o.io_history.iops.shift();
    o.io_history.bytes.push(o.io_stats.read_bytes_ps + o.io_stats.write_bytes_ps);
    o.io_history.bytes.shift();
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
        a.preferred_cluster_id = a.failover_cluster_id;
        a.failover_cluster_id = t;
        a.phase = a.kube_object_protection ? "FailedOver" : "WaitForUser";
        a.progression = progressionsFor(a.phase)[0];
        a.action = null;
        a.vrg_state = "primary";
        a.last_group_sync_at = ago(0);
        a.action_started_ms = null;
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
        a.preferred_cluster_id = a.failover_cluster_id;
        a.failover_cluster_id = t;
        a.phase = "Deployed";
        a.progression = progressionsFor("Deployed")[0];
        a.action = null;
        a.vrg_state = "primary";
        a.last_group_sync_at = ago(0);
        a.action_started_ms = null;
        if (a.preferred_site) a.active_site = a.preferred_site;
        if (window.SB_DR) window.SB_DR.drRollup();
      }
    }
  });
  (DB.migrations || []).forEach(mg => {
    if (["completed", "paused", "failed"].includes(mg.state)) return;
    if (mg.mode === "intra_cluster") {
      // instant migration: move one volume's primary per tick onto a tainted target
      if (mg.moved_count >= mg.lvols_count) {
        mg.state = "completed";
        mg.completed_at = ago(0);
        return;
      }
      const targets = DB.storage_nodes.filter(n => {
        if (n.cluster_id !== mg.target_cluster_id || n.status !== "online") return false;
        const h = DB.hosts.find(x => x.uuid === n.host_id);
        return h && (h.migration_taint || mg.target_zone_id && h.zone_id === mg.target_zone_id);
      });
      if (!targets.length) return;
      const v = DB.lvols.find(x => mg.lvol_ids.includes(x.uuid) && x.nodes && x.nodes.primary && !targets.some(t => t.uuid === x.nodes.primary.uuid));
      if (!v) {
        mg.state = "completed";
        mg.completed_at = ago(0);
        return;
      }
      const t = targets[mg.moved_count % targets.length];
      const from = v.nodes.primary.hostname;
      v.nodes = Object.assign({}, v.nodes, {
        primary: {
          uuid: t.uuid,
          hostname: t.hostname
        }
      });
      v.migration = {
        state: "completed",
        instant: true,
        from,
        target: t.hostname,
        reason: "cluster_migration",
        queued_at: ago(0),
        completed_at: ago(0)
      };
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
    if (p.mode === "synchronous") {
      p.backlog_bytes = 0;
      p.last_replication_at = ago(0);
      return;
    }
    const pair = (DB.cluster_pairs || []).find(x => x.uuid === p.pair_id);
    if (pair && pair.state === "unreachable") {
      p.state = "unhealthy";
      p.backlog_bytes += 4e8;
      return;
    }
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
    if (p.state === "unreachable") {
      p.link.throughput_bytes_ps = 0;
      return;
    }
    p.link.throughput_bytes_ps = Math.max(1e6, Math.round(p.link.throughput_bytes_ps * (1 + (Math.random() - .5) * .25)));
    p.link.rtt_ms = +Math.max(1, p.link.rtt_ms + (Math.random() - .5) * 2).toFixed(1);
  });
  rollup();
}
window.SB_NOW = NOW_MS;
window.SB_DB = DB;
window.SB_JITTER = jitter;
window.SB_UTIL = {
  uuid,
  hex,
  int,
  pick,
  ago,
  series,
  ioStats,
  rollup,
  inspectHost,
  mkNics,
  GB,
  TB,
  MODELS,
  NODE_OPS
};
})();
// ---- mock-api.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK API — intercepts fetch() for the control plane API v2 surface and
// answers from mock-backend.jsx. Remove this <script> tag (or set
// SB_CONFIG.mock = false) and the UI talks to the real control plane.
// ---------------------------------------------------------------------------
const SB_CONFIG = window.SB_CONFIG = Object.assign({
  apiBase: "/api/v2",
  token: "mock-token",
  mock: true
}, window.SB_CONFIG);
const SB_MOCK = window.SB_MOCK = {
  latency: [140, 380],
  // simulated round-trip range, ms
  failRate: 0,
  // 0..1 — random 503s
  forceEmpty: false,
  // return empty collections (empty-state design check)
  offline: false,
  // every request fails (error-state design check)
  failNext: false,
  requests: 0
};
const wait = ms => new Promise(r => setTimeout(r, ms));
const ok = body => new Response(JSON.stringify(Object.assign({
  status: true
}, body)), {
  status: 200,
  headers: {
    "Content-Type": "application/json"
  }
});
const fail = (code, msg) => new Response(JSON.stringify({
  status: false,
  error: msg
}), {
  status: code,
  headers: {
    "Content-Type": "application/json"
  }
});

// Why a protected group refuses membership changes.
const cgLocked = g => `${g.name} carries ${[g.backup_policy && "a backup policy", g.replication_config && "a replication cadence"].filter(Boolean).join(" and ")}, so its membership is fixed — the retained versions and the replica stream are all defined against exactly this set of volumes. Detach ${g.backup_policy && g.replication_config ? "them" : "it"} to change the members, or create a second consistency group with the volumes you want: a volume can belong to more than one group.`;
const D = () => window.SB_DB;
const U = () => window.SB_UTIL;
const byId = (coll, id) => D()[coll].find(x => x.uuid === id);
const where = (coll, k, v) => D()[coll].filter(x => x[k] === v);
const one = (coll, id) => {
  const r = byId(coll, id);
  return r ? {
    results: [r]
  } : {
    __404: true
  };
};
const list = (pColl, pId, coll, fk) => byId(pColl, pId) ? {
  results: SB_MOCK.forceEmpty ? [] : where(coll, fk, pId)
} : {
  __404: true
};
const drop = (coll, id) => {
  const i = D()[coll].findIndex(x => x.uuid === id);
  if (i < 0) return {
    __404: true
  };
  D()[coll].splice(i, 1);
  return {
    results: []
  };
};
const mut = (coll, id, f) => {
  const r = byId(coll, id);
  if (!r) return {
    __404: true
  };
  f(r);
  U().rollup();
  return {
    results: [r]
  };
};
const GET_ROUTES = [[/^\/clusters$/, () => ({
  results: D().clusters
})], [/^\/clusters\/([\w-]+)$/, m => one("clusters", m[1])], [/^\/clusters\/([\w-]+)\/hosts$/, m => list("clusters", m[1], "hosts", "cluster_id")], [/^\/clusters\/([\w-]+)\/storage-nodes$/, m => list("clusters", m[1], "storage_nodes", "cluster_id")], [/^\/clusters\/([\w-]+)\/pools$/, m => list("clusters", m[1], "pools", "cluster_id")], [/^\/clusters\/([\w-]+)\/lvols$/, m => list("clusters", m[1], "lvols", "cluster_id")], [/^\/clusters\/([\w-]+)\/snapshots$/, m => list("clusters", m[1], "snapshots", "cluster_id")], [/^\/clusters\/([\w-]+)\/backups$/, m => list("clusters", m[1], "backups", "cluster_id")], [/^\/clusters\/([\w-]+)\/backup-policies$/, m => list("clusters", m[1], "backup_policies", "cluster_id")], [/^\/clusters\/([\w-]+)\/consistency-groups$/, m => list("clusters", m[1], "consistency_groups", "cluster_id")], [/^\/clusters\/([\w-]+)\/migrations$/, m => byId("clusters", m[1]) ? {
  results: SB_MOCK.forceEmpty ? [] : D().migrations.filter(x => x.source_cluster_id === m[1])
} : {
  __404: true
}], [/^\/migrations$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().migrations
})], [/^\/migrations\/([\w-]+)$/, m => one("migrations", m[1])], [/^\/migrations\/([\w-]+)\/lvols$/, m => {
  const g = byId("migrations", m[1]);
  if (!g) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => g.lvol_ids.includes(v.uuid))
  };
}], [/^\/consistency-groups\/([\w-]+)$/, m => one("consistency_groups", m[1])], [/^\/consistency-groups\/([\w-]+)\/lvols$/, m => {
  const g = byId("consistency_groups", m[1]);
  if (!g) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => g.lvol_ids.includes(v.uuid))
  };
}], [/^\/consistency-groups\/([\w-]+)\/snapshots$/, m => list("consistency_groups", m[1], "cg_snapshots", "cg_id")], [/^\/cg-snapshots\/([\w-]+)$/, m => one("cg_snapshots", m[1])], [/^\/clusters\/([\w-]+)\/replication-policies$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().dr_policies.filter(p => p.source_cluster_id === m[1])
  };
}], [/^\/clusters\/([\w-]+)\/zones$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  return {
    results: D().zones.filter(s => (c.zone_ids || []).includes(s.uuid))
  };
}], [/^\/dr-clusters$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().dr_clusters
})], [/^\/dr-clusters\/([\w-]+)$/, m => one("dr_clusters", m[1])], [/^\/dr-clusters\/([\w-]+)\/protected-apps$/, m => {
  const dc = byId("dr_clusters", m[1]);
  if (!dc) return {
    __404: true
  };
  return {
    results: D().protected_apps.filter(a => a.preferred_cluster_id === dc.uuid || a.failover_cluster_id === dc.uuid)
  };
}], [/^\/replication-policies\/([\w-]+)\/protected-apps$/, m => list("dr_policies", m[1], "protected_apps", "policy_id")], [/^\/protected-apps$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().protected_apps
})], [/^\/protected-apps\/([\w-]+)$/, m => one("protected_apps", m[1])], [/^\/protected-apps\/([\w-]+)\/pvcs$/, m => {
  const a = byId("protected_apps", m[1]);
  if (!a) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().pvcs.filter(p => a.pvc_ids.includes(p.uuid))
  };
}], [/^\/kubernetes-clusters$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().k8s_clusters
})], [/^\/kubernetes-clusters\/([\w-]+)$/, m => one("k8s_clusters", m[1])], [/^\/kubernetes-clusters\/([\w-]+)\/storage-classes$/, m => list("k8s_clusters", m[1], "storage_classes", "k8s_cluster_id")], [/^\/kubernetes-clusters\/([\w-]+)\/pvcs$/, m => list("k8s_clusters", m[1], "pvcs", "k8s_cluster_id")], [/^\/kubernetes-clusters\/([\w-]+)\/hosts$/, m => list("k8s_clusters", m[1], "hosts", "k8s_cluster_id")], [/^\/kubernetes-clusters\/([\w-]+)\/storage-clusters$/, m => {
  const kc = byId("k8s_clusters", m[1]);
  if (!kc) return {
    __404: true
  };
  return {
    results: D().clusters.filter(c => (kc.storage_cluster_ids || []).includes(c.uuid))
  };
}], [/^\/kubernetes-clusters\/([\w-]+)\/zones$/, m => {
  const kc = byId("k8s_clusters", m[1]);
  if (!kc) return {
    __404: true
  };
  return {
    results: D().zones.filter(s => (kc.zone_ids || []).includes(s.uuid))
  };
}], [/^\/clusters\/([\w-]+)\/kubernetes-clusters$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  return {
    results: D().k8s_clusters.filter(k => (c.k8s_cluster_ids || []).includes(k.uuid))
  };
}], [/^\/storage-classes\/([\w-]+)$/, m => one("storage_classes", m[1])], [/^\/storage-classes\/([\w-]+)\/pvcs$/, m => list("storage_classes", m[1], "pvcs", "storage_class_id")], [/^\/pvcs$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().pvcs
})], [/^\/clusters\/([\w-]+)\/buckets$/, m => list("clusters", m[1], "buckets", "cluster_id")], [/^\/buckets\/([\w-]+)$/, m => one("buckets", m[1])], [/^\/pvcs\/([\w-]+)$/, m => one("pvcs", m[1])], [/^\/zones\/([\w-]+)\/kubernetes-clusters$/, m => {
  const s = byId("zones", m[1]);
  if (!s) return {
    __404: true
  };
  return {
    results: D().k8s_clusters.filter(k => (s.k8s_cluster_ids || []).includes(k.uuid))
  };
}], [/^\/zones$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().zones
})], [/^\/zones\/([\w-]+)$/, m => one("zones", m[1])], [/^\/zones\/([\w-]+)\/hosts$/, m => list("zones", m[1], "hosts", "zone_id")], [/^\/zones\/([\w-]+)\/clusters$/, m => {
  const s = byId("zones", m[1]);
  if (!s) return {
    __404: true
  };
  return {
    results: D().clusters.filter(c => (s.cluster_ids || []).includes(c.uuid))
  };
}], [/^\/cluster-pairs$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().cluster_pairs
})], [/^\/cluster-pairs\/([\w-]+)$/, m => one("cluster_pairs", m[1])], [/^\/cluster-pairs\/([\w-]+)\/replication-policies$/, m => {
  const p = byId("cluster_pairs", m[1]);
  if (!p) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().dr_policies.filter(x => x.pair_id === m[1])
  };
}], [/^\/replication-policies$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().dr_policies
})], [/^\/replication-policies\/([\w-]+)$/, m => one("dr_policies", m[1])], [/^\/replication-policies\/([\w-]+)\/lvols$/, m => {
  const p = byId("dr_policies", m[1]);
  if (!p) return {
    __404: true
  };
  return {
    results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => p.lvol_ids.includes(v.uuid))
  };
}], [/^\/hosts\/unassigned$/, () => ({
  results: SB_MOCK.forceEmpty ? [] : D().hosts.filter(h => !h.cluster_id)
})], [/^\/hosts\/([\w-]+)$/, m => one("hosts", m[1])], [/^\/storage-nodes\/([\w-]+)$/, m => one("storage_nodes", m[1])], [/^\/storage-nodes\/([\w-]+)\/devices$/, m => list("storage_nodes", m[1], "devices", "node_id")], [/^\/devices\/([\w-]+)$/, m => one("devices", m[1])], [/^\/pools\/([\w-]+)$/, m => one("pools", m[1])], [/^\/pools\/([\w-]+)\/lvols$/, m => list("pools", m[1], "lvols", "pool_id")], [/^\/pools\/([\w-]+)\/storage-classes$/, m => list("pools", m[1], "storage_classes", "pool_id")], [/^\/pools\/([\w-]+)\/snapshots$/, m => list("pools", m[1], "snapshots", "pool_id")], [/^\/pools\/([\w-]+)\/backups$/, m => list("pools", m[1], "backups", "pool_id")], [/^\/lvols\/([\w-]+)$/, m => one("lvols", m[1])], [/^\/lvols\/([\w-]+)\/snapshots$/, m => list("lvols", m[1], "snapshots", "lvol_id")], [/^\/lvols\/([\w-]+)\/backups$/, m => list("lvols", m[1], "backups", "lvol_id")], [/^\/snapshots\/([\w-]+)$/, m => one("snapshots", m[1])], [/^\/backups\/([\w-]+)$/, m => one("backups", m[1])], [/^\/backup-policies\/([\w-]+)$/, m => one("backup_policies", m[1])]];

// Start a phased node operation. The phase list and the subtask names come from
// SB_UTIL.NODE_OPS so the tracker, the tasks and the tick cannot drift apart.
function startNodeOp(n, kind, extra) {
  const spec = U().NODE_OPS[kind];
  n.status = spec.status;
  n.op = Object.assign({
    kind,
    phase: spec.phases[0],
    phase_index: 0,
    phases: spec.phases,
    started_at: U().ago(0),
    started_ms: Date.now(),
    volumes_moved: 0,
    task_id: null
  }, extra || {});
  if (D().tasks) {
    const master = {
      uuid: U().uuid(),
      cluster_id: n.cluster_id,
      parent_id: null,
      function_name: "node_" + kind,
      target_id: `NodeID:${n.uuid}`,
      node_id: n.uuid,
      distrib: null,
      retry: 0,
      max_retry: 3,
      status: "running",
      result: "running",
      created_at: U().ago(0),
      updated_at: U().ago(0),
      canceled: false,
      subtask_total: spec.subtasks.length
    };
    D().tasks.unshift(master);
    spec.subtasks.forEach((s, i) => D().tasks.push({
      uuid: U().uuid(),
      cluster_id: n.cluster_id,
      parent_id: master.uuid,
      function_name: s,
      target_id: null,
      node_id: n.uuid,
      distrib: null,
      retry: 0,
      max_retry: 0,
      status: i === 0 ? "running" : "new",
      result: "",
      created_at: U().ago(0),
      updated_at: U().ago(0),
      canceled: false,
      subtask_total: 0
    }));
    n.op.task_id = master.uuid;
  }
  return n;
}

// Every logical-volume move — manual, rebalance or affinity-driven — files an
// lvol_migration task. That task is how the move is followed, and the moved
// counters on the cluster are derived from these records.
function lvolMigrationTask(v, target, reason) {
  const t = {
    uuid: U().uuid(),
    cluster_id: v.cluster_id,
    parent_id: null,
    function_name: "lvol_migration",
    target_id: `LvolID:${v.uuid}`,
    node_id: target ? target.uuid : null,
    distrib: null,
    retry: 0,
    max_retry: 3,
    status: "done",
    result: `Volume ${v.lvol_name} moved to ${target ? target.hostname : "another node"}${reason ? " (" + reason + ")" : ""}`,
    created_at: U().ago(0),
    updated_at: U().ago(0),
    canceled: false,
    subtask_total: 0
  };
  if (D().tasks) D().tasks.unshift(t);
  return t;
}

// ---- mutations -------------------------------------------------------------
function addStorageNode(cluster, host) {
  const n = {
    uuid: U().uuid(),
    cluster_id: cluster.uuid,
    host_id: host.uuid,
    hostname: `${cluster.name.split("-").slice(0, 2).join("-")}-stor-${String(D().storage_nodes.filter(x => x.cluster_id === cluster.uuid).length + 1).padStart(2, "0")}`,
    data_nics: Array.from({
      length: cluster.multipathing_enabled ? 2 : 1
    }, (_, k) => ({
      name: host.data_nics && host.data_nics[k] || (k === 0 ? "ens1f0" : "ens1f1"),
      ip: `10.${U().int(10, 60)}.${U().int(0, 40)}.${U().int(2, 250)}`,
      port: 4420 + k,
      numa_socket: k,
      state: "up"
    })),
    mgmt_ip: host.mgmt_ip,
    failure_domain: host.labels["topology.kubernetes.io/zone"],
    physical_label: host.hostname,
    status: "in_restart",
    cpu_count: Math.round(host.vcpu_count / 2),
    vcpu_reserved: 15,
    max_subsystem_count: 100,
    memory_total: host.memory_total / 2,
    memory_reserved: Math.round(host.memory_total / 2 * .6),
    memory_used: 0,
    hugepages_total: host.hugepages_reserved,
    hugepages_used: 0,
    spdk_version: "v24.09"
  };
  D().storage_nodes.push(n);
  host.devices.filter(d => !d.assigned_node_id && (cluster.device_class === "nvme" ? d.kind === "nvme" : true)).forEach(hd => {
    hd.assigned_node_id = n.uuid;
    D().devices.push({
      uuid: U().uuid(),
      node_id: n.uuid,
      cluster_id: cluster.uuid,
      host_id: host.uuid,
      cluster_device_class: cluster.device_class,
      numa_socket: hd.numa_socket,
      serial_number: hd.serial_number || "PENDING",
      pcie_address: hd.pcie_address,
      device_name: hd.device_name,
      model_number: hd.model_number,
      firmware_revision: "GXA7711",
      status: "new",
      health_check: null,
      size_total: hd.size,
      size_util: 0,
      temperature_c: 34,
      percentage_used: 0,
      power_on_hours: 0,
      io_stats: U().ioStats(0, 0),
      io_history: {
        iops: U().series(0, 0),
        bytes: U().series(0, 0)
      }
    });
  });
  return n;
}
function normQos(b) {
  const n = k => b[k] === "" || b[k] === undefined || b[k] === null ? 0 : Number(b[k]);
  const q = {
    rw_ios_per_sec: n("rw_ios_per_sec"),
    rw_mbytes_per_sec: n("rw_mbytes_per_sec"),
    r_mbytes_per_sec: n("r_mbytes_per_sec"),
    w_mbytes_per_sec: n("w_mbytes_per_sec")
  };
  return Object.values(q).some(v => v > 0) ? q : null;
}

// Two rules govern failure domains, and both have to hold after the operation:
//   * every domain in use carries at least two nodes — a domain with one node
//     cannot lose it without losing the whole domain's share of the data
//   * node counts across domains differ by at most one
function fdBalanceCheck(c, host, op, node) {
  if (!c || !c.failure_domain_enabled) return null;
  const scope = c.failure_domain_scope;
  const fdOf = h => {
    if (!h) return null;
    if (scope === "zone") {
      const st = D().zones.find(x => x.uuid === h.zone_id);
      return st ? st.name : null;
    }
    return scope === "cabinet" ? h.cabinet_id : h.rack_id;
  };
  const target = op === "remove" ? node ? node.failure_domain : null : fdOf(host);
  if (!target) return `The host carries no ${scope} taint, so the node cannot be placed in a failure domain.`;
  const ns = D().storage_nodes.filter(n => n.cluster_id === c.uuid);
  const counts = {};
  (c.failure_domains || []).forEach(f => {
    counts[f.name] = 0;
  });
  ns.forEach(n => {
    const k = n.failure_domain || "unassigned";
    counts[k] = (counts[k] || 0) + 1;
  });
  if (counts[target] === undefined) counts[target] = 0;
  counts[target] += op === "remove" ? -1 : 1;
  const used = Object.entries(counts).filter(([k, v]) => k !== "unassigned" && v > 0);
  const list = () => Object.entries(counts).filter(([k]) => k !== "unassigned").map(([k, v]) => `${k}: ${v}`).join(", ");
  // a domain must hold at least two nodes to be a failure domain at all
  const thin = used.filter(([, v]) => v < 2);
  if (thin.length) {
    return op === "remove" ? `That would leave ${thin.map(([k, v]) => `${k}` + (v ? " with one node" : "")).join(", ")} (${list()}). Each ${scope} must carry at least two storage nodes — remove the domain's other node too, or add a node there first.` : `${thin.map(([k]) => k).join(", ")} would carry a single node (${list()}). Each ${scope} must carry at least two storage nodes, so add them in pairs.`;
  }
  const vals = used.map(([, v]) => v);
  if (vals.length < 2) return `A cluster with failure domains needs at least two ${scope}s, each with at least two storage nodes.`;
  const spread = Math.max(...vals) - Math.min(...vals);
  if (spread > 1) return `That would unbalance the failure domains (${list()}). Node counts per ${scope} may differ by at most one.`;
  return null;
}
function appendVersion(snap, bucket) {
  const D2 = D(),
    U2 = U();
  const v = D2.lvols.find(x => x.uuid === snap.lvol_id);
  if (!v) return {
    __err: "Source volume no longer exists"
  };
  let bk = D2.backups.find(x => x.lvol_id === snap.lvol_id);
  if (!bk) {
    bk = {
      uuid: U2.uuid(),
      cluster_id: v.cluster_id,
      pool_id: v.pool_id,
      pool_name: v.pool_name,
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      chain_id: `bk-${U2.hex(6)}`,
      policy_id: v.backup_policy ? v.backup_policy.uuid : null,
      policy_name: v.backup_policy ? v.backup_policy.policy_name : null,
      bucket: bucket || `s3://sb-backup-eu/${v.lvol_name}/`,
      status: "online",
      created_at: U2.ago(0),
      last_merge_at: null,
      versions: []
    };
    D2.backups.push(bk);
  }
  const seq = bk.versions.length + 1;
  const ver = {
    id: `v${String(seq).padStart(4, "0")}`,
    seq,
    tier: v.backup_policy ? "policy" : "manual",
    type: seq === 1 ? "full" : "delta",
    created_at: U2.ago(0),
    size: Math.round(seq === 1 ? v.size_util * .9 : snap.size),
    source_snapshot_id: snap.uuid,
    source_snapshot_name: snap.snapshot_name,
    merged_count: 0
  };
  bk.versions.push(ver);
  snap.backup_version_id = ver.id;
  return bk;
}
const MUT_ROUTES = [["POST", /^\/clusters$/, (m, b) => {
  if (!b.name) return {
    __err: "A cluster label is required"
  };
  const hosts = (b.host_ids || []).map(id => byId("hosts", id)).filter(Boolean);
  if (!hosts.length) return {
    __err: "Select at least one prepared host"
  };
  const edge = (b.location_type || "datacenter") === "edge";
  const deviceClass = edge ? "block" : b.device_class || "nvme";
  if (edge && b.device_class === "nvme") return {
    __err: "The NVMe device class is not available for edge clusters"
  };
  const c = {
    uuid: U().uuid(),
    name: b.name,
    device_class: deviceClass,
    status: "in_activation",
    location_type: edge ? "edge" : "datacenter",
    rebalancing: false,
    capabilities: {
      snapshot_replication: true,
      async_replication: true,
      rebalancing: !edge,
      tasks: !edge
    },
    multipathing_enabled: !!b.multipathing_enabled,
    node_affinity: b.node_affinity || "soft",
    pod_affinity_enabled: !!b.pod_affinity_enabled,
    auto_rebalance: {
      enabled: !edge
    },
    failure_domain_enabled: !!b.failure_domain_enabled,
    failure_domain_scope: b.failure_domain_enabled ? b.failure_domain_scope || "rack" : null,
    sync_replication_enabled: !!b.sync_replication_enabled,
    backup_enabled: !!b.backup_enabled,
    s3: b.backup_enabled ? {
      endpoint: b.s3_endpoint,
      region: b.s3_region,
      bucket: b.s3_bucket,
      path_prefix: b.s3_path_prefix || "",
      access_key_id: b.s3_access_key_id,
      secret_access_key: "••••••••••••••••••••",
      addressing: b.s3_addressing || "virtual-hosted",
      verify_tls: b.s3_verify_tls !== false
    } : null,
    kms: null,
    ha_type: "ha",
    distr_npcs: Number(b.distr_npcs || 1),
    distr_ndcs: Number(b.distr_ndcs || 2),
    blk_size: 4096,
    page_size_in_blocks: 2097152,
    cluster_version: "26.2.1",
    // the operator release, not a per-cluster choice
    mgmt_endpoint: edge ? `https://k8s-api.${b.name}.local:6443` : b.mgmt_endpoint || `https://${b.name}.simplyblock.internal:5000`,
    mgmt_endpoint_kind: "Kubernetes API",
    created_at: U().ago(0)
  };
  const zoneIds = [...new Set((b.zone_ids && b.zone_ids.length ? b.zone_ids : hosts.map(h => h.zone_id)).filter(Boolean))];
  c.zone_ids = zoneIds;
  c.stretched = zoneIds.length > 1;
  zoneIds.forEach(id => {
    const st = byId("zones", id);
    if (st) st.cluster_ids.push(c.uuid);
  });
  D().clusters.push(c);
  hosts.forEach(h => {
    h.cluster_id = c.uuid;
    addStorageNode(c, h);
  });
  D().pools.push({
    uuid: U().uuid(),
    cluster_id: c.uuid,
    pool_name: "default",
    enabled: true,
    qos: null
  });
  U().rollup();
  return {
    results: [c]
  };
}], ["PUT", /^\/lvols\/([\w-]+)\/qos$/, (m, b) => mut("lvols", m[1], v => {
  v.qos = normQos(b);
})],
// ---- file storage (pNFS) ------------------------------------------------
["PUT", /^\/clusters\/([\w-]+)\/file-storage$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (b.enabled) {
    const cand = D().hosts.filter(h => h.cluster_id === c.uuid && h.control_plane && h.status === "available");
    if (!cand.length) return {
      __err: "No control-plane worker available to run the NFS metadata server."
    };
    const mds = byId("hosts", b.mds_host_id) || cand[0];
    c.file_storage = Object.assign({}, c.file_storage, {
      enabled: true,
      nfs_version: "4.2",
      layout_type: b.layout_type || "flexfile",
      export_root: b.export_root || "/export/simplyblock",
      mds_host: {
        uuid: mds.uuid,
        hostname: mds.hostname
      },
      mds_candidates: cand.filter(h => h.uuid !== mds.uuid).slice(0, 3).map(h => ({
        uuid: h.uuid,
        hostname: h.hostname
      })),
      mds_state: "active",
      lease_seconds: Number(b.lease_seconds || 20),
      grace_seconds: Number(b.grace_seconds || 45),
      failover_budget_seconds: Number(b.failover_budget_seconds || 8),
      filesystem: "xfs",
      max_exports: Number(b.max_exports || 128)
    });
  } else {
    const inUse = D().pvcs.filter(p => p.access_mode === "ReadWriteMany" && D().lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
    if (inUse) return {
      __err: inUse + " RWX claim(s) still use pNFS. Delete them before disabling file storage."
    };
    c.file_storage = {
      enabled: false
    };
  }
  U().rollup();
  return {
    results: [c]
  };
}],
// The metadata server holds no local state, so it restarts on another
// control-plane worker within the failover budget; data paths keep serving.
["POST", /^\/clusters\/([\w-]+)\/file-storage\/failover$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (!c.file_storage.enabled) return {
    __err: "File storage is not enabled on this cluster."
  };
  const cand = c.file_storage.mds_candidates || [];
  if (!cand.length) return {
    __err: "No standby control-plane worker to move the metadata server to."
  };
  const prev = c.file_storage.mds_host;
  c.file_storage.mds_host = cand[0];
  c.file_storage.mds_candidates = cand.slice(1).concat(prev ? [prev] : []);
  c.file_storage.mds_state = "restarting";
  c.file_storage.mds_restarted_at = U().ago(0);
  c.file_storage.mds_restart_ms = Date.now();
  return {
    results: [c]
  };
}], ["PUT", /^\/clusters\/([\w-]+)\/object-storage$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (!b.enabled) {
    const n = (D().buckets || []).filter(x => x.cluster_id === c.uuid).length;
    if (n) return {
      __err: n + " bucket(s) still exist. Delete them before disabling object storage."
    };
    c.object_storage = {
      enabled: false
    };
  } else {
    c.object_storage = Object.assign({}, c.object_storage, {
      enabled: true,
      endpoint: b.endpoint,
      region: b.region,
      addressing: b.addressing || "virtual-hosted",
      metadata_backend: "foundationdb",
      versioning_default: !!b.versioning_default,
      max_buckets: Number(b.max_buckets || 500)
    });
  }
  U().rollup();
  return {
    results: [c]
  };
}],
// Compression-dedup is one per-volume switch and only takes effect for data
// written after the change; existing blocks keep their current form.
["PUT", /^\/lvols\/([\w-]+)\/data-reduction$/, (m, b) => mut("lvols", m[1], v => {
  v.compression_dedup_enabled = !!b.compression_dedup_enabled;
  v.logical_used = v.compression_dedup_enabled ? Math.round(v.size_util * 1.9) : v.size_util;
})], ["POST", /^\/cluster-pairs$/, (m, b) => {
  const src = byId("clusters", b.source_cluster_id),
    tgt = byId("clusters", b.target_cluster_id);
  if (!src || !tgt) return {
    __404: true
  };
  if (src.uuid === tgt.uuid) return {
    __err: "A cluster cannot be paired with itself"
  };
  if (!tgt.dr_target_eligible) return {
    __err: `${tgt.name} is not qualified as a DR target`
  };
  if (D().cluster_pairs.some(p => p.source_cluster_id === src.uuid && p.target_cluster_id === tgt.uuid)) return {
    __err: "This directional pair already exists"
  };
  const p = {
    uuid: U().uuid(),
    source_cluster_id: src.uuid,
    target_cluster_id: tgt.uuid,
    state: "pairing",
    link: {
      endpoint: b.endpoint || `nvmf://${tgt.name}.simplyblock.remote:4420`,
      nqn: `nqn.2023-02.io.simplyblock:repl:${U().hex(8)}`,
      rtt_ms: +(2 + Math.random() * 30).toFixed(1),
      bandwidth_mbit: Number(b.bandwidth_mbit || 10000),
      throughput_bytes_ps: 0
    },
    last_handshake_at: U().ago(0),
    created_at: U().ago(0),
    policies_count: 0
  };
  D().cluster_pairs.push(p);
  U().rollup();
  return {
    results: [p]
  };
}], ["POST", /^\/cluster-pairs\/([\w-]+)\/test$/, m => mut("cluster_pairs", m[1], p => {
  p.last_handshake_at = U().ago(0);
  if (p.state === "pairing") p.state = "paired";
})], ["DELETE", /^\/cluster-pairs\/([\w-]+)$/, m => {
  const p = byId("cluster_pairs", m[1]);
  if (!p) return {
    __404: true
  };
  if (D().dr_policies.some(x => x.pair_id === p.uuid)) return {
    __err: "This pair still carries replication policies. Delete them first."
  };
  const r = drop("cluster_pairs", m[1]);
  U().rollup();
  return r;
}], ["POST", /^\/replication-policies$/, (m, b) => {
  const sync = b.mode === "synchronous";
  const retention = (b.retention || []).filter(r => r.interval && Number(r.keep) > 0).map(r => ({
    interval: r.interval,
    keep: Number(r.keep)
  }));
  let src,
    pair = null,
    zoneIds = null,
    cg = null;
  if (sync) {
    src = byId("clusters", b.source_cluster_id);
    if (!src) return {
      __404: true
    };
    if (!src.sync_replication_enabled) return {
      __err: "Synchronous replication is not enabled on this cluster. The flag is set at creation time and cannot be changed."
    };
    zoneIds = (b.zone_ids || []).slice();
    if (zoneIds.length < 2) return {
      __err: "Synchronous replication needs at least two zones"
    };
    if (zoneIds.some(id => !(src.zone_ids || []).includes(id))) return {
      __err: "Every zone must be assigned to the cluster first"
    };
  } else {
    pair = byId("cluster_pairs", b.pair_id);
    if (!pair) return {
      __404: true
    };
    if (pair.state === "unreachable") return {
      __err: "The cluster pair link is down"
    };
    src = byId("clusters", pair.source_cluster_id);
    // The cadence is not a property of the policy: an asynchronous policy names
    // a consistency group, and the group owns the frequency and the retention.
    if (!b.cg_id) return {
      __err: "An asynchronous DR policy has to name a consistency group — the group owns the replication frequency and retention."
    };
    cg = byId("consistency_groups", b.cg_id);
    if (!cg) return {
      __err: "No such consistency group"
    };
    if (cg.cluster_id !== src.uuid) return {
      __err: "That group belongs to a different cluster than the pair's source."
    };
    if (!cg.replication_config) return {
      __err: `${cg.name} has no replication cadence. Attach replication to the group first.`
    };
  }
  const pol = {
    uuid: U().uuid(),
    name: b.name,
    mode: sync ? "synchronous" : "asynchronous",
    pair_id: pair ? pair.uuid : null,
    source_cluster_id: src.uuid,
    target_cluster_id: pair ? pair.target_cluster_id : null,
    zone_ids: zoneIds,
    cg_id: cg ? cg.uuid : null,
    cg_name: cg ? cg.name : null,
    frequency_minutes: cg ? cg.replication_config.frequency_minutes : 0,
    retention: cg ? cg.replication_config.retention : [],
    failback: {
      mode: b.failback_mode || "manual",
      frequency_minutes: Number(b.failback_frequency_minutes || b.frequency_minutes || 0),
      reverse_on_failover: b.reverse_on_failover !== false,
      resync_full: !!b.resync_full
    },
    state: "healthy",
    last_replication_at: U().ago(0),
    backlog_bytes: 0,
    generations_kept: (cg ? cg.replication_config.retention : []).reduce((a, r) => a + r.keep, 0),
    created_at: U().ago(0),
    last_failover_at: null,
    last_test_at: null,
    lvol_ids: []
  };
  D().dr_policies.push(pol);
  U().rollup();
  return {
    results: [pol]
  };
}], ["PUT", /^\/replication-policies\/([\w-]+)$/, (m, b) => mut("dr_policies", m[1], p => {
  // Frequency and retention are NOT settings of the policy: they belong to the
  // consistency group it names. Only the group reference is editable here.
  if (b.cg_id !== undefined && p.mode !== "synchronous") {
    if (!b.cg_id) return {
      __err: "An asynchronous DR policy has to name a consistency group."
    };
    const g = byId("consistency_groups", b.cg_id);
    if (!g) return {
      __err: "No such consistency group"
    };
    if (g.cluster_id !== p.source_cluster_id) return {
      __err: "That group belongs to a different cluster than the policy's source."
    };
    if (!g.replication_config) return {
      __err: `${g.name} has no replication cadence. Attach replication to the group first — the group owns the frequency and retention.`
    };
    p.cg_id = g.uuid;
    p.cg_name = g.name;
  }
  if (b.failback_mode) p.failback.mode = b.failback_mode;
  if (b.failback_frequency_minutes !== undefined) p.failback.frequency_minutes = Number(b.failback_frequency_minutes);
  if (b.resync_full !== undefined) p.failback.resync_full = !!b.resync_full;
  p.generations_kept = p.retention.reduce((a, r) => a + r.keep, 0);
  p.lvol_ids.forEach(id => {
    const v = byId("lvols", id);
    if (v && v.replication) v.replication.generations = p.generations_kept;
  });
})], ["POST", /^\/replication-policies\/([\w-]+)\/lvols$/, (m, b) => mut("dr_policies", m[1], p => {
  (b.lvol_ids || []).forEach(id => {
    const v = byId("lvols", id);
    if (!v || p.lvol_ids.includes(id) || v.replication) return;
    p.lvol_ids.push(id);
    v.replication = {
      policy_id: p.uuid,
      policy_name: p.name,
      mode: p.mode,
      status: "healthy",
      last_replication_at: U().ago(0),
      backlog_bytes: 0,
      consistency_group: p.consistency_group ? `cg-${p.name}` : null,
      generations: p.generations_kept
    };
  });
})], ["DELETE", /^\/replication-policies\/([\w-]+)\/lvols\/([\w-]+)$/, m => mut("dr_policies", m[1], p => {
  p.lvol_ids = p.lvol_ids.filter(x => x !== m[2]);
  const v = byId("lvols", m[2]);
  if (v) v.replication = null;
})], ["POST", /^\/replication-policies\/([\w-]+)\/failover$/, (m, b) => mut("dr_policies", m[1], p => {
  p.last_failover_at = U().ago(0);
  p.failover_mode = b.planned ? "planned cutover" : "unplanned failover";
  p.state = "degraded";
  if (p.failback.reverse_on_failover && p.pair_id) {
    const t = p.target_cluster_id;
    p.target_cluster_id = p.source_cluster_id;
    p.source_cluster_id = t;
    const back = D().cluster_pairs.find(x => x.source_cluster_id === p.source_cluster_id && x.target_cluster_id === p.target_cluster_id);
    p.pair_id = back ? back.uuid : p.pair_id;
  }
})], ["POST", /^\/replication-policies\/([\w-]+)\/failback$/, m => mut("dr_policies", m[1], p => {
  if (!p.last_failover_at) return;
  const t = p.target_cluster_id;
  p.target_cluster_id = p.source_cluster_id;
  p.source_cluster_id = t;
  p.failover_mode = null;
  p.last_failover_at = null;
  p.state = "healthy";
  p.backlog_bytes = 0;
})], ["POST", /^\/replication-policies\/([\w-]+)\/test-failover$/, m => mut("dr_policies", m[1], p => {
  p.last_test_at = U().ago(0);
})], ["POST", /^\/replication-policies\/([\w-]+)\/resync$/, m => mut("dr_policies", m[1], p => {
  p.backlog_bytes = 0;
  p.state = "healthy";
  p.last_replication_at = U().ago(0);
  p.lvol_ids.forEach(id => {
    const v = byId("lvols", id);
    if (v && v.replication) {
      v.replication.backlog_bytes = 0;
      v.replication.status = "healthy";
    }
  });
})], ["DELETE", /^\/replication-policies\/([\w-]+)$/, m => {
  const p = byId("dr_policies", m[1]);
  if (!p) return {
    __404: true
  };
  if (p.lvol_ids.length) return {
    __err: `${p.lvol_ids.length} volume(s) are still attached. A policy can only be deleted once it is empty.`
  };
  const r = drop("dr_policies", m[1]);
  U().rollup();
  return r;
}], ["POST", /^\/clusters\/([\w-]+)\/consistency-groups$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (!b.name) return {
    __err: "A consistency group name is required"
  };
  const ids = (b.lvol_ids || []).filter(id => {
    const v = byId("lvols", id);
    return v && v.cluster_id === c.uuid;
  });
  if (ids.length < 2) return {
    __err: "A consistency group needs at least two volumes"
  };
  if (D().consistency_groups.some(x => x.cluster_id === c.uuid && x.name === b.name)) return {
    __err: `A consistency group named ${b.name} exists on this cluster`
  };
  // Groups may overlap: the same volume can be a member of several groups, so
  // a different crash-consistent set can be protected without disturbing the
  // existing ones.
  const g = {
    uuid: U().uuid(),
    cluster_id: c.uuid,
    name: b.name,
    lvol_ids: ids,
    created_at: U().ago(0),
    backup_policy: null,
    replication_policy: null
  };
  D().consistency_groups.push(g);
  U().rollup();
  return {
    results: [g]
  };
}],
// A group can own its protection. Attaching a policy to the group attaches it
// to every member, and the group keeps it for members added later.
["PUT", /^\/consistency-groups\/([\w-]+)\/backup-policy$/, (m, b) => mut("consistency_groups", m[1], g => {
  if (!b.policy_id) {
    g.backup_policy = null;
    D().lvols.filter(v => g.lvol_ids.includes(v.uuid)).forEach(v => {
      if (v.backup_policy && v.backup_policy.via_cg === g.uuid) v.backup_policy = null;
    });
    return;
  }
  const p = byId("backup_policies", b.policy_id);
  if (!p) return {
    __err: "No such backup policy"
  };
  if (p.cluster_id !== g.cluster_id) return {
    __err: "The policy belongs to a different cluster"
  };
  // a policy driving a group has to be group-consistent, or the members would
  // be snapshotted at different instants and the group would mean nothing
  p.consistency_group = true;
  g.backup_policy = {
    uuid: p.uuid,
    policy_name: p.policy_name
  };
})],
// The group owns the replication cadence: how often the group snapshot is
// taken and shipped, and how many older generations the target keeps. A DR
// policy then just names the group.
["POST", /^\/consistency-groups\/([\w-]+)\/replicate$/, (m, b) => mut("consistency_groups", m[1], g => {
  const freq = Number(b.frequency_minutes);
  if (!freq || freq < 1) return {
    __err: "A replication frequency in minutes is required"
  };
  const retention = (b.retention || []).filter(r => r.interval && Number(r.keep) > 0).map(r => ({
    interval: r.interval,
    keep: Number(r.keep)
  }));
  g.replication_config = {
    frequency_minutes: freq,
    retention
  };
})], ["POST", /^\/consistency-groups\/([\w-]+)\/unreplicate$/, m => mut("consistency_groups", m[1], g => {
  const used = (D().dr_policies || []).filter(p => p.cg_id === g.uuid);
  if (used.length) return {
    __err: `${used.map(p => p.name).join(", ")} replicate${used.length > 1 ? "" : "s"} this group. Delete the DR polic${used.length > 1 ? "ies" : "y"} first.`
  };
  D().lvols.filter(v => g.lvol_ids.includes(v.uuid)).forEach(v => {
    if (v.replication && v.replication.via_cg === g.uuid) v.replication = null;
  });
  g.replication_config = null;
})], ["POST", /^\/consistency-groups\/([\w-]+)\/lvols$/, (m, b) => mut("consistency_groups", m[1], g => {
  if (g.backup_policy || g.replication_config) return {
    __err: cgLocked(g)
  };
  (b.lvol_ids || []).forEach(id => {
    const v = byId("lvols", id);
    if (!v || v.cluster_id !== g.cluster_id || g.lvol_ids.includes(id)) return;
    g.lvol_ids.push(id);
  });
})], ["DELETE", /^\/consistency-groups\/([\w-]+)\/lvols\/([\w-]+)$/, m => {
  const g = byId("consistency_groups", m[1]);
  if (!g) return {
    __404: true
  };
  if (g.backup_policy || g.replication_config) return {
    __err: cgLocked(g)
  };
  const app = (D().protected_apps || []).find(a => a.cg_id === g.uuid);
  if (app) return {
    __err: `${app.namespace}/${app.app_name} is protected through this group. Repoint the application first.`
  };
  if (g.lvol_ids.length <= 2) return {
    __err: "A consistency group must keep at least two volumes. Delete the group instead."
  };
  g.lvol_ids = g.lvol_ids.filter(x => x !== m[2]);
  U().rollup();
  return {
    results: [g]
  };
}], ["POST", /^\/consistency-groups\/([\w-]+)\/snapshot$/, (m, b) => {
  const g = byId("consistency_groups", m[1]);
  if (!g) return {
    __404: true
  };
  const vs = D().lvols.filter(v => g.lvol_ids.includes(v.uuid));
  const s = {
    uuid: U().uuid(),
    cluster_id: g.cluster_id,
    cg_id: g.uuid,
    cg_name: g.name,
    snapshot_name: b.name || `${g.name}-cgsnap`,
    created_at: U().ago(0),
    status: "online",
    members: vs.map(v => ({
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      snapshot_id: U().uuid(),
      size: Math.round(v.size_util * .05)
    })),
    backup_version_id: null,
    backup_bucket: null
  };
  // a group snapshot also lands as a per-volume snapshot on each member
  vs.forEach((v, i) => {
    const chain = D().snapshots.filter(x => x.lvol_id === v.uuid);
    D().snapshots.push({
      uuid: s.members[i].snapshot_id,
      cluster_id: v.cluster_id,
      pool_id: v.pool_id,
      pool_name: v.pool_name,
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      snapshot_name: `${s.snapshot_name}/${v.lvol_name}`,
      seq: chain.length + 1,
      parent_id: chain.length ? chain[chain.length - 1].uuid : null,
      created_at: s.created_at,
      size: s.members[i].size,
      status: "online",
      backup_version_id: null,
      cg_snapshot_id: s.uuid
    });
  });
  D().cg_snapshots.push(s);
  U().rollup();
  return {
    results: [s]
  };
}], ["POST", /^\/cg-snapshots\/([\w-]+)\/backup$/, (m, b) => mut("cg_snapshots", m[1], s => {
  s.backup_version_id = `v${String(D().cg_snapshots.filter(x => x.cg_id === s.cg_id && x.backup_version_id).length + 1).padStart(4, "0")}`;
  s.backup_bucket = b.bucket || `s3://sb-backup-eu/cg/${s.cg_name}/`;
})],
// Restore a group snapshot into new volumes — in this or any other cluster.
["POST", /^\/cg-snapshots\/([\w-]+)\/restore$/, (m, b) => {
  const s = byId("cg_snapshots", m[1]);
  if (!s) return {
    __404: true
  };
  const target = byId("clusters", b.cluster_id || s.cluster_id);
  if (!target) return {
    __404: true
  };
  const pool = D().pools.find(p => p.cluster_id === target.uuid && p.enabled !== false);
  if (!pool) return {
    __err: "The target cluster has no enabled pool to provision into"
  };
  const made = s.members.map((mem, i) => {
    const src = byId("lvols", mem.lvol_id) || {};
    const v = Object.assign({}, src, {
      uuid: U().uuid(),
      lvol_name: `${b.prefix || "restored"}-${mem.lvol_name}`,
      cluster_id: target.uuid,
      pool_id: pool.uuid,
      pool_name: pool.pool_name,
      status: "online",
      created_at: U().ago(0),
      size_util: mem.size,
      base_snapshot: {
        uuid: mem.snapshot_id,
        snapshot_name: s.snapshot_name,
        lvol_name: mem.lvol_name
      },
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
      snapshots_count: 0,
      backups_count: 0,
      consistency_groups: [],
      backup_policy: null,
      replication: null,
      migration: null
    });
    D().lvols.push(v);
    return v;
  });
  U().rollup();
  return {
    results: made
  };
}], ["DELETE", /^\/cg-snapshots\/([\w-]+)$/, m => {
  const s = byId("cg_snapshots", m[1]);
  if (s) D().snapshots = D().snapshots.filter(x => x.cg_snapshot_id !== s.uuid);
  const r = drop("cg_snapshots", m[1]);
  U().rollup();
  return r;
}], ["DELETE", /^\/consistency-groups\/([\w-]+)$/, m => {
  const g = byId("consistency_groups", m[1]);
  if (!g) return {
    __404: true
  };
  const app = (D().protected_apps || []).find(a => a.cg_id === g.uuid);
  if (app) return {
    __err: `${app.namespace}/${app.app_name} is protected through this group. Repoint the application first.`
  };
  if (g.backup_policy || g.replication_config) return {
    __err: "Detach the group's backup policy and replication cadence first."
  };
  if (D().cg_snapshots.some(x => x.cg_id === g.uuid)) return {
    __err: "Delete the group's snapshots first."
  };
  const r = drop("consistency_groups", m[1]);
  U().rollup();
  return r;
}],
// Store only the fields the chosen provider actually has, so switching from
// Vault to AWS KMS does not leave a stale transit mount behind.
["PUT", /^\/clusters\/([\w-]+)\/kms$/, (m, b) => mut("clusters", m[1], c => {
  if (!b.address) return;
  const p = b.provider || "hashicorp_vault";
  const kms = {
    provider: p,
    address: b.address,
    auth_method: b.auth_method || (p === "hashicorp_vault" ? "kubernetes" : "workload_identity"),
    key_name: b.key_name,
    key_type: b.key_type || "aes256-gcm96",
    verify_tls: b.verify_tls !== false,
    rotation_days: Number(b.rotation_days || 0),
    keys_in_use: c.encrypted_lvols_count || 0,
    status: "connected",
    last_check_at: U().ago(0)
  };
  if (b.auth_role) kms.auth_role = b.auth_role;
  if (p === "hashicorp_vault") {
    kms.namespace = b.namespace || null;
    kms.mount_path = b.mount_path || "transit";
  }
  if (p === "aws_kms") kms.region = b.region || null;
  c.kms = kms;
})], ["POST", /^\/clusters\/([\w-]+)\/kms\/test$/, m => mut("clusters", m[1], c => {
  if (!c.kms) return;
  c.kms.last_check_at = U().ago(0);
  c.kms.status = Math.random() > .15 ? "connected" : U().pick(["unreachable", "sealed"]);
})], ["PUT", /^\/hosts\/([\w-]+)\/migration-taint$/, (m, b) => mut("hosts", m[1], h => {
  h.migration_taint = b.taint || null;
})], ["POST", /^\/clusters\/([\w-]+)\/migrations$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (!b.name) return {
    __err: "A migration name is required"
  };
  const cross = b.mode === "cross_cluster";
  const tgtCluster = cross ? byId("clusters", b.target_cluster_id) : c;
  if (cross && !tgtCluster) return {
    __err: "Pick a target cluster"
  };
  if (cross && !tgtCluster.dr_target_eligible) return {
    __err: `${tgtCluster.name} is not qualified as a migration target`
  };
  if (cross && !D().cluster_pairs.some(p => p.source_cluster_id === c.uuid && p.target_cluster_id === tgtCluster.uuid)) return {
    __err: "Cross-cluster migration ships data over a cluster pair. Pair the two clusters first."
  };
  const vols = b.scope === "cluster" ? D().lvols.filter(v => v.cluster_id === c.uuid && v.status === "online") : (b.lvol_ids || []).map(id => byId("lvols", id)).filter(v => v && v.cluster_id === c.uuid);
  if (!vols.length) return {
    __err: "No volume selected to migrate"
  };
  if (!cross) {
    const tainted = D().hosts.filter(h => h.cluster_id === c.uuid && (h.migration_taint || b.target_zone_id && h.zone_id === b.target_zone_id) && (h.storage_node_ids || []).length);
    if (!tainted.length) return {
      __err: "No tainted target host with a storage node. Taint the destination hosts first."
    };
  }
  const first = vols.reduce((a, v) => a + v.size_util, 0);
  const g = {
    uuid: U().uuid(),
    cluster_id: c.uuid,
    name: b.name,
    mode: cross ? "cross_cluster" : "intra_cluster",
    scope: b.scope || "volumes",
    source_cluster_id: c.uuid,
    target_cluster_id: tgtCluster.uuid,
    target_zone_id: b.target_zone_id || null,
    target_taint: b.target_taint || "simplyblock.io/migration-target=true",
    follow_workload: !!b.follow_workload,
    state: cross ? "replicating" : "running",
    lvol_ids: vols.map(v => v.uuid),
    moved_count: 0,
    iterations: 0,
    iteration_limit: Number(b.iteration_limit || 12),
    first_snapshot_bytes: cross ? first : 0,
    last_snapshot_bytes: cross ? first : 0,
    freeze_threshold_bytes: cross ? Number(b.freeze_threshold_mb || 256) * 1e6 : 0,
    estimated_freeze_ms: 0,
    throughput_bytes_ps: 0,
    started_at: U().ago(0),
    completed_at: null,
    frozen_at: null,
    error: null
  };
  D().migrations.push(g);
  U().rollup();
  return {
    results: [g]
  };
}],
// Final phase: freeze IO briefly, apply the last small snapshot, roll the
// NVMe paths over to the target.
["POST", /^\/migrations\/([\w-]+)\/cutover$/, m => {
  const g = byId("migrations", m[1]);
  if (!g) return {
    __404: true
  };
  if (g.mode !== "cross_cluster") return {
    __err: "Intra-cluster migrations cut over per volume as they move."
  };
  if (g.last_snapshot_bytes > g.freeze_threshold_bytes) return {
    __err: `The outstanding snapshot is still ${Math.round(g.last_snapshot_bytes / 1e6)} MB — above the ${Math.round(g.freeze_threshold_bytes / 1e6)} MB freeze threshold. Let it converge further.`
  };
  const tgt = byId("clusters", g.target_cluster_id);
  const pool = D().pools.find(p => p.cluster_id === g.target_cluster_id && p.enabled !== false);
  if (!pool) return {
    __err: "The target cluster has no enabled pool"
  };
  const tgtNodes = D().storage_nodes.filter(n => n.cluster_id === g.target_cluster_id && n.status === "online");
  g.frozen_at = U().ago(0);
  g.lvol_ids.forEach((id, i) => {
    const v = byId("lvols", id);
    if (!v) return;
    v.cluster_id = g.target_cluster_id;
    v.pool_id = pool.uuid;
    v.pool_name = pool.pool_name;
    const t = tgtNodes[i % (tgtNodes.length || 1)];
    if (t) v.nodes = {
      primary: {
        uuid: t.uuid,
        hostname: t.hostname
      },
      secondary: tgtNodes[(i + 1) % (tgtNodes.length || 1)] ? {
        uuid: tgtNodes[(i + 1) % tgtNodes.length].uuid,
        hostname: tgtNodes[(i + 1) % tgtNodes.length].hostname
      } : null,
      tertiary: null
    };
    v.nqn = `nqn.2023-02.io.simplyblock:${U().hex(8)}`;
    v.migration = {
      state: "completed",
      instant: false,
      from: `${byId("clusters", g.source_cluster_id).name}`,
      target: tgt ? tgt.name : "target",
      reason: "cluster_migration",
      queued_at: g.started_at,
      completed_at: U().ago(0)
    };
    v.consistency_groups = [];
    v.replication = null;
  });
  g.moved_count = g.lvol_ids.length;
  g.state = "completed";
  g.completed_at = U().ago(0);
  U().rollup();
  return {
    results: [g]
  };
}], ["POST", /^\/migrations\/([\w-]+)\/pause$/, m => mut("migrations", m[1], g => {
  if (g.state !== "completed") {
    g.state = "paused";
    g.throughput_bytes_ps = 0;
  }
})], ["POST", /^\/migrations\/([\w-]+)\/resume$/, m => mut("migrations", m[1], g => {
  if (g.state === "paused") g.state = g.mode === "cross_cluster" ? "converging" : "running";
})], ["DELETE", /^\/migrations\/([\w-]+)$/, m => {
  const g = byId("migrations", m[1]);
  if (!g) return {
    __404: true
  };
  if (g.state === "frozen") return {
    __err: "The migration is mid-cutover and cannot be cancelled now."
  };
  const r = drop("migrations", m[1]);
  U().rollup();
  return r;
}],
// Ramen actions: failover moves the app to its failover cluster, relocate
// moves it back to the preferred one after a clean sync.
["POST", /^\/protected-apps\/([\w-]+)\/failover$/, (m, b = {}) => {
  const app = byId("protected_apps", m[1]);
  if (!app) return {
    __404: true
  };
  if (app.phase === "FailedOver") return {
    __err: "This application is already failed over. Use relocate to move it back."
  };
  if (app.phase === "WaitForUser") return {
    __err: "The previous failover is still waiting for the stale workload to be cleaned up. Confirm cleanup first."
  };
  if (["FailingOver", "Relocating"].includes(app.phase)) return {
    __err: "An action is already in flight on this application."
  };
  if (app.protection_mode === "backup") {
    // point-in-time: the last backup at or before the chosen generation / time
    const rp = b.recovery_point || {};
    const pts = (app.recovery_points || []).slice().sort((x, y) => y.generation - x.generation);
    const hit = rp.generation ? pts.find(p => p.generation <= Number(rp.generation)) : rp.before ? pts.find(p => new Date(p.at) <= new Date(rp.before)) : pts[0];
    if (!hit) return {
      __err: rp.before ? "No backup exists at or before that time." : "No backup at or below that generation."
    };
    app.restore_point = hit;
  }
  return mut("protected_apps", m[1], a => {
    a.phase = "FailingOver";
    a.action = "Failover";
    a.progression = a.protection_mode === "backup" ? "RestoringBackups" : "EnsuringVolumesAreSecondary";
    a.action_started_ms = Date.now();
  });
}], ["PUT", /^\/protected-apps\/([\w-]+)\/protection$/, (m, b) => {
  const a = byId("protected_apps", m[1]);
  if (!a) return {
    __404: true
  };
  if (["FailingOver", "Relocating"].includes(a.phase)) return {
    __err: "Protection cannot change while an action is running."
  };
  if (b.mode === "backup") {
    const pol0 = byId("dr_policies", a.policy_id);
    const g = pol0 && pol0.cg_id ? byId("consistency_groups", pol0.cg_id) : null;
    if (!g) return {
      __err: "This application's DR policy names no consistency group, so there is no group-consistent chain to recover from."
    };
    if (!g.backup_policy) return {
      __err: `${g.name} has no backup policy. Attach one to the group — the group's chain is what a backup-based recovery restores from.`
    };
    const pol = byId("backup_policies", g.backup_policy.uuid);
    if (!pol) return {
      __err: "The group's backup policy no longer exists"
    };
    a.protection_mode = "backup";
    a.backup_policy_id = pol.uuid;
    a.backup_policy_name = pol.policy_name;
    a.recovery_points = a.recovery_points && a.recovery_points.length ? a.recovery_points : [];
    a.pvc_ids.forEach(id => {
      const p = D().pvcs.find(x => x.uuid === id);
      const lv = p && D().lvols.find(x => x.uuid === p.lvol_id);
      if (lv) lv.backup_policy = {
        uuid: pol.uuid,
        policy_name: pol.policy_name
      };
    });
  } else {
    a.protection_mode = "replication";
    a.backup_policy_id = null;
    a.backup_policy_name = null;
    a.restore_point = null;
  }
  U().rollup();
  return {
    results: [a]
  };
}], ["POST", /^\/protected-apps\/([\w-]+)\/relocate$/, m => {
  const a = byId("protected_apps", m[1]);
  if (!a) return {
    __404: true
  };
  if (a.phase !== "FailedOver") return {
    __err: "Relocate returns a failed-over application to its preferred cluster. This one is not failed over."
  };
  if (!a.rpo_met) return {
    __err: "The last group sync is outside the scheduling interval. Let it catch up before relocating."
  };
  a.phase = "Relocating";
  a.action = "Relocate";
  a.progression = "WaitingForResourceRestore"; // first step of Relocating
  a.action_started_ms = Date.now();
  U().rollup();
  return {
    results: [a]
  };
}], ["POST", /^\/protected-apps\/([\w-]+)\/cleanup$/, m => mut("protected_apps", m[1], a => {
  if (a.phase !== "WaitForUser") return;
  a.phase = "FailedOver";
  a.progression = "Completed";
  a.action = null;
})], ["PUT", /^\/protected-apps\/([\w-]+)$/, (m, b) => mut("protected_apps", m[1], a => {
  if (b.policy_id !== undefined) {
    const pol = byId("dr_policies", b.policy_id);
    if (!pol) return {
      __err: "No such DR policy"
    };
    // The policy brings its consistency group with it: that group's members
    // are the volumes this application protects and fails over.
    const g = pol.cg_id ? byId("consistency_groups", pol.cg_id) : null;
    if (pol.mode === "asynchronous" && !g) return {
      __err: `${pol.name} names no consistency group.`
    };
    a.cg_id = g ? g.uuid : null;
    a.cg_name = g ? g.name : null;
    if (g) {
      const pvcs = g.lvol_ids.map(vid => D().pvcs.find(p => p.lvol_id === vid)).filter(Boolean);
      if (!pvcs.length) return {
        __err: `No PVC is backed by a volume of ${g.name}.`
      };
      a.pvc_ids = pvcs.map(p => p.uuid);
    }
    if (a.protection_mode === "backup") {
      if (!g || !g.backup_policy) {
        a.protection_mode = "replication";
        a.backup_policy_id = null;
        a.backup_policy_name = null;
      } else {
        a.backup_policy_id = g.backup_policy.uuid;
        a.backup_policy_name = g.backup_policy.policy_name;
      }
    }
  }
  if (b.policy_id && byId("dr_policies", b.policy_id)) {
    a.policy_id = b.policy_id;
    a.policy_name = byId("dr_policies", b.policy_id).name;
  }
  if (b.pvc_selector) a.pvc_selector = b.pvc_selector;
  if (b.kube_object_protection !== undefined) a.kube_object_protection = !!b.kube_object_protection;
})],
// An application's PVC set can come from a consistency group instead of a
// label selector: the group is then the crash-consistent boundary.

["PUT", /^\/protected-apps\/([\w-]+)\/recipe$/, (m, b) => {
  const a = byId("protected_apps", m[1]);
  if (!a) return {
    __404: true
  };
  if (["FailingOver", "Relocating"].includes(a.phase)) return {
    __err: "The recipe cannot change while a failover or relocate is running."
  };
  if (b === null || b.remove) {
    a.recipe = null;
    return {
      results: [a]
    };
  }
  if (!b.name || !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(b.name)) return {
    __err: "Recipe name must be a DNS-1123 label."
  };
  const groups = b.groups || [],
    hooks = b.hooks || [];
  if (groups.some(g => !g.name)) return {
    __err: "Every group needs a name."
  };
  if (new Set(groups.map(g => g.name)).size !== groups.length) return {
    __err: "Group names must be unique."
  };
  if (hooks.some(h => !h.name)) return {
    __err: "Every hook needs a name."
  };
  if (hooks.some(h => h.type === "exec" ? !((h.ops || [])[0] || {}).command : !((h.chks || [])[0] || {}).condition)) return {
    __err: "Every exec hook needs a command, every check hook a condition."
  };
  const refs = new Set([...groups.map(g => "group:" + g.name), ...hooks.flatMap(h => [].concat(h.ops || [], h.chks || []).map(o => "hook:" + h.name + "/" + o.name))]);
  for (const wf of ["captureWorkflow", "recoverWorkflow"]) {
    const bad = ((b[wf] || {}).sequence || []).find(s => !refs.has(s.group ? "group:" + s.group : "hook:" + s.hook));
    if (bad) return {
      __err: `${wf} references ${bad.group ? "group " + bad.group : "hook " + bad.hook}, which is not defined.`
    };
  }
  a.recipe = {
    name: b.name,
    namespace: a.namespace,
    appType: b.appType || null,
    groups,
    hooks,
    captureWorkflow: Object.assign({
      failOn: "any-error",
      sequence: []
    }, b.captureWorkflow || {}),
    recoverWorkflow: Object.assign({
      failOn: "any-error",
      sequence: []
    }, b.recoverWorkflow || {})
  };
  return {
    results: [a]
  };
}], ["DELETE", /^\/protected-apps\/([\w-]+)$/, m => {
  const a = byId("protected_apps", m[1]);
  if (!a) return {
    __404: true
  };
  if (["FailingOver", "Relocating"].includes(a.phase)) return {
    __err: "The application is mid-action. Wait for it to settle before unprotecting."
  };
  const r = drop("protected_apps", m[1]);
  U().rollup();
  return r;
}], ["POST", /^\/protected-apps$/, (m, b) => {
  const pol = byId("dr_policies", b.policy_id);
  if (!pol) return {
    __err: "Pick a DR policy"
  };
  if ((pol.dr_cluster_ids || []).length < 2) return {
    __err: `${pol.name} does not reach two Kubernetes clusters — no application can fail over on it.`
  };
  if (!b.app_name || !b.namespace) return {
    __err: "Application name and namespace are required"
  };
  const pref = byId("dr_clusters", b.preferred_cluster_id) || byId("dr_clusters", pol.dr_cluster_ids[0]);
  const other = pol.dr_cluster_ids.find(x => x !== pref.uuid);
  const claims = D().pvcs.filter(p => p.k8s_cluster_id === pref.k8s_cluster_id && p.namespace === b.namespace);
  if (!claims.length) return {
    __err: `No PVC found in namespace ${b.namespace} on ${pref.name}`
  };
  const a = {
    uuid: U().uuid(),
    app_name: b.app_name,
    namespace: b.namespace,
    app_kind: b.app_kind || "ApplicationSet",
    policy_id: pol.uuid,
    policy_name: pol.policy_name,
    preferred_cluster_id: pref.uuid,
    failover_cluster_id: other,
    pvc_selector: b.pvc_selector_key ? {
      [b.pvc_selector_key]: b.pvc_selector_value || ""
    } : {},
    phase: "Deployed",
    progression: "Completed",
    action: null,
    pvc_ids: claims.map(p => p.uuid),
    vrg_state: "primary",
    last_group_sync_at: U().ago(0),
    last_group_sync_duration_s: 0,
    last_group_sync_bytes: 0,
    kube_object_protection: !!b.kube_object_protection,
    created_at: U().ago(0)
  };
  D().protected_apps.push(a);
  U().rollup();
  return {
    results: [a]
  };
}], ["POST", /^\/dr-clusters\/([\w-]+)\/fence$/, m => mut("dr_clusters", m[1], dc => {
  dc.fencing_state = "ManuallyFenced";
})], ["POST", /^\/dr-clusters\/([\w-]+)\/unfence$/, m => mut("dr_clusters", m[1], dc => {
  dc.fencing_state = "Unfenced";
})],
// Zones are fixed when the cluster is created and cannot be changed afterwards.
["PUT", /^\/clusters\/([\w-]+)\/zones$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  return {
    __err: "Cluster zones are fixed at creation time and cannot be added or changed afterwards."
  };
}],
// zone and region are read from the node labels; only the rack / cabinet
// taints can be edited here
["PUT", /^\/hosts\/([\w-]+)\/placement$/, (m, b) => mut("hosts", m[1], h => {
  h.rack_id = b.rack_id || null;
  h.cabinet_id = b.cabinet_id || null;
})], ["POST", /^\/clusters\/([\w-]+)\/pools$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  const name = (b.name || "").trim();
  if (!name) return {
    __err: "Name the pool"
  };
  if (D().pools.some(p => p.cluster_id === c.uuid && p.pool_name === name)) return {
    __err: `A pool named ${name} exists on this cluster`
  };
  // bi-directional DH-CHAP is decided here and cannot be changed later: the
  // pool's namespaces are created with mutual authentication or without it
  const p = {
    uuid: U().uuid(),
    cluster_id: c.uuid,
    pool_name: name,
    enabled: true,
    dhchap_bidirectional: !!b.dhchap_bidirectional,
    qos: normQos(b),
    created_at: U().ago(0)
  };
  D().pools.push(p);
  U().rollup();
  return {
    results: [p]
  };
}], ["PUT", /^\/pools\/([\w-]+)\/qos$/, (m, b) => mut("pools", m[1], p => {
  p.qos = normQos(b);
})], ["POST", /^\/pools\/([\w-]+)\/enable$/, m => mut("pools", m[1], p => {
  p.enabled = true;
})], ["POST", /^\/pools\/([\w-]+)\/disable$/, m => mut("pools", m[1], p => {
  p.enabled = false;
})], ["POST", /^\/clusters\/([\w-]+)\/suspend$/, m => mut("clusters", m[1], c => {
  c.status = "suspended";
})], ["POST", /^\/clusters\/([\w-]+)\/activate$/, m => mut("clusters", m[1], c => {
  c.status = "in_activation";
})], ["POST", /^\/clusters\/([\w-]+)\/storage-nodes$/, (m, b) => {
  const c = byId("clusters", m[1]),
    h = byId("hosts", b.host_id);
  if (!c || !h) return {
    __404: true
  };
  if (h.storage_node_ids.length >= 2) return {
    __err: "Host already runs two storage nodes"
  };
  if ((c.zone_ids || []).length && !(c.zone_ids || []).includes(h.zone_id)) return {
    __err: "That host is not in one of the cluster's zones. Storage nodes can only be added from the zones assigned at cluster creation."
  };
  const fdErr = fdBalanceCheck(c, h, "add");
  if (fdErr) return {
    __err: fdErr
  };
  const n = addStorageNode(c, h);
  if (c.failure_domains_enabled && !b.failure_domain) return {
    __err: "This cluster uses failure domains. A new node must be given a failure domain label — it is fixed for the node's lifetime."
  };
  if (b.failure_domain) n.failure_domain = b.failure_domain;
  startNodeOp(n, "expansion");
  D().devices.filter(d => d.node_id === n.uuid).forEach(d => {
    d.status = "new";
  });
  U().rollup();
  return {
    results: [n]
  };
}], ["POST", /^\/storage-nodes\/([\w-]+)\/shutdown$/, (m, b) => mut("storage_nodes", m[1], n => {
  n.status = "offline";
  D().devices.filter(d => d.node_id === n.uuid).forEach(d => {
    if (d.status !== "removed") d.status = "unavailable";
  });
  n.last_action = b.force ? "force shutdown" : "shutdown";
  n.maintenance = true;
})], ["POST", /^\/storage-nodes\/([\w-]+)\/restart$/, m => mut("storage_nodes", m[1], n => {
  n.status = "in_restart";
  D().devices.filter(d => d.node_id === n.uuid).forEach(d => {
    if (d.status === "unavailable") d.status = "online";
  });
})], ["POST", /^\/storage-nodes\/([\w-]+)\/migrate$/, (m, b) => {
  const h = byId("hosts", b.host_id);
  if (!h) return {
    __404: true
  };
  const n0 = byId("storage_nodes", m[1]);
  const c0 = n0 ? byId("clusters", n0.cluster_id) : null;
  if (c0 && (c0.zone_ids || []).length && !(c0.zone_ids || []).includes(h.zone_id)) return {
    __err: "The target host is not in one of the cluster's zones."
  };
  if (n0 && n0.op) return {
    __err: `A ${n0.op.kind} is already running on this node.`
  };
  if (h.storage_node_ids.length >= 2) return {
    __err: "The target host already runs two storage nodes."
  };
  if (!h.prepared) return {
    __err: "The target host is not prepared. Prepare it first — hugepages and core isolation have to be in place before a node can restart on it."
  };
  return mut("storage_nodes", m[1], n => {
    startNodeOp(n, "migration", {
      target_host_id: h.uuid,
      target_hostname: h.hostname,
      source_hostname: n.hostname
    });
    D().devices.filter(d => d.node_id === n.uuid).forEach(d => {
      d.status = "unavailable";
    });
  });
}], ["POST", /^\/storage-nodes\/([\w-]+)\/devices$/, (m, b) => {
  const n = byId("storage_nodes", m[1]);
  if (!n) return {
    __404: true
  };
  const c = byId("clusters", n.cluster_id);
  const [model, size] = U().pick(U().MODELS);
  D().devices.push({
    uuid: U().uuid(),
    node_id: n.uuid,
    cluster_id: n.cluster_id,
    host_id: n.host_id,
    cluster_device_class: c.device_class,
    numa_socket: 0,
    serial_number: `S${U().hex(3).toUpperCase()}NY0${U().int(100000, 999999)}`,
    pcie_address: b.pcie_address || null,
    device_name: b.device_name || null,
    model_number: `${model} ${(size / 1e12).toFixed(2)}TB`,
    firmware_revision: "GXA7711",
    status: "new",
    health_check: null,
    size_total: size,
    size_util: 0,
    temperature_c: 33,
    percentage_used: 0,
    power_on_hours: 0,
    io_stats: U().ioStats(0, 0),
    io_history: {
      iops: U().series(0, 0),
      bytes: U().series(0, 0)
    }
  });
  U().rollup();
  return {
    results: []
  };
}], ["POST", /^\/devices\/([\w-]+)\/restart$/, m => mut("devices", m[1], d => {
  d.status = "online";
  d.health_check = d.health_check || "good";
})], ["POST", /^\/devices\/([\w-]+)\/fail$/, m => mut("devices", m[1], d => {
  d.status = "removed";
  d.health_check = null;
  d.size_util = 0;
})], ["POST", /^\/devices\/([\w-]+)\/health-check$/, m => mut("devices", m[1], d => {
  d.health_check = d.status === "online" || d.status === "read_only" ? U().pick(["good", "good", "good", "warn"]) : null;
  d.last_health_check = U().ago(0);
})], ["DELETE", /^\/devices\/([\w-]+)$/, m => {
  const d = byId("devices", m[1]);
  if (d) {
    const h = byId("hosts", d.host_id);
    if (h) h.devices.forEach(x => {
      if (x.pcie_address === d.pcie_address || x.device_name === d.device_name) x.assigned_node_id = null;
    });
  }
  const r = drop("devices", m[1]);
  U().rollup();
  return r;
}], ["DELETE", /^\/storage-nodes\/([\w-]+)$/, m => {
  const n = byId("storage_nodes", m[1]);
  if (!n) return {
    __404: true
  };
  const cRem = byId("clusters", n.cluster_id);
  if (cRem) {
    const e = fdBalanceCheck(cRem, byId("hosts", n.host_id), "remove", n);
    if (e) return {
      __err: e
    };
  }
  // Drain first: every volume whose primary sits here is moved off by instant
  // migration. Volumes pinned to this node block the removal.
  const others = D().storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
  const hosted = D().lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
  const pinned = hosted.filter(v => v.affinity && v.affinity.mode === "node" && v.affinity.pinned_node_id === n.uuid);
  if (pinned.length) return {
    __err: `${pinned.length} volume(s) are pinned to this node by affinity. Repin or clear their affinity first.`
  };
  if (hosted.length && !others.length) return {
    __err: `${hosted.length} volume(s) are primary on this node and there is no other online node to move them to.`
  };
  const counts = {};
  others.forEach(x => counts[x.uuid] = 0);
  D().lvols.filter(v => v.cluster_id === n.cluster_id && v.nodes && v.nodes.primary && v.nodes.primary.uuid !== n.uuid).forEach(v => {
    counts[v.nodes.primary.uuid] = (counts[v.nodes.primary.uuid] || 0) + 1;
  });
  hosted.forEach(v => {
    const t = others.slice().sort((p, q) => (counts[p.uuid] || 0) - (counts[q.uuid] || 0))[0];
    v.nodes = Object.assign({}, v.nodes, {
      primary: {
        uuid: t.uuid,
        hostname: t.hostname
      }
    });
    v.migration = {
      state: "completed",
      instant: true,
      from: n.hostname,
      target: t.hostname,
      reason: "node_removal",
      queued_at: U().ago(0),
      completed_at: U().ago(0)
    };
    counts[t.uuid] = (counts[t.uuid] || 0) + 1;
  });
  // Removal is asynchronous: the node enters in_removal and the control plane
  // works through it. finalizeRemovals() completes it a while later.
  startNodeOp(n, "removal", {
    volumes_moved: hosted.length
  });
  D().devices.filter(d => d.node_id === n.uuid).forEach(d => {
    d.status = "unavailable";
  });
  U().rollup();
  return {
    results: [n]
  };
}], ["POST", /^\/clusters\/([\w-]+)\/hosts\/prepare$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  const hosts = (b.host_ids || []).map(id => byId("hosts", id)).filter(h => h && h.cluster_id === c.uuid);
  if (!hosts.length) return {
    __err: "Select at least one worker node"
  };
  hosts.forEach(h => {
    h.status = "inspecting";
    h.inspection = {
      state: "running",
      started_at: U().ago(0),
      started_ms: Date.now(),
      pod: `sb-inspect-${h.hostname}`,
      step: "deploying inspection pod"
    };
  });
  U().rollup();
  return {
    results: hosts
  };
}], ["POST", /^\/hosts\/([\w-]+)\/configure$/, (m, b) => {
  const h = byId("hosts", m[1]);
  if (!h) return {
    __404: true
  };
  if (h.status !== "inspected") return {
    __err: "Host inventory has not been collected yet"
  };
  const sockets = (b.numa_sockets || []).map(Number);
  if (!sockets.length) return {
    __err: "Select at least one NUMA socket"
  };
  if (!(b.device_ids || []).length) return {
    __err: "Select at least one device to assign"
  };
  if (!b.mgmt_nic) return {
    __err: "Select a management NIC"
  };
  if (!(b.data_nics || []).length) return {
    __err: "Select at least one data NIC"
  };
  const hc = byId("clusters", h.cluster_id);
  if (hc && hc.multipathing_enabled && (b.data_nics || []).length !== 2) return {
    __err: "This cluster uses multipathing: exactly two data NICs must be specified per storage node."
  };
  if (hc && !hc.multipathing_enabled && (b.data_nics || []).length > 1) return {
    __err: "This cluster does not use multipathing: specify a single data NIC."
  };
  h.status = "available";
  h.numa_sockets_used = sockets;
  h.memory_per_pod = Number(b.memory_per_pod) * 1e9;
  h.hugepages_reserved = Number(b.hugepages_per_pod || Math.round(b.memory_per_pod / 2)) * 1e9;
  h.hugepages_allocated = 0;
  h.mgmt_nic = b.mgmt_nic;
  h.data_nics = b.data_nics;
  h.selected_device_ids = b.device_ids;
  h.devices = h.devices.filter(d => b.device_ids.includes(d.id));
  h.prepared_at = U().ago(0);
  h.labels = Object.assign({}, h.k8s_labels, {
    "simplyblock.io/storage-node": "true"
  });
  h.inspection = null;
  U().rollup();
  return {
    results: [h]
  };
}], ["POST", /^\/hosts\/([\w-]+)\/devices\/([\w-]+)\/reserve$/, (m, b) => {
  const h = byId("hosts", m[1]);
  if (!h) return {
    __404: true
  };
  const d = h.devices.find(x => x.id === m[2]);
  if (!d) return {
    __404: true
  };
  d.reserved_for_node_id = b.node_id;
  d.reserved = true;
  U().rollup();
  return {
    results: [h]
  };
}], ["DELETE", /^\/lvols\/([\w-]+)$/, m => {
  D().snapshots = D().snapshots.filter(s => s.lvol_id !== m[1]);
  const r = drop("lvols", m[1]);
  U().rollup();
  return r;
}],
// Volumes can only grow — shrinking is refused, as it is in Kubernetes.
["POST", /^\/lvols\/([\w-]+)\/resize$/, (m, b) => {
  const v = byId("lvols", m[1]);
  if (!v) return {
    __404: true
  };
  const size = Number(b.size);
  if (!(size > 0)) return {
    __err: "A positive size is required"
  };
  if (size < v.size_prov) return {
    __err: `Volumes can only be expanded. Current size is ${Math.round(v.size_prov / 1e9)} GB.`
  };
  if (size === v.size_prov) return {
    __err: "That is the current size"
  };
  v.size_prov = size;
  const pvc = (D().pvcs || []).find(p => p.lvol_id === v.uuid);
  if (pvc) {
    pvc.requested_bytes = size;
    pvc.actual_bytes = size;
  }
  U().rollup();
  return {
    results: [v]
  };
}],
// A PVC expansion propagates straight through to its logical volume.
["POST", /^\/pvcs\/([\w-]+)\/resize$/, (m, b) => {
  const p = byId("pvcs", m[1]);
  if (!p) return {
    __404: true
  };
  const sc = byId("storage_classes", p.storage_class_id);
  if (sc && sc.allow_volume_expansion === false) return {
    __err: `StorageClass ${sc.name} does not allow volume expansion.`
  };
  const size = Number(b.size);
  if (size < p.requested_bytes) return {
    __err: `A PVC can only be expanded. It currently requests ${Math.round(p.requested_bytes / 1e9)} GB.`
  };
  if (size === p.requested_bytes) return {
    __err: "That is the current request"
  };
  p.requested_bytes = size;
  p.actual_bytes = size;
  const v = p.lvol_id ? byId("lvols", p.lvol_id) : null;
  if (v) v.size_prov = size;
  U().rollup();
  return {
    results: [p]
  };
}],
// ---- buckets: one bucket is one filesystem is one logical volume --------
["POST", /^\/clusters\/([\w-]+)\/buckets$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (b.lvol_id) {
    const ex = byId("lvols", b.lvol_id);
    if (ex && ex.pvc) return {
      __err: "That volume already backs a PVC. A volume is either a bucket filesystem or a claim, never both."
    };
  }
  if (!c.object_storage.enabled) return {
    __err: "Object storage is not enabled on this cluster."
  };
  if (!b.name) return {
    __err: "A bucket name is required"
  };
  if (!/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(b.name)) return {
    __err: "Bucket names must be 3–63 characters, lowercase letters, digits, dots or hyphens."
  };
  if (D().buckets.some(x => x.cluster_id === c.uuid && x.name === b.name)) return {
    __err: "A bucket with that name already exists in this cluster."
  };
  const pool = byId("pools", b.pool_id) || D().pools.find(p => p.cluster_id === c.uuid && p.enabled !== false);
  if (!pool) return {
    __err: "No enabled pool to provision the bucket's volume into."
  };
  const nodes = D().storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
  const size = Number(b.size) || 1e12;
  const v = {
    uuid: U().uuid(),
    pool_id: pool.uuid,
    pool_name: pool.pool_name,
    cluster_id: c.uuid,
    lvol_name: `bucket-${b.name}`,
    status: "online",
    nodes: {
      primary: nodes[0] ? {
        uuid: nodes[0].uuid,
        hostname: nodes[0].hostname
      } : null,
      secondary: nodes[1] ? {
        uuid: nodes[1].uuid,
        hostname: nodes[1].hostname
      } : null,
      tertiary: null
    },
    size_prov: size,
    size_util: 0,
    crypto_enabled: !!b.encryption,
    qos: null,
    nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
    base_snapshot: null,
    consistency_groups: [],
    affinity: null,
    pvc: null,
    backup_policy: null,
    replication: null,
    migration: null,
    compression_dedup_enabled: true,
    logical_used: 0,
    created_at: U().ago(0),
    io_stats: U().ioStats(0, 0),
    io_history: {
      iops: U().series(0, 0),
      bytes: U().series(0, 0)
    }
  };
  D().lvols.push(v);
  const bucket = {
    uuid: U().uuid(),
    cluster_id: c.uuid,
    name: b.name,
    lvol_id: v.uuid,
    lvol_name: v.lvol_name,
    pool_id: pool.uuid,
    pool_name: pool.pool_name,
    status: "online",
    versioning: !!b.versioning,
    object_lock: !!b.object_lock,
    quota_bytes: Number(b.quota) || 0,
    objects: 0,
    size_bytes: 0,
    access: {
      service_account: b.service_account || `sb-s3-${b.name}`,
      namespace: b.namespace || "default",
      secret_name: `${b.name}-s3-credentials`,
      access_key_id: `SB${U().hex(9).toUpperCase()}`,
      policy: b.policy || "read-write",
      public: false
    },
    created_at: U().ago(0)
  };
  D().buckets.push(bucket);
  v.bucket = {
    uuid: bucket.uuid,
    name: bucket.name
  };
  U().rollup();
  return {
    results: [bucket]
  };
}], ["PUT", /^\/buckets\/([\w-]+)$/, (m, b) => mut("buckets", m[1], x => {
  if (b.versioning !== undefined) x.versioning = !!b.versioning;
  if (b.object_lock !== undefined) x.object_lock = !!b.object_lock;
  if (b.quota !== undefined) x.quota_bytes = Number(b.quota) || 0;
})], ["PUT", /^\/buckets\/([\w-]+)\/access$/, (m, b) => mut("buckets", m[1], x => {
  x.access = Object.assign({}, x.access, {
    service_account: b.service_account || x.access.service_account,
    namespace: b.namespace || x.access.namespace,
    policy: b.policy || x.access.policy,
    public: !!b.public
  });
  if (b.rotate_key) x.access.access_key_id = "SB" + U().hex(9).toUpperCase();
})], ["POST", /^\/buckets\/([\w-]+)\/resize$/, (m, b) => {
  const x = byId("buckets", m[1]);
  if (!x) return {
    __404: true
  };
  const v = byId("lvols", x.lvol_id);
  if (!v) return {
    __err: "The bucket's volume no longer exists."
  };
  const size = Number(b.size);
  if (size < v.size_prov) return {
    __err: `A bucket's filesystem can only grow. It is currently ${Math.round(v.size_prov / 1e9)} GB.`
  };
  v.size_prov = size;
  U().rollup();
  return {
    results: [x]
  };
}], ["DELETE", /^\/buckets\/([\w-]+)$/, m => {
  const x = byId("buckets", m[1]);
  if (!x) return {
    __404: true
  };
  if (x.objects > 0) return {
    __err: `The bucket still holds ${x.objects.toLocaleString()} object(s). Empty it first.`
  };
  D().lvols = D().lvols.filter(v => v.uuid !== x.lvol_id);
  D().snapshots = D().snapshots.filter(s2 => s2.lvol_id !== x.lvol_id);
  const r = drop("buckets", m[1]);
  U().rollup();
  return r;
}], ["POST", /^\/lvols\/([\w-]+)\/snapshot$/, (m, b) => {
  const v = byId("lvols", m[1]);
  if (!v) return {
    __404: true
  };
  const chain = D().snapshots.filter(x => x.lvol_id === v.uuid).sort((a, c) => a.seq - c.seq);
  const prev = chain[chain.length - 1] || null;
  const s = {
    uuid: U().uuid(),
    cluster_id: v.cluster_id,
    pool_id: v.pool_id,
    pool_name: v.pool_name,
    lvol_id: v.uuid,
    lvol_name: v.lvol_name,
    snapshot_name: b.name || `${v.lvol_name}-snap-${String(chain.length + 1).padStart(3, "0")}`,
    seq: chain.length + 1,
    parent_id: prev ? prev.uuid : null,
    created_at: U().ago(0),
    size: Math.round(v.size_util * 0.06),
    status: "online",
    backup_version_id: null
  };
  D().snapshots.push(s);
  U().rollup();
  return {
    results: [s]
  };
}], ["POST", /^\/lvols\/([\w-]+)\/clone$/, (m, b) => {
  const v = byId("lvols", m[1]);
  if (!v) return {
    __404: true
  };
  const c = Object.assign({}, v, {
    uuid: U().uuid(),
    lvol_name: b.name || v.lvol_name + "-clone",
    size_util: Math.round(v.size_util * .02),
    created_at: U().ago(0),
    status: "online",
    nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
    snapshots_count: 0,
    backups_count: 0,
    // a clone is a new volume: it is not protected until it is explicitly joined
    replication: null,
    base_snapshot: null,
    migration: null
  });
  D().lvols.push(c);
  U().rollup();
  return {
    results: [c]
  };
}],
// Instant volume migration: the primary role moves to another node with no
// data copy, so the move completes immediately rather than being queued.
["POST", /^\/lvols\/([\w-]+)\/migrate$/, (m, b) => mut("lvols", m[1], v => {
  const n = byId("storage_nodes", b.node_id);
  if (!n) return;
  const from = v.nodes && v.nodes.primary ? v.nodes.primary.hostname : "?";
  v.nodes = Object.assign({}, v.nodes, {
    primary: {
      uuid: n.uuid,
      hostname: n.hostname
    }
  });
  v.migration = {
    state: "completed",
    instant: true,
    from,
    target: n.hostname,
    reason: b.reason || "manual",
    queued_at: U().ago(0),
    completed_at: U().ago(0),
    task_id: lvolMigrationTask(v, n, b.reason || "manual").uuid
  };
  if (v.affinity && v.affinity.mode === "node") {
    v.affinity.pinned_node_id = n.uuid;
    v.affinity.pinned_node = n.hostname;
    v.affinity.satisfied = true;
  }
  if (v.affinity && v.affinity.mode === "pod") v.affinity.satisfied = v.affinity.workload_node === n.hostname;
})], ["POST", /^\/lvols\/([\w-]+)\/rebalance$/, m => {
  const v = byId("lvols", m[1]);
  if (!v) return {
    __404: true
  };
  const c = byId("clusters", v.cluster_id);
  const ns = D().storage_nodes.filter(n => n.cluster_id === v.cluster_id && n.status === "online");
  if (!ns.length) return {
    __err: "No online node to move the volume to"
  };
  const counts = {};
  ns.forEach(n => counts[n.uuid] = 0);
  D().lvols.filter(x => x.cluster_id === v.cluster_id && x.nodes && x.nodes.primary).forEach(x => {
    counts[x.nodes.primary.uuid] = (counts[x.nodes.primary.uuid] || 0) + 1;
  });
  const target = ns.slice().sort((p, q) => (counts[p.uuid] || 0) - (counts[q.uuid] || 0))[0];
  if (v.nodes && v.nodes.primary && v.nodes.primary.uuid === target.uuid) return {
    __err: "This volume already sits on the least loaded node"
  };
  const from = v.nodes && v.nodes.primary ? v.nodes.primary.hostname : "?";
  v.nodes = Object.assign({}, v.nodes, {
    primary: {
      uuid: target.uuid,
      hostname: target.hostname
    }
  });
  v.migration = {
    state: "completed",
    instant: true,
    from,
    target: target.hostname,
    reason: "rebalance",
    queued_at: U().ago(0),
    completed_at: U().ago(0),
    task_id: lvolMigrationTask(v, target, "rebalance").uuid
  };
  U().rollup();
  return {
    results: [v]
  };
}], ["PUT", /^\/lvols\/([\w-]+)\/affinity$/, (m, b) => mut("lvols", m[1], v => {
  if (b.mode === "none") {
    v.affinity = null;
    return;
  }
  if (b.mode === "node") {
    const n = byId("storage_nodes", b.node_id) || (v.nodes && v.nodes.primary ? byId("storage_nodes", v.nodes.primary.uuid) : null);
    v.affinity = {
      mode: "node",
      pinned_node_id: n ? n.uuid : null,
      pinned_node: n ? n.hostname : null,
      satisfied: true
    };
  } else {
    v.affinity = {
      mode: "pod",
      workload: b.workload || `${v.lvol_name}-0`,
      workload_node: v.nodes && v.nodes.primary ? v.nodes.primary.hostname : null,
      satisfied: true
    };
  }
})], ["PUT", /^\/clusters\/([\w-]+)\/auto-rebalance$/, (m, b) => mut("clusters", m[1], c => {
  c.auto_rebalance = Object.assign({}, c.auto_rebalance, {
    enabled: !!b.enabled
  });
})], ["POST", /^\/clusters\/([\w-]+)\/rebalance$/, m => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  if (!c.capabilities.rebalancing) return {
    __err: "Edge clusters do not rebalance."
  };
  const ns = D().storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
  if (ns.length < 2) return {
    __err: "Rebalancing needs at least two online nodes"
  };
  const counts = {};
  ns.forEach(n => counts[n.uuid] = 0);
  const vols = D().lvols.filter(v => v.cluster_id === c.uuid && v.nodes && v.nodes.primary);
  vols.forEach(v => {
    counts[v.nodes.primary.uuid] = (counts[v.nodes.primary.uuid] || 0) + 1;
  });
  let moves = 0;
  for (let pass = 0; pass < 40; pass++) {
    const sorted = ns.slice().sort((p, q) => (counts[q.uuid] || 0) - (counts[p.uuid] || 0));
    const hot = sorted[0],
      cold = sorted[sorted.length - 1];
    const goal = 1;
    if ((counts[hot.uuid] || 0) - (counts[cold.uuid] || 0) <= goal) break;
    const v = vols.find(x => x.nodes.primary.uuid === hot.uuid && !(x.affinity && x.affinity.mode === "node"));
    if (!v) break;
    const from = v.nodes.primary.hostname;
    v.nodes = Object.assign({}, v.nodes, {
      primary: {
        uuid: cold.uuid,
        hostname: cold.hostname
      }
    });
    v.migration = {
      state: "completed",
      instant: true,
      from,
      target: cold.hostname,
      reason: "rebalance",
      queued_at: U().ago(0),
      completed_at: U().ago(0),
      task_id: lvolMigrationTask(v, cold, "rebalance").uuid
    };
    counts[hot.uuid]--;
    counts[cold.uuid]++;
    moves++;
  }
  U().rollup();
  return {
    results: [c],
    message: `${moves} volume(s) moved`
  };
}],
// A backup is always taken from a snapshot. Backing "the volume" up means:
// take a snapshot now, then back that snapshot up — two independent objects.
["POST", /^\/lvols\/([\w-]+)\/backup$/, (m, b) => {
  const v = byId("lvols", m[1]);
  if (!v) return {
    __404: true
  };
  const chain = D().snapshots.filter(x => x.lvol_id === v.uuid).sort((a, c) => a.seq - c.seq);
  const prev = chain[chain.length - 1] || null;
  const snap = {
    uuid: U().uuid(),
    cluster_id: v.cluster_id,
    pool_id: v.pool_id,
    pool_name: v.pool_name,
    lvol_id: v.uuid,
    lvol_name: v.lvol_name,
    snapshot_name: `${v.lvol_name}-snap-${String(chain.length + 1).padStart(3, "0")}`,
    seq: chain.length + 1,
    parent_id: prev ? prev.uuid : null,
    created_at: U().ago(0),
    size: Math.round(v.size_util * .06),
    status: "online",
    backup_version_id: null
  };
  D().snapshots.push(snap);
  const r = appendVersion(snap, b.bucket);
  U().rollup();
  return r.__err ? r : {
    results: [r]
  };
}], ["POST", /^\/snapshots\/([\w-]+)\/backup$/, (m, b) => {
  const snap = byId("snapshots", m[1]);
  if (!snap) return {
    __404: true
  };
  if (snap.backup_version_id) return {
    __err: "A backup version has already been taken from this snapshot"
  };
  const r = appendVersion(snap, b.bucket);
  U().rollup();
  return r.__err ? r : {
    results: [r]
  };
}],
// Merge a version into its predecessor. Merging the earliest delta grows the
// full version and removes that delta — this is how retention ages out.
["POST", /^\/backups\/([\w-]+)\/versions\/([\w-]+)\/merge$/, m => {
  const bk = byId("backups", m[1]);
  if (!bk) return {
    __404: true
  };
  const i = bk.versions.findIndex(x => x.id === m[2]);
  if (i < 0) return {
    __404: true
  };
  if (i === 0) return {
    __err: "The full version has no predecessor to merge into"
  };
  const prev = bk.versions[i - 1],
    cur = bk.versions[i];
  prev.size += cur.size;
  prev.merged_count = (prev.merged_count || 0) + 1 + (cur.merged_count || 0);
  prev.created_at = cur.created_at;
  bk.versions.splice(i, 1);
  bk.versions.forEach((x, k) => {
    x.seq = k + 1;
    x.type = k === 0 ? "full" : "delta";
  });
  bk.last_merge_at = U().ago(0);
  U().rollup();
  return {
    results: [bk]
  };
}], ["POST", /^\/backups\/([\w-]+)\/merge$/, m => {
  const bk = byId("backups", m[1]);
  if (!bk) return {
    __404: true
  };
  if (bk.versions.length < 2) return {
    __err: "Nothing to merge — the chain holds a single full version"
  };
  const full = bk.versions[0],
    second = bk.versions[1];
  full.size += second.size;
  full.merged_count = (full.merged_count || 0) + 1 + (second.merged_count || 0);
  full.created_at = second.created_at;
  bk.versions.splice(1, 1);
  bk.versions.forEach((x, k) => {
    x.seq = k + 1;
    x.type = k === 0 ? "full" : "delta";
  });
  bk.last_merge_at = U().ago(0);
  U().rollup();
  return {
    results: [bk]
  };
}], ["PUT", /^\/lvols\/([\w-]+)\/backup-policy$/, (m, b) => mut("lvols", m[1], v => {
  const p = byId("backup_policies", b.policy_id);
  v.backup_policy = p ? {
    uuid: p.uuid,
    policy_name: p.policy_name
  } : null;
})], ["POST", /^\/clusters\/([\w-]+)\/backup-policies$/, (m, b) => {
  const c = byId("clusters", m[1]);
  if (!c) return {
    __404: true
  };
  const schedule = (b.schedule || []).filter(r => r.interval && Number(r.versions) > 0).map(r => ({
    interval: r.interval,
    versions: Number(r.versions),
    online: Number(r.online || 0)
  }));
  if (!schedule.length) return {
    __err: "A policy needs at least one schedule row"
  };
  const p = {
    uuid: U().uuid(),
    cluster_id: c.uuid,
    policy_name: b.name,
    schedule,
    consistency_group: !!b.consistency_group,
    created_at: U().ago(0)
  };
  D().backup_policies.push(p);
  U().rollup();
  return {
    results: [p]
  };
}], ["PUT", /^\/backup-policies\/([\w-]+)$/, (m, b) => mut("backup_policies", m[1], p => {
  if (b.consistency_group !== undefined) p.consistency_group = !!b.consistency_group;
  if (b.schedule) p.schedule = b.schedule.filter(r => r.interval && Number(r.versions) > 0).map(r => ({
    interval: r.interval,
    versions: Number(r.versions),
    online: Number(r.online || 0)
  }));
})], ["DELETE", /^\/backup-policies\/([\w-]+)$/, m => {
  const p = byId("backup_policies", m[1]);
  if (!p) return {
    __404: true
  };
  const used = D().lvols.filter(v => v.backup_policy && v.backup_policy.uuid === p.uuid).length;
  if (used) return {
    __err: `${used} volume(s) still use this policy. Detach them first.`
  };
  const r = drop("backup_policies", m[1]);
  U().rollup();
  return r;
}], ["DELETE", /^\/snapshots\/([\w-]+)$/, m => {
  // deleting the online snapshot leaves any backup version taken from it intact
  const s = byId("snapshots", m[1]);
  if (s) D().snapshots.filter(x => x.parent_id === s.uuid).forEach(x => {
    x.parent_id = s.parent_id;
  });
  const r = drop("snapshots", m[1]);
  U().rollup();
  return r;
}], ["POST", /^\/snapshots\/([\w-]+)\/restore$/, (m, b) => {
  const s = byId("snapshots", m[1]);
  if (!s) return {
    __404: true
  };
  const target = byId("clusters", b.cluster_id || s.cluster_id);
  if (!target) return {
    __404: true
  };
  const pool = D().pools.find(p => p.cluster_id === target.uuid && p.enabled !== false);
  if (!pool) return {
    __err: "The target cluster has no enabled pool to provision into"
  };
  const src = byId("lvols", s.lvol_id) || {};
  const v = Object.assign({}, src, {
    uuid: U().uuid(),
    lvol_name: b.name || `${s.lvol_name}-restored`,
    cluster_id: target.uuid,
    pool_id: pool.uuid,
    pool_name: pool.pool_name,
    status: "online",
    created_at: U().ago(0),
    size_util: s.size,
    base_snapshot: {
      uuid: s.uuid,
      snapshot_name: s.snapshot_name,
      lvol_name: s.lvol_name
    },
    nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
    snapshots_count: 0,
    backups_count: 0,
    consistency_groups: [],
    backup_policy: null,
    replication: null,
    migration: null
  });
  D().lvols.push(v);
  U().rollup();
  return {
    results: [v]
  };
}], ["POST", /^\/snapshots\/([\w-]+)\/clone$/, (m, b) => {
  const s = byId("snapshots", m[1]);
  if (!s) return {
    __404: true
  };
  const src = byId("lvols", s.lvol_id) || {};
  const c = Object.assign({}, src, {
    uuid: U().uuid(),
    lvol_name: b.name || s.snapshot_name + "-clone",
    size_util: Math.round(s.size * .05),
    created_at: U().ago(0),
    status: "online",
    base_snapshot: {
      uuid: s.uuid,
      snapshot_name: s.snapshot_name,
      lvol_name: s.lvol_name
    },
    nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
    snapshots_count: 0,
    backups_count: 0,
    replication: null,
    migration: null
  });
  D().lvols.push(c);
  U().rollup();
  return {
    results: [c]
  };
}],
// Restore rebuilds a volume from the chain up to the chosen version.
["POST", /^\/backups\/([\w-]+)\/restore$/, (m, b) => {
  const bk = byId("backups", m[1]);
  if (!bk) return {
    __404: true
  };
  const ver = b.version_id ? bk.versions.find(x => x.id === b.version_id) : bk.versions[bk.versions.length - 1];
  if (!ver) return {
    __err: "Unknown backup version"
  };
  const src = byId("lvols", bk.lvol_id) || {};
  const upTo = bk.versions.filter(x => x.seq <= ver.seq).reduce((a, x) => a + x.size, 0);
  const v = Object.assign({}, src, {
    uuid: U().uuid(),
    lvol_name: b.name || `${bk.lvol_name}-restored`,
    status: "online",
    created_at: U().ago(0),
    size_util: upTo,
    base_snapshot: null,
    nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
    snapshots_count: 0,
    backups_count: 0,
    backup_policy: null,
    replication: null,
    migration: null
  });
  D().lvols.push(v);
  U().rollup();
  return {
    results: [v]
  };
}], ["POST", /^\/backups\/([\w-]+)\/export$/, (m, b) => mut("backups", m[1], bk => {
  bk.exported_to = b.destination;
  bk.exported_version = b.version_id || (bk.versions[bk.versions.length - 1] || {}).id;
})],
// Deleting a volume backup deletes the whole chain.
["DELETE", /^\/backups\/([\w-]+)$/, m => {
  const bk = byId("backups", m[1]);
  if (bk) D().snapshots.filter(s => s.lvol_id === bk.lvol_id).forEach(s => {
    s.backup_version_id = null;
  });
  const r = drop("backups", m[1]);
  U().rollup();
  return r;
}]];

// The operator mock delegates /proposed mutations here, so the control-plane
// behaviours (validation, state changes) stay in one place.
window.SB_CP_ROUTES = {
  GET_ROUTES,
  MUT_ROUTES
};
const realFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const url = typeof input === "string" ? input : input.url;
  const base = SB_CONFIG.apiBase;
  if (!SB_CONFIG.mock || !url.startsWith(base)) return realFetch(input, init);
  const method = (init && init.method || "GET").toUpperCase();
  const route = url.slice(base.length).split("?")[0].replace(/\/$/, "");
  let body = {};
  try {
    if (init && init.body) body = JSON.parse(init.body);
  } catch (e) {}
  SB_MOCK.requests++;
  await wait(SB_MOCK.latency[0] + Math.random() * (SB_MOCK.latency[1] - SB_MOCK.latency[0]));
  if (SB_MOCK.offline) return fail(0, "Control plane unreachable (mock offline)");
  if (SB_MOCK.failNext) {
    SB_MOCK.failNext = false;
    return fail(503, "Upstream control plane returned 503 (injected)");
  }
  if (SB_MOCK.failRate && Math.random() < SB_MOCK.failRate) return fail(503, "Upstream control plane returned 503");
  if (method === "GET") {
    window.SB_JITTER();
    for (const [re, h] of GET_ROUTES) {
      const m = route.match(re);
      if (m) {
        const r = h(m);
        return r.__404 ? fail(404, "Resource not found") : r.__cap ? fail(501, r.__cap) : ok(r);
      }
    }
  } else {
    for (const [mm, re, h] of MUT_ROUTES) {
      if (mm !== method) continue;
      const m = route.match(re);
      if (m) {
        const r = h(m, body);
        if (r.__404) return fail(404, "Resource not found");
        if (r.__err) return fail(409, r.__err);
        return ok(r);
      }
    }
  }
  return fail(404, `No route for ${method} ${route}`);
};
window.SB_ADD_NODE = addStorageNode;
})();
// ---- k8s-client.jsx ----
(function(){
// ---------------------------------------------------------------------------
// KUBERNETES / OPERATOR / HELM TRANSPORT
//
// The console talks to exactly three things and nothing else:
//   · the Kubernetes API   — CRDs in storage.simplyblock.io/v1alpha1, plus core
//                            objects (Node, Pod, PVC, StorageClass, Secret,
//                            Event) and pod logs
//   · the operator API     — what the operator exposes beyond CRDs
//   · Helm                 — release state for the chart
//
// It does NOT call the control plane REST API. Anything the control plane knows
// reaches the UI through a CRD status written by a controller.
// ---------------------------------------------------------------------------
const SB = window.SB_CONFIG;
const GROUP = "storage.simplyblock.io";
const VERSION = "v1alpha1";
const API_GROUP = `${GROUP}/${VERSION}`;
class ApiError extends Error {
  constructor(status, message, path, reason) {
    super(message);
    this.status = status;
    this.path = path;
    this.reason = reason;
  }
}

// ---- resource registry -----------------------------------------------------
// Every kind the console reads, with the plural the API server routes on and the
// short name from the design (§7.11). `core: true` means it is not our group.
const RESOURCES = {
  // entities
  StorageCluster: {
    plural: "storageclusters",
    short: "sbc",
    namespaced: true
  },
  StorageNode: {
    plural: "storagenodes",
    short: "sbn",
    namespaced: true
  },
  StorageDevice: {
    plural: "storagedevices",
    short: "sbd",
    namespaced: true
  },
  StoragePool: {
    plural: "storagepools",
    short: "sbp",
    namespaced: true
  },
  StorageBackup: {
    plural: "storagebackups",
    short: "sbbk",
    namespaced: true
  },
  ControlPlane: {
    plural: "controlplanes",
    short: "sbcp",
    namespaced: true
  },
  SimplyblockDriver: {
    plural: "simplyblockdrivers",
    short: "sbdrv",
    namespaced: true
  },
  ClusterDeploymentConfig: {
    plural: "clusterdeploymentconfigs",
    short: "sbcdc",
    namespaced: true
  },
  // replication: real kinds in v1alpha1. A slot is created by the operator from
  // a PVC annotation, so it is read-only here; the other three are creatable.
  ReplicationPair: {
    plural: "replicationpairs",
    short: "relpair",
    namespaced: true,
    creatable: true
  },
  ReplicationPolicy: {
    plural: "replicationpolicies",
    short: "repl",
    namespaced: true,
    creatable: true
  },
  ReplicationSlot: {
    plural: "replicationslots",
    short: "relslot",
    namespaced: true
  },
  ReplicationOps: {
    plural: "replicationops",
    short: "replops",
    namespaced: true,
    creatable: true
  },
  // one-shot operations
  StorageClusterOps: {
    plural: "storageclusterops",
    short: "sbco",
    namespaced: true,
    ops: "StorageCluster"
  },
  StorageNodeOps: {
    plural: "storagenodeops",
    short: "sbno",
    namespaced: true,
    ops: "StorageNode"
  },
  StorageDeviceOps: {
    plural: "storagedeviceops",
    short: "sbdo",
    namespaced: true,
    ops: "StorageDevice"
  },
  StoragePoolOps: {
    plural: "storagepoolops",
    short: "sbpo",
    namespaced: true,
    ops: "StoragePool"
  },
  StorageBackupOps: {
    plural: "storagebackupops",
    short: "sbbo",
    namespaced: true,
    ops: "StorageBackup"
  },
  ControlPlaneOps: {
    plural: "controlplaneops",
    short: "sbcpo",
    namespaced: true,
    ops: "ControlPlane"
  },
  PersistentVolumeOps: {
    plural: "persistentvolumeops",
    short: "sbpvo",
    namespaced: true,
    ops: "PersistentVolume"
  },
  OperatorOps: {
    plural: "operatorops",
    short: "sbop",
    namespaced: true,
    ops: null
  },
  // core Kubernetes
  Node: {
    plural: "nodes",
    core: "v1",
    namespaced: false
  },
  Pod: {
    plural: "pods",
    core: "v1",
    namespaced: true
  },
  PersistentVolume: {
    plural: "persistentvolumes",
    core: "v1",
    namespaced: false
  },
  PersistentVolumeClaim: {
    plural: "persistentvolumeclaims",
    core: "v1",
    namespaced: true
  },
  Secret: {
    plural: "secrets",
    core: "v1",
    namespaced: true
  },
  Event: {
    plural: "events",
    core: "v1",
    namespaced: true
  },
  StorageClass: {
    plural: "storageclasses",
    core: "storage.k8s.io/v1",
    namespaced: false
  },
  // application resources read for Ramen recipes
  Deployment: {
    plural: "deployments",
    core: "apps/v1",
    namespaced: true
  },
  StatefulSet: {
    plural: "statefulsets",
    core: "apps/v1",
    namespaced: true
  },
  Service: {
    plural: "services",
    core: "v1",
    namespaced: true
  },
  ConfigMap: {
    plural: "configmaps",
    core: "v1",
    namespaced: true
  },
  Ingress: {
    plural: "ingresses",
    core: "networking.k8s.io/v1",
    namespaced: true
  },
  VirtualMachine: {
    plural: "virtualmachines",
    core: "kubevirt.io/v1",
    namespaced: true
  },
  Recipe: {
    plural: "recipes",
    core: "ramendr.openshift.io/v1alpha1",
    namespaced: true
  }
};
const NS = () => SB.namespace || "simplyblock";

// Build the API server path for a kind. Core group is /api/v1, everything else
// /apis/<group>/<version>.
function pathFor(kind, opts) {
  const o = opts || {};
  const r = RESOURCES[kind];
  if (!r) throw new Error("unknown kind " + kind);
  const prefix = !r.core ? `/apis/${API_GROUP}` : r.core === "v1" ? "/api/v1" : `/apis/${r.core}`;
  const ns = r.namespaced ? `/namespaces/${o.namespace || NS()}` : "";
  const name = o.name ? `/${o.name}` : "";
  const sub = o.subresource ? `/${o.subresource}` : "";
  return `${prefix}${ns}/${r.plural}${name}${sub}`;
}

// ---- transport -------------------------------------------------------------
async function call(base, path, init) {
  let res;
  const headers = Object.assign({
    Accept: "application/json",
    Authorization: `Bearer ${SB.token}`
  }, init && init.headers || {});
  try {
    res = await fetch(base + path, Object.assign({}, init, {
      headers
    }));
  } catch (e) {
    throw new ApiError(0, "Cannot reach the Kubernetes API", path, "Unreachable");
  }
  let body = null;
  try {
    body = await res.json();
  } catch (e) {}
  // A Kubernetes failure is a Status object. Trust the body, not just the code:
  // a Failure Status served with a 2xx is still a failure, and treating it as an
  // empty collection would show "no objects" during an outage.
  const failed = body && body.kind === "Status" && body.status === "Failure";
  if (!res.ok || failed) {
    const msg = body && body.message || res.statusText || "Request failed";
    throw new ApiError(body && body.code || res.status, msg, path, body && body.reason || null);
  }
  return body;
}
const k8s = {
  // list: returns items[], never the List envelope
  list: (kind, opts) => call(SB.k8sBase, pathFor(kind, opts) + qs(opts)).then(r => r && r.items || []),
  get: (kind, name, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({
    name
  }, opts))),
  create: (kind, obj, opts) => call(SB.k8sBase, pathFor(kind, opts), {
    method: "POST",
    headers: {
      "Content-Type": "application/json"
    },
    body: JSON.stringify(obj)
  }),
  // spec edits go through a merge patch, which is what kubectl edit does
  patch: (kind, name, patch, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({
    name
  }, opts)), {
    method: "PATCH",
    headers: {
      "Content-Type": "application/merge-patch+json"
    },
    body: JSON.stringify(patch)
  }),
  remove: (kind, name, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({
    name
  }, opts)), {
    method: "DELETE"
  }),
  // pod logs are the Kubernetes API, not a side channel
  logs: (pod, opts) => call(SB.k8sBase, pathFor("Pod", {
    name: pod,
    subresource: "log"
  }) + qs(Object.assign({
    tailLines: 200
  }, opts)))
};
function qs(opts) {
  const o = Object.assign({}, opts);
  delete o.namespace;
  delete o.name;
  delete o.subresource;
  const parts = Object.entries(o).filter(([, v]) => v !== undefined && v !== null && v !== "").map(([k, v]) => `${k}=${encodeURIComponent(v)}`);
  return parts.length ? "?" + parts.join("&") : "";
}

// label selector helpers — the ownership tree is expressed in labels
const sel = o => Object.entries(o).map(([k, v]) => `${k}=${v}`).join(",");
const ownedBy = (kind, name) => ({
  labelSelector: sel({
    [`${GROUP}/owner-kind`]: kind,
    [`${GROUP}/owner-name`]: name
  })
});
const inCluster = name => ({
  labelSelector: sel({
    [`${GROUP}/cluster`]: name
  })
});

// ---- operator API ----------------------------------------------------------
// What the operator serves that is not a CRD: the release/compat matrix it
// mirrors from install.simplyblock.io, and the SSE stream.
const operator = {
  releases: () => call(SB.operatorBase, "/releases"),
  health: () => call(SB.operatorBase, "/healthz"),
  // ?watch=true rows in the design are SSE subscriptions
  watch: (kind, onEvent) => {
    const url = SB.operatorBase + "/watch" + pathFor(kind);
    let es;
    try {
      es = new EventSource(url);
    } catch (e) {
      return () => {};
    }
    es.onmessage = e => {
      try {
        onEvent(JSON.parse(e.data));
      } catch (x) {}
    };
    return () => es.close();
  }
};

// ---- Helm ------------------------------------------------------------------
const helm = {
  releases: () => call(SB.helmBase, "/releases"),
  release: name => call(SB.helmBase, `/releases/${name}`),
  values: name => call(SB.helmBase, `/releases/${name}/values`)
};

// ---- Ops helpers -----------------------------------------------------------
// An action is not a verb against an entity: it is an <Entity>Ops object whose
// spec.action names it. Aborting is a patch of the one mutable field.
function opsKindFor(entityKind) {
  const hit = Object.entries(RESOURCES).find(([, r]) => r.ops === entityKind);
  return hit ? hit[0] : null;
}
function submitOps(entityKind, targetName, action, payload) {
  const kind = opsKindFor(entityKind);
  if (!kind) throw new Error("no Ops kind for " + entityKind);
  const stamp = Date.now().toString(36);
  const body = {
    apiVersion: API_GROUP,
    kind,
    metadata: {
      name: `${targetName}-${action.toLowerCase()}-${stamp}`.slice(0, 63),
      namespace: NS(),
      labels: {
        [`${GROUP}/target`]: targetName,
        [`${GROUP}/action`]: action
      }
    },
    spec: Object.assign({
      action
    }, targetName ? {
      targetRef: {
        name: targetName
      }
    } : {}, payload ? {
      [lowerFirst(action)]: payload
    } : {})
  };
  return k8s.create(kind, body);
}
const abortOps = (opsKind, name) => k8s.patch(opsKind, name, {
  spec: {
    abort: true
  }
});
const lowerFirst = s => s.charAt(0).toLowerCase() + s.slice(1);

// Terminal phases, per the design's phase vocabulary (PascalCase).
const OPS_TERMINAL = ["Succeeded", "Failed", "Aborted"];
const opsRunning = o => o && o.status && !OPS_TERMINAL.includes(o.status.phase);
Object.assign(window, {
  k8s,
  operator,
  helm,
  ApiError,
  RESOURCES,
  API_GROUP,
  GROUP,
  VERSION,
  pathFor,
  ownedBy,
  inCluster,
  sel,
  submitOps,
  abortOps,
  opsKindFor,
  opsRunning,
  OPS_TERMINAL,
  NS
});
})();
// ---- mock-k8s.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK KUBERNETES API SERVER
// Serves the fixture store as Kubernetes objects on the real paths, so the
// console can be developed against the same shapes the API server returns.
// Delete this script and point SB_CONFIG at a real cluster; nothing else moves.
//
// Kinds WITHOUT a CRD in storage.simplyblock.io/v1alpha1 — replication, DR,
// consistency groups, migrations, buckets, zones — are served from the operator
// API under /proposed/ and are flagged in the UI. That boundary is deliberate:
// it is the list of CRDs the model still owes the console.
// ---------------------------------------------------------------------------
const KDB = () => window.SB_DB;
const KU = () => window.SB_UTIL;
const KNS = () => window.SB_CONFIG.namespace || "simplyblock";
const KGROUP = "storage.simplyblock.io";
const KAPI = KGROUP + "/v1alpha1";
const kmeta = (name, o, extra) => Object.assign({
  name,
  namespace: KNS(),
  uid: o.uuid,
  creationTimestamp: o.created_at || o.prepared_at || null,
  generation: 1,
  resourceVersion: String(1000 + (o.__rv || 0)),
  labels: Object.assign({
    "app.kubernetes.io/managed-by": "simplyblock-operator"
  }, extra && extra.labels || {}),
  annotations: extra && extra.annotations || {}
}, extra && extra.rest || {});
const dns = s => String(s || "").toLowerCase().replace(/[^a-z0-9.-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63);
const clusterName = id => dns((KDB().clusters.find(c => c.uuid === id) || {}).name);
const activeOps = (kind, targetName) => {
  const o = (KDB().ops || []).find(x => x.target_kind === kind && x.target_name === targetName && !["Succeeded", "Failed", "Aborted"].includes(x.phase));
  return o ? {
    kind: o.kind,
    name: o.name,
    action: o.action
  } : null;
};
const cond = (type, ok, reason, message) => ({
  type,
  status: ok ? "True" : "False",
  reason,
  message,
  lastTransitionTime: KU().ago(0),
  observedGeneration: 1
});

// ---- entity mappers: fixture record -> Kubernetes object -------------------
const TO_K8S = {
  StorageCluster: c => ({
    apiVersion: KAPI,
    kind: "StorageCluster",
    metadata: kmeta(dns(c.name), c, {
      labels: {
        [KGROUP + "/environment"]: c.cluster_type
      }
    }),
    spec: {
      stripe: {
        dataChunks: c.distr_ndcs,
        parityChunks: c.distr_npcs
      },
      enableFailureDomains: !!c.failure_domain_enabled,
      edgeCluster: c.location_type === "edge",
      version: c.cluster_version,
      // status-side: the operator release
      enableFileStorage: !!(c.file_storage || {}).enabled,
      enableObjectStorage: !!(c.object_storage || {}).enabled,
      encryption: c.kms ? {
        provider: c.kms.provider,
        keyName: c.kms.key_name
      } : null
    },
    status: {
      phase: pascal(c.status),
      observedGeneration: 1,
      activeOpsRef: activeOps("StorageCluster", dns(c.name)),
      clusterId: c.uuid,
      nodes: {
        total: c.storage_nodes_count,
        ready: c.storage_nodes_online
      },
      devices: {
        total: c.devices_count,
        ready: c.devices_online
      },
      capacity: {
        total: String(c.size_total),
        used: String(c.size_util)
      },
      rebalancing: !!c.rebalancing,
      pools: c.pools_count,
      volumes: c.lvols_count,
      io: {
        readIops: c.io_stats.read_io_ps,
        writeIops: c.io_stats.write_io_ps,
        readBytes: c.io_stats.read_bytes_ps,
        writeBytes: c.io_stats.write_bytes_ps
      },
      ioHistory: c.io_history,
      failureDomains: (c.failure_domains || []).map(f => ({
        name: f.name,
        nodes: f.nodes
      })),
      extras: c,
      conditions: [cond("Ready", c.status === "online", pascal(c.status), "Cluster reported by the control plane"), cond("Degraded", c.status === "degraded", c.status === "degraded" ? "NodesUnavailable" : "AllNodesReady", "")]
    }
  }),
  StorageNode: n => ({
    apiVersion: KAPI,
    kind: "StorageNode",
    metadata: kmeta(dns(n.hostname), n, {
      labels: {
        [KGROUP + "/cluster"]: clusterName(n.cluster_id),
        [KGROUP + "/owner-kind"]: "StorageCluster",
        [KGROUP + "/owner-name"]: clusterName(n.cluster_id)
      },
      rest: {
        ownerReferences: [{
          apiVersion: KAPI,
          kind: "StorageCluster",
          name: clusterName(n.cluster_id),
          uid: n.cluster_id,
          controller: true
        }]
      }
    }),
    spec: {
      nodeName: (KDB().hosts.find(h => h.uuid === n.host_id) || {}).hostname || null,
      sizing: {
        maxSubsystemCount: n.max_subsystem_count,
        vcpuCount: n.vcpu_reserved,
        minHugePagesSize: Math.round((n.hugepages_total || 0) / 1e9) + "G"
      },
      mgmtInterface: (KDB().hosts.find(h => h.uuid === n.host_id) || {}).mgmt_nic || null,
      dataInterfaces: (n.data_nics || []).map(x => x.name),
      failureDomain: n.failure_domain || null
    },
    status: {
      phase: pascal(n.status),
      observedGeneration: 1,
      activeOpsRef: activeOps("StorageNode", dns(n.hostname)),
      nodeId: n.uuid,
      dataIPs: (n.data_nics || []).map(x => `${x.ip}:${x.port}`),
      devices: {
        total: n.devices_count,
        ready: n.devices_online
      },
      capacity: {
        total: String(n.size_total),
        used: String(n.size_util)
      },
      memory: {
        total: String(n.memory_total),
        used: String(n.memory_used),
        reserved: n.memory_reserved == null ? null : String(n.memory_reserved)
      },
      hugePages: {
        total: String(n.hugepages_total),
        used: String(n.hugepages_used)
      },
      spdkVersion: n.spdk_version,
      io: {
        readIops: n.io_stats.read_io_ps,
        writeIops: n.io_stats.write_io_ps,
        readBytes: n.io_stats.read_bytes_ps,
        writeBytes: n.io_stats.write_bytes_ps
      },
      ioHistory: n.io_history,
      extras: n,
      conditions: [cond("Ready", n.status === "online", pascal(n.status), "")]
    }
  }),
  StorageDevice: d => ({
    apiVersion: KAPI,
    kind: "StorageDevice",
    metadata: kmeta(dns(d.serial_number), d, {
      labels: {
        [KGROUP + "/cluster"]: clusterName(d.cluster_id),
        [KGROUP + "/owner-kind"]: "StorageNode",
        [KGROUP + "/owner-name"]: dns((KDB().storage_nodes.find(n => n.uuid === d.node_id) || {}).hostname)
      }
    }),
    spec: {
      pcieAddress: d.pcie_address,
      devicePath: d.device_name,
      deviceClass: d.pcie_address ? "NVMe" : "Block",
      numaSocket: d.numa_socket
    },
    status: {
      phase: pascal(d.status),
      observedGeneration: 1,
      activeOpsRef: activeOps("StorageDevice", dns(d.serial_number)),
      deviceId: d.uuid,
      serialNumber: d.serial_number,
      model: d.model_number,
      firmware: d.firmware_revision,
      health: d.health_check ? pascal(d.health_check) : null,
      capacity: {
        total: String(d.size_total),
        used: String(d.size_util)
      },
      temperatureCelsius: d.temperature_c,
      percentageUsed: d.percentage_used,
      powerOnHours: d.power_on_hours,
      io: {
        readIops: d.io_stats.read_io_ps,
        writeIops: d.io_stats.write_io_ps,
        readBytes: d.io_stats.read_bytes_ps,
        writeBytes: d.io_stats.write_bytes_ps
      },
      ioHistory: d.io_history,
      extras: d,
      conditions: [cond("Ready", d.status === "online", pascal(d.status), "")]
    }
  }),
  StoragePool: p => ({
    apiVersion: KAPI,
    kind: "StoragePool",
    metadata: kmeta(dns(p.pool_name), p, {
      labels: {
        [KGROUP + "/cluster"]: clusterName(p.cluster_id),
        [KGROUP + "/owner-kind"]: "StorageCluster",
        [KGROUP + "/owner-name"]: clusterName(p.cluster_id)
      }
    }),
    spec: {
      clusterRef: {
        name: clusterName(p.cluster_id)
      },
      enabled: p.enabled !== false,
      storageClassParameters: p.qos ? {
        qosRwIops: p.qos.rw_ios_per_sec,
        qosRwMbytes: p.qos.rw_mbytes_per_sec,
        qosRMbytes: p.qos.r_mbytes_per_sec,
        qosWMbytes: p.qos.w_mbytes_per_sec
      } : {}
    },
    status: {
      phase: p.enabled === false ? "Disabled" : "Enabled",
      observedGeneration: 1,
      activeOpsRef: activeOps("StoragePool", dns(p.pool_name)),
      poolId: p.uuid,
      storageClassNames: (p.storage_classes || []).map(x => x.name),
      volumes: {
        total: p.lvols_count,
        ready: p.lvols_online
      },
      capacity: {
        provisioned: String(p.size_prov),
        used: String(p.size_util)
      },
      extras: p,
      conditions: [cond("Ready", p.enabled !== false, p.enabled === false ? "Disabled" : "Enabled", "")]
    }
  }),
  StorageBackup: b => ({
    apiVersion: KAPI,
    kind: "StorageBackup",
    metadata: kmeta(dns(b.chain_id), b, {
      labels: {
        [KGROUP + "/cluster"]: clusterName(b.cluster_id),
        [KGROUP + "/volume"]: dns(b.lvol_name)
      }
    }),
    spec: {
      sourceRef: {
        kind: "PersistentVolume",
        name: dns(b.lvol_name)
      },
      bucket: b.bucket,
      policyName: b.policy_name || null
    },
    status: {
      phase: pascal(b.status || "online"),
      observedGeneration: 1,
      activeOpsRef: activeOps("StorageBackup", dns(b.chain_id)),
      backupId: b.uuid,
      chainId: b.chain_id,
      versions: (b.versions || []).map(v => ({
        id: v.id,
        sequence: v.seq,
        type: pascal(v.type),
        createdAt: v.created_at,
        size: String(v.size),
        sourceSnapshot: v.source_snapshot_name,
        mergedCount: v.merged_count || 0
      })),
      lastMergeAt: b.last_merge_at || null,
      extras: b,
      conditions: [cond("Ready", (b.status || "online") === "online", "Available", "")]
    }
  })
};
const pascal = s => String(s || "").split(/[_\s-]+/).map(w => w.charAt(0).toUpperCase() + w.slice(1)).join("");

// which fixture collection backs each kind, and how a name is derived
const COLL = {
  StorageCluster: ["clusters", c => dns(c.name)],
  StorageNode: ["storage_nodes", n => dns(n.hostname)],
  StorageDevice: ["devices", d => dns(d.serial_number)],
  StoragePool: ["pools", p => dns(p.pool_name)],
  StorageBackup: ["backups", b => dns(b.chain_id)]
};

// ---- Ops execution ---------------------------------------------------------
// Creating an Ops object runs the action. The object then carries the phase and
// the step, and the entity carries activeOpsRef until it reaches a terminal phase.
KDB().ops = KDB().ops || [];
const OPS_ACTIONS = {
  StorageClusterOps: ["Suspend", "Activate", "Expand", "Rebalance", "CancelTask"],
  StorageNodeOps: ["Restart", "Shutdown", "Migrate", "Remove", "AddDevice"],
  StorageDeviceOps: ["Restart", "Fail", "Remove", "HealthCheck"],
  StoragePoolOps: ["Enable", "Disable", "UpdateQos"],
  StorageBackupOps: ["Merge", "Restore", "Export", "Delete"],
  PersistentVolumeOps: ["Resize", "Snapshot", "Clone", "Migrate", "Backup"],
  ControlPlaneOps: ["Restart", "RestoreStateDatabase"],
  OperatorOps: ["Discover"]
};
// Steps are the state machine each action walks; the last one is terminal.
const OPS_STEPS = {
  Restart: ["Restarting", "Online"],
  Shutdown: ["ShuttingDown", "Offline"],
  Remove: ["DataMigration", "VolumeMigration", "Removed"],
  // a device is taken out of service, not destroyed: it can be added back
  RemoveDevice: ["Detaching", "Removed"],
  // failing one is permanent, and costs a full rebuild of its chunks
  Fail: ["Excluding", "Rebuilding", "Failed"],
  Migrate: ["RestartingNode", "Rebalancing", "RemovingNode", "Migrated"],
  Expand: ["AddingNode", "RebalancingData", "Complete"],
  Discover: ["Inspecting", "Collecting", "Writing"],
  Resize: ["Validating", "Expanding"],
  Default: ["Preparing", "Applying"]
};
// Which steps an abort can unwind. A step absent here refuses DELETE (§3.1).
const OPS_ABORTABLE = {
  Restart: [],
  Shutdown: ["ShuttingDown"],
  Migrate: ["RestartingNode"],
  Remove: ["DataMigration"],
  RemoveDevice: ["Detaching"],
  Fail: ["Excluding"],
  Expand: ["AddingNode"],
  Discover: ["Inspecting", "Collecting"]
};
function runOps(kind, obj) {
  const spec = obj.spec || {};
  const action = spec.action;
  if (!action) return {
    err: "spec.action is required",
    reason: "Invalid"
  };
  if (!(OPS_ACTIONS[kind] || []).includes(action)) return {
    err: `${action} is not a valid action for ${kind}. Valid: ${(OPS_ACTIONS[kind] || []).join(", ")}`,
    reason: "Invalid"
  };
  const targetKind = window.RESOURCES[kind].ops;
  const targetName = spec.targetRef ? spec.targetRef.name : null;
  if (targetKind && !targetName) return {
    err: "spec.targetRef.name is required",
    reason: "Invalid"
  };
  if (targetKind && activeOps(targetKind, targetName)) return {
    err: `${targetKind}/${targetName} already has an operation in flight. Wait for it or abort it first.`,
    reason: "Conflict"
  };
  const pre = preflight(kind, action, targetName, spec[action.charAt(0).toLowerCase() + action.slice(1)] || null);
  if (pre) return {
    err: pre,
    reason: "Invalid"
  };
  const steps = OPS_STEPS[kind === "StorageDeviceOps" && action === "Remove" ? "RemoveDevice" : action] || OPS_STEPS.Default;
  const rec = {
    uuid: KU().uuid(),
    kind,
    name: (obj.metadata || {}).name || dns(action + "-" + Date.now().toString(36)),
    action,
    target_kind: targetKind,
    target_name: targetName,
    cluster_id: clusterIdOf(targetKind, targetName),
    payload: spec[action.charAt(0).toLowerCase() + action.slice(1)] || null,
    abort: false,
    phase: "Running",
    step: steps[0],
    steps,
    started_ms: Date.now(),
    created_at: KU().ago(0),
    message: "",
    events: [{
      reason: "OperationStarted",
      message: `${action} started`,
      at: KU().ago(0)
    }]
  };
  KDB().ops.push(rec);
  applyEffect(rec);
  return {
    rec
  };
}

// Which cluster an operation belongs to, resolved from its target.
function clusterIdOf(targetKind, targetName) {
  const D = KDB();
  if (!targetName) return null;
  if (targetKind === "StorageCluster") {
    const c = D.clusters.find(x => dns(x.name) === targetName);
    return c ? c.uuid : null;
  }
  if (targetKind === "StorageNode") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === targetName);
    return n ? n.cluster_id : null;
  }
  if (targetKind === "StorageDevice") {
    const d = D.devices.find(x => dns(x.serial_number) === targetName);
    return d ? d.cluster_id : null;
  }
  if (targetKind === "StoragePool") {
    const p = D.pools.find(x => dns(x.pool_name) === targetName);
    return p ? p.cluster_id : null;
  }
  return null;
}

// What an action refuses before it starts. Same rules the REST mock enforced.
function preflight(kind, action, targetName, payload) {
  const D = KDB();
  const hostOf = p => p && p.host_id ? D.hosts.find(h => h.uuid === p.host_id) : null;
  if (kind === "StorageClusterOps" && action === "Expand") {
    const c = D.clusters.find(x => dns(x.name) === targetName);
    const h = hostOf(payload);
    if (!c) return "cluster not found";
    if (!h) return "spec.expand.host_id must name a host";
    if (h.storage_node_ids.length >= 2) return "That host already runs two storage nodes.";
    if ((c.zone_ids || []).length && !(c.zone_ids || []).includes(h.zone_id)) return "That host is not in one of the cluster's zones. Storage nodes can only be added from the zones assigned at cluster creation.";
    if (c.failure_domain_enabled || c.failure_domains_enabled) {
      const fd = payload.failure_domain || h.rack_id || h.zone;
      if (!fd) return "This cluster uses failure domains, so a new node needs a failure domain label. It is fixed for the node's lifetime.";
      // domains hold at least two nodes and stay within one node of each other
      const counts = {};
      D.storage_nodes.filter(n => n.cluster_id === c.uuid).forEach(n => {
        if (n.failure_domain) counts[n.failure_domain] = (counts[n.failure_domain] || 0) + 1;
      });
      counts[fd] = (counts[fd] || 0) + 1;
      const vals = Object.values(counts);
      const thin = Object.keys(counts).filter(x => counts[x] < 2);
      if (thin.length) return `${thin.join(", ")} would carry a single node. Each failure domain must carry at least two storage nodes, so add them in pairs.`;
      if (Math.max(...vals) - Math.min(...vals) > 1) return `That would unbalance the failure domains (${Object.keys(counts).map(x => x + ": " + counts[x]).join(", ")}). Node counts may differ by at most one.`;
    }
  }
  if (kind === "StorageNodeOps" && action === "Migrate") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === targetName);
    const h = hostOf(payload);
    if (!n) return "node not found";
    if (!h) return "spec.migrate.host_id must name a host";
    if (h.uuid === n.host_id) return "The node already runs on that host.";
    if (h.storage_node_ids.length >= 2) return "The target host already runs two storage nodes.";
    if (!h.prepared_at) return "The target host is not prepared. Hugepages and core isolation have to be in place before a node can restart on it.";
  }
  if (kind === "StorageNodeOps" && action === "Remove") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === targetName);
    if (!n) return "node not found";
    const others = D.storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
    const hosted = D.lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
    const pinned = hosted.filter(v => v.affinity && v.affinity.mode === "node" && v.affinity.pinned_node_id === n.uuid);
    if (pinned.length) return `${pinned.length} volume(s) are pinned to this node by affinity. Repin or clear their affinity first.`;
    if (hosted.length && !others.length) return `${hosted.length} volume(s) are primary on this node and there is no other online node to move them to.`;
  }
  return null;
}

// The phase names above, as the UI shows them.
const OPS_STEP_LABEL = {
  DataMigration: "data migration and rebalancing",
  VolumeMigration: "volume migration",
  Removed: "removed",
  Detaching: "detaching device",
  Excluding: "excluding device",
  Rebuilding: "rebuilding chunks",
  Failed: "failed",
  Restarting: "in restart",
  Online: "online",
  ShuttingDown: "in shutdown",
  Offline: "offline",
  RestartingNode: "restarting node",
  Rebalancing: "rebalancing",
  RemovingNode: "removing node",
  Migrated: "migrated",
  AddingNode: "adding node",
  RebalancingData: "rebalancing data",
  Complete: "complete"
};
// Mirror a running operation onto its node as a phase tracker the tiles read.
function syncNodeOp(o) {
  const D = KDB();
  const n = D.storage_nodes.find(x => dns(x.hostname) === o.target_name);
  if (!n) return;
  const kind = {
    Remove: "removal",
    Migrate: "migration",
    Expand: "expansion"
  }[o.action];
  if (!kind) return;
  if (["Succeeded", "Failed", "Aborted"].includes(o.phase)) {
    n.op = null;
    return;
  }
  n.op = {
    kind,
    phase: OPS_STEP_LABEL[o.step] || o.step,
    phase_index: Math.max(0, o.steps.indexOf(o.step)),
    phases: o.steps.map(s => OPS_STEP_LABEL[s] || s),
    started_at: o.created_at,
    volumes_moved: n.op && n.op.volumes_moved || 0,
    target_hostname: n.op && n.op.target_hostname || o.payload && o.payload.targetHost || null,
    source_hostname: n.op && n.op.source_hostname || null,
    task_id: o.uuid
  };
}

// The fixture-store side effect of an action, applied when the operation starts.
function applyEffect(rec) {
  const D = KDB(),
    U = KU();
  const node = () => D.storage_nodes.find(n => dns(n.hostname) === rec.target_name);
  const dev = () => D.devices.find(d => dns(d.serial_number) === rec.target_name);
  const clus = () => D.clusters.find(c => dns(c.name) === rec.target_name);
  const pool = () => D.pools.find(p => dns(p.pool_name) === rec.target_name);
  const a = rec.action;
  if (rec.kind === "StorageClusterOps") {
    const c = clus();
    if (!c) return;
    if (a === "Suspend") c.status = "suspended";
    if (a === "Activate") c.status = "in_activation";
    if (a === "Expand") {
      // the new node joins in_creation and its devices come up as new; the
      // rebalance phase then moves existing data onto it
      const h = rec.payload && rec.payload.host_id ? D.hosts.find(x => x.uuid === rec.payload.host_id) : D.hosts.find(x => x.cluster_id === c.uuid && x.storage_node_ids.length < 2);
      if (h && window.SB_ADD_NODE) {
        const n = window.SB_ADD_NODE(c, h);
        if (n) {
          n.status = "in_creation";
          if (rec.payload && rec.payload.failure_domain) n.failure_domain = rec.payload.failure_domain;
          // the tracker hangs off the node, so the op has to point at it
          rec.target_kind = "StorageNode";
          rec.target_name = dns(n.hostname);
          syncNodeOp(rec);
          D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
            d.status = "new";
          });
        }
      }
    }
  }
  if (rec.kind === "StorageNodeOps") {
    const n = node();
    if (!n) return;
    if (a === "Shutdown") {
      n.status = "in_shutdown";
      n.maintenance = true;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        if (d.status !== "removed") d.status = "unavailable";
      });
    }
    if (a === "Restart") {
      n.status = "in_restart";
      n.maintenance = false;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        if (d.status === "unavailable") d.status = "online";
      });
    }
    if (a === "Remove") {
      n.status = "in_removal";
      // every volume whose primary sits here is moved off first
      const hosted = D.lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
      const others = D.storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
      hosted.forEach((v, i) => {
        const t = others[i % Math.max(1, others.length)];
        if (!t) return;
        v.nodes = Object.assign({}, v.nodes, {
          primary: {
            uuid: t.uuid,
            hostname: t.hostname
          }
        });
      });
      syncNodeOp(rec);
      if (n.op) n.op.volumes_moved = hosted.length;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        d.status = "unavailable";
      });
    }
    if (a === "Migrate") {
      const tgt = rec.payload && rec.payload.host_id ? D.hosts.find(x => x.uuid === rec.payload.host_id) : null;
      n.status = "in_migration";
      syncNodeOp(rec);
      if (n.op) {
        n.op.source_hostname = n.hostname;
        n.op.target_hostname = tgt ? tgt.hostname : null;
        n.op.target_host_id = tgt ? tgt.uuid : null;
      }
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        d.status = "unavailable";
      });
    }
  }
  if (rec.kind === "StorageDeviceOps") {
    const d = dev();
    if (!d) return;
    if (a === "Restart") {
      d.status = "in_restart";
    }
    if (a === "Remove") {
      d.status = "in_removal";
    }
    if (a === "Fail") {
      d.status = "in_failure";
    }
    if (a === "HealthCheck" && D.runHealthCheck) D.runHealthCheck(d.uuid);
  }
  if (rec.kind === "StoragePoolOps") {
    const p = pool();
    if (!p) return;
    if (a === "Enable") p.enabled = true;
    if (a === "Disable") p.enabled = false;
  }
  U.rollup();
}

// How long each step of an operation takes. Long enough that an operator can
// watch the state machine walk through its phases.
const STEP_MS = 5000;

// Operations advance on their own and land in a terminal phase.
function tickOps() {
  (KDB().ops || []).forEach(o => {
    if (["Succeeded", "Failed", "Aborted"].includes(o.phase)) return;
    const age = Date.now() - o.started_ms;
    const idx = Math.min(o.steps.length - 1, Math.floor(age / STEP_MS));
    const next = o.steps[idx];
    if (next !== o.step) {
      o.step = next;
      o.events.push({
        reason: "StepChanged",
        message: `Entered ${next}`,
        at: KU().ago(0)
      });
      syncNodeOp(o);
    }
    if (o.abort) {
      const canAbort = (OPS_ABORTABLE[o.action] || []).includes(o.step);
      o.phase = "Aborted";
      o.message = canAbort ? `Unwound from ${o.step}` : `Aborted at ${o.step}`;
      o.events.push({
        reason: "OperationAborted",
        message: o.message,
        at: KU().ago(0)
      });
      finishEffect(o);
      return;
    }
    if (age > o.steps.length * STEP_MS) {
      o.phase = "Succeeded";
      o.message = `${o.action} completed`;
      o.completed_at = KU().ago(0);
      o.events.push({
        reason: "OperationSucceeded",
        message: o.message,
        at: KU().ago(0)
      });
      finishEffect(o);
    }
  });
}
function finishEffect(o) {
  const D = KDB(),
    U = KU();
  if (o.phase === "Succeeded" && o.target_kind === "StorageDevice") {
    const d = D.devices.find(x => dns(x.serial_number) === o.target_name);
    if (d) {
      if (o.action === "Restart") {
        d.status = "online";
        d.health_check = d.health_check || "good";
      }
      // Removal is reversible: the device is out of service but still known to
      // the cluster and to its node, and can be added back with a restart.
      if (o.action === "Remove") {
        d.status = "removed";
        d.removed_at = U.ago(0);
      }
      // Failing is not: the device is permanently excluded and its chunks have
      // been rebuilt onto the remaining devices, so fault tolerance is restored
      // without it. It never comes back, and its host device is released.
      if (o.action === "Fail") {
        d.status = "failed";
        d.failed_at = U.ago(0);
        d.health_check = null;
        d.size_util = 0;
        const h = D.hosts.find(x => x.uuid === d.host_id);
        if (h) h.devices.forEach(hd => {
          if (hd.serial_number === d.serial_number) hd.assigned_node_id = null;
        });
      }
    }
  }
  if (o.phase === "Succeeded" && o.target_kind === "StorageNode") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === o.target_name);
    if (n && o.action === "Restart") n.status = "online";
    if (n && o.action === "Shutdown") n.status = "offline";
    if (n && o.action === "Expand") {
      n.status = "online";
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        if (d.status === "new") d.status = "online";
      });
    }
    if (n && o.action === "Migrate") {
      const tgtId = n.op && n.op.target_host_id;
      const old = D.hosts.find(x => x.uuid === n.host_id);
      const tgt = tgtId ? D.hosts.find(x => x.uuid === tgtId) : null;
      if (old) {
        old.devices.forEach(d => {
          if (d.assigned_node_id === n.uuid) d.assigned_node_id = null;
        });
        old.storage_node_ids = old.storage_node_ids.filter(x => x !== n.uuid);
      }
      if (tgt) {
        n.host_id = tgt.uuid;
        n.mgmt_ip = tgt.mgmt_ip;
        if (!tgt.storage_node_ids.includes(n.uuid)) tgt.storage_node_ids.push(n.uuid);
        tgt.devices.filter(d => !d.assigned_node_id).forEach(d => d.assigned_node_id = n.uuid);
        D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
          d.host_id = tgt.uuid;
        });
      }
      n.status = "online";
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => {
        if (d.status === "unavailable") d.status = "online";
      });
    }
    if (n) n.op = null;
    if (n && o.action === "Remove") {
      const h = D.hosts.find(x => x.uuid === n.host_id);
      if (h) h.devices.forEach(d => {
        if (d.assigned_node_id === n.uuid) d.assigned_node_id = null;
      });
      D.devices = D.devices.filter(d => d.node_id !== n.uuid);
      D.storage_nodes = D.storage_nodes.filter(x => x.uuid !== n.uuid);
    }
  }
  if (o.phase === "Succeeded" && o.kind === "StorageClusterOps" && o.action === "Activate") {
    const c = D.clusters.find(x => dns(x.name) === o.target_name);
    if (c) c.status = "online";
  }
  U.rollup();
}
const opsToK8s = o => ({
  apiVersion: KAPI,
  kind: o.kind,
  metadata: {
    name: o.name,
    namespace: KNS(),
    uid: o.uuid,
    generation: 1,
    creationTimestamp: o.created_at,
    labels: {
      [KGROUP + "/target"]: o.target_name || "",
      [KGROUP + "/action"]: o.action
    }
  },
  spec: Object.assign({
    action: o.action,
    abort: !!o.abort
  }, o.target_name ? {
    targetRef: {
      name: o.target_name
    }
  } : {}, o.payload ? {
    [o.action.charAt(0).toLowerCase() + o.action.slice(1)]: o.payload
  } : {}),
  status: {
    phase: o.phase,
    observedGeneration: 1,
    clusterId: o.cluster_id || null,
    // the kind the operation actually landed on: an Expand is filed against the
    // cluster but ends up owning the node it created
    targetKind: o.target_kind || null,
    step: {
      state: o.step,
      steps: o.steps,
      index: Math.max(0, o.steps.indexOf(o.step)),
      labels: o.steps.map(s => OPS_STEP_LABEL[s] || s),
      label: OPS_STEP_LABEL[o.step] || o.step,
      abortable: (OPS_ABORTABLE[o.action] || []).includes(o.step)
    },
    message: o.message,
    startedAt: o.created_at,
    completedAt: o.completed_at || null,
    events: o.events
  }
});

// A little operation history at boot, so the Operations panel is not empty
// before the operator has done anything: two finished machines and one still
// walking its phases.
(function seedOps() {
  const D = KDB(),
    U = KU();
  const done = (kind, action, targetKind, targetName, clusterId, hoursAgo, phase) => {
    const steps = OPS_STEPS[kind === "StorageDeviceOps" && action === "Remove" ? "RemoveDevice" : action] || OPS_STEPS.Default;
    D.ops.push({
      uuid: U.uuid(),
      kind,
      name: dns(`${targetName}-${action.toLowerCase()}-${U.hex(4)}`),
      action,
      target_kind: targetKind,
      target_name: targetName,
      cluster_id: clusterId,
      payload: null,
      abort: phase === "Aborted",
      phase,
      step: phase === "Succeeded" ? steps[steps.length - 1] : steps[0],
      steps,
      started_ms: Date.now() - hoursAgo * 3600e3 - steps.length * STEP_MS,
      created_at: U.ago(hoursAgo),
      completed_at: U.ago(hoursAgo - .05),
      message: phase === "Succeeded" ? `${action} completed` : `Unwound from ${steps[0]}`,
      events: [{
        reason: "OperationStarted",
        message: `${action} started`,
        at: U.ago(hoursAgo)
      }, {
        reason: phase === "Succeeded" ? "OperationSucceeded" : "OperationAborted",
        message: phase === "Succeeded" ? `${action} completed` : `Unwound from ${steps[0]}`,
        at: U.ago(hoursAgo - .05)
      }]
    });
  };
  D.clusters.filter(c => c.status === "online" || c.status === "degraded").slice(0, 3).forEach((c, i) => {
    const ns = D.storage_nodes.filter(n => n.cluster_id === c.uuid);
    if (!ns.length) return;
    done("StorageNodeOps", "Restart", "StorageNode", dns(ns[0].hostname), c.uuid, 6 + i * 3, "Succeeded");
    if (ns[1]) done("StorageNodeOps", "Shutdown", "StorageNode", dns(ns[1].hostname), c.uuid, 30 + i * 5, "Succeeded");
    if (i === 0) done("StorageClusterOps", "Expand", "StorageCluster", dns(c.name), c.uuid, 52, "Succeeded");
    if (i === 1 && ns[2]) done("StorageNodeOps", "Migrate", "StorageNode", dns(ns[2].hostname), c.uuid, 12, "Aborted");
  });
  // and one machine still in flight: an expansion of the first eligible cluster
  const c0 = D.clusters.find(c => (c.status === "online" || c.status === "degraded") && D.hosts.some(h => h.cluster_id === c.uuid && h.storage_node_ids.length < 2 && h.prepared_at));
  if (c0) {
    const h = D.hosts.find(h2 => h2.cluster_id === c0.uuid && h2.storage_node_ids.length < 2 && h2.prepared_at);
    const r = runOps("StorageClusterOps", {
      apiVersion: KAPI,
      kind: "StorageClusterOps",
      metadata: {
        name: dns(`${c0.name}-expand-boot`),
        namespace: KNS()
      },
      spec: {
        action: "Expand",
        targetRef: {
          name: dns(c0.name)
        },
        expand: {
          host_id: h.uuid,
          failure_domain: h.rack_id || h.zone || null
        }
      }
    });
    // pretend it started a moment ago, so it is mid-machine rather than brand new
    if (r && r.rec) r.rec.started_ms = Date.now() - STEP_MS * 0.6;
  }
})();
Object.assign(window, {
  TO_K8S,
  COLL,
  opsToK8s,
  runOps,
  tickOps,
  dnsName: dns,
  syncNodeOp,
  OPS_STEP_LABEL,
  STEP_MS,
  OPS_ACTIONS,
  OPS_STEPS,
  OPS_ABORTABLE,
  pascalCase: pascal
});
})();
// ---- mock-k8s-server.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK API SERVER — routes the three real surfaces.
//   SB_CONFIG.k8sBase       Kubernetes API   (CRDs + core objects + pod logs)
//   SB_CONFIG.operatorBase  operator API     (releases, SSE, /proposed/*)
//   SB_CONFIG.helmBase      Helm             (release state)
// Everything else falls through to the network.
// ---------------------------------------------------------------------------
const SBM = window.SB_MOCK = Object.assign({
  latency: [90, 260],
  failRate: 0,
  forceEmpty: false,
  offline: false,
  failNext: false,
  requests: 0
}, window.SB_MOCK);
const jsonRes = (body, status) => new Response(JSON.stringify(body), {
  status: status === undefined ? 200 : status,
  headers: {
    "Content-Type": "application/json"
  }
});
// A Kubernetes failure is a Status object, not a bare error string.
const status4 = (code, reason, message) => jsonRes({
  kind: "Status",
  apiVersion: "v1",
  status: "Failure",
  code,
  reason,
  message
}, code);
const listOf = (kind, items) => ({
  apiVersion: kind.startsWith("Storage") || kind.startsWith("Control") || kind.startsWith("Simplyblock") || kind.startsWith("Cluster") || kind.startsWith("Operator") ? window.API_GROUP : "v1",
  kind: kind + "List",
  metadata: {
    resourceVersion: String(Date.now() % 100000)
  },
  items
});
const DB2 = () => window.SB_DB;
const U2 = () => window.SB_UTIL;
const PLURALS = {};
Object.entries(window.RESOURCES).forEach(([k, r]) => {
  PLURALS[r.plural] = k;
});

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
    items = items.filter(o => want.every(([k, v]) => ((o.metadata.labels || {})[k] || "") === v));
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
const NS_KINDS_M = {
  Deployment: 1,
  StatefulSet: 1,
  Service: 1,
  ConfigMap: 1,
  Secret: 1,
  Ingress: 1,
  VirtualMachine: 1
};
const NS_CACHE = {};
function nsResources(ns) {
  if (!ns) return [];
  if (NS_CACHE[ns]) return NS_CACHE[ns];
  const U = U2();
  const pvcs = (DB2().pvcs || []).filter(p => p.namespace === ns);
  const apps = [...new Set(pvcs.map(p => (p.pvc_name || "data").replace(/^(data|pvc|vol)-/, "").split("-")[0]))].slice(0, 3);
  if (!apps.length) apps.push(ns.split("-")[0]);
  const mk = (kind, name, app, extra) => Object.assign({
    apiVersion: kind === "Deployment" || kind === "StatefulSet" ? "apps/v1" : kind === "Ingress" ? "networking.k8s.io/v1" : kind === "VirtualMachine" ? "kubevirt.io/v1" : "v1",
    kind,
    metadata: {
      name,
      namespace: ns,
      uid: U.uuid(),
      creationTimestamp: U.ago(U.int(100, 3000)),
      labels: Object.assign({
        "app.kubernetes.io/name": app,
        "app.kubernetes.io/instance": app + "-" + ns
      }, extra || {})
    }
  }, {});
  const out = [];
  apps.forEach((app, i) => {
    const vm = /vm|win|desktop/.test(app);
    if (vm) out.push(mk("VirtualMachine", app, app, {
      "kubevirt.io/domain": app
    }));else {
      out.push(mk("StatefulSet", app, app, {
        "app.kubernetes.io/component": "db"
      }));
      if (i === 0) out.push(mk("Deployment", app + "-api", app, {
        "app.kubernetes.io/component": "api"
      }), mk("Deployment", app + "-worker", app, {
        "app.kubernetes.io/component": "worker"
      }));
      out.push(mk("Ingress", app, app));
    }
    out.push(mk("Service", app, app), mk("ConfigMap", app + "-config", app), mk("Secret", app + "-credentials", app));
  });
  out.push(mk("ConfigMap", "kube-root-ca.crt", ns, {}), mk("Secret", "default-token", ns, {}));
  return NS_CACHE[ns] = out;
}

// ---- core object mappers ---------------------------------------------------
const d2 = s => window.dnsName(s);
const cName = id => d2((DB2().clusters.find(c => c.uuid === id) || {}).name);
const pvToK8s = v => ({
  apiVersion: "v1",
  kind: "PersistentVolume",
  metadata: {
    name: d2(v.lvol_name),
    uid: v.uuid,
    creationTimestamp: v.created_at,
    labels: {
      [window.GROUP + "/cluster"]: cName(v.cluster_id),
      [window.GROUP + "/pool"]: d2(v.pool_name)
    },
    annotations: Object.assign({
      "pv.kubernetes.io/provisioned-by": "csi.simplyblock.io"
    }, v.bucket ? {
      [window.GROUP + "/bucket"]: v.bucket.name
    } : {})
  },
  spec: {
    capacity: {
      storage: Math.round(v.size_prov / 1e9) + "Gi"
    },
    accessModes: [v.pvc && v.pvc.access_mode ? v.pvc.access_mode : "ReadWriteOnce"],
    persistentVolumeReclaimPolicy: "Delete",
    storageClassName: v.pvc ? d2(v.pvc.storage_class) : null,
    claimRef: v.pvc ? {
      kind: "PersistentVolumeClaim",
      namespace: v.pvc.namespace,
      name: v.pvc.name
    } : null,
    csi: {
      driver: "csi.simplyblock.io",
      volumeHandle: v.uuid,
      fsType: v.pvc && v.pvc.filesystem || "ext4",
      volumeAttributes: {
        // everything simplyblock-specific about a volume travels here
        pool_name: v.pool_name,
        cluster_id: v.cluster_id,
        encryption: String(!!v.crypto_enabled),
        compression: String(!!v.compression_dedup_enabled),
        nqn: v.nqn,
        qos_rw_iops: String((v.qos || {}).rw_ios_per_sec || 0)
      }
    }
  },
  status: {
    phase: v.status === "online" ? "Bound" : "Available",
    // status the CSI driver reports back from the control plane
    simplyblock: {
      volumeId: v.uuid,
      used: String(v.size_util),
      logicalUsed: String(v.logical_used || v.size_util),
      nodes: v.nodes || {},
      snapshots: v.snapshots_count || 0,
      backupVersions: v.backup_versions_count || 0,
      health: v.status === "online" ? "Healthy" : "Unavailable"
    }
  }
});
const pvcToK8s = p => ({
  apiVersion: "v1",
  kind: "PersistentVolumeClaim",
  metadata: {
    name: p.pvc_name,
    namespace: p.namespace,
    uid: p.uuid,
    creationTimestamp: p.created_at,
    labels: p.labels || {},
    annotations: p.annotations || {}
  },
  spec: {
    accessModes: [p.access_mode],
    volumeMode: p.volume_mode,
    storageClassName: d2(p.storage_class),
    resources: {
      requests: {
        storage: Math.round(p.requested_bytes / 1e9) + "Gi"
      }
    },
    volumeName: p.lvol_id ? d2((DB2().lvols.find(v => v.uuid === p.lvol_id) || {}).lvol_name) : null
  },
  status: {
    phase: p.status,
    capacity: p.actual_bytes ? {
      storage: Math.round(p.actual_bytes / 1e9) + "Gi"
    } : {},
    accessModes: [p.access_mode]
  }
});
const scToK8s = s => ({
  apiVersion: "storage.k8s.io/v1",
  kind: "StorageClass",
  metadata: {
    name: d2(s.name),
    uid: s.uuid,
    creationTimestamp: s.created_at,
    labels: {
      [window.GROUP + "/pool"]: d2(s.pool_name)
    },
    annotations: Object.assign({
      [window.GROUP + "/storage-pool"]: s.storage_pool_ref || ""
    }, s.is_default ? {
      "storageclass.kubernetes.io/is-default-class": "true"
    } : {})
  },
  provisioner: s.provisioner,
  parameters: s.parameters || {},
  reclaimPolicy: s.reclaim_policy,
  volumeBindingMode: s.volume_binding_mode,
  allowVolumeExpansion: s.allow_volume_expansion !== false
});
const nodeToK8s = h => ({
  apiVersion: "v1",
  kind: "Node",
  metadata: {
    name: h.hostname,
    uid: h.uuid,
    creationTimestamp: h.prepared_at,
    labels: Object.assign({
      "kubernetes.io/os": "linux"
    }, h.k8s_labels || {}, h.labels || {}),
    annotations: h.migration_taint ? {
      [window.GROUP + "/migration-target"]: "true"
    } : {}
  },
  spec: {
    taints: h.migration_taint ? [{
      key: window.GROUP + "/migration-target",
      value: "true",
      effect: "NoSchedule"
    }] : []
  },
  status: {
    conditions: [{
      type: "Ready",
      status: h.status === "unreachable" ? "Unknown" : "True",
      reason: "KubeletReady",
      lastTransitionTime: h.prepared_at
    }],
    capacity: {
      cpu: String(h.vcpu_count),
      memory: Math.round(h.memory_total / 1e9) + "Gi",
      "hugepages-2Mi": Math.round((h.hugepages_reserved || 0) / 1e9) + "Gi"
    },
    nodeInfo: {
      kubeletVersion: h.kubelet_version || null,
      operatingSystem: "linux"
    },
    addresses: [{
      type: "InternalIP",
      address: h.mgmt_ip
    }, {
      type: "Hostname",
      address: h.hostname
    }]
  }
});
const podToK8s = c => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: {
    name: c.name,
    namespace: KNSx(),
    uid: c.name,
    labels: {
      "app.kubernetes.io/component": c.group.replace(/\s+/g, "-"),
      "app.kubernetes.io/part-of": "simplyblock",
      [window.GROUP + "/cluster"]: cName(c.cluster_id)
    }
  },
  spec: {
    containers: [{
      name: c.name,
      image: c.image,
      resources: {
        requests: {
          cpu: String(c.cpu_cores_alloc),
          memory: Math.round(c.mem_limit / 1e9) + "Gi"
        },
        limits: {
          cpu: String(c.cpu_cores_alloc),
          memory: Math.round(c.mem_limit / 1e9) + "Gi"
        }
      }
    }]
  },
  status: {
    phase: c.state === "running" ? "Running" : "Succeeded",
    containerStatuses: [{
      name: c.name,
      ready: c.state === "running",
      restartCount: c.restarts
    }],
    // usage the metrics API would serve; carried here so one read answers the panel
    usage: {
      cpu: c.cpu_pct / 100,
      memory: String(c.mem_used),
      disk: String(c.disk_used),
      diskLimit: String(c.disk_limit)
    }
  }
});
const KNSx = () => window.SB_CONFIG.namespace || "simplyblock";
const cpToK8s = c => ({
  apiVersion: window.API_GROUP,
  kind: "ControlPlane",
  metadata: {
    name: d2(c.name) + "-cp",
    namespace: KNSx(),
    uid: c.uuid + "-cp",
    labels: {
      [window.GROUP + "/cluster"]: d2(c.name)
    }
  },
  spec: {
    clusterRef: {
      name: d2(c.name)
    },
    version: c.cluster_version
  },
  status: {
    phase: c.status === "suspended" ? "Suspended" : "Ready",
    observedGeneration: 1,
    activeOpsRef: null,
    endpoint: c.mgmt_endpoint,
    stateDatabase: {
      backend: "FoundationDB",
      backups: (DB2().fdb_backups || []).filter(b => b.cluster_id === c.uuid).length
    },
    conditions: [window.__cond ? window.__cond() : {
      type: "Ready",
      status: "True",
      reason: "Ready"
    }]
  }
});
const driverToK8s = k => ({
  apiVersion: window.API_GROUP,
  kind: "SimplyblockDriver",
  metadata: {
    name: d2(k.name) + "-driver",
    namespace: KNSx(),
    uid: k.uuid + "-drv",
    labels: {
      [window.GROUP + "/kubernetes-cluster"]: d2(k.name)
    }
  },
  spec: {
    version: k.csi_version,
    namespace: k.operator_namespace
  },
  status: {
    phase: k.csi_status === "online" ? "Ready" : "Degraded",
    observedGeneration: 1,
    driverVersion: k.csi_version,
    operatorVersion: k.csi_version,
    controlPlaneVersions: [...new Set((k.storage_cluster_ids || []).map(id => (DB2().clusters.find(c => c.uuid === id) || {}).cluster_version).filter(Boolean))],
    conditions: [{
      type: "Ready",
      status: k.csi_status === "online" ? "True" : "False",
      reason: k.csi_status === "online" ? "Ready" : "DriverDegraded"
    }]
  }
});
const cdcToK8s = c => ({
  apiVersion: window.API_GROUP,
  kind: "ClusterDeploymentConfig",
  metadata: {
    name: c.name,
    namespace: KNSx(),
    uid: c.uuid,
    creationTimestamp: c.created_at
  },
  spec: c.spec,
  status: c.status || {
    phase: "Draft",
    observedGeneration: 1
  }
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
  const method = (init && init.method || "GET").toUpperCase();
  const base = isK8s ? cfg.k8sBase : isOp ? cfg.operatorBase : cfg.helmBase;
  const [rawPath, rawQuery] = url.slice(base.length).split("?");
  const params = new URLSearchParams(rawQuery || "");
  let body = {};
  try {
    if (init && init.body) body = JSON.parse(init.body);
  } catch (e) {}
  SBM.requests++;
  await new Promise(r => setTimeout(r, SBM.latency[0] + Math.random() * (SBM.latency[1] - SBM.latency[0])));
  if (SBM.offline) return status4(503, "ServiceUnavailable", "Cannot reach the Kubernetes API");
  if (SBM.failNext) {
    SBM.failNext = false;
    return status4(503, "ServiceUnavailable", "Injected failure");
  }
  if (SBM.failRate && Math.random() < SBM.failRate) return status4(503, "ServiceUnavailable", "API server unavailable");
  if (window.__tickErr) return status4(500, "InternalError", "fixture tick failed: " + window.__tickErr);
  if (isHelm) return jsonRes(helmMock(rawPath));
  if (isOp) return operatorMock(rawPath, method, body, params);

  // /apis/<group>/<version>/namespaces/<ns>/<plural>[/<name>[/<sub>]]
  // /api/v1[/namespaces/<ns>]/<plural>[/<name>[/<sub>]]
  const parts = rawPath.split("/").filter(Boolean);
  const nsIdx = parts.indexOf("namespaces");
  const tail = nsIdx >= 0 ? parts.slice(nsIdx + 2) : parts.slice(parts[0] === "api" ? 2 : 3);
  const plural = tail[0],
    name = tail[1],
    sub = tail[2];
  const kind = PLURALS[plural];
  if (!kind) return status4(404, "NotFound", `the server could not find the resource: ${plural}`);
  if (sub === "log") {
    const lines = DB2().container_logs[Object.keys(DB2().container_logs).find(k => k.endsWith("/" + name)) || ""] || [];
    return new Response(lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n"), {
      status: 200,
      headers: {
        "Content-Type": "text/plain"
      }
    });
  }
  if (method === "GET") {
    if (nsIdx >= 0) params.set("__ns", parts[nsIdx + 1]);
    if (kind === "PersistentVolumeClaim" && nsIdx >= 0) {
      const ns = parts[nsIdx + 1];
      const its = readList(kind, params).filter(o => o.metadata.namespace === ns);
      return jsonRes(listOf(kind, its));
    }
    const items = readList(kind, params);
    if (!name) return jsonRes(listOf(kind, items));
    const hit = items.find(o => o.metadata.name === name);
    return hit ? jsonRes(hit) : status4(404, "NotFound", `${plural} "${name}" not found`);
  }
  if (method === "POST") {
    // kinds with their own create rules (replication) rather than the Ops shape
    const mk = (window.CRD_CREATE || {})[kind];
    if (mk) {
      try {
        const r = mk(body);
        if (r.err) return status4(r.reason === "Conflict" ? 409 : r.reason === "NotFound" ? 404 : r.reason === "AlreadyExists" ? 409 : 422, r.reason, r.err);
        return jsonRes(r.obj, 201);
      } catch (e) {
        return status4(500, "InternalError", e.message);
      }
    }
    if (!window.RESOURCES[kind].ops && kind !== "OperatorOps") return status4(405, "MethodNotAllowed", `${kind} is not created through this console`);
    try {
      const r = window.runOps(kind, body);
      if (r.err) return status4(r.reason === "Conflict" ? 409 : 422, r.reason, r.err);
      return jsonRes(window.opsToK8s(r.rec), 201);
    } catch (e) {
      return status4(500, "InternalError", e.message);
    }
  }
  if (method === "PATCH") {
    // The only membership control replication has: the PVC annotation. Writing
    // it attaches, clearing it detaches; the operator owns the slot either way.
    if (kind === "PersistentVolumeClaim") {
      const anns = (body.metadata || {}).annotations || {};
      const key = window.SB_REPL.REPL_ANN;
      if (!Object.prototype.hasOwnProperty.call(anns, key)) return status4(422, "Invalid", `this console only patches the ${key} annotation on a PVC`);
      const r = window.SB_REPL.setPvcPolicy(name, anns[key]);
      if (r.err) return status4(r.reason === "Conflict" ? 409 : r.reason === "NotFound" ? 404 : 422, r.reason, r.err);
      return jsonRes(r.obj);
    }
    const rec = (DB2().ops || []).find(o => o.kind === kind && o.name === name);
    if (!rec) return status4(404, "NotFound", `${plural} "${name}" not found`);
    // spec.abort is the one mutable field on an Ops spec
    const keys = Object.keys(body.spec || {});
    if (keys.some(k => k !== "abort")) return status4(422, "Invalid", `spec is immutable except for spec.abort; refused: ${keys.filter(k => k !== "abort").join(", ")}`);
    if (["Succeeded", "Failed", "Aborted"].includes(rec.phase)) return status4(409, "Conflict", `operation is already ${rec.phase}`);
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
      return jsonRes({
        kind: "Status",
        status: "Success"
      });
    }
    const abortable = (window.OPS_ABORTABLE[rec.action] || []).includes(rec.step);
    if (!abortable) return status4(403, "Forbidden", `admission webhook denied the request: ${rec.action} cannot be aborted from step ${rec.step}; deleting now would strand ${rec.target_kind}/${rec.target_name}`);
    rec.abort = true;
    return jsonRes({
      kind: "Status",
      status: "Success",
      message: `abort requested; ${rec.action} will unwind from ${rec.step}`
    });
  }
  return status4(405, "MethodNotAllowed", method + " not supported");
};

// ---- operator API ----------------------------------------------------------
// /releases is the compatibility matrix. /proposed/* are the collections the UI
// needs that have no CRD in v1alpha1 yet — replication, DR, consistency groups,
// migrations, buckets, zones. They are namespaced under /proposed on purpose.
function operatorMock(path, method, body, params) {
  if (path === "/healthz") return jsonRes({
    status: "ok"
  });
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
    const scope0 = params.get("scope"),
      scopeId0 = params.get("scopeId");
    const route = (scope0 && scopeId0 ? `/${scope0}/${scopeId0}${path.slice(9)}` : path.slice(9)).replace(/\/$/, "");
    for (const [mm, re, h] of window.SB_CP_ROUTES.MUT_ROUTES) {
      if (mm !== method) continue;
      const mt = route.match(re);
      if (!mt) continue;
      const r = h(mt, body);
      if (r.__404) return status4(404, "NotFound", "Resource not found");
      if (r.__err) return status4(409, "Conflict", r.__err);
      if (r.__cap) return status4(501, "NotImplemented", r.__cap);
      return jsonRes(Object.assign({
        proposed: true
      }, r.results !== undefined ? r : {
        results: [r]
      }));
    }
  }
  const coll = PROPOSED_COLL[m[1]];
  if (!coll) return status4(404, "NotFound", "no proposed collection " + m[1]);
  let items = DB2()[coll] || [];

  // /proposed/<coll>/<id>[/<verb>]
  if (m[2]) {
    if (m[3] || method !== "GET") return jsonRes({
      status: "ok",
      accepted: true
    });
    const hit = items.find(x => x.uuid === m[2] || x.id === m[2]);
    return hit ? jsonRes({
      results: [hit],
      proposed: true
    }) : status4(404, "NotFound", `${m[1]}/${m[2]} not found`);
  }
  if (method !== "GET") return jsonRes({
    status: "ok",
    accepted: true
  });

  // ?scope=<parent collection>&scopeId=<uuid>
  const scope = params.get("scope"),
    scopeId = params.get("scopeId");
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
      const parent = (DB2()[pColl] || []).find(x => x.uuid === scopeId) || null;
      const singular = m[1].replace(/-/g, "_").replace(/s$/, "");
      const idsKey = parent && [IDS_ALIAS[m[1]], `${singular}_ids`, `${singular}s_ids`, `${m[1].replace(/-/g, "_")}_ids`].filter(Boolean).find(k => Array.isArray(parent[k]));
      items = parent && idsKey ? items.filter(x => (parent[idsKey] || []).includes(x.uuid)) : [];
    }
  }
  return jsonRes({
    results: SBM.forceEmpty ? [] : items,
    proposed: true
  });
}

// The kinds v1alpha1 does not model. Served here until they are CRDs.
const PROPOSED_COLL = {
  lvols: "lvols",
  snapshots: "snapshots",
  backups: "backups",
  "backup-policies": "backup_policies",
  "consistency-groups": "consistency_groups",
  "cg-snapshots": "cg_snapshots",
  "replication-policies": "dr_policies",
  "cluster-pairs": "cluster_pairs",
  pairs: "cluster_pairs",
  migrations: "migrations",
  buckets: "buckets",
  zones: "zones",
  "dr-clusters": "dr_clusters",
  "protected-apps": "protected_apps",
  alerts: "alerts",
  tasks: "tasks",
  logs: "logs",
  "migration-paths": "migration_paths",
  "app-groups": "app_groups",
  hosts: "hosts",
  "storage-classes": "storage_classes",
  pvcs: "pvcs",
  "kubernetes-clusters": "k8s_clusters",
  "k8s-clusters": "k8s_clusters",
  "fdb-backups": "fdb_backups",
  containers: "containers",
  "storage-clusters": "clusters",
  clusters: "clusters",
  pools: "pools",
  "storage-nodes": "storage_nodes",
  devices: "devices"
};
const CORE_COLL = {
  clusters: "clusters",
  pools: "pools",
  "storage-nodes": "storage_nodes",
  devices: "devices"
};
// where the fixture's membership array is not the plural of the collection name
const IDS_ALIAS = {
  "kubernetes-clusters": "k8s_cluster_ids",
  "k8s-clusters": "k8s_cluster_ids",
  "storage-clusters": "storage_cluster_ids",
  clusters: "cluster_ids",
  "storage-nodes": "storage_node_ids",
  "storage-classes": "storage_class_ids"
};
const SCOPE_FIELD_M = {
  clusters: "cluster_id",
  pools: "pool_id",
  lvols: "lvol_id",
  "storage-nodes": "node_id",
  hosts: "host_id",
  devices: "device_id",
  "backup-policies": "policy_id",
  "replication-policies": "policy_id",
  tasks: "parent_id",
  pairs: "pair_id",
  "kubernetes-clusters": "k8s_cluster_id",
  "k8s-clusters": "k8s_cluster_id",
  zones: "zone_id",
  "storage-classes": "storage_class_id",
  "consistency-groups": "cg_id",
  "dr-clusters": "dr_cluster_id",
  snapshots: "snapshot_id",
  "migration-paths": "path_id",
  "app-groups": "group_id"
};

// Mirrors install.simplyblock.io/releases.yaml
const RELEASES = {
  schema: 1,
  components: [{
    name: "controlplane",
    releases: [{
      version: "26.2.1",
      released: "2026-09-01"
    }, {
      version: "26.2",
      released: "2026-08-08"
    }, {
      version: "26.1"
    }, {
      version: "0.2.0"
    }, {
      version: "0.1.0"
    }]
  }, {
    name: "csi-driver",
    releases: [{
      version: "26.2.1",
      released: "2026-07-02",
      compatible: {
        controlplane: ["26.2.x", "26.1.x"]
      }
    }, {
      version: "26.2.0",
      released: "2026-06-01",
      compatible: {
        controlplane: ["26.2.x", "26.1.x"]
      }
    }, {
      version: "26.1.2",
      released: "2026-06-01",
      compatible: {
        controlplane: ["26.1.x", "0.2.0"]
      }
    }, {
      version: "26.1.0",
      released: "2026-01-01",
      compatible: {
        controlplane: ["26.1.x", "0.2.0"]
      }
    }]
  }, {
    name: "operator",
    releases: [{
      version: "26.2.1",
      released: "2026-07-02",
      compatible: {
        controlplane: ["26.2.x", "26.1.x"]
      }
    }, {
      version: "26.2.0",
      released: "2026-06-01",
      compatible: {
        controlplane: ["26.2.x", "26.1.x"]
      }
    }, {
      version: "26.1.2",
      released: "2026-06-01",
      compatible: {
        controlplane: ["26.1.x", "0.2.0"]
      }
    }]
  }]
};
function helmMock(path) {
  const rel = {
    name: "simplyblock",
    namespace: KNSx(),
    revision: 7,
    chart: "simplyblock-operator-26.2.1",
    appVersion: "26.2.1",
    status: "deployed",
    updated: U2().ago(72)
  };
  if (path === "/releases") return {
    releases: [rel]
  };
  if (path.endsWith("/values")) return {
    values: {
      operator: {
        logLevel: "info",
        replicas: 1
      },
      csi: {
        enableSnapshots: true,
        enableExpansion: true
      },
      controlPlane: {
        version: "26.2.1"
      }
    }
  };
  return rel;
}
if (!window.__fixtureClock) window.__fixtureClock = setInterval(() => {
  try {
    window.SB_JITTER();
    window.tickOps();
    window.__tickErr = null;
  } catch (e) {
    window.__tickErr = e.message;
  }
}, 1000);
Object.assign(window, {
  RELEASES,
  readList
});
})();
// ---- mock-extras.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK EXTRAS — fixtures + routes for the data that is NOT part of control
// plane API v2 read models: tasks & cluster event log (v2), and the
// agent/Prometheus sources (container allocation, container logs, FoundationDB
// backups, SPDK logs & thread utilization, SMART output).
// Chains onto the fetch installed by mock-api.jsx.
// ---------------------------------------------------------------------------
const X = window.SB_DB,
  XU = window.SB_UTIL;
const xpick = XU.pick,
  xint = XU.int,
  xuuid = XU.uuid,
  xhex = XU.hex,
  xago = XU.ago;
const XGB = 1e9;
X.tasks = [];
X.logs = [];
X.alerts = [];
X.containers = [];
X.container_logs = {};
X.fdb_backups = [];
X.spdk_threads = {};
X.spdk_logs = {};
X.smart = {};
const TASK_FUNCS = ["node_restart", "port_allow", "balancing_on_restart", "fdb_backup", "device_migration", "new_device_discovery", "device_restart", "snapshot_delete", "lvol_migration", "cluster_upgrade"];
const TASK_STATUS = ["done", "done", "done", "done", "running", "new", "suspended"];
const RESULTS = {
  node_restart: ["Node is restarting", "canceled: node back online", "", "Node restarted"],
  port_allow: ["Port 4440 allowed on node", "Port 4432 allowed on node", ""],
  balancing_on_restart: ["", "running", "Done"],
  fdb_backup: ["Done", "running", ""],
  device_migration: ["Done", "canceled", "running", ""],
  new_device_discovery: ["Done", ""],
  device_restart: ["Done", "canceled: device unavailable", ""],
  snapshot_delete: ["Done", ""],
  lvol_migration: ["Done", "running", ""],
  cluster_upgrade: ["running", ""]
};
X.clusters.forEach(c => {
  const nodes = X.storage_nodes.filter(n => n.cluster_id === c.uuid);
  const devs = X.devices.filter(d => d.cluster_id === c.uuid);

  // ---- tasks + subtasks (edge clusters run no task engine) ----
  // Mirrors `cluster list-tasks`: Task ID · Target ID · Function · Retry · Status · Result · Updated At.
  // Subtasks add Node ID and Distrib, as in `cluster list-subtasks`.
  for (let i = 0; c.capabilities.tasks && i < xint(9, 18); i++) {
    const fn = xpick(TASK_FUNCS);
    const status = xpick(TASK_STATUS);
    const node = nodes.length ? xpick(nodes) : null;
    const master = fn === "balancing_on_restart" || fn === "device_migration" && Math.random() > .5;
    const subCount = master ? xint(4, 36) : 0;
    const maxRetry = fn === "node_restart" ? 11 : fn === "port_allow" ? 8 : 0;
    const t = {
      uuid: xuuid(),
      cluster_id: c.uuid,
      parent_id: null,
      function_name: fn,
      target_id: master ? `Master task for ${subCount} subtasks` : node ? `NodeID:${node.uuid}` : `ClusterID:${c.uuid}`,
      node_id: node ? node.uuid : null,
      distrib: null,
      retry: 0,
      max_retry: maxRetry,
      status,
      result: status === "running" ? xpick(["running", ""]) : xpick(RESULTS[fn] || [""]),
      created_at: xago(xint(1, 400)),
      updated_at: xago(Math.random() * 40),
      canceled: false,
      subtask_total: subCount
    };
    if (t.status === "done" && /^canceled/.test(t.result)) t.canceled = true;
    X.tasks.push(t);
    for (let s = 0; s < subCount; s++) {
      const sstatus = status === "done" ? "done" : xpick(["done", "done", "running", "new"]);
      const sn = nodes.length ? xpick(nodes) : null;
      const sretry = Math.random() > .8 ? xint(1, 3) : 0;
      X.tasks.push({
        uuid: xuuid(),
        cluster_id: c.uuid,
        parent_id: t.uuid,
        function_name: fn === "balancing_on_restart" ? "device_migration" : fn,
        target_id: null,
        node_id: sn ? sn.uuid : null,
        distrib: `distrib_${s + 1}`,
        retry: sretry,
        max_retry: 0,
        status: sstatus,
        result: sstatus === "done" ? sretry ? "canceled" : "Done" : sstatus === "running" ? "running" : "",
        created_at: t.created_at,
        updated_at: xago(Math.random() * 30),
        canceled: sretry > 0,
        subtask_total: 0
      });
    }
  }

  // ---- cluster event log (mirrors `cluster get-logs` output) ----
  // Columns: Date · NodeId · Event · Level · Message · Storage_ID · VUID · Status
  const nodeIds = nodes.map(n => n.uuid);
  const devIds = devs.map(d => d.uuid);
  const lvols = X.lvols.filter(v => v.cluster_id === c.uuid);
  const nodeName = id => (nodes.find(n => n.uuid === id) || {}).hostname || null;
  const devOf = id => devs.find(d => d.uuid === id) || {};
  const SKIP = ["skipped:dev_unavailable", "skipped:device_node_offline", "skipped:device_node_unreachable", "skipped:node_wide_io_quorum", "skipped:node_offline", "late_by_13s_skipping", "late_by_14s_skipping", "late_by_15s_skipping"];
  const DEV_ERR = ["SPDK_BDEV_EVENT_REMOVE (3)", "error_write (3)", "error_read (3)", "error_write_cannot_allocate (4)", "error_unmap (3)", "error_read (4)"];
  const push = (ts, o) => X.logs.push(Object.assign({
    uuid: xuuid(),
    cluster_id: c.uuid,
    ts,
    node_id: null,
    event: "STATUS_CHANGE",
    level: "Info",
    message: "",
    storage_id: null,
    vuid: null,
    record_status: "None",
    object_kind: null,
    object_id: null,
    object_name: null
  }, o));

  // bootstrap trail
  push(xago(310), {
    event: "OBJ_CREATED",
    message: `Cluster created ${c.uuid}`,
    object_kind: "cluster",
    object_id: c.uuid,
    object_name: c.name
  });
  push(xago(309.9), {
    event: "OBJ_CREATED",
    node_id: xuuid(),
    message: `Management node added ip-172-31-46-${xint(10, 90)}`,
    object_kind: "node"
  });
  devs.slice(0, 12).forEach((d, i) => push(xago(309 - i * .01), {
    event: "OBJ_CREATED",
    node_id: d.uuid,
    message: `Device created: ${d.uuid}`,
    storage_id: i,
    object_kind: "device",
    object_id: d.uuid,
    object_name: d.serial_number
  }));
  nodes.forEach((n, i) => push(xago(308 - i * .02), {
    message: "Storage node status changed from: in_creation to: online",
    node_id: n.uuid,
    object_kind: "node",
    object_id: n.uuid,
    object_name: n.hostname
  }));
  push(xago(307.5), {
    message: "Cluster status changed from unready to in_activation",
    object_kind: "cluster",
    object_id: c.uuid,
    object_name: c.name
  });
  nodes.forEach((n, i) => push(xago(307.2 - i * .02), {
    level: "Warning",
    node_id: n.uuid,
    message: `Storage node ports set, LVol:${4432 + i * 2} RPC:${4420 + i} Internal:${4426 + i}`,
    object_kind: "node",
    object_id: n.uuid,
    object_name: n.hostname
  }));
  push(xago(307), {
    message: `Cluster status changed from in_activation to ${c.status === "online" ? "active" : c.status}`,
    object_kind: "cluster",
    object_id: c.uuid,
    object_name: c.name
  });
  X.pools.filter(p => p.cluster_id === c.uuid).forEach((p, i) => push(xago(306.8 - i * .05), {
    event: "OBJ_CREATED",
    node_id: c.uuid,
    message: `Pool created ${p.pool_name}`,
    object_kind: "pool",
    object_id: p.uuid,
    object_name: p.pool_name
  }));
  lvols.slice(0, 8).forEach((v, i) => push(xago(300 - i * .02), {
    event: "OBJ_CREATED",
    node_id: v.uuid,
    message: `LVol created, ${v.lvol_name}`,
    object_kind: "lvol",
    object_id: v.uuid,
    object_name: v.lvol_name
  }));

  // running operational stream
  for (let i = 0; i < 260; i++) {
    const ts = xago(i * 0.42 + Math.random() * 0.3);
    const roll = Math.random();
    const nid = nodeIds.length ? xpick(nodeIds) : null;
    const d = devIds.length ? devOf(xpick(devIds)) : {};
    const sid = xint(0, 11);
    if (roll < .26) {
      push(ts, {
        event: "device_status",
        level: "Error",
        node_id: nid,
        message: xpick(DEV_ERR),
        storage_id: sid,
        record_status: Math.random() > .3 ? "processed" : xpick(SKIP),
        object_kind: "device",
        object_id: d.uuid,
        object_name: d.serial_number
      });
    } else if (roll < .40) {
      const allowed = Math.random() > .5;
      push(ts, {
        level: "Warning",
        node_id: nid,
        message: `Port ${allowed ? "allowed" : "blocked"}: ${4420 + xint(0, 22)}`,
        record_status: "None",
        object_kind: "node",
        object_id: nid,
        object_name: nodeName(nid)
      });
    } else if (roll < .54) {
      const to = Math.random() > .5;
      push(ts, {
        node_id: nid,
        message: `Storage node health check changed from: ${!to} to: ${to}`,
        object_kind: "node",
        object_id: nid,
        object_name: nodeName(nid)
      });
    } else if (roll < .68) {
      const down = Math.random() > .45;
      push(ts, {
        node_id: d.uuid,
        storage_id: sid,
        message: down ? "Device status changed from: online to: unavailable" : "Device restarted, status: online",
        level: down ? "Warning" : "Info",
        record_status: down && Math.random() > .6 ? "forced_unavailable:remote_io_quorum" : "None",
        object_kind: "device",
        object_id: d.uuid,
        object_name: d.serial_number
      });
    } else if (roll < .76) {
      push(ts, {
        node_id: d.uuid,
        storage_id: sid,
        message: "Device health changed from: None to: True",
        object_kind: "device",
        object_id: d.uuid,
        object_name: d.serial_number
      });
    } else if (roll < .84) {
      const deg = Math.random() > .5;
      push(ts, {
        message: `Cluster status changed from ${deg ? "active to degraded" : "degraded to active"}`,
        level: deg ? "Warning" : "Info",
        object_kind: "cluster",
        object_id: c.uuid,
        object_name: c.name
      });
    } else if (roll < .90) {
      push(ts, {
        event: "OBJ_CREATED",
        node_id: nid,
        message: xpick(["Task created", "Re-balancing task updated"]),
        record_status: xpick(["new", "running", "suspended"]),
        object_kind: "task",
        object_id: nid
      });
    } else if (roll < .96) {
      const from = xpick(["offline to: in_restart", "in_restart to: online", "online to: in_shutdown", "in_shutdown to: offline", "online to: unreachable", "unreachable to: offline", "in_restart to: offline"]);
      push(ts, {
        node_id: nid,
        message: `Storage node status changed from: ${from}`,
        level: /offline|unreachable/.test(from.split("to: ")[1]) ? "Warning" : "Info",
        object_kind: "node",
        object_id: nid,
        object_name: nodeName(nid)
      });
    } else if (roll < .985) {
      push(ts, {
        event: "jm_compression",
        node_id: nid,
        vuid: String(xint(1, 24)),
        message: xpick(["compression_started", "compression_finished"]),
        record_status: xpick(["running", "processed"]),
        object_kind: "node",
        object_id: nid,
        object_name: nodeName(nid)
      });
    } else {
      push(ts, {
        level: "Error",
        node_id: nid,
        message: "Storage node LVStore recovery failed",
        object_kind: "node",
        object_id: nid,
        object_name: nodeName(nid)
      });
    }
  }
  // one long, wrapped diagnostic like the real output
  if (devs.length) {
    const d = devs[0];
    push(xago(4.2), {
      level: "Warning",
      node_id: d.uuid,
      storage_id: 2,
      message: `Device ${d.uuid} (storage_id 2) block-size-normalized IO latency 2.7x the cluster average over 10m (19.29 vs 7.25 ticks/byte) - possible degraded device`,
      object_kind: "device",
      object_id: d.uuid,
      object_name: d.serial_number
    });
  }
  X.logs.sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts));

  // ---- SPDK threads + logs per node (Prometheus / node agent) ----
  nodes.forEach(n => {
    const cores = xint(4, 10);
    const threads = [];
    for (let core = 0; core < cores; core++) {
      const names = core === 0 ? ["app_thread"] : [`nvmf_tgt_poll_group_${core - 1}`];
      if (core > 0 && Math.random() > .6) names.push(`distr_qos_${core}`);
      names.forEach(nm => threads.push({
        name: nm,
        core,
        busy_pct: n.status === "online" ? +(Math.random() * 82 + 4).toFixed(1) : 0,
        poll_count: n.status === "online" ? xint(200000, 9000000) : 0,
        idle_tsc: xint(1e9, 9e9)
      }));
    }
    X.spdk_threads[n.uuid] = {
      cores,
      threads
    };
    X.spdk_logs[n.uuid + "/spdk"] = Array.from({
      length: 80
    }, (_, i) => spdkLine(i, false));
    X.spdk_logs[n.uuid + "/spdk-proxy"] = Array.from({
      length: 80
    }, (_, i) => spdkLine(i, true));
  });
});

// Some volumes have been moved recently, so the rebalancing counters on a
// cluster are not all zero. Every move is an lvol_migration task, and the
// counters are derived from these records by the rollup.
X.clusters.forEach(c => {
  if (!c.auto_rebalance || !c.auto_rebalance.enabled) return;
  const vols = X.lvols.filter(v => v.cluster_id === c.uuid && v.nodes && v.nodes.primary);
  const n = Math.min(vols.length, xint(0, 9));
  for (let i = 0; i < n; i++) {
    const v = vols[i];
    const hrs = i < 2 ? Math.random() * 0.9 : 1 + Math.random() * 22;
    X.tasks.push({
      uuid: xuuid(),
      cluster_id: c.uuid,
      parent_id: null,
      function_name: "lvol_migration",
      target_id: `LvolID:${v.uuid}`,
      node_id: v.nodes.primary.uuid,
      distrib: null,
      retry: 0,
      max_retry: 3,
      status: "done",
      result: `Volume ${v.lvol_name} moved to ${v.nodes.primary.hostname}`,
      created_at: xago(hrs),
      updated_at: xago(hrs - .01),
      canceled: false,
      subtask_total: 0
    });
  }
});
XU.rollup();

// ---- SMART per device -----------------------------------------------------
// A health check reads the drive's SMART counters and the verdict follows from
// them: spare below the 10% threshold, rising media errors or a set critical
// warning bit mean critical; a handful of corrected errors or a hot drive mean
// warn. The device's health traffic light is that verdict — there is no second,
// independent notion of health anywhere.
function smartRead(d, bias) {
  // bias lets the fixture keep a seeded verdict stable across a re-read, and
  // lets a genuinely dying drive stay dying instead of randomly recovering
  const b = bias || d.health_check || "good";
  const bad = b === "critical",
    warn = b === "warn";
  const spare = bad ? xint(3, 9) : warn ? xint(28, 62) : xint(92, 100);
  const media = bad ? xint(180, 4000) : warn ? xint(1, 12) : 0;
  const errLog = bad ? xint(40, 900) : warn ? xint(1, 6) : 0;
  const temp = d.temperature_c;
  const critWarn = bad ? 0x04 : 0x00;
  // the verdict, derived from the counters above
  const verdict = critWarn || spare < 10 || media > 100 ? "critical" : media > 0 || errLog > 0 || spare < 70 || temp > 60 ? "warn" : "good";
  return {
    verdict,
    report: {
      checked_at: xago(0),
      overall: verdict === "critical" ? "FAILED" : verdict === "warn" ? "PASSED (warnings)" : "PASSED",
      verdict,
      model: d.model_number,
      serial: d.serial_number,
      firmware: d.firmware_revision,
      attributes: [{
        name: "Critical warning",
        value: "0x" + critWarn.toString(16).padStart(2, "0"),
        note: critWarn ? "reliability degraded" : null
      }, {
        name: "Temperature",
        value: temp + " °C",
        note: temp > 60 ? "above advisory threshold" : null
      }, {
        name: "Available spare",
        value: spare + " %",
        note: spare < 10 ? "below threshold (10 %)" : spare < 70 ? "declining" : null
      }, {
        name: "Percentage used",
        value: d.percentage_used + " %"
      }, {
        name: "Data units read",
        value: xint(2, 900) + " M"
      }, {
        name: "Data units written",
        value: xint(2, 700) + " M"
      }, {
        name: "Power on hours",
        value: d.power_on_hours + " h"
      }, {
        name: "Unsafe shutdowns",
        value: String(xint(0, 12))
      }, {
        name: "Media & data integrity errors",
        value: String(media),
        note: media > 100 ? "rising" : media ? "corrected" : null
      }, {
        name: "Error information log entries",
        value: String(errLog)
      }]
    }
  };
}

// Run a health check: re-read SMART and update the device's health from it.
// Both the HealthCheck operation and an on-demand SMART re-read come here.
X.runHealthCheck = function (uuid, bias) {
  const d = X.devices.find(y => y.uuid === uuid);
  if (!d) return null;
  const live = d.status === "online" || d.status === "read_only";
  if (!live) {
    // nvme-cli cannot talk to a drive that is not attached
    X.smart[uuid] = Object.assign({}, X.smart[uuid] || {}, {
      unavailable: true,
      checked_at: xago(0)
    });
    d.health_check = null;
    return X.smart[uuid];
  }
  // a drive usually reports what it reported last time; sometimes it degrades
  const drift = bias || (Math.random() > .88 ? d.health_check === "good" ? "warn" : d.health_check === "warn" ? "critical" : d.health_check : d.health_check);
  const r = smartRead(d, drift);
  X.smart[uuid] = r.report;
  d.health_check = r.verdict;
  d.last_health_check = xago(0);
  return r.report;
};
X.devices.forEach(d => {
  const live = d.status === "online" || d.status === "read_only";
  if (!live) {
    X.smart[d.uuid] = {
      unavailable: true,
      checked_at: xago(xint(1, 60))
    };
    return;
  }
  const r = smartRead(d);
  d.health_check = r.verdict;
  d.last_health_check = xago(xint(0, 48));
  X.smart[d.uuid] = Object.assign(r.report, {
    checked_at: d.last_health_check
  });
});
function logLine(name, i) {
  const lvl = xpick(["INFO", "INFO", "INFO", "INFO", "WARN", "ERROR", "DEBUG"]);
  const msgs = {
    INFO: [`${name} healthy, heartbeat ok`, "reconciled desired state", "GET /api/v2/clusters 200 12ms", "flushed metrics to prometheus", "lease renewed"],
    WARN: ["retrying transaction after conflict", "slow query 812ms", "connection pool at 85%"],
    ERROR: ["transaction_too_old, retrying", "failed to reach node, backing off", "read version timeout"],
    DEBUG: ["cache hit ratio 0.94", "gc pass complete"]
  };
  return {
    ts: xago(i * 0.12),
    level: lvl,
    msg: xpick(msgs[lvl])
  };
}
function spdkLine(i, proxy) {
  const lvl = xpick(["INFO", "INFO", "INFO", "NOTICE", "WARNING", "ERROR"]);
  const msgs = proxy ? ["rpc: bdev_get_bdevs took 3ms", "proxy accepted connection from 10.20.0.4", "forwarding rpc distr_status", "client disconnected", "rpc timeout, retry 1/3"] : ["nvmf_tgt: new qpair on poll group 2", "bdev_distr: chunk rebuild 42%", "accel_fw: task queue depth 12", "nvme_pcie: admin cmd completed", "bdev_nvme: reset controller 0000:5e:00.0", "distr: page migration queued"];
  return {
    ts: xago(i * 0.05),
    level: lvl,
    msg: xpick(msgs)
  };
}

// ---- the control plane (agent, not API) -----------------------------------
// One deployment manages every cluster, so its containers and its state
// database are seeded once — not per cluster.
(function seedControlPlane() {
  const cs = window.SB_DB.clusters;
  const version = (cs[0] || {}).cluster_version || "26.2.1";
  const anyLive = cs.some(c => c.status !== "unready");
  const mk = (name, group, cores, memLimit, diskLimit) => {
    X.containers.push({
      name,
      group,
      image: `simplyblock/${name}:${version}`,
      state: "running",
      cpu_cores_alloc: cores,
      cpu_pct: +(Math.random() * cores * 60).toFixed(1),
      mem_limit: memLimit,
      mem_used: Math.round(memLimit * (.25 + Math.random() * .5)),
      disk_limit: diskLimit,
      disk_used: Math.round(diskLimit * (.15 + Math.random() * .6)),
      restarts: xint(0, 4),
      uptime_h: xint(2, 900)
    });
    X.container_logs[name] = Array.from({
      length: 60
    }, (_, i) => logLine(name, i));
  };
  mk("fdb-coordinator", "state db", 2, 4 * XGB, 40 * XGB);
  mk("fdb-storage-1", "state db", 4, 8 * XGB, 200 * XGB);
  mk("fdb-storage-2", "state db", 4, 8 * XGB, 200 * XGB);
  mk("simplyblock-core", "services", 4, 8 * XGB, 20 * XGB);
  mk("simplyblock-tasks", "services", 2, 4 * XGB, 10 * XGB);
  mk("simplyblock-webapp", "services", 1, 2 * XGB, 5 * XGB);
  mk("prometheus", "monitoring", 2, 8 * XGB, 500 * XGB);
  if (anyLive) {
    mk("graylog", "observability", 2, 8 * XGB, 200 * XGB);
    mk("opensearch", "observability", 4, 16 * XGB, 1000 * XGB);
    mk("mongodb", "observability", 2, 4 * XGB, 100 * XGB);
  }
  for (let i = 0; i < xint(8, 16); i++) {
    X.fdb_backups.push({
      id: `fdb-bk-${xhex(6)}`,
      version: `${version}-${String(200 - i * 3).padStart(4, "0")}`,
      created_at: xago(i * 6 + xint(0, 3)),
      size: xint(180, 2400) * 1e6,
      type: i % 4 === 0 ? "full" : "incremental",
      status: i === 0 ? xpick(["complete", "complete", "in_progress"]) : "complete"
    });
  }
  // A few conditions are lit on purpose, so the indicator board has something
  // to show. Everything else the rules derive from ordinary fixture state.
  cs.forEach((c, ci) => {
    const ns = window.SB_DB.storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
    if (!ns.length) return;
    if (ci % 3 === 0 && ns[0]) {
      ns[0].meta_compaction_failed = true;
      ns[0].meta_size_util = Math.round(ns[0].meta_size_total * .93);
    }
    if (ci % 3 === 1 && ns[0]) ns[0].objects_used = ns[0].objects_max - xint(1, 400);
    if (ns[1]) ns[1].latency_us = xint(1400, 2600);
  });
  const fdbFail = X.fdb_backups[1];
  if (fdbFail) fdbFail.status = "failed";
  const core = X.containers.find(c => c.name === "simplyblock-webapp");
  if (core) core.restarts = 6;
})();

// ---- alerts ---------------------------------------------------------------
// An alert is NOT an event. It is an indicator: a named condition evaluated
// against live state on every read. While the condition holds the alert is
// there; the moment it clears the alert is gone. Nothing to dismiss, no
// reconciliation event to file — the lamp simply goes out.
//
// Silences are the one piece of stored state: an operator can turn a lamp's
// noise off, keyed by rule + object, and the silence dies with the condition.
X.silences = {};
const silKey = (rule, id, scopeId) => rule + "|" + (scopeId || "cp") + "|" + (id || "-");
// first_seen: the console wants to know how long a lamp has been lit, and the
// condition itself carries no timestamp, so the first sighting is remembered.
X.alert_seen = {};
// Conditions that are already true when the mock boots have been true for a
// while — dating them all "now" would make every lamp look brand new.
const ALERT_BOOT_MS = Date.now();
const seenAt = key => {
  if (!X.alert_seen[key]) X.alert_seen[key] = Date.now() - ALERT_BOOT_MS < 20000 ? xago(xint(1, 190) + Math.random()) : new Date().toISOString();
  return X.alert_seen[key];
};
const pctOf = (u, t) => t ? u / t : 0;

// Every rule: id, severity, scope, and a function returning zero or more
// conditions. Node and device rules always name the node and the device(s).
const ALERT_RULES = [{
  rule: "storage_node_offline",
  severity: "critical",
  scope: "cluster",
  title: "Storage node offline",
  remedy: "Restart the node. If it was stopped on purpose, this alert clears once it is back online.",
  eval: (c, ctx) => ctx.nodes.filter(n => n.status === "offline" && !n.maintenance).map(n => ({
    node: n,
    devices: ctx.devices.filter(d => d.node_id === n.uuid),
    detail: `${n.hostname} stopped without a shutdown request. Volumes primary on this node have failed over to their secondaries.`
  }))
}, {
  rule: "storage_node_unreachable",
  severity: "critical",
  scope: "cluster",
  title: "Storage node unreachable",
  remedy: "Check the host and the management network, then restart the node.",
  eval: (c, ctx) => ctx.nodes.filter(n => n.status === "unreachable" || n.status === "down").map(n => ({
    node: n,
    devices: ctx.devices.filter(d => d.node_id === n.uuid),
    detail: `No heartbeat from ${n.hostname} on ${n.mgmt_ip}. Its devices cannot be reached, so their chunks are served with reduced redundancy.`
  }))
}, {
  rule: "device_unavailable",
  severity: "critical",
  scope: "cluster",
  title: "Device unavailable",
  remedy: "Restart the device. If it does not come back, fail it so the cluster rebuilds its chunks elsewhere.",
  eval: (c, ctx) => {
    // grouped per node: one lamp per node, naming every affected device
    const byNode = {};
    ctx.devices.filter(d => d.status === "unavailable").forEach(d => (byNode[d.node_id] = byNode[d.node_id] || []).push(d));
    return Object.keys(byNode).map(nid => {
      const n = ctx.nodes.find(y => y.uuid === nid),
        ds = byNode[nid];
      // a device that is unavailable only because its node is down is not a
      // separate fault; the node alert already says it
      if (n && n.status !== "online" && n.status !== "read_only") return null;
      return {
        node: n,
        devices: ds,
        detail: `${ds.length} device(s) on ${n ? n.hostname : "an unknown node"} report gone to SPDK: ${ds.map(d => d.serial_number).join(", ")}.`
      };
    }).filter(Boolean);
  }
}, {
  rule: "metadata_compaction_failed",
  severity: "critical",
  scope: "cluster",
  title: "Metadata compaction failed",
  remedy: "Inspect the SPDK log on the node, free metadata capacity, then let compaction retry.",
  eval: (c, ctx) => ctx.nodes.filter(n => n.meta_compaction_failed).map(n => ({
    node: n,
    devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").slice(0, 2),
    detail: `The last compaction pass on ${n.hostname} did not finish. Metadata keeps growing until it succeeds — at ${Math.round(pctOf(n.meta_size_util, n.meta_size_total) * 100)}% of the metadata arena.`
  }))
}, {
  rule: "metadata_capacity_critical",
  severity: "critical",
  scope: "cluster",
  title: "Metadata capacity critical",
  remedy: "Free logical volumes or snapshots on this node, or add capacity. Writes stop when the arena is full.",
  eval: (c, ctx) => ctx.nodes.filter(n => pctOf(n.meta_size_util, n.meta_size_total) > .9).map(n => ({
    node: n,
    devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").slice(0, 2),
    detail: `The metadata arena on ${n.hostname} is ${Math.round(pctOf(n.meta_size_util, n.meta_size_total) * 100)}% full.`
  }))
}, {
  rule: "cluster_degraded",
  severity: "critical",
  scope: "cluster",
  title: "Cluster degraded",
  remedy: "Bring the affected nodes back online and let rebalancing finish.",
  eval: (c, ctx) => {
    if (c.status !== "degraded") return [];
    const down = ctx.nodes.filter(n => n.status !== "online" && n.status !== "read_only");
    // a degradation that is nothing but a maintenance shutdown is expected
    if (down.length && down.every(n => n.maintenance)) return [];
    return [{
      node: down[0] || null,
      devices: down.length === 1 ? ctx.devices.filter(d => d.node_id === down[0].uuid).slice(0, 6) : [],
      nodes: down,
      detail: `${down.length} of ${ctx.nodes.length} storage nodes are not online${down.length ? ": " + down.map(n => n.hostname).join(", ") : ""}. Capacity is served with reduced redundancy.`
    }];
  }
}, {
  rule: "slow_node",
  severity: "warning",
  scope: "cluster",
  title: "Slow node",
  remedy: "Check the node's SPDK thread utilization and its devices' latency; consider failing a slow device.",
  eval: (c, ctx) => {
    const live = ctx.nodes.filter(n => n.status === "online");
    if (live.length < 2) return [];
    const avg = live.reduce((s, n) => s + (n.latency_us || 0), 0) / live.length;
    return live.filter(n => n.latency_us > avg * 1.8).map(n => ({
      node: n,
      devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").sort((a, b) => b.temperature_c - a.temperature_c).slice(0, 3),
      detail: `${n.hostname} answers at ${n.latency_us} µs against a cluster average of ${Math.round(avg)} µs. Volumes primary on this node see the difference.`
    }));
  }
}, {
  rule: "object_limit_reached",
  severity: "critical",
  scope: "cluster",
  title: "Object limit reached",
  remedy: "Move volumes to another node, or raise max-subsystems and restart the node.",
  eval: (c, ctx) => ctx.nodes.filter(n => pctOf(n.objects_used, n.objects_max) > .95).map(n => ({
    node: n,
    devices: [],
    detail: `${n.hostname} holds ${n.objects_used} of ${n.objects_max} objects. No new volume, snapshot or clone can be created on this node.`
  }))
}, {
  rule: "capacity_warning",
  severity: "warning",
  scope: "cluster",
  title: "Cluster capacity warning",
  remedy: "Expand the cluster with another node, or free provisioned capacity.",
  eval: c => {
    const p = pctOf(c.size_util, c.size_total);
    return p > .75 && p <= .9 ? [{
      detail: `${Math.round(p * 100)}% of ${(c.size_total / 1e12).toFixed(1)} TB is utilized.`
    }] : [];
  }
}, {
  rule: "capacity_critical",
  severity: "critical",
  scope: "cluster",
  title: "Cluster capacity critical",
  remedy: "Expand the cluster now. The cluster switches to read-only when it fills.",
  eval: c => {
    const p = pctOf(c.size_util, c.size_total);
    return p > .9 ? [{
      detail: `${Math.round(p * 100)}% of ${(c.size_total / 1e12).toFixed(1)} TB is utilized.`
    }] : [];
  }
}, {
  rule: "cluster_read_only",
  severity: "critical",
  scope: "cluster",
  title: "Cluster switched to read-only",
  remedy: "Free or add capacity. Writes resume once the cluster is below its threshold.",
  eval: c => c.status === "read_only" ? [{
    detail: "Every volume in this cluster refuses writes. Reads are unaffected."
  }] : []
}, {
  rule: "cluster_suspended",
  severity: "warning",
  scope: "cluster",
  title: "Cluster suspended",
  remedy: "Restart the cluster to resume serving volumes.",
  eval: c => c.status === "suspended" ? [{
    detail: "All storage nodes are stopped and no volume in this cluster is being served."
  }] : []
}, {
  rule: "fdb_backup_failed",
  severity: "critical",
  scope: "control-plane",
  title: "State DB backup failed",
  remedy: "Check the backup target and the fdb containers, then take a backup manually.",
  eval: () => X.fdb_backups.some(b => b.status === "failed") ? [{
    detail: `The most recent FoundationDB backup did not complete. Last good version: ${(X.fdb_backups.find(b => b.status === "complete") || {}).version || "none"}.`
  }] : []
}, {
  rule: "fdb_degraded",
  severity: "critical",
  scope: "control-plane",
  title: "State DB degraded",
  remedy: "Restart the failed fdb container. The control plane cannot accept writes while the state DB is degraded.",
  eval: () => X.containers.some(c => c.group === "state db" && c.state !== "running") ? [{
    detail: `${X.containers.filter(c => c.group === "state db" && c.state !== "running").map(c => c.name).join(", ")} not running.`
  }] : []
}, {
  rule: "fdb_capacity_critical",
  severity: "critical",
  scope: "control-plane",
  title: "State DB capacity critical",
  remedy: "Grow the fdb volumes or trim old task and log records.",
  eval: () => X.containers.filter(c => c.group === "state db" && c.disk_limit && c.disk_used / c.disk_limit > .9).map(c => ({
    detail: `${c.name} is at ${Math.round(c.disk_used / c.disk_limit * 100)}% of its ${Math.round(c.disk_limit / 1e9)} GB volume.`,
    container: c.name
  }))
}, {
  rule: "webapi_degraded",
  severity: "warning",
  scope: "control-plane",
  title: "Web API degraded",
  remedy: "Check the simplyblock-core and webapp containers; the console and the CSI driver both talk through this API.",
  eval: () => X.containers.filter(c => (c.name === "simplyblock-core" || c.name === "simplyblock-webapp") && (c.state !== "running" || c.restarts > 3)).map(c => ({
    detail: c.state !== "running" ? `${c.name} is ${c.state}.` : `${c.name} has restarted ${c.restarts} times.`,
    container: c.name
  }))
}];

// Evaluate every rule in a scope and return the lamps that are lit.
function evalAlerts(scope, cluster) {
  const out = [];
  ALERT_RULES.filter(r => r.scope === scope).forEach(r => {
    const ctx = cluster ? {
      nodes: window.SB_DB.storage_nodes.filter(n => n.cluster_id === cluster.uuid),
      devices: window.SB_DB.devices.filter(d => d.cluster_id === cluster.uuid)
    } : {
      nodes: [],
      devices: []
    };
    let hits = [];
    try {
      hits = r.eval(cluster, ctx) || [];
    } catch (e) {
      hits = [];
    }
    hits.forEach(h => {
      const node = h.node || null;
      const key = silKey(r.rule, node ? node.uuid : h.container || null, cluster ? cluster.uuid : null);
      out.push({
        uuid: key,
        rule: r.rule,
        severity: r.severity,
        scope: r.scope,
        cluster_id: cluster ? cluster.uuid : null,
        cluster_name: cluster ? cluster.name : null,
        title: r.title,
        detail: h.detail,
        remedy: r.remedy,
        node_id: node ? node.uuid : null,
        node_name: node ? node.hostname : null,
        node_ids: (h.nodes || (node ? [node] : [])).map(n => n.uuid),
        node_names: (h.nodes || (node ? [node] : [])).map(n => n.hostname),
        device_ids: (h.devices || []).map(d => d.uuid),
        device_names: (h.devices || []).map(d => d.serial_number || d.device_name),
        container: h.container || null,
        since: seenAt(key),
        silenced: !!X.silences[key],
        silenced_by: X.silences[key] || null
      });
    });
  });
  // Forget the first-sighting timestamp and the silence of a condition that has
  // cleared, so the lamp starts fresh when it lights again. The sweep may only
  // touch keys belonging to the scope just evaluated — this function is called
  // once per cluster, and a cluster must not garbage-collect its neighbours.
  const live = new Set(out.map(a => a.uuid));
  const mine = k2 => k2.split("|")[1] === (cluster ? cluster.uuid : "cp");
  Object.keys(X.alert_seen).forEach(k2 => {
    if (mine(k2) && !live.has(k2)) delete X.alert_seen[k2];
  });
  Object.keys(X.silences).forEach(k2 => {
    if (mine(k2) && !live.has(k2)) delete X.silences[k2];
  });
  return out;
}
X.evalAlerts = evalAlerts;
X.alertRuleCounts = {
  cluster: ALERT_RULES.filter(r => r.scope === "cluster").length,
  "control-plane": ALERT_RULES.filter(r => r.scope === "control-plane").length
};

// ---- live drift -----------------------------------------------------------
function extrasTick() {
  X.containers.forEach(c => {
    if (c.state !== "running") return;
    c.cpu_pct = Math.max(0, +(c.cpu_pct * (1 + (Math.random() - .5) * .3)).toFixed(1));
    c.mem_used = Math.min(c.mem_limit, Math.round(c.mem_used * (1 + (Math.random() - .5) * .04)));
  });
  Object.values(X.spdk_threads).forEach(s => s.threads.forEach(t => {
    if (!t.busy_pct) return;
    t.busy_pct = Math.min(99.9, Math.max(0.5, +(t.busy_pct + (Math.random() - .5) * 9).toFixed(1)));
    t.poll_count += xint(1000, 90000);
  }));
  X.tasks.forEach(t => {
    if (t.status === "running" && !t.canceled) t.updated_at = xago(Math.random() * .3);
  });
  X.storage_nodes.forEach(n => {
    if (n.status !== "online") return;
    n.memory_used = Math.min(n.memory_total, Math.round(n.memory_used * (1 + (Math.random() - .5) * .03)));
  });
}

// ---- routes ---------------------------------------------------------------
const xok = b => new Response(JSON.stringify(Object.assign({
  status: true
}, b)), {
  status: 200,
  headers: {
    "Content-Type": "application/json"
  }
});
const xfail = (c, m) => new Response(JSON.stringify({
  status: false,
  error: m
}), {
  status: c,
  headers: {
    "Content-Type": "application/json"
  }
});
const API_EXTRA = [["GET", /^\/clusters\/([\w-]+)\/tasks$/, m => {
  const c = X.clusters.find(x => x.uuid === m[1]);
  if (!c) return {
    __404: true
  };
  if (!c.capabilities.tasks) return {
    __cap: "This is an edge cluster. Edge deployments run no task engine — long-running operations are executed directly by the Kubernetes operator."
  };
  return {
    results: X.tasks.filter(t => t.cluster_id === m[1] && !t.parent_id)
  };
}], ["GET", /^\/tasks\/([\w-]+)\/subtasks$/, m => ({
  results: X.tasks.filter(t => t.parent_id === m[1])
})], ["POST", /^\/tasks\/([\w-]+)\/cancel$/, m => {
  const t = X.tasks.find(x => x.uuid === m[1]);
  if (!t) return {
    __404: true
  };
  if (t.status === "done") return {
    __err: "Completed tasks cannot be cancelled"
  };
  t.status = "suspended";
  t.canceled = true;
  t.updated_at = xago(0);
  X.tasks.filter(s => s.parent_id === t.uuid && s.status !== "done").forEach(s => {
    s.status = "suspended";
    s.canceled = true;
  });
  return {
    results: [t]
  };
}], ["GET", /^\/clusters\/([\w-]+)\/logs$/, m => ({
  results: X.logs.filter(l => l.cluster_id === m[1])
})], ["GET", /^\/clusters\/([\w-]+)\/alerts$/, m => {
  const c = window.SB_DB.clusters.find(x2 => x2.uuid === m[1]);
  return {
    results: c ? X.evalAlerts("cluster", c) : []
  };
}], ["GET", /^\/alerts$/, () => ({
  results: window.SB_DB.clusters.flatMap(c => X.evalAlerts("cluster", c)).concat(X.evalAlerts("control-plane", null))
})], ["GET", /^\/control-plane\/alerts$/, () => ({
  results: X.evalAlerts("control-plane", null)
})], ["POST", /^\/alerts\/(.+)\/silence$/, m => {
  X.silences[decodeURIComponent(m[1])] = "ops@simplyblock.io";
  return {
    results: []
  };
}], ["POST", /^\/alerts\/(.+)\/unsilence$/, m => {
  delete X.silences[decodeURIComponent(m[1])];
  return {
    results: []
  };
}],
// ---- S3 buckets: one bucket is one logical volume ----
["POST", /^\/clusters\/([\w-]+)\/buckets$/, (m, b) => {
  const c = X.clusters.find(x => x.uuid === m[1]);
  if (!c) return {
    __404: true
  };
  if (!c.object_storage || !c.object_storage.enabled) return {
    __cap: "Object storage is not enabled on this cluster."
  };
  if (!b.name || !/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(b.name)) return {
    __err: "Bucket names are 3–63 characters of lowercase letters, digits, dots and hyphens (S3 naming rules)."
  };
  if (X.buckets.some(x => x.name === b.name)) return {
    __err: `A bucket named ${b.name} already exists — bucket names are unique across the endpoint.`
  };
  const pool = X.pools.find(p => p.uuid === b.pool_id) || X.pools.find(p => p.cluster_id === c.uuid);
  if (!pool) return {
    __err: "No pool to provision the bucket's volume in."
  };
  const tmpl = X.lvols.find(v => v.cluster_id === c.uuid && v.status === "online") || X.lvols[0];
  const v = JSON.parse(JSON.stringify(tmpl));
  v.uuid = xuuid();
  v.lvol_name = `s3-${b.name}`;
  v.pool_id = pool.uuid;
  v.pool_name = pool.pool_name;
  v.size_prov = Number(b.size) || 1e12;
  v.size_util = 0;
  v.status = "online";
  v.pvc = null;
  v.bucket = null;
  v.replication = null;
  v.consistency_group = null;
  v.crypto_enabled = !!b.encryption;
  v.created_at = xago(0);
  X.lvols.push(v);
  const bucket = {
    uuid: xuuid(),
    cluster_id: c.uuid,
    name: b.name,
    lvol_id: v.uuid,
    lvol_name: v.lvol_name,
    pool_id: pool.uuid,
    pool_name: pool.pool_name,
    status: "online",
    versioning: !!b.versioning,
    object_lock: !!b.object_lock,
    quota_bytes: Number(b.quota) || 0,
    objects: 0,
    size_bytes: 0,
    region: c.object_storage.region,
    storage_class: b.storage_class || "standard",
    owner: b.owner || b.namespace || "default",
    tags: b.tags || {},
    lifecycle_rules: [],
    cors_enabled: false,
    access: {
      service_account: b.service_account || `sb-s3-${b.name}`,
      namespace: b.namespace || "default",
      secret_name: `${b.name}-s3-credentials`,
      access_key_id: `SB${xhex(9).toUpperCase()}`,
      policy: b.policy || "read-write",
      public: false
    },
    created_at: xago(0)
  };
  X.buckets.push(bucket);
  v.bucket = {
    uuid: bucket.uuid,
    name: bucket.name
  };
  XU.rollup();
  return {
    results: [bucket]
  };
}], ["DELETE", /^\/buckets\/([\w-]+)$/, m => {
  const i = X.buckets.findIndex(x => x.uuid === m[1]);
  if (i < 0) return {
    __404: true
  };
  const b = X.buckets[i];
  if (b.objects > 0) return {
    __err: `${b.name} still holds ${b.objects} object(s). S3 refuses to delete a non-empty bucket — empty it first.`
  };
  if (b.object_lock) return {
    __err: "Object lock is enabled — the bucket cannot be deleted while a retention configuration exists."
  };
  X.buckets.splice(i, 1);
  X.lvols = X.lvols.filter(v => v.uuid !== b.lvol_id);
  (X.dr_policies || []).forEach(p => {
    p.lvol_ids = p.lvol_ids.filter(id => id !== b.lvol_id);
  });
  XU.rollup();
  return {
    results: []
  };
}], ["PUT", /^\/buckets\/([\w-]+)$/, (m, b) => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  if (x.object_lock && b.object_lock === false) return {
    __err: "Object lock cannot be disabled once enabled (S3 semantics)."
  };
  if (b.versioning !== undefined) x.versioning = !!b.versioning;
  if (b.object_lock !== undefined) x.object_lock = !!b.object_lock;
  if (b.quota !== undefined) x.quota_bytes = Number(b.quota) || 0;
  if (b.storage_class) x.storage_class = b.storage_class;
  return {
    results: [x]
  };
}], ["PUT", /^\/buckets\/([\w-]+)\/tags$/, (m, b) => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  const tags = b.tags || {};
  if (Object.keys(tags).length > 50) return {
    __err: "S3 allows at most 50 tags per bucket."
  };
  x.tags = tags;
  return {
    results: [x]
  };
}], ["PUT", /^\/buckets\/([\w-]+)\/access$/, (m, b) => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  Object.assign(x.access, {
    namespace: b.namespace || x.access.namespace,
    service_account: b.service_account || x.access.service_account,
    policy: b.policy || x.access.policy,
    public: !!b.public
  });
  if (b.rotate_key) x.access.access_key_id = `SB${xhex(9).toUpperCase()}`;
  return {
    results: [x]
  };
}], ["POST", /^\/buckets\/([\w-]+)\/resize$/, (m, b) => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  const v = X.lvols.find(y => y.uuid === x.lvol_id);
  if (v && Number(b.size) < v.size_prov) return {
    __err: "A bucket's filesystem can only grow."
  };
  if (v) v.size_prov = Number(b.size);
  XU.rollup();
  return {
    results: [x]
  };
}],
// Replication: the bucket is its volume, so attaching it to a policy is
// attaching the volume. Only policies whose source is this cluster qualify.
["POST", /^\/buckets\/([\w-]+)\/replicate$/, (m, b) => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  const pol = (X.dr_policies || []).find(p => p.uuid === b.policy_id);
  if (!pol) return {
    __404: true
  };
  if (pol.source_cluster_id !== x.cluster_id) return {
    __err: `${pol.name} replicates from another cluster — a bucket can only join a policy whose source is its own cluster.`
  };
  const v = X.lvols.find(y => y.uuid === x.lvol_id);
  if (!v) return {
    __404: true
  };
  if (v.replication) return {
    __err: `${x.name} is already replicated by ${v.replication.policy_name}. Detach it first.`
  };
  pol.lvol_ids.push(v.uuid);
  v.replication = {
    policy_id: pol.uuid,
    policy_name: pol.name,
    mode: pol.mode,
    status: "healthy",
    last_replication_at: xago(0),
    backlog_bytes: 0,
    target_cluster_id: pol.target_cluster_id
  };
  XU.rollup();
  return {
    results: [x]
  };
}], ["POST", /^\/buckets\/([\w-]+)\/unreplicate$/, m => {
  const x = X.buckets.find(y => y.uuid === m[1]);
  if (!x) return {
    __404: true
  };
  const v = X.lvols.find(y => y.uuid === x.lvol_id);
  if (!v || !v.replication) return {
    __err: "The bucket is not replicated."
  };
  (X.dr_policies || []).forEach(p => {
    p.lvol_ids = p.lvol_ids.filter(id => id !== v.uuid);
  });
  v.replication = null;
  XU.rollup();
  return {
    results: [x]
  };
}]];
const AGENT = [["GET", /^\/control-plane\/containers$/, () => ({
  results: X.containers
})], ["GET", /^\/control-plane\/containers\/([\w.-]+)\/logs$/, m => ({
  results: X.container_logs[m[1]] || []
})], ["GET", /^\/control-plane\/fdb\/backups$/, () => ({
  results: X.fdb_backups
})], ["POST", /^\/control-plane\/fdb\/backups\/([\w-]+)\/restore$/, m => {
  const b = X.fdb_backups.find(x => x.id === m[1]);
  if (!b) return {
    __404: true
  };
  if (b.status !== "complete") return {
    __err: "Backup is still being written"
  };
  b.restore_requested_at = xago(0);
  return {
    results: [b]
  };
}], ["GET", /^\/nodes\/([\w-]+)\/logs\/([\w-]+)$/, m => {
  const key = m[1] + "/" + m[2];
  const buf = X.spdk_logs[key];
  if (!buf) return {
    __404: true
  };
  for (let i = 0; i < xint(1, 4); i++) {
    buf.unshift(spdkLine(0, m[2] === "spdk-proxy"));
    buf.pop();
  }
  return {
    results: buf
  };
}], ["GET", /^\/devices\/([\w-]+)\/smart$/, m => X.smart[m[1]] ? {
  results: [X.smart[m[1]]]
} : {
  __404: true
}], ["POST", /^\/devices\/([\w-]+)\/smart\/refresh$/, m => {
  const r = X.runHealthCheck(m[1]);
  return r ? {
    results: [r]
  } : {
    __404: true
  };
}]];
const prevFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfg = window.SB_CONFIG;
  if (!cfg.mock) return prevFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const method = (init && init.method || "GET").toUpperCase();
  const M = window.SB_MOCK;
  const table = url.startsWith(cfg.agentBase) ? {
    base: cfg.agentBase,
    routes: AGENT
  } : url.startsWith(cfg.operatorBase + "/proposed") ? {
    base: cfg.operatorBase + "/proposed",
    routes: API_EXTRA
  } : null;
  const isProm = url.startsWith(cfg.promBase);
  if (!table && !isProm) return prevFetch(input, init);
  if (isProm) {
    await new Promise(r => setTimeout(r, M.latency[0] + Math.random() * (M.latency[1] - M.latency[0])));
    if (M.offline) return xfail(0, "Prometheus unreachable (mock offline)");
    extrasTick();
    const q = decodeURIComponent((url.split("query=")[1] || "").split("&")[0]);
    const nodeId = (q.match(/node="([\w-]+)"/) || [])[1];
    const s = X.spdk_threads[nodeId];
    if (!s) return xok({
      data: {
        resultType: "vector",
        result: []
      }
    });
    return xok({
      data: {
        resultType: "vector",
        result: s.threads.map(t => ({
          metric: {
            __name__: "spdk_thread_busy_percent",
            node: nodeId,
            thread: t.name,
            core: String(t.core)
          },
          value: [Date.now() / 1000, String(t.busy_pct)]
        }))
      }
    });
  }

  // The client now scopes child collections with ?scope=<parent>&scopeId=<id>.
  // These routes were written against the nested form, so fold the query back
  // into it and keep their logic — including the edge-cluster capability gates.
  const [rawRoute, rawQ] = url.slice(table.base.length).split("?");
  const q = new URLSearchParams(rawQ || "");
  const scope = q.get("scope"),
    scopeId = q.get("scopeId");
  const route = (scope && scopeId ? `/${scope}/${scopeId}${rawRoute}` : rawRoute).replace(/\/$/, "");
  let body = {};
  try {
    if (init && init.body) body = JSON.parse(init.body);
  } catch (e) {}
  const hit = table.routes.find(([mm, re]) => mm === method && re.test(route));
  if (!hit) return prevFetch(input, init);
  M.requests++;
  await new Promise(r => setTimeout(r, M.latency[0] + Math.random() * (M.latency[1] - M.latency[0])));
  if (M.offline) return xfail(0, "Control plane unreachable (mock offline)");
  if (M.failNext) {
    M.failNext = false;
    return xfail(503, "Upstream returned 503 (injected)");
  }
  if (M.failRate && Math.random() < M.failRate) return xfail(503, "Upstream returned 503");
  extrasTick();
  const r = hit[2](route.match(hit[1]), body);
  if (r.__404) return xfail(404, "Resource not found");
  if (r.__cap) return xfail(501, r.__cap);
  if (r.__err) return xfail(409, r.__err);
  return xok(M.forceEmpty && r.results ? {
    results: []
  } : r);
};
})();
// ---- mock-repl.jsx ----
(function(){
// ---------------------------------------------------------------------------
// REPLICATION — the real v1alpha1 kinds
//
//   ReplicationPair    {sourceCluster, targetCluster}      targetCluster immutable
//     └─ ReplicationPolicy  {pairRef, mode, interval, snapshotRetention}
//          └─ ReplicationSlot  one per PVC, created by the operator, owned by
//                              the PVC, named <policy>-<pvc>
//          └─ ReplicationOps   one-shot {action, scope, ref}
//
// Volumes are NOT members of a policy. A volume opts in through the annotation
// storage.simplyblock.io/replication-policy, read from the PVC and from its
// StorageClass with the PVC winning. Everything the console does here is either
// an annotation write or a ReplicationOps create.
// ---------------------------------------------------------------------------
const P = window.SB_DB,
  PU = window.SB_UTIL;
const puuid = PU.uuid,
  pint = PU.int,
  ppick = PU.pick,
  pago = PU.ago;
const REPL_ANN = "storage.simplyblock.io/replication-policy";
const dns1123 = s => String(s).toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 253);

// The operator rounds the interval to whole minutes, floors it at one, and
// silently falls back to 5m on anything it cannot parse.
function normInterval(raw) {
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(String(raw || "").trim());
  if (!m) return {
    interval: "5m",
    minutes: 5,
    fellBack: true
  };
  const n = parseFloat(m[1]);
  const unit = m[2] || "m";
  const mins = unit === "s" ? n / 60 : unit === "h" ? n * 60 : unit === "d" ? n * 1440 : unit === "w" ? n * 10080 : n;
  const whole = Math.max(1, Math.round(mins));
  return {
    interval: whole >= 60 && whole % 60 === 0 ? `${whole / 60}h` : `${whole}m`,
    minutes: whole,
    fellBack: false
  };
}
const ivMin = iv => normInterval(iv).minutes;
P.replication_pairs = [];
P.replication_policies = [];
P.replication_slots = [];
P.replication_ops = [];

// ---- pairs -----------------------------------------------------------------
// Both clusters must be StorageClusters in this namespace, and the source's
// status.uuid must be populated before replication can be configured.
const eligible = () => P.clusters.filter(c => c.status !== "unready" && c.uuid);
(() => {
  const cs = eligible();
  if (cs.length < 2) return;
  const mk = (a, b) => ({
    uuid: puuid(),
    name: dns1123(`${a.name}-to-${b.name}`),
    source_cluster: a.name,
    target_cluster: b.name,
    source_cluster_id: a.uuid,
    target_cluster_id: b.uuid,
    ready: true,
    backend_target_id: puuid(),
    message: "Backend replication target available",
    active_ops_ref: null,
    created_at: pago(pint(400, 3000))
  });
  P.replication_pairs.push(mk(cs[0], cs[1]));
  if (cs.length > 2) {
    // fan-out and reverse are just more pairs; nothing in the CRD models a mesh
    P.replication_pairs.push(mk(cs[0], cs[2]));
    P.replication_pairs.push(mk(cs[1], cs[0]));
  }
  // one pair whose backend target has not come up, so "ready: false" is visible
  if (cs.length > 3) {
    const p = mk(cs[2], cs[3]);
    p.ready = false;
    p.backend_target_id = null;
    p.message = "Waiting for the backend replication target: target cluster unreachable";
    P.replication_pairs.push(p);
  }
})();
const pairBy = n => P.replication_pairs.find(p => p.name === n);

// ---- policies --------------------------------------------------------------
// mode is exactly failover | migration. There is no synchronous mode and no
// tiered retention: one interval, one snapshot count (minimum 2).
const POLICY_DEFS = [{
  suffix: "dr-5m",
  mode: "failover",
  interval: "5m",
  retention: 3
}, {
  suffix: "dr-1h",
  mode: "failover",
  interval: "1h",
  retention: 6
}, {
  suffix: "cutover",
  mode: "migration",
  interval: "15m",
  retention: 2
}];
P.replication_pairs.filter(p => p.ready).forEach((pair, pi) => {
  POLICY_DEFS.slice(0, pi === 0 ? 3 : 1).forEach(def => {
    P.replication_policies.push({
      uuid: puuid(),
      name: dns1123(`${pair.name}-${def.suffix}`),
      pair_ref: pair.name,
      mode: def.mode,
      interval: def.interval,
      snapshot_retention: def.retention,
      ready: true,
      backend_policy_id: puuid(),
      slot_count: 0,
      active_ops_ref: null,
      message: "Backend replication policy created",
      created_at: pago(pint(200, 2000))
    });
  });
});
const polBy = n => P.replication_policies.find(p => p.name === n);

// ---- slots -----------------------------------------------------------------
// One per bound PVC that carries the annotation. Owned by the PVC, so the
// console never creates or deletes one directly.
function slotFor(pol, pvc, lvol, state) {
  const pair = pairBy(pol.pair_ref) || {};
  const mins = ivMin(pol.interval);
  const lagMin = state === "replicating" ? mins * (0.1 + Math.random() * 0.8) : mins * 2;
  // <policy>-<pvc> collides when two namespaces use the same claim name, and
  // every slot lives in the one operator namespace — so qualify it.
  const dup = (P.pvcs || []).filter(x => x.pvc_name === pvc.pvc_name).length > 1;
  return {
    uuid: puuid(),
    name: dns1123(`${pol.name}-${dup ? pvc.namespace + "-" : ""}${pvc.pvc_name}`),
    policy_ref: pol.name,
    pvc_ref: pvc.pvc_name,
    pvc_namespace: pvc.namespace,
    // spec.volumeID is <clusterUUID>:<poolUUID>:<volumeUUID>
    volume_id: `${pair.source_cluster_id || ""}:${(lvol || {}).pool_id || ""}:${(lvol || {}).uuid || ""}`,
    lvol_id: (lvol || {}).uuid || null,
    state,
    direction: "source",
    source_lvol_id: (lvol || {}).uuid || null,
    target_lvol_id: state === "failed_over" || state === "cutover_done" ? puuid() : null,
    target_nqn: state === "failed_over" ? `nqn.2024-05.io.simplyblock:${puuid().slice(0, 8)}` : null,
    last_replicated_at: state === "error" ? pago(pint(3, 20)) : pago(lagMin / 60),
    message: state === "error" ? "Backend refused the replication snapshot: target pool out of capacity" : state === "cutover_pending" ? "Final delta transferred, waiting for cutover commit" : state === "failed_over" ? "Target volume promoted, source is no longer authoritative" : "Replicating on schedule",
    // the operator polls every 60s while replicating and backs off to 30s
    poll_interval_seconds: state === "error" ? 30 : 60,
    created_at: pago(pint(100, 1500))
  };
}
(() => {
  const pvcs = (P.pvcs || []).filter(p => p.status === "Bound");
  let cursor = 0;
  P.replication_policies.forEach((pol, i) => {
    const take = i === 0 ? 4 : 2;
    for (let k = 0; k < take && cursor < pvcs.length; k++, cursor++) {
      const pvc = pvcs[cursor];
      const lvol = (P.lvols || []).find(v => v.uuid === pvc.lvol_id) || (P.lvols || [])[cursor] || null;
      if (!lvol) continue;
      // one error slot and one cutover_pending slot so both states are real
      const state = i === 0 && k === 3 ? "error" : pol.mode === "migration" && k === 0 ? "cutover_pending" : "replicating";
      const slot = slotFor(pol, pvc, lvol, state);
      P.replication_slots.push(slot);
      // the annotation is the source of truth for membership
      pvc.annotations = Object.assign({}, pvc.annotations, {
        [REPL_ANN]: pol.name
      });
      pvc.replication_policy = pol.name;
      pvc.replication_slot = slot.name;
    }
  });
})();

// ---- a completed and a running operation ----------------------------------
(() => {
  const pol = P.replication_policies[0];
  if (!pol) return;
  const slots = P.replication_slots.filter(s => s.policy_ref === pol.name);
  P.replication_ops.push({
    uuid: puuid(),
    name: dns1123(`failback-${pol.name}-1`),
    action: "failback",
    scope: "policy",
    ref: pol.name,
    source_cluster_id: null,
    delete_source: false,
    phase: "Succeeded",
    subphase: "",
    message: "Failback completed for 3 of 4 volumes",
    started_at: pago(20),
    completed_at: pago(19.6),
    results: slots.map((s, i) => ({
      slot_ref: s.name,
      status: i === 3 ? "failed" : "succeeded",
      detail: i === 3 ? "Backend rejected the failback commit: source volume is still attached" : "",
      target_lvol_id: null
    })),
    started_ms: null,
    created_at: pago(20)
  });
  // one that ends Failed overall, because a single volume failed — the CRD
  // reports per-volume outcomes independently
  P.replication_ops[0].phase = "Failed";
})();

// ---- k8s projections -------------------------------------------------------
const RNS = () => window.SB_CONFIG.namespace || "simplyblock";
const RAPI = () => window.API_GROUP;
const meta = (name, uid, at) => ({
  name,
  namespace: RNS(),
  uid,
  creationTimestamp: at
});
const pairToK8s = p => ({
  apiVersion: RAPI(),
  kind: "ReplicationPair",
  metadata: meta(p.name, p.uuid, p.created_at),
  spec: {
    sourceCluster: p.source_cluster,
    targetCluster: p.target_cluster
  },
  status: {
    ready: p.ready,
    backendTargetID: p.backend_target_id,
    message: p.message,
    activeOpsRef: p.active_ops_ref || undefined,
    conditions: [{
      type: "Ready",
      status: p.ready ? "True" : "False",
      reason: p.ready ? "TargetAvailable" : "TargetUnavailable",
      message: p.message,
      lastTransitionTime: p.created_at
    }]
  }
});
const polToK8s = p => ({
  apiVersion: RAPI(),
  kind: "ReplicationPolicy",
  metadata: meta(p.name, p.uuid, p.created_at),
  spec: {
    pairRef: p.pair_ref,
    mode: p.mode,
    interval: p.interval,
    snapshotRetention: p.snapshot_retention
  },
  status: {
    ready: p.ready,
    backendPolicyID: p.backend_policy_id,
    slotCount: P.replication_slots.filter(s => s.policy_ref === p.name).length,
    activeOpsRef: p.active_ops_ref || undefined,
    conditions: [{
      type: "Ready",
      status: p.ready ? "True" : "False",
      reason: p.ready ? "PolicyCreated" : "PolicyPending",
      message: p.message,
      lastTransitionTime: p.created_at
    }]
  }
});
const slotToK8s = s => ({
  apiVersion: RAPI(),
  kind: "ReplicationSlot",
  metadata: Object.assign(meta(s.name, s.uuid, s.created_at), {
    // owned by its PVC: deleting the PVC cascades to the slot
    ownerReferences: [{
      apiVersion: "v1",
      kind: "PersistentVolumeClaim",
      name: s.pvc_ref,
      uid: s.uuid,
      controller: true,
      blockOwnerDeletion: true
    }]
  }),
  spec: {
    policyRef: s.policy_ref,
    pvcRef: s.pvc_ref,
    volumeID: s.volume_id
  },
  status: {
    state: s.state,
    direction: s.direction,
    sourceLvolID: s.source_lvol_id,
    targetLvolID: s.target_lvol_id || undefined,
    targetNQN: s.target_nqn || undefined,
    lastReplicatedAt: s.last_replicated_at,
    message: s.message,
    conditions: [{
      type: "Replicating",
      status: s.state === "replicating" ? "True" : "False",
      reason: s.state,
      message: s.message,
      lastTransitionTime: s.last_replicated_at
    }]
  }
});
const replOpsToK8s = o => ({
  apiVersion: RAPI(),
  kind: "ReplicationOps",
  metadata: meta(o.name, o.uuid, o.created_at),
  spec: Object.assign({
    action: o.action,
    scope: o.scope,
    ref: o.ref
  }, o.source_cluster_id ? {
    sourceClusterID: o.source_cluster_id
  } : {}, o.delete_source ? {
    deleteSource: true
  } : {}),
  status: {
    phase: o.phase,
    subphase: o.subphase || undefined,
    message: o.message,
    startedAt: o.started_at || undefined,
    completedAt: o.completed_at || undefined,
    results: (o.results || []).map(r => ({
      slotRef: r.slot_ref,
      status: r.status,
      detail: r.detail || undefined,
      targetLvolID: r.target_lvol_id || undefined
    }))
  }
});

// ---- create / delete with the operator's own rules -------------------------
const rerr = (msg, reason) => ({
  err: msg,
  reason: reason || "Invalid"
});
function createPair(body) {
  const s = body.spec || {};
  const name = (body.metadata || {}).name || dns1123(`${s.sourceCluster}-to-${s.targetCluster}`);
  if (!s.sourceCluster || !s.targetCluster) return rerr("spec.sourceCluster and spec.targetCluster are required");
  if (s.sourceCluster === s.targetCluster) return rerr("A pair cannot replicate a cluster to itself");
  const src = P.clusters.find(c => c.name === s.sourceCluster);
  if (!src) return rerr(`No StorageCluster named ${s.sourceCluster} in this namespace. Cross-namespace references are not supported.`, "NotFound");
  if (!src.uuid) return rerr(`StorageCluster ${s.sourceCluster} has no status.uuid yet — wait for it to be registered with the control plane`, "Conflict");
  const tgt = P.clusters.find(c => c.name === s.targetCluster || c.uuid === s.targetCluster);
  if (!tgt) return rerr(`No StorageCluster named ${s.targetCluster} in this namespace. Both clusters must be attached to the same control plane.`, "NotFound");
  if (pairBy(name)) return rerr(`ReplicationPair "${name}" already exists`, "AlreadyExists");
  const rec = {
    uuid: puuid(),
    name,
    source_cluster: src.name,
    target_cluster: tgt.name,
    source_cluster_id: src.uuid,
    target_cluster_id: tgt.uuid,
    ready: true,
    backend_target_id: puuid(),
    message: "Backend replication target created",
    active_ops_ref: null,
    created_at: pago(0)
  };
  P.replication_pairs.push(rec);
  return {
    obj: pairToK8s(rec)
  };
}
function createPolicy(body) {
  const s = body.spec || {};
  const name = (body.metadata || {}).name;
  if (!name) return rerr("metadata.name is required");
  if (!s.pairRef) return rerr("spec.pairRef is required");
  const pair = pairBy(s.pairRef);
  if (!pair) return rerr(`No ReplicationPair named ${s.pairRef}`, "NotFound");
  if (polBy(name)) return rerr(`ReplicationPolicy "${name}" already exists`, "AlreadyExists");
  const mode = s.mode || "failover";
  if (!["failover", "migration"].includes(mode)) return rerr(`spec.mode must be failover or migration (got "${mode}"). There is no synchronous replication mode.`);
  const ret = s.snapshotRetention == null ? 3 : Number(s.snapshotRetention);
  if (!(ret >= 2)) return rerr("spec.snapshotRetention has a minimum of 2");
  const iv = normInterval(s.interval || "5m");
  const rec = {
    uuid: puuid(),
    name,
    pair_ref: pair.name,
    mode,
    interval: iv.interval,
    snapshot_retention: ret,
    ready: true,
    backend_policy_id: puuid(),
    slot_count: 0,
    active_ops_ref: null,
    message: iv.fellBack ? `spec.interval could not be parsed and fell back to 5m` : "Backend replication policy created",
    created_at: pago(0)
  };
  P.replication_policies.push(rec);
  return {
    obj: polToK8s(rec)
  };
}

// A one-shot operation. The webhook checks that ref resolves to the kind scope
// names, that failback is not target-scoped, and that nothing else holds the
// lock; a terminal op is never re-run.
const OPS_SUBPHASES = {
  failover: ["TriggeringFailover", "UpdatingSlotStatuses", "ReleasingLock"],
  failback: ["StartingFailback", "CommittingFailback", "UpdatingSlotStatuses", "ReleasingLock"],
  migration: ["TriggeringTargetFailover", "UpdatingSlotStatuses", "ReleasingLock"]
};
function slotsInScope(scope, ref) {
  if (scope === "volume") return P.replication_slots.filter(s => s.name === ref);
  if (scope === "policy") return P.replication_slots.filter(s => s.policy_ref === ref);
  const pols = P.replication_policies.filter(p => p.pair_ref === ref).map(p => p.name);
  return P.replication_slots.filter(s => pols.includes(s.policy_ref));
}
function createReplOps(body) {
  const s = body.spec || {};
  const name = (body.metadata || {}).name || dns1123(`${s.action}-${s.ref}-${Date.now().toString(36)}`);
  if (!["failover", "failback", "migration"].includes(s.action)) return rerr("spec.action must be one of failover, failback, migration");
  if (!["target", "policy", "volume"].includes(s.scope)) return rerr("spec.scope must be one of target, policy, volume");
  if (!s.ref) return rerr("spec.ref is required");
  // the validating webhook resolves ref against the kind the scope names
  const kindFor = {
    target: "ReplicationPair",
    policy: "ReplicationPolicy",
    volume: "ReplicationSlot"
  }[s.scope];
  const found = s.scope === "target" ? pairBy(s.ref) : s.scope === "policy" ? polBy(s.ref) : P.replication_slots.find(x => x.name === s.ref);
  if (!found) return rerr(`spec.ref "${s.ref}" does not resolve to a ${kindFor}`, "Invalid");
  // failback is policy- or volume-scoped only; a pair-wide failback is done one
  // policy at a time
  if (s.action === "failback" && s.scope === "target") return rerr("Failback supports only policy and volume scope. Fail back one policy at a time.");
  // locks: policy-scoped locks the policy, target-scoped locks the pair
  const holder = s.scope === "target" ? found : s.scope === "policy" ? found : polBy(found.policy_ref);
  if (holder && holder.active_ops_ref) return rerr(`${holder.name} already has ${holder.active_ops_ref} in flight. A second operation waits for the lock.`, "Conflict");
  const affected = slotsInScope(s.scope, s.ref);
  if (!affected.length) return rerr(`No ReplicationSlot is in scope for ${s.scope}=${s.ref}, so there is nothing to ${s.action}`);
  const rec = {
    uuid: puuid(),
    name,
    action: s.action,
    scope: s.scope,
    ref: s.ref,
    source_cluster_id: s.sourceClusterID || null,
    delete_source: s.action === "migration" ? !!s.deleteSource : false,
    phase: "Running",
    subphase: OPS_SUBPHASES[s.action][0],
    message: `${s.action} started for ${affected.length} volume(s)`,
    started_at: pago(0),
    completed_at: null,
    results: [],
    slot_names: affected.map(x => x.name),
    started_ms: Date.now(),
    step: 0,
    created_at: pago(0)
  };
  P.replication_ops.push(rec);
  if (holder) holder.active_ops_ref = rec.name;
  return {
    obj: replOpsToK8s(rec)
  };
}
function deletePair(name) {
  const p = pairBy(name);
  if (!p) return rerr(`ReplicationPair "${name}" not found`, "NotFound");
  const pols = P.replication_policies.filter(x => x.pair_ref === name);
  if (pols.length) return rerr(`${pols.length} ReplicationPolicy resource(s) still reference this pair (${pols.map(x => x.name).join(", ")}). Delete them first.`, "Conflict");
  P.replication_pairs = P.replication_pairs.filter(x => x.name !== name);
  return {
    obj: {
      status: "Success",
      details: {
        name,
        kind: "replicationpairs"
      }
    }
  };
}
function deletePolicy(name) {
  const p = polBy(name);
  if (!p) return rerr(`ReplicationPolicy "${name}" not found`, "NotFound");
  const slots = P.replication_slots.filter(x => x.policy_ref === name);
  if (slots.length) return rerr(`${slots.length} ReplicationSlot resource(s) still reference this policy. Remove the ${REPL_ANN} annotation from their PVCs first.`, "Conflict");
  P.replication_policies = P.replication_policies.filter(x => x.name !== name);
  return {
    obj: {
      status: "Success",
      details: {
        name,
        kind: "replicationpolicies"
      }
    }
  };
}

// The console's only membership control: write or clear the PVC annotation.
// Repointing is a detach plus a fresh attach, so the new target takes a full copy.
function setPvcPolicy(pvcName, policyName) {
  const pvc = (P.pvcs || []).find(p => p.pvc_name === pvcName);
  if (!pvc) return rerr(`No PersistentVolumeClaim named ${pvcName}`, "NotFound");
  const prev = pvc.annotations && pvc.annotations[REPL_ANN];
  if (!policyName) {
    // removing the annotation deletes the replication snapshots on both sides,
    // then removes the slot
    P.replication_slots = P.replication_slots.filter(s => s.pvc_ref !== pvcName);
    if (pvc.annotations) delete pvc.annotations[REPL_ANN];
    pvc.replication_policy = null;
    pvc.replication_slot = null;
    return {
      obj: {
        status: "Success",
        detached: prev || null
      }
    };
  }
  const pol = polBy(policyName);
  if (!pol) return rerr(`No ReplicationPolicy named ${policyName}`, "NotFound");
  if (pvc.status !== "Bound") return rerr(`${pvcName} is ${pvc.status}; a slot is created once the PVC is Bound`, "Conflict");
  P.replication_slots = P.replication_slots.filter(s => s.pvc_ref !== pvcName);
  const lvol = (P.lvols || []).find(v => v.uuid === pvc.lvol_id) || null;
  const slot = slotFor(pol, pvc, lvol, "replicating");
  if (P.replication_slots.some(x => x.name === slot.name)) return rerr(`ReplicationSlot "${slot.name}" already exists`, "AlreadyExists");
  // a fresh attach starts from a full copy, so nothing has replicated yet
  slot.last_replicated_at = null;
  slot.message = prev && prev !== policyName ? `Re-attached from ${prev}: taking a full copy to the new target` : "Attached: taking the initial full copy";
  P.replication_slots.push(slot);
  pvc.annotations = Object.assign({}, pvc.annotations, {
    [REPL_ANN]: policyName
  });
  pvc.replication_policy = policyName;
  pvc.replication_slot = slot.name;
  return {
    obj: slotToK8s(slot)
  };
}

// ---- tick ------------------------------------------------------------------
// Ops walk their subphases and write per-volume results; slots advance their
// lastReplicatedAt on the operator's 60s poll.
const OPS_STEP_MS = 2600;
function tickRepl() {
  const now = Date.now();
  P.replication_ops.forEach(o => {
    if (o.phase !== "Running" || !o.started_ms) return;
    const subs = OPS_SUBPHASES[o.action] || [];
    const step = Math.min(subs.length, Math.floor((now - o.started_ms) / OPS_STEP_MS));
    if (step >= subs.length) {
      const slots = (o.slot_names || []).map(n => P.replication_slots.find(s => s.name === n)).filter(Boolean);
      // per-volume outcomes are independent: one failure does not stop the rest,
      // but the operation as a whole ends Failed
      o.results = slots.map((s, i) => {
        const fail = s.state === "error";
        if (!fail) {
          if (o.action === "failover") {
            s.state = "failed_over";
            s.direction = "target";
            s.target_lvol_id = s.target_lvol_id || puuid();
            s.target_nqn = `nqn.2024-05.io.simplyblock:${puuid().slice(0, 8)}`;
            s.message = "Target volume promoted, source is no longer authoritative";
          } else if (o.action === "failback") {
            s.state = "replicating";
            s.direction = "source";
            s.target_nqn = null;
            s.last_replicated_at = pago(0);
            s.message = "Source restored as primary";
          } else {
            s.state = "cutover_done";
            s.direction = "target";
            s.message = o.delete_source ? "Cutover committed, source volume deleted" : "Cutover committed";
          }
        }
        return {
          slot_ref: s.name,
          status: fail ? "failed" : "succeeded",
          detail: fail ? "Backend rejected the operation for this volume: " + s.message : "",
          target_lvol_id: o.action === "failover" && !fail ? s.target_lvol_id : null
        };
      });
      const failed = o.results.filter(r => r.status === "failed").length;
      o.phase = failed ? "Failed" : "Succeeded";
      o.subphase = "";
      o.message = failed ? `${o.action} completed for ${o.results.length - failed} of ${o.results.length} volume(s); ${failed} failed` : `${o.action} completed for ${o.results.length} volume(s)`;
      o.completed_at = pago(0);
      o.started_ms = null;
      // release the lock
      const holder = o.scope === "target" ? pairBy(o.ref) : o.scope === "policy" ? polBy(o.ref) : polBy((P.replication_slots.find(s => s.name === o.ref) || {}).policy_ref);
      if (holder && holder.active_ops_ref === o.name) holder.active_ops_ref = null;
      return;
    }
    o.subphase = subs[step];
    if (o.action === "failback" && subs[step] === "CommittingFailback") o.message = "Holding a short write freeze while the final delta transfers";
  });
  // the poll that writes lastReplicatedAt
  P.replication_slots.forEach(s => {
    if (s.state !== "replicating" || !s.last_replicated_at) return;
    const pol = polBy(s.policy_ref);
    if (!pol) return;
    const ageMin = (Date.parse(pago(0)) - Date.parse(s.last_replicated_at)) / 60000;
    if (ageMin >= ivMin(pol.interval)) s.last_replicated_at = pago(0);
  });
}
setInterval(tickRepl, 1200);

// ---- registration ----------------------------------------------------------
window.TO_K8S.ReplicationPair = pairToK8s;
window.TO_K8S.ReplicationPolicy = polToK8s;
window.TO_K8S.ReplicationSlot = slotToK8s;
window.TO_K8S.ReplicationOps = replOpsToK8s;
window.COLL.ReplicationPair = ["replication_pairs"];
window.COLL.ReplicationPolicy = ["replication_policies"];
window.COLL.ReplicationSlot = ["replication_slots"];
window.COLL.ReplicationOps = ["replication_ops"];
window.CRD_CREATE = Object.assign(window.CRD_CREATE || {}, {
  ReplicationPair: createPair,
  ReplicationPolicy: createPolicy,
  ReplicationOps: createReplOps
});
window.CRD_DELETE = Object.assign(window.CRD_DELETE || {}, {
  ReplicationPair: deletePair,
  ReplicationPolicy: deletePolicy
});
window.SB_REPL = {
  setPvcPolicy,
  normInterval,
  ivMin,
  REPL_ANN,
  tickRepl,
  slotsInScope,
  dns1123
};
})();
// ---- mock-dr.jsx ----
(function(){
// ---------------------------------------------------------------------------
// DR — TARGET ARCHITECTURE FIXTURES
//
// Two authored objects, everything below them derived:
//
//   ProtectionPlan        sites + storageProfile + methods[]        cluster-scoped
//   ProtectedApplication  planRef + pvcSelector + consistencyGroup + preferredSite
//                         + orchestratedMethod                     namespaced
//
// A method is one declared protection relationship — sync, async or
// snapshot-s3. All three are configured identically and travel the same control
// path; only the class parameters the driver receives differ. From the plan the
// orchestrator derives DRCluster (one per site), DRPolicy (one per pair ×
// interval, all created up front because every field is immutable),
// DRPlacementControl (one per application) and the VRClass / VGRClass matrix.
//
// Six payloads have no field anywhere in the Ramen API and travel a side
// channel: peering, generation selection, retention/lock enforcement, the
// generation catalogue, per-leg lag, and the arbitration token. Two of them are
// reads the console cannot do without — without payloads 4 and 5 a plan with
// three declared methods looks like a healthy single-leg posture with no
// generations.
// ---------------------------------------------------------------------------
const R = window.SB_DB,
  RU = window.SB_UTIL;
const ruuid = RU.uuid,
  rint = RU.int,
  rpick = RU.pick,
  rago = RU.ago;
const rnd2 = Math.random;

// ---- sites: one per managed cluster ---------------------------------------
// A site name must equal the OCM ManagedCluster name — it is the identity Ramen
// keys DRCluster on. The region is how sync and async are declared: equal
// region means the pair can mirror synchronously, distinct region cannot.
R.dr_sites = [];
R.k8s_clusters.forEach(kc => {
  const zone = R.zones.find(z => z.uuid === (kc.zone_ids || [])[0]) || {};
  R.dr_sites.push({
    uuid: ruuid(),
    k8s_cluster_id: kc.uuid,
    name: kc.name,
    region: zone.region || "unknown",
    s3_profile_name: `s3-${kc.name.replace(/^k8s-/, "")}`,
    s3_endpoint: `https://s3.${zone.region || "eu-central-1"}.amazonaws.com`,
    s3_bucket: `ramen-metadata-${kc.name.replace(/^k8s-/, "")}`,
    cidrs: [`10.${rint(10, 90)}.0.0/16`],
    // clusterFence is pair-scoped in Ramen; three sites need a quorum decision
    // it does not model, which is why the arbitration token is payload 6
    fencing_state: "Unfenced",
    status: kc.status === "online" ? "online" : kc.status,
    // DRClusterConfig discovery, reported up by the managed cluster
    discovered_classes: [],
    ramen_version: "v4.19.0-rc2",
    onboarded_at: rago(rint(600, 4000))
  });
});
const siteBy = n => R.dr_sites.find(s => s.name === n);
const siteById = id => R.dr_sites.find(s => s.uuid === id);

// ---- the class matrix, laid out at install time ---------------------------
// A single shared replicationID across the async family pre-validates every
// cluster pair at once, because Ramen matches replicationID pairwise. The
// synchronous class carries a second, pair-scoped identifier instead.
const RETENTION_DEFS = [{
  hourly: 24,
  daily: 14,
  weekly: 8
}, {
  hourly: 12,
  daily: 7,
  weekly: 4
}, {
  hourly: 48,
  daily: 30,
  weekly: 12
}];
function classFor(plan, m) {
  const base = {
    storage_class: plan.storage_profile,
    storage_id: `sb-${plan.name}`,
    provisioner: "csi.simplyblock.io"
  };
  if (m.type === "sync") return Object.assign(base, {
    name: `sb-sync-${m.name}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-sync-${plan.name}-${m.name}`,
    labels: {
      "sb.io/method": "sync"
    },
    parameters: {
      mode: "sync"
    }
  });
  if (m.type === "async") return Object.assign(base, {
    name: `sb-async-${m.interval}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-async-mesh`,
    labels: {
      "sb.io/method": "async"
    },
    parameters: {
      mode: "async",
      schedulingInterval: m.class_interval || m.interval
    }
  });
  return Object.assign(base, {
    name: `sb-vault-${m.interval}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-async-mesh`,
    labels: {
      "sb.io/method": "snapshot-s3"
    },
    parameters: {
      mode: "snapshot-s3",
      schedulingInterval: m.class_interval || m.interval,
      bucket: m.bucket,
      objectLock: m.immutable ? "compliance" : "none",
      retentionHourly: m.retention.hourly,
      retentionDaily: m.retention.daily,
      retentionWeekly: m.retention.weekly
    }
  });
}

// ---- derived Ramen objects ------------------------------------------------
// DRPolicy fields are immutable, so every pair a method could ever use is
// created at onboarding. Nothing may depend on editing one later.
function derivedPolicies(plan) {
  const out = [];
  const names = plan.site_names;
  names.forEach(a => names.forEach(b => {
    if (a === b) return;
    const sa = siteBy(a),
      sb = siteBy(b);
    if (!sa || !sb) return;
    const sync = sa.region === sb.region;
    const intervals = sync ? [null] : [...new Set(plan.methods.filter(m => m.type !== "sync").map(m => m.interval))];
    intervals.forEach(iv => {
      const method = plan.methods.find(m => m.target === b && (sync ? m.type === "sync" : m.interval === iv));
      // peerClass is what proves protection exists. It appears only when the
      // interval on the policy and the interval in the class parameters match
      // exactly, and the replicationID is present on both clusters.
      const cls = method ? classFor(plan, method) : null;
      const ivOk = !method || method.type === "sync" || (method.class_interval || method.interval) === method.interval;
      out.push({
        name: `sb-${a}-${b}${iv ? "-" + iv : ""}`,
        kind: "DRPolicy",
        dr_clusters: [a, b],
        scheduling_interval: iv,
        replication_class_selector: method ? {
          "sb.io/method": method.type
        } : {
          "sb.io/method": sync ? "sync" : "async"
        },
        method_name: method ? method.name : null,
        peer_class: method && ivOk ? {
          storageClassName: plan.storage_profile,
          replicationId: cls.replication_id,
          storageId: cls.storage_id,
          grouping: "VolumeGroupReplication"
        } : null,
        // documented failure mode: the policy validates cleanly, no class
        // resolves, and the application is protected by nothing
        validated: !!(method && ivOk) || !method,
        created_at: plan.created_at
      });
    });
  }));
  return out;
}

// ---- plans ----------------------------------------------------------------
R.protection_plans = [];
// Two sites in one region can mirror synchronously; a third in another region
// cannot, and takes the asynchronous and vault legs instead. Sites are named
// rather than sliced, because a synchronous method across regions is invalid.
const byRegion = {};
R.dr_sites.forEach(s => (byRegion[s.region] = byRegion[s.region] || []).push(s.name));
const metroPair = Object.values(byRegion).find(v => v.length >= 2) || [];
const remotes = R.dr_sites.filter(s => !metroPair.includes(s.name)).map(s => s.name);
const PLAN_DEFS = [{
  // the document's own example: a metro pair, a remote async leg, and a vault
  name: "gold",
  profile: "sb-nvme-gold",
  sites: [metroPair[0], metroPair[1], remotes[0]].filter(Boolean),
  methods: [{
    name: "metro",
    type: "sync",
    target: metroPair[1]
  }, {
    name: "regional",
    type: "async",
    target: remotes[0],
    interval: "5m"
  }, {
    name: "vault",
    type: "snapshot-s3",
    target: remotes[0],
    interval: "1h",
    retention: 0,
    immutable: true
  }]
}, {
  name: "silver",
  profile: "sb-nvme-standard",
  sites: [remotes[0], remotes[1]].filter(Boolean),
  methods: [{
    name: "regional",
    type: "async",
    target: remotes[1],
    interval: "15m"
  }, {
    name: "vault",
    type: "snapshot-s3",
    target: remotes[1],
    interval: "6h",
    retention: 1,
    immutable: true
  }]
}, {
  // vault only: generations in an object store and no live peer at all, so
  // the vault method is the orchestrated one in steady state
  name: "archive",
  profile: "sb-nvme-standard",
  sites: [remotes[1], remotes[2] || metroPair[0]].filter(Boolean),
  methods: [{
    name: "vault",
    type: "snapshot-s3",
    target: remotes[2] || metroPair[0],
    interval: "1d",
    retention: 2,
    immutable: false
  }]
}, {
  // deliberately broken: the interval on the policy and the interval in the
  // class parameters disagree, so no class resolves and nothing is protected
  name: "bronze",
  profile: "sb-nvme-standard",
  sites: [metroPair[0], remotes[1]].filter(Boolean),
  methods: [{
    name: "regional",
    type: "async",
    target: remotes[1],
    interval: "10m",
    classInterval: "10min"
  }]
}];
PLAN_DEFS.forEach(def => {
  const siteNames = (def.sites || []).filter(Boolean);
  const sites = siteNames.map(siteBy).filter(Boolean);
  if (sites.length < 2) return;
  const plan = {
    uuid: ruuid(),
    name: def.name,
    kind: "ProtectionPlan",
    storage_profile: def.profile,
    site_names: siteNames,
    site_ids: sites.map(s => s.uuid),
    methods: def.methods.filter(m => siteNames.includes(m.target)).map(m => ({
      name: m.name,
      type: m.type,
      target: m.target,
      interval: m.interval || null,
      // when these two disagree the derivation silently produces nothing
      class_interval: m.classInterval || m.interval || null,
      retention: m.type === "snapshot-s3" ? RETENTION_DEFS[m.retention || 0] : null,
      immutable: m.type === "snapshot-s3" ? !!m.immutable : false,
      bucket: m.type === "snapshot-s3" ? `sb-vault-${def.name}` : null
    })),
    created_at: rago(rint(400, 3000))
  };
  plan.classes = plan.methods.map(m => classFor(plan, m));
  plan.policies = derivedPolicies(plan);
  R.protection_plans.push(plan);
});
const planBy = n => R.protection_plans.find(p => p.name === n);

// ---- per-leg status: bypass payload 5 -------------------------------------
// Only the orchestrated method has a VRG, so only its RPO appears in
// DRPC.status. Every other declared method reports its lag up the side channel.
const ivMinutes = iv => {
  if (!iv) return 0;
  const n = parseInt(iv, 10),
    u = iv.replace(/[\d.]/g, "");
  return n * (u === "m" ? 1 : u === "h" ? 60 : u === "d" ? 1440 : u === "w" ? 10080 : 1);
};
function legFor(plan, m, orchestrated, broken, stale) {
  const mins = ivMinutes(m.interval);
  const sync = m.type === "sync";
  if (broken) return {
    leg_id: `${plan.name}/${m.name}`,
    method: m.name,
    type: m.type,
    target: m.target,
    state: "Unprotected",
    last_sync_at: null,
    lag_seconds: null,
    epoch: 0,
    health: "unhealthy",
    orchestrated,
    note: "No replication class resolved for this method, so nothing is being replicated."
  };
  const lagMin = sync ? 0 : stale ? mins * (2.6 + rnd2() * 3) : mins * (.15 + rnd2() * .7);
  return {
    leg_id: `${plan.name}/${m.name}`,
    method: m.name,
    type: m.type,
    target: m.target,
    state: "Replicating",
    last_sync_at: rago(lagMin / 60),
    lag_seconds: Math.round(lagMin * 60),
    epoch: sync ? null : rint(400, 9000),
    bytes_last_cycle: sync ? null : rint(40, 3800) * 1e6,
    health: stale ? "degraded" : "healthy",
    orchestrated,
    note: null
  };
}

// ---- generation catalogue: bypass payload 4 -------------------------------
// No Kubernetes object represents a generation. VRG status describes the
// current relationship only, so the catalogue is a side-channel read.
function generationsFor(m, cgName) {
  const out = [];
  const tiers = [{
    label: "hourly",
    every: 60,
    n: m.retention.hourly
  }, {
    label: "daily",
    every: 1440,
    n: m.retention.daily
  }, {
    label: "weekly",
    every: 10080,
    n: m.retention.weekly
  }];
  let gen = 1;
  tiers.slice().reverse().forEach(t => {
    for (let i = t.n - 1; i >= 0; i--) {
      out.push({
        generation: gen++,
        tier: t.label,
        at: rago(t.every * (i + 1) / 60),
        size_bytes: rint(300, 9000) * 1e6,
        integrity_state: "Verified",
        consistency_group: cgName || null,
        // Object Lock state is object-store state with no CR anywhere:
        // enforcement is payload 3
        locked_until: m.immutable ? rago(-(14 * 24) + t.every * (i + 1) / 60) : null
      });
    }
  });
  const kept = out.reverse().map((g, i) => Object.assign(g, {
    generation: out.length - i
  }));
  if (kept.length) {
    const base = kept[kept.length - 1];
    base.kind = "full";
    base.size_bytes = rint(9000, 44000) * 1e6;
  }
  kept.forEach(g => {
    if (!g.kind) g.kind = "delta";
  });
  // one generation in the middle failed verification, so the console has
  // something honest to show about integrity
  if (kept.length > 6) kept[3].integrity_state = "Unverified";
  return kept;
}

// ---- bind the existing protected applications to plans --------------------
// One DRPC per application is the hard limit: a DRPC selects PVCs by label, so
// two over the same PVCs would both claim them and the documented outcome is
// data corruption. orchestratedMethod names which method Ramen drives, and the
// rest run with identical parameters in the data plane.
let planCursor = 0;
(R.protected_apps || []).forEach((a, ai) => {
  const plan = R.protection_plans[planCursor % R.protection_plans.length];
  planCursor++;
  if (!plan) return;
  const broken = plan.name === "bronze";
  const stale = ai === 3;
  a.kind = "ProtectedApplication";
  a.plan_id = plan.uuid;
  a.plan_name = plan.name;
  a.storage_profile = plan.storage_profile;
  // the site the workload normally runs on, and where it is right now
  a.preferred_site = plan.site_names[0];
  a.active_site = a.phase === "FailedOver" ? plan.site_names[1] : plan.site_names[0];
  a.preferred_site_id = (siteBy(a.preferred_site) || {}).uuid || null;
  a.active_site_id = (siteBy(a.active_site) || {}).uuid || null;
  // Ramen drives exactly one method. The vault method is never the orchestrated
  // one in steady state — it becomes so only for the duration of a restore.
  const orch = plan.methods.find(m => m.type === "sync") || plan.methods.find(m => m.type === "async") || plan.methods[0];
  a.orchestrated_method = orch.name;
  a.legs = plan.methods.map(m => legFor(plan, m, m.name === orch.name, broken, stale && m.type !== "sync"));
  // the vault leg's catalogue, and the restore targets it makes possible
  const vault = plan.methods.find(m => m.type === "snapshot-s3");
  a.generations = vault ? generationsFor(vault, a.cg_name) : [];
  a.vault_method = vault ? vault.name : null;
  if (vault && a.generations.length) {
    const leg = a.legs.find(l => l.method === vault.name);
    if (leg) {
      leg.generations = a.generations.length;
      leg.oldest_generation_at = a.generations[a.generations.length - 1].at;
      leg.newest_generation_at = a.generations[0].at;
      leg.locked_until = a.generations[0].locked_until;
    }
  }
  // failover targets are every site with a live peer leg; restore targets are
  // the sites a vault generation can be materialised on
  a.failover_targets = plan.methods.filter(m => m.type !== "snapshot-s3").map(m => m.target);
  a.restore_targets = vault ? [vault.target] : [];
  // a vault restore has run: the source is gone or untrusted, so the original
  // site is no longer a failover target and the app is re-protected as new
  a.restored_from_generation = null;
  a.pvc_selector = a.pvc_selector || {
    matchLabels: {
      app: (a.app_name || "").split("-")[0]
    }
  };
  // payload 2: pinned immediately before the DRPC rebind, cleared after the
  // promote resolves — a stale pin would make an ordinary failover resolve to
  // an old generation
  a.pinned_generation = null;
  delete a.protection_mode;
  delete a.recovery_points;
  delete a.restore_point;
});

// ---- arbitration token: bypass payload 6 ----------------------------------
// DRCluster.clusterFence and NetworkFence are pair-scoped. Three sites need a
// quorum decision Ramen does not model at all.
R.dr_arbitration = {
  token_holder: (R.dr_sites[0] || {}).name || null,
  generation: rint(40, 900),
  quorum_ack: R.dr_sites.slice(0, 3).map(s => ({
    site: s.name,
    acked_at: rago(rnd2() * .4)
  })),
  fenced_sites: [],
  quorum_size: Math.min(3, R.dr_sites.length),
  updated_at: rago(rnd2() * .2)
};

// ---- rollup ---------------------------------------------------------------
function drRollup() {
  R.protection_plans.forEach(p => {
    const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
    p.apps_count = apps.length;
    p.pvcs_count = apps.reduce((n, a) => n + (a.pvcs_count || 0), 0);
    p.methods_count = p.methods.length;
    p.sites_count = p.site_names.length;
    p.policies_count = p.policies.length;
    p.unvalidated_count = p.policies.filter(x => !x.validated).length;
    // a plan whose methods do not all resolve a class is protecting nothing on
    // those legs, however healthy the rest of it looks
    p.protection_gap = p.methods.some(m => (m.class_interval || m.interval) !== m.interval);
    const legs = apps.flatMap(a => a.legs || []);
    p.health = p.protection_gap || legs.some(l => l.health === "unhealthy") ? "unhealthy" : legs.some(l => l.health === "degraded") ? "degraded" : "healthy";
    p.worst_lag_seconds = legs.reduce((n, l) => Math.max(n, l.lag_seconds || 0), 0);
    p.generations_total = apps.reduce((n, a) => n + (a.generations || []).length, 0);
  });
  R.dr_sites.forEach(s => {
    const apps = R.protected_apps || [];
    s.active_apps_count = apps.filter(a => a.active_site === s.name).length;
    s.standby_apps_count = apps.filter(a => a.active_site !== s.name && (a.failover_targets || []).concat(a.restore_targets || []).includes(s.name)).length;
    s.plans_count = R.protection_plans.filter(p => p.site_names.includes(s.name)).length;
    s.discovered_classes = [...new Set(R.protection_plans.filter(p => p.site_names.includes(s.name)).flatMap(p => p.classes.map(c => c.name)))];
  });
  (R.protected_apps || []).forEach(a => {
    const plan = R.protection_plans.find(p => p.uuid === a.plan_id);
    if (!plan) return;
    a.legs = (a.legs || []).map(l => Object.assign(l, {
      orchestrated: l.method === a.orchestrated_method
    }));
    const orch = a.legs.find(l => l.orchestrated);
    // the orchestrated leg's RPO is the only one Ramen reports; it is also what
    // DRPC.status.lastGroupSyncTime carries
    a.last_group_sync_at = orch && orch.last_sync_at ? orch.last_sync_at : a.last_group_sync_at;
    a.rpo_met = !a.legs.some(l => {
      const m = plan.methods.find(x => x.name === l.method);
      return m && m.interval && l.lag_seconds > ivMinutes(m.interval) * 60;
    });
    a.health = a.phase === "WaitForUser" ? "unhealthy" : a.legs.some(l => l.health === "unhealthy") ? "unhealthy" : a.legs.some(l => l.health === "degraded") || !a.rpo_met ? "degraded" : a.progression === "Completed" ? "healthy" : "degraded";
    a.legs_count = a.legs.length;
    a.generations_count = (a.generations || []).length;
  });
}
drRollup();
R.dr_rollup = drRollup;

// ---- routes ---------------------------------------------------------------
const rfail = (c, m) => new Response(JSON.stringify({
  status: false,
  error: m
}), {
  status: c,
  headers: {
    "Content-Type": "application/json"
  }
});
const appById = id => (R.protected_apps || []).find(a => a.uuid === id);
const planById = id => R.protection_plans.find(p => p.uuid === id);
const DR_ROUTES = [["GET", /^\/protection-plans$/, () => ({
  results: R.protection_plans
})], ["GET", /^\/protection-plans\/([\w-]+)$/, m => {
  const p = planById(m[1]);
  return p ? {
    results: [p]
  } : {
    __404: true
  };
}], ["GET", /^\/protection-plans\/([\w-]+)\/protected-apps$/, m => ({
  results: (R.protected_apps || []).filter(a => a.plan_id === m[1])
})], ["GET", /^\/protection-plans\/([\w-]+)\/sites$/, m => {
  const p = planById(m[1]);
  return {
    results: p ? p.site_names.map(siteBy).filter(Boolean) : []
  };
}], ["GET", /^\/dr-sites$/, () => ({
  results: R.dr_sites
})], ["GET", /^\/dr-sites\/([\w-]+)$/, m => {
  const s = siteById(m[1]);
  return s ? {
    results: [s]
  } : {
    __404: true
  };
}], ["GET", /^\/dr-sites\/([\w-]+)\/protected-apps$/, m => {
  const s = siteById(m[1]);
  if (!s) return {
    __404: true
  };
  return {
    results: (R.protected_apps || []).filter(a => a.active_site === s.name || (a.failover_targets || []).includes(s.name) || (a.restore_targets || []).includes(s.name))
  };
}], ["GET", /^\/dr-sites\/([\w-]+)\/protection-plans$/, m => {
  const s = siteById(m[1]);
  return {
    results: s ? R.protection_plans.filter(p => p.site_names.includes(s.name)) : []
  };
}],
// payload 4: the generation catalogue, per application
["GET", /^\/protected-apps\/([\w-]+)\/generations$/, m => {
  const a = appById(m[1]);
  return a ? {
    results: a.generations || []
  } : {
    __404: true
  };
}],
// payload 6: the arbitration token
["GET", /^\/dr-arbitration$/, () => ({
  results: [R.dr_arbitration]
})],
// ---- plan mutations ----
["POST", /^\/protection-plans$/, b => {
  const name = (b.name || "").trim();
  if (!name) return {
    __err: "Name the plan"
  };
  if (planBy(name)) return {
    __err: `A plan named ${name} already exists`
  };
  const siteNames = (b.site_names || []).filter(Boolean);
  if (siteNames.length < 2) return {
    __err: "A plan needs at least two sites"
  };
  const plan = {
    uuid: ruuid(),
    name,
    kind: "ProtectionPlan",
    storage_profile: b.storage_profile || "sb-nvme-standard",
    site_names: siteNames,
    site_ids: siteNames.map(n => (siteBy(n) || {}).uuid).filter(Boolean),
    methods: [],
    created_at: rago(0)
  };
  plan.classes = [];
  plan.policies = derivedPolicies(plan);
  R.protection_plans.push(plan);
  drRollup();
  return {
    results: [plan]
  };
}], ["POST", /^\/protection-plans\/([\w-]+)\/methods$/, (m, b) => {
  const p = planById(m[1]);
  if (!p) return {
    __404: true
  };
  const name = (b.name || "").trim();
  if (!name) return {
    __err: "Name the method"
  };
  if (p.methods.some(x => x.name === name)) return {
    __err: `${p.name} already has a method named ${name}`
  };
  const target = b.target;
  if (!p.site_names.includes(target)) return {
    __err: `${target} is not a site in this plan`
  };
  const src = siteBy(p.site_names[0]),
    tgt = siteBy(target);
  if (b.type === "sync" && src && tgt && src.region !== tgt.region) return {
    __err: `Synchronous replication needs both sites in one region — ${src.name} is in ${src.region} and ${tgt.name} in ${tgt.region}.`
  };
  if (b.type !== "sync" && !b.interval) return {
    __err: "An interval is required for asynchronous and vault methods"
  };
  const method = {
    name,
    type: b.type,
    target,
    interval: b.type === "sync" ? null : b.interval,
    // one field, emitted to both places, never configured independently
    class_interval: b.type === "sync" ? null : b.interval,
    retention: b.type === "snapshot-s3" ? {
      hourly: Number(b.retention_hourly) || 24,
      daily: Number(b.retention_daily) || 14,
      weekly: Number(b.retention_weekly) || 8
    } : null,
    immutable: b.type === "snapshot-s3" ? b.immutable !== false : false,
    bucket: b.type === "snapshot-s3" ? b.bucket || `sb-vault-${p.name}` : null
  };
  p.methods.push(method);
  p.classes = p.methods.map(x => classFor(p, x));
  p.policies = derivedPolicies(p);
  // every application on the plan gains the leg
  (R.protected_apps || []).filter(a => a.plan_id === p.uuid).forEach(a => {
    a.legs.push(legFor(p, method, false, false, false));
    if (method.type === "snapshot-s3") {
      a.generations = generationsFor(method, a.cg_name);
      a.vault_method = method.name;
      a.restore_targets = [method.target];
    } else if (!a.failover_targets.includes(method.target)) a.failover_targets.push(method.target);
  });
  drRollup();
  return {
    results: [p]
  };
}], ["DELETE", /^\/protection-plans\/([\w-]+)\/methods\/([\w-]+)$/, m => {
  const p = planById(m[1]);
  if (!p) return {
    __404: true
  };
  const name = m[2];
  const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
  if (apps.some(a => a.orchestrated_method === name)) return {
    __err: `${name} is the orchestrated method of ${apps.filter(a => a.orchestrated_method === name).length} application(s). Point them at another method first.`
  };
  p.methods = p.methods.filter(x => x.name !== name);
  p.classes = p.methods.map(x => classFor(p, x));
  p.policies = derivedPolicies(p);
  apps.forEach(a => {
    a.legs = a.legs.filter(l => l.method !== name);
  });
  drRollup();
  return {
    results: [p]
  };
}],
// fixing the interval writes it to both places at once, which is the whole
// point of the invariant
["PUT", /^\/protection-plans\/([\w-]+)\/methods\/([\w-]+)\/interval$/, (m, b) => {
  const p = planById(m[1]);
  if (!p) return {
    __404: true
  };
  const method = p.methods.find(x => x.name === m[2]);
  if (!method) return {
    __404: true
  };
  if (!b.interval) return {
    __err: "An interval is required"
  };
  method.interval = b.interval;
  method.class_interval = b.interval;
  p.classes = p.methods.map(x => classFor(p, x));
  p.policies = derivedPolicies(p);
  (R.protected_apps || []).filter(a => a.plan_id === p.uuid).forEach(a => {
    a.legs = a.legs.map(l => l.method === method.name ? legFor(p, method, l.orchestrated, false, false) : l);
  });
  drRollup();
  return {
    results: [p]
  };
}], ["DELETE", /^\/protection-plans\/([\w-]+)$/, m => {
  const p = planById(m[1]);
  if (!p) return {
    __404: true
  };
  const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
  if (apps.length) return {
    __err: `${apps.length} application(s) are still bound to ${p.name}. Unbind them first.`
  };
  R.protection_plans = R.protection_plans.filter(x => x.uuid !== p.uuid);
  drRollup();
  return {
    results: []
  };
}],
// ---- application mutations ----
["POST", /^\/protected-apps$/, (m, b) => {
  const p = planById(b.plan_id);
  if (!p) return {
    __err: "Choose a protection plan"
  };
  if (!p.methods.length) return {
    __err: `${p.name} declares no method, so it protects nothing`
  };
  const name = (b.app_name || "").trim(),
    ns = (b.namespace || "").trim();
  if (!name || !ns) return {
    __err: "Name the application and its namespace"
  };
  if ((R.protected_apps || []).some(a => a.app_name === name && a.namespace === ns)) return {
    __err: `${ns}/${name} is already protected`
  };
  const key = (b.pvc_selector_key || "").trim(),
    val = (b.pvc_selector_value || "").trim();
  // an empty selector claims every PVC in the namespace, and two placement
  // controls would then contend over the same volumes
  if (!key || !val) return {
    __err: "A PVC selector is required — an empty selector claims every PVC in the namespace"
  };
  const site = b.preferred_site && p.site_names.includes(b.preferred_site) ? b.preferred_site : p.site_names[0];
  const orch = p.methods.find(x => x.name === b.orchestrated_method) || p.methods.find(x => x.type !== "snapshot-s3") || p.methods[0];
  const vault = p.methods.find(x => x.type === "snapshot-s3");
  const a = {
    uuid: ruuid(),
    kind: "ProtectedApplication",
    app_name: name,
    namespace: ns,
    app_kind: b.app_kind || "ApplicationSet",
    plan_id: p.uuid,
    plan_name: p.name,
    storage_profile: p.storage_profile,
    preferred_site: site,
    active_site: site,
    preferred_site_id: (siteBy(site) || {}).uuid || null,
    active_site_id: (siteBy(site) || {}).uuid || null,
    orchestrated_method: orch.name,
    pvc_selector: {
      matchLabels: {
        [key]: val
      }
    },
    phase: "Deployed",
    progression: "Completed",
    action: null,
    vrg_state: "primary",
    kube_object_protection: b.kube_object_protection !== false,
    recipe: null,
    pvc_ids: [],
    pvcs_count: 0,
    volumes: [],
    legs: p.methods.map(x => legFor(p, x, x.name === orch.name, false, false)),
    generations: vault ? generationsFor(vault, null) : [],
    vault_method: vault ? vault.name : null,
    pinned_generation: null,
    restored_from_generation: null,
    failover_targets: p.methods.filter(x => x.type !== "snapshot-s3").map(x => x.target),
    restore_targets: vault ? [vault.target] : [],
    last_group_sync_at: rago(0),
    group_last_at: rago(0),
    group_written_since: 0,
    rpo_met: true,
    health: "healthy",
    created_at: rago(0)
  };
  R.protected_apps.push(a);
  drRollup();
  return {
    results: [a]
  };
}],
// rebinding the DRPC: delete + create, because DRPolicy fields are immutable
["PUT", /^\/protected-apps\/([\w-]+)\/orchestrated-method$/, (m, b) => {
  const a = appById(m[1]);
  if (!a) return {
    __404: true
  };
  const p = planById(a.plan_id);
  const method = p && p.methods.find(x => x.name === b.method);
  if (!method) return {
    __err: `${b.method} is not a method on ${p ? p.name : "this plan"}`
  };
  if (method.type === "snapshot-s3" && !b.allow_vault) return {
    __err: "The vault method becomes the orchestrated one only for the duration of a restore. Use Restore from a generation instead."
  };
  a.orchestrated_method = method.name;
  a.progression = "UpdatingPlacement";
  a.action_started_ms = Date.now();
  drRollup();
  return {
    results: [a]
  };
}],
// payload 2: pin the generation, then rebind. PromoteVolume has no
// point-in-time argument, so the driver reads the pin when the promote lands.
["POST", /^\/protected-apps\/([\w-]+)\/restore$/, (m, b) => {
  const a = appById(m[1]);
  if (!a) return {
    __404: true
  };
  const p = planById(a.plan_id);
  const vault = p && p.methods.find(x => x.type === "snapshot-s3");
  if (!vault) return {
    __err: `${p ? p.name : "This plan"} declares no vault method, so there are no generations to restore from.`
  };
  const gen = (a.generations || []).find(g => String(g.generation) === String(b.generation));
  if (!gen) return {
    __err: "That generation is not in the catalogue"
  };
  if (gen.integrity_state !== "Verified") return {
    __err: `Generation ${gen.generation} has not passed verification. Restoring from it may produce an unusable volume set.`
  };
  a.pinned_generation = gen.generation;
  a.orchestrated_method = vault.name;
  a.phase = "FailingOver";
  a.progression = "RestoringGeneration";
  a.action = "Failover";
  a.action_started_ms = Date.now();
  a.restore_target = vault.target;
  drRollup();
  return {
    results: [a]
  };
}],
// Fencing is pair-scoped in Ramen, so with three or more sites the decision
// belongs to the arbitration token rather than to the DRCluster alone.
["POST", /^\/dr-sites\/([\w-]+)\/fence$/, m => {
  const s = siteById(m[1]);
  if (!s) return {
    __404: true
  };
  s.fencing_state = "Fenced";
  if (!R.dr_arbitration.fenced_sites.includes(s.name)) R.dr_arbitration.fenced_sites.push(s.name);
  R.dr_arbitration.generation++;
  R.dr_arbitration.updated_at = rago(0);
  drRollup();
  return {
    results: [s]
  };
}], ["POST", /^\/dr-sites\/([\w-]+)\/unfence$/, m => {
  const s = siteById(m[1]);
  if (!s) return {
    __404: true
  };
  s.fencing_state = "Unfenced";
  R.dr_arbitration.fenced_sites = R.dr_arbitration.fenced_sites.filter(n => n !== s.name);
  R.dr_arbitration.generation++;
  R.dr_arbitration.updated_at = rago(0);
  drRollup();
  return {
    results: [s]
  };
}], ["POST", /^\/protected-apps\/([\w-]+)\/verify-generation$/, (m, b) => {
  const a = appById(m[1]);
  if (!a) return {
    __404: true
  };
  const gen = (a.generations || []).find(g => String(g.generation) === String(b.generation));
  if (!gen) return {
    __404: true
  };
  gen.integrity_state = "Verified";
  gen.verified_at = rago(0);
  return {
    results: [gen]
  };
}]];

// ---- fetch chain ----------------------------------------------------------
const drPrevFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfg = window.SB_CONFIG;
  if (!cfg.mock) return drPrevFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const base = cfg.operatorBase + "/proposed";
  if (!url.startsWith(base)) return drPrevFetch(input, init);
  // The client flattens a GET drill-down into /child?scope=<parent collection>
  // &scopeId=<uuid>, so fold it back into the nested form these routes are
  // written against. Anything not ours falls through to the next handler.
  const qi = url.indexOf("?");
  const params = new URLSearchParams(qi >= 0 ? url.slice(qi) : "");
  let route = url.slice(base.length).split("?")[0];
  const scope = params.get("scope"),
    scopeId = params.get("scopeId");
  if (scope && scopeId) {
    const child = route.replace(/^\//, "");
    const nest = {
      "protection-plans|protected-apps": "/protection-plans/" + scopeId + "/protected-apps",
      "protection-plans|sites": "/protection-plans/" + scopeId + "/sites",
      "dr-sites|protected-apps": "/dr-sites/" + scopeId + "/protected-apps",
      "dr-sites|protection-plans": "/dr-sites/" + scopeId + "/protection-plans",
      "protected-apps|generations": "/protected-apps/" + scopeId + "/generations"
    }[scope + "|" + child];
    if (nest) route = nest;
  }
  const method = ((init || {}).method || "GET").toUpperCase();
  let body = {};
  try {
    body = init && init.body ? JSON.parse(init.body) : {};
  } catch (e) {
    body = {};
  }
  for (const [verb, re, h] of DR_ROUTES) {
    if (verb !== method) continue;
    const mm = route.match(re);
    if (!mm) continue;
    window.SB_JITTER();
    const out = verb === "POST" && re.source === "^\\/protection-plans$" ? h(body) : h(mm, body);
    if (out.__404) return rfail(404, "not found");
    if (out.__err) return rfail(409, out.__err);
    return new Response(JSON.stringify({
      status: true,
      results: out.results
    }), {
      status: 200,
      headers: {
        "Content-Type": "application/json"
      }
    });
  }
  return drPrevFetch(input, init);
};
window.SB_DR = {
  drRollup,
  siteBy,
  siteById,
  planBy,
  ivMinutes,
  generationsFor,
  derivedPolicies,
  classFor
};
})();
// ---- mock-deploy.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK: DISCOVERY + CLUSTER DEPLOYMENT
//   helm install control plane          (outside this console)
//   install operator, connect to cp     (outside this console)
//   OperatorOps{Discover}               -> the Kubernetes cluster becomes "discovered":
//                                          nodes, free devices, NUMA, vCPU, RAM, NICs
//   ClusterDeploymentConfig (draft)     -> review -> approve
//   three asynchronous steps, per node where it makes sense, each with a log
// ---------------------------------------------------------------------------
const P = window.SB_DB,
  PU = window.SB_UTIL;
const puuid = PU.uuid,
  pint = PU.int,
  ppick = PU.pick,
  pago = PU.ago;
const PGB = 1e9,
  PTB = 1e12;
P.discoveries = [];
P.deployment_configs = [];
P.deployment_logs = {};
(P.k8s_clusters || []).forEach(k => {
  k.discovered = false;
  k.discovered_at = null;
});

// SPDK pre-allocates per-subsystem structures out of hugepage memory:
// 2 GB base + 256 MB per subsystem, rounded up to a 2 GB group.
const hugepagesFor = maxSubsystems => Math.ceil((2 + maxSubsystems * .25) / 2) * 2 * PGB;
window.hugepagesFor = hugepagesFor;

// ---- device filter ---------------------------------------------------------
// The same predicate runs at discovery (what is reported at all), on the
// cluster (the default selection) and per node (a narrower override).
const globRe = p => new RegExp("^" + p.trim().replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".") + "$");
function deviceMatches(d, f) {
  if (!f) return true;
  if (d.kind === "block" && f.enableLogicalBlockDevices === false) return false;
  if (f.deviceClass && d.kind !== f.deviceClass) return false;
  const pcie = d.pcie_address || "";
  const allow = (f.pcieAllowList || []).filter(Boolean);
  const deny = (f.pcieDenyList || []).filter(Boolean);
  if (allow.length && !allow.some(p => pcie.startsWith(p))) return false;
  if (deny.some(p => pcie && pcie.startsWith(p))) return false;
  const names = (f.blockDeviceNames || []).filter(Boolean);
  if (names.length && !names.some(n => n.includes("*") || n.includes("?") ? globRe(n).test(d.device_name || "") : (d.device_name || "").includes(n))) return false;
  const models = (f.models || []).filter(Boolean);
  if (models.length && !models.some(m => (d.model_number || "").toLowerCase().includes(m.toLowerCase()))) return false;
  const r = f.driveSizeRange || {};
  if (r.min && d.size < r.min) return false;
  if (r.max && d.size > r.max) return false;
  return true;
}
window.deviceMatches = deviceMatches;
const nodeMatches = (h, sel) => {
  const ml = (sel || {}).matchLabels || {};
  return Object.entries(ml).every(([k, v]) => ((h.k8s_labels || {})[k] || (h.labels || {})[k] || "") === v);
};
const DEFAULT_FILTER = () => ({
  pcieAllowList: [],
  pcieDenyList: [],
  blockDeviceNames: [],
  models: [],
  driveSizeRange: {
    min: 400 * PGB,
    max: 0
  },
  enableLogicalBlockDevices: true
});

// ---- discovery -------------------------------------------------------------
const DISCOVERY_STEPS = ["Scheduling inspection pods", "Collecting inventory", "Writing inventory"];
function startDiscovery(kid, spec, opName) {
  const rec = {
    uuid: puuid(),
    k8s_cluster_id: kid,
    op_name: opName || null,
    started_at: pago(0),
    finished_at: null,
    status: "running",
    step: DISCOVERY_STEPS[0],
    tick: 0,
    node_selector: spec && spec.nodeSelector || {
      matchLabels: {}
    },
    device_filter: spec && spec.deviceFilter || DEFAULT_FILTER(),
    host_ids: [],
    node_count: 0,
    device_count: 0,
    filtered_count: 0
  };
  P.discoveries = P.discoveries.filter(d => d.k8s_cluster_id !== kid || d.status === "complete");
  P.discoveries.unshift(rec);
  return rec;
}
function finishDiscovery(rec) {
  const hosts = P.hosts.filter(h => h.k8s_cluster_id === rec.k8s_cluster_id && !(h.storage_node_ids || []).length && nodeMatches(h, rec.node_selector));
  let devices = 0,
    filtered = 0;
  hosts.forEach(h => {
    if (h.status === "discovered" || !h.devices.length) PU.inspectHost(h);
    h.devices.forEach(d => {
      d.usable = deviceMatches(d, rec.device_filter) && !d.assigned_node_id;
      if (d.usable) devices++;else filtered++;
    });
    h.discovered_at = pago(0);
  });
  rec.host_ids = hosts.map(h => h.uuid);
  rec.node_count = hosts.length;
  rec.device_count = devices;
  rec.filtered_count = filtered;
  rec.status = "complete";
  rec.step = null;
  rec.started_ms = null;
  rec.finished_at = pago(0);
  const kc = P.k8s_clusters.find(k => k.uuid === rec.k8s_cluster_id);
  if (kc) {
    kc.discovered = true;
    kc.discovered_at = rec.finished_at;
  }
  PU.rollup();
}

// ---- the deployment document ----------------------------------------------
const DEPLOY_STEPS = [{
  k: "ConfigureNodes",
  label: "Configure worker nodes — hugepages, core isolation, reboot",
  perNode: true
}, {
  k: "AddStorageNodes",
  label: "Add storage nodes — deploy pods, configure SPDK",
  perNode: true
}, {
  k: "ActivateCluster",
  label: "Activate the cluster",
  perNode: false
}];
window.DEPLOY_STEPS = DEPLOY_STEPS;
const freshSteps = () => DEPLOY_STEPS.map(s => ({
  name: s.k,
  label: s.label,
  phase: "Pending",
  message: null,
  startedAt: null,
  finishedAt: null,
  progress: null
}));

// one storage node per selected NUMA socket
function groupsFor(host, hsel, cl) {
  const sockets = (hsel.sockets && hsel.sockets.length ? hsel.sockets : cl.numaSockets || [0]).filter(s => s < (host.numa_sockets || 1));
  const hp = cl.hugepagesOverride || hugepagesFor(cl.maxSubsystems || 128);
  return sockets.map(s => {
    const devs = host.devices.filter(d => d.numa_socket === s && (hsel.deviceIds || []).includes(d.id));
    return {
      numaSocket: s,
      devices: {
        nvme: devs.filter(d => d.kind === "nvme").map(d => d.pcie_address),
        block: devs.filter(d => d.kind === "block").map(d => d.device_name)
      },
      mgmtInterface: cl.mgmtNic || (host.nics[0] || {}).name || "eth0",
      dataInterfaces: (cl.dataNics || []).filter(Boolean),
      sizing: {
        maxSubsystemCount: cl.maxSubsystems || 128,
        vcpuCount: cl.vcpu || 8,
        minHugePagesSize: hp,
        systemMemory: (cl.systemMemoryGb || 32) * PGB
      },
      coreIsolation: !!cl.coreIsolation,
      failureDomain: cl.failureDomains ? host.rack_id || host.zone || "fd-1" : null
    };
  });
}
function mkConfig(o) {
  const kc = P.k8s_clusters.find(k => k.uuid === o.k8s_cluster_id) || {};
  const disc = P.discoveries.find(d => d.k8s_cluster_id === o.k8s_cluster_id && d.status === "complete");
  const cl = o.cluster || {};
  const hsels = (o.hosts || []).map(hs => Object.assign({}, hs, {
    host: P.hosts.find(h => h.uuid === hs.id)
  })).filter(x => x.host);
  const cfg = {
    uuid: puuid(),
    name: o.name,
    created_at: pago(o.ageHours || 0),
    k8s_cluster_id: o.k8s_cluster_id,
    cluster_id: o.cluster_id || null,
    spec: {
      approved: !!o.approved,
      environment: kc.environment || "Vanilla",
      kubernetesClusterRef: kc.name || null,
      nodeSelector: o.nodeSelector || {
        matchLabels: {}
      },
      cluster: cl,
      nodeSets: [{
        name: "default",
        nodes: hsels.map(x => x.host.hostname),
        groups: hsels.flatMap(x => groupsFor(x.host, x, cl).map(g => Object.assign({
          node: x.host.hostname
        }, g)))
      }]
    },
    status: {
      phase: o.phase || "Draft",
      message: o.message || null,
      kubernetesClusterId: o.k8s_cluster_id,
      clusterId: o.cluster_id || null,
      discoveryRef: disc ? disc.uuid : null,
      hostIds: hsels.map(x => x.host.uuid),
      steps: o.steps || freshSteps(),
      nodes: hsels.map(x => ({
        host: x.host.hostname,
        hostId: x.host.uuid,
        phase: "Pending",
        message: null,
        progress: 0,
        storageNodeIds: []
      })),
      log: [],
      observedGeneration: 1
    }
  };
  P.deployment_logs[cfg.uuid] = {};
  P.deployment_configs.push(cfg);
  return cfg;
}

// ---- logging ---------------------------------------------------------------
const nowIso = () => new Date().toISOString();
function glog(cfg, level, step, node, msg) {
  cfg.status.log.push({
    ts: nowIso(),
    level,
    step,
    node: node || null,
    msg
  });
  if (cfg.status.log.length > 400) cfg.status.log.shift();
}
function dlog(cfg, name, level, msg) {
  const L = P.deployment_logs[cfg.uuid] = P.deployment_logs[cfg.uuid] || {};
  (L[name] = L[name] || []).push({
    ts: nowIso(),
    level,
    msg
  });
}
const logName = (step, host) => host ? `${step}/${host}` : step;

// ---- seeds -----------------------------------------------------------------
(P.k8s_clusters || []).slice(0, 2).forEach(kc => {
  const rec = startDiscovery(kc.uuid, {
    deviceFilter: DEFAULT_FILTER()
  }, null);
  rec.started_at = pago(pint(20, 200));
  finishDiscovery(rec);
  rec.finished_at = rec.started_at;
  kc.discovered_at = rec.started_at;
});
const seedCluster = (name, dc) => ({
  name,
  deviceClass: dc,
  vcpu: 12,
  maxSubsystems: 128,
  hugepagesOverride: null,
  systemMemoryGb: 32,
  ec: "2+1",
  backups: true,
  objectStorage: false,
  failureDomains: true,
  coreIsolation: true,
  mgmtNic: "eno1",
  dataNics: ["ens1f0", "ens1f1"],
  numaSockets: [0, 1],
  deviceFilter: {
    deviceClass: dc,
    pcieAllowList: [],
    pcieDenyList: [],
    models: [],
    blockDeviceNames: [],
    driveSizeRange: {
      min: 400 * PGB,
      max: 0
    }
  }
});

// a deployed document — the record of how a live cluster was built
(function seedDeployed() {
  const kc = (P.k8s_clusters || [])[0];
  const cluster = P.clusters.find(c => c.status === "online");
  if (!kc || !cluster) return;
  const hosts = P.hosts.filter(h => h.cluster_id === cluster.uuid && h.storage_node_ids.length).slice(0, 3);
  const cfg = mkConfig({
    name: cluster.name + "-deployment",
    k8s_cluster_id: kc.uuid,
    cluster_id: cluster.uuid,
    approved: true,
    phase: "Deployed",
    ageHours: pint(900, 2000),
    hosts: hosts.map(h => ({
      id: h.uuid,
      sockets: [0, 1],
      deviceIds: h.devices.map(d => d.id)
    })),
    cluster: Object.assign(seedCluster(cluster.name, cluster.device_class), {
      ec: `${cluster.distr_ndcs}+${cluster.distr_npcs}`
    })
  });
  const t = cfg.created_at;
  cfg.status.steps.forEach(s => Object.assign(s, {
    phase: "Succeeded",
    startedAt: t,
    finishedAt: t,
    message: "completed",
    progress: 100
  }));
  cfg.status.nodes.forEach(n => Object.assign(n, {
    phase: "Added",
    message: "storage node online",
    progress: 100
  }));
  cfg.status.message = "Cluster deployed and activated";
  cfg.status.log = [{
    ts: t,
    level: "INFO",
    step: "ConfigureNodes",
    node: null,
    msg: `${hosts.length} worker node(s) configured`
  }, {
    ts: t,
    level: "INFO",
    step: "AddStorageNodes",
    node: null,
    msg: `${cfg.spec.nodeSets[0].groups.length} storage node(s) added`
  }, {
    ts: t,
    level: "INFO",
    step: "ActivateCluster",
    node: null,
    msg: "cluster active"
  }];
})();

// a draft awaiting approval
(function seedDraft() {
  const disc = P.discoveries.find(d => d.status === "complete" && d.host_ids.length >= 2);
  if (!disc) return;
  const hosts = disc.host_ids.slice(0, Math.min(4, disc.host_ids.length)).map(id => P.hosts.find(h => h.uuid === id));
  mkConfig({
    name: "prod-eu-west-3-deployment",
    k8s_cluster_id: disc.k8s_cluster_id,
    approved: false,
    phase: "Draft",
    ageHours: pint(1, 30),
    hosts: hosts.map(h => ({
      id: h.uuid,
      sockets: [0, 1],
      deviceIds: h.devices.filter(d => d.usable && d.kind === "nvme").map(d => d.id)
    })),
    cluster: seedCluster("prod-eu-west-3", "nvme"),
    message: "Awaiting approval. Nothing has been applied to any node yet."
  });
})();

// ---- deployment state machine ----------------------------------------------
// Approval creates the cluster record in the control plane (unready) and
// starts step 1. Steps 1 and 2 run per node, in parallel and staggered; each
// node and each step writes its own log.
function createClusterRecord(cfg) {
  const spec = cfg.spec.cluster || {};
  const tmpl = P.clusters.find(c => c.status === "online") || P.clusters[0];
  const c = JSON.parse(JSON.stringify(tmpl));
  const [nd, np] = String(spec.ec || "2+1").split("+").map(Number);
  Object.assign(c, {
    uuid: puuid(),
    name: spec.name || cfg.name,
    status: "unready",
    device_class: spec.deviceClass || "nvme",
    location_type: "datacenter",
    distr_ndcs: nd || 2,
    distr_npcs: np || 1,
    ha_type: "ha",
    cluster_version: "26.2.1",
    zone_ids: [],
    rebalancing: false,
    failure_domain_enabled: !!spec.failureDomains,
    failure_domain_scope: spec.failureDomains ? "rack" : null,
    file_storage: {
      enabled: false
    },
    object_storage: {
      enabled: !!spec.objectStorage
    },
    backup: Object.assign({}, c.backup || {}, {
      enabled: !!spec.backups
    }),
    auto_rebalance: Object.assign({}, c.auto_rebalance, {
      enabled: false,
      last_run_at: null,
      moves_last_run: 0
    }),
    created_at: nowIso(),
    mgmt_endpoint: `https://cp-${P.clusters.length + 1}.simplyblock.internal:5000`,
    size_total: 0,
    size_util: 0,
    storage_nodes_count: 0,
    storage_nodes_online: 0,
    devices_count: 0
  });
  P.clusters.push(c);
  cfg.cluster_id = c.uuid;
  cfg.status.clusterId = c.uuid;
  const kc = P.k8s_clusters.find(k => k.uuid === cfg.k8s_cluster_id);
  if (kc) kc.storage_cluster_ids = [...new Set((kc.storage_cluster_ids || []).concat(c.uuid))];
  glog(cfg, "INFO", "Approve", null, `Cluster ${c.name} created in the control plane (${c.uuid.slice(0, 8)}), status unready`);
  return c;
}
const CONFIGURE_PHASES = [{
  at: 0,
  phase: "Applying",
  msg: "writing sysctl and kernel parameters",
  lines: h => [`applying hugepage reservation: vm.nr_hugepages persisted via /etc/sysctl.d/90-simplyblock.conf`, `grub: adding hugepagesz=2M default_hugepagesz=2M to GRUB_CMDLINE_LINUX`, `tuned profile simplyblock-storage activated`]
}, {
  at: 3,
  phase: "Isolating",
  msg: "core isolation and CPU topology",
  lines: h => [`cpu topology: ${h.numa_sockets} NUMA node(s), ${h.vcpu_count} vCPU — enforced`, `isolcpus set for the storage node cores, kubelet reservedSystemCPUs updated`]
}, {
  at: 5,
  phase: "Rebooting",
  msg: "node rebooting to apply kernel parameters",
  lines: h => [`cordoning node ${h.hostname}`, `draining node ${h.hostname} (ignore-daemonsets, delete-emptydir-data)`, `reboot requested via privileged pod`]
}, {
  at: 10,
  phase: "Verifying",
  msg: "node back, verifying hugepages",
  lines: h => [`node ${h.hostname} Ready again after reboot`, `verified: HugePages_Total matches reservation`, `uncordoning node ${h.hostname}`]
}, {
  at: 12,
  phase: "Configured",
  msg: "configured",
  lines: () => [`node configuration complete`]
}];
const ADD_PHASES = [{
  at: 0,
  phase: "Scheduling",
  msg: "scheduling storage node pod",
  lines: (h, g) => [`creating pod simplyblock-storage-node-${h.hostname}-s${g.numaSocket}`, `pod scheduled to ${h.hostname}, NUMA socket ${g.numaSocket}`]
}, {
  at: 3,
  phase: "Starting SPDK",
  msg: "starting SPDK, binding devices",
  lines: (h, g) => [`spdk_tgt started with ${g.sizing.minHugePagesSize / PGB} GB hugepages, ${g.sizing.vcpuCount} cores`, ...(g.devices.nvme || []).map(a => `nvme attach ${a} → bdev ok`), ...(g.devices.block || []).map(a => `aio bdev ${a} → ok`)]
}, {
  at: 7,
  phase: "Joining",
  msg: "joining the cluster",
  lines: (h, g) => [`NVMe/TCP listener on ${g.dataInterfaces.join(", ") || "data nic"}:4420`, `registering storage node with the control plane`, `distrib layout: node accepted, devices reported`]
}, {
  at: 10,
  phase: "Added",
  msg: "storage node added",
  lines: () => [`storage node online — waiting for cluster activation`]
}];
const ACTIVATE_LINES = [`validating node set: all storage nodes online`, `building distribution map (erasure coding)`, `creating default storage pool`, `enabling NVMe/TCP subsystems`, `starting health monitor and task engine`, `cluster status → online`];
function runPerNode(cfg, step, phases, secsPer, stagger, onNodeDone) {
  const groups = (cfg.spec.nodeSets[0] || {}).groups || [];
  let allDone = true,
    sum = 0;
  cfg.status.nodes.forEach((n, i) => {
    const host = P.hosts.find(h => h.uuid === n.hostId);
    if (!n.__ms) n.__ms = Date.now() + i * stagger * 1000;
    const el = (Date.now() - n.__ms) / 1000;
    if (el < 0) {
      n.phase = "Pending";
      n.message = "queued";
      n.progress = 0;
      allDone = false;
      return;
    }
    const ph = phases.filter(p => el >= p.at).pop() || phases[0];
    const mine = groups.filter(g => g.node === n.host);
    if (n.__phase !== ph.phase) {
      n.__phase = ph.phase;
      n.phase = ph.phase;
      n.message = ph.msg;
      glog(cfg, "INFO", step, n.host, `${n.host}: ${ph.msg}`);
      const lines = step === "AddStorageNodes" ? mine.flatMap(g => ph.lines(host, g)) : ph.lines(host);
      lines.forEach(l => dlog(cfg, logName(step, n.host), "INFO", l));
      if (ph === phases[phases.length - 1] && onNodeDone) onNodeDone(n, host, mine);
    }
    n.progress = Math.min(100, Math.round(el / secsPer * 100));
    if (el < secsPer) allDone = false;
    sum += n.progress;
  });
  return {
    allDone,
    progress: cfg.status.nodes.length ? Math.round(sum / cfg.status.nodes.length) : 100
  };
}
function configureDone(cfg, n, h, mine) {
  const hp = mine.reduce((a, g) => a + g.sizing.minHugePagesSize, 0);
  Object.assign(h, {
    hugepages_reserved: hp,
    hugepages_allocated: 0,
    core_isolation: mine.some(g => g.coreIsolation),
    cpu_topology_enforced: true,
    status: "available",
    prepared_at: pago(0),
    cluster_id: cfg.cluster_id,
    inspection: null,
    labels: Object.assign({}, h.k8s_labels, {
      "simplyblock.io/storage-node": "true"
    })
  });
}
function addDone(cfg, n, h, mine) {
  const c = P.clusters.find(x => x.uuid === cfg.cluster_id);
  if (!c) return;
  const spec = cfg.spec.cluster || {};
  mine.forEach(g => {
    const idx = P.storage_nodes.filter(x => x.cluster_id === c.uuid).length + 1;
    const sn = {
      uuid: puuid(),
      cluster_id: c.uuid,
      host_id: h.uuid,
      hostname: `${(spec.name || cfg.name).split("-").slice(0, 2).join("-")}-stor-${String(idx).padStart(2, "0")}`,
      mgmt_ip: h.mgmt_ip,
      failure_domain: g.failureDomain,
      physical_label: h.cabinet_id || null,
      status: "in_restart",
      numa_socket: g.numaSocket,
      cpu_count: g.sizing.vcpuCount,
      vcpu_reserved: g.sizing.vcpuCount,
      max_subsystem_count: g.sizing.maxSubsystemCount,
      memory_total: g.sizing.systemMemory,
      memory_reserved: g.sizing.systemMemory,
      memory_used: Math.round(g.sizing.systemMemory * .3),
      hugepages_total: g.sizing.minHugePagesSize,
      hugepages_used: Math.round(g.sizing.minHugePagesSize * .45),
      spdk_version: "v24.09",
      core_isolation: g.coreIsolation,
      data_nics: g.dataInterfaces.map((nm, k) => ({
        name: nm,
        ip: `10.${pint(10, 60)}.${pint(0, 40)}.${pint(2, 250)}`,
        port: 4420 + k,
        numa_socket: g.numaSocket,
        state: "up"
      }))
    };
    P.storage_nodes.push(sn);
    h.storage_node_ids.push(sn.uuid);
    h.hugepages_allocated += g.sizing.minHugePagesSize;
    n.storageNodeIds.push(sn.uuid);
    const want = [].concat(g.devices.nvme || [], g.devices.block || []);
    h.devices.filter(d => want.includes(d.pcie_address) || want.includes(d.device_name)).forEach(hd => {
      hd.assigned_node_id = sn.uuid;
      P.devices.push({
        uuid: puuid(),
        node_id: sn.uuid,
        cluster_id: c.uuid,
        host_id: h.uuid,
        cluster_device_class: c.device_class,
        numa_socket: hd.numa_socket,
        serial_number: hd.serial_number,
        pcie_address: hd.pcie_address,
        device_name: hd.device_name,
        model_number: hd.model_number,
        firmware_revision: `GXA7${pint(10, 99)}1`,
        status: "new",
        health_check: null,
        size_total: hd.size,
        size_util: 0,
        temperature_c: pint(31, 48),
        percentage_used: 0,
        power_on_hours: pint(20, 400),
        io_stats: PU.ioStats(0, 0),
        io_history: {
          iops: PU.series(0, 0),
          bytes: PU.series(0, 0)
        }
      });
    });
  });
  PU.rollup();
}
function activate(cfg) {
  const c = P.clusters.find(x => x.uuid === cfg.cluster_id);
  if (!c) return;
  c.status = "online";
  P.storage_nodes.filter(n => n.cluster_id === c.uuid).forEach(n => {
    n.status = "online";
  });
  P.devices.filter(d => d.cluster_id === c.uuid).forEach(d => {
    d.status = "online";
    d.health_check = "good";
    const io = pint(9000, 60000);
    d.io_stats = PU.ioStats(io, io * pint(3800, 9200));
  });
  if (!P.pools.some(p => p.cluster_id === c.uuid)) P.pools.push({
    uuid: puuid(),
    cluster_id: c.uuid,
    pool_name: "default",
    status: "active",
    enabled: true,
    qos: null,
    size_total: 0,
    size_used: 0,
    lvol_count: 0,
    created_at: nowIso()
  });
  PU.rollup();
}
function deployTick() {
  const now = Date.now();
  P.discoveries.forEach(d => {
    if (d.status !== "running") return;
    if (!d.started_ms) d.started_ms = now;
    const secs = (now - d.started_ms) / 1000,
      per = 3;
    d.step = DISCOVERY_STEPS[Math.min(DISCOVERY_STEPS.length - 1, Math.floor(secs / per))];
    if (secs >= DISCOVERY_STEPS.length * per) finishDiscovery(d);
  });
  P.deployment_configs.forEach(cfg => {
    if (cfg.status.phase !== "Deploying") return;
    const steps = cfg.status.steps;
    let cur = steps.find(s => s.phase === "Running");
    if (!cur) {
      cur = steps.find(s => s.phase === "Pending");
      if (!cur) {
        cfg.status.phase = "Deployed";
        return;
      }
      cur.phase = "Running";
      cur.startedAt = nowIso();
      cur.__ms = now;
      if (cur.name !== "ActivateCluster") cfg.status.nodes.forEach(n => {
        n.__ms = null;
        n.__phase = null;
        n.phase = "Pending";
        n.progress = 0;
      });
      glog(cfg, "INFO", cur.name, null, `step started: ${cur.label}`);
      if (cur.name === "ActivateCluster") {
        cur.__lines = 0;
      }
    }
    if (cur.name === "ConfigureNodes") {
      const r = runPerNode(cfg, cur.name, CONFIGURE_PHASES, 12, .8, (n, h, mine) => configureDone(cfg, n, h, mine));
      cur.progress = r.progress;
      cur.message = `${cfg.status.nodes.filter(n => n.phase === "Configured").length}/${cfg.status.nodes.length} node(s) configured`;
      if (r.allDone) finishStep(cfg, cur, `${cfg.status.nodes.length} worker node(s) configured — hugepages persisted, core isolation applied`);
    } else if (cur.name === "AddStorageNodes") {
      const r = runPerNode(cfg, cur.name, ADD_PHASES, 10, .6, (n, h, mine) => addDone(cfg, n, h, mine));
      cur.progress = r.progress;
      cur.message = `${cfg.status.nodes.filter(n => n.phase === "Added").length}/${cfg.status.nodes.length} node(s) added`;
      const c = P.clusters.find(x => x.uuid === cfg.cluster_id);
      if (c && c.status === "unready") c.status = "in_activation";
      if (r.allDone) finishStep(cfg, cur, `${(cfg.spec.nodeSets[0] || {}).groups.length} storage node(s) added`);
    } else {
      const el = (now - cur.__ms) / 1000,
        secs = 8;
      const want = Math.min(ACTIVATE_LINES.length, Math.floor(el / secs * ACTIVATE_LINES.length) + 1);
      while ((cur.__lines || 0) < want) {
        dlog(cfg, "ActivateCluster", "INFO", ACTIVATE_LINES[cur.__lines]);
        cur.__lines = (cur.__lines || 0) + 1;
      }
      cur.progress = Math.min(99, Math.round(el / secs * 100));
      cur.message = ACTIVATE_LINES[Math.max(0, (cur.__lines || 1) - 1)];
      if (el >= secs) {
        activate(cfg);
        finishStep(cfg, cur, "Cluster active");
        cfg.status.phase = "Deployed";
        cfg.status.message = "Cluster deployed and activated";
      }
    }
  });
}
function finishStep(cfg, step, msg) {
  step.phase = "Succeeded";
  step.progress = 100;
  step.finishedAt = nowIso();
  step.message = msg;
  glog(cfg, "INFO", step.name, null, `step complete: ${msg}`);
}
setInterval(() => {
  try {
    deployTick();
  } catch (e) {
    window.__tickErr = e.message;
  }
}, 1200);

// ---- routes ----------------------------------------------------------------
const dres = (b, s) => new Response(JSON.stringify(b), {
  status: s || 200,
  headers: {
    "Content-Type": "application/json"
  }
});
const dfail = (code, reason, message) => dres({
  kind: "Status",
  apiVersion: "v1",
  status: "Failure",
  code,
  reason,
  message
}, code);
const strip = o => JSON.parse(JSON.stringify(o, (k, v) => k.startsWith("__") ? undefined : v));
const cdcOut = c => ({
  apiVersion: window.API_GROUP,
  kind: "ClusterDeploymentConfig",
  metadata: {
    name: c.name,
    namespace: window.SB_CONFIG.namespace,
    uid: c.uuid,
    creationTimestamp: c.created_at,
    generation: 1
  },
  spec: c.spec,
  status: strip(c.status)
});
const deployBase = window.SB_CONFIG.k8sBase;
const priorFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfgc = window.SB_CONFIG;
  if (!cfgc.mock) return priorFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const method = (init && init.method || "GET").toUpperCase();
  try {
    deployTick();
  } catch (e) {
    window.__tickErr = e.message;
  }
  let body = {};
  try {
    if (init && init.body) body = JSON.parse(init.body);
  } catch (e) {}
  if (url.startsWith(cfgc.operatorBase + "/proposed/discoveries")) {
    const q = new URLSearchParams(url.split("?")[1] || "");
    const kid = q.get("scopeId");
    return dres({
      results: kid ? P.discoveries.filter(d => d.k8s_cluster_id === kid) : P.discoveries,
      proposed: true
    });
  }
  // deployment step / node logs — pod logs of the operator's job pods
  const lm = url.match(/\/proposed\/deployments\/([\w-]+)\/logs\?name=(.+)$/);
  if (url.startsWith(cfgc.operatorBase) && lm) {
    const L = P.deployment_logs[lm[1]] || {};
    return dres({
      results: L[decodeURIComponent(lm[2])] || [],
      proposed: true
    });
  }
  const cdc = url.startsWith(deployBase) && url.includes("/clusterdeploymentconfigs");
  if (cdc && method === "GET") {
    const name = (url.split("/clusterdeploymentconfigs/")[1] || "").split("?")[0];
    if (name) {
      const rec = P.deployment_configs.find(c => c.name === name);
      return rec ? dres(cdcOut(rec)) : dfail(404, "NotFound", `clusterdeploymentconfigs "${name}" not found`);
    }
    return dres({
      apiVersion: window.API_GROUP,
      kind: "ClusterDeploymentConfigList",
      items: P.deployment_configs.map(cdcOut)
    });
  }
  if (cdc && method !== "GET") {
    const name = (url.split("/clusterdeploymentconfigs/")[1] || "").split("?")[0];
    if (method === "POST") {
      const b = body.spec || {};
      if (!b.cluster || !b.cluster.name) return dfail(422, "Invalid", "spec.cluster.name is required");
      const kc = P.k8s_clusters.find(k => k.uuid === b.__kubernetesClusterId);
      if (!kc || !kc.discovered) return dfail(422, "Invalid", "the Kubernetes cluster has not been discovered — run discovery first");
      if (P.deployment_configs.some(c => c.name === body.metadata.name)) return dfail(409, "AlreadyExists", `clusterdeploymentconfigs "${body.metadata.name}" already exists`);
      if (P.clusters.some(c => c.name === b.cluster.name)) return dfail(409, "AlreadyExists", `a storage cluster named ${b.cluster.name} already exists`);
      if (!(b.__hosts || []).length) return dfail(422, "Invalid", "no nodes selected");
      const rec = mkConfig({
        name: body.metadata.name,
        k8s_cluster_id: b.__kubernetesClusterId,
        hosts: b.__hosts,
        nodeSelector: b.nodeSelector,
        cluster: b.cluster,
        approved: false,
        phase: "Draft",
        message: "Awaiting approval. Nothing has been applied to any node yet."
      });
      if (!rec.spec.nodeSets[0].groups.some(g => g.devices.nvme.length + g.devices.block.length)) {
        P.deployment_configs = P.deployment_configs.filter(c => c !== rec);
        return dfail(422, "Invalid", "no devices selected on any node");
      }
      return dres(cdcOut(rec), 201);
    }
    const rec = P.deployment_configs.find(c => c.name === name);
    if (!rec) return dfail(404, "NotFound", `clusterdeploymentconfigs "${name}" not found`);
    if (method === "DELETE") {
      if (rec.status.phase === "Deploying") return dfail(403, "Forbidden", "admission webhook denied the request: a deployment in flight cannot be deleted — it would strand half-configured nodes");
      P.deployment_configs = P.deployment_configs.filter(c => c !== rec);
      return dres({
        kind: "Status",
        status: "Success"
      });
    }
    if (method === "PATCH") {
      const keys = Object.keys(body.spec || {});
      if (keys.some(k => k !== "approved")) return dfail(422, "Invalid", `spec is immutable except for spec.approved; refused: ${keys.filter(k => k !== "approved").join(", ")}`);
      if (!body.spec.approved) return dfail(422, "Invalid", "approval cannot be withdrawn");
      if (rec.spec.approved) return dfail(409, "Conflict", "this document is already approved");
      rec.spec.approved = true;
      rec.status.phase = "Deploying";
      rec.status.message = "Approved — deployment running";
      rec.status.steps = freshSteps();
      rec.status.log = [];
      P.deployment_logs[rec.uuid] = {};
      glog(rec, "INFO", "Approve", null, `document approved by operator`);
      createClusterRecord(rec);
      return dres(cdcOut(rec));
    }
  }
  if (url.startsWith(deployBase) && url.includes("/operatorops") && method === "POST" && (body.spec || {}).action === "Discover") {
    const res = await priorFetch(input, init);
    if (!res.ok) return res;
    const out = await res.clone().json().catch(() => ({}));
    const kid = (body.spec.target || {}).kubernetesClusterId || (body.spec.kubernetesClusterRef ? (P.k8s_clusters.find(k => k.name === body.spec.kubernetesClusterRef) || {}).uuid : null);
    if (kid) startDiscovery(kid, body.spec.discover || body.spec, (out.metadata || {}).name);
    return dres(out, 201);
  }
  return priorFetch(input, init);
};
})();
// ---- mock-migrate.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK: MIGRATION PATHS — online migration of workloads and their volumes from
// site A to site B inside one (stretched) Kubernetes cluster.
//   path      A → B: source storage cluster, target storage cluster, the k8s cluster
//   app group ordered unit of work: VMs + containers → their PVCs → their volumes
//   phases    Queued → Replicating → Converged → MovingWorkloads → MigratingVolumes → Cleanup → Completed
// Under the hood a group owns an asynchronous replication policy (created at
// start, deleted at cleanup); its per-volume backlog is what "converged" means.
// ---------------------------------------------------------------------------
const MG = window.SB_DB,
  MGU = window.SB_UTIL;
const mguuid = MGU.uuid,
  mgint = MGU.int,
  mgpick = MGU.pick,
  mgago = MGU.ago;
MG.migration_paths = [];
MG.app_groups = [];
const PHASES = ["Queued", "Replicating", "Converged", "MovingWorkloads", "MigratingVolumes", "Cleanup", "Completed"];
window.MIG_PHASES = PHASES;
const mnow = () => new Date().toISOString();
// backlog, in the same units the UI shows elsewhere
const fb = b => b >= 1e12 ? (b / 1e12).toFixed(2) + " TB" : b >= 1e9 ? (b / 1e9).toFixed(1) + " GB" : Math.round(b / 1e6) + " MB";
const plog = (p, level, msg, gid) => {
  p.log.push({
    ts: mnow(),
    level,
    group_id: gid || null,
    msg
  });
  if (p.log.length > 300) p.log.shift();
};

// the PVCs a member owns: in the mock, the namespace's claims whose name shares the member's stem
function claimsFor(kc, ns, members) {
  const pvcs = MG.pvcs.filter(p => p.k8s_cluster_id === kc.uuid && p.namespace === ns && p.lvol_id);
  const stems = members.map(m => m.name.split("-")[0].toLowerCase());
  const mine = pvcs.filter(p => stems.some(s => p.pvc_name.toLowerCase().includes(s)));
  return mine.length ? mine : pvcs.slice(0, Math.max(1, members.length));
}
function mkGroup(path, o) {
  const kc = MG.k8s_clusters.find(k => k.uuid === path.k8s_cluster_id);
  const taken = new Set(MG.app_groups.filter(g => g.path_id === path.uuid).flatMap(g => g.pvc_ids));
  const pvcs = claimsFor(kc, o.namespace, o.members).filter(p => !taken.has(p.uuid));
  const vols = pvcs.map(p => MG.lvols.find(v => v.uuid === p.lvol_id)).filter(Boolean);
  const g = {
    uuid: mguuid(),
    path_id: path.uuid,
    name: o.name,
    namespace: o.namespace,
    members: o.members.map(m => ({
      kind: m.kind,
      name: m.name,
      state: "Pending",
      progress: 0
    })),
    approval: o.approval || "manual",
    order: path.queue.length,
    pvc_ids: pvcs.map(p => p.uuid),
    lvol_ids: vols.map(v => v.uuid),
    volumes: vols.map(v => ({
      lvol_id: v.uuid,
      lvol_name: v.lvol_name,
      size: v.size_util || v.size_prov,
      backlog_bytes: v.size_util || 0,
      last_replication_at: null,
      progress: 0,
      migrated: false
    })),
    phase: o.phase || "Queued",
    message: "queued",
    rpolicy_id: null,
    cg_id: null,
    created_at: mgago(o.ageHours || 0),
    started_at: null,
    finished_at: null
  };
  MG.app_groups.push(g);
  path.queue.push(g.uuid);
  return g;
}
function mkPath(o) {
  const p = {
    uuid: mguuid(),
    name: o.name,
    k8s_cluster_id: o.k8s_cluster_id,
    source_cluster_id: o.source_cluster_id,
    target_cluster_id: o.target_cluster_id,
    source_zone_id: o.source_zone_id || null,
    target_zone_id: o.target_zone_id || null,
    status: o.status || "active",
    created_at: mgago(o.ageHours || 0),
    queue: [],
    log: []
  };
  MG.migration_paths.push(p);
  plog(p, "INFO", `migration path ${p.name} created`);
  return p;
}

// ---- seeds -----------------------------------------------------------------
(function seed() {
  const kc = MG.k8s_clusters.find(k => (k.storage_cluster_ids || []).length >= 2) || MG.k8s_clusters[0];
  if (!kc) return;
  const src = MG.clusters.find(c => (kc.storage_cluster_ids || []).includes(c.uuid) && c.status === "online") || MG.clusters[0];
  const tgt = MG.clusters.find(c => c.uuid !== src.uuid && c.status === "online" && c.dr_target_eligible) || MG.clusters.find(c => c.uuid !== src.uuid);
  if (!src || !tgt) return;
  kc.storage_cluster_ids = [...new Set((kc.storage_cluster_ids || []).concat([src.uuid, tgt.uuid]))];
  const p = mkPath({
    name: `${src.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(0, 2).join("-")}`,
    k8s_cluster_id: kc.uuid,
    source_cluster_id: src.uuid,
    target_cluster_id: tgt.uuid,
    source_zone_id: (src.zone_ids || [])[0] || null,
    target_zone_id: (tgt.zone_ids || [])[0] || null,
    ageHours: mgint(30, 300)
  });
  const nss = [...new Set(MG.pvcs.filter(x => x.k8s_cluster_id === kc.uuid && x.lvol_id).map(x => x.namespace))];
  const defs = [{
    name: "erp-vms",
    members: [{
      kind: "VirtualMachine",
      name: "erp-db-vm"
    }, {
      kind: "VirtualMachine",
      name: "erp-app-vm"
    }],
    phase: "Completed"
  }, {
    name: "payments",
    members: [{
      kind: "StatefulSet",
      name: "postgres"
    }, {
      kind: "Deployment",
      name: "payments-api"
    }],
    phase: "Replicating"
  }, {
    name: "analytics",
    members: [{
      kind: "StatefulSet",
      name: "kafka"
    }, {
      kind: "Deployment",
      name: "flink-worker"
    }, {
      kind: "VirtualMachine",
      name: "legacy-etl-vm"
    }],
    phase: "Queued"
  }, {
    name: "shared-services",
    members: [{
      kind: "Deployment",
      name: "grafana"
    }, {
      kind: "StatefulSet",
      name: "registry"
    }],
    phase: "Queued"
  }];
  defs.forEach((d, i) => {
    const g = mkGroup(p, Object.assign({
      namespace: nss[i % Math.max(1, nss.length)] || "default",
      approval: i === 1 ? "manual" : "auto",
      ageHours: mgint(2, 28)
    }, d));
    if (d.phase === "Completed") {
      g.members.forEach(m => {
        m.state = "Moved";
        m.progress = 100;
      });
      g.volumes.forEach(v => {
        v.backlog_bytes = 0;
        v.progress = 100;
        v.migrated = true;
        v.last_replication_at = g.created_at;
      });
      g.message = "completed — source volumes deleted";
      g.started_at = g.created_at;
      g.finished_at = g.created_at;
      g.lvol_ids.forEach(id => {
        const v = MG.lvols.find(x => x.uuid === id);
        if (v) v.cluster_id = tgt.uuid;
      });
      plog(p, "INFO", `${g.name}: completed`, g.uuid);
    }
    if (d.phase === "Replicating") startGroup(p, g);
  });
  // a second, paused path on another cluster pair, still empty
  const other = MG.clusters.find(c => ![src.uuid, tgt.uuid].includes(c.uuid) && c.status === "online");
  if (other) mkPath({
    name: `${other.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(0, 2).join("-")}`,
    k8s_cluster_id: kc.uuid,
    source_cluster_id: other.uuid,
    target_cluster_id: tgt.uuid,
    status: "paused",
    ageHours: mgint(5, 40)
  });
})();

// ---- state machine ---------------------------------------------------------
function startGroup(p, g) {
  // the group's volumes become a consistency group, which owns the cadence
  const cg = {
    uuid: mguuid(),
    cluster_id: p.source_cluster_id,
    name: `mig-${g.name}`,
    lvol_ids: g.lvol_ids.slice(),
    created_at: mnow(),
    backup_policy: null,
    replication_config: {
      frequency_minutes: 5,
      retention: []
    }
  };
  MG.consistency_groups.push(cg);
  g.cg_id = cg.uuid;
  const pol = {
    uuid: mguuid(),
    name: `mig-${g.name}`,
    mode: "asynchronous",
    pair_id: null,
    migration_group_id: g.uuid,
    source_cluster_id: p.source_cluster_id,
    target_cluster_id: p.target_cluster_id,
    zone_ids: null,
    cg_id: cg.uuid,
    cg_name: cg.name,
    frequency_minutes: 5,
    retention: [],
    failback: {
      mode: "manual",
      frequency_minutes: 0,
      reverse_on_failover: false,
      resync_full: false
    },
    state: "healthy",
    last_replication_at: mnow(),
    backlog_bytes: 0,
    generations_kept: 0,
    created_at: mnow(),
    last_failover_at: null,
    last_test_at: null,
    lvol_ids: g.lvol_ids.slice(),
    dr_cluster_ids: [],
    apps_count: 0
  };
  MG.dr_policies.push(pol);
  g.lvol_ids.forEach(id => {
    const v = MG.lvols.find(x => x.uuid === id);
    if (v) v.replication = {
      policy_id: pol.uuid,
      policy_name: pol.name,
      mode: "asynchronous",
      status: "healthy",
      last_replication_at: mnow(),
      backlog_bytes: v.size_util || 0,
      target_cluster_id: p.target_cluster_id,
      consistency_group: `cg-${pol.name}`
    };
  });
  g.rpolicy_id = pol.uuid;
  g.phase = "Replicating";
  g.message = "initial replication of the group's volumes";
  g.started_at = mnow();
  g.__ms = Date.now();
  plog(p, "INFO", `${g.name}: consistency group ${cg.name} and replication policy ${pol.name} created for ${g.lvol_ids.length} volume(s)`, g.uuid);
}
function setPhase(p, g, phase, msg) {
  g.phase = phase;
  g.message = msg;
  g.__ms = Date.now();
  plog(p, "INFO", `${g.name}: ${phase} — ${msg}`, g.uuid);
}
function tickGroup(p, g) {
  const now = Date.now();
  const el = (now - (g.__ms || now)) / 1000;
  if (g.phase === "Replicating") {
    let left = 0;
    g.volumes.forEach(v => {
      const rate = Math.max(2e8, v.size * .06); // bytes per second, ~16 s for a full copy
      v.backlog_bytes = Math.max(0, v.backlog_bytes - rate * 1.2 + (Math.random() < .3 ? v.size * .002 : 0));
      if (v.backlog_bytes < v.size * .005) v.backlog_bytes = 0;
      v.last_replication_at = mnow();
      left += v.backlog_bytes;
      const lv = MG.lvols.find(x => x.uuid === v.lvol_id);
      if (lv && lv.replication) {
        lv.replication.backlog_bytes = v.backlog_bytes;
        lv.replication.last_replication_at = v.last_replication_at;
      }
    });
    const pol = MG.dr_policies.find(x => x.uuid === g.rpolicy_id);
    if (pol) pol.backlog_bytes = left;
    g.message = left ? `backlog ${fb(left)} across ${g.volumes.filter(v => v.backlog_bytes).length} volume(s)` : "converged";
    if (!left) setPhase(p, g, "Converged", g.approval === "auto" ? "backlog zero — moving workloads" : "backlog zero — waiting for approval to move");
  } else if (g.phase === "Converged") {
    if (g.approval === "auto" && el > 2) setPhase(p, g, "MovingWorkloads", "live-migrating VMs, restarting containers on the target site");
  } else if (g.phase === "MovingWorkloads") {
    let done = true;
    g.members.forEach((m, i) => {
      const dur = m.kind === "VirtualMachine" ? 8 : 3,
        start = i * .8;
      const t = el - start;
      if (t < 0) {
        m.state = "Pending";
        done = false;
        return;
      }
      m.progress = Math.min(100, Math.round(t / dur * 100));
      if (m.progress < 100) {
        done = false;
        if (m.state === "Pending") {
          m.state = m.kind === "VirtualMachine" ? "LiveMigrating" : "Restarting";
          plog(p, "INFO", `${g.name}: ${m.kind}/${m.name} ${m.kind === "VirtualMachine" ? "live migration started (kubevirt)" : "rescheduled to the target site"}`, g.uuid);
        }
      } else if (m.state !== "Moved") {
        m.state = "Moved";
        plog(p, "INFO", `${g.name}: ${m.kind}/${m.name} running on the target site`, g.uuid);
      }
    });
    g.message = `${g.members.filter(m => m.state === "Moved").length}/${g.members.length} workload(s) moved`;
    if (done) setPhase(p, g, "MigratingVolumes", "online migration of the volumes to the target nodes");
  } else if (g.phase === "MigratingVolumes") {
    let done = true;
    g.volumes.forEach((v, i) => {
      const t = el - i * .5,
        dur = 7;
      v.progress = Math.max(0, Math.min(100, Math.round(t / dur * 100)));
      if (v.progress < 100) done = false;else if (!v.migrated) {
        v.migrated = true;
        const lv = MG.lvols.find(x => x.uuid === v.lvol_id);
        if (lv) lv.cluster_id = p.target_cluster_id;
        plog(p, "INFO", `${g.name}: ${v.lvol_name} now served from the target site`, g.uuid);
      }
    });
    g.message = `${g.volumes.filter(v => v.migrated).length}/${g.volumes.length} volume(s) migrated`;
    if (done) setPhase(p, g, "Cleanup", "deleting the source copies and the replication policy");
  } else if (g.phase === "Cleanup") {
    if (el > 3) {
      MG.dr_policies = MG.dr_policies.filter(x => x.uuid !== g.rpolicy_id);
      // the group's consistency group goes with it
      MG.consistency_groups = MG.consistency_groups.filter(x => x.uuid !== g.cg_id);
      g.lvol_ids.forEach(id => {
        const v = MG.lvols.find(x => x.uuid === id);
        if (v) v.replication = null;
      });
      g.finished_at = mnow();
      setPhase(p, g, "Completed", "completed — source volumes deleted");
      MGU.rollup();
    }
  }
}
function migTick() {
  MG.migration_paths.forEach(p => {
    if (p.status !== "active") return;
    const groups = p.queue.map(id => MG.app_groups.find(g => g.uuid === id)).filter(Boolean);
    const active = groups.find(g => !["Queued", "Completed", "Paused", "Failed"].includes(g.phase));
    if (active) {
      tickGroup(p, active);
      return;
    }
    const next = groups.find(g => g.phase === "Queued");
    if (next) {
      if (!next.lvol_ids.length) {
        next.phase = "Failed";
        next.message = "no bound PVC found for the members";
        plog(p, "ERROR", `${next.name}: no volumes to migrate`, next.uuid);
        return;
      }
      startGroup(p, next);
    } else if (groups.length && groups.every(g => g.phase === "Completed") && p.status !== "completed") {
      p.status = "completed";
      plog(p, "INFO", "all application groups migrated");
    }
  });
}
setInterval(() => {
  try {
    migTick();
  } catch (e) {
    window.__tickErr = e.message;
  }
}, 1300);

// ---- routes ----------------------------------------------------------------
const gid = (m, i) => MG.app_groups.find(g => g.uuid === m[i]);
const pid = (m, i) => MG.migration_paths.find(p => p.uuid === m[i]);
const ROUTES = [["POST", /^\/migration-paths$/, (m, b) => {
  if (!b.name) return {
    __err: "A name is required"
  };
  const kc = MG.k8s_clusters.find(k => k.uuid === b.k8s_cluster_id);
  if (!kc) return {
    __err: "Pick the stretched Kubernetes cluster"
  };
  if (!b.source_cluster_id || !b.target_cluster_id || b.source_cluster_id === b.target_cluster_id) return {
    __err: "Source and target storage cluster must differ"
  };
  if (!(kc.storage_cluster_ids || []).includes(b.target_cluster_id)) return {
    __err: `${kc.name} has no storage class on the target cluster — add nodes at the target site and a storage class first`
  };
  if (MG.migration_paths.some(p => p.source_cluster_id === b.source_cluster_id && p.target_cluster_id === b.target_cluster_id && p.status !== "completed")) return {
    __err: "A path between these clusters already exists"
  };
  const s = MG.clusters.find(c => c.uuid === b.source_cluster_id),
    t = MG.clusters.find(c => c.uuid === b.target_cluster_id);
  return {
    results: [mkPath({
      name: b.name,
      k8s_cluster_id: kc.uuid,
      source_cluster_id: s.uuid,
      target_cluster_id: t.uuid,
      source_zone_id: (s.zone_ids || [])[0],
      target_zone_id: (t.zone_ids || [])[0]
    })]
  };
}], ["POST", /^\/migration-paths\/([\w-]+)\/pause$/, m => {
  const p = pid(m, 1);
  if (!p) return {
    __404: true
  };
  p.status = "paused";
  plog(p, "WARN", "path paused — the running group finishes its current step, nothing new starts");
  return {
    results: [p]
  };
}], ["POST", /^\/migration-paths\/([\w-]+)\/resume$/, m => {
  const p = pid(m, 1);
  if (!p) return {
    __404: true
  };
  p.status = "active";
  plog(p, "INFO", "path resumed");
  return {
    results: [p]
  };
}], ["DELETE", /^\/migration-paths\/([\w-]+)$/, m => {
  const p = pid(m, 1);
  if (!p) return {
    __404: true
  };
  if (MG.app_groups.some(g => g.path_id === p.uuid && g.phase !== "Completed")) return {
    __err: "The path still has application groups that are not completed. Remove them first."
  };
  MG.app_groups = MG.app_groups.filter(g => g.path_id !== p.uuid);
  MG.migration_paths = MG.migration_paths.filter(x => x !== p);
  return {
    results: []
  };
}], ["PUT", /^\/migration-paths\/([\w-]+)\/queue$/, (m, b) => {
  const p = pid(m, 1);
  if (!p) return {
    __404: true
  };
  const order = (b.order || []).filter(id => p.queue.includes(id));
  if (order.length !== p.queue.length) return {
    __err: "The order must list every group of the path exactly once"
  };
  p.queue = order;
  p.queue.forEach((id, i) => {
    const g = gid([id], 0);
    if (g) g.order = i;
  });
  plog(p, "INFO", "queue reordered");
  return {
    results: [p]
  };
}], ["POST", /^\/migration-paths\/([\w-]+)\/app-groups$/, (m, b) => {
  const p = pid(m, 1);
  if (!p) return {
    __404: true
  };
  if (!b.name) return {
    __err: "A group name is required"
  };
  if (!b.namespace) return {
    __err: "A namespace is required"
  };
  if (!(b.members || []).length) return {
    __err: "Add at least one VM or container workload"
  };
  if (MG.app_groups.some(g => g.path_id === p.uuid && g.name === b.name)) return {
    __err: `A group named ${b.name} exists on this path`
  };
  const g = mkGroup(p, {
    name: b.name,
    namespace: b.namespace,
    members: b.members,
    approval: b.approval
  });
  plog(p, "INFO", `${g.name}: queued with ${g.members.length} workload(s), ${g.lvol_ids.length} volume(s)`, g.uuid);
  return {
    results: [g]
  };
}], ["POST", /^\/app-groups\/([\w-]+)\/move$/, m => {
  const g = gid(m, 1);
  if (!g) return {
    __404: true
  };
  if (g.phase !== "Converged") return {
    __err: "The group can only be moved once replication has converged"
  };
  const p = pid([g.path_id], 0);
  setPhase(p, g, "MovingWorkloads", "approved — live-migrating VMs, restarting containers");
  return {
    results: [g]
  };
}], ["POST", /^\/app-groups\/([\w-]+)\/pause$/, m => {
  const g = gid(m, 1);
  if (!g) return {
    __404: true
  };
  if (!["Replicating", "Converged"].includes(g.phase)) return {
    __err: "Only a group that is replicating or converged can be paused — workload and volume moves run to completion"
  };
  g.__resume = g.phase;
  const p = pid([g.path_id], 0);
  setPhase(p, g, "Paused", "paused by operator — replication policy stays in place");
  return {
    results: [g]
  };
}], ["POST", /^\/app-groups\/([\w-]+)\/resume$/, m => {
  const g = gid(m, 1);
  if (!g) return {
    __404: true
  };
  if (g.phase !== "Paused") return {
    __err: "The group is not paused"
  };
  const p = pid([g.path_id], 0);
  setPhase(p, g, g.__resume || "Replicating", "resumed");
  return {
    results: [g]
  };
}], ["PUT", /^\/app-groups\/([\w-]+)\/approval$/, (m, b) => {
  const g = gid(m, 1);
  if (!g) return {
    __404: true
  };
  g.approval = b.approval === "auto" ? "auto" : "manual";
  return {
    results: [g]
  };
}], ["DELETE", /^\/app-groups\/([\w-]+)$/, m => {
  const g = gid(m, 1);
  if (!g) return {
    __404: true
  };
  if (!["Queued", "Completed", "Failed"].includes(g.phase)) return {
    __err: "A group in progress cannot be removed — pause it, or let it finish"
  };
  const p = pid([g.path_id], 0);
  if (p) {
    p.queue = p.queue.filter(id => id !== g.uuid);
    plog(p, "INFO", `${g.name}: removed`, g.uuid);
  }
  MG.app_groups = MG.app_groups.filter(x => x !== g);
  return {
    results: []
  };
}]];
if (window.SB_CP_ROUTES) window.SB_CP_ROUTES.MUT_ROUTES.push(...ROUTES);
})();
// ---- mock-rbac.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MOCK ACCESS CONTROL — the hub API server's view of RBAC, per
// uploads/simplyblock-multicluster-rbac-design.md §2.3, §4, §5.
//
// Kubernetes RBAC IS the store. A grant is (subject, role, scope) and becomes a
// RoleBinding in the scope's namespace — or an AccessGrant when it needs expiry,
// a reason, or DR fan-out. Roles are the eight aggregated sb:* ClusterRoles from
// the chart; nothing here authors roles. Served from /proposed/access*.
// The "View as" switcher (localStorage sb.viewas) stands in for impersonation.
// ---------------------------------------------------------------------------
const RB_DB = window.SB_DB,
  RB_U = window.SB_UTIL;
const rbSlug = s => String(s || "").toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40);
const RB_RW = ["get", "list", "watch", "create", "update", "patch", "delete"],
  RB_RO = ["get", "list", "watch"];
const RB_RBAC = "rbac.authorization.k8s.io";
const RB_NS_DR = "sb-dr-system";
const rbRule = (resources, verbs, extra) => Object.assign({
  apiGroups: ["simplyblock.io"],
  resources,
  verbs
}, extra || {});
const RB_STORAGE = ["storageclusters", "storagenodes", "devices", "storageclusterops", "storagenodeops", "deviceops", "backuppolicies"];
const RB_POOL = ["storagepools", "volumes", "snapshots", "backups", "buckets", "consistencygroups", "backuppolicies"];
const RB_DR = ["drpolicies", "drclusters", "replicationpolicies", "clusterpairs", "protectionplans"];
const RB_ROLE_NAMES = ["sb:infra-admin", "sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader", "sb:dr-admin", "sb:dr-reader", "sb:app-admin"];

// ---- the eight aggregated ClusterRoles (§2.3) --------------------------------
const RB_ROLES = [{
  name: "sb:infra-admin",
  boundAt: "cluster scope",
  description: "Owns the envelope: node pool allocations, managed clusters, cluster classes. May bind every sb:* role anywhere.",
  parts: [{
    name: "sb:infra-admin-allocations",
    rules: [rbRule(["nodepoolallocations", "managedclusters", "storageclusterclasses"], RB_RW)]
  }, {
    name: "sb:infra-admin-grants",
    rules: [rbRule(["accessgrants"], RB_RW), rbRule(["clusterroles"], ["bind"], {
      apiGroups: [RB_RBAC],
      resourceNames: RB_ROLE_NAMES
    })]
  }]
}, {
  name: "sb:cluster-admin",
  boundAt: "sb-sc-<storage cluster>",
  description: "Runs one storage cluster: nodes, devices, operations. Binds cluster and pool roles inside its own namespace only.",
  parts: [{
    name: "sb:cluster-admin-storage",
    rules: [rbRule(RB_STORAGE, RB_RW)]
  }, {
    name: "sb:cluster-admin-grants",
    rules: [rbRule(["accessgrants"], RB_RW), rbRule(["clusterroles"], ["bind"], {
      apiGroups: [RB_RBAC],
      resourceNames: ["sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader"]
    })]
  }]
}, {
  name: "sb:cluster-reader",
  boundAt: "sb-sc-<storage cluster>",
  description: "Reads the storage cluster, its nodes, devices and grants. Changes nothing.",
  parts: [{
    name: "sb:cluster-reader-storage",
    rules: [rbRule(RB_STORAGE, RB_RO), rbRule(["accessgrants"], RB_RO)]
  }]
}, {
  name: "sb:pool-admin",
  boundAt: "sb-sc-<storage cluster> or sb-sp-<pool>",
  description: "Owns the data plane below the pool: volumes, snapshots, backups, buckets, backup policies, KEKs. No verbs on nodes or devices, no RBAC on StorageClass.",
  parts: [{
    name: "sb:pool-admin-volumes",
    rules: [rbRule(RB_POOL, RB_RW)]
  }]
}, {
  name: "sb:pool-reader",
  boundAt: "sb-sc-<storage cluster> or sb-sp-<pool>",
  description: "Reads pools and everything beneath, including backups — which is what a restore into another pool needs on the source side.",
  parts: [{
    name: "sb:pool-reader-volumes",
    rules: [rbRule(RB_POOL, RB_RO)]
  }]
}, {
  name: "sb:dr-admin",
  boundAt: RB_NS_DR,
  description: "Defines DR between cluster pairs: DR policies, DR clusters, replication policies and protection plans.",
  parts: [{
    name: "sb:dr-admin-policies",
    rules: [rbRule(RB_DR, RB_RW)]
  }]
}, {
  name: "sb:dr-reader",
  boundAt: RB_NS_DR,
  description: "Reads DR configuration and replication backlog.",
  parts: [{
    name: "sb:dr-reader-policies",
    rules: [rbRule(RB_DR, RB_RO)]
  }]
}, {
  name: "sb:app-admin",
  boundAt: "application namespace",
  description: "Protects an application and may fail it over — failover is create on applicationfailovers, so it is grantable without the right to rewrite the policy.",
  parts: [{
    name: "sb:app-admin-apps",
    rules: [rbRule(["protectedapplications", "recipes"], RB_RW), rbRule(["applicationfailovers"], ["create", "get", "list"])]
  }]
}];
const rbRoleRules = name => {
  const r = RB_ROLES.find(x => x.name === name);
  return r ? r.parts.flatMap(p => p.rules) : [];
};
RB_ROLES.forEach(r => {
  r.rules = r.parts.flatMap(p => p.rules);
  r.uuid = RB_U.uuid();
});

// ---- namespaces: hierarchy in labels, not names (§2.2) ----------------------
const RB_NS = {};
const rbClusterOf = id => RB_DB.clusters.find(c => c.uuid === id);
const rbK8sOf = id => (RB_DB.k8s_clusters || []).find(k => k.uuid === id);
const nsMc = k => "sb-mc-" + rbSlug(k.name);
const nsSc = c => "sb-sc-" + rbSlug(c.name);
const nsApp = (ns, c) => `${ns}@${rbSlug(c.name)}`;
(RB_DB.k8s_clusters || []).forEach(k => {
  RB_NS[nsMc(k)] = {
    kind: "managed-cluster",
    id: k.uuid,
    label: k.name,
    labels: {
      "simplyblock.io/managed-cluster": rbSlug(k.name),
      "simplyblock.io/scope-kind": "managed-cluster"
    }
  };
});
RB_DB.clusters.forEach(c => {
  RB_NS[nsSc(c)] = {
    kind: "storage-cluster",
    id: c.uuid,
    label: c.name,
    labels: {
      "simplyblock.io/storage-cluster": rbSlug(c.name),
      "simplyblock.io/scope-kind": "storage-cluster"
    }
  };
});
// pools default to the storage cluster's namespace (§2.1); every fourth is a
// tenant boundary with its own sb-sp-* namespace, so the difference is visible
RB_DB.pools.forEach((p, i) => {
  const c = rbClusterOf(p.cluster_id);
  p.isolated = i % 4 === 1;
  p.namespace = p.isolated ? `sb-sp-${rbSlug(p.pool_name)}-${p.uuid.slice(0, 6)}` : nsSc(c);
  if (p.isolated) RB_NS[p.namespace] = {
    kind: "storage-pool",
    id: p.uuid,
    label: p.pool_name,
    labels: {
      "simplyblock.io/storage-cluster": rbSlug(c.name),
      "simplyblock.io/storage-pool": rbSlug(p.pool_name),
      "simplyblock.io/scope-kind": "storage-pool"
    }
  };
});
RB_NS[RB_NS_DR] = {
  kind: "dr",
  label: "DR",
  labels: {
    "simplyblock.io/scope-kind": "dr"
  }
};
(RB_DB.protected_apps || []).forEach(a => {
  const c = rbClusterOf(a.source_cluster_id);
  if (!c) return;
  const key = nsApp(a.namespace, c);
  RB_NS[key] = RB_NS[key] || {
    kind: "application",
    label: `${a.namespace} on ${c.name}`,
    namespace: a.namespace,
    clusterId: c.uuid,
    labels: {
      "simplyblock.io/scope-kind": "application"
    }
  };
});
const rbPoolNs = p => p.namespace;
// namespaces a scope resolves to; application + drPolicy fans out to both members (§4.4)
function rbScopeNamespaces(sc) {
  if (sc.kind === "cluster-scope") return ["*"];
  if (sc.kind === "managed-cluster") {
    const k = rbK8sOf(sc.id);
    return k ? [nsMc(k)] : [];
  }
  if (sc.kind === "storage-cluster") {
    const c = rbClusterOf(sc.id);
    return c ? [nsSc(c)] : [];
  }
  if (sc.kind === "storage-pool") {
    const p = RB_DB.pools.find(x => x.uuid === sc.id);
    return p ? [rbPoolNs(p)] : [];
  }
  if (sc.kind === "dr-pair") return [RB_NS_DR];
  if (sc.kind === "application") {
    const a = (RB_DB.protected_apps || []).find(x => x.uuid === sc.id);
    if (!a) return [];
    const src = rbClusterOf(a.source_cluster_id);
    const out = src ? [nsApp(a.namespace, src)] : [];
    if (sc.drPolicy) {
      const pol = (RB_DB.dr_policies || []).find(p => p.uuid === sc.drPolicy || p.name === sc.drPolicy);
      if (pol) [pol.source_cluster_id, pol.target_cluster_id].map(rbClusterOf).filter(Boolean).forEach(c => {
        const k = nsApp(a.namespace, c);
        if (!out.includes(k)) out.push(k);
      });
    }
    return out;
  }
  return [];
}
const rbScopeLabel = sc => sc.kind === "cluster-scope" ? "cluster scope" : sc.name || sc.id;

// ---- identities: IdP groups first, users flagged (§4.1) ----------------------
const rb_c0 = RB_DB.clusters[0],
  rb_c1 = RB_DB.clusters[1] || rb_c0,
  rb_c2 = RB_DB.clusters[2] || rb_c1;
const rb_k0 = (RB_DB.k8s_clusters || [])[0];
const rb_pool0 = RB_DB.pools.find(p => p.isolated) || RB_DB.pools[0];
const rb_app0 = (RB_DB.protected_apps || []).find(a => a.source_cluster_id === rb_c0.uuid && a.policy_id) || (RB_DB.protected_apps || [])[0];
const rb_pol0 = rb_app0 ? (RB_DB.dr_policies || []).find(p => p.uuid === rb_app0.policy_id) : null;
const RB_USERS = [{
  name: "oidc:root@simplyblock.io",
  groups: ["oidc:platform-admins", "system:authenticated"],
  label: "Root (infra admin)",
  initials: "RT"
}, {
  name: "oidc:maria@simplyblock.io",
  groups: ["oidc:eu-storage", "system:authenticated"],
  label: `Maria (cluster admin · ${rb_c0.name})`,
  initials: "MA"
}, {
  name: "oidc:jonas@simplyblock.io",
  groups: ["oidc:team-a", "system:authenticated"],
  label: `Jonas (pool admin · ${rb_pool0.pool_name})`,
  initials: "JO"
}, {
  name: "oidc:dr-oncall@simplyblock.io",
  groups: ["oidc:dr-oncall", "system:authenticated"],
  label: "DR on-call (app admin, failover)",
  initials: "DR"
}, {
  name: "oidc:audit@simplyblock.io",
  groups: ["oidc:auditors", "system:authenticated"],
  label: "Auditor (readers everywhere)",
  initials: "AU"
}, {
  name: "oidc:backup@simplyblock.io",
  groups: ["system:authenticated"],
  label: `Backup operator · ${rb_c1.name} (user-bound, external)`,
  initials: "BK"
}];
const scSc = c => ({
  kind: "storage-cluster",
  id: c.uuid,
  name: c.name
});
const scPool = p => ({
  kind: "storage-pool",
  id: p.uuid,
  name: p.pool_name
});
let rbSeq = 0;
function RB_G(subject, role, scope, o) {
  o = o || {};
  const namespaces = rbScopeNamespaces(scope);
  return {
    uuid: RB_U.uuid(),
    name: o.name || `${role.replace("sb:", "sb-")}-${rbSlug(subject.name.replace(/^oidc:/, ""))}-${++rbSeq}`,
    subject,
    role,
    scope,
    namespaces,
    source: o.source || "control-center",
    expires_at: o.expiresAt || null,
    reason: o.reason || "",
    created_by: o.by || "oidc:root@simplyblock.io",
    created_at: RB_U.ago(o.age || RB_U.int(50, 2000))
  };
}
const RB_GRANTS = [RB_G({
  kind: "Group",
  name: "oidc:platform-admins"
}, "sb:infra-admin", {
  kind: "cluster-scope"
}, {
  name: "sb-infra-admin-platform",
  reason: "installed by simplyblock-crds chart",
  age: 3000
}),
// the install also hands the platform group the storage roles on every cluster
...RB_DB.clusters.flatMap(c => [RB_G({
  kind: "Group",
  name: "oidc:platform-admins"
}, "sb:cluster-admin", scSc(c), {
  reason: "bootstrap",
  age: 2900
}), RB_G({
  kind: "Group",
  name: "oidc:platform-admins"
}, "sb:pool-admin", scSc(c), {
  reason: "bootstrap",
  age: 2900
})]), RB_G({
  kind: "Group",
  name: "oidc:platform-admins"
}, "sb:dr-admin", {
  kind: "dr-pair",
  name: "all pairs"
}, {
  reason: "bootstrap",
  age: 2900
}), RB_G({
  kind: "Group",
  name: "oidc:eu-storage"
}, "sb:cluster-admin", scSc(rb_c0), {
  reason: "OPS-3310 EU storage team"
}), RB_G({
  kind: "Group",
  name: "oidc:eu-storage"
}, "sb:pool-admin", scSc(rb_c0), {
  reason: "OPS-3310 EU storage team"
}), RB_G({
  kind: "Group",
  name: "oidc:team-a"
}, "sb:pool-admin", scPool(rb_pool0), {
  reason: "tenant pool, isolated namespace"
}), RB_G({
  kind: "Group",
  name: "oidc:team-a"
}, "sb:cluster-reader", scSc(rb_c1), {
  by: "oidc:maria@simplyblock.io"
}), RB_G({
  kind: "Group",
  name: "oidc:dr-oncall"
}, "sb:dr-reader", {
  kind: "dr-pair",
  name: "all pairs"
}), rb_app0 ? RB_G({
  kind: "Group",
  name: "oidc:dr-oncall"
}, "sb:app-admin", {
  kind: "application",
  id: rb_app0.uuid,
  name: `${rb_app0.namespace}/${rb_app0.app_name}`,
  drPolicy: rb_pol0 ? rb_pol0.uuid : null,
  drPolicyName: rb_pol0 ? rb_pol0.name : null
}, {
  name: "team-dr-oncall-failover",
  expiresAt: new Date(Date.now() + 86400e3 * 45).toISOString(),
  reason: "OPS-4821 on-call rotation"
}) : null, ...RB_DB.clusters.flatMap(c => [RB_G({
  kind: "Group",
  name: "oidc:auditors"
}, "sb:cluster-reader", scSc(c), {
  reason: "SOC2 audit"
}), RB_G({
  kind: "Group",
  name: "oidc:auditors"
}, "sb:pool-reader", scSc(c), {
  reason: "SOC2 audit"
})]), RB_G({
  kind: "Group",
  name: "oidc:auditors"
}, "sb:dr-reader", {
  kind: "dr-pair",
  name: "all pairs"
}, {
  reason: "SOC2 audit"
}),
// a RoleBinding someone made with kubectl — shown, not hidden (§4.5)
RB_G({
  kind: "User",
  name: "oidc:backup@simplyblock.io"
}, "sb:pool-admin", scSc(rb_c1), {
  name: "backup-ops-manual",
  source: "external",
  by: "kubectl"
}),
// expired: bindings removed by the controller, grant kept for the record
RB_G({
  kind: "Group",
  name: "oidc:team-a"
}, "sb:cluster-admin", scSc(rb_c2), {
  expiresAt: new Date(Date.now() - 86400e3 * 3).toISOString(),
  reason: "INC-2207 incident response"
})].filter(Boolean);
RB_DB.access_grants = RB_GRANTS;
RB_DB.access_users = RB_USERS;
RB_DB.access_roles = RB_ROLES;
const rbActive = g => !g.expires_at || Date.parse(g.expires_at) > Date.now();
const rbStatus = g => !rbActive(g) ? "Expired" : g.namespaces.length ? "Active" : "Unresolved";

// ---- who is asking ----------------------------------------------------------
const rbViewAs = () => {
  const n = localStorage.getItem("sb.viewas");
  return RB_USERS.find(u => u.name === n) || RB_USERS[0];
};
const rbSubjectMatches = (g, u) => g.subject.kind === "User" && g.subject.name === u.name || g.subject.kind === "Group" && (u.groups || []).includes(g.subject.name);
const rbGrantsFor = u => RB_GRANTS.filter(g => rbActive(g) && rbSubjectMatches(g, u));

// SelfSubjectRulesReview, one per namespace, aggregated (§5.3)
function rbRulesFor(u) {
  const out = {
    cluster: [],
    ns: {}
  };
  rbGrantsFor(u).forEach(g => {
    const rules = rbRoleRules(g.role).map(r => Object.assign({}, r, {
      via: g.role
    }));
    g.namespaces.forEach(n => {
      if (n === "*") out.cluster.push(...rules);else (out.ns[n] = out.ns[n] || []).push(...rules);
    });
  });
  return out;
}
const rbRuleAllows = (r, verb, resource, group, name) => (r.apiGroups.includes(group || "simplyblock.io") || r.apiGroups.includes("*")) && (r.resources.includes(resource) || r.resources.includes("*")) && (r.verbs.includes(verb) || r.verbs.includes("*")) && (!r.resourceNames || !name || r.resourceNames.includes(name));
function rbAllowed(u, verb, resource, ns, group, name) {
  const rules = rbRulesFor(u);
  if (rules.cluster.some(r => rbRuleAllows(r, verb, resource, group, name))) return true;
  return !!ns && ns !== "*" && (rules.ns[ns] || []).some(r => rbRuleAllows(r, verb, resource, group, name));
}
// §4.3: may `u` grant `role` in `ns`? bind on the ClusterRole by name, or every rule of it
const rbMayBind = (u, role, ns) => rbAllowed(u, "create", "accessgrants", ns) && (rbAllowed(u, "bind", "clusterroles", ns, RB_RBAC, role) || rbRoleRules(role).length > 0 && rbRoleRules(role).every(r => r.resources.every(res => r.verbs.every(v => rbAllowed(u, v, res, ns, r.apiGroups[0])))));

// ---- scope discovery (§5.2): SAR per candidate, only the permitted subtree ---
function rbScopes(u) {
  const canGet = (res, ns) => rbAllowed(u, "get", res, ns);
  const pools = RB_DB.pools.map(p => ({
    id: p.uuid,
    name: p.pool_name,
    clusterId: p.cluster_id,
    namespace: p.namespace,
    isolated: !!p.isolated,
    visible: canGet("storagepools", p.namespace)
  }));
  const clusters = RB_DB.clusters.map(c => {
    const ps = pools.filter(p => p.clusterId === c.uuid);
    const own = canGet("storageclusters", nsSc(c));
    return {
      id: c.uuid,
      name: c.name,
      namespace: nsSc(c),
      k8sIds: c.k8s_cluster_ids || [],
      pools: ps,
      visible: own || ps.some(p => p.visible),
      full: own,
      sharedPools: ps.filter(p => !p.isolated).length
    };
  });
  const managed = (RB_DB.k8s_clusters || []).map(k => {
    const cs = clusters.filter(c => c.k8sIds.includes(k.uuid));
    return {
      id: k.uuid,
      name: k.name,
      namespace: nsMc(k),
      visible: canGet("managedclusters", nsMc(k)) || cs.some(c => c.visible),
      clusters: cs.map(c => c.id)
    };
  });
  const dr = {
    namespace: RB_NS_DR,
    visible: canGet("drpolicies", RB_NS_DR),
    pairs: (RB_DB.dr_policies || []).map(p => ({
      id: p.uuid,
      name: p.name,
      sourceClusterId: p.source_cluster_id,
      targetClusterId: p.target_cluster_id
    }))
  };
  const apps = (RB_DB.protected_apps || []).map(a => {
    const c = rbClusterOf(a.source_cluster_id);
    const key = c ? nsApp(a.namespace, c) : null;
    return {
      id: a.uuid,
      name: `${a.namespace}/${a.app_name}`,
      clusterId: a.source_cluster_id,
      namespace: key,
      drPolicy: a.policy_id || null,
      visible: !!key && canGet("protectedapplications", key)
    };
  });
  return {
    managed,
    clusters,
    pools,
    dr,
    apps,
    // id -> namespace, so the client never slugs names itself
    ns: {
      clusters: Object.fromEntries(clusters.map(c => [c.id, c.namespace])),
      pools: Object.fromEntries(pools.map(p => [p.id, p.namespace])),
      managed: Object.fromEntries(managed.map(m => [m.id, m.namespace])),
      apps: Object.fromEntries(apps.map(a => [a.id, a.namespace])),
      dr: RB_NS_DR
    }
  };
}
const rbSelf = () => {
  const u = rbViewAs();
  return {
    user: u.name,
    groups: u.groups,
    initials: u.initials,
    label: u.label,
    rules: rbRulesFor(u),
    incomplete: false,
    grants: rbGrantsFor(u).map(g => ({
      uuid: g.uuid,
      role: g.role,
      scope: g.scope,
      namespaces: g.namespaces,
      subject: g.subject
    })),
    scopes: rbScopes(u),
    demo_users: RB_USERS.map(x => ({
      name: x.name,
      label: x.label,
      initials: x.initials
    }))
  };
};
const rbJ = (o, code) => new Response(JSON.stringify(o), {
  status: code || 200,
  headers: {
    "Content-Type": "application/json"
  }
});
const rbDeny = msg => rbJ({
  status: false,
  error: msg,
  reason: "Forbidden"
}, 403);
const rbBad = msg => rbJ({
  status: false,
  error: msg,
  reason: "Invalid"
}, 409);

// the AccessGrant webhook (§4.3, §4.4). Returns {error} or {grant}. `requester`
// comes from AdmissionRequest.userInfo — never from the body (invariant 4).
function rbAdmitGrant(u, body) {
  const sc = body.scope || {};
  if (!body.subject || !body.subject.name || !body.role) return {
    error: "subject and role are required"
  };
  if (!RB_ROLES.some(r => r.name === body.role)) return {
    error: `${body.role} is not one of the sb:* ClusterRoles. Roles are aggregated by the chart, not created here.`
  };
  if (body.expires_at && !(Date.parse(body.expires_at) > Date.now())) return {
    error: "expiresAt must be in the future"
  };
  const namespaces = rbScopeNamespaces(sc);
  if (!namespaces.length) return {
    error: "the scope does not resolve to a namespace"
  };
  if (sc.kind === "cluster-scope" && body.role !== "sb:infra-admin") return {
    error: "only sb:infra-admin is bound at cluster scope; every other role needs a namespace"
  };
  if (sc.kind !== "cluster-scope" && body.role === "sb:infra-admin") return {
    error: "sb:infra-admin is cluster-scoped"
  };
  // §4.4 DR symmetry: both members registered and reachable, or nothing
  if (sc.kind === "application" && sc.drPolicy) {
    const pol = (RB_DB.dr_policies || []).find(p => p.uuid === sc.drPolicy || p.name === sc.drPolicy);
    if (!pol) return {
      error: "the DR policy named in the scope does not exist"
    };
    const pair = (RB_DB.cluster_pairs || []).find(p => p.uuid === pol.pair_id);
    const members = [pol.source_cluster_id, pol.target_cluster_id].map(rbClusterOf);
    if (members.some(c => !c)) return {
      error: "a member cluster of the DR pair is not registered — refusing to bind one side only"
    };
    if (pair && pair.status === "unreachable") return {
      error: `the pair link to ${members[1].name} is down — bindings would land on ${members[0].name} only, so the grant is refused (both or neither)`
    };
    if (namespaces.length < 2) return {
      error: "DR fan-out resolved to a single namespace"
    };
  }
  // §4.3: the requester must be able to bind this role in EVERY target namespace
  const denied = namespaces.filter(n => !rbMayBind(u, body.role, n));
  if (denied.length) return {
    error: `${u.name} cannot bind ${body.role} in ${denied.join(", ")} — needs bind on that ClusterRole, or every right it grants, there`,
    forbidden: true
  };
  const g = RB_G(body.subject, body.role, sc, {
    name: body.name,
    expiresAt: body.expires_at || null,
    reason: body.reason || "",
    by: u.name,
    age: 0
  });
  g.created_at = RB_U.ago(0);
  return {
    grant: g,
    warning: body.subject.kind === "User" ? "Bound to a user, not a group. Offboarding this person now needs a Kubernetes action." : null
  };
}

// Called by the mock API server for /proposed/access*. Returns a Response or null.
window.SB_ACCESS_ROUTE = function (method, path, body) {
  const u = rbViewAs();
  const [p, qs] = path.split("?");
  const q = new URLSearchParams(qs || "");
  if (p === "/proposed/access/self") return rbJ({
    results: [rbSelf()],
    proposed: true
  });
  if (p === "/proposed/access/scopes") return rbJ({
    results: [rbScopes(u)],
    proposed: true
  });
  if (p === "/proposed/access-roles") return method === "GET" ? rbJ({
    results: RB_ROLES,
    proposed: true
  }) : rbJ({
    status: false,
    error: "sb:* roles are aggregated ClusterRoles owned by the simplyblock-crds chart. To extend one, label a ClusterRole simplyblock.io/aggregate-to-<role>: \"true\"."
  }, 405);
  if (p === "/proposed/access/effective") {
    const [kind, ...rest] = (q.get("subject") || "").split(":");
    const name = rest.join(":");
    const user = kind === "User" ? RB_USERS.find(x => x.name === name) || {
      name,
      groups: []
    } : {
      name: "",
      groups: [name]
    };
    const gs = RB_GRANTS.filter(g => rbSubjectMatches(g, user));
    return rbJ({
      results: [{
        subject: {
          kind,
          name
        },
        groups: user.groups,
        grants: gs,
        rules: rbRulesFor(user),
        note: "Assembled from grants issued through simplyblock. ClusterRoles the platform team created independently are not visible here."
      }],
      proposed: true
    });
  }
  if (p === "/proposed/access/review" && method === "POST") {
    // SubjectAccessReview for a named subject (§4.5 spot check)
    const s = body.subject || {};
    const user = s.kind === "User" ? RB_USERS.find(x => x.name === s.name) || {
      name: s.name,
      groups: []
    } : {
      name: "",
      groups: [s.name]
    };
    const allowed = rbAllowed(user, body.verb, body.resource, body.namespace, body.group);
    const via = allowed ? rbGrantsFor(user).filter(g => (g.namespaces.includes("*") || g.namespaces.includes(body.namespace)) && rbRoleRules(g.role).some(r => rbRuleAllows(r, body.verb, body.resource, body.group))).map(g => g.role) : [];
    return rbJ({
      results: [{
        allowed,
        via: [...new Set(via)],
        reason: allowed ? `granted via ${[...new Set(via)].join(", ")}` : `no rule grants ${body.verb} on ${body.resource} in ${body.namespace || "cluster scope"}`
      }]
    });
  }
  if (p === "/proposed/access/subjects") {
    const subs = {};
    RB_GRANTS.forEach(g => {
      subs[g.subject.kind + ":" + g.subject.name] = g.subject;
    });
    RB_USERS.forEach(x => {
      subs["User:" + x.name] = {
        kind: "User",
        name: x.name
      };
      x.groups.forEach(gr => {
        subs["Group:" + gr] = {
          kind: "Group",
          name: gr
        };
      });
    });
    return rbJ({
      results: Object.values(subs)
    });
  }
  if (p === "/proposed/access/invariants") return rbJ({
    results: rbInvariants()
  });
  if (p === "/proposed/access/namespaces") return rbJ({
    results: Object.entries(RB_NS).map(([name, v]) => Object.assign({
      name
    }, v))
  });
  let m = p.match(/^\/proposed\/access-grants(?:\/([\w-]+))?$/);
  if (m) {
    if (method === "GET") {
      // a grant is visible where the caller may get accessgrants (§5.1: 403, not empty, when nothing at all is visible)
      const vis = RB_GRANTS.filter(g => g.namespaces.some(n => rbAllowed(u, "get", "accessgrants", n)));
      if (!vis.length && !rbAllowed(u, "get", "accessgrants", "*")) return rbDeny(`${u.name} may not list accessgrants in any namespace`);
      const withStatus = vis.map(g => Object.assign({}, g, {
        status: rbStatus(g)
      }));
      return rbJ({
        results: m[1] ? withStatus.filter(g => g.uuid === m[1]) : withStatus,
        proposed: true
      });
    }
    if (method === "POST") {
      const r = rbAdmitGrant(u, body);
      if (r.error) return r.forbidden ? rbDeny(r.error) : rbBad(r.error);
      RB_GRANTS.push(r.grant);
      return rbJ({
        results: [Object.assign({
          warning: r.warning,
          status: "Active"
        }, r.grant)]
      });
    }
    const g = RB_GRANTS.find(x => x.uuid === m[1]);
    if (!g) return rbJ({
      status: false,
      error: "not found"
    }, 404);
    if (method === "DELETE") {
      if (g.source === "external") return rbBad(`${g.name} is a RoleBinding not owned by an AccessGrant — remove it with kubectl in ${g.namespaces.join(", ")}`);
      const denied = g.namespaces.filter(n => !rbAllowed(u, "delete", "accessgrants", n));
      if (denied.length) return rbDeny(`${u.name} may not delete accessgrants in ${denied.join(", ")}`);
      RB_GRANTS.splice(RB_GRANTS.indexOf(g), 1);
      return rbJ({
        results: [g]
      });
    }
    return rbJ({
      status: "ok"
    });
  }
  return null;
};

// ---- §6 invariants, run against this authorizer ------------------------------
function rbInvariants() {
  const maria = RB_USERS[1],
    jonas = RB_USERS[2],
    root = RB_USERS[0];
  const out = [];
  const T = (n, title, run) => {
    try {
      const r = run();
      out.push(Object.assign({
        n,
        title
      }, r));
    } catch (e) {
      out.push({
        n,
        title,
        ok: false,
        detail: "threw: " + e.message
      });
    }
  };
  T(1, "cluster-admin on one storage cluster reaches nothing in another", () => {
    const other = nsSc(rb_c1),
      own = nsSc(rb_c0);
    const leaks = ["get", "update", "delete"].filter(v => rbAllowed(maria, v, "storageclusters", other)).concat(rbAllowed(maria, "create", "accessgrants", other) ? ["grant"] : []);
    return {
      ok: !leaks.length && rbAllowed(maria, "update", "storageclusters", own),
      detail: leaks.length ? `leaks: ${leaks.join(", ")} in ${other}` : `no verb on ${other}; full on ${own}`
    };
  });
  T(2, "pool-admin cannot read nodes or devices, nor create a StorageClass", () => {
    const g = RB_G({
      kind: "Group",
      name: "probe-pool"
    }, "sb:pool-admin", scSc(rb_c0));
    RB_GRANTS.push(g);
    const only = {
      name: "probe",
      groups: ["probe-pool"]
    };
    const bad = Object.keys(RB_NS).filter(n => rbAllowed(only, "get", "storagenodes", n) || rbAllowed(only, "get", "devices", n) || rbAllowed(only, "create", "storageclasses", n, "storage.k8s.io"));
    RB_GRANTS.pop();
    return {
      ok: !bad.length,
      detail: bad.length ? `reachable in ${bad.join(", ")}` : "no storagenodes, devices or storage.k8s.io verbs in any namespace"
    };
  });
  T(3, "StorageCluster outside its NodePoolAllocation is rejected at admission", () => {
    const env = window.SB_ENVELOPE;
    if (!env) return {
      ok: null,
      detail: "admission half: no envelope validator loaded; node-agent half is backend-only"
    };
    const r = env({
      managedCluster: "x",
      nodeSelector: {
        "simplyblock.io/pool": "other"
      },
      devices: [{
        path: "/dev/disk/by-id/nvme-INTEL_1"
      }],
      isolatedCoresPerNode: 64
    }, {
      managedCluster: "x",
      allowedNodeSelector: {
        "simplyblock.io/pool": "storage-a"
      },
      allowedDevicePatterns: ["/dev/disk/by-id/nvme-SAMSUNG_MZ"],
      maxIsolatedCoresPerNode: 8
    });
    return {
      ok: r.length >= 3,
      detail: `${r.length} violation(s): ${r.join("; ")} — node agent's second gate is backend-only`
    };
  });
  T(4, "client-supplied actor annotations are overwritten", () => {
    const r = rbAdmitGrant(root, {
      subject: {
        kind: "Group",
        name: "oidc:probe"
      },
      role: "sb:cluster-reader",
      scope: scSc(rb_c0),
      created_by: "oidc:forged@evil",
      createdBy: "oidc:forged@evil"
    });
    return {
      ok: !!r.grant && r.grant.created_by === root.name,
      detail: r.grant ? `stamped ${r.grant.created_by}, body value ignored` : r.error
    };
  });
  out.push({
    n: 5,
    title: "delegated token scoped to pool1 is rejected for pool2 by the simplyblock API",
    ok: null,
    detail: "backend-only: token exchange and scope intersection live in the REST API (§3)"
  });
  T(6, "every console action maps to a (resource, verb)", () => {
    const A = window.ACTIONS || {},
      KE = window.KIND_ENTITY || {};
    const missing = Object.keys(A).filter(k => !KE[k]);
    return {
      ok: !missing.length,
      detail: missing.length ? `unmapped kinds: ${missing.join(", ")}` : `${Object.keys(A).length} action kinds mapped — the REST router half (§3.5) is backend-only`
    };
  });
  T(7, "no role can grant a role it may not bind", () => {
    const own = nsSc(rb_c0);
    let checked = 0,
      bad = [];
    RB_ROLE_NAMES.forEach(a => RB_ROLE_NAMES.forEach(b => {
      const holder = {
        name: "probe",
        groups: ["probe-" + a]
      };
      const g = RB_G({
        kind: "Group",
        name: "probe-" + a
      }, a, a === "sb:infra-admin" ? {
        kind: "cluster-scope"
      } : a.startsWith("sb:dr") ? {
        kind: "dr-pair",
        name: "x"
      } : a === "sb:app-admin" && rb_app0 ? {
        kind: "application",
        id: rb_app0.uuid
      } : scSc(rb_c0));
      RB_GRANTS.push(g);
      const allowed = rbMayBind(holder, b, own);
      RB_GRANTS.pop();
      checked++;
      const expected = a === "sb:infra-admin" || a === "sb:cluster-admin" && ["sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader"].includes(b);
      if (allowed !== expected) bad.push(`${a}→${b}`);
    }));
    return {
      ok: !bad.length,
      detail: bad.length ? `unexpected: ${bad.join(", ")}` : `${checked} role pairs, only infra-admin and cluster-admin (within its namespace) may bind`
    };
  });
  out.push({
    n: 8,
    title: "a UI backend bug cannot escalate",
    ok: null,
    detail: "every mutation here goes through the API server under the impersonated identity; the fixture re-authorizes accessgrants writes (403 on denied) — other kinds are backend-only"
  });
  T(9, "revoking IdP group membership removes access with no Kubernetes action", () => {
    const users = RB_GRANTS.filter(g => g.source === "control-center" && g.subject.kind === "User");
    const before = rbAllowed(maria, "update", "storageclusters", nsSc(rb_c0));
    const after = rbAllowed({
      name: maria.name,
      groups: ["system:authenticated"]
    }, "update", "storageclusters", nsSc(rb_c0));
    return {
      ok: before && !after && !users.length,
      detail: `group removal: ${before} → ${after}; ${users.length} control-center grant(s) bound to a user${users.length ? " — those need a Kubernetes action" : ""}`
    };
  });
  T(10, "a DR grant lands on both members or neither", () => {
    if (!rb_app0 || !rb_pol0) return {
      ok: null,
      detail: "no application with a DR policy in the fixture"
    };
    const pair = (RB_DB.cluster_pairs || []).find(p => p.uuid === rb_pol0.pair_id);
    const was = pair && pair.status;
    if (pair) pair.status = "unreachable";
    const r1 = rbAdmitGrant(root, {
      subject: {
        kind: "Group",
        name: "probe"
      },
      role: "sb:app-admin",
      scope: {
        kind: "application",
        id: rb_app0.uuid,
        drPolicy: rb_pol0.uuid
      }
    });
    if (pair) pair.status = was;
    const r2 = rbAdmitGrant(root, {
      subject: {
        kind: "Group",
        name: "probe"
      },
      role: "sb:app-admin",
      scope: {
        kind: "application",
        id: rb_app0.uuid,
        drPolicy: rb_pol0.uuid
      }
    });
    const ok = !!r1.error && !!r2.grant && r2.grant.namespaces.length === 2;
    return {
      ok,
      detail: `link down → ${r1.error ? "refused" : "ACCEPTED"}; link up → ${r2.grant ? r2.grant.namespaces.length + " namespaces" : r2.error}`
    };
  });
  return out;
}

// §2.4 envelope, the admission half — used by the deploy wizard's dry run
window.SB_ENVELOPE = function (spec, alloc) {
  const v = [];
  if (spec.managedCluster !== alloc.managedCluster) v.push("targets a managed cluster outside the bound allocation");
  if ((spec.isolatedCoresPerNode || 0) > alloc.maxIsolatedCoresPerNode) v.push("requested core isolation exceeds the allocation budget");
  const sel = spec.nodeSelector || {};
  if (!Object.keys(sel).every(k => k in alloc.allowedNodeSelector && sel[k] === alloc.allowedNodeSelector[k])) v.push("node selector outside the allocation");
  if (!(spec.devices || []).every(d => alloc.allowedDevicePatterns.some(p => (d.path || "").startsWith(p)))) v.push("device path outside the allocation");
  return v;
};
Object.assign(window, {
  ACCESS_ROLE_NAMES: RB_ROLE_NAMES
});
})();