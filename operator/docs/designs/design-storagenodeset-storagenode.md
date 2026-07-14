# Design Document: StorageNodeSet / StorageNode / StorageNodeOps Three-Tier Model

**Status:** Phase 1 Complete  
**Author:** Israel Geoffrey  
**Date:** 2026-07-14

---

## 1. Background

The current `StorageNodeSet` CRD conflates three distinct concerns:

| Concern | Current location | Problem |
|---|---|---|
| Fleet management (DaemonSet, RBAC, provisioning) | `StorageNodeSet.spec` | Mixed with per-node config |
| Per-node configuration and status | `StorageNodeSet.status.nodes[]` | No individual CR to target |
| Imperative operations (shutdown, restart, drain) | `StorageNodeSet.spec.action` | One action at a time on a fleet object |

---

## 2. Goals

- **`StorageNodeSet`** — fleet template: defaults, DaemonSet, RBAC, services. Creates and owns `StorageNode` CRs.
- **`StorageNode`** — individual node: configuration overrides, observed status, health. One CR per (worker, socket).
- **`StorageNodeOps`** — imperative operations: a one-shot CR that targets a `StorageNode` and drives an action (shutdown, restart, suspend, resume, remove/drain) to completion, then records the result.

## 3. Non-Goals

- Changing the backend provisioning protocol.
- Replacing DaemonSet/RBAC/Service management.
- Automatic migration of existing `StorageNodeSet`-only deployments (migration strategy is a follow-up).

---

## 4. Architecture Overview

```
StorageNodeSet  (fleet / template)
    │  owns
    ├─► StorageNode  (worker-A, socket 0)  ←── StorageNodeOps (action: remove)
    ├─► StorageNode  (worker-A, socket 1)
    ├─► StorageNode  (worker-B, socket 0)  ←── StorageNodeOps (action: restart)
    └─► StorageNode  (worker-C, socket 0)

VolumeMigration CRs  (owned by StorageNodeOps during drain)
```

`StorageNodeOps` is scoped to a single `StorageNode`. Multiple operations can run concurrently on different nodes. Only one `StorageNodeOps` can be active per `StorageNode` at a time (enforced by a validating webhook or controller).

---

## 5. API Design

### 5.1 StorageNodeSet (revised — fleet only)

Removes all per-node action and status fields. Adds a summary status.

```go
type StorageNodeSetSpec struct {
    ClusterName    string   `json:"clusterName"`
    ClusterImage   string   `json:"clusterImage,omitempty"`
    SpdkImage      string   `json:"spdkImage,omitempty"`
    SpdkProxyImage string   `json:"spdkProxyImage,omitempty"`
    MgmtIfname     string   `json:"mgmtIfname"`
    DataIfname     []string `json:"dataIfname,omitempty"`
    WorkerNodes    []string `json:"workerNodes"`

    // Fleet-wide defaults — apply to every StorageNode unless overridden by NodeConfigs.
    MaxLogicalVolumeCount *int32              `json:"maxLogicalVolumeCount,omitempty"`
    Partitions            *int32              `json:"partitions,omitempty"`
    JournalManagerSpec    *JournalManagerSpec `json:"journalManager,omitempty"`
    CorePercentage        *int32              `json:"corePercentage,omitempty"`
    SpdkSystemMemory      string              `json:"spdkSystemMemory,omitempty"`
    MaxSize               string              `json:"maxSize,omitempty"`
    NodesPerSocket        *int32              `json:"nodesPerSocket,omitempty"`
    SocketsToUse          []string            `json:"socketsToUse,omitempty"`
    // ... tolerations, resources, image pull policy, OpenShift fields, etc.

    // NodeConfigs allows per-worker-node configuration overrides keyed by the
    // Kubernetes worker node name. Entries here are propagated to the corresponding
    // StorageNode.spec.overrides by the reconciler on every reconcile — the
    // StorageNodeSet is the single source of truth for all per-node config,
    // including failure domain assignment via nodeConfigs[worker].failureDomain.
    //
    // Example:
    //   nodeConfigs:
    //     vm02.simplyblock3.localdomain:
    //       maxLogicalVolumeCount: 50
    //       spdkSystemMemory: "8G"
    //       failureDomain: 1
    //     vm04.simplyblock3.localdomain:
    //       maxLogicalVolumeCount: 10
    //       failureDomain: 2
    // +optional
    NodeConfigs map[string]StorageNodeOverrides `json:"nodeConfigs,omitempty"`

    // REMOVED: action, nodeUUID, force, reattachVolume, workerNode
    // REMOVED: systemVolumeFilterRegex (moves to StorageNodeOps)
    // REMOVED: nodeFailureDomains (consolidated into NodeConfigs[].failureDomain)
}

type StorageNodeSetStatus struct {
    // Summary counts derived from owned StorageNode statuses.
    TotalNodes   int `json:"totalNodes,omitempty"`
    OnlineNodes  int `json:"onlineNodes,omitempty"`
    OfflineNodes int `json:"offlineNodes,omitempty"`

    // PendingNodeAdds is the provisioning guard against duplicate POSTs.
    PendingNodeAdds         map[string]metav1.Time `json:"pendingNodeAdds,omitempty"`
    SchedulingFailedWorkers map[string]bool        `json:"schedulingFailedWorkers,omitempty"`
    DrainCoordination       []NodeDrainState       `json:"drainCoordination,omitempty"`

    // REMOVED: nodes[], actionStatus (both live on StorageNode)
}
```

