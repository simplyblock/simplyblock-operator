# Test Plan: Client-Side Compression and Deduplication

Related design: [`designs/design-issue-277-client-side-compression.md`](../designs/design-issue-277-client-side-compression.md)

Scope: the operator, the CSI driver, and the `atlas-lib` primitives underneath
them. Control-plane (`sbcli`) and SPDK behavior is a dependency, faked at the
boundary, and the design's Phase 0 table lists the dependencies this plan does
not test.

Scenario IDs are permanent: `U-` unit (no cluster, a fake LVM command runner or
a fake Kubernetes client), `I-` integration (the operator's reconcile loop
against `envtest` and a mock backend), `E-` end-to-end (live cluster, real
NVMe-oF lvol, real data path), and `M-` manual (needs failure injection or
orchestration that is not automated). Types are `Positive`, `Negative`,
`Boundary`, and `Regression`.

The `Test` column names the implementing function, or `—` when nothing covers
the scenario yet. An `M-` ID there means the scenario has been executed by hand,
with the procedure and the evidence in §4, and has no automated coverage. Every
`—` reappears in §6 What Is Not Yet Covered.

Two notes on where the code lives. The `atlas-lib` packages and their tests are
merged, so every `atlas-lib` test named below runs today. The CSI-side and
operator-side wiring, and the tests in `csi-driver/pkg/spdk`, land with branch
`issue-277-client-side-compression-impl`, and the live evidence in §4 was
gathered against that branch on the `config-israel` cluster.

---

## 1. Unit Tests

No cluster and no `lvm2` binary. The `atlas-lib` groups drive
`lvm.NewManagerWithRunner` with a fake runner that records every command line
and answers from a script, so each row asserts the exact command sequence,
scope, and order that a wrong implementation would get wrong only on a live node
with `dm-vdo` loaded. The CSI-side groups use a fake `kubernetes.Interface`.
Numbering runs continuously across the groups.

### Device Scoping and Command Construction (design §7.3)

File: `atlas-lib/lvm/lvm_test.go`

| #    | Scenario                                                                          | Type     | Test                                             |
|------|-----------------------------------------------------------------------------------|----------|--------------------------------------------------|
| U-01 | One device renders as `--devices <path>`                                          | Positive | `TestDeviceScope`                                |
| U-02 | Several devices are comma-joined into one `--devices` value                       | Positive | `TestDeviceScope`                                |
| U-03 | An empty device set contributes no flags, and the command runs unscoped           | Boundary | `TestDeviceScope`                                |
| U-04 | The scope is inserted directly after the binary name, where LVM requires it       | Positive | `TestManager_exec_InsertsDeviceScopeAfterBinary` |
| U-05 | A manager with no devices runs unscoped rather than emitting an empty `--devices` | Boundary | `TestManager_exec_NoDevicesRunsUnscoped`         |
| U-06 | `Run`, the deliberate escape hatch, is always unscoped                            | Positive | `TestManager_Run_IsUnscoped`                     |
| U-07 | `Run` with no command name is rejected                                            | Negative | `TestManager_Run_RequiresACommandName`           |

### Content-Based Identity (design §7.3)

File: `atlas-lib/lvm/identity_test.go`

| #    | Scenario                                                                                                     | Type     | Test                                                   |
|------|--------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-08 | `VolumeGroup` reads the owning volume group from the device's own content                                    | Positive | `TestManager_VolumeGroup`                              |
| U-09 | A blank device belongs to no volume group, and is not an error                                               | Boundary | `TestManager_VolumeGroup`                              |
| U-10 | `WARNING: … is duplicate for PVID …` ahead of the real field is skipped, which is the HA duplicate-PV output | Negative | `TestManager_VolumeGroup`                              |
| U-11 | A device with no PV signature reports no volume group rather than failing                                    | Negative | `TestManager_VolumeGroup`                              |
| U-12 | A real probe failure is propagated, never misread as "no volume group"                                       | Negative | `TestManager_VolumeGroup_PropagatesRealProbeError`     |
| U-13 | `ListLogicalVolumes` parses the LV names out of `lvs` output                                                 | Positive | `TestManager_ListLogicalVolumes`                       |
| U-14 | An `lvs` failure is propagated                                                                               | Negative | `TestManager_ListLogicalVolumes_PropagatesRunnerError` |
| U-15 | `HasLogicalVolume` finds the LV among others in the volume group                                             | Positive | `TestManager_HasLogicalVolume`                         |
| U-16 | Zero LVs reports absent, which is the orphaned-volume-group signal design §7.3 relies on                     | Boundary | `TestManager_HasLogicalVolume`                         |
| U-17 | The named LV absent while other LVs exist reports absent                                                     | Negative | `TestManager_HasLogicalVolume`                         |
| U-18 | An `lvs` failure is not misread as absent, which would trigger a destructive `lvcreate` over live data       | Negative | `TestManager_HasLogicalVolume_PropagatesRunnerError`   |
| U-19 | `Rescan` refreshes the LVM cache for the given physical volumes                                              | Positive | `TestManager_Rescan`                                   |
| U-20 | A rescan failure is propagated                                                                               | Negative | `TestManager_Rescan_PropagatesRunnerError`             |

### Stack Construction Primitives (design §7.2)

File: `atlas-lib/lvm/volume_test.go`

