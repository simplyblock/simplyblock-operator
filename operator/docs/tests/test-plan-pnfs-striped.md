# Test Plan: Striped pNFS (RWX) Volumes and Consistency-Group Snapshots

Related design: [`designs/design-pnfs-striped.md`](../designs/design-pnfs-striped.md)
Base plan: [`test-plan-pnfs-rwx.md`](test-plan-pnfs-rwx.md), which covers
single-volume exports. Every scenario there still applies with `stripe_count = 1`
and is not repeated here.
Harness: `csi-driver` (`make -C csi-driver unit-test`, `e2e-test`) and `operator`
(`make -C operator test`).

This work is unimplemented, so every `Test` cell is `—` and every scenario appears
in §9 as a gap.

Scope: the CSI driver, the operator, and the Kubernetes surface of this repository.
The consistency-group primitive itself is a control-plane dependency, faked at the
boundary, and tracked as an external blocker in the design's Phase 0 table rather
than as a scenario here.

Scenario IDs are permanent and continue the base plan's prefixes with an `S`
qualifier, so they cannot collide with it:

| Prefix | Class                     | Harness                                                                                      |
|--------|---------------------------|----------------------------------------------------------------------------------------------|
| `SU-`  | CSI driver unit           | mock `ClusterAPI` and mock HTTP, no cluster                                                  |
| `SO-`  | Operator unit and envtest | fake client, mock webapi, `envtest`                                                          |
| `SI-`  | Integration               | driver plus the guest's export assembler against a mock control plane, host commands stubbed |
| `SE-`  | End-to-end                | live cluster, kernel ≥ 6.11, at least three storage nodes                                    |
| `SF-`  | Failure injection         | live cluster plus fault injection                                                            |
| `VGS-` | VolumeGroupSnapshot       | mock control plane for unit and integration, Kubernetes ≥ 1.32 for e2e                       |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when nothing covers the scenario yet.

---

## 1. Unit Tests — CSI driver (`csi-driver/internal`)

Mock `ClusterAPI` and mock HTTP. No cluster, no host commands.

### StorageClass parsing and `CreateVolume` planning (design §2.2, §4)

| #         | Scenario                                                                                                      | Type     | Test |
|-----------|---------------------------------------------------------------------------------------------------------------|----------|------|
| SU-01     | `stripe_count` absent defaults to `1` and takes the base design's single-volume path                          | Positive | —    |
| SU-02     | `stripe_count = 4` plans four lvols of `ceil(S/4)`, each GiB-aligned                                          | Positive | —    |
| SU-03     | `S` not divisible by `n` rounds up per member, so total capacity is at least `S`                              | Boundary | —    |
| SU-04     | `stripe_count` above the eligible node count is rejected with `InvalidArgument`                               | Negative | —    |
| SU-05     | `stripe_count = 0` or negative is rejected                                                                    | Negative | —    |
| SU-06     | `stripe_count > 1` on an RWO volume is rejected                                                               | Negative | —    |
| SU-07     | Members are recorded on the CR with lvol id, NGUID, and size, in stable order                                 | Positive | —    |
| ~~SU-08~~ | ~~VG and LV names derive from the volume name alone, with no host or random component~~ (superseded by SU-25) | Positive | —    |
| SU-09     | Members landing on fewer than `n` distinct nodes fail provisioning rather than degrading                      | Negative | —    |

### Snapshot planning (design §3.3)

| #     | Scenario                                                                                      | Type     | Test |
|-------|-----------------------------------------------------------------------------------------------|----------|------|
| SU-10 | `CreateSnapshot` on a `stripe_count = 1` volume still uses the per-lvol path                  | Positive | —    |
| SU-11 | `CreateSnapshot` on a striped volume issues exactly one group call for all members            | Positive | —    |
| SU-12 | Snapshot of a striped volume with no group API available is refused with `FailedPrecondition` | Negative | —    |
| SU-13 | Snapshot id encodes the group as `{clusterID}:{poolID}:{groupSnapUUID}`                       | Positive | —    |
| SU-14 | Resize plans a uniform per-member grow, never an uneven one                                   | Boundary | —    |
| SU-15 | `stripe_count = 2`, the smallest true stripe, plans two members and a two-way stripe          | Boundary | —    |
| SU-16 | `stripe_count` exactly equal to the eligible node count is accepted, not rejected             | Boundary | —    |