### 5.2 StorageNode (new CRD)

One CR per backend storage node instance. Owned by `StorageNodeSet`.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.workerNode"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Health",type=boolean,JSONPath=".status.health"
type StorageNode struct { ... }

type StorageNodeSpec struct {
    // StorageNodeSetRef is the owning StorageNodeSet name. Immutable.
    // +k8s:immutable
    StorageNodeSetRef string `json:"storageNodeSetRef"`

    // WorkerNode is the Kubernetes node hostname. Immutable.
    // +k8s:immutable
    WorkerNode string `json:"workerNode"`

    // SocketIndex is the NUMA socket index (0-based). Immutable.
    // +k8s:immutable
    // +optional
    SocketIndex *int32 `json:"socketIndex,omitempty"`

    // Overrides allow per-node configuration overrides of the parent StorageNodeSet defaults.
    // Immutable fields from the parent (clusterName, mgmtIfname, partitions, etc.) cannot
    // be overridden here.
    // +optional
    Overrides *StorageNodeOverrides `json:"overrides,omitempty"`
}

type StorageNodeOverrides struct {
    MaxLogicalVolumeCount *int32 `json:"maxLogicalVolumeCount,omitempty"`
    SpdkSystemMemory      string `json:"spdkSystemMemory,omitempty"`

    // FailureDomain is the failure-domain group index (≥ 1) for this node.
    // Required when the parent StorageCluster has enableFailureDomains=true.
    // Set via StorageNodeSet.spec.nodeConfigs[workerNode].failureDomain.
    // +kubebuilder:validation:Minimum=1
    // +optional
    FailureDomain *int32 `json:"failureDomain,omitempty"`

    // ... other mutable per-node tuning knobs
}

