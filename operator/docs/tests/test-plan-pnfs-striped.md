# Test Plan: Striped pNFS (RWX) Volumes and Consistency-Group Snapshots

Related design: [`designs/design-pnfs-striped.md`](../designs/design-pnfs-striped.md)
Base plan: [`test-plan-pnfs-rwx.md`](test-plan-pnfs-rwx.md), which covers
single-volume exports. Every scenario there still applies with `stripe_count = 1`
and is not repeated here.
Harness: `csi-driver` (`make -C csi-driver unit-test`, `e2e-test`) and `operator`
(`make -C operator test`).

This work is unimplemented, so every `Test` cell is `—` and every scenario appears
in §7 as a gap.

Scope: the CSI driver, the operator, and the Kubernetes surface of this repository.
The consistency-group primitive itself is a control-plane dependency, faked at the
boundary, and tracked as an external blocker in the design's Phase 0 table rather
than as a scenario here.

Scenario IDs are permanent and continue the base plan's prefixes with an `S`
qualifier, so they cannot collide with it:

| Prefix  | Class                     | Harness                                                                           |
|---------|---------------------------|-----------------------------------------------------------------------------------|
| `SU-`   | CSI driver unit           | mock `ClusterAPI` and mock HTTP, no cluster                                       |
| `SO-`   | Operator unit and envtest | fake client, mock webapi, `envtest`                                               |
| `SI-`   | Integration               | driver plus MDS-host csi-node against a mock control plane, host commands stubbed |
| `SE-`   | End-to-end                | live cluster, kernel ≥ 6.11, at least three storage nodes                          |
| `SF-`   | Failure injection         | live cluster plus fault injection                                                 |
| `VGS-`  | VolumeGroupSnapshot       | mock control plane for unit and integration, Kubernetes ≥ 1.32 for e2e            |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when nothing covers the scenario yet.

---

## 1. Unit Tests — CSI driver (`csi-driver/pkg`)

Mock `ClusterAPI` and mock HTTP. No cluster, no host commands.

### StorageClass parsing and `CreateVolume` planning (design §2.2, §4)

| #     | Scenario                                                                                       | Type     | Test |
|-------|------------------------------------------------------------------------------------------------|----------|------|
| SU-01 | `stripe_count` absent defaults to `1` and takes the base design's single-volume path            | Positive | —    |
| SU-02 | `stripe_count = 4` plans four lvols of `ceil(S/4)`, each GiB-aligned                            | Positive | —    |
| SU-03 | `S` not divisible by `n` rounds up per member, so total capacity is at least `S`                | Boundary | —    |
| SU-04 | `stripe_count` above the eligible node count is rejected with `InvalidArgument`                 | Negative | —    |
| SU-05 | `stripe_count = 0` or negative is rejected                                                      | Negative | —    |
| SU-06 | `stripe_count > 1` on an RWO volume is rejected                                                 | Negative | —    |
| SU-07 | Members are recorded on the CR with lvol id, NGUID, and size, in stable order                   | Positive | —    |
| SU-08 | VG and LV names derive from the volume name alone, with no host or random component             | Positive | —    |
| SU-09 | Members landing on fewer than `n` distinct nodes fails provisioning rather than degrading        | Negative | —    |

### Snapshot planning (design §3.3)

| #     | Scenario                                                                                | Type     | Test |
|-------|-----------------------------------------------------------------------------------------|----------|------|
| SU-10 | `CreateSnapshot` on a `stripe_count = 1` volume still uses the per-lvol path             | Positive | —    |
| SU-11 | `CreateSnapshot` on a striped volume issues exactly one group call for all members       | Positive | —    |
| SU-12 | Snapshot of a striped volume with no group API available is refused with `FailedPrecondition` | Negative | —    |
| SU-13 | Snapshot id encodes the group as `{clusterID}:{poolID}:{groupSnapUUID}`                   | Positive | —    |
| SU-14 | Resize plans a uniform per-member grow, never an uneven one                               | Boundary | —    |

---

## 2. Unit Tests — operator (`operator/internal`)

| #     | Scenario                                                                                          | Type     | Test |
|-------|---------------------------------------------------------------------------------------------------|----------|------|
| SO-01 | `NFSExport` status carrying `stripeCount` and `members[]` round-trips through the API              | Positive | —    |
| SO-02 | An export written before striping (single `lvolID`) is rewritten in place to a one-member `members[]` | Regression | —  |
| SO-03 | Failover reproduces `vgName` and `lvName` from the CR rather than recomputing them                  | Positive | —    |
| SO-04 | Failover with a member missing leaves the export `Degraded` and mounts nowhere                      | Negative | —    |
| SO-05 | The no-dual-mount invariant holds when reassembly fails halfway on the target host                   | Negative | —    |

---

## 3. Integration Tests

Driver plus MDS-host csi-node against a mock control plane, with host commands
stubbed. The step sequence and its idempotency are what break here.

| #     | Scenario                                                                                     | Type     | Test |
|-------|----------------------------------------------------------------------------------------------|----------|------|
| SI-01 | Assembly order is attach all `n`, `pvcreate`, `vgcreate`, `lvcreate`, `mkfs.xfs`, mount, export | Positive | —  |
| SI-02 | Re-running `CreateExport` on a fully assembled export changes nothing                          | Positive | —    |
| SI-03 | Re-running after a failure between `vgcreate` and `lvcreate` completes without duplicating     | Positive | —    |
| SI-04 | `CreateExport` waits for all `n` devices and does not assemble a short stripe                  | Negative | —    |
| SI-05 | Device path churn between reconnects does not break assembly, because LVM matches PV metadata  | Positive | —    |
| SI-06 | `DeleteExport` tears down in reverse: unexport, unmount, `lvremove`, `vgremove`, `pvremove`, release | Positive | — |
| SI-07 | `mkfs.xfs` is skipped when `blkid` already reports XFS on the logical volume                    | Boundary | —    |
| SI-08 | Snapshot freezes, calls the group API once, and thaws even when the call fails                   | Negative | —    |