| #    | Scenario                                                                                             | Type     | Test                                                                 |
|------|------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------|
| U-21 | `CreatePhysicalVolume` scopes `pvcreate` to the one intended device                                  | Positive | `TestManager_CreatePhysicalVolume`                                   |
| U-22 | A `pvcreate` failure is wrapped                                                                      | Negative | `TestManager_CreatePhysicalVolume_WrapsRunnerError`                  |
| U-23 | `CreateVolumeGroup` on a single device                                                               | Positive | `TestManager_CreateVolumeGroup`                                      |
| U-24 | `CreateVolumeGroup` across several devices, for a striped volume group                               | Positive | `TestManager_CreateVolumeGroup`                                      |
| U-25 | `ActivateVolumeGroup` issues `vgchange -ay`                                                          | Positive | `TestManager_ActivateVolumeGroup`                                    |
| U-26 | `DeactivateVolumeGroup` issues `vgchange -an`                                                        | Positive | `TestManager_DeactivateVolumeGroup`                                  |
| U-27 | A deactivation failure is wrapped                                                                    | Negative | `TestManager_DeactivateVolumeGroup_WrapsRunnerError`                 |
| U-28 | `RemoveVolumeGroup` removes the volume group                                                         | Positive | `TestManager_RemoveVolumeGroup`                                      |
| U-29 | A removal failure is wrapped                                                                         | Negative | `TestManager_RemoveVolumeGroup_WrapsRunnerError`                     |
| U-30 | `CreateLogicalVolume` dispatches by asking each registered handler whether it handles the definition | Positive | `TestManager_CreateLogicalVolume_DispatchesByHandles`                |
| U-31 | No handler matches, so no segment-type arguments are contributed                                     | Boundary | `TestManager_CreateLogicalVolume_NoHandlerMatchesContributesNothing` |

### VDO Feature Arguments (design §6)

File: `atlas-lib/lvm/vdo/volume_test.go`

| #    | Scenario                                                                                                     | Type     | Test                                              |
|------|--------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------|
| U-32 | Both features on produce `--compression y --deduplication y`                                                 | Positive | `TestVolumeHandler_CreateVolumeArgs`              |
| U-33 | Compression only produces `--compression y --deduplication n`                                                | Positive | `TestVolumeHandler_CreateVolumeArgs`              |
| U-34 | Deduplication only produces `--compression n --deduplication y`                                              | Positive | `TestVolumeHandler_CreateVolumeArgs`              |
| U-35 | Neither feature contributes no arguments at all                                                              | Boundary | `TestVolumeHandler_CreateVolumeArgs`              |
| U-36 | `Handles` agrees with `CreateVolumeArgs` on all four combinations, so dispatch and arguments cannot disagree | Boundary | `TestVolumeHandler_Handles`                       |
| U-37 | The handler reaches `lvcreate` only through the registry the package's `init` populates                      | Positive | `TestRegisteredHandlerReachesCreateLogicalVolume` |
| U-38 | `UpdateVolume` toggles the features on an existing volume through `lvchange`                                 | Positive | `TestUpdateVolume`                                |
| U-39 | An `lvchange` failure is wrapped                                                                             | Negative | `TestUpdateVolume_WrapsRunnerError`               |

### VDO Stack Lifecycle (design §7.2, §7.4, §8, §9)

File: `atlas-lib/lvm/vdo/stack_test.go`

| #    | Scenario                                                                                      | Type     | Test                                                          |
|------|-----------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-40 | `DevicePath` names the logical volume to format and mount, not the pool's dm device           | Positive | `TestDevicePath`                                              |
| U-41 | Fresh device: `pvcreate`, `vgcreate`, and `lvcreate` in that order, each scoped to the device | Positive | `TestCreateOrAttach_FreshDevice`                              |
| U-42 | An existing volume group is reactivated and never recreated                                   | Positive | `TestCreateOrAttach_ExistingVolumeGroupReactivates`           |
| U-43 | An orphaned volume group with zero LVs is removed, then the stack is created fresh            | Boundary | `TestCreateOrAttach_OrphanedVolumeGroupIsRemovedAndRecreated` |
| U-44 | `Deactivate` deactivates the stack without destroying it                                      | Positive | `TestDeactivate_Success`                                      |
| U-45 | Device unreachable: `vgchange -an` fails, and the dm-node fallback runs                       | Negative | `TestDeactivate_UnreachableDeviceFallsBackToDMCleanup`        |
| U-46 | Any other deactivation error surfaces rather than being swallowed by the fallback             | Negative | `TestDeactivate_OtherErrorIsNotSwallowed`                     |
| U-47 | `Remove` deactivates and removes the stack                                                    | Positive | `TestRemove_Success`                                          |
| U-48 | `Remove` with the backing device already gone falls back to dm-node removal                   | Negative | `TestRemove_UnreachableDeviceFallsBackToDMCleanup`            |
| U-49 | `Grow` extends the pool LV, then the VDO LV, then returns the device path                     | Positive | `TestGrow`                                                    |
| U-50 | `ResolveClone` re-stamps a foreign volume-group identity before any activation                | Positive | `TestResolveClone_ForeignVolumeGroupIsReStamped`              |
| U-51 | A device already carrying this volume's own identity is a no-op                               | Negative | `TestResolveClone_OwnIdentityIsANoOp`                         |
| U-52 | `SetFeatures` toggles compression and deduplication on an active volume                       | Positive | `TestSetFeatures`                                             |

### Clone Identity Resolution (design §7.4)

File: `atlas-lib/lvm/clone_test.go`

| #    | Scenario                                                                                                                      | Type     | Test                                                            |
|------|-------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------|
| U-53 | `ImportClonedVolumeGroup` regenerates the PV and VG UUIDs and renames the volume group                                        | Positive | `TestManager_ImportClonedVolumeGroup`                           |
| U-54 | An import failure is wrapped                                                                                                  | Negative | `TestManager_ImportClonedVolumeGroup_WrapsRunnerError`          |
| U-55 | `RenameLogicalVolume` renames the volume's own LV                                                                             | Positive | `TestManager_RenameLogicalVolume`                               |
| U-56 | A foreign identity is resolved in the right order: rescan, probe, import, rename                                              | Positive | `TestManager_ResolveClonedVolumeGroup_ResolvesAForeignIdentity` |
| U-57 | A device carrying this volume's own identity, and a blank device, are both left alone                                         | Negative | `TestManager_ResolveClonedVolumeGroup_NoOps`                    |
| U-58 | VDO's structural pool LV, named identically in every stack, survives the rename                                               | Boundary | `TestManager_ResolveClonedVolumeGroup_PreservesStructuralLVs`   |
| U-59 | A failed cache rescan is not fatal, because the content probe reads the device directly                                       | Negative | `TestManager_ResolveClonedVolumeGroup_SurvivesAFailedRescan`    |
| U-60 | A probe failure is wrapped, never read as evidence that the device is no clone, which would activate a colliding volume group | Negative | `TestManager_ResolveClonedVolumeGroup_WrapsAProbeFailure`       |