type StorageNodeStatus struct {
    // UUID is the backend storage node UUID. Set once after node-add completes.
    UUID     string `json:"uuid,omitempty"`
    Status   string `json:"status,omitempty"`
    Health   bool   `json:"health,omitempty"`
    CPU      *int32 `json:"cpu,omitempty"`
    Memory   string `json:"memory,omitempty"`
    Volumes  *int32 `json:"volumes,omitempty"`
    Devices  string `json:"devices,omitempty"`
    MgmtIp   string `json:"mgmtIp,omitempty"`
    Hostname string `json:"hostname,omitempty"`
    Uptime   string `json:"uptime,omitempty"`
    RpcPort  *int32 `json:"rpcPort,omitempty"`
    LvolPort *int32 `json:"lvolPort,omitempty"`
    NvmfPort *int32 `json:"nvmfPort,omitempty"`

    // PostedAt is when the node-add POST was sent (provisioning guard).
    PostedAt *metav1.Time `json:"postedAt,omitempty"`

    // ActiveOpsRef is the name of the currently active StorageNodeOps on this node.
    // Empty when no operation is in progress.
    ActiveOpsRef string `json:"activeOpsRef,omitempty"`
}
```

### 5.3 StorageNodeOps (new CRD)

A one-shot operational CR targeting a single `StorageNode`. Analogous to a Kubernetes `Job`.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.storageNodeRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type StorageNodeOps struct { ... }

type StorageNodeOpsSpec struct {
    // StorageNodeRef is the name of the target StorageNode. Immutable.
    // +k8s:immutable
    StorageNodeRef string `json:"storageNodeRef"`

    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove
    // +k8s:immutable
    Action string `json:"action"`

    // Force enables forced execution where the backend supports it.
    // +optional
    Force *bool `json:"force,omitempty"`

    // ReattachVolume reattaches volumes during restart.
    // +optional
    ReattachVolume *bool `json:"reattachVolume,omitempty"`

    // Drain configures the drain workflow for action=remove.
    // +optional
    Drain *DrainSpec `json:"drain,omitempty"`
}

type DrainSpec struct {
    // SystemVolumeFilterRegex filters system/benchmark volumes from drain migration.
    // Defaults to "^sb-fio-baseline-.*".
    // +optional
    SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
}

// StorageNodeOpsPhase is the lifecycle phase of a StorageNodeOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageNodeOpsPhase string

const (
    StorageNodeOpsPhasePending   StorageNodeOpsPhase = "Pending"
    StorageNodeOpsPhaseRunning   StorageNodeOpsPhase = "Running"
    StorageNodeOpsPhaseSucceeded StorageNodeOpsPhase = "Succeeded"
    StorageNodeOpsPhaseFailed    StorageNodeOpsPhase = "Failed"
)

type StorageNodeOpsStatus struct {
    // Phase is the high-level lifecycle phase.
    Phase StorageNodeOpsPhase `json:"phase,omitempty"`

    // SubPhase tracks the active drain step (Validating|Suspending|Migrating|Verifying|Removing).
    // +kubebuilder:validation:Enum=Validating;Suspending;Migrating;Verifying;Removing
    // +optional
    SubPhase string `json:"subPhase,omitempty"`

    // Message is a human-readable description of the current state or failure reason.
    Message string `json:"message,omitempty"`

    // VolumesMigrated is the count of volumes migrated (drain only).
    VolumesMigrated int `json:"volumesMigrated,omitempty"`
    // VolumesPending is the count of volumes awaiting migration (drain only).
    VolumesPending int `json:"volumesPending,omitempty"`

    // StartedAt is when the operation began.
    StartedAt *metav1.Time `json:"startedAt,omitempty"`
    // CompletedAt is when the operation finished (successfully or not).
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}
```

---

## 6. Controller Changes

### 6.1 StorageNodeSetReconciler

Narrowed to **fleet management only**:

1. Ensure DaemonSet, RBAC, Services, EndpointSlices — unchanged.
2. **Reconcile StorageNode CRs** — for each `(workerNode, socket)` in `spec.workerNodes × spec.socketsToUse`:
   - Create a `StorageNode` CR with owner reference if it does not exist.
   - On every reconcile, sync `StorageNode.spec.overrides` from `StorageNodeSet.spec.nodeConfigs[workerNode]`. The `StorageNodeSet` is the single source of truth — users edit `spec.nodeConfigs` on the `StorageNodeSet`, never the `StorageNode` directly.
   - Sync `StorageNode.spec.overrides.failureDomain` from `StorageNodeSet.spec.nodeConfigs[workerNode].failureDomain`. If the referenced `StorageCluster` has `enableFailureDomains=true` and no `failureDomain` is set for this worker, emit a Warning event and block node-add until it is populated.
   - Do NOT manage the backend node lifecycle here.
3. **Remove stale StorageNode CRs** — if a worker is removed from `spec.workerNodes`, delete the owned `StorageNode` CRs (deletion triggers drain via `StorageNodeOps` if the node is online).
4. **Aggregate status** — count online/offline from `StorageNode` statuses.

### 6.2 StorageNodeReconciler (new)

Owns the per-node provisioning loop:

```
StorageNode.status.uuid == ""  →  postStorageNode() → pollNodeOnline()
StorageNode.status.uuid != ""  →  syncStatus() every 30s
StorageNode deletion           →  ensure a StorageNodeOps(action=remove) exists if node is online
```

Watches:
- `StorageNode` (primary)
- `StorageNodeSet` (to read effective config)

### 6.3 StorageNodeOpsReconciler (new)

Drives all imperative operations. Replaces the existing action handling in `StorageNodeSetReconciler`.

```
StorageNodeOps created →
    1. Check no other active ops on the target StorageNode (set StorageNode.status.activeOpsRef)
    2. Set Phase=Running
    3. Dispatch to handler based on spec.action:
       - shutdown/restart/suspend/resume → existing waitForActionCompletion pattern
       - remove → existing drain state machine (Validating→Suspending→Migrating→Verifying→Removing)
    4. On completion → set Phase=Succeeded/Failed, clear StorageNode.status.activeOpsRef
```