---

## 4. End-to-End Tests (live cluster, kernel ≥ 6.11, three or more storage nodes)

| #     | Scenario                                                                                            | Type     | Test |
|-------|-----------------------------------------------------------------------------------------------------|----------|------|
| SE-01 | Provision a striped RWX PVC with `n = 3`, mount it on three nodes, read and write shared data         | Positive | —    |
| SE-02 | Members verifiably landed on three distinct storage nodes                                             | Positive | —    |
| SE-03 | Aggregate sequential throughput exceeds a single-member export by a meaningful margin                  | Positive | —    |
| SE-04 | The direct block path is in use, confirmed by per-namespace counters rather than by absence of errors  | Positive | —    |
| SE-05 | Snapshot a striped volume, restore it, mount the restore, and `fsck` clean                             | Positive | —    |
| SE-06 | Restore a snapshot taken under sustained write load, then verify a database checkpoint replays          | Positive | —    |
| SE-07 | Online resize grows every member, then the LV, then the filesystem, with data intact                    | Positive | —    |
| SE-08 | Clone a striped volume and confirm the clone is independent, with its own export, Service, and `fsid`   | Positive | —    |
| SE-09 | Provision with `n` greater than the available nodes and confirm a clean failure, not a degenerate stripe | Negative | —   |

---

## 5. Failure Injection

| #     | Scenario                                                                                           | Type     | Test |
|-------|----------------------------------------------------------------------------------------------------|----------|------|
| SF-01 | Kill one member's NVMe-oF path and confirm the export degrades with a diagnosable event             | Negative | —    |
| SF-02 | Kill the MDS host and confirm failover reassembles the stripe with the same `fsid` and file handles  | Positive | —    |
| SF-03 | Measure the failover freeze with a stripe and compare it against the base design's NFR-2 bound       | Boundary | —    |
| SF-04 | Fail the group snapshot midway and confirm no partial group is left presented as usable              | Negative | —    |
| SF-05 | Partition the old MDS during failover and confirm PR fencing prevents a second mount                 | Negative | —    |
| SF-06 | Interrupt a resize between member grow and `lvextend`, then confirm the retry converges              | Negative | —    |

---

## 6. VolumeGroupSnapshot and Group Controller (design §5)

Exercise the CSI GroupController service and the upstream feature. Unit and
integration use a mock control plane. End-to-end needs Kubernetes ≥ 1.32 with the
beta feature enabled.

| #      | Scenario                                                                                                                       | Type     | Test |
|--------|--------------------------------------------------------------------------------------------------------------------------------|----------|------|
| VGS-01 | `CreateVolumeGroupSnapshot` flattens source volumes, including each striped volume's `n` members, into one backend group call   | Positive | —    |
| VGS-02 | A mixed group of one RWO and one striped RWX volume produces a single atomic group request                                      | Positive | —    |
| VGS-03 | Idempotent by group name under sidecar retry, returning the same group id and creating no duplicate                             | Positive | —    |
| VGS-04 | Sources spanning two clusters or pools are rejected with `InvalidArgument`                                                      | Negative | —    |
| VGS-05 | `DeleteVolumeGroupSnapshot` removes the backend group and every member snapshot                                                 | Positive | —    |
| VGS-06 | `GetVolumeGroupSnapshot` surfaces a partial member failure rather than hiding it                                                | Boundary | —    |
| VGS-07 | E2E: label two or more PVCs, take a group snapshot, and confirm member `VolumeSnapshot`s appear and are mutually consistent      | Positive | —    |
| VGS-08 | E2E: restore each member into a new PVC, confirming RWX members get fresh exports and data matches                              | Positive | —    |
| VGS-09 | E2E: on Kubernetes below 1.32, the group feature is unavailable while single-PVC snapshots still work                            | Negative | —    |
| VGS-10 | Sanity: identity reports `GROUP_CONTROLLER_SERVICE` and the group controller reports its capability                              | Positive | —    |

---

## 7. Axis Coverage and Gaps

| Axis                          | Values and the scenarios covering them                                                                |
|-------------------------------|-------------------------------------------------------------------------------------------------------|
| Stripe width                  | `n = 1` (SU-01, SU-10), `n = 3` (SE-01), `n` above capacity (SU-04, SE-09)                              |
| Node count                    | three or more (SE-01, SE-02), fewer than `n` (SU-09, SE-09)                                            |
| Namespace scope               | single namespace throughout. **Multi-namespace is a gap**, and it is where export paths and `fsid`s collide |
| Cluster scope                 | single cluster (all). Cross-cluster is out of scope by design (§1.2)                                    |
| Snapshot consistency          | idle (SE-05), under load (SE-06), failed midway (SF-04)                                                |
| Failover                      | planned (SF-02), partitioned (SF-05), unreassemblable (SO-04, SO-05)                                    |

**Not covered yet.** Every scenario above, because nothing is implemented. Beyond
that, three gaps are deliberate and worth naming rather than discovering later:

- **Multi-namespace striping.** No scenario places two striped exports from different namespaces on one MDS host, which is exactly where the per-host `fsid` allocator is under pressure. This mirrors the same gap in the base plan.
- **Stripe size tuning.** No scenario varies `--stripesize`, because the design has no answer for what value is right (design S2).
- **Reshaping `n`.** Nothing tests growing a stripe's width, because the design forbids it (design §1.2, S5). If S5 resolves the other way, this becomes a gap rather than an exclusion.
