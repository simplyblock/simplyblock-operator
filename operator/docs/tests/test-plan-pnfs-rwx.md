# Test Plan: pNFS (RWX) Support

Related design: [`designs/design-pnfs-rwx.md`](../designs/design-pnfs-rwx.md)
Striped volumes and group snapshots: [`test-plan-pnfs-striped.md`](test-plan-pnfs-striped.md)
Harness: `csi-driver` (`make -C csi-driver unit-test`, `e2e-test`) and
`operator` (`make -C operator test`). The pNFS work is unimplemented, so every
`Test` cell is `—` and every scenario is listed in §13 as a gap.

Scope: the CSI driver, the operator, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) and SPDK behavior is a dependency, faked at
the boundary — the backend preconditions this design needs are tracked as
external blockers in the design's Phase 0 table, not as scenarios here.

This plan covers **single-volume** pNFS exports only. Striping, consistency-group
snapshots, and the user-facing `VolumeGroupSnapshot` moved to
[`test-plan-pnfs-striped.md`](test-plan-pnfs-striped.md) along with their design.

Scenario IDs are permanent. This plan keeps the prefixes the design assigned:

| Prefix | Class                     | Harness                                                                           |
|--------|---------------------------|-----------------------------------------------------------------------------------|
| `U-`   | CSI driver unit           | mock `ClusterAPI` and mock HTTP, no cluster                                       |
| `O-`   | Operator unit and envtest | fake client, mock webapi, `envtest`                                               |
| `SAN-` | CSI sanity                | `csi-sanity` against the driver                                                   |
| `I-`   | Integration               | driver plus MDS-host csi-node against a mock control plane, host commands stubbed |
| `E-`   | End-to-end                | live cluster, kernel ≥ 6.11                                                       |
| `F-`   | Failure injection         | live cluster plus fault injection                                                 |
| `SEC-` | Security                  | live cluster, allow-listing and fencing                                           |
| `L-`   | Load, scale, and soak     | live cluster, multi-day for `L-03`                                                |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when the scenario is not covered yet.

Scenario phrasing follows the sibling plans under `operator/docs/tests/`.

---

## 1. Unit Tests — CSI driver (`csi-driver/pkg`)

Pure functions and server helpers, covered without external dependencies (mock `ClusterAPI` / mock HTTP).

#### StorageClass Parsing & CreateVolume Planning (design §9.2–9.3) — `pkg/spdk/controllerserver_test.go`

| #    | Scenario                                                                                    | Type     | Test |
|------|---------------------------------------------------------------------------------------------|----------|------|
| U-01 | `pnfs=true` + `stripe_count=n` → plan with `n` members and `PR=true`                        | Positive | —    |
| U-02 | Size split: requested `S` → `n` members each `ceil(S/n)` GiB-aligned; `sum ≥ S`             | Positive | —    |
| U-03 | `stripe_count` omitted → defaults to `1`                                                    | Positive | —    |
| U-04 | `MULTI_NODE_MULTI_WRITER` → pNFS path; `SINGLE_NODE_WRITER` → RWO path unchanged            | Positive | —    |
| U-05 | Idempotency: repeated `CreateVolume` (same name) → same handle/record, no extra lvols       | Positive | —    |
| U-06 | `stripe_count == #eligible nodes` exactly → accepted                                        | Boundary | —    |
| U-07 | `stripe_count = 0` or negative → `InvalidArgument`                                          | Negative | —    |
| U-08 | `stripe_count > #eligible nodes` → `InvalidArgument`/`ResourceExhausted` with clear message | Negative | —    |
| U-09 | `pnfs=true` with `fsType=ext4` → rejected (XFS required)                                    | Negative | —    |
| U-10 | `pnfs=true` with `volumeMode=Block` → rejected                                              | Negative | —    |
| U-11 | Non-integer `stripe_count` → `InvalidArgument`                                              | Negative | —    |

#### Volume Handle (design §11) — `pkg/kubernetes/volumehandle/index_test.go`