Owns `VolumeMigration` CRs during drain (`.Owns(&VolumeMigration{})`).

**Mutual exclusion:** The reconciler checks `StorageNode.status.activeOpsRef` before starting. If set, it requeues. A validating webhook may additionally reject a new `StorageNodeOps` if one is already active on the same node.

---

## 7. Migration Strategy

### Phase 1 — Complete ✓

Implemented on 2026-07-14. Deviations from the original plan:

- `StorageNodeSet.spec.action`, `spec.nodeUUID`, `spec.workerNode`, `spec.force`,
  `spec.reattachVolume`, `spec.systemVolumeFilterRegex` and `status.actionStatus` were
  **removed outright** rather than shimmed. All imperative operations must now use a
  `StorageNodeOps` CR. This is a breaking API change but results in a significantly
  cleaner codebase.
- `StorageNodeSetReconciler` creates `StorageNode` CRs for every `(workerNode, socket)`
  pair in `spec.workerNodes × spec.socketsToUse` and aggregates their status into
  `StorageNodeSetStatus.TotalNodes / OnlineNodes / OfflineNodes`.
- `StorageNodeOpsReconciler` watches `StorageNode` changes so pending ops acquire the
  lock immediately when `activeOpsRef` is cleared, rather than waiting for the poll timer.
- CRD YAMLs generated and deployed to the helm chart.
- 28 unit tests added covering both new reconcilers.

### Phase 2 — Move provisioning to StorageNodeReconciler

- `StorageNodeReconciler` takes over node-add POST and status sync from
  `StorageNodeSetReconciler.reconcileWorkerNodes`. The `provisionNode` and `syncStatus`
  stubs are already in place; they need to consume the existing `postStorageNodeSet` and
  `pollNodeOnline` logic refactored to read from `StorageNode` instead of `StorageNodeSet`.

### Phase 3 — Remove legacy provisioning fields

- Remove `status.nodes[]`, `status.pendingNodeAdds`, `status.schedulingFailedWorkers`
  and `status.drainCoordination` from `StorageNodeSet` once `StorageNodeReconciler`
  drives provisioning end-to-end.
- Remove `reconcileWorkerNodes` and related helpers from `StorageNodeSetReconciler`.

---

## 8. Open Questions

**Q1: StorageNodeOps retention policy**  
How long should completed `StorageNodeOps` CRs be retained? Similar to `ttlSecondsAfterFinished`
on Jobs, or retained indefinitely for audit?

**Q2: Concurrent ops on different nodes of the same set**  
Multiple `StorageNodeOps` targeting different `StorageNode` CRs in the same `StorageNodeSet`
should be allowed. How many concurrent drains are permitted given FTT constraints?

**Q3: StorageNode naming**  
Name pattern: `{storagenodeset-name}-{sanitised-worker}-{socket}`. Needs to be deterministic
and DNS-label safe.

**Q4: Adoption of pre-existing backend nodes**  
If a node already exists in the backend (from a prior deployment), the `StorageNode` CR should
be created with `status.uuid` pre-populated rather than triggering a new node-add.

**Q5: Who triggers the drain StorageNodeOps on scale-down?**  
When `StorageNodeSet.spec.workerNodes` shrinks, the controller must decide whether to trigger
`action=remove` or simply delete the `StorageNode` CR. Explicit `StorageNodeOps(action=remove)`
is safer.

---

## 9. Architecture Diagrams

**Ownership Diagram** — Shows the ownership hierarchy from StorageCluster through StorageNodeSet down to individual StorageNode and StorageNodeOps resources, including how VolumeMigration CRs are created during drain operations.

