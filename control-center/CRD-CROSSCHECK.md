# UI ↔ CRD cross-check · `storage.simplyblock.io/v1alpha1`

Checked against **simplyblock-operator PR #475** (`design/crd-rework`, commit `dbf487f`) —
ten design documents under `operator/docs/designs/crd-redesign/`.

**Source access caveat.** The PR body, the file tree, and two asset manifests
(`example-cluster-config.yaml`, `operator-ops-discover.yaml`, `releases.yaml`) were
readable without authentication. The nine design bodies and their Go-type appendices
were **not** — GitHub does not serve blob contents to an unauthenticated fetch. So the
conventions below are quoted from the PR description and the assets; anything marked
*unverified* needs a second pass once the repo is connected.

---

## 1. The kinds

Seventeen kinds, split into entities (desired state) and one-shot operations named
`<Entity>Ops`. Designs exist for nine kinds plus the anchor.

| CRD kind | UI object today | Verdict |
|---|---|---|
| `StorageCluster` | Cluster | present |
| `StorageNode` | Storage node | present |
| `StorageDevice` | Device | present |
| `StoragePool` | Pool | present |
| `StorageBackup` | Backup chain | present, shape differs (§4) |
| `ControlPlane` | Control-plane tab (containers) | **not an object** — panel only |
| `SimplyblockDriver` | CSI version string on the k8s cluster | **missing** (§5) |
| `ClusterDeploymentConfig` | Deployment documents (draft → approve → deploy) | **implemented** |
| `PersistentVolumeOps` | Volume actions | **missing as an object** (§2) |
| `StorageClusterOps` | Cluster actions | **missing as an object** |
| `StorageNodeOps` | Node actions | **missing as an object** |
| `StorageDeviceOps` | Device actions | **missing as an object** |
| `StoragePoolOps` | Pool actions | **missing as an object** |
| `StorageBackupOps` | Backup actions | **missing as an object** |
| `ControlPlaneOps` | — | **missing** |
| `OperatorOps` | Discovery runs | **implemented** (Discover) |

UI objects with **no CRD** in v1alpha1 — expected, the user confirmed replication and DR
are not yet modelled: cluster pairs, DR policies (one object: the storage replication
policy that also carries protected applications — Ramen's DRPolicy is its Kubernetes
projection, named by the replication class), DR clusters, protected apps, consistency
groups, group snapshots, migrations, buckets, zones. Also **PVC** and **StorageClass**, which are core/CSI rather than this group.

---

## 2. The `Ops` model — the largest gap

The design makes every long-running action a **first-class object**, not a fire-and-forget
call. The UI models actions as an API call plus a separate Tasks table, which is a
different architecture, and four consequences are visible to an operator:

| Design rule | UI today |
|---|---|
| `spec.action`, typed and PascalCase | action is the HTTP verb + path |
| `status.phase` and `status.step` (a `statemachine.KubeSnapshot`) | one `status` string per task |
| `status.activeOpsRef` on the entity is a **lock** — one operation at a time | every action is always offered |
| `spec.abort` is the **only** way to stop an operation, and the one mutable field | task list has a generic *Cancel* |
| DELETE webhook: terminal admitted, abortable step unwound, non-abortable **refused** | no delete semantics at all |
| Six events every operation owes, identical on every kind | ad-hoc toasts |
| `observedGeneration` everywhere | absent |

**What this costs the operator today.** The UI will happily offer *Restart* on a node that
is already mid-shutdown, because nothing reads `activeOpsRef`. Under the CRD model that
second action is rejected by the API. The disabled-with-a-reason pattern already used for
failure-domain balance is the right precedent — it should be driven by `activeOpsRef`.

**Recommended change.** Introduce an `Ops` layer: each entity detail shows its
`activeOpsRef` if set, every action button is disabled while one is active with the running
action named as the reason, and the operation view shows `phase` + `step.state` with an
**Abort** control instead of *Cancel*. The existing Tasks tab becomes the history view.

---

## 3. Deployment and discovery — CLOSED

**Resolved.** The console now implements the CRD flow as written: `OperatorOps{Discover}` with a
device filter and node selector, a `ClusterDeploymentConfig` draft carrying `spec.approved: false`,
a review page for the whole document, one-way approval, and the four asynchronous deployment
steps. `nodeSets[].groups[]` are per NUMA socket and carry their own sizing, NICs, device class
and failure domain. The per-host *configure* path remains for a single host joining an existing
cluster. What follows is the original finding, kept for the record.