### Growth Primitives (design §9)

File: `atlas-lib/lvm/grow_test.go`

| #    | Scenario                                                                                       | Type     | Test                                                     |
|------|------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| U-61 | `ExpandPhysicalVolume` runs `pvresize` scoped to the device                                    | Positive | `TestManager_ExpandPhysicalVolume`                       |
| U-62 | `ExtendVolumeGroup` adds newly available physical space                                        | Positive | `TestManager_ExtendVolumeGroup`                          |
| U-63 | An extend failure is wrapped                                                                   | Negative | `TestManager_ExtendVolumeGroup_WrapsRunnerError`         |
| U-64 | `ExpandLogicalVolume` consumes all free space, matching the `100%FREE` creation convention     | Positive | `TestManager_ExpandLogicalVolume`                        |
| U-65 | `LogicalVolumeSize` parses the reported size                                                   | Positive | `TestManager_LogicalVolumeSize`                          |
| U-66 | Unparsable size output is an error rather than a zero                                          | Negative | `TestManager_LogicalVolumeSize_UnparsableOutput`         |
| U-67 | `ExtendLogicalVolumeToSize` extends to an exact size                                           | Positive | `TestManager_ExtendLogicalVolumeToSize`                  |
| U-68 | An extend-to-size failure is wrapped                                                           | Negative | `TestManager_ExtendLogicalVolumeToSize_WrapsRunnerError` |
| U-69 | A target size at or below the current size is refused, since VDO and LVM have no online shrink | Boundary | —                                                        |

### Orphaned dm Node Cleanup (design §8)

File: `atlas-lib/lvm/dm_test.go`

| #    | Scenario                                                                                | Type     | Test                                |
|------|-----------------------------------------------------------------------------------------|----------|-------------------------------------|
| U-70 | `escapeDMName` doubles every dash, matching how `dmsetup ls` spells a volume-group name | Positive | `TestEscapeDMName`                  |
| U-71 | No matching dm nodes is a clean no-op                                                   | Boundary | `TestManager_RemoveOrphanedDMNodes` |
| U-72 | Matching escaped names are removed, and an unrelated dm node such as `rl-root` is not   | Positive | `TestManager_RemoveOrphanedDMNodes` |
| U-73 | A node still blocked by a live dependency on the first pass clears on a later one       | Negative | `TestManager_RemoveOrphanedDMNodes` |
| U-74 | `dmsetup ls` failing is propagated rather than reported as a successful cleanup         | Negative | `TestManager_RemoveOrphanedDMNodes` |

### Node Capability Advertisement (design §4.3)

File: `csi-driver/pkg/spdk/nodeserver_vdo_capability_test.go`

| #    | Scenario                                                                                          | Type     | Test                                                  |
|------|---------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------|
| U-75 | No label at all: the node is free for the auto-detect probe to claim                              | Positive | `TestVDOCapableOperatorManaged`                       |
| U-76 | A label carrying the `auto-detect` annotation stays the probe's to manage                         | Positive | `TestVDOCapableOperatorManaged`                       |
| U-77 | A label set by hand, with no `managed-by` annotation, is an operator override                     | Negative | `TestVDOCapableOperatorManaged`                       |
| U-78 | A label present with an unrelated annotation value is still an override                           | Boundary | `TestVDOCapableOperatorManaged`                       |
| U-79 | The probe leaves an operator-set label untouched across a `csi-node` restart                      | Negative | `TestAdvertiseVDOCapability_RespectsOperatorOverride` |
| U-80 | The marker file never appears within the wait: the node is treated as not capable, not as unknown | Negative | —                                                     |

### Topology Segment (design §5)

File: `csi-driver/pkg/spdk/controllerserver_test.go`

| #    | Scenario                                                                                           | Type     | Test                    |
|------|----------------------------------------------------------------------------------------------------|----------|-------------------------|
| U-81 | Neither parameter set: no `vdo-capable` segment is added                                           | Negative | `TestVDOCapableSegment` |
| U-82 | `client_compression` true: the segment is present                                                  | Positive | `TestVDOCapableSegment` |
| U-83 | `client_deduplication` alone is enough, since a deduplication-only volume needs the module too     | Positive | `TestVDOCapableSegment` |
| U-84 | Both parameters spelled `"False"`: no segment                                                      | Boundary | `TestVDOCapableSegment` |
| U-85 | The segment ignores `AccessibilityRequirements`, including one that advertises `vdo-capable=false` | Negative | `TestVDOCapableSegment` |
| U-86 | `vdoParams` accepts both the `"True"` that `boolStr` emits and a lowercase `"true"`                | Boundary | —                       |

### StorageClass Parameter Generation (design §6)

File: `operator/internal/controller/simplyblockstoragepool_controller_unit_test.go`

| #    | Scenario                                                                                                            | Type       | Test |
|------|---------------------------------------------------------------------------------------------------------------------|------------|------|
| U-87 | `clientCompression` true and `clientDeduplication` false generate `"True"` and `"False"`                            | Positive   | —    |
| U-88 | The inverse combination generates the inverse parameters                                                            | Positive   | —    |
| U-89 | Both true generate both parameters as `"True"`                                                                      | Positive   | —    |
| U-90 | Both unset default to `false`, and VDO is never attempted                                                           | Boundary   | —    |
| U-91 | Either parameter true adds the `vdo-capable=true` segment to `allowedTopologies`                                    | Positive   | —    |
| U-92 | Neither parameter true leaves `allowedTopologies` unconstrained by `vdo-capable`                                    | Negative   | —    |
| U-93 | DHCHAP's own `allowedTopologies` gate on the same pool composes as AND, and neither constraint overwrites the other | Regression | —    |