```
StorageCluster
  spec.enableFailureDomains = true
  │
  │  (referenced by clusterName)
  ▼
StorageNodeSet                              (fleet / template)
  spec.nodeFailureDomains:
    worker-A: 1
    worker-B: 2
    worker-C: 1
  spec.nodeConfigs:
    worker-A: { maxLogicalVolumeCount: 50 }
  spec.workerNodes: [worker-A, worker-B, worker-C]
  │
  │  owns (OwnerReference)
  ├──────────────────────────────────────────────────────┐
  ▼                                                      ▼
StorageNode                                         StorageNode
  name: sns-worker-a-0                                name: sns-worker-b-0
  spec.workerNode:    worker-A                         spec.workerNode:    worker-B
  spec.socketIndex:   0                                spec.socketIndex:   0
  spec.failureDomain: 1   ◄── propagated               spec.failureDomain: 2   ◄── propagated
  spec.overrides:                                      status.uuid:        <uuid-b>
    maxLogicalVolumeCount: 50                          status.activeOpsRef: "ops-restart-b"
  status.uuid:        <uuid-a>                         │
  status.activeOpsRef: "ops-remove-a"                 │  targeted by
  │                                                    ▼
  │  targeted by                                  StorageNodeOps
  ▼                                                 name: ops-restart-b
StorageNodeOps                                      spec.storageNodeRef: sns-worker-b-0
  name: ops-remove-a                                spec.action: restart
  spec.storageNodeRef: sns-worker-a-0               status.phase: Running
  spec.action:  remove                              status.subPhase: ""
  spec.drain:                                       (no VolumeMigration CRs)
    systemVolumeFilterRegex: "^sb-fio-.*"
  status.phase:    Running
  status.subPhase: Migrating
  status.volumesMigrated: 3
  status.volumesPending:  2
  │
  │  owns (during drain only)
  ├──────────────────────────┐
  ▼                          ▼
VolumeMigration          VolumeMigration
  name: vm-lvol-aaa          name: vm-lvol-bbb
  status.phase: Completed    status.phase: Running
  (deleted on Verifying)     (in-flight)

  ┌─────────────────────────────────────────────────────────┐
  │  StorageNode  (worker-C, socket 0)                      │
  │    spec.failureDomain: 1                                │
  │    status.activeOpsRef: ""  (no active operation)       │
  └─────────────────────────────────────────────────────────┘
```

---

**Reconciler Flow** — Illustrates the three controllers and what each reconciler watches, manages, and produces.

```
  ┌───────────────────────────────────────────────────────────────────────────────────┐
  │                          Kubernetes Controller Manager                            │
  └───────────────────────────────────────────────────────────────────────────────────┘
           │                         │                          │
           ▼                         ▼                          ▼
  ┌─────────────────────┐  ┌──────────────────────┐  ┌──────────────────────────┐
  │ StorageNodeSet      │  │ StorageNode           │  │ StorageNodeOps           │
  │ Reconciler          │  │ Reconciler  (new)     │  │ Reconciler  (new)        │
  ├─────────────────────┤  ├──────────────────────┤  ├──────────────────────────┤
  │ WATCHES             │  │ WATCHES               │  │ WATCHES                  │
  │  • StorageNodeSet   │  │  • StorageNode        │  │  • StorageNodeOps        │
  │  • Pod (SPDK ready) │  │  • StorageNodeSet     │  │  • StorageNode           │
  │  • Node (labels)    │  │    (read config)      │  │  • VolumeMigration       │
  ├─────────────────────┤  ├──────────────────────┤  ├──────────────────────────┤
  │ MANAGES             │  │ MANAGES               │  │ MANAGES                  │
  │  • DaemonSet        │  │  • backend node-add   │  │  • backend actions:      │
  │  • ServiceAccount   │  │    POST (if uuid=="") │  │    shutdown / restart    │
  │  • ClusterRole /    │  │  • status sync (30s)  │  │    suspend / resume      │
  │    ClusterRoleBinding│  │  • pollNodeOnline     │  │    remove/drain          │
  │  • Service          │  │  • creates            │  │  • VolumeMigration CRs   │
  │  • EndpointSlices   │  │    StorageNodeOps     │  │    (drain only)          │
  │  • TLS Certificates │  │    (action=remove)    │  │  • StorageNode.status    │
  │  • StorageNode CRs  │  │    on deletion        │  │    .activeOpsRef         │
  │    (create/delete)  │  │  • StorageNode.status │  │                          │
  │  • status aggregate │  │    (uuid, health...)  │  │ MUTUAL EXCLUSION         │
  │    (totalNodes,     │  │                       │  │  checks activeOpsRef     │
  │     online/offline) │  │ READS EFFECTIVE CFG   │  │  before starting;        │
  │  • nodeConfigs sync │  │  StorageNodeSet.spec  │  │  requeues if set         │
  │    → StorageNode    │  │  defaults merged with │  │                          │
  │    .spec.overrides  │  │  StorageNode.spec     │  │ ON COMPLETION            │
  │  • failureDomain    │  │  .overrides           │  │  Phase=Succeeded/Failed  │
  │    sync             │  │                       │  │  clears activeOpsRef     │
  └─────────────────────┘  └──────────────────────┘  └──────────────────────────┘
           │  creates/deletes            │                        │  owns
           ▼                            ▼                        ▼
      StorageNode CRs           backend API calls         VolumeMigration CRs
      (one per worker/socket)   /api/v2/clusters/...      (drain sub-phase only)
```