### Original finding

The CRD flow is: `OperatorOps{action: Discover}` inspects workers and **writes a
`ClusterDeploymentConfig` draft** carrying `spec.approved: false`; an administrator reviews
the whole document and approves it; only then is anything deployed.

```
OperatorOps.Discover  →  ClusterDeploymentConfig (approved: false)  →  review  →  approve  →  deploy
    nodeSelector           nodeSets[].groups[].devices{nvme|block}
    deviceFilter           mgmtInterface / dataInterfaces
      pcieDenyList         sizing{maxSubsystemCount, vcpuCount, minHugePagesSize}
      driveSizeRange       failureDomain: <int>   (per group)
      enableLogicalBlockDevices
```

The UI has the same *intent* — prepare workers, inspect, configure — but:

- **No draft document and no approval gate.** The UI configures each host individually and
  applies immediately. The CRD reviews one document as a whole. This is the difference
  between "approve a plan" and "edit six hosts", and the approval gate is a deliberate
  safety property.
- **No device filter.** `pcieDenyList`, `driveSizeRange` and `enableLogicalBlockDevices` are
  inputs to discovery; the UI has no equivalent, so a boot device can only be excluded by
  hand afterwards.
- **Grouping is missing.** The CRD groups workers into `nodeSets[].groups[]` that share a
  sizing, NICs and a device class. The UI treats every host independently.
- **A growth document** names an existing cluster in `spec.clusterRef` and carries no
  `spec.cluster`. The UI's *Expand — add storage node* is the analogue but is not a document.

---

## 4. Field-level mismatches

Corrected in this pass:

| Was | Now | Source |
|---|---|---|
| `cluster_version: 26.3.x` | `26.2.1 / 26.2.0 / 26.1.2` | `releases.yaml` — controlplane tops out at 26.2.1 |
| CSI version `26.3.x` | `26.2.1 / 26.2.0 / 26.1.2 / 26.1.0` | `releases.yaml` csi-driver |
| distribution `vanilla / eks / rke2 / openshift` | environment `Vanilla / OpenShift / Rancher / K3s / Talos` | `spec.environment`, PascalCase per §7.8 |
| "Erasure coding 4+2" | "Stripe — 4 data + 2 parity chunks" | `spec.cluster.stripe{dataChunks, parityChunks}` |
| node sizing without it | `maxSubsystemCount` added | `nodeSets[].sizing` |

Still open:

- **Failure domains.** CRD: `enableFailureDomains` on the cluster, and `failureDomain: <int>`
  declared **per group**. UI: a scope of `rack | cabinet | zone` derived from host taints.
  The UI's ±1 balance rule is a reasonable operator aid but is **not** in the CRD; the
  numeric per-group declaration is. *(unverified — from the asset, not the design body.)*
- **Device class is per group, not per cluster.** The asset says "a group carries one class
  or the other". The UI puts `device_class` on the cluster.
- **Logical block devices are a 26.4 capability** ("the class 26.4 adds"). The UI offers the
  block device class on clusters running 26.2.x, which the backend would reject.
- **NICs are per group** (`mgmtInterface`, `dataInterfaces`), the UI puts them per host.
- **`spec.creatorRef`, `managed-by` label, finalizers** — the ownership tree is not surfaced
  anywhere. Low operator value on a tile, useful on a detail page.

---

## 5. `SimplyblockDriver` — a real missing screen

`releases.yaml` (served from `install.simplyblock.io`) is a compatibility matrix: each
`csi-driver` and `operator` release declares which `controlplane` releases it works
against. The design reads it for version-skew checking.

The UI shows a CSI version string and nothing else. An operator cannot answer *"is my
driver compatible with my control plane?"* — which is exactly the question a version-skew
design exists to answer. **Recommended: a driver/skew card on the Kubernetes cluster page**,
showing operator, driver and control-plane versions with a compatible / skewed verdict.

---

## 6. Naming conventions to adopt

- Enum values **PascalCase** (§7.8) — the UI uses lowercase `online`, `in_restart`,
  `cutover_pending` throughout. The status vocabulary in `api.jsx` `STATUS_META` is the one
  place to change, and display labels can stay lowercase.