### `nodeserver.go` Wiring (design §7.6)

File: `csi-driver/pkg/spdk/nodeserver_test.go`

| #     | Scenario                                                                                                                                          | Type     | Test |
|-------|---------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-94  | Either parameter true: `CreateOrAttachVDO` runs between `initiator.Connect` and `stageVolume`, and its device path is what `stageVolume` receives | Positive | —    |
| U-95  | Neither parameter true: `CreateOrAttachVDO` is never called, and the raw device path is used unchanged                                            | Negative | —    |
| U-96  | `ResolveClonedVDO` runs before the volume-group check on every stage, not only when a `VolumeContentSource` is set                                | Positive | —    |
| U-97  | `NodeUnstageVolume` calls `DeactivateVDO` before `initiator.Disconnect`, with the order asserted                                                  | Positive | —    |
| U-98  | A `DeactivateVDO` failure stops the unstage before the raw device is disconnected                                                                 | Negative | —    |
| U-99  | The reconnect path reattaches the existing stack and never issues `lvcreate`                                                                      | Positive | —    |
| U-100 | The reconnect path finds a blank device where VDO was expected, and fails loudly instead of creating a fresh container                            | Negative | —    |
| U-101 | `NodeExpandVolume` calls `GrowVDO` before the filesystem resize                                                                                   | Positive | —    |
| U-102 | `GrowVDO` failing skips the filesystem resize against a device that did not grow                                                                  | Negative | —    |
| U-103 | `stageVolume` skips `xfsStripeOptions` whenever VDO is in play                                                                                    | Positive | —    |
| U-104 | An `ext4` volume is unaffected, because the stripe-option path never applied to it                                                                | Negative | —    |

---

## 2. Integration Tests

Run the operator's reconcile loop against a real Kubernetes API through
`envtest` and a mock backend HTTP server. Only the operator-side half of this
feature has an `envtest`-shaped surface: the node plugin needs a real node with
`dm-vdo`, so its live coverage is §3 and §4.

### Pool to StorageClass Reconciliation (design §6)

| #    | Scenario                                                                                                                 | Type       | Test |
|------|--------------------------------------------------------------------------------------------------------------------------|------------|------|
| I-01 | A pool with either parameter reconciles to a StorageClass carrying both parameters and the `vdo-capable` topology term   | Positive   | —    |
| I-02 | Flipping a parameter on an existing pool updates the StorageClass in place, and leaves already-provisioned volumes alone | Positive   | —    |
| I-03 | Two pools of the same name in different namespaces produce distinct, DNS-valid StorageClass names                        | Positive   | —    |
| I-04 | A pool with both DHCHAP and client compression composes both topology constraints                                        | Regression | —    |
| I-05 | The backend rejects pool creation: no StorageClass carrying the client parameters is left behind                         | Negative   | —    |

---

## 3. E2E Tests

Live cluster, real NVMe-oF-backed lvols, real `dm-vdo` on the consumer node.
Every row that touches the data path asserts checksum equality rather than
merely that I/O continued. An `M-` ID in the `Test` column points at the manual
procedure in §4 that has been executed against branch
`issue-277-client-side-compression-impl` on `config-israel`.

### Provisioning and Data Reduction (design §6, §7.2)

| #    | Scenario                                                                                            | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Pool with both parameters: PVC, pod, stack created with both features on, XFS mounted               | Positive | M-01 |
| E-02 | Compression-only and deduplication-only pools, independently toggled, both mount                    | Positive | M-01 |
| E-03 | Compressible and duplicate data produce measurable savings in `vdostats`                            | Positive | M-02 |
| E-04 | A volume below VDO's floor fails the stage with the minimum-size error, and leaves no partial stack | Boundary | M-15 |

### Reattachment and Scheduling (design §5, §7.6)

| #    | Scenario                                                                                                          | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------|----------|------|
| E-05 | Pod deleted and recreated on the same node: the stack is reattached, never recreated, and the checksum matches    | Positive | M-03 |
| E-06 | Pod reschedules onto a different capable node: the reactivate path runs there, and the PV's `nodeAffinity` holds  | Positive | M-04 |
| E-07 | A node that is not `vdo-capable` is never selected for a compression-requesting PVC                               | Negative | M-04 |
| E-08 | An operator-set `vdo-capable` label survives a `csi-node` restart and reaches the `NodeGetInfo` topology response | Positive | M-05 |
| E-09 | Every node in the cluster lacks capability: the PVC stays `Pending` with a clear reason, and nothing stages       | Negative | —    |

### Clone, Snapshot, and Expansion (design §7.4, §9)

| #    | Scenario                                                                                                       | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------|----------|------|
| E-10 | Direct PVC clone scheduled onto the same node as its still-live source                                         | Positive | M-06 |
| E-11 | Snapshot restore scheduled onto the same node as its still-live source                                         | Positive | M-06 |
| E-12 | Source, clone, and restore coexisting on one node with independent identities                                  | Positive | M-06 |
| E-13 | Two clones of one source staged onto the same node in the same batch, without racing each other's rename       | Negative | —    |
| E-14 | Online expansion: pool grows, logical volume grows, filesystem resizes, checksum survives, new space is usable | Positive | M-07 |
| E-15 | Expansion starting exactly at VDO's minimum-size floor                                                         | Boundary | —    |

### Filesystem and Failure Handling (design §7.6, §8)

