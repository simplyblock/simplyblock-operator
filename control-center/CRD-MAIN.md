# Real CRDs — `storage.simplyblock.io/v1alpha1`

Source: **generated CRD reference for main**,
<https://docs.simplyblock.io/dev/reference/operator/reference/> (`dev` tracks
`simplyblock-operator@main`). This file is the authority for field names; where
the UI disagrees, the UI is wrong.

## Headline correction

Replication **is** modelled. The earlier cross-check (against PR #475) recorded
"replication and DR are not yet in the CRD set" — that is no longer true. There
are four replication kinds plus four backup kinds, and the console's
`/proposed/*` collections for cluster pairs, replication policies and per-volume
replication must be rebuilt onto them.

## Resource types (17 root kinds)

```
BackupImport        BackupPolicy      BackupRestore     ControlPlane
ReplicationOps      ReplicationPair   ReplicationPolicy ReplicationSlot
StorageBackup       StorageCluster    StorageClusterOps StorageNode
StorageNodeOps      StorageNodeSet    StoragePool       Task
VolumeMigration
```

## Replication — the real model

Four kinds, and the shape is **not** what the console assumed:

```
ReplicationPair    reusable {sourceCluster, targetCluster}   targetCluster immutable
  └─ ReplicationPolicy  {pairRef, mode, interval, snapshotRetention}
       └─ ReplicationSlot   one per PVC, created automatically
            ReplicationOps  one-shot imperative action
```

**ReplicationPairSpec** — `sourceCluster` (local StorageCluster name, required),
`targetCluster` (name or UUID of remote, required, **immutable after creation**).
Status: `ready`, `backendTargetID`, `message`, `conditions`, `activeOpsRef`
(only one `scope=target` op per pair at a time).

**ReplicationPolicySpec** — `pairRef` (required), `mode` enum
`[failover migration]` default `failover`, `interval` default `5m`,
`snapshotRetention` default `3` **minimum 2**. Status: `ready`,
`backendPolicyID`, `slotCount`, `activeOpsRef`, `conditions`.

- A **StorageClass or PVC** references a policy via the annotation
  `storage.simplyblock.io/replication-policy`.
- The operator creates **one ReplicationSlot per bound PVC** automatically.
- Deletion is blocked while any slot references the policy.
- There is **no retention *schedule*** — one interval, one snapshot count. The
  console's tiered `5m 10x / 15m 4x / 1h 11x / 1d 6x` generation schedule has no
  field here.
- `mode` is only `failover` or `migration`. There is **no `synchronous` mode**.

**ReplicationSlotSpec** — `policyRef`, `pvcRef`, `volumeID` (backend lvol UUID),
all three immutable. Status: `state` enum
`[attaching replicating cutover_pending cutover_done failed_over detaching error]`,
`direction` enum `[source target]`, `sourceLvolID`, `targetLvolID`, `targetNQN`
(after failover), `lastReplicatedAt`, `message`, `conditions`.

**ReplicationOpsSpec** — one-shot, all fields immutable:
- `action` enum `[failover failback migration]` (required).
  `failover` = unplanned, promote target clone, source may be down.
  `failback` = restore source as primary after a failover.
  `migration` = planned cutover, `replication_commit` per volume, both clusters up.
  State progression `replicating → cutover_pending → cutover_done`.
- `scope` enum `[target policy volume]` (required) — pair-wide, policy-wide, or
  a single slot.
- `ref` — the name of the pair / policy / slot named by `scope`.
- `sourceClusterID` — failback only, omit to recover to the original source.
- `deleteSource` — migration only.

Status: `phase` enum `[Pending Running Succeeded Failed]`, `subphase` (free
text, e.g. `TriggeringFailover`, `UpdatingSlotStatuses`, `ReleasingLock`),
`message`, `startedAt`, `completedAt`, `results[]` of
`{slotRef, status enum [succeeded skipped failed], detail, targetLvolID}`.

### Replication — operational rules

From <https://docs.simplyblock.io/dev/kubernetes/operations/data-protection/asynchronous-replication/>.

Short names: `relpair`, `repl`, `relslot`, `replops`.

**Prerequisites the console must respect.** Both clusters are `StorageCluster`
resources **in the same namespace** as the replication resources, i.e. both
attached to the same control plane. A cluster is referenced by its
`StorageCluster` **name**, and its `status.uuid` must be populated before
replication can be configured. **Cross-namespace references are not
supported.** Both clusters active, storage nodes online, network
interconnectivity. Kubernetes-only feature.

**Volume selection is by annotation, not by membership.** The console's "add
and remove volumes from a policy" is wrong. A volume opts in via
`storage.simplyblock.io/replication-policy: <policy>`, read from the **PVC and
from its StorageClass, PVC wins**. Annotating the StorageClass replicates every
volume it provisions. The slot is created once the PVC is `Bound` and the policy
is ready, is named **`<policy>-<pvc>`**, and is **owned by its PVC** — deleting
the PVC deletes the slot.

Repointing the annexation at another policy is a **detach then a fresh attach**,
so the new target takes a **full copy**. Removing the annotation deletes the
replication snapshots on both sides, then removes the slot.

`spec.volumeID` format is `<clusterUUID>:<poolUUID>:<volumeUUID>`.

**Interval** is rounded to whole minutes, minimum one minute; an unparseable
value silently falls back to `5m`. `snapshotRetention` minimum 2.

**Failover is never automatic.** It is always a `ReplicationOps`. Scope table:

| scope | `ref` names | effect |
|---|---|---|
| `target` | ReplicationPair | every volume of every policy on the pair |
| `policy` | ReplicationPolicy | every volume attached to that policy |
| `volume` | ReplicationSlot | one volume, exactly one slot must match |

- A `ref` that does not resolve to the matching kind is **rejected by a
  validating webhook** at create time.
- An op that reached `Succeeded` or `Failed` is **never re-run** — a repeat or a
  correction needs a **new** `ReplicationOps`.
- **Failback supports only `policy` and `volume` scope.** A `target`-scoped
  failback is rejected and the operation fails; a pair-wide failback is done one
  policy at a time.
- Locking: one op per policy via `ReplicationPolicy.status.activeOpsRef`; a
  `target`-scoped op locks the pair via `ReplicationPair.status.activeOpsRef`.
  A second operation **waits for the lock rather than failing**.
- Failback holds a **short freeze** while the final delta transfers — plan a
  maintenance window.
- Failback reports **per-volume outcomes independently**: one volume `failed`
  while the rest continue, and the operation as a whole ends `Failed`.

`status.subphase` values seen during `Running`: `TriggeringFailover`,
`TriggeringTargetFailover`, `UpdatingSlotStatuses`, `StartingFailback`,
`CommittingFailback`.

**Monitoring.** While replicating, the operator polls the backend every **60 s**
and writes `status.lastReplicatedAt`; after a failed call it backs off to
**30 s**. Backend-originated state changes (an external cutover or failover) are
picked up by the same poll.

**Slot state notes.** An attach is synchronous on the backend, so a new slot
reaches `replicating` **directly**; `attaching` is a legacy state only seen on
slots from earlier operator versions.

**Deletion order** is enforced by the operator: a policy cannot go while a slot
references it, a pair cannot go while a policy references it. The backend
replication target is deleted with the pair.

**Events** worth surfacing — on `ReplicationSlot`: `Replicating`,
`CutoverPending`, `FailedOver`, `Detached`, `Error`; on `ReplicationOps`:
`FailoverSucceeded`, `FailbackSucceeded`, `Failed`.

**What does not exist.** There is no synchronous replication mode, no tiered
retention schedule, no consistency-group field, and no per-policy volume list.
`mode` is exactly `failover | migration`.

## Backup — the real model

```
BackupPolicy   namespaced, attached to a PVC by ANNOTATION
StorageBackup  one backup instance (snapshot + upload)
BackupRestore  restore a StorageBackup into a new PVC
BackupImport   import a foreign cluster's backup, creates a StorageBackup CR
```

**BackupPolicy** is attached with the PVC annotation
`simplyblock.io/backup-policy: <name>` (deprecated alias
`simplybk/backup-policy`, new one wins). Policy must be in the **same
namespace** as the PVC. Spec:

- `clusterName`
- `maxVersions` — max completed versions; **when exceeded the oldest is merged
  into the second-oldest** (matches the console's merge semantics exactly)
- `maxAge` — pattern `^[1-9]\d*[mhdw]$`, older backups are merged
- `schedule` — pattern `^(\d+[mhdw],\d+)( +\d+[mhdw],\d+)*$`, i.e.
  space-separated `interval,keep_count` pairs, e.g. `"15m,4 60m,11 24h,7"`,
  **intervals strictly increasing**, units `m h d w`

Status: `phase`, `message`, `clusterUUID`, `policyID`, `attachedLvols[]` of
`{pvcName, pvcNamespace, lvolID}`.

> The console's schedule editor is close but must emit this exact string form,
> and there is **no "online snapshot generations" column** — `maxVersions` +
> `schedule` keep-counts are the whole retention model.

**StorageBackupSpec** — `clusterName`, `pvcRef {name, namespace}`,
`snapshotName` (override), `sourceClusterUUID` (set by BackupImport only).
Status is rich and worth surfacing: `phase`, `apiStatus`, `message`,
`clusterUUID`, `pvcNamespace`, `pvName`, `poolName`, `poolUUID`, `lvolID`,
`lvolName`, `fsType`, `snapshotID`, `snapshotName`, `sourceClusterUUID`,
`backupID`, `s3ID`, `nodeID`, **`prevBackupID` (the chain link)**, `size`,
`allowedHosts[]`, `createdAt`, `completedAt`.

**BackupRestoreSpec** — `clusterName`, `backupRef {name}`, `targetPool`
(override), `targetNode` (node UUID), `pvcTemplate {metadata{name,labels,
annotations}, spec: PersistentVolumeClaimSpec}`; requested storage must be
>= backup size. Status adds `sourceLvolID`, `fsType` (preserved from the
backup so the restored PV mounts the original filesystem), `restoredLvolID`,
`pvName`, `pvcName`, `pvcNamespace`, `sourceClusterUUID`, `startedAt`,
`completedAt`.

**BackupImportSpec** — `sourceClusterName`, `sourceBackupID` (pattern
`^[a-zA-Z0-9_-]{1,128}$`), `targetClusterName`. Status yields
`storageBackupRef`, the new CR a BackupRestore can point at.

## StorageCluster

Spec (exact field names):

```
enableNodeAffinity            bool
stripe                        {dataChunks, parityChunks}
fabricType                    string
clientDataIfname              string
nvmfBasePort / rpcBasePort / snodeApiPort   int
maxConcurrentWorkerRestarts   int
maxSubsystemCount             int
maxHugePagesSize              string
vcpuCount                     int
warningThreshold              {capacity, provisionedCapacity}
criticalThreshold             {capacity, provisionedCapacity}
backup                        {localEndpoint, snapshotBackups, withCompression,
                               secondaryTarget, localTesting,
                               credentialsSecretRef{name}}
hashicorpVaultSettings        {baseURL}
volumeMigrationSettings       {enabled, rebalancerImage,
                               dataRealignment{enabled, interval, minMoves}}
volumeAutoPlacement           {enabled, migrationEnabled, evaluationInterval,
                               imbalanceThreshold, minHotColdDifferencePct,
                               defaultCoolDownSeconds,
                               maxVolumeMigrationsPerCycle,
                               storageNodeCandidateCount, metricsBackend,
                               prometheusURL, latencyBenchmarkEnabled,
                               latencyBenchmarkInterval, iopsWeight,
                               throughputWeight}
enableFailureDomains          bool
```

Status:

```
uuid, phase, subPhase, clusterName, mgmtNodes, storageNodes, nqn, status,
rebalancing, volumeMoveGeneration, realignedGeneration, lastDataRealignmentAt,
erasureCodingScheme, lastUpdated, created, configured, maxFaultTolerance,
maxConcurrentWorkerRestarts, activeOpsRef, rebalancingMetrics{...}
```

`maxFaultTolerance` is the real name for the fault budget the console computes
locally. `rebalancingMetrics` = `{avgDeviationPct, maxDeviationPct,
hottestNodeUUID, coolestNodeUUID, imbalancePercent, lastEvaluatedAt,
lastMigrationAt, nodeMetrics[]{nodeUUID, latencyDeviationPct, volumeCount,
lastUpdated}}`.

Notable: **KMS is `hashicorpVaultSettings.baseURL` only** — no provider
enum, no key rotation fields. The S3 backup target is
`backup.localEndpoint` + `backup.credentialsSecretRef`, and
`backup.snapshotBackups` is the snapshot-backup toggle the console describes
in prose.

`volumeAutoPlacement.metricsBackend` enum `[controlplane prometheus uniform]`
(`uniform` returns IOPS=1 per node, disabling IOPS scoring while keeping
capacity/volume-count balancing).

## Ops kinds

`StorageClusterOps` (spec/status/phase), `StorageNodeOps` (spec/status/phase
**and subPhase**), plus:

- `NodeRollingRestartSpec {refreshSNodeAPI}` and `NodeRollingRestartStatus
  {pendingNodes[], processedNodes[], nodePhase, phaseTriggered}` where
  `nodePhase` ∈ `snode-refresh | snode-refresh-wait | shutting-down |
  restarting | rebalancing`.
- `DrainOpsSpec {systemVolumeFilterRegex}` for `action=remove`, default
  `^sb-fio-baseline-.*` — matching volumes are excluded from drain migration
  and deleted inline in the **Verifying** phase.
- `StorageCluster.status.activeOpsRef` / `ReplicationPolicy.status.activeOpsRef`
  are the single-op locks.

## StorageNodeSet — the deployment object

This is what the console's invented `ClusterDeploymentConfig` should be. Known
fields: `spec.clusterImage`, `spec.journalManager {count, percentPerDevice}`,
`spec.nodeConfigs[hostname].failureDomain`, `spec.nodeFailureDomains[hostname]`.
Status carries `nodes[]` of `NodeStatus` and `nodeDrainStates[]`:

**NodeStatus** — `uuid, health(bool), status, cpu, memory, volumes, rpcPort,
lvolPort, nvmfPort, devices(string), uptime, hostname, mgmtIp, postedAt,
failureDomain(int, 0 = unset)`.

**NodeDrainState** — `hostname`, `phase` enum
`[detected shutdown_called draining restart_called complete failed]`,
`startedAt`, `message`, `activeNodeUUID` (sequences multiple NUMA-socket nodes
on one worker, one at a time).

**NodeLatencyMetrics** — `nodeUUID, baselineP50NS, baselineP99NS,
baselineMeasuredAt` (fio-measured 4K NVMe-oF latency).

## StoragePool

`StoragePoolSpec` carries `storageClassParameters`, **immutable once set**
because Kubernetes StorageClass parameters are immutable — to change a pool's
StorageClass defaults you create a new pool. Parameters:

```
qosRwIops (0=unlimited)   qosRwMbytes   qosRMbytes   qosWMbytes
encryption (bool, false)  fabric (tcp)  maxNamespacePerSubsys (1)
tune2fsReservedBlocks     filesystem enum [ext4 xfs] default xfs
```

`cluster_id` and `pool_name` are always set automatically and cannot be
overridden. Also `StoragePoolQoSSpec` / `StoragePoolQoSThroughputSpec` and
their status counterparts.

> Confirms the console's pool→StorageClass story, and adds the immutability
> rule it does not yet state. Note `filesystem` defaults to **xfs**, and
> `tune2fsReservedBlocks` left unset ≠ `"0"` (the latter actively runs
> `tune2fs -m 0`).

## VolumeMigration

Real kind, with `VolumeMigrationPhase`, `VolumeMigrationSpec/Status`,
`MigrationConnection` (per-path NVMe-oF connect parameters returned by the
storage API's `CreateMigration`: `nqn, ip, port, transport, nrIoQueues,
reconnectDelay, ctrlLossTmo, fastIOFailTmo, keepAliveTmo`, passed verbatim to
`nvme connect` in a validation Job) and `ValidationJob`.

Cluster-level settings live in `StorageCluster.spec.volumeMigrationSettings`
and `volumeAutoPlacement` (above), not on the migration itself.

## ControlPlane

Singleton, **one per namespace, named `simplyblock`**, created by the Helm
chart, must not be created or deleted by hand. `spec.image` is pattern-checked
against three trusted registries (`quay.io/simplyblock-io`,
`docker.io/simplyblock`, `public.ecr.aws/simply-block`) with optional digest
pinning; StorageNodeSets omitting `spec.clusterImage` inherit it.
`status.phase` enum `[Initializing Ready]` — Ready once the FDB health check
passes — plus `message` and `lastChecked`.

## Task

`Task`, `TaskSpec`, `TaskStatus`, `TaskEntry` — the task list the console shows
is a real CRD, not a control-plane-only read.

## Still to read

Field tables for `StorageNodeSpec/Status`, `StorageNodeSetSpec/Status`,
`StoragePoolSpec/Status`, `StorageClusterOpsSpec/Status` +
`StorageClusterOpsPhase`, `StorageNodeOpsSpec/Status` + `Phase`/`SubPhase`
enums, `VolumeMigrationSpec/Status` + `Phase`, `TaskSpec/Status/Entry`,
`StorageNodeOverrides`, `StorageNodePorts`, `StorageNodeResources`,
`StripeSpec`, `VolumeAutoPlacementSettings`. The generated page truncates
mid-`StorageCluster`; these are below the cut.

## Access control — CRD change request

`AccessRole` and `AccessBinding` do not exist. The console implements
RBAC-DESIGN.md against `operator /proposed/access-roles`,
`/proposed/access-bindings` and `GET /proposed/access/self`; the proposed
CRD shapes and the webhook contract are in that document. The pre-defined
roles should generate the matching `ClusterRole simplyblock:<role>` objects so
the Kubernetes layer and the scoped layer cannot drift.

## Not in the CRD set (unchanged)

Everything above the storage layer: Ramen kinds (`ramendr.openshift.io` —
consumed as instances, never redefined), the console's `ProtectionPlan` /
`ProtectedApplication` top layer, sites/zones, consistency groups, buckets and
the S3 service, pNFS file storage, hosts/discovery, and the observability
reads (SPDK logs, thread utilisation, container allocation, FDB backups).
These stay on `operator /proposed/*` and are listed as CRD change requests.
