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
const P = window.SB_DB, PU = window.SB_UTIL;
const puuid = PU.uuid, pint = PU.int, ppick = PU.pick, pago = PU.ago;
const REPL_ANN = "storage.simplyblock.io/replication-policy";
const dns1123 = s => String(s).toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 253);

// The operator rounds the interval to whole minutes, floors it at one, and
// silently falls back to 5m on anything it cannot parse.
function normInterval(raw) {
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(String(raw || "").trim());
  if (!m) return {interval: "5m", minutes: 5, fellBack: true};
  const n = parseFloat(m[1]);
  const unit = m[2] || "m";
  const mins = unit === "s" ? n / 60 : unit === "h" ? n * 60 : unit === "d" ? n * 1440 : unit === "w" ? n * 10080 : n;
  const whole = Math.max(1, Math.round(mins));
  return {interval: whole >= 60 && whole % 60 === 0 ? `${whole / 60}h` : `${whole}m`, minutes: whole, fellBack: false};
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
    uuid: puuid(), name: dns1123(`${a.name}-to-${b.name}`),
    source_cluster: a.name, target_cluster: b.name,
    source_cluster_id: a.uuid, target_cluster_id: b.uuid,
    ready: true, backend_target_id: puuid(),
    message: "Backend replication target available",
    active_ops_ref: null, created_at: pago(pint(400, 3000))
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
    p.ready = false; p.backend_target_id = null;
    p.message = "Waiting for the backend replication target: target cluster unreachable";
    P.replication_pairs.push(p);
  }
})();
const pairBy = n => P.replication_pairs.find(p => p.name === n);

