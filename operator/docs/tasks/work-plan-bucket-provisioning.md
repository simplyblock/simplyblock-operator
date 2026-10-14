# Work Plan: S3 Bucket Provisioning

Related design: [`designs/design-bucket-provisioning.md`](../designs/design-bucket-provisioning.md)
Test plan: [`tests/test-plan-bucket-provisioning.md`](../tests/test-plan-bucket-provisioning.md)

**Author:** Max Schettler
**Date:** 2026-10-12 (last updated 2026-10-14)
**Design status at split:** Draft — the section citations may be stale. Open Questions 1, 2, and 7 from the original split are all resolved (`0004-direct-csi-mount-spike-result.md`, `design-objectstore.md`, `0005-s3bucket-joins-the-main-operator-manager.md`). No blocking open questions remain in the sense §0 of the `work-plan` skill describes.
**Issues:** not yet filed

Work item IDs (`W-01`) are permanent and are cited from issue bodies and commit messages, so they are never renumbered. Each item below is one GitHub issue: the description and acceptance criteria are the issue body verbatim.

---

## 1. Execution Order

<!-- Generated. Run `.claude/skills/work-plan/scripts/check-work-plan.py
     <this file> --write` to refresh this section, and never edit the
     three tables by hand. -->

### Waves

| Wave | Items                        | Runs after |
|------|------------------------------|------------|
| 1    | W-01, W-02, W-04, W-07, W-10 | —          |
| 2    | W-08, W-11                   | wave 1     |
| 3    | W-03                         | wave 2     |
| 4    | W-12                         | wave 3     |
| 5    | W-06, W-09                   | wave 4     |

### Critical path

W-02 → W-08 → W-03 → W-09 (4 items, weight 7)

### Serialization notes

| Items      | Reason                                                                                                                                                                |
|------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| W-03, W-12 | both register reconcilers and RBAC rules into the existing manager's `ClusterRole`, and both extend `s3bucket_types.go`'s status handling; land one, rebase the other |

---

## 2. External Prerequisites

| Prerequisite                                                                                                      | Blocks                                                                                                                                                       | Design               |
|-------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------|
| A cluster with the pNFS RWX path (`NFSExport` CRD, MDS pod, `fstype: pnfs` StorageClass) actually deployed        | W-09                                                                                                                                                         | design Phase 0, P0-1 |
| The `ObjectStore` CRD (gateway Deployment, Service, TLS), designed (`design-objectstore.md`), not yet implemented | W-12 (node-set discovery reads the gateway Deployment's pods by `design-objectstore.md`'s fixed label, not a field, but still needs the Deployment to exist) | design Phase 0, P0-3 |
| The S3 gateway's `CreateBucket` handler, a separate repository                                                    | none in this plan — this plan's items are reachable and testable by any authorized Kubernetes client, per design §12 Open Question 3                         | design Phase 0, P0-4 |

An item blocked only on an external prerequisite is still filed. It carries the `blocked` label and names the prerequisite in its body.

---

## 3. Work Items

### W-01 — ~~Verify mount propagation into a gateway pod~~ Resolved

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, critical
**Design:** §5.4, Phase 0 P0-2
**Scenarios:** M-01, M-02, E-01, E-02, E-03
**Size:** M

Resolved by a live-cluster spike, recorded in `0004-direct-csi-mount-spike-result.md`: `HostToContainer` propagation alone is sufficient, no `Bidirectional` mount is needed anywhere, and the delivery path must be a subdirectory of one of the three paths the CSI node plugin's own `DaemonSet` already mounts `Bidirectional`. Design §5.4 and §12 Open Question 1 were updated to reflect this. No further work item needed for the finding itself; W-10/W-11/W-12 build the production mechanism on top of it.

**Acceptance criteria**

- [x] A live-cluster test confirmed the mount appears without `Bidirectional` anywhere, constrained to the node plugin's own `hostPath` subtrees
- [x] M-01 and M-02 are answered with evidence, not reasoning
- [x] Design §5.4 and §12 Open Question 1 are updated to reflect the confirmed mechanism

### W-02 — Add the `S3Bucket` CRD

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §4, Appendix A
**Scenarios:** —
**Size:** S

Add `S3Bucket`, `S3BucketSpec`, `S3BucketStatus`, `S3BucketPhase`, and `S3BucketFinalizer` to a new `objectstore.simplyblock.io/v1alpha1` API group, exactly as specified in design Appendix A. This is a new API group, not an addition to `storage.simplyblock.io`, keeping this feature's API versioning independent of the existing group's (design §7.1). No reconciler logic in this item: types, kubebuilder markers, and generated code only.

