// ---------------------------------------------------------------------------
// ACTION LAYER — per-object command registry, kebab menus and the
// parameterised confirm dialogs they open. Every command maps 1:1 to a
// mutation endpoint in api.jsx.
// ---------------------------------------------------------------------------
const GBn = 1e9;
const gb = n => Math.round((n || 0) / GBn);
const qosFields = q => [
  {k: "rw_ios_per_sec", label: "Max read+write IOPS", type: "number", min: 0, def: (q && q.rw_ios_per_sec) || 0, placeholder: "0 = unlimited"},
  {k: "rw_mbytes_per_sec", label: "Max read+write throughput", unit: "MB/s", type: "number", min: 0, def: (q && q.rw_mbytes_per_sec) || 0},
  {k: "r_mbytes_per_sec", label: "Max read throughput", unit: "MB/s", type: "number", min: 0, def: (q && q.r_mbytes_per_sec) || 0},
  {k: "w_mbytes_per_sec", label: "Max write throughput", unit: "MB/s", type: "number", min: 0, def: (q && q.w_mbytes_per_sec) || 0},
  {k: "n1", type: "note", label: "0 means unlimited. Setting every field to 0 removes the QoS profile."}
];

const configureHostDialog = h => {
  const sockets = Array.from({length: h.sockets || 1}, (_, s) => s);
  const mp = !!(REG[h.clusterId] && REG[h.clusterId].multipathing);
  return {
    title: `Configure ${h.hostname} as a storage host`, confirm: "Apply configuration",
    desc: "Runs the storage-plane configuration on the inspected node. Only the resources you pick here are handed to simplyblock; everything else stays with the kubelet.",
    fields: [
      {k: "numa_sockets", label: "NUMA socket(s) to use", type: "multiselect", required: true,
        options: sockets.map(s => ({v: String(s),
          l: `socket ${s} · ${h.devices.filter(d => d.socket === s).length} device(s) · ${h.nics.filter(n => n.socket === s).length} NIC(s)`}))},
      {k: "memory_per_pod", label: "System memory per storage-plane pod", unit: "GB", type: "number", min: 8,
        def: Math.max(16, Math.round(h.memory / 1e9 / 4)), required: true},
      {k: "hugepages_per_pod", label: "Hugepages per pod", unit: "GB", type: "number", min: 4,
        def: Math.max(8, Math.round(h.memory / 1e9 / 8)), required: true},
      {k: "device_ids", label: "Devices to assign", type: "multiselect", required: true,
        options: h.devices.map(d => ({v: d.id,
          l: `${d.kind === "nvme" ? d.pcie : d.blockdev} · socket ${d.socket} · ${fmtBytes(d.size)} · ${d.model}`})),
        empty: "The inspection pod found no usable device on this node."},
      {k: "mgmt_nic", label: "Management NIC", type: "select", required: true,
        options: h.nics.map(n => ({v: n.name, l: `${n.name} · ${n.address} · ${n.speed}G · ${n.state}`}))},
      {k: "data_nics", label: mp ? "Data NICs — exactly two required" : "Data NIC", type: "multiselect", required: true,
        options: h.nics.map(n => ({v: n.name, l: `${n.name} · ${n.address} · ${n.speed}G · socket ${n.socket} · ${n.state}`}))},
      {k: "n22", type: "note", label: mp
        ? "This cluster uses multipathing, so every storage node needs two data NICs. Pick two on different NUMA sockets where possible."
        : "This cluster does not use multipathing, so a single data NIC is expected."},
      {k: "n4", type: "note", label: "After configuration the host is labelled and appears as available — you can then start a storage node on it."}
    ],
    run: v => api.hostConfigure(h.id, v)
  };
};

const newPoolDialog = cluster => ({
  title: `New pool on ${cluster.name}`, confirm: "Create pool", done: "Pool created",
  desc: "A pool groups volumes and carries their QoS ceiling. Bi-directional DH-CHAP is decided here and cannot be changed afterwards.",
  fields: [
    {k: "name", label: "Pool name", type: "text", required: true, placeholder: "prod-oltp"},
    {k: "dhchap_bidirectional", label: "Bi-directional DH-CHAP", type: "checkbox", def: false},
    {k: "n70", type: "note", label: "With DH-CHAP the host and the subsystem authenticate each other on every connection. It applies to every volume in the pool, is fixed for the pool's lifetime, and cannot be added later — create a second pool if you need both."},
    {k: "max_rw_iops", label: "Max IOPS", type: "number", min: 0, def: 0, sub: "0 = unlimited"},
    {k: "max_rw_mbytes", label: "Max throughput", type: "number", min: 0, def: 0, sub: "MB/s, 0 = unlimited"}
  ],
  run: v => api.poolCreate(cluster.id, v)
});

// A plan needs its sites up front: DRCluster and DRPolicy fields are immutable,
// so every pair a method could ever use is derived at creation.
const newPlanDialog = sites => ({
  title: "New protection plan", confirm: "Create plan", done: "Plan created",
  desc: "A plan names the sites it spans and the storage profile its volumes come from. Methods are added afterwards — each one declares a protection relationship and derives its own policy and replication class.",
  fields: [
    {k: "name", label: "Plan name", type: "text", required: true, placeholder: "gold",
      sub: "becomes the storageID suffix and the vault bucket name"},
    {k: "site_names", label: "Sites", type: "multiselect", required: true,
      options: (sites || []).map(s => ({v: s.name, l: `${s.name} · ${s.region}`})),
      sub: "at least two. Two sites in one region can mirror synchronously; a site in another region cannot"},
    {k: "storage_profile", label: "Storage profile", type: "select", def: "sb-nvme-gold",
      options: [{v: "sb-nvme-gold", l: "sb-nvme-gold"}, {v: "sb-nvme-standard", l: "sb-nvme-standard"}],
      sub: "the StorageClass, and the storageID Ramen intersects across sites"},
    {k: "n98", type: "note", label: "The storageID granularity has to be at least as coarse as an application's volume set — Ramen derives the consistency-group boundary from it, so volumes on different storageIDs cannot be restored crash-consistently together."}
  ],
  run: v => api.planCreate(v)
});

const addMethodDialog = plan => ({
  title: `Add a method to ${plan.name}`, confirm: "Add method", done: "Method added",
  desc: "A method is one declared protection relationship. All three types are configured identically and travel the same path to the driver; what differs is the parameters the class carries and therefore what the driver does with the data.",
  fields: v => [
    {k: "name", label: "Method name", type: "text", required: true, placeholder: "regional"},
    {k: "type", label: "Type", type: "select", required: true, def: "async", options: [
      {v: "sync", l: "synchronous — inline mirror to a peer, RPO 0, failback yes"},
      {v: "async", l: "asynchronous — block delta per epoch to a peer, RPO = interval, failback yes"},
      {v: "snapshot-s3", l: "generation vault — immutable snapshots to S3, generation select, no failback"}
    ]},
    {k: "target", label: "Target site", type: "select", required: true,
      options: plan.siteNames.slice(1).map(s => ({v: s, l: s})),
      sub: v.type === "sync" ? "must be in the same region as the source site"
        : v.type === "snapshot-s3" ? "the site a restore materialises on — the bucket itself is not a site" : "the peer that holds the replica"},
    v.type !== "sync" ? {k: "interval", label: "Interval", type: "text", required: true,
      def: v.type === "snapshot-s3" ? "1h" : "5m", placeholder: "5m",
      sub: "the target RPO. Emitted to the policy and to the class parameters as one value"} : null,
    v.type === "snapshot-s3" ? {k: "bucket", label: "Bucket", type: "text", def: `sb-vault-${plan.name}`} : null,
    v.type === "snapshot-s3" ? {k: "retention_hourly", label: "Hourly generations", type: "number", def: 24, min: 0} : null,
    v.type === "snapshot-s3" ? {k: "retention_daily", label: "Daily generations", type: "number", def: 14, min: 0} : null,
    v.type === "snapshot-s3" ? {k: "retention_weekly", label: "Weekly generations", type: "number", def: 8, min: 0} : null,
    v.type === "snapshot-s3" ? {k: "immutable", label: "Object Lock (compliance)", type: "checkbox", def: true,
      sub: "generations cannot be deleted before their lock expires — the guarantee the vault exists for"} : null,
    v.type === "sync" ? {k: "n99", type: "note", label: "Synchronous protection needs both sites in one region: it is declared by an equal region on the two DRClusters, and the mirror is inline so every write waits for the peer."} : null,
    v.type === "snapshot-s3" ? {k: "n100", type: "note", label: "There is no failback from a vault restore. The source volume is gone or untrusted and the vault holds generations rather than a live peer, so the original site's pre-compromise state cannot be reconstructed — the restored application is re-protected as a new plan with a full baseline."} : null
  ].filter(Boolean),
  run: v => api.planAddMethod(plan.id, v)
});

const restoreGenerationDialog = (a, g) => ({
  title: `Restore ${a.namespace}/${a.name} from generation ${g.generation}`, danger: true,
  desc: `Taken ${fmtDate(g.at)} · ${g.tier} tier · ${fmtBytes(g.size)}${g.kind === "full" ? " · full baseline" : " · delta"}. The volumes are materialised from this generation's objects at ${a.restoreTargets.join(", ") || "the restore target"}, and the application is brought up from them.`,
  fields: [
    {k: "n101", type: "note", label: "The generation is pinned out of band immediately before the placement rebind, because PromoteVolume carries no point-in-time argument. The driver resolves the pin when the promote arrives, and the pin is cleared afterwards so a later ordinary failover cannot resolve to an old generation."},
    g.consistencyGroup ? {k: "n102", type: "note", label: `This generation belongs to consistency group ${g.consistencyGroup}, so its volumes are crash-consistent with each other as of that moment.`} : null,
    {k: "n103", type: "note", label: "Failback will not be available afterwards. Re-protect the restored application as a new plan; it will take a full baseline."}
  ].filter(Boolean),
  confirm: "Pin and restore", run: () => api.appRestore(a.id, g.generation),
  done: "Generation pinned — the placement control is rebinding"
});

// Both clusters must be StorageClusters in this namespace with status.uuid
// populated. Cross-namespace references are not supported.
const newPairDialog = () => ({
  title: "New replication pair", confirm: "Create pair", done: "ReplicationPair created",
  desc: "A pair names a source and a target cluster and is reusable: several policies can replicate between the same two clusters on different schedules. Both clusters must be attached to this control plane — cross-namespace references are not supported.",
  fields: [
    {k: "name", label: "Name", type: "text", required: true, placeholder: "prod-to-dr",
      sub: "metadata.name, DNS-1123"},
    {k: "sourceCluster", label: "Source cluster", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.filter(c => c.status !== "unready")
        .map(c => ({v: c.name, l: `${c.name} · ${c.status}`}))),
      sub: "the local StorageCluster; its status.uuid must be populated"},
    {k: "targetCluster", label: "Target cluster", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.map(c => ({v: c.name, l: `${c.name} · ${c.status}`}))),
      sub: "immutable after creation — changing a target means a new pair"},
    {k: "n70", type: "note", label: "The operator creates the backend replication target and records its ID in status.backendTargetID. No policy on the pair replicates until status.ready is true."}
  ],
  run: v => api.pairCreateCrd(v.name, v.sourceCluster, v.targetCluster)
});

const newReplPolicyDialog = pair => ({
  title: pair ? `New policy on ${pair.name}` : "New replication policy",
  confirm: "Create policy", done: "ReplicationPolicy created",
  desc: "One interval and one snapshot count. There is no tiered retention schedule and no synchronous mode — mode is exactly failover or migration.",
  fields: v => [
    {k: "name", label: "Name", type: "text", required: true, placeholder: "prod-to-dr-5m"},
    pair ? null : {k: "pairRef", label: "Replication pair", type: "select", required: true,
      load: () => api.pairs().then(ps => ps.map(p => ({v: p.name,
        l: `${p.name} · ${p.sourceCluster} → ${p.targetCluster}${p.ready ? "" : " · not ready"}`}))),
      empty: "No ReplicationPair exists yet. Create one first."},
    {k: "mode", label: "Mode", type: "select", def: "failover", options: [
      {v: "failover", l: "failover — target is a read-only DR standby"},
      {v: "migration", l: "migration — planned online cutover to the target"}
    ]},
    {k: "interval", label: "Interval", type: "text", def: "5m", required: true,
      sub: "rounded to whole minutes, minimum 1m; an unparseable value falls back to 5m"},
    {k: "snapshotRetention", label: "Snapshot retention", type: "number", def: 3, min: 2,
      sub: "minimum snapshots kept on the target — the CRD floor is 2"},
    v.mode === "migration"
      ? {k: "n71", type: "note", label: "A migration policy cuts over per volume: replicating → cutover_pending → cutover_done, with both clusters up throughout."}
      : {k: "n71", type: "note", label: "Failover is never automatic. Promoting the target is always an explicit ReplicationOps."},
    {k: "n72", type: "note", label: "Volumes are not added to a policy. A PVC opts in through the storage.simplyblock.io/replication-policy annotation, and the operator creates one ReplicationSlot per bound PVC."}
  ].filter(Boolean),
  run: v => api.rpolicyCreateCrd(v.name, {
    pairRef: pair ? pair.name : v.pairRef, mode: v.mode,
    interval: v.interval, snapshotRetention: Number(v.snapshotRetention)
  })
});

// Membership is an annotation write. Nothing else.
const attachPvcDialog = pol => ({
  title: `Attach a PVC to ${pol.name}`, confirm: "Write annotation",
  done: "Annotation written — the operator creates the slot",
  desc: `Sets ${REPL_ANNOTATION} on the PVC. The operator creates a ReplicationSlot named <policy>-<pvc> once the claim is Bound, and owns it from then on.`,
  fields: [
    {k: "pvc", label: "PVC", type: "select", required: true,
      load: () => api.pvcs().then(ps => ps.filter(p => p.status === "Bound")
        .map(p => ({v: `${p.name}|${p.namespace}`,
          l: `${p.namespace}/${p.name}${p.replicationPolicy ? ` · already on ${p.replicationPolicy}` : ""}`}))),
      sub: "only Bound PVCs — a slot is created once the claim is bound"},
    {k: "n73", type: "note", label: "Annotating the StorageClass instead replicates every volume it provisions. Where both carry the annotation, the PVC wins."},
    {k: "n74", type: "note", label: "Repointing a PVC that is already replicating is a detach followed by a fresh attach, so the new target takes a full copy."}
  ],
  run: v => { const [name, namespace] = v.pvc.split("|"); return api.pvcSetReplPolicy(name, namespace, pol.name); }
});

const detachPvcDialog = slot => ({
  title: `Detach ${slot.pvcRef} from ${slot.policyRef}?`, danger: true,
  desc: `Clears ${REPL_ANNOTATION} on the PVC. The operator deletes the replication snapshots on both sides and then removes the slot. The source volume itself is untouched.`,
  confirm: "Clear annotation", done: "Annotation cleared",
  run: () => api.pvcSetReplPolicy(slot.pvcRef, slot.pvcNamespace, null)
});