| #    | Scenario                                                                                                              | Type       | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|------------|------|
| E-16 | XFS on a VDO device: `mkfs.xfs` runs with no stripe-alignment flags, mounts with `nouuid`, and data round-trips       | Positive   | M-08 |
| E-17 | Several stacks on one node survive a node reboot, and every checksum matches                                          | Positive   | M-09 |
| E-18 | Backing device force-disconnected while the node stays up: cleanup is automatic on the next unstage                   | Negative   | M-10 |
| E-19 | Node goes `NotReady` and later rejoins: every stale stack is torn down, and the replacement pod runs                  | Negative   | M-11 |
| E-20 | Unclean node crash under the `async` write policy: an `fsync()`'d write survives, and an unsynced one is lost         | Negative   | M-12 |
| E-21 | Physical pool exhausted by incompressible data: a clean `ENOSPC` reaches the workload, with no hang and no corruption | Negative   | M-13 |
| E-22 | HA duplicate-PV ambiguity between a volume's two paths: scoped commands stage both pools                              | Regression | M-14 |
| E-23 | A host whose `system.devices` restricts LVM visibility: the content-based existence check answers correctly           | Regression | M-14 |
| E-24 | An orphaned volume group from an interrupted create is removed and recreated rather than reactivated forever          | Regression | M-14 |
| E-25 | The node's `kvdo` module becomes unloadable after a volume is bound there: the next stage fails loudly                | Negative   | —    |
| E-26 | Node or pod dies during the initial `lvcreate`, leaving a partial stack                                               | Negative   | —    |
| E-27 | Genuinely overlapping `vgchange -ay` and `pvscan --cache` calls racing at the LVM level                               | Negative   | —    |
| E-28 | Boot-time activation race across several volume groups on one node                                                    | Negative   | —    |
| E-29 | Graceful reboot teardown, as distinct from the unclean crash in E-20                                                  | Positive   | —    |
| E-30 | `system.devices` entries pruned rather than accumulating across repeated node-failure cycles                          | Negative   | —    |

### Compatibility and Scale (design §12)

| #    | Scenario                                                                                                                                           | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-31 | `VolumeMigration` moves a VDO-backed volume's storage node: the client-side stack is untouched, the pod does not restart, and the checksum matches | Positive | M-16 |
| E-32 | Server-side `encryption=true` with `client_compression=true`: the combination provisions and compresses                                            | Positive | M-17 |
| E-33 | Server-side `compression=true` with `client_compression=true`: redundant, correct, and only wasteful of CPU                                        | Negative | —    |
| E-34 | The placement injector co-locates a workload with a non-capable storage node: no conflict with the topology gate                                   | Negative | —    |
| E-35 | Ten or more VDO-backed PVCs provisioned concurrently on one node                                                                                   | Boundary | —    |
| E-36 | Sustained high-throughput writes to a VDO volume, with CPU and memory overhead measured                                                            | Positive | —    |
| E-37 | A large volume with a realistic mix of compressible, duplicate, and random data                                                                    | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

Every block below has been executed by hand against branch
`issue-277-client-side-compression-impl` on the `config-israel` cluster, and
each records what it proved. None is automated, so each one is also a
specification for the E2E case that should replace it.

### M-01: A pool's two parameters reach a real VDO device

**Design reference:** §6, §7.2

**What to verify:** the two parameters are independent all the way down, so
`lvcreate` receives exactly the combination the pool declared.

**Test concept**, in order:

1. Create three pools: compression and deduplication both on, compression only,
   and deduplication only.
2. Provision a PVC and pod from each, and read the CSI node log.
3. Assert the `lvcreate` line carries the matching `--compression` and
   `--deduplication` values, and that each pod mounts a real
   `/dev/mapper/vdo--…` device.

**Verified:** all three pools mounted. The log shows
`--compression y --deduplication y`, `--compression y --deduplication n`, and
`--compression n --deduplication y` respectively, each followed by a successful
mount. The deduplication-only pool needed a node with enough free VDO memory,
the per-node ceiling that bounds E-35.

### M-02: Data reduction on compressible and duplicate data

**Design reference:** §6

**What to verify:** the layer actually reduces data, and the reduction is
visible in `vdostats` rather than inferred.

**Test concept**, in order:

1. Write one original file and two exact duplicates into a VDO-backed volume.
2. Read `vdostats` for the pool's data-block accounting.

**Verified:** roughly 104MB of input reported about 89% savings. Only small,
synthetic, highly duplicate datasets have been used, which is why E-36 and E-37
remain open.

### M-03: Reattachment across a pod delete and recreate

**Design reference:** §7.6

**What to verify:** the reattach path is taken and the reformat path is not, so
the data survives a routine pod recreate on the same node.

**Test concept**, in order:

1. Write checksummed data into a VDO-backed volume.
2. Delete the pod and let it be recreated on the same node.
3. Assert the CSI log shows `lvs` then `vgchange -ay`, with no `lvcreate`, and
   that the checksum matches.

**Verified:** checksum matched, and the reactivate path was taken.

### M-04: Reschedule onto a different capable node

**Design reference:** §5

**What to verify:** the topology gate keeps a compression-requesting volume on
capable nodes for the volume's whole life, not only at first binding, and that a
node which has never hosted the stack can adopt it.

**Test concept**, in order:

1. Cordon the pod's original node and delete the pod.
2. Assert the scheduler excludes the non-capable node and picks a capable one
   that has never hosted this volume's stack.
3. Assert the reactivate path runs there, the restart count stays `0`, and the
   checksum matches.

**Verified:** the non-capable control-plane node was excluded, and the pod landed
on a capable node that had never hosted the stack. The CSI log there shows `lvs`
detecting the existing LV and then `vgchange --devices … -ay`, with no
`pvcreate`, `vgcreate`, or `lvcreate` at all, followed by a successful XFS mount.

### M-05: An operator-set capability label survives the probe

**Design reference:** §4.3

**What to verify:** the escape hatch holds. A label set by hand is not
overwritten by the auto-detect probe on the next DaemonSet restart, and the
hand-set value is what the CSI topology response carries.

**Test concept**, in order:

1. Strip the label and let auto-detect claim it, which stamps `true` and the
   `managed-by: auto-detect` annotation.
