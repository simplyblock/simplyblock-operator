# Test Plan: Synchronous Replication through csi-addons

Related design: [`designs/design-sync-replication-csi-addons.md`](../designs/design-sync-replication-csi-addons.md)

Scenario IDs are permanent. `Type` is one of `Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column holds the verbatim test function, or `—` when nothing covers the scenario yet, in which case the ID reappears in [What Is Not Yet Covered](#what-is-not-yet-covered). Parts of this design are implemented on the `sync-replication` branch (design [Implementation Status](../designs/design-sync-replication-csi-addons.md#implementation-status)): the method dispatch and the sync write verbs (§5, §7) and the operator provisioning (§9.4–§9.6) are built and carry the tests named below. The node path (§6), the sync status read (§5.3, §8), and most integration and end-to-end coverage are not yet built and stay `—`.

Class prefixes: `U-` unit (mock backend HTTP and fake Kubernetes objects, no cluster), `I-` integration (`envtest` plus the csi-addons sidecar plus a mock backend), `E-` end-to-end (a live two-site synchronous cluster, the switch driven by pod scheduling), `M-` manual (needs orchestration not yet automated). The group scenarios (`G-`, Phase 2) are a planned block and carry no `Type` until the work is scoped (design §14).

The control model is scheduling-driven: the node plugin reads the pod's site, signals it onto the `VolumeReplication`, the replication controller promotes there, and `NodeStageVolume` gates the connection on the promote (design §6). There is no RamenDR for sync. The per-RPC behavior (dispatch, verbs, status) is tested on its own, and the node-stage logic and the gate are tested as their own group, because the gate is where a wrong-site attach would happen and it is the property the design exists to guarantee.

---

## 1. Unit Tests

Drive each replication RPC handler against an `httptest` backend, and the node-stage logic against fake Kubernetes objects, asserting which route is called, with which query parameters, how the backend's status code maps to the gRPC code, and when the gate releases. No cluster, because the dispatch, the classification, the correlation, and the gate decision are pure logic.

### Method dispatch (§5)

File: `csi-driver/internal/csi/controller/replication_method_test.go`

| #    | Scenario                                                                  | Type     | Test              |
|------|---------------------------------------------------------------------------|----------|-------------------|
| U-01 | No `method` parameter: the RPC takes the async path unchanged             | Negative | TestResolveMethod |
| U-02 | `method: async` explicit: the async path                                  | Positive | TestResolveMethod |
| U-03 | `method: sync`: the sync route is called                                  | Positive | TestResolveMethod |
| U-04 | An unrecognized `method` value is rejected, not silently treated as async | Boundary | TestResolveMethod |

### Site resolution (§5.1, §9.2)

File: `csi-driver/internal/csi/controller/replication_method_test.go`

| #    | Scenario                                                          | Type     | Test                       |
|------|-------------------------------------------------------------------|----------|----------------------------|
| U-05 | `method: sync` with `site`: the value is passed as `?site=`       | Positive | TestSyncPromoteDispatch    |
| U-06 | `method: sync` with no `site`: `InvalidArgument`, no backend call | Negative | TestSyncPromoteMissingSite |

### Promote (§7.1, §12)

File: `csi-driver/internal/csi/controller/replication_sync_promote_test.go`

| #    | Scenario                                                                                                                       | Type     | Test                                          |
|------|--------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------|
| U-07 | Unforced `PromoteVolume` maps to `planned=true`                                                                                | Positive | TestSyncPromoteDispatch                       |
| U-08 | Forced `PromoteVolume` maps to `planned=false`                                                                                 | Positive | TestSyncPromoteDispatch                       |
| U-09 | `409` "in progress" returns a retryable code and does not escalate                                                             | Negative | TestSyncPromoteStatusProtocol                 |
| U-10 | `200` with a `SyncPromoteResultDTO` returns success                                                                            | Positive | TestSyncPromoteDispatch                       |
| U-11 | `412` peer-site-offline on an unforced promote makes the controller auto-escalate to a forced promote (`planned=false`) (§7.1) | Boundary | TestSyncPromoteStatusProtocol                 |
| U-12 | `409` gate refusal returns retryable and is not the escalation code                                                            | Boundary | TestSyncPromoteStatusProtocol                 |
| U-13 | `409` forced-while-peer-live returns retryable, force does not act on a live site                                              | Negative | TestSyncPromoteForcedWhilePeerLiveIsRetryable |

### Demote (§7.2)

File: `csi-driver/internal/csi/controller/replication_sync_demote_test.go`

| #    | Scenario                                                          | Type     | Test                                     |
|------|-------------------------------------------------------------------|----------|------------------------------------------|
| U-14 | `204` fenced on this site returns success                         | Positive | TestSyncDemoteDispatch                   |
| U-15 | `204` not-served-here (no-op) returns success, so a retry is safe | Boundary | TestSyncDemoteNotServedIsASuccessfulNoOp |
| U-16 | `409` gate refusal returns retryable                              | Negative | TestSyncDemoteGateRefusalIsRetryable     |
| U-17 | `500` ANA RPC failed returns retryable                            | Negative | TestSyncDemoteServerErrorIsRetryable     |

### No-op verbs (§7.3)

File: `csi-driver/internal/csi/controller/replication_sync_noop_test.go`

| #    | Scenario                                                                        | Type     | Test              |
|------|---------------------------------------------------------------------------------|----------|-------------------|
| U-18 | `EnableVolumeReplication` on a sync class returns success with no backend call  | Negative | TestSyncNoOpVerbs |
| U-19 | `DisableVolumeReplication` on a sync class returns success with no backend call | Negative | TestSyncNoOpVerbs |
| U-20 | `ResyncVolume` on a sync class returns success with no backend call             | Negative | TestSyncNoOpVerbs |

### Status mapping (§8)

File: `csi-driver/internal/csi/controller/replication_test.go`

`GetVolumeReplicationInfo` carries no class parameters, so it reads the status route regardless of method, and the backend fills it from the sync status on a sync cluster. The vendored csi-addons response carries only `lastSyncTime`, so that is the one field the driver maps; the `Completed`/`Degraded`/`Resyncing` conditions are the csi-addons controller's, not this read's, so `U-21`–`U-24` are out of the driver's scope and are not driver test rows.

| #    | Scenario                                                                                                         | Type     | Test                                                                      |
|------|------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------------------|
| U-25 | In-sync reports `lastSyncTime` as the backend's last-in-sync instant; a never-replicated volume reports none     | Boundary | TestGetVolumeReplicationInfo, TestGetVolumeReplicationInfoNeverReplicated |
| U-26 | `GetVolumeReplicationInfo` reads `replication/status` for a sync volume (no async relationship) without erroring | Positive | TestGetVolumeReplicationInfoServesASyncVolume                             |

### Node-stage site signaling and the gate (§6)

File: `csi-driver/internal/csi/node/stage_sync_test.go` and `syncwiring_test.go`, with the per-PVC correlation and class-by-site lookups in `atlas-lib/kube` (`volumereplication_test.go`, `volumereplicationclass_test.go`).

| #        | Scenario                                                                                                                                                                                                                                                                                            | Type       | Test                                                   |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|--------------------------------------------------------|
| U-27     | The node's site is read from its `topology.kubernetes.io/zone` label (§6.1)                                                                                                                                                                                                                         | Positive   | TestReadNodeSite                                       |
| U-28     | The volume's PVC identity (from the CSI volume context) correlates to the sync `VolumeReplication` that protects it (§6.2, §6.6)                                                                                                                                                                    | Positive   | TestInformerResolver_SyncVolumeReplicationForClaim     |
| ~~U-29~~ | ~~The class whose `site` parameter equals the node's site is selected~~ (superseded by U-42: one site-agnostic class, the site is a VR annotation, §9.1)                                                                                                                                            | Positive   | ~~TestInformerResolver_VolumeReplicationClassForSite~~ |
| U-30     | A volume the cache resolves to no sync `VolumeReplication` stages with no patch and no gate; an async-cluster volume is not looked up (§6.6)                                                                                                                                                        | Regression | TestDetectSyncReplication                              |
| ~~U-31~~ | ~~The patch is idempotent: a VR already at this site's class and `primary` is not re-patched; staging goes to the gate~~ (superseded by U-43: the VR is replaced, not patched)                                                                                                                      | Boundary   | ~~TestEnsureSyncPromoted~~                             |
| U-32     | The gate proceeds only when the VR reports `state: Primary` with `Completed` true (§6.4)                                                                                                                                                                                                            | Positive   | TestEnsureSyncPromoted                                 |
| U-33     | The gate holds with a retryable error while the promote is converging, and the re-stage re-checks (§6.4)                                                                                                                                                                                            | Boundary   | TestEnsureSyncPromoted                                 |
| U-34     | The gate never attaches on an incomplete promote: a promote the controller refuses or escalates stays non-`Completed`, so the gate keeps holding (the `409`/`412` protocol itself is U-09, U-11, U-12) (§6.4)                                                                                       | Negative   | TestEnsureSyncPromoted                                 |
| U-35     | On a sync cluster, a stage with no PVC identity in the volume context fails closed with `FailedPrecondition`: membership cannot be determined (§6.6)                                                                                                                                                | Negative   | TestDetectSyncReplication                              |
| ~~U-36~~ | ~~No sync class names the node's site: staging fails with `FailedPrecondition` naming the site~~ (superseded: one site-agnostic class, §9.1; a missing or non-sync class is U-30, stage as plain)                                                                                                   | Negative   | ~~TestEnsureSyncPromoted~~                             |
| ~~U-37~~ | ~~On `NodeUnstageVolume` a sync volume's site is demoted (best-effort, never blocking teardown)~~ (removed: the node no longer demotes on unstage — the new site's planned promote fences the old site, §6.5)                                                                                       | Positive   | ~~TestDemoteSyncOnUnstage~~                            |
| U-41     | On a sync cluster, a stage before the VR informer has synced fails closed with a retryable error, never attaching the volume as plain on a guess (§6.6)                                                                                                                                             | Boundary   | TestDetectSyncReplication                              |
| U-42     | The node signals the site by patching `metadata.annotations["simplyblock.io/sync-site"]` to its site and `spec.replicationState` to `primary`; the class is never changed (§6.2)                                                                                                                    | Positive   | TestPatchVolumeReplicationSite                         |
| U-43     | A VR already carrying this site and `primary` is not re-patched; staging goes straight to the gate (§6.2)                                                                                                                                                                                           | Boundary   | TestEnsureSyncPromoted                                 |
| U-44     | The site patch is idempotent across a crash: a stage interrupted after the patch re-patches (a no-op) and re-checks the gate; the `VolumeReplication` always exists, so the volume is never briefly unrecognizable as sync (§6.2, §6.7)                                                             | Regression | —                                                      |
| U-45     | Co-ownership: a re-apply of the `VolumeReplication` spec does not revert the node's `simplyblock.io/sync-site` annotation (server-side-apply field managers) (§6.2, §9.1)                                                                                                                           | Boundary   | —                                                      |
| U-46     | `PromoteVolume` and `DemoteVolume` read the target site from the `simplyblock.io/sync-site` annotation, correlating the volume to its `VolumeReplication`, not from a class parameter (§5.2, §6.3)                                                                                                  | Positive   | TestSyncPromoteDispatch                                |
| U-47     | A backend volume handle resolves to its `VolumeReplication` and the `simplyblock.io/sync-site` annotation value (the controller-side `atlas-lib` resolver: PV-by-handle to PVC to VR) (§5.2)                                                                                                        | Positive   | TestInformerResolver_VolumeReplicationForVolumeHandle  |
| U-48     | A `NodeUnstageVolume` whose NVMe-oF disconnect is abandoned at its deadline (`bounded.Error`; the disconnect itself is bounded in `atlas-lib/bounded`) still completes, releasing the single-writer attachment so the volume can attach elsewhere, rather than failing with `Internal` (§12)        | Boundary   | TestUnstageCompletesWhenTheDisconnectIsAbandoned       |
| U-49     | `NodeUnstageVolume` identifies the volume's namespace from the stack record when the stashed volume context is missing, so the teardown completes instead of erroring (§12)                                                                                                                         | Regression | TestTeardownConnectionNamesTheSubsystemFromTheRecord   |
| U-50     | A `NodeUnstageVolume` whose release fails for any reason other than a bounded-out abandonment still fails, so a slow or misconfigured path is not mistaken for a wedged one (§12)                                                                                                                   | Negative   | TestUnstageStillFailsOnAnOrdinaryDownError             |
| U-51     | A `NodeUnstageVolume` for a volume no longer staged at the target path (staging directory removed; no stash, no record, nothing the host can name) is a completed no-op per the CSI spec, not an `Internal` that wedges the external-attacher's detach and blocks the volume moving (§12)           | Regression | TestUnstageIsIdempotentWhenNothingIsStaged             |
| U-52     | A move forces the promote to re-run: when the volume is `Primary`/`Completed` but annotated for another site, the gate transitions `replicationState` `primary → secondary → primary` (waiting for the demote to be observed) rather than releasing on the stale, site-agnostic status (§6.3, §6.4) | Regression | TestEnsureSyncPromoted                                 |

### Misconfiguration and transport (§11, §12)

File: `csi-driver/internal/csi/controller/replication_sync_errors_test.go`

| #    | Scenario                                                                                                                                                                 | Type     | Test                                           |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------|
| U-38 | A sync write against a non-sync cluster (`400`) returns `InvalidArgument`; the backend returns 400 for both this and a bad site, so the driver does not distinguish them | Negative | TestSyncWriteOnNonSyncClusterIsInvalidArgument |
| U-39 | A backend transport error returns a retryable code                                                                                                                       | Negative | TestSyncPromoteTransportErrorIsRetryable       |
| U-40 | The node plugin cannot read the VolumeReplication (resolver error): staging holds with a retryable error                                                                 | Negative | TestDetectSyncReplication                      |

---

## 2. Integration Tests

Run the full staging path and the `VolumeReplication` reconcile against the kubernetes-csi-addons sidecar and a mock backend over `envtest`, proving the node-plugin-driven switch and the condition surface.

File: `csi-driver/e2e/` (csi-addons integration), mock backend in-process

| #    | Scenario                                                                                                                                                       | Type       | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| I-01 | A patched-to-primary sync `VolumeReplication` drives `Enable` then `Promote` and serves the volume on its site                                                 | Positive   | —    |
| I-02 | The promote poll advances across reconciles (`409` in progress to `200`)                                                                                       | Positive   | —    |
| I-03 | `Demote` on the secondary side fences the volume                                                                                                               | Positive   | —    |
| I-04 | The `VolumeReplication` conditions (`Completed`, `Degraded`, `Resyncing`) reflect the sync status and `lastSyncTime` is fresh while in sync                    | Positive   | —    |
| I-05 | An async-class `VolumeReplication` is unaffected by the sync dispatch and is never patched by the node plugin                                                  | Regression | —    |
| I-06 | A sync class missing `site` surfaces an error condition and a `SyncClassMisconfigured` event                                                                   | Negative   | —    |
| I-07 | `NodeStageVolume` reads the node site, patches the VR's `simplyblock.io/sync-site` annotation, waits out the promote poll, and attaches only after `Completed` | Positive   | —    |
| I-08 | A pod re-staged on the other site re-points the VR and moves leadership: the volume follows the pod                                                            | Positive   | —    |
| I-09 | `NodeStageVolume` holds (pod stays `ContainerCreating`) on a `409` gate refusal and on a `412`, with the matching event                                        | Negative   | —    |
| I-10 | The node `DaemonSet` service account can read node, PV, PVC, and classes and patch the VR, and is denied everything else (§4, RBAC)                            | Positive   | —    |

### Operator provisioning (§9.5, §9.6)

Run the `ClusterDeploymentConfig` expansion, the `StorageCluster` and `StorageNode` reconcilers, and the `spec.config` webhook against `envtest` with a mock control plane, asserting the create and add bodies and the admission rules.

File: `operator/internal/controller/*_unit_test.go` and the envtest suites

| #    | Scenario                                                                                                                                          | Type     | Test                                  |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------|
| I-11 | `ClusterTemplate.enableSyncReplication=true` expands to the `StorageCluster` toggle and sends `sync_replication: true` in the cluster-create body | Positive | —                                     |
| I-12 | `enableSyncReplication` is immutable: the webhook rejects a change on a live `StorageCluster`                                                     | Negative | —                                     |
| I-13 | A `NodeGroup.site` expands to `StorageNode.spec.config.site` and is sent as `site` in the node-add body                                           | Positive | —                                     |
| I-14 | On a sync cluster, a node with no `site` is rejected before add, mirroring the failure-domain check                                               | Negative | TestSitesAreCheckedAgainstTheTemplate |
| I-15 | `StorageNode.spec.config.site` is immutable: the `spec.config` webhook rejects a change once set                                                  | Negative | —                                     |

---

## 3. End-to-End Tests

A live two-site synchronous cluster, the switch driven by pod scheduling, with data verified on the target site after each move. The only class that proves the gate actually prevents a wrong-site attach and that the connection entries serve the volume.

File: live-cluster recipe (see M-01 for the orchestration this shares)

| #    | Scenario                                                                                                                                                                                               | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A pod scheduled to site A comes up with its volume promoted on A, data written and read back                                                                                                           | Positive | —    |
| E-02 | The pod is rescheduled to site B (node drain or affinity change): leadership follows, data verified on B                                                                                               | Positive | —    |
| E-03 | Disaster: site A taken down, the pod is rescheduled onto B, the stage's planned promote gets `412` and the controller auto-escalates to a forced promote with no human in the loop, data verified on B | Boundary | —    |
| E-04 | Recovery: site A returns and resyncs as secondary while the pod on B keeps serving, data on B unchanged throughout                                                                                     | Positive | —    |
| E-05 | The gate blocks the attach until the promote completes: a pod never comes up connected to a site the volume is not led from                                                                            | Boundary | —    |

---

## 4. Group Tests — Phase 2 (Planned)

The consistency-group variant (design §14). A planned block: the type of each scenario is decided when the group RPC handlers are scoped.

| #    | Scenario                                                                                                                                 | Type |
|------|------------------------------------------------------------------------------------------------------------------------------------------|------|
| G-01 | One pod mounting several members of a group patches the one `VolumeGroupReplication` and promotes the group as a unit on the node's site | —    |
| G-02 | A group demote fences members in order, and a retry is a no-op for those already fenced                                                  | —    |
| G-03 | The group status reads cluster-wide plus `member_count`, never a per-member sum                                                          | —    |
| G-04 | A group with an unresolvable or foreign member refuses the whole request before fencing anything                                         | —    |
| G-05 | Every member's stage gates on the one group promote, so no pod comes up with members split across sites                                  | —    |

---

## 5. Manual Scenarios

### M-01: a store shared across the site boundary blocks a switchover

**Design reference:** §12.

**What to verify:** a promote on the target site answers `409` naming the still-served volumes (`volumes`) when the store carries a volume outside the switching set that was never demoted, and succeeds once that volume is demoted too.

**Test concept:**
1. On a two-site sync cluster, create two volumes on one store, one whose pod is moving sites and one pinned to the source site.
2. Move the first volume's pod to the target site so its stage promotes there, while the second volume stays served on the source.
3. Confirm the promote answers `409` with the undemoted volume in `volumes`, and the moving pod holds in `ContainerCreating`.
4. Move or stop the second volume too, confirm the promote then reaches `200` and the pod comes up.

**Open question:** whether the operator should detect a shared store at provision time rather than at switchover (design §12 treats it as operational).

---

## Coverage Summary

| Class                 | Scenarios | Covered | Not covered |
|-----------------------|-----------|---------|-------------|
| Unit (`U-`)           | 44        | 42      | 2           |
| Integration (`I-`)    | 15        | 1       | 14          |
| End-to-end (`E-`)     | 5         | 0       | 5           |
| Group, Phase 2 (`G-`) | 5         | 0       | 5           |
| Manual (`M-`)         | 1         | 0       | 1           |
| **Total**             | **70**    | **43**  | **27**      |

The method dispatch and sync write verbs including the edge cases (`U-01`–`U-20`), the status read (`U-25`, `U-26`), the baseline node-stage path with the gate (`U-27`, `U-30`, `U-32`–`U-35`, `U-40`, `U-41`), the misconfiguration and transport cases (`U-38`, `U-39`), the mutable site-annotation signal and the driver reading the site from it (`U-42`, `U-43`, `U-46`, `U-47`, §6.2, §5.2, §6.3, superseding the struck patch-the-class path `U-29`, `U-31`, `U-36`), and the unstage-teardown robustness (`U-48`, `U-49`, `U-50`, `U-51`, §12) are covered. The unstage-time demote (`U-37`) is struck: the node no longer demotes the old site, so the switch rests on the new site's planned promote (§6.5). They span the controller package, the node package (`TestDetectSyncReplication`, `TestEnsureSyncPromoted`, `TestReadNodeSite`, `TestPatchVolumeReplicationSite`, `TestUnstageCompletesWhenTheDisconnectIsAbandoned`), and the `atlas-lib/kube` resolver tests (`TestInformerResolver_VolumeReplicationForVolumeHandle`). The two uncovered unit scenarios are the crash-idempotency of the site patch (`U-44`) and the server-side-apply co-ownership of the annotation (`U-45`, §6.2, §6.7, §9.1), which need an envtest harness against a real API server rather than a fake client. `I-14` is the sync-site provisioning check. See the [Implementation Status](../designs/design-sync-replication-csi-addons.md#implementation-status) in the design.

---

## What Is Not Yet Covered

The gaps are the `VolumeReplication` replacement at the unit tier, then the integration, end-to-end, group, and manual tiers, in the order they should land:

| #                      | Gap                                                                                         | Reason                                                                                                                                                                                                                                                                                                              |
|------------------------|---------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-44, U-45             | The site patch's crash-idempotency and the server-side-apply co-ownership of the annotation | Both need a real API server: crash-idempotency re-enters the patch across an interruption, and co-ownership asserts that distinct SSA field managers keep the node's annotation from being reverted by a GitOps re-apply of the spec (design §6.2, §6.7, §9.1). They land with the envtest harness (`I-01`–`I-10`). |
| I-01 through I-10      | The reconcile loop and the staging path against the sidecar, plus the node plugin RBAC      | The node path is built and unit-tested; these prove it against a live API server and the csi-addons sidecar (the informer sync, the real patch, the RBAC), which the units cannot. Needs an envtest harness.                                                                                                        |
| I-11, I-12, I-13, I-15 | The operator provisioning CR fields beyond the site check                                   | The cluster toggle's expansion and body, the node-site expansion, and the two immutability webhooks are wired but not yet each asserted (design §9.5, §9.6).                                                                                                                                                        |
| E-01 through E-05      | Pod placement and movement, disaster, recovery resync, and the gate's wrong-site guard      | Needs a two-site cluster; the only proof the gate prevents a wrong-site attach and the connection entries serve the volume.                                                                                                                                                                                         |
| G-01 through G-05      | The group variant scenarios                                                                 | The group dispatch is covered by `TestSyncGroupDispatch`; these scenarios (ordered demote, member resolution, split-site guard) are not yet each asserted (design §14).                                                                                                                                             |
| M-01                   | Shared-store blocking                                                                       | Needs a store deliberately shared across the site boundary.                                                                                                                                                                                                                                                         |
