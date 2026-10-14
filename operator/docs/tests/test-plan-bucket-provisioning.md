Related design: [`designs/design-bucket-provisioning.md`](../designs/design-bucket-provisioning.md)
Harness: `operator/internal/controllers/objectstore` (W-02, W-03, W-08). A new nested controller package rather than the legacy flat `internal/controller` this plan's file-path citations originally named: the repo's newer controller groups (backup, cluster, deployment, volume) had already moved to a `TestMain` + shared `envtest.Environment` + plain `testing.T` convention, and this package follows that rather than the legacy Ginkgo-based one.

Scope: the new bucket-provisioning reconciler and its `S3Bucket` CRD. The CSI driver's pNFS path and the `NFSExportReconciler` are dependencies, faked at the boundary for unit and integration tests: their own behavior is `test-plan-pnfs-rwx.md`'s scope, not this one's.

Scenario IDs are permanent: `U-` unit (no cluster, pure functions, fake `client.Client`, mock HTTP), `I-` integration (full reconcile loop against `envtest` plus a mock backend), `E-` end-to-end (live cluster, real data path), `M-` manual (needs failure injection or orchestration not yet automated). Types are `Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column names the implementing function, or `—` when the scenario is not yet covered, and every `—` also appears in §8 What Is Not Yet Covered.

W-03 landed the phase machine (excluding bucket delivery, W-12's own scope): U-01 through U-10, U-17, U-18, and I-01 through I-06 are covered below. U-11 through U-16 (bucket-delivery node-set diffing) and I-07 through I-10 (delivery reconciliation) stay `—`, W-12's own work. I-11 (RBAC) also stays `—`: proving the manager's `ClusterRole` is sufficient needs a constrained client built from `config/rbac/role.yaml`, not yet set up in this harness.

---

## 1. Unit Tests

All pure functions and controller helper methods are covered without a cluster: the phase-transition table and the bucket-delivery node-set diff.

### Phase Transitions (§6)

File: `operator/internal/controllers/objectstore/s3bucket_phase_unit_test.go`

| #    | Scenario                                                                                                                               | Type     | Test                                                                                                                                            |
|------|----------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 | New `S3Bucket`, no PVC yet → `Pending`                                                                                                 | Positive | `TestNewBucketStartsPending`                                                                                                                    |
| U-02 | PVC created, not yet Bound → stays `Pending`                                                                                           | Positive | `TestClaimNotYetBoundStaysPending`                                                                                                              |
| U-03 | PVC Bound, no matching `NFSExport` found yet → `Provisioning`                                                                          | Positive | `TestBoundClaimEntersProvisioning`                                                                                                              |
| U-04 | PVC Bound, `NFSExport` Ready, `DeliverBucket` not yet confirmed on every node → `Provisioning`                                         | Positive | `TestExportNotYetReadyStaysProvisioning` (tests the export-not-Ready sub-case; the mount-delivery sub-case is W-12's own, see the note above)   |
| U-05 | PVC Bound, `NFSExport` Ready, `DeliverBucket` confirmed on every node → `Ready`                                                        | Positive | `TestReadyExportCompletesProvisioning` (mount delivery is a stub returning true until W-12; see design note on `mountDeliveryComplete`)         |
| U-06 | `NFSExport` reaches `Degraded` while `Provisioning` → `Degraded`, `ExportReady` condition `False`                                      | Negative | `TestDegradedExportDegradesTheBucket`                                                                                                           |
| U-07 | A node's `csi-link` session is down past the deadline, or its `DeliverBucket` call fails → `Degraded`, `MountDelivered` `False`        | Negative | `TestProvisioningDeadlinePassedDegrades` (the generic deadline path; the specific csi-link-down trigger is W-12's own, once it drives delivery) |
| U-08 | Terminal-state re-reconcile: `Ready` with nothing changed → no writes, no event                                                        | Positive | `TestReadyBucketReconcilesToNoWrites`                                                                                                           |
| U-09 | Deletion requested while `Pending` (no PVC ever created) → finalizer removed immediately, no orphaned PVC delete attempted             | Boundary | `TestDeletePendingBucketRemovesFinalizerWithNoPVCAttempt`                                                                                       |
| U-10 | Deletion requested while `Ready` → `WithdrawBucket` called on every delivered node, PVC deleted, then finalizer removed, in that order | Positive | `TestDeleteReadyBucketWithdrawsThenDeletesThenReleasesFinalizer`                                                                                |

### Bucket-Delivery Node-Set Diffing (§5.3)

File: `operator/internal/controllers/objectstore/s3bucket_delivery_unit_test.go` (not yet created; W-12)

| #    | Function            | Scenario                                                                                                               | Type     | Test |
|------|---------------------|------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-11 | `diffDeliveryNodes` | Gateway Deployment has 3 pods on 3 distinct nodes, nothing delivered yet → 3 `DeliverBucket` calls, 0 `WithdrawBucket` | Positive | —    |
| U-12 | `diffDeliveryNodes` | A gateway pod is rescheduled from node A to node B → `DeliverBucket` on B, `WithdrawBucket` on A                       | Positive | —    |
| U-13 | `diffDeliveryNodes` | Two gateway pods land on the same node → exactly one `DeliverBucket` call for that node, not two                       | Boundary | —    |
| U-14 | `diffDeliveryNodes` | Gateway Deployment scaled to zero pods → `WithdrawBucket` on every previously delivered node, none newly delivered     | Boundary | —    |
| U-15 | `diffDeliveryNodes` | No gateway pods exist yet (`ObjectStore` just created) → no calls, no error                                            | Boundary | —    |
| U-16 | `diffDeliveryNodes` | Node set unchanged between two reconciles → empty diff, no RPC calls                                                   | Positive | —    |

### Deterministic Naming (§4.1)

File: `operator/internal/controllers/objectstore/s3bucket_naming_unit_test.go`

| #    | Function     | Scenario                                                                                                                     | Type     | Test                                              |
|------|--------------|------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------|
| U-17 | `pvcNameFor` | Bucket name at the 63-character DNS label limit → valid, collision-free PVC name                                             | Boundary | `TestPVCNameForBucketAtTheLengthLimit`            |
| U-18 | `pvcNameFor` | Two buckets with names differing only in a character the S3 grammar allows but DNS labels do not → distinct, valid PVC names | Negative | `TestPVCNameForDistinguishesNamesDNSLabelsCannot` |

~~U-19 `mountHolderNameFor`~~ **Moved.** The deterministic `target_path` naming this scenario covered is now `DeliverBucket`'s own concern inside `csi-driver` (`W-11`), not this reconciler's. A replacement scenario belongs in `csi-driver`'s own test suite, not here.

---

## 2. Integration Tests

Run the full controller reconciliation loop against a real Kubernetes API via `envtest`, with the CSI driver, `NFSExportReconciler`, and a fake `PeerRegistry` faked at the boundary: a test double marks a created `PersistentVolumeClaim` `Bound`, creates a matching `NFSExport` already in `Ready`, and acknowledges a `DeliverBucket`/`WithdrawBucket` call, since the real CSI driver, MDS guest, and a real `csi-link` session all need a live cluster (§3 E2E).

### Bucket Creation (§6, §7)

| #    | Scenario                                                                                                                                                                    | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `S3Bucket` created → PVC created with the spec's `storageClassName` and `capacityBytes`                                                                                     | Positive | —    |
| I-02 | PVC bound, matching `NFSExport` fabricated as `Ready` → `S3Bucket` reaches `Ready` once `DeliverBucket` is confirmed on every node                                          | Positive | —    |
| I-03 | `S3Bucket.spec.storageClassName` does not exist → PVC never binds, event `PersistentVolumeClaimNotBound`, `S3Bucket` stays `Pending`                                        | Negative | —    |
| I-04 | No `NFSExport` ever appears referencing the bound PV (faked CSI controller does not create one) → `Degraded`, event `NoMatchingExport`                                      | Negative | —    |
| I-05 | `spec.objectStoreRef` names an `ObjectStore` that does not exist → `Degraded`, event `ObjectStoreNotFound`                                                                  | Negative | —    |
| I-06 | Operator process restarted mid-`Provisioning` (envtest manager stopped and restarted) → resumes from `status` without re-issuing `DeliverBucket` on already-confirmed nodes | Positive | —    |

### Bucket Delivery Reconciliation (§5.3, §7.3)

| #    | Scenario                                                                                                                                                  | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-07 | Gateway Deployment's pod rescheduled to a new node mid-`Ready` → `DeliverBucket` called on the new node within one reconcile, `WithdrawBucket` on the old | Positive | —    |
| I-08 | Two `S3Bucket` reconciles triggered concurrently for the same object (two events in quick succession) → converge to one delivery set, no duplicate calls  | Positive | —    |
| I-09 | A node's reported delivery state drifts from `status` out-of-band (simulated via the fake `PeerRegistry`) → reconciled on the next pass                   | Negative | —    |
| I-10 | `ObjectStore`'s gateway Deployment deleted while an `S3Bucket` is `Ready` → existing deliveries left in place, not withdrawn (§6 failure table)           | Negative | —    |

### RBAC (§7.5)

| #    | Scenario                                                                                                                                                                            | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-11 | The main operator's `ClusterRole`, carrying the §7.5 rules this reconciler adds alongside its existing ones, performs a full create-to-`Ready` cycle without a forbidden-verb error | Positive | —    |

---

## 3. E2E Tests

Run against a live simplyblock cluster with the pNFS path actually deployed (`design-pnfs-rwx.md`, `design-pnfs-mds-vm.md`, Phase 0 P0-1). `0004-direct-csi-mount-spike-result.md` already settled Phase 0 P0-2's raw mechanism. This class proves the production `DeliverBucket`/`WithdrawBucket` path built on top of it, which unit and integration tests fake at exactly this boundary.

### `DeliverBucket`/`WithdrawBucket` End to End (§5.4)

| #    | Scenario                                                                                                                                                                                                                                                                            | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | One gateway pod already running, one bucket created afterward → `DeliverBucket` is called on its node and the bucket's mount appears under the gateway pod's `hostPath` mount without the pod being restarted                                                                       | Positive | —    |
| E-02 | Same as E-01, but the gateway Deployment has 3 replicas across 3 nodes → `DeliverBucket` is called on every node independently and the mount appears on each                                                                                                                        | Positive | —    |
| E-03 | A write made through the `DeliverBucket`-staged mount directly (simulating what the gateway's `PutObject` would do) is visible read-back through the gateway pod's propagated view, with correct bytes                                                                              | Positive | —    |
| E-04 | Gateway pod rescheduled to a node with no existing delivery for a bucket it already serves → `DeliverBucket` called for that node, mount appears before the pod is considered ready to serve that bucket (or: a defined, observable window during which it does not, if one exists) | Positive | —    |

### Bucket Lifecycle Timing (§10, Open Question 4)

| #    | Scenario                                                                                                                                                | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-05 | First bucket ever created on a storage cluster with no MDS pod yet → full `Pending`→`Ready` latency measured, dominated by MDS guest boot               | Positive | —    |
| E-06 | Second and later buckets on the same storage cluster (MDS already running) → `Pending`→`Ready` latency measured, expected substantially lower than E-05 | Positive | —    |
| E-07 | `simplyblock_objectstore_bucket_provisioning_seconds` histogram records both E-05 and E-06 with plausible, distinguishable values                       | Positive | —    |

### Failure and Recovery

| #    | Scenario                                                                                                           | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------|----------|------|
| E-08 | Operator pod killed mid-`Provisioning` on a live cluster → resumes and reaches `Ready` without manual intervention | Negative | —    |

~~E-09~~ **Obsolete.** "A node hosting a mount-holder pod is drained → mount-holder reschedules" no longer applies: there is no mount-holder pod to drain. The CSI node plugin's own `DaemonSet` pod is the component to consider instead, and `kubectl drain` skips `DaemonSet`-managed pods by default, so this scenario has no direct replacement. A scenario for the CSI node plugin's own pod restart already exists as U-07/I-09/I-10 (the `csi-link` session going down, and the drift it can leave behind).

---

## 4. Manual Scenarios and Test Concepts

### M-01 — ~~Mount propagation needs `Bidirectional`, not `HostToContainer`~~ Resolved

**Design reference:** §5.4

**What was verified:** `HostToContainer` propagation on the gateway pod's `hostPath` volume is sufficient on its own. No `Bidirectional` mount is needed anywhere, on any pod: the bind mount originates inside the CSI node plugin's own container, which already carries `Bidirectional` on the specific host paths its `DaemonSet` mounts, and that is what makes it visible to the host and to `HostToContainer` pods.

**Confirmed behavior:** `0004-direct-csi-mount-spike-result.md`. A direct `NodeStageVolume`/`NodePublishVolume` call, with `target_path` under the node plugin's own `hostPath`, appeared live inside an already-running pod whose only configuration was `HostToContainer`.

**Historical test concept** (as run during the spike):
1. Deploy a pod with the `hostPath` + `HostToContainer` volume as designed, no `Bidirectional` anywhere.
2. Call `NodeStageVolume`/`NodePublishVolume` directly against the CSI node plugin's socket, `target_path` under that same shared host path.
3. Exec into the `HostToContainer` pod and confirm the bucket's directory is visible and populated.

### M-02 — ~~Where kubelet actually stages the mount, relative to the shared host path~~ Resolved

**Design reference:** §5.1, §5.4

**What was verified:** the shared `hostPath` cannot be chosen independently of the CSI node plugin's own `DaemonSet` manifest. It must be a subdirectory of one of the three paths that manifest already mounts `Bidirectional` (`/var/lib/kubelet/plugins/…`, `/var/lib/kubelet/pods/…`, `/var/lib/nvme/…`). A path outside those three stays inside the node plugin's own container and reaches neither the host nor any other pod.

**Confirmed behavior:** `0004-direct-csi-mount-spike-result.md`, tested directly: an arbitrary path outside the three produced a mount visible only inside the node plugin's own container, confirmed by `exec`.

**Historical test concept** (as run during the spike):
1. Call `NodeStageVolume`/`NodePublishVolume` with `target_path` at an arbitrary host path, not under any of the node plugin's `Bidirectional` mounts.
2. Confirm, from inside the node plugin's own container and from a separate `HostToContainer` pod on the same host path, that the mount is visible only in the former.
3. Repeat with `target_path` under one of the three documented subtrees and confirm it is now visible in both.

---

## 5. Axis Coverage

| Axis                  | Values covered                         | IDs              | Not covered                                                                                                                                                                                   |
|-----------------------|----------------------------------------|------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Bucket count          | 0, 1, many (per-node fan-out)          | U-15, I-01, E-02 | hundreds-to-thousands of buckets on one `ObjectStore` (no longer a scale concern per node, since delivery is one RPC per node rather than one Pod per bucket, though still untested at scale) |
| Gateway replica count | 1, 3                                   | E-01, E-02       | large replica counts (10+)                                                                                                                                                                    |
| Node topology         | single node, multiple nodes            | U-13, E-02       | heterogeneous node pools (some nodes never run gateway pods)                                                                                                                                  |
| Storage cluster state | fresh (no MDS yet), warm (MDS running) | E-05, E-06       | MDS mid-failover during bucket creation (`design-pnfs-rwx.md` §13 interaction)                                                                                                                |
| Lifecycle / restart   | operator restart mid-provisioning      | I-06, E-08       | operator restart mid-deletion, CSI node plugin's own pod restart mid-delivery (Open Question 6)                                                                                               |
| Concurrency           | concurrent reconciles of one object    | I-08             | concurrent creation of many distinct `S3Bucket`s under load                                                                                                                                   |

---

## 6. Coverage Summary

| Class       | Scenarios                                   | Covered | Not covered |
|-------------|---------------------------------------------|---------|-------------|
| Unit        | 18 (U-19 moved to `csi-driver`'s own suite) | 0       | U-01 … U-18 |
| Integration | 11                                          | 0       | I-01 … I-11 |
| E2E         | 8 (E-09 obsolete)                           | 0       | E-01 … E-08 |
| Manual      | 2 (both resolved)                           | 2       | —           |

---

## 7. What Is Not Yet Covered

| #           | Gap                                                                                             | Reason                                                                                                                                                                              |
|-------------|-------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-18 | All unit scenarios                                                                              | Reconciler not implemented yet                                                                                                                                                      |
| I-01 … I-11 | All integration scenarios                                                                       | Reconciler not implemented yet                                                                                                                                                      |
| E-01 … E-08 | All E2E scenarios                                                                               | Reconciler not implemented yet. E-01 through E-04 additionally need a cluster with the pNFS path deployed and `DeliverBucket`/`WithdrawBucket` implemented (`W-10`, `W-11`, `W-12`) |
| —           | Concurrent creation of many distinct `S3Bucket`s under sustained load                           | Not scoped. No performance target exists yet for bucket-creation throughput                                                                                                         |
| —           | MDS failover (`design-pnfs-rwx.md` §13) occurring concurrently with a bucket's own provisioning | Cross-design interaction, not assessed by either design as written                                                                                                                  |
| —           | CSI node plugin's own pod restart mid-delivery (stack-record drift, design §12 Open Question 6) | Not designed yet. No reconciliation-against-drift mechanism exists to test                                                                                                          |