2. Set the label to `false` by hand and remove the annotation.
3. Restart the `csi-node` pod, then read the label and the `NodeGetInfo`
   topology response.

**Verified:** the label stayed `false`, the log recorded that the node has an
operator-set label and was left alone, the annotation stayed absent, and the
value fed through into the topology response. U-79 covers the same decision as a
unit test.

### M-06: Clone and restore beside a live source

**Design reference:** §7.4

**What to verify:** a byte-identical copy is given its own LVM identity before
activation, so neither the clone nor its source is damaged, and three
identities coexist on one node.

**Test concept**, in order:

1. Create a VDO-backed source volume with checksummed data.
2. Create a PVC clone and a VolumeSnapshot restore from it, both scheduled onto
   the source's node while the source stays live.
3. Assert `vgimportclone` and `lvrename` ran, each volume mounts, each checksum
   matches the source, and writing into one leaves the other two unchanged.

**Verified:** both paths resolved, both mounted, and all three volumes held
independent volume-group identities with no cross-contamination.

### M-07: Online expansion

**Design reference:** §9

**What to verify:** the physical extend, the logical extend, and the filesystem
resize all succeed while the filesystem stays mounted, and data written into
the new space reads back intact.

**Test concept**, in order:

1. Grow the backing device.
2. Run `pvresize`, `lvextend` against the pool LV, `lvextend` against the VDO
   LV, and the online filesystem resize, in that order.
3. Assert a canary file's checksum is unchanged, and write incompressible data
   into the newly available space.

**Verified:** the pool grew from 5.5G to 7.5G and the VDO LV from 4G to 6G, with
the filesystem mounted throughout. Usable space went from 3.9G to 5.9G, the
canary checksum was unchanged, and 500MB of incompressible data wrote
successfully into the new space. The pool's `Data%` fell from 63.68% to 46.71%,
which matches the design's statement that VDO's overhead is roughly fixed in
absolute terms. Growth from exactly at the floor is E-15 and untested.

### M-08: XFS on a VDO device

**Design reference:** §7.6

**What to verify:** stripe alignment computed for the erasure-coded raw device
is not applied to a VDO virtual device.

**Test concept**, in order:

1. Provision a VDO-backed volume with `fsType: xfs`.
2. Assert the `mkfs.xfs` command line carries only `-f <device>`, with no
   `xfs_su` or `xfs_sw` hints, and that the mount keeps `nouuid`.
3. Round-trip data, then recreate the pod and confirm reattachment.

**Verified:** no stripe-alignment flags were passed, `nouuid` was intact, both
features stayed enabled, and the data round-tripped.

### M-09: Several stacks through a node reboot

**Design reference:** §8

**What to verify:** each stack reattaches independently after a reboot, with no
naming or dm collision, and no volume comes back read-only.

**Test concept**, in order:

1. Provision two VDO-backed PVCs on one node with distinct checksummed data.
2. Reboot the node.
3. Assert the `kvdo` usage count equals the number of stacks, that each reports
   `VDOOperatingMode: normal`, and that both checksums match.

**Verified:** both reattached cleanly, the usage count was exactly 2, both
reported normal operating mode, and both checksums matched. The two
`NodeStageVolume` sequences happened not to overlap, so genuinely concurrent LVM
locking is still E-27.

### M-10: Backing device disconnected without a clean unstage

**Design reference:** §8

**What to verify:** stale VDO and dm state is cleaned up automatically when the
device is gone and the normal LVM teardown cannot read its metadata.

**Test concept**, in order:

1. Forcibly disconnect a VDO-backed volume's NVMe-oF subsystem at host level.
2. Delete the pod, and let the unstage run.
3. Assert `dmsetup ls` shows no leftover nodes for that volume group.

**Verified:** `vgchange -an` failed with `Volume group … not found`, as expected,
and the run found two real defects: `DeactivateVDO` had no fallback, and the
fallback's device-name matching did not account for device-mapper's
dash-escaping, so it matched nothing. With both fixed, cleanup is automatic, and
a second run of the whole sequence confirmed it. E-22, E-23, and E-24 pin the
same class of defect as unit-testable behavior, covered by U-45, U-48, and
U-70 through U-74.

### M-11: Node failure and rejoin

**Design reference:** §8

**What to verify:** a node that goes `NotReady` and later rejoins tears down
every stale stack, and the replacement pod reaches `Running` with intact data.

**Test concept**, in order:

1. Stop kubelet on the node hosting several VDO-backed volumes.
2. Let the default 300s `NoExecute` toleration evict the pods.
3. Restart kubelet, and assert the stale stacks are torn down and the
   replacement pods run with matching checksums.

**Verified:** the replacement pod on another node blocked on
`FailedAttachVolume: Multi-Attach error` until the original node's kubelet
restarted, at which point the block cleared within seconds and the new pod
reached `Running` with an exact checksum match. `dmsetup ls` on the original node
showed a fully clean teardown for all three simultaneous stacks.

**Open question:** a node that never rejoins leaves the replacement pod blocked
indefinitely, because nothing applies the
`node.kubernetes.io/out-of-service` taint. Design §14's Q4 owns the decision,
and no scenario here can close it.

### M-12: Crash consistency under the `async` write policy

**Design reference:** §7.5, §8

**What to verify:** an acknowledged write survives an unclean crash, and an
unsynced write does not, which is what makes the default policy safe.

**Test concept**, in order:

1. Force `async` explicitly on a real NVMe-oF-backed lvol.
2. Write one `fsync()`'d file and one unsynced file.
3. Crash the node with `sysrq` rather than rebooting it, and confirm the crash
   through a new `who -b` boot timestamp.
4. Compare both files after the node returns.

**Verified:** the `fsync()`'d file survived with an exact checksum match, and the
unsynced file was lost entirely, which is the correct POSIX outcome and proof
that the test distinguished durable from non-durable writes. A graceful reboot
would flush both and prove nothing, which is why E-29 is a separate scenario.