| #    | Scenario                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------|----------|------|
| U-12 | pNFS handle (`nfs:cluster:pool:export`) round-trips through `Parse`        | Positive | —    |
| U-13 | Legacy 3-part RWO handle still parses (backward compat)                    | Positive | —    |
| U-14 | Malformed pNFS handle (wrong part count, non-UUID export) → `(Nil, false)` | Negative | —    |
| U-15 | Empty / whitespace handle → `(Nil, false)`                                 | Negative | —    |

#### CreateLVolData / API Client (design §6.2–6.3) — `pkg/util/*_test.go` (mock HTTP)

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| U-16 | `PR=true` serializes to `"pr": true` in the create body                                  | Positive | —    |
| U-17 | Group-snapshot request serializes all `n` member ids; response parses group + member ids | Positive | —    |
| U-18 | Connect info for `n` members returns `n` connection sets                                 | Positive | —    |
| U-19 | Group-snapshot with a missing member → error surfaced                                    | Negative | —    |

#### Node Stage / Publish Planning (design §10) — `pkg/spdk/nodeserver_test.go` (new)

| # | Scenario | Type | Test |
|---|---|---|---|
| U-20 | pNFS `VolumeContext` parses into `n` members, `mds_ip`, `export_path`, `fsid` | Positive | — |
| U-21 | NFS mount command is `mount -t nfs -o v4.1 {mds_ip}:{export_path} {stage}` (+ extra opts) | Positive | — |
| U-22 | `eui64` symlink target computed correctly from an `nvme id-ns` NGUID fixture | Positive | — |
| U-23 | Publish refcount: two publishes then one unpublish keeps staging mount; 2nd unpublish+unstage tears down | Boundary | — |
| U-24 | Missing `mds_ip`/`fsid` in context → `InvalidArgument` | Negative | — |
| U-25 | `access_protocol=nfs` but empty member list → error | Negative | — |

#### Export Record / Migration State Machine (design §7.1, design §13) — new package

| #    | Scenario                                                       | Type     | Test |
|------|----------------------------------------------------------------|----------|------|
| U-26 | `Ready → Migrating → Ready` bumps `generation`                 | Positive | —    |
| U-27 | Bind a second MDS while `Ready` (single-writer) → rejected     | Negative | —    |
| U-28 | Snapshot request while record is `Migrating` → rejected/queued | Negative | —    |

---

## 2. Unit Tests — operator (`operator/internal`, fake client and envtest)

| #    | Reconciler / Client              | Scenario                                                                                                                                      | Type     | Test |
|------|----------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| O-01 | `StorageNodeSetReconciler`       | `nfsServerEnabled` deploys NFS daemon; node ready only after `/snode/info` `nfs_server_status=healthy`                                        | Positive | —    |
| O-02 | `StorageNodeSetReconciler`       | Node kernel < `kernelVersionMin` → marked NFS-incompatible; not selected as MDS                                                               | Negative | —    |
| O-03 | `NodeDrainCoordinatorReconciler` | Export-quiesce phase runs before shutdown; `exportPhase` tracked; shutdown blocked until drained                                              | Positive | —    |
| O-04 | `VolumeMigrationReconciler`      | Advances `Running → ExportTransitioning → Completed`; export re-created with same `fsid` on target                                            | Positive | —    |
| O-05 | `VolumeMigrationReconciler`      | Abort during `ExportTransitioning` restores the source export cleanly                                                                         | Negative | —    |
| O-06 | `webapi.Client`                  | `CreateExport`/`DeleteExport`/`QuiesceExports` hit the expected v2 endpoints (mock webapi)                                                    | Positive | —    |
| O-07 | `TaskReconciler`                 | Export create/teardown task types surface in status; reconciler waits before advancing                                                        | Positive | —    |
| O-08 | `StorageNodeSetReconciler`       | `/snode/info` omits the capability fields, reports `nfs_capable=false`, or 404s → node marked ineligible with a reason, never selected as MDS | Negative | —    |
| O-09 | `webapi.Client`                  | An endpoint the design depends on is absent (501/404) → reported as a clean provisioning failure, not a panic or a silent success             | Negative | —    |

---

## 3. Sanity Tests (`pkg/spdk/sanity_test.go`)

| #      | Scenario                                                                                                         | Type     | Test |
|--------|------------------------------------------------------------------------------------------------------------------|----------|------|
| SAN-01 | With `MULTI_NODE_MULTI_WRITER` added, csi-sanity confirms identity/controller capability reporting is consistent | Positive | —    |
| SAN-02 | Existing RWO sanity suite still passes (no regression)                                                           | Positive | —    |