// Every failover, failback and cutover is a ReplicationOps. Scope decides how
// much it covers; failback cannot be target-scoped.
const replOpsDialog = ({action, scope, ref, obj}) => {
  const meta = OPS_ACTION_META[action] || {};
  const n = obj && obj.counts ? obj.counts.slots : null;
  return {
    title: `${meta.label || action} — ${scope} ${ref}`, danger: !!meta.danger,
    desc: `${meta.desc} Scope ${scope}: ${SCOPE_HINT[scope]}${n != null ? ` (${n} volume(s))` : ""}.`,
    fields: [
      action === "migration" ? {k: "deleteSource", label: "Delete the source volume after the cutover",
        type: "checkbox", def: false, sub: "spec.deleteSource — only meaningful for a migration"} : null,
      action === "failback" ? {k: "sourceClusterID", label: "Recover into a different cluster", type: "text",
        placeholder: "leave empty for the original source",
        sub: "spec.sourceClusterID — omit to recover to the original source"} : null,
      action === "failback" ? {k: "n75", type: "note",
        label: "A short write freeze holds while the final delta transfers. Plan a maintenance window."} : null,
      {k: "n76", type: "note", label: "Per-volume outcomes are independent: one volume can fail while the rest succeed, and the operation as a whole then ends Failed."},
      {k: "n77", type: "note", label: "The operation is one-shot. Once it reaches Succeeded or Failed it is never re-run — a repeat needs a new ReplicationOps."},
      obj && obj.activeOpsRef ? {k: "n78", type: "note",
        label: `${obj.activeOpsRef} currently holds the lock. This operation waits for it rather than failing.`} : null
    ].filter(Boolean),
    confirm: meta.label || action,
    done: "ReplicationOps created — it runs to a terminal phase",
    run: v => api.replOpsCreate(Object.assign({action, scope, ref},
      action === "migration" && v.deleteSource ? {deleteSource: true} : {},
      action === "failback" && v.sourceClusterID ? {sourceClusterID: v.sourceClusterID} : {}))
  };
};

const volumeOpsDialog = slot => ({
  title: `Operate on ${slot.pvcRef}`, confirm: "Create operation",
  desc: "A volume-scoped ReplicationOps affects exactly this slot. Which actions apply depends on the state it is in.",
  fields: v => [
    {k: "action", label: "Action", type: "select", required: true,
      def: slot.state === "failed_over" ? "failback" : slot.state === "cutover_pending" ? "migration" : "failover",
      options: [
        {v: "failover", l: "fail over — promote the target, unplanned"},
        {v: "failback", l: "fail back — restore the source as primary"},
        {v: "migration", l: "cut over — commit the planned migration"}
      ]},
    v.action === "migration" ? {k: "deleteSource", label: "Delete the source volume after the cutover", type: "checkbox", def: false} : null,
    slot.state === "error" ? {k: "n79", type: "note",
      label: `This slot is in error: ${slot.message} The backend is likely to refuse the operation for it.`} : null,
    {k: "n80", type: "note", label: `Current state: ${slot.state}. ${smeta(slot.state).hint}`}
  ].filter(Boolean),
  run: v => api.replOpsCreate(Object.assign({action: v.action, scope: "volume", ref: slot.name},
    v.action === "migration" && v.deleteSource ? {deleteSource: true} : {}))
});

const newPairDialogLegacy = () => ({
  title: "Pair two clusters", confirm: "Create pair",
  desc: "A pair is a one-way link. Replication policies attach to pairs, so replicating both ways means creating both a→b and b→a. Fan-out is the same: one pair per route.",
  fields: [
    {k: "source_cluster_id", label: "Source cluster", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.filter(c => c.caps.async_replication)
        .map(c => ({v: c.id, l: `${c.name} · ${c.siting}`}))),
      empty: "No cluster available as a replication source."},
    {k: "target_cluster_id", label: "Target cluster", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.filter(c => c.drEligible)
        .map(c => ({v: c.id, l: `${c.name} · ${c.siting} · ${fmtBytes(c.capacity.total - c.capacity.used)} free`}))),
      empty: "No cluster is qualified as a DR target."},
    {k: "bandwidth_mbit", label: "Provisioned link bandwidth", unit: "Mbit/s", type: "number", min: 100, def: 10000},
    {k: "n6", type: "note", label: "Only clusters flagged as DR-eligible can be targets. The pair starts in the pairing state until the first handshake succeeds."}
  ],
  run: v => api.pairCreate(v)
});

const RET_INTERVALS = ["5m", "15m", "30m", "1h", "6h", "12h", "1d", "7d"];
const newRPolicyDialog = ctx => ({
  title: "Create a replication policy", confirm: "Create policy",
  desc: "An asynchronous policy is a cluster pair plus a consistency group — the group already carries the frequency and retention, so the policy needs nothing else. A synchronous policy is a stretched cluster and its zones, and has no schedule at all.",
  fields: v => {
    const sync = v.mode === "synchronous";
    return [
      {k: "name", label: "Policy name", type: "text", placeholder: "dr-prod-eu-to-us-east", required: true},
      {k: "mode", label: "Mode", type: "select", def: ctx && ctx.mode ? ctx.mode : "asynchronous",
        options: [{v: "asynchronous", l: "Asynchronous — between two clusters"}, {v: "synchronous", l: "Synchronous — across zones of one cluster"}]},
      sync ? {k: "source_cluster_id", label: "Stretched cluster", type: "select", required: true,
        load: () => api.clusters().then(cs => cs.filter(c => c.counts.zones > 1)
          .map(c => ({v: c.id, l: `${c.name} · ${c.counts.zones} zones`}))),
        empty: "No cluster spans two or more zones. Assign a cluster to several zones first."} : null,
      sync ? {k: "zone_ids", label: "Zones in the write quorum", type: "multiselect", required: true,
        load: () => api.zones().then(ss => ss.map(s => ({v: s.id, l: `${s.name} · ${s.location}`}))),
        empty: "No zones defined."} : null,
      sync ? null : {k: "pair_id", label: "Cluster pair", type: "select", required: true,
        load: () => api.pairs().then(ps => ps.filter(p => p.status !== "unreachable").map(p => ({
          v: p.id, l: `${regName(p.sourceClusterId)} → ${regName(p.targetClusterId)} · ${p.link.rtt_ms} ms`}))),
        empty: "No usable cluster pair. Pair two clusters first."},
      sync ? null : {k: "cg_id", label: "Consistency group", type: "select", required: true,
        load: () => api.pairs().then(ps => {
          const p2 = ps.find(x => x.id === v.pair_id);
          return p2 ? api.cgroups(p2.sourceClusterId) : Promise.resolve([]);
        }).then(gs => gs.filter(g => g.replicationConfig).map(g => ({v: g.id,
          l: `${g.name} · ${g.counts.volumes} volumes · every ${g.replicationConfig.frequency} min · ${g.replicationConfig.retention.reduce((a2, r) => a2 + r.keep, 0)} generations`}))).catch(() => []),
        empty: "No consistency group on the pair's source cluster has a replication cadence. Attach replication to a group first — the group owns the frequency and retention."},
      sync ? null : {k: "n84", type: "note", label: "The policy replicates exactly that group's volumes, on the cadence the group defines. Change the frequency or the retention on the group, not here."},
      sync ? null : {k: "retention", label: "Retained snapshot generations", type: "schedule",
        def: [{interval: "5m", keep: 10}, {interval: "15m", keep: 4}, {interval: "1h", keep: 11}, {interval: "1d", keep: 6}]},
      sync ? null : {k: "consistency_group", label: "Create a consistency group for the member volumes", type: "checkbox", def: true},
      sync ? null : {k: "failback_mode", label: "Failback policy", type: "select", def: "manual",
        options: [{v: "manual", l: "Manual — operator initiates failback"}, {v: "automatic", l: "Automatic — on source recovery"}]},
      sync ? null : {k: "resync_full", label: "Full resync on failback (instead of delta)", type: "checkbox"},
      {k: "n7", type: "note", label: sync
        ? "Writes are acknowledged only when every listed zone has them — no frequency, no backlog and no consistency group. Volumes can be added to and removed from the policy at any time."
        : "Volumes are attached after the policy exists. A policy can only be deleted once no volumes are attached."}
    ].filter(Boolean);
  },
  run: v => api.rpolicyCreate(v)
});

const newMigrationDialog = cluster => ({
  title: "Migrate storage", confirm: "Start migration",
  desc: "Within a cluster, volumes move between nodes by instant migration. Between clusters or zones, asynchronous replication ships the data and a brief IO freeze rolls the NVMe paths over at the end.",
  fields: v => {
    const cross = v.mode === "cross_cluster";
    return [
      {k: "name", label: "Migration name", type: "text", placeholder: "move-prod-to-dc2", required: true},
      {k: "mode", label: "Mechanism", type: "select", def: "intra_cluster",
        options: [{v: "intra_cluster", l: "Within the cluster — instant volume migration"},
          {v: "cross_cluster", l: "Between clusters / zones — replicate, then roll over"}]},
      {k: "scope", label: "Scope", type: "select", def: "volumes",
        options: [{v: "volumes", l: "Selected volumes"}, {v: "cluster", l: "The whole cluster"}]},
      v.scope !== "cluster" ? {k: "lvol_ids", label: "Volumes to migrate", type: "multiselect", required: true,
        load: () => api.clusterVolumes(cluster.id).then(vs => vs.filter(x => x.status === "online")
          .map(x => ({v: x.id, l: `${x.name} · ${x.poolName} · ${fmtBytes(x.capacity.used)} used`}))),
        empty: "No online volume in this cluster."} : null,
      cross ? {k: "target_cluster_id", label: "Target cluster", type: "select", required: true,
        load: () => api.pairs().then(ps => ps.filter(p => p.sourceClusterId === cluster.id && p.status !== "unreachable")
          .map(p => ({v: p.targetClusterId, l: `${regName(p.targetClusterId)} · ${p.link.rtt_ms} ms`}))),
        empty: "No usable cluster pair from this cluster. Pair it with the target first."} : null,
      cross ? null : {k: "target_zone_id", label: "Target zone", type: "select",
        load: () => api.clusterZones(cluster.id).then(ss => ss.map(x => ({v: x.id, l: `${x.name} · ${x.location}`}))),
        empty: "This cluster spans a single zone — targets come from the taint alone."},
      {k: "target_taint", label: "Target host taint", type: "text", def: "simplyblock.io/migration-target=true"},
      cross ? {k: "freeze_threshold_mb", label: "Freeze threshold", unit: "MB", type: "number", min: 16, def: 256} : null,
      cross ? {k: "iteration_limit", label: "Max snapshot iterations", type: "number", min: 2, def: 12} : null,
      {k: "follow_workload", label: "Follow the workload", type: "checkbox", def: true},
      {k: "n27", type: "note", label: cross
        ? "Snapshots are taken iteratively until the outstanding delta is under the freeze threshold. You then trigger the cutover, which freezes IO for a moment and rolls the NVMe paths over to the target."
        : "Taint the destination hosts first — only tainted hosts that already run a storage node can receive volumes."}
    ].filter(Boolean);
  },
  run: v => api.migrationCreate(cluster.id, v)
});

const protectAppDialog = () => ({
  title: "Protect an application", confirm: "Protect",
  desc: "An application binds a workload to a protection plan. The plan's methods become its legs; Ramen drives one of them, and the rest replicate in the data plane with identical parameters.",
  fields: v => [
    {k: "plan_id", label: "Protection plan", type: "select", required: true,
      load: () => api.plans().then(ps => ps.filter(p => p.methods.length).map(p => ({v: p.id,
        l: `${p.name} · ${p.methods.map(m => `${m.name} (${mmeta(m.type).short})`).join(", ")} · ${p.siteNames.length} sites`}))),
      empty: "No plan declares a method yet. Create a plan under Disaster recovery and add at least one method."},
    {k: "preferred_site", label: "Preferred site", type: "select", required: true,
      load: () => v.plan_id ? api.plan(v.plan_id).then(p => p.siteNames.map(s => ({v: s, l: s}))) : Promise.resolve([]),
      sub: "where the workload normally runs — becomes DRPC.spec.preferredCluster",
      empty: "Choose a plan first."},
    {k: "orchestrated_method", label: "Orchestrated method", type: "select", required: true,
      load: () => v.plan_id ? api.plan(v.plan_id).then(p => p.methods.filter(m => m.type !== "snapshot-s3")
        .map(m => ({v: m.name, l: `${m.name} · ${mmeta(m.type).label} → ${m.target}`}))) : Promise.resolve([]),
      sub: "the one method Ramen drives through this application's single placement control",
      empty: "This plan declares only a vault method, so that is what Ramen will drive."},
    {k: "n89", type: "note", label: "A placement control selects PVCs by label, so two over the same PVCs would both claim them — the documented outcome is data corruption. One application therefore names one orchestrated method; the others still replicate, and report their lag out of band."},
    {k: "app_name", label: "Application name", type: "text", placeholder: "postgres-ha", required: true},
    {k: "namespace", label: "Namespace", type: "text", placeholder: "db-prod", required: true},
    {k: "app_kind", label: "Deployed as", type: "select", def: "ApplicationSet",
      options: [{v: "ApplicationSet", l: "ApplicationSet"}, {v: "Subscription", l: "Subscription"}, {v: "VirtualMachine", l: "VirtualMachine"}]},
    {k: "pvc_selector_key", label: "PVC selector label", type: "text", def: "app.kubernetes.io/name", required: true},
    {k: "pvc_selector_value", label: "Selector value", type: "text", placeholder: "postgres", required: true,
      sub: "never leave the selector empty — an empty selector claims every PVC in the namespace"},
    {k: "kube_object_protection", label: "Also protect Kubernetes objects", type: "checkbox", def: true},
    {k: "n28", type: "note", label: "Without Kubernetes object protection a failover moves the volumes only: the workload has to be recreated by hand and the application parks in WaitForUser."}
  ],
  run: v => api.appProtect(v)
});


const newCgroupDialog = cluster => ({
  title: "Create a consistency group", confirm: "Create group",
  desc: "A consistency group is a named set of volumes. Group snapshots capture every member at the same instant so they restore to one common point in time.",
  fields: [
    {k: "name", label: "Group name", type: "text", placeholder: "sap-hana-prod", required: true},
    {k: "lvol_ids", label: "Member volumes", type: "multiselect", required: true,
      load: () => api.clusterVolumes(cluster.id).then(vs => vs.filter(v => v.status === "online")
        .map(v => ({v: v.id, l: `${v.name} · ${v.poolName} · ${fmtBytes(v.capacity.total)}`
          + ((v.consistencyGroups || []).length ? ` · also in ${v.consistencyGroups.map(g => g.name).join(", ")}` : "")}))),
      empty: "No online volume in this cluster."},
    {k: "n82", type: "note", label: "Groups may overlap: a volume can belong to several consistency groups. That is how a differently-scoped crash-consistent set gets its own protection without disturbing an existing group."},
    {k: "n18", type: "note", label: "At least two volumes. A volume can belong to only one consistency group. Groups exist independently of replication."}
  ],
  run: v => api.cgroupCreate(cluster.id, v)
});