**Acceptance criteria**

- [x] `s3bucket_types.go` matches design Appendix A field for field
- [x] `make -C operator manifests generate` and `make helm-sync` run clean, and their diffs are committed with this item (this is the first CRD type outside `storage.simplyblock.io`, so the new API group also needed `operator/api/objectstore/v1alpha1/` scaffolding, a `config/crd/kustomization.yaml` entry, a scheme registration in `cmd/main.go`, and a second glob in `helm-charts/scripts/sync-from-operator.sh`, which only ever matched `storage.simplyblock.io_*.yaml`)
- [x] `.claude/skills/api-design/scripts/check-crds.py --kind S3Bucket` reports no error (the checker's own `API_DIRS` list needed the new group added too, for the same reason)

### W-03 — Reconcile `S3Bucket` through its PVC and `NFSExport` phases

**Depends on:** W-02, W-08
**Conflicts with:** W-12
**Labels:** operator, enhancement, high
**Design:** §6, §7.2, §7.3, §7.4, §9
**Scenarios:** U-01, U-02, U-03, U-04, U-05, U-06, U-07, U-08, U-09, U-10, I-01, I-02, I-03, I-04, I-05, I-06, I-11
**Size:** M

Implement the `S3Bucket` reconciler's phase machine, excluding bucket delivery (W-12): create the backing `PersistentVolumeClaim` idempotently from `spec`, watch it to `Bound`, find the `NFSExport` whose `spec.volumeRef` matches the bound `PersistentVolume`'s CSI volume handle, and track `Pending` → `Provisioning` → `Ready`/`Degraded` per design §6's table. Apply the `S3BucketFinalizer`, and implement deletion (prune, release the PVC, remove the finalizer) per the same section. Register the controller into the existing `kubernetes/operator` manager, and add its RBAC rules (design §7.5, except the `pods`/`deployments.apps` rules W-12 needs) to that manager's existing `ClusterRole`.

**Acceptance criteria**

- [ ] U-01 through U-10 pass
- [ ] I-01 through I-06 and I-11 pass against the `envtest` harness and CSI/`NFSExport` test double W-08 provides
- [ ] An operator restart mid-`Provisioning` resumes from `status` without re-creating the PVC (I-06)
- [ ] The design §9 failure table's first four rows each produce the named event and phase
- [ ] The existing manager's `ClusterRole` diff is committed with this item, scoped to exactly the resources design §7.5 lists for `S3BucketReconciler`

### W-04 — ~~Reconcile mount-holder pods to the gateway's node set~~ Superseded

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §5.1, §5.2, §5.3 (pre-revision)
**Scenarios:** —
**Size:** M

Superseded by W-10, W-11, and W-12. The mount-holder-pod mechanism this item described is rejected in the current design (§5.4, `0004-direct-csi-mount-spike-result.md`): a privileged pod per `(bucket, node)` pair, replaced by `DeliverBucket`/`WithdrawBucket` on the CSI node plugin's existing `csi-link` session. This item is not filed.

**Acceptance criteria**

- [x] Not filed; superseded before any issue existed for it

### W-06 — Observability: events and metrics for bucket provisioning

**Depends on:** W-03, W-12
**Conflicts with:** —
**Labels:** operator, enhancement, medium
**Design:** §10
**Scenarios:** —
**Size:** S

Emit the five events in design §10's Kubernetes Events table, targeting the `S3Bucket` object, and export the three Prometheus metrics in its table: `simplyblock_objectstore_bucket_phase`, `simplyblock_objectstore_bucket_provisioning_seconds`, and `simplyblock_objectstore_mount_delivery_calls_total`. `simplyblock_objectstore_bucket_provisioning_seconds` is the measurement design §12 Open Question 4 (the `CreateBucket` synchronous-wait bound) depends on, so this item is a prerequisite for answering that question, not just for dashboards.

**Acceptance criteria**

- [ ] Each of the five events in design §10 fires on its named condition
- [ ] All three metrics are exported with the labels design §10 specifies
- [ ] A test plan row is added for each metric and event, since none currently exists for this item's own scope (test plan is missing dedicated observability scenarios — note this at filing, per `work-plan` skill §1)

### W-07 — ~~Scaffold the separate `S3Bucket` operator manager~~ Superseded

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §7.1 (pre-revision)
**Scenarios:** —
**Size:** M

Superseded by `0005-s3bucket-joins-the-main-operator-manager.md`: `S3Bucket`/`ObjectStore` register into the existing `kubernetes/operator` manager, so there is no second manager binary, chart entry, or leader-election setup to scaffold. The RBAC this item would have granted a new `ServiceAccount` is now simply added to the existing `ClusterRole`, as an acceptance criterion of W-03 and W-12 rather than its own item. This item is not filed.

**Acceptance criteria**

- [x] Not filed; superseded before any issue existed for it

### W-08 — Build the `envtest` fakes for CSI, `NFSExport`, and `PeerRegistry`

**Depends on:** W-02
**Conflicts with:** —
**Labels:** operator, enhancement, medium
**Design:** §11 (Testing Strategy)
**Scenarios:** —
**Size:** M

Build the integration-test double that W-03's and W-12's `I-` scenarios both need: a controller that watches `PersistentVolumeClaim`s created against the `pnfs` `StorageClass` and marks them `Bound`, one that creates a matching `NFSExport` already in `Ready`, and a fake `PeerRegistry` that acknowledges a `DeliverBucket`/`WithdrawBucket` call per node, standing in for the real CSI controller, `NFSExportReconciler` (`design-pnfs-rwx.md`), and a real `csi-link` session, all of which need a live cluster this harness does not have.

**Acceptance criteria**

- [x] A test PVC against the fake `StorageClass` reaches `Bound` without a real CSI driver
- [x] A matching `NFSExport` appears in `Ready` with a `spec.volumeRef` that resolves against the bound `PersistentVolume`
- [x] The fake `PeerRegistry` records and acknowledges a `DeliverBucket`/`WithdrawBucket` call per node without a real `csi-link` session
- [x] The fakes are reusable from both W-03's and W-12's test files without duplication (they live in `operator/internal/controllers/objectstore`, a new nested package rather than the legacy flat `internal/controller`: the repo's newer controller groups — backup, cluster, deployment, volume — have moved to a `TestMain` + shared `envtest.Environment` + plain `testing.T` convention over the older Ginkgo-based one, and this package follows that)

### W-09 — End-to-end verification on a live, pNFS-capable cluster

**Depends on:** W-03, W-11, W-12
**Conflicts with:** —
**Labels:** operator, enhancement, high, blocked
**Design:** §5.4, §10, Phase 0 P0-1
**Scenarios:** E-01, E-02, E-03, E-04, E-05, E-06, E-07, E-08
**Size:** M

Run the full E2E class from the test plan against a real cluster with the pNFS path deployed: bucket creation end to end, mount visibility in an already-running gateway pod via real `DeliverBucket` calls, a gateway pod rescheduled mid-lifecycle, first-bucket-versus-later-bucket provisioning latency (feeding `simplyblock_objectstore_bucket_provisioning_seconds`), and operator-restart recovery. E-09 is obsolete (test plan §3) and is not part of this item.

**Acceptance criteria**

- [ ] E-01 through E-08 pass
- [ ] `simplyblock_objectstore_bucket_provisioning_seconds` records both a first-bucket (cold MDS) and a later-bucket (warm MDS) measurement, and the two are distinguishable
- [ ] Design §12 Open Question 4 (the `CreateBucket` synchronous-wait bound) is answered with a measured number, not an estimate

### W-10 — Define the `DeliverBucket`/`WithdrawBucket` contract in `atlas-lib`

**Depends on:** —
**Conflicts with:** —
**Labels:** atlas-lib, enhancement, high
**Design:** §5.2
**Scenarios:** —
**Size:** S

Add a new `atlas/bucketdelivery/bucketdeliveryrpc` package (mirroring `atlas/nfsexport/nfsexportrpc`'s existing shape), defining the `DeliverBucket`/`WithdrawBucket` gRPC service: request messages carrying the bucket's volume handle and `VolumeContext` (`access_protocol`, `export_service`, `export_path`), and empty or minimal acknowledgment responses, both calls idempotent. This is the shared contract `csi-driver` (W-11) implements as a server and `operator` (W-12) calls as a client, consistent with how `nfsexportrpc` already crosses that same boundary for NFSExport assembly.

**Acceptance criteria**

- [x] `bucketdeliveryrpc` package exists with generated Go types for both RPCs
- [x] `go doc github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc` documents the contract (there is no package at `atlas/bucketdelivery` itself, the same shape as `atlas/storage/storagerpc` and `atlas/conntrack/conntrackrpc`: the parent directory is a namespace, not a package, since this feature's node-side action is the CSI node plugin's own existing `NodeStageVolume`/`NodePublishVolume`, not a new atlas-lib domain package)
- [x] `atlas-lib/README.md`'s package index and *Today* notes are updated to point at this package

### W-11 — Implement `DeliverBucket`/`WithdrawBucket` on the CSI node plugin

**Depends on:** W-10
**Conflicts with:** —
**Labels:** csi, enhancement, high
**Design:** §5.1, §5.2, §5.4
**Scenarios:** — (belongs in `csi-driver`'s own test plan, not this feature's; `test-plan-bucket-provisioning.md`'s struck `U-19` named the gap this item fills)
**Size:** M

Register `bucketdeliveryrpc`'s service on the `csilink.Agent` the node plugin already runs (`csi-driver/internal/csilink/agent.go`'s `Register`/`Capabilities`), alongside whatever `design-pnfs-rwx.md`'s NFSExport assembly already registers. Implement `DeliverBucket` by calling the node server's own `NodeStageVolume` then `NodePublishVolume` in-process, with `target_path` derived deterministically from the bucket name under one of the three `Bidirectional` `hostPath` subtrees the `DaemonSet` already mounts (`design-bucket-provisioning.md` §5.1). Implement `WithdrawBucket` the same way via `NodeUnpublishVolume`/`NodeUnstageVolume`. Both read and write the plugin's existing stack record (`ns.stack.store`); neither adds a second one.

**Acceptance criteria**

- [ ] `DeliverBucket` stages and publishes a real pNFS volume at the derived path, verified against a live cluster
- [ ] `WithdrawBucket` reverses it, idempotent on a second call
- [ ] A `target_path` outside the three documented subtrees is rejected before any mount is attempted, rather than silently succeeding invisibly (`0004-direct-csi-mount-spike-result.md`'s finding)
- [ ] The plugin's own restart recovery (`ns.stack.store`) covers a bucket `DeliverBucket` staged, with no new persistence mechanism added

### W-12 — Reconcile bucket delivery to the gateway's node set

**Depends on:** W-02, W-08, W-10
**Conflicts with:** W-03
**Labels:** operator, enhancement, high
**Design:** §5.1, §5.3, §7.2, §7.3, §7.5
**Scenarios:** U-11, U-12, U-13, U-14, U-15, U-16, I-07, I-08, I-09, I-10
**Size:** M

Runs in the existing `kubernetes/operator` manager (`0005-s3bucket-joins-the-main-operator-manager.md`), the same process as W-03 and the one that already hosts the `csi-link` hub's `PeerRegistry`. Read the `ObjectStore` named by `spec.objectStoreRef`, list its gateway Deployment's pods by the fixed label `design-objectstore.md` specifies (no field read off `ObjectStore` itself), and diff that node set against the bucket's current `status.mountDeliveredNodes`. For every node newly in the set, resolve its `csi-link` session (`registry.Conn(link.NodePeer(nodeName))`, the same in-process lookup `NFSExportReconciler` already uses) and call `DeliverBucket`; for every node no longer in the set, call `WithdrawBucket`. This item adds no CSI-facing code of its own; it is a client of W-10's contract and W-11's implementation.

**Acceptance criteria**

- [ ] U-11 through U-16 pass
- [ ] I-07 through I-10 pass against the `envtest` harness and fakes W-08 provides
- [ ] A gateway pod rescheduled to a new node gets `DeliverBucket` called there within one reconcile (I-07), and `WithdrawBucket` is called for the stale node
- [ ] Two gateway pods on the same node produce exactly one `DeliverBucket` call (U-13)
- [ ] The existing manager's `ClusterRole` diff is committed with this item, scoped to exactly the `pods`/`deployments.apps` rules design §7.5 lists for this controller

---

## 4. Not in This Plan

| Deferred                                                                                     | Why                                                                             | Where it goes                                                         |
|----------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------|-----------------------------------------------------------------------|
| The `ObjectStore` CRD's own implementation (gateway Deployment, Service, TLS, replica count) | designed (`design-objectstore.md`), this plan's design treats it as given       | that design's own work plan, not yet written                          |
| The S3 gateway binary, `CreateBucket` handler, and Vault-backed IAM                          | design §2 Non-Goals; separate repository                                        | that repository's own planning                                        |
| Resolving design §12 Open Question 3 (who holds the Kubernetes RBAC to create an `S3Bucket`) | gateway-repository and RBAC-hardening concern, not an operator-side code change | gateway repository, or a `rbac-hardening` pass once W-02's CRD exists |
| Phase 2's native backend                                                                     | out of scope for this feature's Phase 1 entirely                                | a future design                                                       |