## 4. Integration Tests (driver + MDS-host csi-node ↔ mock control plane; host commands stubbed)

The server-side export assembly — LVM, `mkfs.xfs`, mount, `exportfs` — is
csi-node logic in this repository, not control-plane logic, which is why its step
order, idempotency, and failure paths are tested here with the host commands
stubbed rather than left to the backend.

| #    | Scenario                                                                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `CreateVolume` → csi-node `CreateExport` runs steps in order (wait-for-members → LVM → mkfs.xfs → mount → export → `exportfs -ra`) | Positive | —    |
| I-02 | `DeleteVolume` issues teardown in reverse order and deletes all `n` lvols                                                          | Positive | —    |
| I-03 | `CreateExport` idempotency: re-invoked after mid-way failure skips satisfied steps (`blkid` formatted → no re-mkfs)                | Positive | —    |
| I-04 | Resize path: member resize + LVM extend + `xfs_growfs` on MDS; `NodeExpandVolume` is a no-op                                       | Positive | —    |
| I-05 | Reconciler GCs a record stuck in `Provisioning` past timeout (orphan lvols removed)                                                | Positive | —    |
| I-06 | `mkfs.xfs` failure → `CreateVolume` error; no export published; record stays `Provisioning`                                        | Negative | —    |
| I-07 | `nvme connect` fails for one member → abort, no LVM assembly, partial members cleaned up                                           | Negative | —    |
| I-08 | Snapshot without consistency-group support → clean `FailedPrecondition`                                                            | Negative | —    |

## 5. End-to-End Tests (`e2e/pnfs.go`, real cluster, kernel ≥ 6.11)

| #    | Scenario                                                                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Provision RWX PVC; 3 pods across 3 nodes mount and read/write a shared file; data visible across pods                           | Positive | —    |
| E-02 | Direct block path used: `n` namespaces attached on client; layout stats show block (not MDS) I/O for large sequential writes    | Positive | —    |
| E-03 | Concurrent writers with byte-range locks: two pods append to the same file, checksum verified                                   | Positive | —    |
| E-04 | `stripe_count=1` MVP path (Phase 1) end-to-end                                                                                  | Positive | —    |
| E-05 | `stripe_count=n` (n ≥ 3) end-to-end; throughput vs. RWO baseline (NFR-1)                                                        | Positive | —    |
| E-06 | Snapshot RWX (consistency group) → restore into new RWX PVC → data matches; new PVC has its own MDS binding                     | Positive | —    |
| E-07 | Clone RWX → independent RWX PVC                                                                                                 | Positive | —    |
| E-08 | Online resize under active I/O: `xfs_growfs` grows the mount; pods see new capacity; no I/O interruption                        | Positive | —    |
| E-09 | Delete RWX PVC → unexported, LV/VG removed, namespaces disconnected (MDS + clients), lvols deleted                              | Positive | —    |
| E-10 | Distro matrix: core provision/mount/rw on RHEL/Rocky/Alma **and** Ubuntu; `eui64`+direct path work or graceful fallback (FM-10) | Positive | —    |
| E-11 | RWX pod on kernel < 6.11 node → scheduling avoided / stage fails with clear event; no partial mount                             | Negative | —    |
| E-12 | Request RWX with `ext4` → provisioning fails with a clear message                                                               | Negative | —    |
| E-13 | Two RWX PVCs never share an `fsid` on the same MDS host (provision many, assert uniqueness)                                     | Negative | —    |
| E-14 | `blkmapd` stopped on a client → I/O continues via MDS fallback; node plugin restarts `blkmapd` and logs                         | Negative | —    |

## 6. Failure-Injection / Resilience E2E (extend `e2e/reconnect*.go` patterns)