const kmsDialog = c => {
  const k = c.kms || {};
  return {
    title: `External key management for ${c.name}`, confirm: "Save KMS configuration",
    desc: "The cluster never stores a data encryption key in the clear: each encrypted volume's key is wrapped by a key held in your KMS. Saving does not rekey anything — existing volumes keep referencing their current key.",
    fields: v => {
      const p = v.provider || k.provider || "hashicorp_vault";
      const base = [{k: "provider", label: "Provider", type: "select", def: p,
        options: [{v: "hashicorp_vault", l: "HashiCorp Vault"}, {v: "aws_kms", l: "AWS KMS"},
          {v: "azure_key_vault", l: "Azure Key Vault"}, {v: "gcp_kms", l: "Google Cloud KMS"},
          {v: "kmip", l: "KMIP appliance"}]}];
      const perProvider = p === "hashicorp_vault" ? [
        {k: "address", label: "Vault address", type: "text", def: k.address || "", placeholder: "https://vault.internal:8200", required: true},
        {k: "namespace", label: "Namespace", type: "text", def: k.namespace || "", placeholder: "admin/storage (Enterprise)"},
        {k: "auth_method", label: "Auth method", type: "select", def: k.auth_method || "kubernetes",
          options: [{v: "kubernetes", l: "Kubernetes service account"}, {v: "approle", l: "AppRole"}, {v: "token", l: "Token"}]},
        {k: "auth_role", label: "Role", type: "text", def: k.auth_role || "simplyblock-storage"},
        {k: "mount_path", label: "Transit mount", type: "text", def: k.mount_path || "transit", required: true}
      ] : p === "aws_kms" ? [
        {k: "address", label: "Endpoint", type: "text", def: k.address || "https://kms.eu-central-1.amazonaws.com", required: true},
        {k: "region", label: "Region", type: "text", def: k.region || "eu-central-1", required: true},
        {k: "auth_method", label: "Auth method", type: "select", def: "irsa",
          options: [{v: "irsa", l: "IRSA — service account role"}, {v: "access_key", l: "Access key"}]},
        {k: "auth_role", label: "Role ARN", type: "text", def: k.auth_role || "arn:aws:iam::…:role/simplyblock-kms"}
      ] : p === "azure_key_vault" ? [
        {k: "address", label: "Vault URI", type: "text", def: k.address || "https://sb-kv.vault.azure.net", required: true},
        {k: "auth_method", label: "Auth method", type: "select", def: "workload_identity",
          options: [{v: "workload_identity", l: "Workload identity"}, {v: "client_secret", l: "Client secret"}]},
        {k: "auth_role", label: "Client / tenant id", type: "text", def: k.auth_role || ""}
      ] : p === "gcp_kms" ? [
        {k: "address", label: "Key ring resource", type: "text", def: k.address || "projects/…/locations/…/keyRings/simplyblock", required: true},
        {k: "auth_method", label: "Auth method", type: "select", def: "workload_identity",
          options: [{v: "workload_identity", l: "Workload identity"}, {v: "service_account_key", l: "Service account key"}]}
      ] : [
        {k: "address", label: "KMIP endpoint", type: "text", def: k.address || "kmip.internal:5696", required: true},
        {k: "auth_method", label: "Auth method", type: "select", def: "certificate",
          options: [{v: "certificate", l: "Client certificate"}]},
        {k: "auth_role", label: "Client certificate secret", type: "text", def: k.auth_role || "sb-kmip-client"}
      ];
      return base.concat(perProvider, [
        {k: "key_name", label: "Key name", type: "text", def: k.key_name || `sb-${c.name}-dek`, required: true},
        {k: "key_type", label: "Key type", type: "select", def: k.key_type || "aes256-gcm96",
          options: [{v: "aes256-gcm96", l: "aes256-gcm96"}, {v: "chacha20-poly1305", l: "chacha20-poly1305"}]},
        {k: "rotation_days", label: "Rotate every", unit: "days, 0 = never", type: "number", min: 0, def: k.rotation_days || 0},
        {k: "verify_tls", label: "Verify TLS certificate", type: "checkbox", def: k.verify_tls !== false},
        {k: "n19", type: "note", label: "Applies to every encrypted volume and bucket in this cluster. Unencrypted volumes are unaffected. This is separate from the S3 target the cluster writes backups to."}
      ]);
    },
    run: v => api.clusterSetKms(c.id, v)
  };
};

const fileStorageDialog = c => {
  const f = c.fileStorage || {};
  return {
    title: `File storage for ${c.name}`, confirm: "Apply",
    desc: "A pNFS filesystem over this cluster's capacity. Every worker becomes a pNFS client and reads and writes directly against the storage nodes; the kernel NFS server on one control-plane worker serves metadata only.",
    fields: v => [
      {k: "enabled", label: "Enable file storage (RWX)", type: "checkbox", def: f.enabled !== false},
      v.enabled === false ? {k: "n30", type: "note", label: "Disabling is refused while any ReadWriteMany claim still exists."} : null,
      v.enabled === false ? null : {k: "mds_host_id", label: "NFS metadata server", type: "select",
        load: () => api.hosts(c.id).then(hs => hs.filter(h => h.controlPlane && h.status === "available")
          .map(h => ({v: h.id, l: `${h.hostname}${f.mds_host && f.mds_host.uuid === h.id ? " (current)" : ""}`}))),
        empty: "No control-plane worker available. File storage needs one to run the metadata service."},
      v.enabled === false ? null : {k: "export_root", label: "Export root", type: "text", def: f.export_root || "/export/simplyblock", required: true},
      v.enabled === false ? null : {k: "layout_type", label: "pNFS layout", type: "select", def: f.layout_type || "flexfile",
        options: [{v: "flexfile", l: "Flexible file layout"}, {v: "block", l: "Block layout"}]},
      v.enabled === false ? null : {k: "lease_seconds", label: "Lease", unit: "s", type: "number", min: 5, def: f.lease_seconds || 20},
      v.enabled === false ? null : {k: "grace_seconds", label: "Grace period", unit: "s", type: "number", min: 10, def: f.grace_seconds || 45},
      v.enabled === false ? null : {k: "failover_budget_seconds", label: "Failover budget", unit: "s", type: "number", min: 3, def: f.failover_budget_seconds || 8},
      v.enabled === false ? null : {k: "max_exports", label: "Max exports", type: "number", min: 8, def: f.max_exports || 128},
      v.enabled === false ? null : {k: "n31", type: "note", label: "pNFS on the Linux kernel NFS server supports XFS only, so every ReadWriteMany claim is formatted XFS."}
    ].filter(Boolean),
    run: v => api.clusterSetFileStorage(c.id, v)
  };
};

const objectStorageDialog = c => {
  const o = c.objectStorage || {};
  return {
    title: `Object storage for ${c.name}`, confirm: "Apply",
    desc: "S3 on this cluster's own capacity: object data lands on cluster volumes, object metadata in FoundationDB. One bucket is one filesystem is one logical volume.",
    fields: v => [
      {k: "enabled", label: "Enable object storage (S3)", type: "checkbox", def: o.enabled !== false},
      v.enabled === false ? {k: "n32", type: "note", label: "Disabling is refused while any bucket still exists."} : null,
      v.enabled === false ? null : {k: "endpoint", label: "S3 endpoint", type: "text", def: o.endpoint || `https://s3.${c.name}.simplyblock.internal`, required: true},
      v.enabled === false ? null : {k: "region", label: "Region", type: "text", def: o.region || "eu-central-1", required: true},
      v.enabled === false ? null : {k: "addressing", label: "Addressing style", type: "select", def: o.addressing || "virtual-hosted",
        options: [{v: "virtual-hosted", l: "Virtual-hosted"}, {v: "path", l: "Path"}]},
      v.enabled === false ? null : {k: "versioning_default", label: "Version new buckets by default", type: "checkbox", def: !!o.versioning_default},
      v.enabled === false ? null : {k: "max_buckets", label: "Max buckets", type: "number", min: 1, def: o.max_buckets || 500},
      v.enabled === false ? null : {k: "n33", type: "note", label: "This is the S3 service the cluster serves. It is separate from the S3 target the cluster writes its own backups to."}
    ].filter(Boolean),
    run: v => api.clusterSetObjectStorage(c.id, v)
  };
};

const newBucketDialog = c => ({
  title: "Create a bucket", confirm: "Create bucket",
  desc: "A bucket is provisioned as one logical volume. Access is Kubernetes-native: a service account and a secret holding the access key.",
  fields: [
    {k: "name", label: "Bucket name", type: "text", placeholder: "media-assets", required: true},
    {k: "pool_id", label: "Pool", type: "select", required: true,
      load: () => api.pools(c.id).then(ps => ps.filter(p => p.enabled).map(p => ({v: p.id, l: p.name}))),
      empty: "No enabled pool in this cluster."},
    {k: "size", label: "Filesystem size", unit: "GB", type: "number", min: 10, def: 1000, required: true},
    {k: "quota", label: "Quota", unit: "GB, 0 = none", type: "number", min: 0, def: 0},
    {k: "namespace", label: "Kubernetes namespace", type: "text", def: "default", required: true},
    {k: "service_account", label: "Service account", type: "text", placeholder: "sb-s3-media-assets"},
    {k: "policy", label: "Policy", type: "select", def: "read-write",
      options: [{v: "read-write", l: "Read / write"}, {v: "read-only", l: "Read only"}, {v: "write-only", l: "Write only"}]},
    {k: "versioning", label: "Enable versioning", type: "checkbox", def: !!(c.objectStorage || {}).versioning_default},
    {k: "object_lock", label: "Enable object lock", type: "checkbox"},
    {k: "n40", type: "note", label: "Object lock cannot be turned off again once enabled, and a locked bucket cannot be deleted."},
    {k: "encryption", label: "Encrypt with the cluster KMS key", type: "checkbox", def: true},
    {k: "storage_class", label: "Default storage class", type: "select", def: "standard",
      options: [{v: "standard", l: "Standard"}, {v: "infrequent-access", l: "Infrequent access"}]},
    {k: "owner", label: "Owner", type: "text", placeholder: "platform-team"},
    {k: "tags", label: "Bucket tags", type: "kv", max: 50, def: [{k: "env", v: "prod"}, {k: "team", v: ""}],
      hint: "S3 bucket tagging — searchable in the bucket list as key or key=value."}
  ],
  run: v => api.bucketCreate(c.id, Object.assign({}, v, {size: Number(v.size) * 1e9, quota: Number(v.quota) * 1e9,
    tags: Object.fromEntries((v.tags || []).filter(t => t.k).map(t => [t.k.trim(), (t.v || "").trim()]))}))
});

const newClusterDialog = () => ({
  title: "Create a storage cluster", confirm: "Create cluster",
  desc: "The cluster is bootstrapped on prepared, labelled hosts. A storage node is started on each selected host and their unassigned devices are claimed.",
  fields: v => {
    const edge = v.location_type === "edge";
    return [
      {k: "name", label: "Cluster label", type: "text", placeholder: "prod-eu-central-2", required: true},
      {k: "location_type", label: "Siting", type: "select", def: "datacenter",
        options: [{v: "datacenter", l: "Data center"}, {v: "edge", l: "Edge"}]},
      {k: "device_class", label: "Device class", type: "select", def: "nvme",
        options: edge ? [{v: "block", l: "Block devices"}]
          : [{v: "nvme", l: "NVMe (PCIe address)"}, {v: "block", l: "Block devices"}]},
      edge ? {k: "n5", type: "note", label: "Edge clusters are block-device only, do not rebalance, run no task engine, and are managed through the Kubernetes API rather than a separate control plane endpoint."} : null,
      {k: "ec", label: "Stripe — data + parity chunks", type: "select", def: "2+1",
        options: ["1+1", "2+1", "2+2", "4+1", "4+2", "8+2"].map(x => ({v: x, l: `${x} (data+parity chunks)`}))},
      {k: "zone_ids", label: "Zones the cluster spans", type: "multiselect", required: true,
        load: () => api.zones().then(ss => ss.map(x => ({v: x.id, l: `${x.name} · ${x.location}`}))),
        empty: "No zones defined. Add a zone under Disaster recovery first."},
      {k: "n10", type: "note", label: "Zones are permanent: they cannot be added or changed after creation, and storage nodes can only ever be started on hosts in these zones."},
      {k: "node_affinity", label: "Node affinity", type: "select", def: "soft",
        options: [{v: "none", l: "None — free placement"},
          {v: "soft", l: "Soft — prefer to keep volumes in place"},
          {v: "strict", l: "Strict — volumes can be pinned to a node"}]},
      {k: "pod_affinity_enabled", label: "Pod affinity — front storage follows the workload", type: "checkbox", def: true},
      {k: "n25", type: "note", label: "With pod affinity a volume's primary is moved by instant migration whenever its workload pod is rescheduled to another node."},
      {k: "multipathing_enabled", label: "Enable multipathing", type: "checkbox", def: true},
      v.multipathing_enabled
        ? {k: "n21", type: "note", label: "Every storage node must then be given two data NICs, and clients use both paths automatically."}
        : {k: "n21", type: "note", label: "Each storage node gets a single data NIC and clients have one path to it."},
      {k: "sync_replication_enabled", label: "Enable synchronous replication", type: "checkbox"},
      v.sync_replication_enabled && (v.zone_ids || []).length < 2
        ? {k: "n14", type: "note", label: "Synchronous replication needs at least two zones — pick another zone above."} : null,
      {k: "failure_domain_enabled", label: "Enable failure domains", type: "checkbox", def: true},
      v.failure_domain_enabled ? {k: "failure_domain_scope", label: "Failure domain granularity", type: "select", def: "rack",
        options: [{v: "rack", l: "Rack — node taint"}, {v: "cabinet", l: "Cabinet — node taint"},
          {v: "zone", l: "Zone — topology.kubernetes.io/zone"}]} : null,
      v.failure_domain_enabled ? {k: "n15", type: "note",
        label: "Every node must sit in a failure domain, and node counts per domain may differ by at most one. Hosts without the matching taint cannot take a node."} : null,
      {k: "backup_enabled", label: "Enable backups", type: "checkbox", def: true},
      v.backup_enabled ? {k: "s3_endpoint", label: "S3 endpoint", type: "text", placeholder: "https://s3.eu-central-1.amazonaws.com", required: true} : null,
      v.backup_enabled ? {k: "s3_region", label: "S3 region", type: "text", placeholder: "eu-central-1", required: true} : null,
      v.backup_enabled ? {k: "s3_bucket", label: "Bucket", type: "text", placeholder: "sb-backup-prod", required: true} : null,
      v.backup_enabled ? {k: "s3_path_prefix", label: "Path prefix", type: "text", placeholder: "clusters/prod/"} : null,
      v.backup_enabled ? {k: "s3_access_key_id", label: "Access key id", type: "text", placeholder: "AKIA…", required: true} : null,
      v.backup_enabled ? {k: "s3_secret_access_key", label: "Secret access key", type: "text", placeholder: "••••", required: true} : null,
      v.backup_enabled ? {k: "s3_addressing", label: "Addressing style", type: "select", def: "virtual-hosted",
        options: [{v: "virtual-hosted", l: "Virtual-hosted"}, {v: "path", l: "Path"}]} : null,
      v.backup_enabled ? {k: "s3_verify_tls", label: "Verify TLS certificate", type: "checkbox", def: true} : null,
      {k: "n16", type: "note", label: "Zones, failure domains and synchronous replication are all fixed at creation and cannot be changed later."},
      {k: "host_ids", label: "Prepared hosts to build on", type: "multiselect", required: true,
        load: () => api.unassignedHosts().then(hs => hs.map(h => ({
          v: h.id, l: `${h.hostname} · ${h.sockets} socket(s) · ${h.counts.free} free devices · ${h.vcpu} vCPU`}))),
        empty: "No unassigned prepared host. Prepare and label a host first — it then appears here automatically."},
      {k: "n2", type: "note", label: "A default storage pool is created with the cluster. The cluster reports in_activation until every node is online."}
    ].filter(Boolean);
  },
  run: v => api.clusterCreate(Object.assign({}, v, {
    device_class: v.location_type === "edge" ? "block" : v.device_class,
    distr_ndcs: Number(String(v.ec).split("+")[0]), distr_npcs: Number(String(v.ec).split("+")[1])
  }))
});

