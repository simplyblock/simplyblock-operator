# Test Plan: Synchronous Replication through csi-addons

Related design: [`designs/design-sync-replication-csi-addons.md`](../designs/design-sync-replication-csi-addons.md)

Scenario IDs are permanent. `Type` is one of `Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column holds the verbatim test function, or `—` when nothing covers the scenario yet, in which case the ID reappears in [What Is Not Yet Covered](#what-is-not-yet-covered). Nothing in this design is implemented, so every row is `—` today and this plan is the coverage the implementation must deliver.

Class prefixes: `U-` unit (mock backend HTTP, no cluster), `I-` integration (`envtest` plus the csi-addons sidecar plus a mock backend), `E-` end-to-end (a live two-site synchronous cluster through RamenDR), `M-` manual (needs orchestration not yet automated). The group scenarios (`G-`, Phase 2) are a planned block and carry no `Type` until the work is scoped (design §13).

---

## 1. Unit Tests

Drive each replication RPC handler against an `httptest` backend, asserting which route is called, with which query parameters, and how the backend's status code maps to the gRPC code. No Kubernetes and no cluster, because the dispatch and the classification are pure driver logic.

### Method dispatch (§5)

File: `csi-driver/internal/csi/controller/replication_method_test.go`

| #    | Scenario                                                                  | Type     | Test |
|------|---------------------------------------------------------------------------|----------|------|
| U-01 | No `method` parameter: the RPC takes the async path unchanged             | Negative | —    |
| U-02 | `method: async` explicit: the async path                                  | Positive | —    |
| U-03 | `method: sync`: the sync route is called                                  | Positive | —    |
| U-04 | An unrecognized `method` value is rejected, not silently treated as async | Boundary | —    |

### Site resolution (§5.1, §8.2)

File: `csi-driver/internal/csi/controller/replication_method_test.go`

| #    | Scenario                                                          | Type     | Test |
|------|-------------------------------------------------------------------|----------|------|
| U-05 | `method: sync` with `site`: the value is passed as `?site=`       | Positive | —    |
| U-06 | `method: sync` with no `site`: `InvalidArgument`, no backend call | Negative | —    |

### Promote (§6.1, §11)

File: `csi-driver/internal/csi/controller/replication_sync_promote_test.go`

| #    | Scenario                                                                                | Type     | Test |
|------|-----------------------------------------------------------------------------------------|----------|------|
| U-07 | Unforced `PromoteVolume` maps to `planned=true`                                         | Positive | —    |
| U-08 | Forced `PromoteVolume` maps to `planned=false`                                          | Positive | —    |
| U-09 | `409` "in progress" returns a retryable code and does not escalate                      | Negative | —    |
| U-10 | `200` with a `SyncPromoteResultDTO` returns success                                     | Positive | —    |
| U-11 | `412` peer-site-offline on an unforced promote returns the code csi-addons escalates on | Boundary | —    |
| U-12 | `409` gate refusal returns retryable and is not the escalation code                     | Boundary | —    |
| U-13 | `409` forced-while-peer-live returns retryable, force does not act on a live site       | Negative | —    |

### Demote (§6.2)

File: `csi-driver/internal/csi/controller/replication_sync_demote_test.go`

| #    | Scenario                                                          | Type     | Test |
|------|-------------------------------------------------------------------|----------|------|
| U-14 | `204` fenced on this site returns success                         | Positive | —    |
| U-15 | `204` not-served-here (no-op) returns success, so a retry is safe | Boundary | —    |
| U-16 | `409` gate refusal returns retryable                              | Negative | —    |
| U-17 | `500` ANA RPC failed returns retryable                            | Negative | —    |

### No-op verbs (§6.3)

File: `csi-driver/internal/csi/controller/replication_sync_noop_test.go`

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| U-18 | `EnableVolumeReplication` on a sync class returns success with no backend call  | Negative | —    |
| U-19 | `DisableVolumeReplication` on a sync class returns success with no backend call | Negative | —    |
| U-20 | `ResyncVolume` on a sync class returns success with no backend call             | Negative | —    |

### Status mapping (§7)

File: `csi-driver/internal/csi/controller/replication_sync_status_test.go`

| #    | Scenario                                                                                                                                                       | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-21 | Healthy sync status maps to `Completed` true, `Degraded` and `Resyncing` false                                                                                 | Positive | —    |
| U-22 | A running catch-up maps to `Resyncing` true, `Completed` false                                                                                                 | Positive | —    |
| U-23 | A behind or unknown zone maps to `Degraded` true, `Completed` false                                                                                            | Negative | —    |
| U-24 | `role` primary maps to `source`, secondary to `secondary`                                                                                                      | Positive | —    |
| U-25 | In-sync reports `lastSyncTime` as the read time, behind reports the real older instant; `lag_seconds`/`bytes_behind` map to `lastSyncDuration`/`lastSyncBytes` | Boundary | —    |
| U-26 | `GetVolumeReplicationInfo` reads `replication/status` regardless of method (§5.3)                                                                              | Positive | —    |

### Misconfiguration and transport (§10)

File: `csi-driver/internal/csi/controller/replication_sync_errors_test.go`

| #    | Scenario                                                                                              | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------|----------|------|
| U-27 | A sync class against a non-sync cluster (`400`) returns `FailedPrecondition` with the backend message | Negative | —    |
| U-28 | A backend transport error returns a retryable code                                                    | Negative | —    |

---

## 2. Integration Tests

Run the full `VolumeReplication` reconcile against the kubernetes-csi-addons sidecar and a mock backend over `envtest`, proving the RamenDR-issued RPC sequence and the condition surface.

File: `csi-driver/e2e/` (csi-addons integration), mock backend in-process

| #    | Scenario                                                                                                                                    | Type       | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| I-01 | `Enable` then `Promote` on a sync class serves the volume on this site                                                                      | Positive   | —    |
| I-02 | The promote poll advances across reconciles (`409` in progress to `200`)                                                                    | Positive   | —    |
| I-03 | `Demote` on the secondary side fences the volume                                                                                            | Positive   | —    |
| I-04 | The `VolumeReplication` conditions (`Completed`, `Degraded`, `Resyncing`) reflect the sync status and `lastSyncTime` is fresh while in sync | Positive   | —    |
| I-05 | An async-class `VolumeReplication` is unaffected by the sync dispatch                                                                       | Regression | —    |
| I-06 | A sync class missing `site` surfaces an error condition and a `SyncClassMisconfigured` event                                                | Negative   | —    |

### Operator provisioning (§8.5, §8.6)

Run the `ClusterDeploymentConfig` expansion, the `StorageCluster` and `StorageNode` reconcilers, and the `spec.config` webhook against `envtest` with a mock control plane, asserting the create and add bodies and the admission rules.

File: `operator/internal/controller/*_unit_test.go` and the envtest suites

| #    | Scenario                                                                                                                                          | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-07 | `ClusterTemplate.enableSyncReplication=true` expands to the `StorageCluster` toggle and sends `sync_replication: true` in the cluster-create body | Positive | —    |
| I-08 | `enableSyncReplication` is immutable: the webhook rejects a change on a live `StorageCluster`                                                     | Negative | —    |
| I-09 | A `NodeGroup.site` expands to `StorageNode.spec.config.site` and is sent as `site` in the node-add body                                           | Positive | —    |
| I-10 | On a sync cluster, a node with no `site` is rejected before add, mirroring the failure-domain check                                               | Negative | —    |
| I-11 | `StorageNode.spec.config.site` is immutable: the `spec.config` webhook rejects a change once set                                                  | Negative | —    |

---

## 3. End-to-End Tests

A live two-site synchronous cluster, driven through RamenDR, with data verified on the target site after each switchover. The only class that proves the `412` escalation and that the connection entries actually serve the volume.

File: live-cluster recipe (see M-01 for the orchestration this shares)

| #    | Scenario                                                                                        | Type     | Test |
|------|-------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Planned switchover A to B (both sites up, in sync), data verified on B                          | Positive | —    |
| E-02 | Disaster fail-over (A lost): `412` escalates to a forced promote, data verified on B            | Boundary | —    |
| E-03 | Fail-back B to A once the status is healthy, data verified on A                                 | Positive | —    |
| E-04 | The connection entries from the promote and `GET V/connect` serve the volume on the target site | Positive | —    |

---

## 4. Group Tests — Phase 2 (Planned)

The consistency-group variants (design §13). A planned block: the type of each scenario is decided when the group RPC handlers are scoped.

| #    | Scenario                                                                                         |
|------|--------------------------------------------------------------------------------------------------|
| G-01 | A group promote serves every member on the target site as one unit                               |
| G-02 | A group demote fences members in order, and a retry is a no-op for those already fenced          |
| G-03 | The group status reads cluster-wide plus `member_count`, never a per-member sum                  |
| G-04 | A group with an unresolvable or foreign member refuses the whole request before fencing anything |
| G-05 | An empty group answers promote `200` with no members and demote `204`                            |

---

## 5. Manual Scenarios

### M-01: a store shared across the DR boundary blocks a switchover

**Design reference:** §11.

**What to verify:** a planned promote on the target site answers `409` naming the still-served volumes (`volumes`) when the store carries a volume outside the DR set that was never demoted, and succeeds once that volume is demoted too.

**Test concept:**
1. On a two-site sync cluster, create two volumes on one store, one managed by a `VolumeReplication` in the DR set and one not.
2. Demote only the DR-set volume on the source site, then promote it on the target.
3. Confirm the promote answers `409` with the undemoted volume in `volumes`.
4. Demote the second volume, retry the promote, confirm `200`.

**Open question:** whether the operator should detect a shared store at provision time rather than at switchover (design §11 treats it as operational).

---

## Coverage Summary

| Class                 | Scenarios | Covered | Not covered |
|-----------------------|-----------|---------|-------------|
| Unit (`U-`)           | 28        | 0       | 28          |
| Integration (`I-`)    | 11        | 0       | 11          |
| End-to-end (`E-`)     | 4         | 0       | 4           |
| Group, Phase 2 (`G-`) | 5         | 0       | 5           |
| Manual (`M-`)         | 1         | 0       | 1           |
| **Total**             | **49**    | **0**   | **49**      |

---

## What Is Not Yet Covered

Nothing is implemented: this plan defines the coverage the implementation delivers. The gaps that matter most, in the order they should land:

| #                 | Gap                                                                                                    | Reason                                                                                                                                                |
|-------------------|--------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-09, U-11, U-12  | The `409` against `412` protocol: retryable gate refusal, the one escalation trigger, in-progress poll | The highest risk (design §11, §14). A `409` miscoded as escalation drives a force loop; a `412` miscoded as a plain retry hangs a disaster fail-over. |
| U-01 through U-08 | Method dispatch, site resolution, and `force` to `planned` inversion                                   | The feature does nothing correctly until the dispatch and the inversion are right.                                                                    |
| U-14 through U-28 | Demote, the no-op verbs, status mapping, and misconfiguration                                          | The rest of the per-RPC behavior.                                                                                                                     |
| I-01 through I-06 | The reconcile loop against the sidecar                                                                 | Proves the RamenDR RPC sequence and the condition surface, which the unit tests cannot.                                                               |
| I-07 through I-11 | The operator provisioning CR fields                                                                    | The cluster toggle and node site must reach the backend create and add bodies and be enforced by admission (design §8.5, §8.6).                       |
| E-01 through E-04 | Planned switchover, disaster escalation, fail-back, real data path                                     | Needs a two-site cluster; the only proof the connection entries serve the volume and the escalation works end to end.                                 |
| G-01 through G-05 | The group variants                                                                                     | Phase 2; testable once the group RPC handlers land (design §13).                                                                                      |
| M-01              | Shared-store blocking                                                                                  | Needs a store deliberately shared across the DR boundary.                                                                                             |