| #    | Scenario                                                                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------|----------|------|
| F-01 | MDS host graceful drain → migration → clients freeze then resume within NFR-2 bound; checksum clean                        | Positive | —    |
| F-02 | MDS host hard kill (unplanned) → PR fencing + migration; no corruption; bounded freeze                                     | Positive | —    |
| F-03 | Single stripe-member NVMe path loss then heal → direct path recovers; MDS fallback covers the gap                          | Positive | —    |
| F-04 | Client node reboot with active RWX mount → restage reconnects namespaces + remounts NFS; pods recover                      | Positive | —    |
| F-05 | CSI node/controller pod killed mid-operation → at-most-one export/lvol set; recovery on restart                            | Positive | —    |
| F-06 | MDS data-IP float variant → transparent NFS reconnect after freeze                                                         | Positive | —    |
| F-07 | MDS IP-change variant → clients remount on `generation` bump without pod disruption                                        | Positive | —    |
| F-08 | Rolling storage-node restart under active RWX I/O → sustained I/O (per-node migration)                                     | Positive | —    |
| F-09 | Stripe member permanently lost (exceeds member redundancy) → XFS errors surface as volume-unhealthy, not silent corruption | Negative | —    |
| F-10 | Network partition: clients↔MDS cut but namespaces reachable → metadata blocks, direct I/O verified; heal reconnects        | Negative | —    |

## 7. Security Tests

| #      | Scenario                                                                                                   | Type     | Test |
|--------|------------------------------------------------------------------------------------------------------------|----------|------|
| SEC-01 | With host allow-listing + PR fencing, a fenced client can no longer write to the shared namespaces         | Positive | —    |
| SEC-02 | (If in scope) `sec=krb5` mount succeeds with valid credentials                                             | Positive | —    |
| SEC-03 | `root_squash`/tenancy: in-pod root cannot write as root when squashed                                      | Positive | —    |
| SEC-04 | Unauthorized node (NQN not allow-listed) attempts `nvme connect` a member namespace → refused              | Negative | —    |
| SEC-05 | Host outside the export client set attempts `mount` → denied (once `*` replaced by allow-list, design §15) | Negative | —    |
| SEC-06 | (If in scope) `sec=krb5` mount without valid credentials → denied                                          | Negative | —    |

## 8. Long-Term / Load / Soak Tests

| #    | Scenario                                                                                                                                                                                    | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| L-01 | Load: 20–50 clients driving `fio` (mixed random/sequential, varying block sizes) on one RWX volume; measure aggregate throughput, per-client latency, direct-vs-MDS I/O ratio, MDS CPU      | Positive | —    |
| L-02 | Scale: many RWX volumes per MDS host (find `nfsd`/export limits); many exports per cluster; `stripe_count` upper bound tracks `#eligible nodes`                                             | Boundary | —    |
| L-03 | Soak (multi-day): sustained mixed I/O + periodic snapshot/clone/resize; assert no leaks (lvols/exports/mounts/symlinks), no `fsid` exhaustion, stable CSI+SNodeAPI memory, XFS `fsck` clean | Positive | —    |
| L-04 | Chaos/soak: continuous random MDS migrations + node reboots + path flaps with active writers; end-state checksum + `fsck` clean; freeze times within NFR-2                                  | Positive | —    |
| L-05 | Provisioning churn: rapid create/delete of RWX PVCs → detect record/registry leaks and reconciler correctness under load                                                                    | Negative | —    |
| L-06 | Regression guard: existing RWO load/reconnect suites pass alongside (no regression from shared initiator/monitor code, NFR-4)                                                               | Positive | —    |

## 9. Test Environment Requirements


- Kubernetes cluster with worker + storage nodes on kernel ≥ 6.11, `nfs-utils`/`nfs-common` installed, storage data network reachable.
- Backend (`sbcli`) build with `-pr` and consistency-group APIs (Phase 0); operator build with the CRD/reconciler changes (design §14.2).
- For VolumeGroupSnapshot e2e (design §20.10): Kubernetes ≥ 1.32 with `external-snapshotter`/`snapshot-controller` ≥ 8.2, `external-provisioner` ≥ 5.1, and the group-snapshot CRDs installed.
- Distro matrix: at least one RHEL-family and one Debian-family node pool.
- Fault-injection tooling consistent with the existing reconnect e2e suites (network partition, process kill, node drain).

---

---

## 10. Axis Coverage

Which topologies the matrix exercises. An axis value with no IDs is a gap, not an
omission — see §13.