### Client plan and the `lvmFilter` layer (design §2.2, §2.4)

| #     | Scenario                                                                                                   | Type     | Test |
|-------|------------------------------------------------------------------------------------------------------------|----------|------|
| SU-17 | The striped client plan is `lvmFilter` → `members(n)` → `alias` → `nfsMount`, with no LVM activation layer | Positive | —    |
| SU-18 | `lvmFilter` `Ensure` writes one reject entry per member NGUID into the marked `global_filter` block        | Positive | —    |
| SU-19 | `lvmFilter` on a host without LVM reports `StateReady` and writes nothing                                  | Boundary | —    |
| SU-20 | `lvmFilter` on a devices-file host that lists no member reports `StateReady` and writes nothing            | Positive | —    |
| SU-21 | `lvmFilter` on a devices-file host that lists a member reports `StateForeign`, and staging is refused      | Negative | —    |
| SU-22 | `lvmFilter` with some entries present reports `StatePartial`, and `Ensure` adds only the missing ones      | Boundary | —    |
| SU-23 | `lvmFilter` `Release` removes this volume's entries and leaves another volume's entries in place           | Positive | —    |
| SU-24 | `lvmFilter` `Healthy` reports false when a device-mapper device holds a member                             | Negative | —    |

### Naming (design §2.3)

| #     | Scenario                                                                                                     | Type     | Test |
|-------|--------------------------------------------------------------------------------------------------------------|----------|------|
| SU-25 | A striped export's group and logical volume are `vol-<uuid>` and `lv-<uuid>` of the first member's lvol UUID | Positive | —    |
| SU-26 | Two same-named PVCs in different Kubernetes namespaces plan different groups                                 | Negative | —    |

---

## 2. Unit Tests — operator (`operator/internal`)

| #     | Scenario                                                                                              | Type       | Test |
|-------|-------------------------------------------------------------------------------------------------------|------------|------|
| SO-01 | `NFSExport` status carrying `stripeCount` and `members[]` round-trips through the API                 | Positive   | —    |
| SO-02 | An export written before striping (single `lvolID`) is rewritten in place to a one-member `members[]` | Regression | —    |
| SO-03 | Failover reproduces `vgName` and `lvName` from the CR rather than recomputing them                    | Positive   | —    |
| SO-04 | Failover with a member missing leaves the export `Degraded` and mounts nowhere                        | Negative   | —    |
| SO-05 | The no-dual-mount invariant holds when reassembly fails halfway on the target host                    | Negative   | —    |

---

## 3. Integration Tests

Driver plus the guest's export assembler (`mds-agent`) against a mock control plane, with host commands
stubbed. The step sequence and its idempotency are what break here.

| #     | Scenario                                                                                                    | Type     | Test |
|-------|-------------------------------------------------------------------------------------------------------------|----------|------|
| SI-01 | The `Striped` plan's order is attach all `n`, `pvcreate`, `vgcreate`, `lvcreate`, `mkfs.xfs`, mount, export | Positive | —    |
| SI-02 | Re-running `CreateExport` on a fully assembled export changes nothing                                       | Positive | —    |
| SI-03 | Re-running after a failure between `vgcreate` and `lvcreate` completes without duplicating                  | Positive | —    |
| SI-04 | `CreateExport` waits for all `n` devices and does not assemble a short stripe                               | Negative | —    |
| SI-05 | Device path churn between reconnects does not break assembly, because LVM matches PV metadata               | Positive | —    |
| SI-06 | `DeleteExport` tears down in reverse: unexport, unmount, `lvremove`, `vgremove`, `pvremove`, release        | Positive | —    |
| SI-07 | `mkfs.xfs` is skipped when `blkid` already reports XFS on the logical volume                                | Boundary | —    |
| SI-08 | Snapshot freezes, calls the group API once, and thaws even when the call fails                              | Negative | —    |
| SI-09 | The assembler refuses to export `n > 1` when the guest nfsd lacks the `pnfs_dm_stripe` parameter            | Negative | —    |
| SI-10 | After a resize, a table that is not one `striped` target stops the grow before `xfs_growfs`                 | Negative | —    |
| SI-11 | After activation, a table whose device order differs from the CR's member order is not exported             | Negative | —    |