### M-13: Physical pool exhaustion

**Design reference:** §8

**What to verify:** running the physical pool out of space reaches the workload
as an ordinary `ENOSPC`, with no hang, no corruption, and no silent success.

**Test concept**, in order:

1. Fill a compression-only VDO volume with incompressible data from
   `/dev/urandom` in a loop.
2. Assert the filesystem returns `No space left on device`, and correlate with
   `vdostats` reporting the physical pool genuinely full.

**Verified:** XFS returned `No space left on device` with a correctly truncated
partial write and clean failures afterward, while `vdostats` showed the physical
pool at 97% used. Real physical exhaustion, not a logical-size limit.

### M-14: Device identity on a host with HA paths and a devices file

**Design reference:** §7.3

**What to verify:** a volume's two byte-identical HA paths and a restrictive
`system.devices` file cannot make LVM answer a question about the wrong device.

**Current behavior:** all three defects this scenario found are fixed, and each
is now pinned by a unit test.

**Test concept**, in order:

1. Stage a VDO-backed volume on a host where both HA device nodes are visible.
2. Assert no duplicate-PV ambiguity appears, that the existence check answers
   from the device's content, and that an orphaned volume group is repaired.

**Verified:** an unscoped `pvscan` hit a genuine duplicate-PV ambiguity between
the two HA paths, fixed by scoping every command with `--devices`. A name-based
`vgs --devices <path> <name>` lookup then still reported a volume group as
existing where it had never been created, fixed by making the check content-based.
With both fixes in place the check correctly identified two volume groups as
orphaned leftovers from before the first fix, removed them, and recreated the
stacks, after which both volumes mounted end-to-end.

### M-15: A volume below VDO's size floor

**Design reference:** §6, §14 Q2

**What to verify:** a volume too small for a VDO container fails visibly rather
than mounting raw or leaving a partial stack.

**Test concept**, in order:

1. Provision a 2Gi PVC from a pool with either client parameter set.
2. Assert the stage fails, the error names the minimum size, and no PV, VG, or
   pool LV is left registered on the device.

**Verified:** `lvcreate` exited 5 with
`Minimum required size for VDO volume: 5063921664 bytes`, confirmed live against
a 2Gi PVC.

**Open question:** nothing pre-checks the size, so the failure arrives from LVM
rather than from admission. Design §14's Q2 owns whether the webhook should
reject it instead.

### M-16: Volume migration under a live VDO stack

**Design reference:** §12

**What to verify:** moving an lvol between storage nodes does not disturb a
client-side VDO stack, and the pod neither restarts nor loses data.

**Test concept**, in order:

1. Provision a VDO-backed volume, write checksummed data, and note the pod's
   node and restart count.
2. Migrate the backing lvol to another storage node through a `VolumeMigration`
   CR.
3. Assert the CR completes, the restart count is unchanged, the checksum
   matches, and `nvme list-subsys` on the consumer node shows only the path
   change.

**Verified:** two runs, a plain volume and a VDO-backed one, each completing in
under 30 seconds with `Phase: Completed`, restart count `0`, and matching
checksums. On the consumer node the primary path's `traddr` moved to the target
node while the HA sibling path was untouched, and the `dm-vdo` device and its
mount were unaffected. `CreateOrAttachVDO` is never re-invoked by a migration,
and a grep of `volumemigration_controller.go` confirms the controller carries no
`vdo-capable` reference, which is correct because VDO never runs on the storage
node a migration moves.

### M-17: Server-side encryption with client-side compression

**Design reference:** §12

**What to verify:** an encrypted volume reaches the client as plaintext, which
is what lets compression work on it at all.

**Test concept**, in order:

1. Confirm in `sbcli`'s `lvol_controller.py` that the crypto vbdev is layered
   below the NVMf attach point and that the namespace-add RPC exposes
   `lvol.top_bdev`.
2. Provision a volume with `encryption=true` and `client_compression=true`, and
   confirm the data reduction a plaintext workload gets.

**Verified:** the crypto bdev sits below the NVMf attach point, the DEK stays on
the storage node, and the CSI path passes only a boolean flag. The measurement
that makes this matter: the same 536MB dataset reached roughly 12.4x reduction as
plaintext, using 11,072 data blocks, and roughly 1:1 as AES-256-CTR ciphertext,
using 137,186 new data blocks for 137,173 new logical blocks. Ciphertext does
not compress, and the client never sees it.

---

## 5. Axis Coverage

| Axis                          | Values covered                                                                                  | IDs                                | Not covered                                                                                                |
|-------------------------------|-------------------------------------------------------------------------------------------------|------------------------------------|------------------------------------------------------------------------------------------------------------|
| Cluster topology              | Multi-node cluster with a mix of storage and non-storage nodes                                  | M-01, M-04, M-09                   | Single-node cluster, asymmetric node sizes                                                                 |
| Node capability mix           | All capable, and capable beside non-capable                                                     | E-06, E-07, U-81 through U-85      | No capable node in the cluster (E-09)                                                                      |
| Namespace scope               | Single namespace                                                                                | Every E row                        | Two pools of one name in two namespaces (I-03), namespace deleted mid-stage                                |
| Cluster count                 | One StorageCluster in one Kubernetes cluster                                                    | Every E row                        | Several StorageClusters, cross-cluster. The stack is node-local, so neither applies                        |
| Object scale                  | One, two, and three stacks on a node                                                            | M-01, M-06, M-09                   | Ten or more per node (E-35), 100 or more volumes                                                           |
| Lifecycle and timing          | Pod recreate, reschedule, node reboot, unclean crash, force-disconnect, node failure and rejoin | M-03, M-04, M-09, M-10, M-11, M-12 | Interrupted `lvcreate` (E-26), overlapping LVM calls (E-27), boot-time race (E-28), graceful reboot (E-29) |
| Trigger and actor             | The auto-detect probe, and an operator's hand-set label                                         | M-05, U-75 through U-79            | An administrator applying the out-of-service taint (design §14 Q4)                                         |
| Node OS, kernel, architecture | RHEL-family, `x86_64`, out-of-tree `kmod-kvdo`                                                  | Every E row                        | Kernel 6.9 or newer with in-tree `dm-vdo`, `aarch64`, non-RHEL distributions                               |
| Feature combination           | Compression only, deduplication only, both, neither                                             | U-32 through U-36, M-01            | Toggling a feature on a staged volume (design §14 Q1)                                                      |
| Data-path correctness         | Checksum equality across every live scenario that moves data                                    | M-03, M-04, M-07, M-09, M-12, M-16 | Integrity under sustained load (E-36), a large mixed dataset (E-37)                                        |
| Failure domains and placement | Consumer node distinct from the storage node hosting the lvol                                   | M-16                               | The placement injector beside the topology gate (E-34)                                                     |