| Axis              | Values covered                                                   | IDs                           | Not covered                                                                                                                                        |
|-------------------|------------------------------------------------------------------|-------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| Cluster topology  | 3 nodes (three clients, three members)                           | E-01, E-05, F-08              | single-node cluster; a cluster with fewer eligible MDS hosts than `stripe_count` beyond the `U-08` unit check; 5+ nodes                            |
| Stripe width      | 1 member, `n ≥ 3`, `n == #eligible nodes`, 0 and negative        | E-04, E-05, U-06, U-07, U-08  | `n` larger than the member limit of one MDS host                                                                                                   |
| Namespace scope   | single namespace (implicit in every e2e row)                     | —                             | **multi-namespace: two PVCs of the same name in two namespaces sharing one MDS host, `fsid` and export-path collision, cross-namespace isolation** |
| Cluster count     | one cluster; sources spanning two clusters or pools rejected     | VGS-04                        | two `StorageCluster`s each hosting RWX exports; per-cluster `fsid` allocation                                                                      |
| Kernel and distro | kernel ≥ 6.11, kernel < 6.11, RHEL family, Debian family         | E-10, E-11, O-02              | a mixed-kernel cluster where only some nodes are eligible                                                                                          |
| Lifecycle         | create, resize, snapshot, clone, delete, migrate, drain, restart | E-06 … E-09, F-01, F-04, F-08 | operator restart mid-export-assembly (`I-05` covers the record, not the operator)                                                                  |
| Scale             | 20–50 clients, many volumes per MDS host, churn                  | L-01, L-02, L-05              | zero-member and single-client degenerate cases                                                                                                     |

---

## 11. Coverage Summary

| Class                        | Scenarios | Covered | Not covered |
|------------------------------|-----------|---------|-------------|
| CSI unit (`U-`)              | 28        | 0       | all         |
| Operator (`O-`)              | 9         | 0       | all         |
| Sanity (`SAN-`)              | 2         | 0       | all         |
| Integration (`I-`)           | 8         | 0       | all         |
| End-to-end (`E-`)            | 14        | 0       | all         |
| Failure injection (`F-`)     | 10        | 0       | all         |
| Security (`SEC-`)            | 6         | 0       | all         |
| Load and soak (`L-`)         | 6         | 0       | all         |
| VolumeGroupSnapshot (`VGS-`) | 11        | 0       | all         |
| **Total**                    | **94**    | **0**   | **all**     |

Nothing is implemented yet, by design: the document is a Draft and the feature
depends on backend work that has not landed (the design's §6.1). The number to
watch is the first non-zero one — a row moving from `—` to a test function is
what turns this plan from a proposal into coverage.

---

## 12. What Is Not Yet Covered

| #      | Gap                                                                                                      | Reason                                                                                                                                                                                                                                                                           |
|--------|----------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| all 94 | Every scenario in this plan                                                                              | The feature is unimplemented: no pNFS code exists in `csi-driver` or `operator`, so no test can be written against it yet                                                                                                                                                        |
| —      | Multi-namespace RWX provisioning                                                                         | No scenario exists. Two PVCs of the same name in different namespaces sharing an MDS host is exactly where the export path and the `fsid` can collide, and the design's §11 volume handle is the only thing separating them. Needs rows before implementation starts             |
| —      | Single-node cluster                                                                                      | `stripe_count=1` on a one-node cluster leaves the MDS host and the only member host identical; whether that is supported or rejected is undecided (design §18)                                                                                                                   |
| —      | Two `StorageCluster`s with RWX exports                                                                   | `fsid` allocation is per MDS host (design §18); with two clusters on one host the allocator's scope is undefined                                                                                                                                                                 |
| —      | Operator restart during export assembly                                                                  | `I-05` covers a record stuck in `Provisioning`; nothing covers the operator dying between the backend call and the record write                                                                                                                                                  |
| —      | Backend behavior (`-pr` on lvol create, consistency-group atomicity, `/snode/info` capability reporting) | Out of scope for this repository: those suites live in the `sbcli` repository. The design tracks them as external blockers in design §6.1, and this plan covers only the boundary against them — `U-16`, `U-19`, `O-08`, `O-09`, and the outcome assertions in `E-06` and `E-07` |