const ACTIONS = {
  deployconfig: o => [
    o.status === "Draft" ? {label: "Approve and deploy", icon: "check", op: "create",
      dialog: window.approveDialog(o)} : null,
    o.status === "Draft" || o.status === "Failed"
      ? {label: "Delete document", icon: "trash", danger: true, removes: true, dialog: {
          title: `Delete ${o.name}?`, danger: true,
          desc: o.status === "Draft"
            ? "The document has never been applied, so deleting it changes nothing on any node."
            : "The deployment failed. Deleting the document leaves whatever the failed steps already applied — check the cluster before re-drafting.",
          confirm: "Delete document", run: () => api.deployConfigDelete(o.name)}}
      : {label: "Deployed — document is immutable", icon: "lock", disabled: true,
          hint: "approval is one-way"},
  ].filter(Boolean),
  cluster: o => [
    o.caps.rebalancing ? {label: "Rebalance front storage now", icon: "gauge", dialog: {
      title: `Rebalance ${o.name}?`,
      desc: "Evens out volume placement across the online storage nodes using instant migration — the primary role is handed over without copying data. Each move files an lvol_migration task, which is where it can be followed. Volumes pinned by node affinity are left where they are.",
      confirm: "Rebalance now", run: () => api.clusterRebalance(o.id)}} : null,
    o.caps.rebalancing ? {label: o.autoRebalance.enabled ? "Turn off automatic rebalancing" : "Turn on automatic rebalancing", icon: "clock", dialog: {
      title: `${o.autoRebalance.enabled ? "Turn off" : "Turn on"} automatic rebalancing for ${o.name}?`,
      desc: o.autoRebalance.enabled
        ? "Volumes stay where they are until you rebalance the cluster by hand or move one yourself."
        : "The control plane evens out volume placement across the online storage nodes on its own, by instant migration. Every move files an lvol_migration task.",
      fields: [{k: "n74", type: "note", label: `${o.autoRebalance.moved24h} volume(s) moved in the last 24 hours, ${o.autoRebalance.moved1h} in the last hour.`}],
      confirm: o.autoRebalance.enabled ? "Turn off" : "Turn on",
      run: () => api.clusterSetRebalance(o.id, !o.autoRebalance.enabled)}} : null,
    o.type === "kubernetes"
      ? {label: "File storage (RWX)…", icon: "folder", dialog: fileStorageDialog(o)}
      : {label: "File storage (RWX)…", icon: "folder", disabled: true, hint: "Kubernetes deployments only"},
    {label: "Object storage (S3)…", icon: "cloud", dialog: objectStorageDialog(o)},
    o.objectStorage && o.objectStorage.enabled
      ? {label: "Create bucket…", icon: "plus", dialog: newBucketDialog(o)} : null,
    {label: "KMS — external key management…", icon: "lock", dialog: kmsDialog(o)},
    o.kms ? {label: "Test KMS connection", icon: "gauge", run: () => api.clusterTestKms(o.id), toast: "Vault connection tested"} : null,
    o.status !== "suspended" && {label: "Shut down cluster", icon: "power", danger: true, dialog: {
      title: `Shut down ${o.name}?`, danger: true,
      desc: "All storage nodes are stopped and the cluster is suspended. Connected volumes lose their NVMe-oF targets until the cluster is restarted.",
      confirm: "Suspend cluster", run: () => api.clusterSuspend(o.id)}},
    o.status === "suspended" && {label: "Restart cluster", icon: "refresh", dialog: {
      title: `Restart ${o.name}?`,
      desc: "The control plane re-activates every storage node in sequence. The cluster reports in_activation until all nodes are online.",
      confirm: "Restart", run: () => api.clusterActivate(o.id)}},
    {label: "Expand — add storage node", icon: "plus", dialog: {
      title: "Add a storage node",
      desc: "Pick a prepared host. Its unassigned devices are claimed by the new node, then existing data is rebalanced onto it — the expansion runs asynchronously through adding node, rebalancing data and complete.",
      fields: [{k: "host_id", label: "Target host", type: "select", required: true,
        load: () => api.hosts(o.id).then(hs => {
          const ok = hs.filter(h => h.status === "available" && h.counts.nodes < 2 && h.counts.free > 0
            && (!(o.zoneIds || []).length || (o.zoneIds || []).includes(h.zoneId)));
          if (!o.fd || !o.fd.enabled) return ok.map(h => ({v: h.id, l: `${h.hostname} · ${regName(h.zoneId, "zone")} · ${h.counts.free} free devices`}));
          const fdOf = h => o.fd.scope === "zone" ? regName(h.zoneId, null) : o.fd.scope === "cabinet" ? h.cabinet : h.rack;
          const counts = {};
          (o.fd.domains || []).forEach(f => counts[f.name] = f.nodes);
          const min = o.fd.domains.length ? Math.min(...o.fd.domains.map(f => f.nodes)) : 0;
          return ok.filter(h => { const f = fdOf(h); return f && (counts[f] === undefined || counts[f] <= min); })
            .map(h => ({v: h.id, l: `${h.hostname} · ${o.fd.scope} ${fdOf(h)} · ${h.counts.free} free devices`}));
        }),
        empty: "No eligible host. A host must be in one of the cluster's zones, carry a failure-domain taint, and sit in a domain that is not already ahead of the others."},
        o.fd && o.fd.enabled ? {k: "failure_domain", label: `Failure domain (${o.fd.scope})`, type: "text", required: true,
          placeholder: (o.fd.domains && o.fd.domains[0] && o.fd.domains[0].name) || "rack-1",
          sub: (o.fd.domains || []).length ? "existing: " + o.fd.domains.map(f => f.name).join(", ") : null} : null,
        o.fd && o.fd.enabled ? {k: "n71", type: "note", label: "The failure domain is assigned once, now. It is fixed for the node's lifetime — moving a node between domains means removing it and adding it again."} : null,
        {k: "n13", type: "note", label: o.fd && o.fd.enabled
          ? `Nodes can only be started on hosts in the cluster's zones. Each ${o.fd.scope} must carry at least two storage nodes and counts may differ by at most one, so expand in pairs` + ((o.fd.domains || []).length ? ` — currently ${o.fd.domains.filter(f => f.name !== "unassigned").map(f => f.name + ": " + f.nodes).join(", ")}.` : ".")
          : "Storage nodes can only be started on hosts in the zones assigned when the cluster was created."}].filter(Boolean),
      confirm: "Add node", run: v => api.clusterAddNode(o.id, v.host_id, v.failure_domain)}}
  ].filter(Boolean),

  host: o => o.status === "discovered" ? [
    {label: "Prepare this worker node", icon: "plus", dialog: {
      title: `Prepare ${o.hostname}?`,
      desc: "Schedules the simplyblock inspection pod on this Kubernetes worker node. It collects the NUMA topology, the NVMe and block devices and the available NICs. Nothing is configured yet — you pick the resources afterwards.",
      confirm: "Deploy inspection pod", run: () => api.hostsPrepare(o.clusterId, [o.id]), done: "Inspection pod scheduled"}}
  ] : o.status === "inspecting" ? [
    {label: "Inspection in progress…", icon: "clock", disabled: true, hint: "waiting for the inspection pod"}
  ] : o.status === "inspected" ? [
    {label: "Configure storage plane…", icon: "gauge", dialog: configureHostDialog(o)}
  ] : [
    {label: "Add storage node on host", icon: "plus", disabled: o.status !== "available" || o.counts.nodes >= 2 || !o.counts.free,
      hint: o.counts.nodes >= 2 ? "host already runs two nodes" : !o.counts.free ? "no unassigned devices" : null,
      dialog: {title: `Add a storage node on ${o.hostname}`,
        desc: `${o.counts.free} unassigned device(s) will be claimed by the new node, then existing data is rebalanced onto it.`,
        confirm: "Add node", run: () => api.clusterAddNode(o.clusterId, o.id, o.rack || o.zone)}},
    {label: "Reserve device for a node", icon: "device", disabled: !o.counts.free, hint: !o.counts.free ? "no unassigned devices" : null,
      dialog: {title: "Reserve a host device",
        desc: "The device is earmarked for a storage node. It is picked up on the next node restart.",
        fields: [
          {k: "device_id", label: "Unassigned device", type: "select", required: true,
            options: o.devices.filter(d => !d.assignedNodeId).map(d => ({v: d.id, l: `${d.kind === "nvme" ? d.pcie : d.blockdev} · socket ${d.socket} · ${fmtBytes(d.size)}${d.reserved ? " (reserved)" : ""}`}))},
          {k: "node_id", label: "Storage node", type: "select", required: true,
            load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => o.nodeIds.includes(n.id)).map(n => ({v: n.id, l: n.hostname}))),
            empty: "This host runs no storage node yet."},
          {k: "note", type: "note", label: "Requires a node restart to take effect."}
        ], confirm: "Reserve", run: v => api.hostReserveDevice(o.id, v.device_id, v.node_id)}},
    {label: o.migrationTaint ? "Clear migration target taint" : "Taint as migration target", icon: "move",
      dialog: {title: o.migrationTaint ? `Clear the taint on ${o.hostname}?` : `Taint ${o.hostname} as a migration target`,
        desc: o.migrationTaint
          ? "The host stops being offered as a destination for cluster migrations."
          : "Tainted hosts are the destinations a cluster migration moves front storage onto. The host must already run a storage node to receive volumes.",
        fields: o.migrationTaint ? [] : [{k: "taint", label: "Taint", type: "text", def: "simplyblock.io/migration-target=true", required: true}],
        confirm: o.migrationTaint ? "Clear taint" : "Apply taint",
        run: v => api.hostSetTaint(o.id, o.migrationTaint ? null : v.taint)}},
    {label: "Rack & cabinet taints…", icon: "zone", dialog: {
      title: `Placement of ${o.hostname}`,
      desc: "A host is racked in exactly one zone. The rack and cabinet labels are how operators find it on the floor.",
      fields: [
        {k: "n28", type: "note", label: `Zone and region come from the node labels topology.kubernetes.io/zone and /region — this host reports ${o.zone || "no zone"}${o.region ? " in " + o.region : ""}. They cannot be set here.`},
        {k: "rack_id", label: "Rack", type: "text", def: o.rack || "", placeholder: "r14 (optional)"},
        {k: "cabinet_id", label: "Cabinet", type: "text", def: o.cabinet || "", placeholder: "c03 (optional)"},
        {k: "n9", type: "note", label: `Rack and cabinet come from the worker node's taints and may be absent. Device class is derived from the installed hardware: this host is ${o.hostClass || "not yet inspected"}.`}
      ],
      confirm: "Apply", run: v => api.hostSetPlacement(o.id, v)}}
  ],
  node: o => o.op ? [
    {label: `${OP_LABEL[o.op.kind]} in progress…`, icon: "clock", disabled: true,
      hint: `${o.op.phase} · phase ${o.op.phaseIndex + 1} of ${o.op.phases.length}`}
  ] : [
    o.status === "online" || o.status === "in_restart" ? {label: "Shut down node", icon: "power", danger: true, dialog: {
      title: `Shut down ${o.hostname}?`, danger: true,
      desc: "The node reports in shutdown and then offline. Nothing is migrated off it — volumes whose primary sits here fail over to their secondaries, and the cluster runs at reduced redundancy until the node is back. A forced shutdown stops the node without waiting for it to close cleanly.",
      fields: [{k: "force", label: "Force (do not wait for a clean stop)", type: "checkbox"}],
      confirm: "Shut down", run: v => api.nodeShutdown(o.id, v.force)}} : null,
    o.status !== "online" && o.status !== "in_restart" ? {label: "Restart node", icon: "refresh", dialog: {
      title: `Restart ${o.hostname}?`,
      desc: "The node reports in restart and then online. It re-registers with the control plane, re-attaches its devices, and its chunks resynchronise from the surviving copies.",
      confirm: "Restart", run: () => api.nodeRestart(o.id)}} : null,
    {label: "Migrate to another host", icon: "move", dialog: {
      title: `Migrate ${o.hostname}`,
      desc: "The node is restarted on a different, already prepared host. Prepare and label the target host first.",
      fields: [{k: "host_id", label: "Target host", type: "select", required: true,
        load: () => Promise.all([api.hosts(o.clusterId), api.cluster(o.clusterId)]).then(([hs, c]) =>
          hs.filter(h => h.status === "available" && h.id !== o.hostId && h.counts.nodes === 0
              && (!(c.zoneIds || []).length || (c.zoneIds || []).includes(h.zoneId)))
            .map(h => ({v: h.id, l: `${h.hostname} · ${regName(h.zoneId, "zone")} · ${h.counts.free} free devices`}))),
        empty: "No prepared, empty host in this cluster's zones."}],
      confirm: "Queue migration", run: v => api.nodeMigrate(o.id, v.host_id)}},
    {label: "Add device", icon: "plus", dialog: {
      title: "Add a device to this node",
      desc: (REG[o.clusterId] || {}).mode === "nvme" ? "NVMe cluster — identify the drive by PCIe address." : "Block device cluster — identify the drive by block device name.",
      fields: (REG[o.clusterId] || {}).mode === "nvme"
        ? [{k: "pcie_address", label: "PCIe address", type: "text", placeholder: "0000:5e:00.0", required: true}]
        : [{k: "device_name", label: "Block device", type: "text", placeholder: "/dev/sdb", required: true}],
      confirm: "Add device", run: v => api.nodeAddDevice(o.id, v)}},
    {label: "Remove node", icon: "trash", danger: true, removes: true, dialog: {
      title: `Remove ${o.hostname}?`, danger: true,
      desc: "The node is drained first: every volume whose primary sits here is moved off by instant migration, without copying data. The node and its device records are then deleted and its host devices released back to the host pool.",
      fields: [
        {k: "n26", type: "note", label: "Blocked if a volume is pinned to this node by affinity, if there is no other online node to move to, or if the removal would leave failure domains more than one node apart."},
        {k: "confirmName", label: "Type the node hostname to confirm", type: "text", match: o.hostname, required: true}
      ],
      confirm: "Drain & remove", run: () => api.nodeRemove(o.id),
      done: "Node removal started — this runs asynchronously and may take a while"}}
  ].filter(Boolean),

  // Two different things can happen to a bad device, and the difference matters:
  //   Remove — takes it out of service. Reversible: the cluster still knows the
  //            device and a restart adds it back.
  //   Fail   — excludes it permanently and rebuilds its chunks onto the
  //            remaining devices, restoring fault tolerance without it. Only
  //            from removed or unavailable, and there is no way back.
  device: o => {
    const stopped = o.status === "unavailable" || o.status === "removed";
    const live = o.status === "online" || o.status === "read_only";
    const busy = o.status === "in_removal" || o.status === "in_failure" || o.status === "in_restart";
    const name = o.mode === "nvme" ? o.serial : (o.blockdev || o.serial);
    if (busy) return [{label: `${STATUS_META[o.status].label}…`, icon: "clock", disabled: true,
      hint: "follow it under the cluster's Operations tab"}];
    if (o.status === "failed") return [{label: "Permanently failed", icon: "alert", disabled: true,
      hint: "its chunks were rebuilt onto the remaining devices; this device cannot be re-added"}];
    return [
      stopped && {label: o.status === "removed" ? "Add device back" : "Restart device", icon: "refresh", dialog: {
        title: o.status === "removed" ? `Add ${name} back?` : `Restart ${name}?`,
        desc: o.status === "removed"
          ? "Removal is reversible. The node re-attaches the drive, re-admits it to the distribution layer, and its chunks are resynchronised from the surviving copies."
          : "The node re-attaches the drive and re-admits it to the distribution layer. Its chunks are resynchronised from the surviving copies.",
        confirm: o.status === "removed" ? "Add back" : "Restart", run: () => api.deviceRestart(o.id)}},
      {label: "Run health check", icon: "gauge", disabled: !live,
        hint: !live ? "the drive is not attached, so its SMART log cannot be read" : null,
        dialog: {title: `Run a health check on ${name}?`,
          desc: "nvme-cli re-reads the drive's SMART log on the node. The report is stored on the device and the health traffic light is rewritten from its verdict — available spare against the 10% threshold, media and integrity errors, and the critical warning bit.",
          confirm: "Run check", run: () => api.deviceHealthCheck(o.id),
          done: "Health check running — the report and the health status update when it finishes"}},
      live && {label: "Remove device", icon: "power", danger: true, dialog: {
        title: `Remove ${name}?`, danger: true,
        desc: "The device is taken out of service. This is a temporary state — the cluster keeps the device and you can add it back at any time. Redundancy is reduced while it is out.",
        fields: [{k: "n73", type: "note", label: "Use this to pull a drive for inspection or replacement. If the drive is not coming back, fail it afterwards so its chunks are rebuilt and fault tolerance is restored without it."}],
        confirm: "Remove", run: () => api.deviceRemove(o.id),
        done: "Device removal started — follow it under the cluster's Operations tab"}},
      stopped && {label: "Fail device (permanent)", icon: "alert", danger: true, removes: true, dialog: {
        title: `Permanently fail ${name}?`, danger: true,
        desc: "The device is excluded from the cluster for good and every chunk that lived on it is rebuilt onto the remaining devices, which restores fault tolerance without this drive. It cannot be added back afterwards.",
        fields: [
          {k: "n72", type: "note", label: `Redundancy is currently reduced because this device is ${STATUS_META[o.status].label}. The rebuild moves ${fmtBytes(o.capacity.used)} and restores it. Only fail the device once you are sure the drive will not come back — a removed device can simply be added again.`},
          {k: "confirmName", label: "Type the device name to confirm", type: "text", match: name, required: true}
        ],
        confirm: "Fail permanently", run: () => api.deviceFail(o.id),
        done: "Failure migration started — follow the rebuild under the cluster's Operations tab"}}
    ].filter(Boolean);
  },

  volume: o => [
    {label: "Expand…", icon: "move", dialog: {
      title: `Expand ${o.name}`,
      desc: o.pvc
        ? "Logical volumes can only grow. Expanding here also raises the request on the PVC bound to this volume; the filesystem is extended on the next mount."
        : "Logical volumes can only grow — shrinking is refused. The filesystem must be extended inside the guest afterwards.",
      fields: [
        {k: "size", label: "Provisioned size", type: "number", unit: "GB", def: gb(o.capacity.total), min: gb(o.capacity.total), required: true},
        {k: "n36", type: "note", label: `Currently ${fmtBytes(o.capacity.total)}.` + (o.pvc ? ` Bound to PVC ${o.pvc.namespace}/${o.pvc.name}.` : "")}
      ],
      confirm: "Expand", run: v => api.volumeResize(o.id, Number(v.size) * GBn)}},
    {label: "Take snapshot", icon: "camera", dialog: {
      title: `Snapshot ${o.name}`, desc: "A copy-on-write snapshot is taken immediately and chained onto the existing snapshot chain. It consumes space only as the volume diverges.",
      fields: [{k: "name", label: "Snapshot name", type: "text", def: `${o.name}-snap`, required: true}],
      confirm: "Create snapshot", run: v => api.volumeSnapshot(o.id, v.name)}},
    {label: "Clone", icon: "copy", dialog: {
      title: `Clone ${o.name}`, desc: "Creates a new thin volume in the same pool, backed by a snapshot of this one.",
      fields: [{k: "name", label: "New volume name", type: "text", def: `${o.name}-clone`, required: true}],
      confirm: "Clone", run: v => api.volumeClone(o.id, v.name)}},
    {label: "Migrate instantly…", icon: "move", dialog: {
      title: `Move ${o.name} to another node`,
      desc: "Instant volume migration moves the primary role to another storage node without copying data, so the move completes immediately. Use it to follow a workload or to relieve a hot node.",
      fields: [
        {k: "node_id", label: "Target primary node", type: "select", required: true,
          load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => n.status === "online" && (!o.nodes.primary || n.id !== o.nodes.primary.uuid))
            .map(n => ({v: n.id, l: `${n.hostname} · ${n.ip}`}))),
          empty: "No other online node in this cluster."},
        {k: "reason", label: "Reason", type: "select", def: "manual",
          options: [{v: "manual", l: "Manual move"}, {v: "follow_workload", l: "Follow the workload"}, {v: "rebalance", l: "Relieve a hot node"}]},
        o.affinity && o.affinity.mode === "node"
          ? {k: "n23", type: "note", label: `This volume is pinned to ${o.affinity.pinned_node}. Moving it re-pins the affinity to the new node.`} : null
      ].filter(Boolean),
      confirm: "Move now", run: v => api.volumeMigrate(o.id, v)}},
    {label: "Rebalance this volume", icon: "gauge", dialog: {
      title: `Rebalance ${o.name}?`,
      desc: "Moves the volume to the least loaded online node in the cluster using instant migration. Nothing is copied.",
      confirm: "Rebalance", run: () => api.volumeRebalance(o.id)}},
    {label: "Affinity…", icon: "link", dialog: {
      title: `Affinity for ${o.name}`,
      desc: "Node affinity pins the volume's primary to one node. Pod affinity keeps front storage on whichever node the workload pod runs on, moving it instantly when the pod is rescheduled.",
      fields: v2 => [
        {k: "mode", label: "Affinity", type: "select", def: o.affinity ? o.affinity.mode : "none",
          options: [{v: "none", l: "None — the control plane places it freely"},
            {v: "node", l: "Node affinity — pin to a storage node"},
            {v: "pod", l: "Pod affinity — follow the workload"}]},
        v2.mode === "node" ? {k: "node_id", label: "Pin to node", type: "select",
          load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => n.status === "online")
            .map(n => ({v: n.id, l: `${n.hostname}${o.nodes.primary && n.id === o.nodes.primary.uuid ? " (current primary)" : ""}`})))} : null,
        v2.mode === "pod" ? {k: "workload", label: "Workload pod", type: "text",
          def: o.affinity && o.affinity.workload ? o.affinity.workload : `${o.name}-0`} : null,
        v2.mode === "pod" ? {k: "n24", type: "note", label: "Requires pod affinity to be enabled on the cluster."} : null
      ].filter(Boolean),
      confirm: "Apply", run: v => api.volumeSetAffinity(o.id, v)}},
    {label: "Snapshot & back up now", icon: "cloud", dialog: {
      title: `Back up ${o.name}`,
      desc: "Backups are always taken from a snapshot. This takes a snapshot now and appends a version to the volume\u2019s backup chain — the first version is a full copy, every later one a delta.",
      fields: [
        {k: "bucket", label: "Bucket location", type: "text", def: `s3://sb-backup-eu/${o.name}/`, required: true},
        {k: "n11", type: "note", label: "Snapshot and backup version become independent objects: deleting the online snapshot later leaves the backup version untouched."}
      ], confirm: "Snapshot & back up", run: v => api.volumeBackup(o.id, v)}},
    {label: "Compression-dedup…", icon: "move", dialog: {
      title: `Compression-dedup for ${o.name}`,
      desc: "Compression-dedup is a single per-volume switch. Turning it on or off only affects data written from now on — blocks already on disk keep their current form.",
      fields: [
        {k: "compression_dedup_enabled", label: "Compression-dedup", type: "checkbox", def: o.dataReduction},
        {k: "n20", type: "note", label: "Costs CPU on the storage node and hugepage memory for the fingerprint table."}
      ], confirm: "Apply", run: v => api.volumeSetDataReduction(o.id, v)}},
    {label: "QoS limits…", icon: "gauge", dialog: {
      title: `QoS limits for ${o.name}`,
      desc: "Caps applied by the storage node to this volume. They override the pool defaults.",
      fields: qosFields(o.qos), confirm: "Apply limits", run: v => api.volumeSetQos(o.id, v)}},
    {label: "Backup policy…", icon: "clock", dialog: {
      title: `Backup policy for ${o.name}`, desc: "Link this volume to a cluster backup policy, or detach it.",
      fields: [{k: "policy_id", label: "Policy", type: "select", def: o.backupPolicy ? o.backupPolicy.uuid : "",
        load: () => api.policies(o.clusterId).then(ps => [{v: "", l: "— none —"}].concat(ps.map(p => ({v: p.id, l: `${p.name} · window ${p.window}`}))))}],
      confirm: "Apply", run: v => api.volumeSetPolicy(o.id, v.policy_id)}},
    o.replication
      ? {label: "Detach from replication", icon: "link", danger: true, dialog: {
          title: `Stop replicating ${o.name}?`,
          desc: `The volume leaves ${o.replication.policyName}. Data already replicated to the target is kept, but no further generations are shipped.`,
          confirm: "Detach", run: () => api.rpolicyRemoveVolume(o.replication.policyId, o.id)}}
      : {label: "Attach to replication policy…", icon: "shield", dialog: {
          title: `Replicate ${o.name}`,
          desc: "Pick a replication policy in this cluster. Asynchronous policies ship deltas over a cluster pair; synchronous policies acknowledge writes at every zone of a stretched cluster.",
          fields: [{k: "policy", label: "Replication policy", type: "select", required: true,
            load: () => api.rpolicies(o.clusterId).then(ps => ps.map(p => ({
              v: p.id, l: p.mode === "synchronous"
                ? `${p.name} · synchronous · ${(p.zoneIds || []).length} zones`
                : `${p.name} · every ${p.frequency} min → ${regName(p.targetClusterId)}`}))).catch(() => []),
            empty: "No replication policy in this cluster. Create one under Disaster recovery."}],
          confirm: "Attach", run: v => api.rpolicyAddVolumes(v.policy, [o.id])}},
    {label: "Consistency group…", icon: "link",
      hint: (o.consistencyGroups || []).length ? `in ${o.consistencyGroups.map(g => g.name).join(", ")}` : null,
      dialog: {title: `Add ${o.name} to a consistency group`,
        desc: "Members of a consistency group are snapshotted together, at one common point in time. A volume can belong to several groups; a group whose protection is already active cannot take new members.",
        fields: [{k: "cg", label: "Consistency group", type: "select", required: true,
          load: () => api.cgroups(o.clusterId).then(gs => gs.filter(g => !g.locked && !(o.consistencyGroups || []).some(x => x.uuid === g.id))
            .map(g => ({v: g.id, l: `${g.name} · ${g.counts.volumes} volumes`}))),
          empty: "No group in this cluster can take this volume — every one either already has it or carries an active policy."}],
        confirm: "Add", run: v => api.cgroupAddVolumes(v.cg, [o.id])}},
    {label: "Delete volume", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: "The volume and all of its snapshots are destroyed. Backups in object storage are kept.",
      fields: [{k: "confirmName", label: `Type the volume name to confirm`, type: "text", match: o.name, required: true}],
      confirm: "Delete", run: () => api.volumeDelete(o.id)}}
  ],

  snapshot: o => [
    o.backupVersionId
      ? {label: "Already backed up", icon: "cloud", disabled: true, hint: `version ${o.backupVersionId} was taken from this snapshot`}
      : {label: "Back up this snapshot", icon: "cloud", dialog: {
          title: `Back up ${o.name}`,
          desc: "Appends a version to the volume\u2019s backup chain from this snapshot. Afterwards the snapshot and the backup version are independent — deleting the snapshot leaves the version in the bucket.",
          fields: [{k: "bucket", label: "Bucket location", type: "text", def: `s3://sb-backup-eu/${o.volumeName}/`, required: true}],
          confirm: "Back up", run: v => api.snapshotBackup(o.id, v.bucket)}},
    {label: "Restore to new volume…", icon: "refresh", op: "create", dialog: {
      title: `Restore ${o.name}`,
      desc: "Creates a new volume from this snapshot. The target can be this cluster or any other cluster the control plane manages.",
      fields: [
        {k: "cluster_id", label: "Target cluster", type: "select", required: true, def: o.clusterId,
          load: () => api.clusters().then(cs => cs.map(c => ({v: c.id, l: `${c.name}${c.id === o.clusterId ? " (source)" : ""} · ${c.siting}`})))},
        {k: "name", label: "New volume name", type: "text", def: `${o.volumeName}-restored`, required: true}
      ], confirm: "Restore", run: v => api.snapshotRestore(o.id, v)}},
    {label: "Clone to new volume", icon: "copy", dialog: {
      title: `Clone ${o.name}`, desc: "Creates a thin volume from this snapshot in the same pool.",
      fields: [{k: "name", label: "New volume name", type: "text", def: `${o.volumeName}-from-snap`, required: true}],
      confirm: "Clone", run: v => api.snapshotClone(o.id, v.name)}},
    {label: "Delete snapshot", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: o.backupVersionId
        ? `Only the online snapshot is removed. Backup version ${o.backupVersionId} stays in the bucket unchanged, and volumes cloned from this snapshot keep their data.`
        : "No backup was taken from this snapshot, so this point in time is lost. Volumes cloned from it keep their data.",
      confirm: "Delete", run: () => api.snapshotDelete(o.id)}}
  ],

  backup: o => {
    const vs = o.versions || [];
    const opts = vs.slice().reverse().map(v => ({v: v.id, l: `${v.id} · ${v.type} · ${fmtDate(v.createdAt)}`}));
    return [
      {label: "Restore to new volume…", icon: "refresh", op: "restore", dialog: {
        title: `Restore ${o.chainId}`,
        desc: "Rebuilds a new logical volume from the full version plus every delta up to the version you pick — that version is the point in time you get back.",
        fields: [
          {k: "version_id", label: "Restore to version", type: "select", required: true, options: opts,
            empty: "This chain holds no versions."},
          {k: "name", label: "New volume name", type: "text", def: `${o.volumeName}-restored`, required: true}
        ], confirm: "Restore", run: v => api.backupRestore(o.id, v.name, v.version_id)}},
      {label: "Merge oldest delta into full", icon: "move",
        disabled: vs.length < 2, hint: vs.length < 2 ? "chain holds a single full version" : null,
        dialog: {title: `Merge the earliest delta of ${o.chainId}?`, danger: true,
          desc: "The second-earliest version is folded into the full version, which then covers both, and the delta is removed. This is how older retention is aged out — points in time before the merged version become unrecoverable.",
          confirm: "Merge", run: () => api.backupMerge(o.id)}},
      {label: "Export a version…", icon: "ext", dialog: {
        title: `Export from ${o.chainId}`,
        desc: "Copies the chain up to the chosen version to another bucket or an external target.",
        fields: [
          {k: "version_id", label: "Version", type: "select", required: true, options: opts},
          {k: "destination", label: "Destination", type: "text", def: "s3://sb-archive-cold/", required: true}
        ], confirm: "Export", run: v => api.backupExport(o.id, v.destination, v.version_id)}},
      {label: "Delete backup chain", icon: "trash", danger: true, removes: true, dialog: {
        title: `Delete ${o.chainId}?`, danger: true,
        desc: `Deleting a volume backup deletes the entire chain — all ${vs.length} version(s), full and deltas. Online snapshots on the cluster are not touched.`,
        fields: [{k: "confirmName", label: "Type the chain id to confirm", type: "text", match: o.chainId, required: true}],
        confirm: "Delete chain", run: () => api.backupDelete(o.id)}}
    ];
  },

  pool: o => [
    o.enabled
      ? {label: "Disable pool", icon: "power", dialog: {
          title: `Disable ${o.name}?`,
          desc: "The pool keeps serving its existing volumes — no I/O is interrupted. New volumes can no longer be provisioned into it until it is enabled again.",
          confirm: "Disable", run: () => api.poolDisable(o.id)}}
      : {label: "Enable pool", icon: "refresh", dialog: {
          title: `Enable ${o.name}?`,
          desc: "Provisioning of new volumes into this pool is allowed again.",
          confirm: "Enable", run: () => api.poolEnable(o.id)}},
    {label: "QoS limits…", icon: "gauge", dialog: {
      title: `QoS limits for pool ${o.name}`,
      desc: "Pool-wide caps. Volumes without their own QoS profile inherit these.",
      fields: qosFields(o.qos), confirm: "Apply limits", run: v => api.poolSetQos(o.id, v)}}
  ],
  // A pair is never edited: spec.targetCluster is immutable. It can only be
  // replaced, and the delete is refused while a policy references it.
  pair: o => [
    {label: "Add a policy…", icon: "plus", op: "create", dialog: newReplPolicyDialog(o)},
    o.counts.slots ? {label: "Fail over the whole pair…", icon: "move", danger: true, op: "failover", entity: "application",
      dialog: replOpsDialog({action: "failover", scope: "target", ref: o.name, obj: o})} : null,
    {label: "Delete pair", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: o.counts.policies
        ? `${o.counts.policies} ReplicationPolicy resource(s) still reference this pair. The operator refuses the delete until they are gone.`
        : "The backend replication target is deleted with the pair.",
      confirm: "Delete", run: () => api.pairDeleteCrd(o.name)}}
  ].filter(Boolean),

  rpolicy: o => [
    {label: "Attach a PVC…", icon: "plus", dialog: attachPvcDialog(o)},
    o.counts.slots ? {label: o.mode === "migration" ? "Commit the cutover…" : "Fail over this policy…",
      icon: "move", danger: o.mode !== "migration",
      dialog: replOpsDialog({action: o.mode === "migration" ? "migration" : "failover",
        scope: "policy", ref: o.name, obj: o})} : null,
    o.counts.failedOver ? {label: "Fail back this policy…", icon: "refresh", op: "failback", entity: "application",
      dialog: replOpsDialog({action: "failback", scope: "policy", ref: o.name, obj: o})} : null,
    {label: "Delete policy", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: o.counts.slots
        ? `${o.counts.slots} ReplicationSlot resource(s) still reference this policy. Detach their PVCs first — the operator refuses the delete otherwise.`
        : "The backend replication policy is deleted with it.",
      confirm: "Delete", run: () => api.rpolicyDeleteCrd(o.name)}}
  ].filter(Boolean),

  slot: o => [
    {label: "Operate on this volume…", icon: "move", dialog: volumeOpsDialog(o)},
    {label: "Detach from replication", icon: "x", danger: true, removes: true, dialog: detachPvcDialog(o)}
  ],

  // A terminal operation is spent: there is nothing left to act on.
  replops: o => o.terminal ? [] : [
    {label: `${o.subphase || o.phase} — in progress`, icon: "clock", disabled: true,
      hint: "a ReplicationOps has no abort: it runs to a terminal phase"}
  ],

  zone: () => [],

  migration: o => [
    o.mode === "cross_cluster" && o.status !== "completed"
      ? {label: "Cut over now", icon: "move", danger: true,
          disabled: !o.readyToCutover,
          hint: !o.readyToCutover ? `outstanding snapshot ${fmtBytes(o.lastSnapshot)} is above the threshold` : null,
          dialog: {title: `Cut over ${o.name}?`, danger: true,
            desc: `IO is frozen for roughly ${o.freezeMs} ms while the last ${fmtBytes(o.lastSnapshot)} snapshot is applied and the NVMe paths roll over to ${regName(o.targetClusterId)}. Clients reconnect to the target.`,
            fields: [{k: "confirmName", label: "Type the migration name to confirm", type: "text", match: o.name, required: true}],
            confirm: "Freeze & roll over", run: () => api.migrationCutover(o.id)}}
      : null,
    o.status === "paused"
      ? {label: "Resume migration", icon: "refresh", run: () => api.migrationResume(o.id), toast: "Migration resumed"}
      : o.status !== "completed"
      ? {label: "Pause migration", icon: "power", dialog: {title: `Pause ${o.name}?`,
          desc: "Shipping stops and the outstanding delta grows again. Volumes keep serving from the source.",
          confirm: "Pause", run: () => api.migrationPause(o.id)}}
      : null,
    o.status !== "completed"
      ? {label: "Cancel migration", icon: "trash", danger: true, removes: true, dialog: {
          title: `Cancel ${o.name}?`, danger: true,
          desc: "Volumes already moved stay where they are. Anything still replicating is abandoned and its target data discarded.",
          confirm: "Cancel migration", run: () => api.migrationCancel(o.id)}}
      : null
  ].filter(Boolean),

  protectedapp: o => {
    const busy = ["FailingOver", "Relocating"].includes(o.phase);
    const legs = o.legs || [];
    const live = legs.filter(l => l.type !== "snapshot-s3" && l.state !== "Unprotected");
    const canFailover = !!o.failoverTargets.length;
    return [
      !busy && o.phase !== "FailedOver" && o.phase !== "WaitForUser"
        ? {label: "Fail over to a peer site", icon: "move", danger: true, op: "failover",
            disabled: !canFailover,
            hint: !canFailover ? "no live peer leg — a vault restore leaves no site to fail over to" : null,
            dialog: {
              title: `Fail over ${o.namespace}/${o.name}?`, danger: true,
              desc: "The application is brought up at a peer site from the last synced PVC group. This is the unplanned move: anything written after that sync is lost. Fence the old site afterwards if it is still reachable.",
              fields: [
                {k: "target", label: "Failover target", type: "select", required: true,
                  options: o.failoverTargets.map(t => {
                    const leg = legs.find(l => l.target === t);
                    return {v: t, l: leg ? `${t} · via ${leg.method} · lag ${fmtLag(leg.lagSeconds)}` : t};
                  })},
                {k: "n90", type: "note", label: o.group && o.group.lastAt
                  ? `The group was last made safe at ${clockOf(o.group.lastAt)} with ${fmtBytes(o.group.writtenSince)} written since. That is what a failover now would lose.`
                  : "The group has never completed a cycle, so a failover now would bring up empty volumes."},
                !o.kubeObjectProtection ? {k: "n91", type: "note", label: "Kubernetes object protection is off, so only the volumes travel. The workload has to be recreated by hand and the application parks in WaitForUser."} : null
              ].filter(Boolean),
              confirm: "Fail over", run: v => api.appFailover(o.id, v.target),
              done: "Failover started — this runs asynchronously"}}
        : null,
      !busy && o.phase === "WaitForUser"
        ? {label: "Confirm cleanup", icon: "check", dialog: {
            title: "Confirm the stale workload is gone?",
            desc: "Ramen waits for the operator here because it cannot tell a deleted workload from an unreachable one. Confirm only once the old site's workload really is deleted — two live copies writing to one volume set is the failure this guard exists to prevent.",
            confirm: "Confirm", run: () => api.appConfirmCleanup(o.id)}}
        : null,
      !busy && o.phase === "FailedOver" && o.failoverTargets.length
        ? {label: "Fail back to the preferred site", icon: "refresh", op: "failback",
            disabled: !o.rpoMet,
            hint: !o.rpoMet ? "a leg is outside its scheduling interval" : null,
            dialog: {title: `Fail back ${o.namespace}/${o.name}?`,
              desc: `Planned move back to ${o.preferredSite}. Replication has been running in reverse since the failover, and the move only proceeds once the group is inside its interval, so nothing is lost.`,
              fields: [{k: "n88", type: "note", label: o.group && o.group.lastAt
                ? `The group last synced at ${clockOf(o.group.lastAt)} with ${fmtBytes(o.group.writtenSince)} written since. That is what a fail back has to ship before the cutover.`
                : "The group's replication status decides when the fail back can proceed."}],
              confirm: "Fail back", run: () => api.appRelocate(o.id),
              done: "Relocation started — this runs asynchronously"}}
        : null,
      busy ? {label: `${o.phase} in progress…`, icon: "clock", disabled: true, hint: o.progression} : null,
      // Ramen drives one DRPC per application, so switching which method it
      // drives is a rebind: delete and create, because DRPolicy is immutable.
      !busy && live.length > 1
        ? {label: "Switch orchestrated method…", icon: "swap", dialog: {
            title: `Orchestrated method for ${o.name}`,
            desc: "A placement control selects PVCs by label, so two over the same PVCs would both claim them — the documented outcome is data corruption. Exactly one method is therefore driven by Ramen; the rest keep replicating in the data plane with identical parameters and report their lag out of band.",
            fields: [
              {k: "method", label: "Method Ramen drives", type: "select", required: true, def: o.orchestratedMethod,
                options: live.map(l => ({v: l.method, l: `${l.method} · ${mmeta(l.type).label} → ${l.target} · lag ${fmtLag(l.lagSeconds)}`}))},
              {k: "n92", type: "note", label: "Switching deletes and recreates the placement control. The data plane is untouched: no baseline is retaken and no generation is lost."},
              {k: "n93", type: "note", label: "The vault method is never chosen here — it becomes the orchestrated one only for the duration of a restore, from the generation catalogue."}
            ],
            confirm: "Rebind", run: v => api.appSetOrchestrated(o.id, v.method),
            done: "Placement control rebinding — this runs asynchronously"}}
        : null,
      !busy && o.vaultMethod && o.generations.length
        ? {label: "Restore from a generation…", icon: "camera", danger: true, op: "failover", dialog: {
            title: `Restore ${o.namespace}/${o.name} from a generation`, danger: true,
            desc: `Materialises the application's volumes from one immutable generation in the vault and brings it up at ${o.restoreTargets.join(", ") || "the restore target"}. This is the ransomware path: recovery takes minutes rather than seconds because volumes are built from object storage, and afterwards the application has to be re-protected from scratch.`,
            fields: [
              {k: "generation", label: "Generation", type: "select", required: true,
                options: o.generations.filter(g => g.integrity === "Verified")
                  .slice(0, 40).map(g => ({v: String(g.generation), l: `${g.generation} · ${fmtDate(g.at)} · ${g.tier} · ${fmtBytes(g.size)}${g.kind === "full" ? " · full" : ""}`}))},
              {k: "n94", type: "note", label: "The generation is pinned out of band immediately before the placement rebind, because PromoteVolume carries no point-in-time argument. The driver reads the pin when the promote arrives."},
              {k: "n95", type: "note", label: "Failback will not be available afterwards: the source volume is gone or untrusted and the vault holds generations rather than a live peer, so the original site's pre-compromise state cannot be reconstructed."}
            ],
            confirm: "Pin and restore", run: v => api.appRestore(o.id, v.generation),
            done: "Generation pinned — the placement control is rebinding"}}
        : null,
      !busy ? {label: "Edit recipe…", icon: "list", dialog: {
        title: `Recipe for ${o.namespace}/${o.name}`,
        desc: "The Ramen Recipe this application's placement control references. Groups select Kubernetes objects; hooks run a command in a pod or wait for a condition; the recover workflow is the boot sequence at the standby site, the capture workflow runs before objects are backed up. Discover the namespace's resources to start from what is actually deployed.",
        wide: true,
        fields: [
          {k: "recipe", type: "recipe", namespace: o.namespace, def: o.recipe || {name: o.name.toLowerCase().replace(/[^a-z0-9-]/g, "-") + "-recipe", namespace: o.namespace, appType: "", groups: [], hooks: [],
            captureWorkflow: {failOn: "any-error", sequence: []}, recoverWorkflow: {failOn: "any-error", sequence: []}}},
          !o.kubeObjectProtection ? {k: "n50", type: "note", label: "Kubernetes object protection is off on this application — the recipe is stored but not executed until it is enabled."} : null
        ].filter(Boolean),
        confirm: "Save recipe",
        run: v => api.appSetRecipe(o.id, v.recipe)}} : null,
      !busy && o.recipe ? {label: "Remove recipe", icon: "x", dialog: {
        title: `Remove the recipe from ${o.name}?`,
        desc: "Without a recipe Ramen captures and restores every object in the namespace in one pass — no ordering, no hooks.",
        confirm: "Remove", run: () => api.appSetRecipe(o.id, {remove: true})}} : null,
      !busy ? {label: "Edit protection…", icon: "gauge", dialog: {
        title: `Protection for ${o.name}`,
        desc: "The plan decides which methods exist and what they cost. Kubernetes object protection decides whether the workload itself travels or only its volumes.",
        fields: [
          {k: "kube_object_protection", label: "Protect Kubernetes objects", type: "checkbox", def: o.kubeObjectProtection},
          {k: "n96", type: "note", label: "Without object protection a failover moves the volumes only: the workload has to be recreated by hand and the application parks in WaitForUser."}
        ], confirm: "Apply",
        run: v => api.appUpdate(o.id, {kube_object_protection: v.kube_object_protection})}} : null,
      !busy ? {label: "Stop protecting", icon: "trash", danger: true, removes: true, dialog: {
        title: `Stop protecting ${o.namespace}/${o.name}?`, danger: true,
        desc: "The application is removed from DR. Its PVCs keep their data and storage-level replication is unaffected, but no failover and no restore is possible.",
        confirm: "Stop protecting", run: () => api.appUnprotect(o.id)}} : null
    ].filter(Boolean);
  },

  // A plan is authored; everything under it is derived, so the only things to
  // act on are its methods.
  plan: o => [
    {label: "Add method…", icon: "plus", dialog: addMethodDialog(o)},
    o.protectionGap ? {label: "Fix the interval mismatch…", icon: "alert", dialog: {
      title: `Fix the interval on ${o.name}`,
      desc: "The interval is written to the policy and to the class parameters. When they differ no class resolves, no peerClass appears, and every application on that method is protected by nothing — while the policy still validates cleanly. Setting it here emits one value to both places.",
      fields: [
        {k: "method", label: "Method", type: "select", required: true,
          options: o.methods.filter(m => !m.intervalConsistent).map(m => ({v: m.name, l: `${m.name} · policy ${m.interval} vs class ${m.classInterval}`}))},
        {k: "interval", label: "Interval", type: "text", required: true, placeholder: "5m",
          sub: "written to DRPolicy.spec.schedulingInterval and parameters.schedulingInterval at once"}
      ],
      confirm: "Set interval", run: v => api.planSetInterval(o.id, v.method, v.interval),
      done: "Interval corrected — the class should now resolve"}} : null,
    {label: "Delete plan", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: o.counts.apps
        ? `${o.counts.apps} application(s) are still bound to this plan. Unbind them first.`
        : "The plan and its derived policies and classes are removed. Nothing is deleted in the data plane and no vault generation is touched.",
      confirm: "Delete", run: () => api.planDelete(o.id)}}
  ].filter(Boolean),

  method: o => [
    {label: "Set interval…", icon: "clock", disabled: o.type === "sync",
      hint: o.type === "sync" ? "a synchronous method has no interval — every write is mirrored before it is acknowledged" : null,
      dialog: {
        title: `Interval for ${o.name}`,
        desc: "One field, written to two places: the policy's schedulingInterval and the class's parameters.schedulingInterval. They must agree exactly — a difference of formatting alone leaves the application protected by nothing.",
        fields: [
          {k: "interval", label: "Interval", type: "text", required: true, def: o.interval, placeholder: "5m"},
          !o.intervalConsistent ? {k: "n97", type: "note", label: `Currently the policy carries ${o.interval} and the class carries ${o.classInterval}, so no class resolves for this method.`} : null
        ].filter(Boolean),
        confirm: "Set interval", run: v => api.planSetInterval(o.planId, o.name, v.interval)}},
    {label: "Remove method", icon: "trash", danger: true, removes: true, dialog: {
      title: `Remove ${o.name} from ${o.planName}?`, danger: true,
      desc: o.type === "snapshot-s3"
        ? "The schedule stops and no further generation is uploaded. Existing generations are not deleted — locked objects cannot be removed before their lock expires, which is the guarantee the vault exists for."
        : "The relationship is torn down and the peer copy is released per policy. Every application on this plan loses the leg.",
      confirm: "Remove", run: () => api.planRemoveMethod(o.planId, o.name)}}
  ],

  site: o => [
    o.fencing === "Unfenced"
      ? {label: "Fence this site", icon: "power", danger: true, op: "fence", entity: "application", dialog: {
          title: `Fence ${o.name}?`, danger: true,
          desc: "Blocks the site's access to storage so an application failed over elsewhere cannot be written to from two places. Fencing in Ramen is pair-scoped, so with three or more sites the decision is taken by quorum through the arbitration token.",
          confirm: "Fence", run: () => api.siteFence(o.id)}}
      : {label: "Unfence this site", icon: "power", op: "fence", entity: "application", dialog: {
          title: `Unfence ${o.name}?`,
          desc: "Restores the site's access to storage. Do this only once the site is known healthy and its stale workloads are gone.",
          confirm: "Unfence", run: () => api.siteUnfence(o.id)}}
  ],

  mpath: o => [
    o.status === "active" ? {label: "Pause path", icon: "pause", dialog: {title: `Pause ${o.name}?`,
      desc: "A group already moving finishes its current step. No new step and no new group starts until the path is resumed.", confirm: "Pause", run: () => api.mpathPause(o.id)}}
      : o.status === "paused" ? {label: "Resume path", icon: "play", dialog: {title: `Resume ${o.name}?`, desc: "The queue continues with the next pending step.", confirm: "Resume", run: () => api.mpathResume(o.id)}} : null,
    {label: "Add application group…", icon: "plus", dialog: window.newAppGroupDialog(o)},
    {label: "Delete path", icon: "trash", danger: true, removes: true, dialog: {title: `Delete ${o.name}?`, danger: true,
      desc: "Only possible once every application group has completed. Completed groups are removed with the path; the migrated volumes stay at site B.", confirm: "Delete", run: () => api.mpathDelete(o.id)}}
  ].filter(Boolean),

  appgroup: o => [
    o.phase === "Converged" ? {label: "Move workloads now", icon: "move", dialog: {title: `Move ${o.name} to site B?`,
      desc: `Replication has converged (backlog zero). ${o.counts.vms} VM(s) are live-migrated with KubeVirt and ${o.counts.members - o.counts.vms} container workload(s) are restarted at the target site. Afterwards the ${o.counts.volumes} volume(s) follow by online migration.`,
      confirm: "Move now", run: () => api.appGroupMove(o.id)}} : null,
    ["Replicating", "Converged"].includes(o.phase) ? {label: "Pause group", icon: "pause", dialog: {title: `Pause ${o.name}?`, desc: "The replication policy stays in place; nothing moves until resumed.", confirm: "Pause", run: () => api.appGroupPause(o.id)}} : null,
    o.phase === "Paused" ? {label: "Resume group", icon: "play", dialog: {title: `Resume ${o.name}?`, confirm: "Resume", run: () => api.appGroupResume(o.id)}} : null,
    !["Completed", "Failed"].includes(o.phase) ? {label: o.approval === "auto" ? "Require approval before moving" : "Move automatically when converged", icon: "check",
      dialog: {title: "Change approval", desc: o.approval === "auto" ? "The group will wait at Converged until an operator approves the move." : "The group moves its workloads as soon as every volume's backlog is zero.", confirm: "Apply", run: () => api.appGroupApproval(o.id, o.approval === "auto" ? "manual" : "auto")}} : null,
    ["Queued", "Completed", "Failed"].includes(o.phase) ? {label: "Remove group", icon: "trash", danger: true, removes: true, dialog: {title: `Remove ${o.name}?`, danger: true,
      desc: o.phase === "Completed" ? "Removes the record; the workloads and volumes stay at site B." : "The group leaves the queue. Nothing has been replicated or moved.", confirm: "Remove", run: () => api.appGroupDelete(o.id)}} : null
  ].filter(Boolean),

  bucket: o => [
    {label: "Resize filesystem…", icon: "move", dialog: {
      title: `Resize ${o.name}`,
      desc: "The bucket's filesystem can only grow — S3 has no shrink, and neither does the volume beneath it.",
      fields: [{k: "size", label: "Filesystem size", unit: "GB", type: "number",
        min: gb(o.capacity.total), def: gb(o.capacity.total), required: true}],
      confirm: "Resize", run: v => api.bucketResize(o.id, Number(v.size) * GBn)}},
    {label: "Bucket settings…", icon: "gauge", dialog: {
      title: `Settings for ${o.name}`,
      desc: "Versioning and object lock apply to objects written from now on.",
      fields: [
        {k: "versioning", label: "Versioning", type: "checkbox", def: o.versioning},
        {k: "object_lock", label: "Object lock", type: "checkbox", def: o.objectLock},
        {k: "quota", label: "Quota", unit: "GB, 0 = none", type: "number", min: 0, def: gb(o.quota)}
      ], confirm: "Apply", run: v => api.bucketUpdate(o.id, Object.assign({}, v, {quota: Number(v.quota) * GBn}))}},
    {label: "Tags…", icon: "filter", dialog: {
      title: `Tags on ${o.name}`,
      desc: "S3 bucket tagging. Tags are the metadata the bucket list searches and filters by — cost center, team, environment, retention class.",
      fields: [{k: "tags", label: "Bucket tags", type: "kv", max: 50,
        def: Object.entries(o.tags || {}).map(([k, v]) => ({k, v}))}],
      confirm: "Save tags",
      run: v => api.bucketSetTags(o.id, Object.fromEntries((v.tags || []).filter(t => t.k).map(t => [t.k.trim(), (t.v || "").trim()])))}},
    o.replication
      ? {label: `Detach from ${o.replication.policy_name}`, icon: "shield", dialog: {
          title: `Stop replicating ${o.name}?`,
          desc: `The bucket leaves the ${o.replication.mode} policy ${o.replication.policy_name}. The replica on the target keeps the last generation it received; nothing is deleted there.`,
          confirm: "Detach", run: () => api.bucketUnreplicate(o.id)}}
      : {label: "Replicate — attach to a policy…", icon: "shield", dialog: {
          title: `Replicate ${o.name}`,
          desc: "A bucket is its volume, so it joins a replication policy the way a volume does. Only policies whose source is this bucket's cluster qualify — synchronous ones across zones, asynchronous ones to a paired cluster.",
          fields: [{k: "policy_id", label: "Replication policy", type: "select", required: true,
            load: () => api.rpolicies(o.clusterId).then(ps => ps.map(p => ({v: p.id,
              l: `${p.name} · ${p.mode}${p.mode === "asynchronous" ? ` · every ${p.frequency} min → ${regName(p.targetClusterId)}` : " · across zones"}`}))),
            empty: "No replication policy has this cluster as its source. Create one under Disaster recovery first."}],
          confirm: "Attach", run: v => api.bucketReplicate(o.id, v.policy_id)}},
    {label: "Access & credentials…", icon: "lock", dialog: {
      title: `Access for ${o.name}`,
      desc: "Bucket security rides on Kubernetes: a service account in a namespace, and a secret holding the access key.",
      fields: [
        {k: "namespace", label: "Namespace", type: "text", def: o.access.namespace, required: true},
        {k: "service_account", label: "Service account", type: "text", def: o.access.service_account, required: true},
        {k: "policy", label: "Policy", type: "select", def: o.access.policy,
          options: [{v: "read-write", l: "Read / write"}, {v: "read-only", l: "Read only"}, {v: "write-only", l: "Write only"}]},
        {k: "public", label: "Allow anonymous access", type: "checkbox", def: o.access.public},
        {k: "rotate_key", label: "Rotate the access key", type: "checkbox"},
        {k: "n34", type: "note", label: "Rotating the key rewrites the secret. Clients holding the old key stop working immediately."}
      ], confirm: "Apply", run: v => api.bucketSetAccess(o.id, v)}},
    {label: "Delete bucket", icon: "trash", danger: true, removes: true,
      disabled: o.objects > 0 || o.objectLock,
      hint: o.objects > 0 ? `holds ${fmtNum(o.objects)} object(s)` : o.objectLock ? "object lock is on" : null,
      dialog: {title: `Delete ${o.name}?`, danger: true,
        desc: "The bucket and the logical volume beneath it are destroyed, along with its snapshots. Backups already in object storage are kept.",
        fields: [{k: "confirmName", label: "Type the bucket name to confirm", type: "text", match: o.name, required: true}],
        confirm: "Delete bucket", run: () => api.bucketDelete(o.id)}}
  ],

  pvc: o => [
    {label: "Expand claim…", icon: "move", dialog: {
      title: `Expand ${o.namespace}/${o.name}`,
      desc: "Kubernetes only supports growing a claim. The new size is applied to the logical volume behind it; the filesystem is extended on the next mount.",
      fields: [
        {k: "size", label: "Requested size", unit: "GB", type: "number",
          min: gb(o.requested), def: gb(o.requested), required: true},
        {k: "n35", type: "note", label: `Currently ${fmtBytes(o.requested)}. Shrinking a claim is refused.`}
      ], confirm: "Expand", run: v => api.pvcResize(o.id, Number(v.size) * GBn)}},
  ],

  cgroup: o => [
    {label: "Take group snapshot", icon: "camera", dialog: {
      title: `Snapshot ${o.name}`,
      desc: "Every member volume is snapshotted at the same instant, so the group restores to one common point in time.",
      fields: [{k: "name", label: "Group snapshot name", type: "text", def: `${o.name}-cgsnap`, required: true}],
      confirm: "Take snapshot", run: v => api.cgroupSnapshot(o.id, v.name)}},
    // Protection lives on the group, not on the individual volumes: attaching a
    // policy here applies it to every member and to members added later.
    o.backupPolicy
      ? {label: "Detach backup policy", icon: "cloud", dialog: {
          title: `Detach ${o.backupPolicy.policy_name} from ${o.name}?`,
          desc: `The ${o.counts.volumes} member volumes stop being snapshotted and backed up together. Existing backup chains and their retained versions are untouched.`,
          confirm: "Detach", run: () => api.cgroupSetPolicy(o.id, null)}}
      : {label: "Attach backup policy…", icon: "cloud", dialog: {
          title: `Back up ${o.name}`,
          desc: "Every member volume is snapshotted and backed up on the policy's schedule, all at the same instant, so any retained version is a point in time the whole group can be restored to.",
          fields: [
            {k: "policy", label: "Backup policy", type: "select", required: true,
              load: () => api.policies(o.clusterId).then(ps => ps.map(p => ({v: p.id,
                l: `${p.name} · ${p.schedule.map(r => r.interval + "×" + r.versions).join(" ")}${p.consistencyGroup ? " · group-consistent" : ""}`}))),
              empty: "No backup policy in this cluster. Create one from the cluster's Backup policies layer."},
            {k: "n80", type: "note", label: "The policy is switched to group-consistent if it is not already: a policy driving a group has to snapshot every member at the same instant, otherwise the group means nothing. Member volumes lose any policy of their own."}
          ], confirm: "Attach", run: v => api.cgroupSetPolicy(o.id, v.policy)}},
    o.replicationConfig
      ? {label: "Replication cadence…", icon: "shield", dialog: {
          title: `Replication cadence for ${o.name}`,
          desc: "How often the group snapshot is taken and shipped, and how many older generations the target keeps. Every DR policy naming this group replicates on exactly this cadence.",
          fields: [
            {k: "frequency_minutes", label: "Replication frequency", unit: "minutes", type: "number", min: 1, required: true, def: o.replicationConfig.frequency},
            {k: "retention", label: "Retained generations at the target", type: "schedule", def: o.replicationConfig.retention},
            {k: "n86", type: "note", label: `${o.drPolicyIds.length} DR polic${o.drPolicyIds.length === 1 ? "y" : "ies"} currently replicate${o.drPolicyIds.length === 1 ? "s" : ""} this group.`}
          ], confirm: "Apply", run: v => api.cgroupReplicate(o.id, v)}}
      : {label: "Enable replication…", icon: "shield", dialog: {
          title: `Replicate ${o.name}`,
          desc: "Give the group a replication cadence: every cycle a group snapshot is taken and shipped, so the target always holds one common point in time across all members. A DR policy can then name this group to replicate it to a paired cluster.",
          fields: [
            {k: "frequency_minutes", label: "Replication frequency", unit: "minutes", type: "number", min: 1, def: 15, required: true},
            {k: "retention", label: "Retained generations at the target", type: "schedule", def: [{interval: "1h", keep: 12}, {interval: "1d", keep: 7}]},
            {k: "n81", type: "note", label: "Enabling replication fixes the group's membership: the replica stream is defined against exactly this set of volumes."}
          ], confirm: "Enable", run: v => api.cgroupReplicate(o.id, v)}},
    o.replicationConfig ? {label: "Disable replication", icon: "x", danger: true, disabled: !!o.drPolicyIds.length,
      hint: o.drPolicyIds.length ? `${o.drPolicyIds.length} DR policy/policies replicate this group` : null,
      dialog: {title: `Stop replicating ${o.name}?`,
        desc: "The group loses its cadence and its members stop replicating. Replicas already at the target are kept.",
        confirm: "Disable", run: () => api.cgroupUnreplicate(o.id)}} : null,
    {label: "Add volumes…", icon: "plus", disabled: o.locked,
      hint: o.locked ? "membership is fixed while the group carries a policy" : null, dialog: {
      title: `Add volumes to ${o.name}`,
      desc: o.backupPolicy || o.replicationPolicy
        ? `Only volumes of the same cluster that are not already in a consistency group can join. A volume joining inherits the group's ${[o.backupPolicy && "backup policy", o.replicationPolicy && "replication policy"].filter(Boolean).join(" and ")}.`
        : "Only volumes of the same cluster that are not already in a consistency group can join.",
      fields: [{k: "lvol_ids", label: "Volumes", type: "multiselect", required: true,
        load: () => api.clusterVolumes(o.clusterId).then(vs => vs.filter(v => !v.consistencyGroup && v.status === "online")
          .map(v => ({v: v.id, l: `${v.name} · ${v.poolName} · ${fmtBytes(v.capacity.total)}`}))),
        empty: "Every online volume in this cluster is already in a consistency group."}],
      confirm: "Add", run: v => api.cgroupAddVolumes(o.id, v.lvol_ids)}},
    {label: "Delete group", icon: "trash", danger: true, removes: true,
      disabled: o.counts.snapshots > 0 || !!o.counts.apps || !!o.backupPolicy || !!o.replicationPolicy,
      hint: o.counts.apps ? `${o.counts.apps} protected application(s) are based on this group`
        : o.backupPolicy || o.replicationPolicy ? "detach the group's policies first"
        : o.counts.snapshots > 0 ? `${o.counts.snapshots} group snapshot(s) exist` : null,
      dialog: {title: `Delete ${o.name}?`, danger: true,
        desc: "The member volumes are released and keep their data. Delete the group's snapshots first.",
        confirm: "Delete group", run: () => api.cgroupDelete(o.id)}}
  ],

  cgsnapshot: o => [
    {label: "Restore into new volumes…", icon: "refresh", op: "create", dialog: {
      title: `Restore ${o.name}`,
      desc: `Creates one new volume per member (${o.counts.volumes} in total), all at the same point in time. The target can be this cluster or any other.`,
      fields: [
        {k: "cluster_id", label: "Target cluster", type: "select", required: true, def: o.clusterId,
          load: () => api.clusters().then(cs => cs.map(c => ({v: c.id, l: `${c.name}${c.id === o.clusterId ? " (source)" : ""} · ${c.siting}`})))},
        {k: "prefix", label: "Name prefix for the new volumes", type: "text", def: "restored", required: true},
        {k: "n17", type: "note", label: "The volumes land in the first enabled pool of the target cluster and are not placed in a consistency group."}
      ], confirm: "Restore", run: v => api.cgSnapRestore(o.id, v)}},
    o.backupVersionId
      ? {label: "Already backed up", icon: "cloud", disabled: true, hint: `version ${o.backupVersionId}`}
      : {label: "Back up group snapshot", icon: "cloud", dialog: {
          title: `Back up ${o.name}`,
          desc: "Writes every member snapshot of this group snapshot to object storage as one backup version.",
          fields: [{k: "bucket", label: "Bucket location", type: "text", def: `s3://sb-backup-eu/cg/${o.cgName}/`, required: true}],
          confirm: "Back up", run: v => api.cgSnapBackup(o.id, v.bucket)}},
    {label: "Delete group snapshot", icon: "trash", danger: true, removes: true, dialog: {
      title: `Delete ${o.name}?`, danger: true,
      desc: o.backupVersionId
        ? `The ${o.counts.volumes} member snapshots are removed from the cluster. Backup version ${o.backupVersionId} stays in the bucket.`
        : `The ${o.counts.volumes} member snapshots are removed and this point in time is lost.`,
      confirm: "Delete", run: () => api.cgSnapDelete(o.id)}}
  ],

  policy: o => [
    {label: "Edit schedule…", icon: "clock", dialog: {
      title: `Schedule for ${o.name}`,
      desc: "Each row is a tier: how often a snapshot is taken and backed up, how many backup versions of that tier are retained, and how many of those snapshots stay online on the cluster.",
      fields: [{k: "schedule", label: "Schedule", type: "bschedule", def: o.schedule},
        {k: "consistency_group", label: "Group-consistent — all linked volumes in one consistency group", type: "checkbox", def: o.consistencyGroup},
        {k: "n60", type: "note", label: "Group-consistent policies are what Ramen applications link for ransomware recovery: any retained version is a point-in-time the application can be restored to."}],
      confirm: "Apply", run: v => api.policyUpdate(o.id, v)}},
    {label: "Delete policy", icon: "trash", danger: true, removes: true,
      disabled: o.counts.volumes > 0,
      hint: o.counts.volumes > 0 ? `${o.counts.volumes} volume(s) still use it` : null,
      dialog: {title: `Delete ${o.name}?`, danger: true,
        desc: "Existing backup chains and their versions are kept — only the schedule that would extend them is removed.",
        confirm: "Delete policy", run: () => api.policyDelete(o.id)}}
  ]
};