---

## 4. End-to-End Tests (live cluster, kernel ≥ 6.11, three or more storage nodes)

| #     | Scenario                                                                                                  | Type     | Test |
|-------|-----------------------------------------------------------------------------------------------------------|----------|------|
| SE-01 | Provision a striped RWX PVC with `n = 3`, mount it on three nodes, read and write shared data             | Positive | —    |
| SE-02 | Members verifiably landed on three distinct storage nodes                                                 | Positive | —    |
| SE-03 | Aggregate sequential throughput exceeds a single-member export by a meaningful margin                     | Positive | —    |
| SE-04 | The direct block path is in use, confirmed by per-namespace counters rather than by absence of errors     | Positive | —    |
| SE-05 | Snapshot a striped volume, restore it, mount the restore, and `fsck` clean                                | Positive | —    |
| SE-06 | Restore a snapshot taken under sustained write load, then verify a database checkpoint replays            | Positive | —    |
| SE-07 | Online resize grows every member, then the LV, then the filesystem, with data intact                      | Positive | —    |
| SE-08 | Clone a striped volume and confirm the clone is independent, with its own export, Service, and `fsid`     | Positive | —    |
| SE-09 | Provision with `n` greater than the available nodes and confirm a clean failure, not a degenerate stripe  | Negative | —    |
| SE-10 | GETDEVICEINFO for a striped export returns one `STRIPE` of `n` `SLICE` and `SCSI` volumes, in table order | Positive | —    |
| SE-11 | Data written through the direct path reads back identically from each member at the table's offsets       | Positive | —    |
| SE-12 | A fenced client is preempted on every member, and its direct writes fail on all `n`                       | Negative | —    |
| SE-13 | A client node with LVM event activation enabled stages a striped volume and never activates its group     | Negative | —    |
| SE-14 | After an online resize the table is still one `striped` target and the direct path stays in use           | Positive | —    |

---

## 5. Failure Injection

| #     | Scenario                                                                                                       | Type     | Test |
|-------|----------------------------------------------------------------------------------------------------------------|----------|------|
| SF-01 | Kill one member's NVMe-oF path and confirm the export degrades with a diagnosable event                        | Negative | —    |
| SF-02 | Kill the metadata server pod and confirm failover reassembles the stripe with the same `fsid` and file handles | Positive | —    |
| SF-03 | Measure the failover freeze with a stripe and compare it against the base design's NFR-2 bound                 | Boundary | —    |
| SF-04 | Fail the group snapshot midway and confirm no partial group is left presented as usable                        | Negative | —    |
| SF-05 | Partition the old MDS during failover and confirm PR fencing prevents a second mount                           | Negative | —    |
| SF-06 | Interrupt a resize between member grow and `lvextend`, then confirm the retry converges                        | Negative | —    |

---

## 6. VolumeGroupSnapshot and Group Controller (design §5)

Exercise the CSI GroupController service and the upstream feature. Unit and
integration use a mock control plane. End-to-end needs Kubernetes ≥ 1.32 with the
beta feature enabled.

| #      | Scenario                                                                                                                      | Type     | Test |
|--------|-------------------------------------------------------------------------------------------------------------------------------|----------|------|
| VGS-01 | `CreateVolumeGroupSnapshot` flattens source volumes, including each striped volume's `n` members, into one backend group call | Positive | —    |
| VGS-02 | A mixed group of one RWO and one striped RWX volume produces a single atomic group request                                    | Positive | —    |
| VGS-03 | Idempotent by group name under sidecar retry, returning the same group id and creating no duplicate                           | Positive | —    |
| VGS-04 | Sources spanning two clusters or pools are rejected with `InvalidArgument`                                                    | Negative | —    |
| VGS-05 | `DeleteVolumeGroupSnapshot` removes the backend group and every member snapshot                                               | Positive | —    |
| VGS-06 | `GetVolumeGroupSnapshot` surfaces a partial member failure rather than hiding it                                              | Boundary | —    |
| VGS-07 | E2E: label two or more PVCs, take a group snapshot, and confirm member `VolumeSnapshot`s appear and are mutually consistent   | Positive | —    |
| VGS-08 | E2E: restore each member into a new PVC, confirming RWX members get fresh exports and data matches                            | Positive | —    |
| VGS-09 | E2E: on Kubernetes below 1.32, the group feature is unavailable while single-PVC snapshots still work                         | Negative | —    |
| VGS-10 | Sanity: identity reports `GROUP_CONTROLLER_SERVICE` and the group controller reports its capability                           | Positive | —    |