---

**Drain State Machine** — The five sequential sub-phases of the remove/drain operation, including the cluster pause guard, migration retry loop, and cancellation path.

```
  StorageNodeOps(action=remove) created
                            │
                            ▼
  ╔══════════════════════════════════════════════════════╗
  ║  ENTRY: SubPhase=Validating, Triggered=false         ║
  ╚══════════════════════════════════════════════════════╝
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────┐
  │  [1] VALIDATING                                               │
  │  • Fetch all volumes on the node from backend API             │
  │  • Identify pinned / unmanaged volumes (no matching PVC)      │
  │  ─────────────────────────────────────────────────────────────│
  │  EXIT → SUSPENDING  : no pinned or unmanaged volumes          │
  │  BLOCK (requeue 60s): pinned or unmanaged volumes present     │
  └───────────────────────────────────────────────────────────────┘
                            │
  ┌─────────────────────────▼──────────────────────────────────────────┐
  │              CLUSTER PAUSE GUARD  (all phases after Validating)    │
  │  if cluster.status != "active" OR rebalancing == true              │
  │  → emit DrainPaused event, requeue 60s                             │
  │  → auto-resumes when cluster is active and not rebalancing         │
  └─────────────────────────┬──────────────────────────────────────────┘
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────┐
  │  [2] SUSPENDING                                               │
  │  • GET node status; if already suspended → skip POST          │
  │  • POST /storage-nodes/{uuid}/suspend, set Triggered=true     │
  │  • Poll GET every 10s                                         │
  │  ─────────────────────────────────────────────────────────────│
  │  EXIT → MIGRATING  : node.status == "suspended"               │
  │  RETRY (10s)       : node not yet suspended                   │
  └───────────────────────────────────────────────────────────────┘
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────────────────┐
  │  [3] MIGRATING                                                            │
  │  • createMissingVolumeMigrations: one CR per PV-managed volume            │
  │    with round-robin target node assignment                                │
  │  • Track VolumesMigrated / VolumesPending                                │
  │  ┌────────────────────────────────────────────────────────────┐           │
  │  │  FAILURE RETRY LOOP                                        │           │
  │  │  VolumeMigration.phase == Failed/Aborted:                  │           │
  │  │    cluster not ready → delete CR, pause (DrainPaused)      │           │
  │  │    cluster ready     → delete CR, recreate with new target  │           │
  │  └────────────────────────────────────────────────────────────┘           │
  │  ─────────────────────────────────────────────────────────────────────────│
  │  EXIT → VERIFYING  : all migrations complete, no in-flight                │
  │  RETRY (15s)       : migrations still running                             │
  └───────────────────────────────────────────────────────────────────────────┘
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────┐
  │  [4] VERIFYING                                                │
  │  • GET all volumes remaining on node                          │
  │  • Delete system volumes (match systemVolumeFilterRegex)      │
  │  • Poll 30s until none remain                                 │
  │  ─────────────────────────────────────────────────────────────│
  │  EXIT → REMOVING   : no non-system volumes remain             │
  │  RETRY (30s)       : volumes still present                    │
  └───────────────────────────────────────────────────────────────┘
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────┐
  │  [5] REMOVING                                                 │
  │  • DELETE /api/v2/clusters/{uuid}/storage-nodes/{nodeUUID}    │
  │  ─────────────────────────────────────────────────────────────│
  │  200/204/404 → SUCCEEDED                                      │
  │  error       → FAILED (resume node best-effort)               │
  └───────────────────────────────────────────────────────────────┘
            │                              │
            ▼                             ▼
  ╔══════════════════════╗      ╔═══════════════════════════════════╗
  ║  SUCCEEDED           ║      ║  FAILED                           ║
  ║  Phase=Succeeded     ║      ║  POST /resume (best-effort)       ║
  ║  activeOpsRef=""     ║      ║  Phase=Failed, activeOpsRef=""    ║
  ╚══════════════════════╝      ╚═══════════════════════════════════╝

  CANCELLATION (any phase after Suspending):
    spec.action cleared → POST /resume if suspended
                        → delete all owned VolumeMigration CRs
                        → ActionStatus = nil
```