const newBackupPolicyDialog = cluster => ({
  title: "Create a backup policy", confirm: "Create policy",
  desc: "A policy triggers background snapshot-and-backup cycles. Each schedule row carries the cadence, how many backup versions are retained, and how many snapshots stay online.",
  fields: [
    {k: "name", label: "Policy name", type: "text", placeholder: "5m-tiered", required: true},
    {k: "schedule", label: "Schedule", type: "bschedule",
      def: [{interval: "5m", versions: 12, online: 3}, {interval: "1h", versions: 11, online: 0}, {interval: "1d", versions: 6, online: 0}]},
    {k: "consistency_group", label: "Group-consistent — all linked volumes in one consistency group", type: "checkbox", def: false},
    {k: "n12", type: "note", label: "Once a tier holds more versions than it retains, the oldest is merged into its predecessor — so the interval is also the merge cadence. Group-consistent policies snapshot every linked volume atomically per cycle, which is what an application needs to recover from ransomware to one point in time."}
  ],
  run: v => api.policyCreate(cluster.id, v)
});

// ---- menu + dialog host ----------------------------------------------------
// The operation an action needs. Default: a removal is delete, everything else
// is update. Items override with op: "create" | "failover" | "restore" | …;
// restore is the three-part check from RBAC-DESIGN.md §11.
const actionOp = it => it.op || (it.removes ? "delete" : "update");
const permitted = (obj, items) => items.filter(it => {
  const entity = it.entity || KIND_ENTITY[obj.kind] || "storagecluster";
  // a disabled item is still a change the caller could not make — §3 says
  // such controls are absent, not greyed, so it needs the same right
  if (it.op === "restore") return window.access.canRestore(obj, it.targetPool || null);
  return window.access.can(actionOp(it), entity, obj);
});