- Metrics `simplyblock_<entity>_<item>_<agg>` (§7.12) — matters when the Prometheus panel
  queries real series; today it queries `spdk_thread_busy_percent`.
- One annotation prefix (§7.3); short names for all seventeen kinds (§7.11).

---

## 7. What has landed

The console no longer calls the control plane REST API. It reads three surfaces
and nothing else:

| Surface | `SB_CONFIG` | Serves |
|---|---|---|
| Kubernetes API | `k8sBase` | CRDs in `storage.simplyblock.io/v1alpha1`, core objects (Node, Pod, PV, PVC, StorageClass, Secret, Event), pod logs |
| Operator API | `operatorBase` | `/releases` compat matrix, SSE watch, and `/proposed/*` for kinds with no CRD |
| Helm | `helmBase` | release, revision, chart and values |

- `k8s-client.jsx` is the transport: real resource paths
  (`/apis/storage.simplyblock.io/v1alpha1/namespaces/{ns}/{plural}`), merge-patch
  for edits, label selectors for the ownership tree, pod-log subresource, and the
  `Ops` helpers.
- `mock-k8s.jsx` / `mock-k8s-server.jsx` serve the fixture store as Kubernetes
  objects on those paths, returning a `Status` object on failure the way an API
  server does. Deleting them and pointing `SB_CONFIG` at a real cluster is the
  whole cutover.
- **Actions are now `Ops` objects.** *Restart node* creates a `StorageNodeOps`
  with `spec.action: Restart`; the object carries `status.phase` and
  `status.step.state` and walks its state machine. `spec.abort` is the only stop
  and is applied as a merge patch; every other spec edit is refused. DELETE
  follows the webhook rule — terminal admitted, abortable step unwound,
  non-abortable **refused** with the reason.
- **`status.activeOpsRef`** is written on the entity while an operation is in
  flight, and a second operation against the same entity is refused with 409.

Still to do, in order:

1. **Read `activeOpsRef` in the action menus** so a second operation is disabled
   in the UI with the running action as the reason, rather than refused at the API.
2. **An operations view** — phase, step, the six events, and Abort — replacing the
   generic task list.
3. **`ClusterDeploymentConfig`** — the draft-and-approve flow.
4. **`SimplyblockDriver`** — the version-skew card, from `/releases`.
5. **Per-group device class and failure domain**, and 26.4 gating for block devices.
6. **PascalCase enums** — one file, mechanical, last.

### The CRDs this console still needs

Every screen below runs on `operator /proposed/*` because v1alpha1 does not model
it. This is the list the CRD model owes the console, and the reason the boundary
is drawn explicitly rather than hidden:

DR policies · cluster pairs · DR clusters · protected apps · consistency groups and their snapshots · migrations · buckets ·
zones · backup policies · snapshots · alerts · tasks · cluster event log

Of these, **tasks** and the **cluster event log** are the least comfortable: a task
is very close to an `Ops` object, and the event log overlaps core `Event`. Both
may be better modelled as those rather than as new kinds.

## 8. Original order of work

1. **`activeOpsRef` locking** — smallest change, largest correctness win: stop offering
   actions the API will reject.
2. **`Ops` objects as a visible layer** — phase, step, abort, and the six events.
3. **`ClusterDeploymentConfig`** — the draft-and-approve flow, replacing per-host configure.
4. **`SimplyblockDriver`** — the version-skew card.
5. **Field corrections** — per-group device class and failure domain, 26.4 gating.
6. **PascalCase enums** — mechanical, do last, one file.

Items 1–4 are behaviour changes and want a decision before they are built.


## Removed from the console (settings the operator owns)

- `blockSize`, `pageSize` — not exposed in cluster create, deploy or detail.
- `haType` — not a per-cluster choice; every cluster is HA.
- `version` — read-only, follows the operator / Helm release; shown on the cluster detail as such.

## Deployment flow (operator model)

1. Kubernetes cluster listed (operator connected) → **not discovered**: worker nodes only, no hardware.
2. `OperatorOps{Discover}` with node label, PCIe allow/deny, block-device pattern, model, capacity range → asynchronous → cluster **discovered**.
3. `ClusterDeploymentConfig` drafted from a discovered cluster only: cluster parameters (name, vCPU, max-subsystems, hugepage override, EC, backups, S3, failure domains, NVMe/block, core isolation, mgmt + data NICs, device filter, NUMA sockets), nodes by label or picked (filterable by site), per-node device and socket overrides.
4. Approval creates the cluster (unready) and runs three asynchronous steps: configure worker nodes (per node, reboot), add storage nodes (per node, parallel), activate. `status.nodes[]` and `status.log[]` carry progress; step/node logs are the operator job pod logs (`/proposed/deployments/{uid}/logs`).