// ---- policies --------------------------------------------------------------
// mode is exactly failover | migration. There is no synchronous mode and no
// tiered retention: one interval, one snapshot count (minimum 2).
const POLICY_DEFS = [
  {suffix: "dr-5m", mode: "failover", interval: "5m", retention: 3},
  {suffix: "dr-1h", mode: "failover", interval: "1h", retention: 6},
  {suffix: "cutover", mode: "migration", interval: "15m", retention: 2}
];
P.replication_pairs.filter(p => p.ready).forEach((pair, pi) => {
  POLICY_DEFS.slice(0, pi === 0 ? 3 : 1).forEach(def => {
    P.replication_policies.push({
      uuid: puuid(), name: dns1123(`${pair.name}-${def.suffix}`),
      pair_ref: pair.name, mode: def.mode,
      interval: def.interval, snapshot_retention: def.retention,
      ready: true, backend_policy_id: puuid(), slot_count: 0,
      active_ops_ref: null, message: "Backend replication policy created",
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
    uuid: puuid(), name: dns1123(`${pol.name}-${dup ? pvc.namespace + "-" : ""}${pvc.pvc_name}`),
    policy_ref: pol.name, pvc_ref: pvc.pvc_name, pvc_namespace: pvc.namespace,
    // spec.volumeID is <clusterUUID>:<poolUUID>:<volumeUUID>
    volume_id: `${pair.source_cluster_id || ""}:${(lvol || {}).pool_id || ""}:${(lvol || {}).uuid || ""}`,
    lvol_id: (lvol || {}).uuid || null,
    state, direction: "source",
    source_lvol_id: (lvol || {}).uuid || null,
    target_lvol_id: state === "failed_over" || state === "cutover_done" ? puuid() : null,
    target_nqn: state === "failed_over" ? `nqn.2024-05.io.simplyblock:${puuid().slice(0, 8)}` : null,
    last_replicated_at: state === "error" ? pago(pint(3, 20)) : pago(lagMin / 60),
    message: state === "error" ? "Backend refused the replication snapshot: target pool out of capacity"
      : state === "cutover_pending" ? "Final delta transferred, waiting for cutover commit"
      : state === "failed_over" ? "Target volume promoted, source is no longer authoritative"
      : "Replicating on schedule",
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
      const state = i === 0 && k === 3 ? "error"
        : pol.mode === "migration" && k === 0 ? "cutover_pending" : "replicating";
      const slot = slotFor(pol, pvc, lvol, state);
      P.replication_slots.push(slot);
      // the annotation is the source of truth for membership
      pvc.annotations = Object.assign({}, pvc.annotations, {[REPL_ANN]: pol.name});
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
    uuid: puuid(), name: dns1123(`failback-${pol.name}-1`),
    action: "failback", scope: "policy", ref: pol.name,
    source_cluster_id: null, delete_source: false,
    phase: "Succeeded", subphase: "", message: "Failback completed for 3 of 4 volumes",
    started_at: pago(20), completed_at: pago(19.6),
    results: slots.map((s, i) => ({
      slot_ref: s.name,
      status: i === 3 ? "failed" : "succeeded",
      detail: i === 3 ? "Backend rejected the failback commit: source volume is still attached" : "",
      target_lvol_id: null
    })),
    started_ms: null, created_at: pago(20)
  });
  // one that ends Failed overall, because a single volume failed — the CRD
  // reports per-volume outcomes independently
  P.replication_ops[0].phase = "Failed";
})();

// ---- k8s projections -------------------------------------------------------
const RNS = () => window.SB_CONFIG.namespace || "simplyblock";
const RAPI = () => window.API_GROUP;
const meta = (name, uid, at) => ({name, namespace: RNS(), uid, creationTimestamp: at});

const pairToK8s = p => ({
  apiVersion: RAPI(), kind: "ReplicationPair",
  metadata: meta(p.name, p.uuid, p.created_at),
  spec: {sourceCluster: p.source_cluster, targetCluster: p.target_cluster},
  status: {ready: p.ready, backendTargetID: p.backend_target_id, message: p.message,
    activeOpsRef: p.active_ops_ref || undefined,
    conditions: [{type: "Ready", status: p.ready ? "True" : "False",
      reason: p.ready ? "TargetAvailable" : "TargetUnavailable", message: p.message,
      lastTransitionTime: p.created_at}]}
});
const polToK8s = p => ({
  apiVersion: RAPI(), kind: "ReplicationPolicy",
  metadata: meta(p.name, p.uuid, p.created_at),
  spec: {pairRef: p.pair_ref, mode: p.mode, interval: p.interval, snapshotRetention: p.snapshot_retention},
  status: {ready: p.ready, backendPolicyID: p.backend_policy_id,
    slotCount: P.replication_slots.filter(s => s.policy_ref === p.name).length,
    activeOpsRef: p.active_ops_ref || undefined,
    conditions: [{type: "Ready", status: p.ready ? "True" : "False",
      reason: p.ready ? "PolicyCreated" : "PolicyPending", message: p.message,
      lastTransitionTime: p.created_at}]}
});
const slotToK8s = s => ({
  apiVersion: RAPI(), kind: "ReplicationSlot",
  metadata: Object.assign(meta(s.name, s.uuid, s.created_at), {
    // owned by its PVC: deleting the PVC cascades to the slot
    ownerReferences: [{apiVersion: "v1", kind: "PersistentVolumeClaim",
      name: s.pvc_ref, uid: s.uuid, controller: true, blockOwnerDeletion: true}]}),
  spec: {policyRef: s.policy_ref, pvcRef: s.pvc_ref, volumeID: s.volume_id},
  status: {state: s.state, direction: s.direction,
    sourceLvolID: s.source_lvol_id, targetLvolID: s.target_lvol_id || undefined,
    targetNQN: s.target_nqn || undefined, lastReplicatedAt: s.last_replicated_at,
    message: s.message,
    conditions: [{type: "Replicating", status: s.state === "replicating" ? "True" : "False",
      reason: s.state, message: s.message, lastTransitionTime: s.last_replicated_at}]}
});
const replOpsToK8s = o => ({
  apiVersion: RAPI(), kind: "ReplicationOps",
  metadata: meta(o.name, o.uuid, o.created_at),
  spec: Object.assign({action: o.action, scope: o.scope, ref: o.ref},
    o.source_cluster_id ? {sourceClusterID: o.source_cluster_id} : {},
    o.delete_source ? {deleteSource: true} : {}),
  status: {phase: o.phase, subphase: o.subphase || undefined, message: o.message,
    startedAt: o.started_at || undefined, completedAt: o.completed_at || undefined,
    results: (o.results || []).map(r => ({slotRef: r.slot_ref, status: r.status,
      detail: r.detail || undefined, targetLvolID: r.target_lvol_id || undefined}))}
});

// ---- create / delete with the operator's own rules -------------------------
const rerr = (msg, reason) => ({err: msg, reason: reason || "Invalid"});

function createPair(body) {
  const s = (body.spec || {});
  const name = (body.metadata || {}).name || dns1123(`${s.sourceCluster}-to-${s.targetCluster}`);
  if (!s.sourceCluster || !s.targetCluster) return rerr("spec.sourceCluster and spec.targetCluster are required");
  if (s.sourceCluster === s.targetCluster) return rerr("A pair cannot replicate a cluster to itself");
  const src = P.clusters.find(c => c.name === s.sourceCluster);
  if (!src) return rerr(`No StorageCluster named ${s.sourceCluster} in this namespace. Cross-namespace references are not supported.`, "NotFound");
  if (!src.uuid) return rerr(`StorageCluster ${s.sourceCluster} has no status.uuid yet — wait for it to be registered with the control plane`, "Conflict");
  const tgt = P.clusters.find(c => c.name === s.targetCluster || c.uuid === s.targetCluster);
  if (!tgt) return rerr(`No StorageCluster named ${s.targetCluster} in this namespace. Both clusters must be attached to the same control plane.`, "NotFound");
  if (pairBy(name)) return rerr(`ReplicationPair "${name}" already exists`, "AlreadyExists");
  const rec = {uuid: puuid(), name, source_cluster: src.name, target_cluster: tgt.name,
    source_cluster_id: src.uuid, target_cluster_id: tgt.uuid,
    ready: true, backend_target_id: puuid(), message: "Backend replication target created",
    active_ops_ref: null, created_at: pago(0)};
  P.replication_pairs.push(rec);
  return {obj: pairToK8s(rec)};
}

function createPolicy(body) {
  const s = (body.spec || {});
  const name = (body.metadata || {}).name;
  if (!name) return rerr("metadata.name is required");
  if (!s.pairRef) return rerr("spec.pairRef is required");
  const pair = pairBy(s.pairRef);
  if (!pair) return rerr(`No ReplicationPair named ${s.pairRef}`, "NotFound");
  if (polBy(name)) return rerr(`ReplicationPolicy "${name}" already exists`, "AlreadyExists");
  const mode = s.mode || "failover";
  if (!["failover", "migration"].includes(mode))
    return rerr(`spec.mode must be failover or migration (got "${mode}"). There is no synchronous replication mode.`);
  const ret = s.snapshotRetention == null ? 3 : Number(s.snapshotRetention);
  if (!(ret >= 2)) return rerr("spec.snapshotRetention has a minimum of 2");
  const iv = normInterval(s.interval || "5m");
  const rec = {uuid: puuid(), name, pair_ref: pair.name, mode,
    interval: iv.interval, snapshot_retention: ret,
    ready: true, backend_policy_id: puuid(), slot_count: 0, active_ops_ref: null,
    message: iv.fellBack
      ? `spec.interval could not be parsed and fell back to 5m`
      : "Backend replication policy created",
    created_at: pago(0)};
  P.replication_policies.push(rec);
  return {obj: polToK8s(rec)};
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
  const s = (body.spec || {});
  const name = (body.metadata || {}).name || dns1123(`${s.action}-${s.ref}-${Date.now().toString(36)}`);
  if (!["failover", "failback", "migration"].includes(s.action))
    return rerr("spec.action must be one of failover, failback, migration");
  if (!["target", "policy", "volume"].includes(s.scope))
    return rerr("spec.scope must be one of target, policy, volume");
  if (!s.ref) return rerr("spec.ref is required");
  // the validating webhook resolves ref against the kind the scope names
  const kindFor = {target: "ReplicationPair", policy: "ReplicationPolicy", volume: "ReplicationSlot"}[s.scope];
  const found = s.scope === "target" ? pairBy(s.ref)
    : s.scope === "policy" ? polBy(s.ref)
    : P.replication_slots.find(x => x.name === s.ref);
  if (!found) return rerr(`spec.ref "${s.ref}" does not resolve to a ${kindFor}`, "Invalid");
  // failback is policy- or volume-scoped only; a pair-wide failback is done one
  // policy at a time
  if (s.action === "failback" && s.scope === "target")
    return rerr("Failback supports only policy and volume scope. Fail back one policy at a time.");
  // locks: policy-scoped locks the policy, target-scoped locks the pair
  const holder = s.scope === "target" ? found
    : s.scope === "policy" ? found
    : polBy(found.policy_ref);
  if (holder && holder.active_ops_ref)
    return rerr(`${holder.name} already has ${holder.active_ops_ref} in flight. A second operation waits for the lock.`, "Conflict");
  const affected = slotsInScope(s.scope, s.ref);
  if (!affected.length) return rerr(`No ReplicationSlot is in scope for ${s.scope}=${s.ref}, so there is nothing to ${s.action}`);
  const rec = {uuid: puuid(), name, action: s.action, scope: s.scope, ref: s.ref,
    source_cluster_id: s.sourceClusterID || null,
    delete_source: s.action === "migration" ? !!s.deleteSource : false,
    phase: "Running", subphase: OPS_SUBPHASES[s.action][0],
    message: `${s.action} started for ${affected.length} volume(s)`,
    started_at: pago(0), completed_at: null,
    results: [], slot_names: affected.map(x => x.name),
    started_ms: Date.now(), step: 0, created_at: pago(0)};
  P.replication_ops.push(rec);
  if (holder) holder.active_ops_ref = rec.name;
  return {obj: replOpsToK8s(rec)};
}

function deletePair(name) {
  const p = pairBy(name);
  if (!p) return rerr(`ReplicationPair "${name}" not found`, "NotFound");
  const pols = P.replication_policies.filter(x => x.pair_ref === name);
  if (pols.length) return rerr(`${pols.length} ReplicationPolicy resource(s) still reference this pair (${pols.map(x => x.name).join(", ")}). Delete them first.`, "Conflict");
  P.replication_pairs = P.replication_pairs.filter(x => x.name !== name);
  return {obj: {status: "Success", details: {name, kind: "replicationpairs"}}};
}
function deletePolicy(name) {
  const p = polBy(name);
  if (!p) return rerr(`ReplicationPolicy "${name}" not found`, "NotFound");
  const slots = P.replication_slots.filter(x => x.policy_ref === name);
  if (slots.length) return rerr(`${slots.length} ReplicationSlot resource(s) still reference this policy. Remove the ${REPL_ANN} annotation from their PVCs first.`, "Conflict");
  P.replication_policies = P.replication_policies.filter(x => x.name !== name);
  return {obj: {status: "Success", details: {name, kind: "replicationpolicies"}}};
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
    pvc.replication_policy = null; pvc.replication_slot = null;
    return {obj: {status: "Success", detached: prev || null}};
  }
  const pol = polBy(policyName);
  if (!pol) return rerr(`No ReplicationPolicy named ${policyName}`, "NotFound");
  if (pvc.status !== "Bound") return rerr(`${pvcName} is ${pvc.status}; a slot is created once the PVC is Bound`, "Conflict");
  P.replication_slots = P.replication_slots.filter(s => s.pvc_ref !== pvcName);
  const lvol = (P.lvols || []).find(v => v.uuid === pvc.lvol_id) || null;
  const slot = slotFor(pol, pvc, lvol, "replicating");
  if (P.replication_slots.some(x => x.name === slot.name))
    return rerr(`ReplicationSlot "${slot.name}" already exists`, "AlreadyExists");
  // a fresh attach starts from a full copy, so nothing has replicated yet
  slot.last_replicated_at = null;
  slot.message = prev && prev !== policyName
    ? `Re-attached from ${prev}: taking a full copy to the new target`
    : "Attached: taking the initial full copy";
  P.replication_slots.push(slot);
  pvc.annotations = Object.assign({}, pvc.annotations, {[REPL_ANN]: policyName});
  pvc.replication_policy = policyName; pvc.replication_slot = slot.name;
  return {obj: slotToK8s(slot)};
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
            s.state = "failed_over"; s.direction = "target";
            s.target_lvol_id = s.target_lvol_id || puuid();
            s.target_nqn = `nqn.2024-05.io.simplyblock:${puuid().slice(0, 8)}`;
            s.message = "Target volume promoted, source is no longer authoritative";
          } else if (o.action === "failback") {
            s.state = "replicating"; s.direction = "source";
            s.target_nqn = null; s.last_replicated_at = pago(0);
            s.message = "Source restored as primary";
          } else {
            s.state = "cutover_done"; s.direction = "target";
            s.message = o.delete_source ? "Cutover committed, source volume deleted" : "Cutover committed";
          }
        }
        return {slot_ref: s.name, status: fail ? "failed" : "succeeded",
          detail: fail ? "Backend rejected the operation for this volume: " + s.message : "",
          target_lvol_id: o.action === "failover" && !fail ? s.target_lvol_id : null};
      });
      const failed = o.results.filter(r => r.status === "failed").length;
      o.phase = failed ? "Failed" : "Succeeded";
      o.subphase = "";
      o.message = failed
        ? `${o.action} completed for ${o.results.length - failed} of ${o.results.length} volume(s); ${failed} failed`
        : `${o.action} completed for ${o.results.length} volume(s)`;
      o.completed_at = pago(0);
      o.started_ms = null;
      // release the lock
      const holder = o.scope === "target" ? pairBy(o.ref)
        : o.scope === "policy" ? polBy(o.ref)
        : polBy((P.replication_slots.find(s => s.name === o.ref) || {}).policy_ref);
      if (holder && holder.active_ops_ref === o.name) holder.active_ops_ref = null;
      return;
    }
    o.subphase = subs[step];
    if (o.action === "failback" && subs[step] === "CommittingFailback")
      o.message = "Holding a short write freeze while the final delta transfers";
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
  ReplicationPair: createPair, ReplicationPolicy: createPolicy, ReplicationOps: createReplOps
});
window.CRD_DELETE = Object.assign(window.CRD_DELETE || {}, {
  ReplicationPair: deletePair, ReplicationPolicy: deletePolicy
});
window.SB_REPL = {setPvcPolicy, normInterval, ivMin, REPL_ANN, tickRepl, slotsInScope, dns1123};