function ActionBtn({obj, big}) {
  useAccess();
  const items = permitted(obj, (ACTIONS[obj.kind] || (() => []))(obj));
  if (!items.length) return null;
  return (
    <button className={big ? "chip" : "kebab"} title="Actions" onClick={e => {
      e.stopPropagation();
      window.__ui.menu(e.currentTarget.getBoundingClientRect(), items, obj);
    }}>{big ? <><Icon n="dots" s={13} />Actions</> : <Icon n="dots" s={14} />}</button>
  );
}

function Field({f, val, setVal}) {
  const [opts, setOpts] = useState(f.options || null);
  const [loading, setLoading] = useState(!!f.load);
  useEffect(() => {
    if (f.load) f.load().then(o => { setOpts(o); setLoading(false); if (o.length && f.type !== "multiselect" && (val === undefined || val === "")) setVal(o[0].v); }).catch(() => setLoading(false));
    else if (f.options && f.options.length && f.type !== "multiselect" && val === undefined) setVal(f.options[0].v);
  }, []);
  // conditional forms can swap the option set out from under a chosen value
  useEffect(() => {
    if (!f.options || f.type === "multiselect") return;
    setOpts(f.options);
    if (f.options.length && !f.options.some(o => o.v === val)) setVal(f.options[0].v);
  }, [f.options && f.options.map(o => o.v).join("|")]);
  if (f.type === "note") return <div className="fnote"><Icon n="alert" s={12} />{f.label}</div>;
  if (f.type === "recipe") return <RecipeField f={f} val={val} setVal={setVal} />;
  if (f.type === "members") return <MembersField f={f} val={val} setVal={setVal} />;
  if (f.type === "kv") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {[k]: x}) : r));
    return (
      <label className="field">
        <span className="flabel">{f.label} <em>({rows.length} of {f.max || 50})</em></span>
        <div className="schedbox">
          {rows.map((r, i) => (
            <div className="schedrow" key={i}>
              <input className="finput sm" placeholder="key" value={r.k} onChange={e => set(i, "k", e.target.value)} />
              <span className="sl">=</span>
              <input className="finput sm" placeholder="value" value={r.v} onChange={e => set(i, "v", e.target.value)} />
              <button type="button" className="kebab" title="Remove tag" onClick={() => setVal(rows.filter((_, j) => j !== i))}><Icon n="x" s={11} /></button>
            </div>
          ))}
          <button type="button" className="schedadd" disabled={rows.length >= (f.max || 50)} onClick={() => setVal(rows.concat({k: "", v: ""}))}>
            <Icon n="plus" s={11} />Add tag</button>
        </div>
        {f.hint && <span className="fhint">{f.hint}</span>}
      </label>
    );
  }
  if (f.type === "bschedule") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {[k]: x}) : r));
    return (
      <label className="field">
        <span className="flabel">{f.label} <em>({rows.reduce((a, r) => a + (Number(r.versions) || 0), 0)} versions, {rows.reduce((a, r) => a + (Number(r.online) || 0), 0)} online)</em></span>
        <div className="schedbox">
          <div className="schedrow head"><span className="sl">every</span><span className="sl">versions</span><span className="sl">online</span><span style={{width: 24}}></span></div>
          {rows.map((r, i) => (
            <div className="schedrow" key={i}>
              <select className="finput sm" value={r.interval} onChange={e => set(i, "interval", e.target.value)}>
                {RET_INTERVALS.map(x => <option key={x} value={x}>{x}</option>)}
              </select>
              <input className="finput sm" type="number" min="1" value={r.versions} onChange={e => set(i, "versions", e.target.value)} />
              <input className="finput sm" type="number" min="0" value={r.online || 0} onChange={e => set(i, "online", e.target.value)} />
              <button type="button" className="kebab" title="Remove tier" onClick={() => setVal(rows.filter((_, j) => j !== i))}><Icon n="x" s={11} /></button>
            </div>
          ))}
          <button type="button" className="schedadd" onClick={() => setVal(rows.concat({interval: "1d", versions: 6, online: 0}))}>
            <Icon n="plus" s={11} />Add tier</button>
        </div>
        <span className="fhint">Written short: {rows.map(r => `${r.interval} ${r.versions}x${r.online ? ` ${r.online}x online` : ""}`).join(" · ") || "—"}</span>
      </label>
    );
  }
  if (f.type === "schedule") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {[k]: x}) : r));
    return (
      <label className="field">
        <span className="flabel">{f.label} <em>({rows.reduce((a, r) => a + (Number(r.keep) || 0), 0)} generations total)</em></span>
        <div className="schedbox">
          {rows.map((r, i) => (
            <div className="schedrow" key={i}>
              <span className="sl">every</span>
              <select className="finput sm" value={r.interval} onChange={e => set(i, "interval", e.target.value)}>
                {RET_INTERVALS.map(x => <option key={x} value={x}>{x}</option>)}
              </select>
              <input className="finput sm" type="number" min="1" value={r.keep} onChange={e => set(i, "keep", e.target.value)} />
              <span className="sl">kept</span>
              <button type="button" className="kebab" title="Remove rule" onClick={() => setVal(rows.filter((_, j) => j !== i))}><Icon n="x" s={11} /></button>
            </div>
          ))}
          <button type="button" className="schedadd" onClick={() => setVal(rows.concat({interval: "1h", keep: 6}))}>
            <Icon n="plus" s={11} />Add retention rule</button>
        </div>
        <span className="fhint">Each rule keeps its own number of older snapshot generations at its own interval.</span>
      </label>
    );
  }
  if (f.type === "multiselect") {
    const sel = val || [];
    return (
      <label className="field">
        <span className="flabel">{f.label} <em>({sel.length} selected)</em></span>
        {loading ? <div className="fskel"></div>
          : !opts || !opts.length ? <div className="fempty">{f.empty || "No options available"}</div>
          : <div className="msbox">{opts.map(o => (
              <button type="button" key={o.v} className={"msrow" + (sel.includes(o.v) ? " on" : "")}
                onClick={() => setVal(sel.includes(o.v) ? sel.filter(x => x !== o.v) : sel.concat(o.v))}>
                <span className="msbox-i">{sel.includes(o.v) && <Icon n="check" s={10} />}</span>
                <span className="mono">{o.l}</span>
              </button>))}</div>}
      </label>
    );
  }
  const empty = (f.type === "select") && !loading && (!opts || !opts.length);
  return (
    <label className="field">
      <span className="flabel">{f.label}{f.unit && <em> ({f.unit})</em>}</span>
      {f.type === "select" ? (
        loading ? <div className="fskel"></div>
          : empty ? <div className="fempty">{f.empty || "No options available"}</div>
          : <select className="finput" value={val === undefined ? "" : val} onChange={e => setVal(e.target.value)}>
              {opts.map(o => <option key={o.v} value={o.v}>{o.l}</option>)}
            </select>
      ) : f.type === "checkbox" ? (
        <button type="button" className={"toggle" + (val ? " on" : "")} onClick={() => setVal(!val)}><i></i></button>
      ) : (
        <input className="finput" type={f.type === "number" ? "number" : "text"} min={f.min} value={val === undefined ? "" : val}
          placeholder={f.placeholder} onChange={e => setVal(e.target.value)} />
      )}
      {f.match && val && val !== f.match && <span className="fhint" style={{color: "var(--bad)"}}>must match “{f.match}”</span>}
    </label>
  );
}