---

## 7. Axis Coverage

| Axis                 | Values and the scenarios covering them                                                                                                                                                                               |
|----------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Stripe width         | `n = 1` (SU-01, SU-10), `n = 2` as the smallest true stripe (SU-15), `n = 3` (SE-01), `n` exactly at capacity (SU-16), `n` above capacity (SU-04, SE-09)                                                             |
| Node count           | three or more (SE-01, SE-02), fewer than `n` (SU-09, SE-09)                                                                                                                                                          |
| Namespace scope      | group naming across namespaces (SU-26). **Multi-namespace end-to-end is a gap**, and it is where export paths and `fsid`s collide                                                                                    |
| Group membership     | valid single-cluster and single-pool group (VGS-01, VGS-02), rejected cross-cluster or cross-pool sources (VGS-04). A striped volume spanning two clusters is out of scope by design (§1.2) and is not the same case |
| Snapshot consistency | idle (SE-05), under load (SE-06), failed midway (SF-04)                                                                                                                                                              |
| Failover             | planned (SF-02), partitioned (SF-05), unreassemblable (SO-04, SO-05)                                                                                                                                                 |
| Guest kernel         | topology support present (SE-10, SE-11), absent (SI-09)                                                                                                                                                              |
| Client LVM           | none installed (SU-19), devices file (SU-20, SU-21), `global_filter` (SU-18, SU-22, SU-23), event activation enabled (SE-13)                                                                                         |

## 8. Coverage Summary

| Class                        | Scenarios | Covered | Gap                       |
|------------------------------|-----------|---------|---------------------------|
| CSI driver unit (`SU-`)      | 25        | 0       | SU-01…SU-26, except SU-08 |
| Operator (`SO-`)             | 5         | 0       | SO-01…SO-05               |
| Integration (`SI-`)          | 11        | 0       | SI-01…SI-11               |
| End-to-end (`SE-`)           | 14        | 0       | SE-01…SE-14               |
| Failure injection (`SF-`)    | 6         | 0       | SF-01…SF-06               |
| VolumeGroupSnapshot (`VGS-`) | 10        | 0       | VGS-01…VGS-10             |
| **Total**                    | **71**    | **0**   |                           |

---

## 9. What Is Not Yet Covered

| IDs           | Reason                                                                                                    |
|---------------|-----------------------------------------------------------------------------------------------------------|
| SU-01…SU-26   | Nothing is implemented, so there is no `CreateVolume` striping path and no `lvmFilter` layer to test      |
| SO-01…SO-05   | The `NFSExport` CRD does not carry `members[]` yet, and no reconciler assembles a stripe                  |
| SI-01…SI-11   | No `CreateExport` LVM path exists to drive                                                                |
| SE-01…SE-14   | Requires a live cluster with three or more storage nodes, the guest topology patch, and the feature built |
| SF-01…SF-06   | Requires the above plus fault injection, and SF-04 additionally requires the consistency group (S0-1)     |
| VGS-01…VGS-10 | Requires the GroupController service, the group CRDs in the chart, and Kubernetes ≥ 1.32                  |

Three further gaps are deliberate rather than pending, and are recorded so they are
not mistaken for coverage:

- **Multi-namespace striping.** No end-to-end scenario places two striped exports from different namespaces on one metadata server, which is where the `fsid` allocator is under real pressure. Group naming is covered at unit level (SU-26). The base plan carries the same gap, and it is inherited here rather than fixed.
- **Single-node topology.** The base plan leaves a one-node cluster uncovered, and that inherited gap applies here too: at `n = 1` on one node, the sole member and the MDS necessarily co-locate.
- **Stripe chunk size.** No scenario varies `--stripesize`, because the design has no answer yet for what value is right (design S2).