---

**Failure Domain Propagation** — How `enableFailureDomains` and `nodeFailureDomains` flow from StorageCluster and StorageNodeSet down to individual StorageNode specs and the backend API, including the blocking guard for missing entries.

```
  ┌─────────────────────────────────────────────────┐
  │  StorageCluster                                 │
  │    spec.enableFailureDomains: true              │
  └─────────────────────────────────────────────────┘
                    │  referenced by StorageNodeSet.spec.clusterName
                    ▼
  ┌─────────────────────────────────────────────────┐
  │  StorageNodeSet                                 │
  │    spec.nodeFailureDomains:                     │
  │      worker-A: 1   ← rack-1 / AZ-1             │
  │      worker-B: 2   ← rack-2 / AZ-2             │
  │      worker-C: 1   ← rack-1 / AZ-1             │
  │      worker-D: ?   ← MISSING                   │
  │                                                 │
  │  CEL rule: all values >= 1                      │
  └──────────┬──────────────┬──────────────┬────────┘
             │ sync         │ sync         │ MISSING
             ▼              ▼              ▼
  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐
  │ StorageNode  │  │ StorageNode  │  │ StorageNode  (worker-D)  │
  │ worker-A, s0 │  │ worker-B, s0 │  │                          │
  │ .failureDomain│  │ .failureDomain│  │ enableFailureDomains=T  │
  │  = 1         │  │  = 2         │  │ no entry in map          │
  └──────┬───────┘  └──────┬───────┘  │ → Warning event emitted  │
         │                 │          │ → node-add BLOCKED        │
         ▼                 ▼          │ → requeue until populated │
  ┌────────────────────────────────┐  └──────────────────────────┘
  │  Backend API POST              │
  │  { "failure_domain": 1 }       │
  │  { "failure_domain": 2 }       │
  └────────────────────────────────┘
```

---

**StorageNodeOps Lifecycle** — Full lifecycle from creation through mutual exclusion gate, action dispatch, sub-phases for `action=remove`, and terminal states with `activeOpsRef` cleanup.

```
  kubectl apply -f ops.yaml  (or auto-created on StorageNode deletion)
                    │
                    ▼
  ┌─────────────────────────────────────────────────────────────┐
  │  PENDING  — reconciler checks StorageNode.status.activeOpsRef│
  │                                                             │
  │  activeOpsRef == ""          activeOpsRef != ""             │
  │  → set activeOpsRef = self   → requeue (back-off)           │
  │  → phase = Running           (another op is running)        │
  └──────────────────────┬──────────────────────────────────────┘
                         │
                         ▼
  ╔═════════════════════════════════════════════════════╗
  ║  status.phase: Running                              ║
  ╚═════════════════════════════════════════════════════╝
                         │
          ┌──────────────┴───────────────────────────┐
          │  action dispatch                          │
          ▼                                           ▼
  ┌────────────────────────────┐   ┌──────────────────────────────────────┐
  │  shutdown / restart /      │   │  remove                              │
  │  suspend  / resume         │   │                                      │
  │                            │   │  Validating → Suspending             │
  │  POST action to backend    │   │      → Migrating                     │
  │  poll until terminal state │   │        (VolumeMigration CRs owned)   │
  │    suspend  → "suspended"  │   │      → Verifying                     │
  │    resume   → "online"     │   │      → Removing                      │
  │    restart  → "online"     │   │                                      │
  │    shutdown → "offline"    │   │  VolumesMigrated / VolumesPending    │
  └─────────────┬──────────────┘   │  tracked in status                   │
                │                  └──────────────────┬───────────────────┘
                │                                     │
                └──────────────────┬──────────────────┘
                                   │
                    ┌──────────────┴──────────────┐
                    ▼                             ▼
  ╔═══════════════════════════╗    ╔═══════════════════════════════╗
  ║  Succeeded                ║    ║  Failed                       ║
  ║  completedAt: <now>       ║    ║  message: <reason>            ║
  ╚═══════════════════════════╝    ║  completedAt: <now>           ║
                    │              ╚═══════════════════════════════╝
                    └──────────────────┬──────────────────────────┘
                                       │  both terminal states:
                                       ▼
                       StorageNode.status.activeOpsRef = ""
                       (next StorageNodeOps may now acquire the lock)
```