## Ramen Recipe (protected applications)

The console edits the common subset of `ramendr.openshift.io/v1alpha1 Recipe`: groups (kinds + label selector), hooks (exec / check with one op), captureWorkflow and recoverWorkflow (ordered sequence, failOn). Namespace resources are discovered through the Kubernetes API per kind (Deployment, StatefulSet, Service, ConfigMap, Secret, Ingress, PVC, VirtualMachine). Left out: includeClusterResources, inverseOp, essential flags, recipeParameters, multiple ops per hook in the editor (existing ones are preserved and selectable).

## Ransomware recovery (backup-based protection)

A backup policy gains `consistency_group`: every cycle snapshots and backs up all
linked volumes atomically, and every retained version of the chain is a recovery
point. A protected application's `protection_mode` is then either `replication`
(the DR policy's async/sync stream, near-zero RPO) or `backup` (a group-consistent
backup policy). Failover on a backup-protected app takes a recovery point — latest,
a generation, or the last backup before a timestamp — and runs
`RestoringBackups → PromotingVolumes → WaitingForResourceRestore` before the
Recipe boots the workload. Not in the CRD set: `BackupPolicy.spec.consistencyGroup`,
`ProtectedApplication.spec.protectionMode` / `.backupPolicyRef`, and a recovery-point
selector on the failover action.

## Migration paths

Online site migration inside one stretched Kubernetes cluster: a path pairs a source
and target storage cluster, application groups name the workloads, their PVCs' volumes
are replicated asynchronously, and once the backlog converges the VMs live-migrate
(KubeVirt) / containers restart at the target before the volumes are moved online and
the source copies dropped. Not in the CRD set: `MigrationPath`, `ApplicationGroup`.

## DR target architecture (converged Ramen path)

The DR section is now modelled on the target architecture rather than on cluster
pairs. Two authored kinds — `ProtectionPlan` (cluster-scoped) and
`ProtectedApplication` (namespaced) — and everything below them derived:
`DRCluster` one per site, `DRPolicy` one per pair × interval created up front
because every field is immutable, `DRPlacementControl` one per application, and
a `VolumeGroupReplicationClass` per method × interval.

Neither kind exists in `storage.simplyblock.io/v1alpha1`, and neither belongs
there: `dr.simplyblock.io/v1` is the proposed group, and the Ramen kinds must be
consumed as instances only — redefining `ramendr.openshift.io` CRDs breaks the
supported RHACM installation. Both run on `operator /proposed/*` today.

Six side-channel payloads have no field anywhere in the Ramen API. Two are reads
this console depends on: the generation catalogue (no CR represents a
generation) and per-leg lag (only the orchestrated method has a VRG, so
`DRPC.status.lastGroupSyncTime` describes one leg of several). The rest are
peering, generation selection, retention/lock enforcement and the arbitration
token.

Invariants the console enforces or surfaces:

1. **Interval emitted once, written twice.** Divergence between
   `DRPolicy.spec.schedulingInterval` and `parameters.schedulingInterval`
   produces silent non-protection — the policy validates, no class resolves, no
   peerClass appears. Surfaced as a protection gap with a one-field fix.
2. **`pvcSelector` never empty.** Refused at creation.
3. **One DRPC per application.** `orchestratedMethod` is the single source of
   truth for the method Ramen drives.
4. **All pair-policies created at onboarding.** Nothing depends on editing them.
5. **Locked vault objects are never deleted by the CR lifecycle.** Retention is
   object-store state.
6. **Generation pinned before rebind, cleared after promote.**
7. **storageID granularity ≥ application volume set**, or a consistency group
   cannot span the application and restores are not crash-consistent. Stated on
   plan creation.
8. **Instances only, never CRDs.**

Also removed: the `drcluster` object kind (superseded by `site`, which is the
DRCluster projection), the `protectionMode` replication-versus-backup split on
an application (a plan's methods carry that distinction now), and the
application-level recovery-point list (replaced by the generation catalogue).