function Dialog({spec, obj, removes, onClose}) {
  const resolve = v => typeof spec.fields === "function" ? spec.fields(v) : (spec.fields || []);
  const [vals, setVals] = useState(() => {
    const v = {}; resolve({}).forEach(f => { if (f.def !== undefined) v[f.k] = f.def; if (f.type === "checkbox") v[f.k] = !!f.def; });
    return v;
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const allFields = resolve(vals);
  const fields = allFields.filter(f => f.type !== "note");
  const invalid = fields.some(f => (f.required && (vals[f.k] === undefined || vals[f.k] === "" || (Array.isArray(vals[f.k]) && !vals[f.k].length))) || (f.match && vals[f.k] !== f.match));
  const submit = async () => {
    setBusy(true); setErr(null);
    try {
      await spec.run(vals);
      window.__toast(spec.done || `${spec.confirm} — accepted by the control plane`);
      if (removes) window.__removed(obj); else window.__refresh();
      onClose();
    } catch (e) { setErr(e.message || "Request failed"); setBusy(false); }
  };
  useEffect(() => {
    const h = e => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", h, true); return () => window.removeEventListener("keydown", h, true);
  }, []);
  return (
    <div className="ovl" onClick={onClose}>
      <div className="modal" onClick={e => e.stopPropagation()}>
        <div className="mhead">
          <h3>{spec.title}</h3>
          <button className="tbtn" style={{color: "var(--dim)"}} onClick={onClose}><Icon n="x" s={13} /></button>
        </div>
        <div className="mbody">
          {spec.desc && <p className="mdesc">{spec.desc}</p>}
          {allFields.map(f => f.type === "note"
            ? <Field key={f.k || f.label} f={f} />
            : <Field key={f.k} f={f} val={vals[f.k]} setVal={v => setVals(s => Object.assign({}, s, {[f.k]: v}))} />)}
          {err && <div className="banner" style={{marginTop: 10, marginBottom: 0}}><Icon n="alert" s={14} />{err}</div>}
        </div>
        <div className="mfoot">
          <span className="mono" style={{fontSize: 10.5, color: "var(--dim2)", marginRight: "auto"}}>
            {obj.id && obj.id !== "new" ? `${obj.kind} · ${shortId(obj.id)}` : obj.kind}</span>
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className={"btn primary" + (spec.danger ? " danger" : "")} disabled={invalid || busy} onClick={submit}>
            {busy ? "Working…" : (spec.confirm || "Confirm")}
          </button>
        </div>
      </div>
    </div>
  );
}

function UiLayer({routeKey}) {
  const [menu, setMenu] = useState(null);
  const [dialog, setDialog] = useState(null);
  const menuRef = useRef(null), dialogRef = useRef(null);
  menuRef.current = menu; dialogRef.current = dialog;
  useEffect(() => { setMenu(null); setDialog(null); }, [routeKey]);
  useEffect(() => {
    window.__ui = {
      menu: (rect, items, obj) => setMenu({rect, items, obj}),
      dialog: (spec, obj) => setDialog({spec, obj}),
      isOpen: () => !!(menuRef.current || dialogRef.current),
      close: () => { setMenu(null); setDialog(null); }
    };
  }, []);
  const runItem = async it => {
    setMenu(null);
    if (it.dialog) return setDialog({spec: it.dialog, obj: menu.obj, removes: it.removes});
    try { await it.run(); window.__toast(it.toast || "Done"); window.__refresh(); }
    catch (e) { window.__toast(e.message || "Request failed"); }
  };
  const pos = menu ? {top: Math.min(menu.rect.bottom + 6, window.innerHeight - 280), left: Math.max(8, Math.min(menu.rect.right - 232, window.innerWidth - 240))} : null;
  return (
    <>
      {menu && <>
        <div style={{position: "fixed", inset: 0, zIndex: 80}} onClick={() => setMenu(null)}></div>
        <div className="fmenu" style={pos}>
          {menu.items.map((it, i) => (
            <button key={i} className={"menu-item" + (it.danger ? " danger" : "")} disabled={it.disabled} title={it.hint || ""}
              onClick={() => !it.disabled && runItem(it)}>
              <Icon n={it.icon} s={13} /><span style={{flex: 1}}>{it.label}</span>
              {it.disabled && it.hint && <Icon n="alert" s={11} />}
            </button>
          ))}
        </div>
      </>}
      {dialog && <Dialog spec={dialog.spec} obj={dialog.obj} removes={dialog.removes} onClose={() => setDialog(null)} />}
    </>
  );
}

Object.assign(window, {ACTIONS, ActionBtn, Dialog, UiLayer, newClusterDialog, newPairDialog, newRPolicyDialog, newPoolDialog,
  newReplPolicyDialog, attachPvcDialog, detachPvcDialog, replOpsDialog, volumeOpsDialog,
  newPlanDialog, addMethodDialog, restoreGenerationDialog,
  fileStorageDialog, objectStorageDialog, newBucketDialog,
  newBackupPolicyDialog, newCgroupDialog, newMigrationDialog, protectAppDialog, kmsDialog,
  configureHostDialog, qosFields, RET_INTERVALS});