---

## 6. Coverage Summary

| Class       | Scenarios | Automated | Hand-executed | Not covered |
|-------------|-----------|-----------|---------------|-------------|
| Unit        | 104       | 83        | 0             | 21          |
| Integration | 5         | 0         | 0             | 5           |
| E2E         | 37        | 0         | 23            | 14          |
| Manual      | 17        | 0         | 17            | 0           |

Every `atlas-lib` unit row is automated and merged. Every unit row that is not
covered belongs to the CSI-side and operator-side wiring, which is where the
automated coverage is thinnest and the live coverage is strongest.

---

## 7. What Is Not Yet Covered

| #                                                       | Gap                                                                | Reason                                                                                                                                                                            |
|---------------------------------------------------------|--------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-69                                                    | A target size at or below the current size is refused              | `ExtendLogicalVolumeToSize` has no guard, and the caller never requests a shrink today                                                                                            |
| U-80                                                    | Marker file absent past the wait                                   | Needs the timeout injected, which the current test setup does not expose                                                                                                          |
| U-86                                                    | `vdoParams` spelling tolerance                                     | Covered indirectly by the live runs in §4, and not by a unit test                                                                                                                 |
| U-87 … U-93                                             | StorageClass parameter and topology generation                     | The operator-side unit test file carries no case for the two new parameters                                                                                                       |
| U-94 … U-104                                            | `nodeserver.go` wiring                                             | No `nodeserver_test.go` case exercises the VDO branches. The behavior is verified live in §4, so these rows are automation debt                                                   |
| I-01 … I-05                                             | Pool-to-StorageClass reconciliation under `envtest`                | No `envtest` case covers the pool controller's client parameters yet                                                                                                              |
| E-01 … E-08, E-10 … E-12, E-14, E-16 … E-24, E-31, E-32 | The live scenarios executed by hand                                | Hand-executed and evidenced in §4, with no automated E2E suite for this feature. Automating them is the largest single piece of work                                              |
| E-09                                                    | No capable node anywhere in the cluster                            | Not attempted. The expected outcome is a `Pending` PVC, which needs a cluster with capability disabled everywhere                                                                 |
| E-13                                                    | Two clones of one source staged in the same batch                  | Not forced. The live clone runs were sequential                                                                                                                                   |
| E-15                                                    | Expansion from exactly at the size floor                           | The growth run started from an already-above-floor pool                                                                                                                           |
| E-25                                                    | `kvdo` unloadable after a volume is bound                          | The safe proxy test through an `lvmlocal.conf` activation restriction was blocked before it could run. The hard-fail path is reached by other injections, but not by this trigger |
| E-26                                                    | Interruption during the initial `lvcreate`                         | Three separate interrupt attempts at the raw LVM level failed to catch a genuinely partial state. The code path is defensive but unverified                                       |
| E-27                                                    | Genuinely overlapping `vgchange` and `pvscan`                      | The reboot run in M-09 serialized naturally, so the LVM locking path is unexercised                                                                                               |
| E-28                                                    | Boot-time activation race across several volume groups             | Not attempted, and it needs more stacks on one node than any run has used                                                                                                         |
| E-29                                                    | Graceful reboot teardown                                           | Only the unclean crash in M-12 has been run                                                                                                                                       |
| E-30                                                    | `system.devices` pruning across failure cycles                     | Nothing prunes a stale entry today. Design §14's Q5 owns the decision, so there is nothing to assert yet                                                                          |
| E-33                                                    | Server-side and client-side compression on one volume              | Expected to be redundant rather than harmful, and not measured                                                                                                                    |
| E-34                                                    | The placement injector beside the topology gate                    | Settled by code inspection: the injector only hints which storage node hosts the lvol and never touches pod scheduling (design §12). No runtime scenario has been run             |
| E-35                                                    | Ten or more stacks on one node                                     | Not attempted. M-01 already met a per-node VDO memory ceiling that likely bounds this lower than expected                                                                         |
| E-36                                                    | Sustained high-throughput writes                                   | Only small, non-sustained writes have been used                                                                                                                                   |
| E-37                                                    | A large volume with mixed compressible, duplicate, and random data | All savings measurements so far used small, synthetic, highly duplicate data                                                                                                      |
| —                                                       | Single-node cluster, and asymmetric node sizes                     | Not exercised. The feature is node-local, so the topology gate is the only part a node count changes                                                                              |
| —                                                       | In-tree `dm-vdo` on kernel 6.9 or newer                            | The probe implements only the legacy install path (design §14 Q3), so there is nothing to test yet                                                                                |
| —                                                       | `aarch64` and non-RHEL nodes                                       | Both are non-goals (design §2), and `aarch64` lacks a `vdo` package altogether                                                                                                    |
| —                                                       | Toggling a feature on an already-staged volume                     | `SetFeatures` is covered by U-52, and no live update path calls it (design §14 Q1)                                                                                                |
| —                                                       | `lvm2` VDO segtype detection in the capability probe               | Undecided whether it belongs there (design §14 Q7)                                                                                                                                |
