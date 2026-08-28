# Test Plan: Client-Side Compression and Deduplication via VDO

**Related design:** [`../designs/design-client-side-vdo-compression.md`](../designs/design-client-side-vdo-compression.md)

**Legend:** `U-` unit (Go, fake command runner, no `lvm2`/kernel), `M-` manual
(live cluster). Type: `Positive` / `Negative` / `Boundary` / `Regression`.

---

## 1. `atlas-lib/lvm` Typed Primitives

The general-purpose LVM layer `atlas-lib/lvm/vdo` is built on (design §3). Not
VDO-specific. Covers `PhysicalVolume`/`VolumeGroup`/`LogicalVolume` construction,
device scoping, and volume-group/logical-volume identity.

### Volume Group and Logical Volume Assembly (`atlas-lib/lvm/volume_test.go`)

| #    | Scenario                                                                                                              | Type     | Test                                                                 |
|------|-----------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------|
| U-1  | Create a physical volume, scoped to its device                                                                        | Positive | `TestManager_CreatePhysicalVolume`                                   |
| U-2  | Physical volume creation propagates a runner error                                                                    | Negative | `TestManager_CreatePhysicalVolume_WrapsRunnerError`                  |
| U-3  | Create a volume group on a single device                                                                              | Positive | `TestManager_CreateVolumeGroup` (single device)                      |
| U-4  | Create a volume group across several devices (striped-VG shape)                                                       | Positive | `TestManager_CreateVolumeGroup` (multiple devices)                   |
| U-5  | Activate a volume group                                                                                               | Positive | `TestManager_ActivateVolumeGroup`                                    |
| U-6  | Deactivate a volume group                                                                                             | Positive | `TestManager_DeactivateVolumeGroup`                                  |
| U-7  | Deactivation propagates a runner error                                                                                | Negative | `TestManager_DeactivateVolumeGroup_WrapsRunnerError`                 |
| U-8  | Remove a volume group                                                                                                 | Positive | `TestManager_RemoveVolumeGroup`                                      |
| U-9  | Removal propagates a runner error                                                                                     | Negative | `TestManager_RemoveVolumeGroup_WrapsRunnerError`                     |
| U-10 | `CreateLogicalVolume` dispatches to a registered `VolumeProvisioning` handler by `Handles(def)`, not a hardcoded name | Positive | `TestManager_CreateLogicalVolume_DispatchesByHandles`                |
| U-11 | No registered handler matches the definition: no extra flags contributed                                              | Negative | `TestManager_CreateLogicalVolume_NoHandlerMatchesContributesNothing` |

### Growing a Stack (`atlas-lib/lvm/grow_test.go`)

| #    | Scenario                                                                   | Type     | Test                                                     |
|------|----------------------------------------------------------------------------|----------|----------------------------------------------------------|
| U-12 | Expand a physical volume to its device's current full size                 | Positive | `TestManager_ExpandPhysicalVolume`                       |
| U-13 | Extend a volume group with an additional device                            | Positive | `TestManager_ExtendVolumeGroup`                          |
| U-14 | Volume group extension propagates a runner error                           | Negative | `TestManager_ExtendVolumeGroup_WrapsRunnerError`         |
| U-15 | Expand a logical volume by the additive `-l+100%FREE` form (design §3, §7) | Positive | `TestManager_ExpandLogicalVolume`                        |
| U-16 | Read a logical volume's current size in bytes                              | Positive | `TestManager_LogicalVolumeSize`                          |
| U-17 | Unparsable `lvs` size output is an error, not a silent zero                | Negative | `TestManager_LogicalVolumeSize_UnparsableOutput`         |
| U-18 | Extend a logical volume to an absolute byte size                           | Positive | `TestManager_ExtendLogicalVolumeToSize`                  |
| U-19 | Absolute-size extension propagates a runner error                          | Negative | `TestManager_ExtendLogicalVolumeToSize_WrapsRunnerError` |

### Clone and Snapshot-Restore Identity (`atlas-lib/lvm/clone_test.go`)

| #    | Scenario                                                                               | Type     | Test                                                            |
|------|----------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------|
| U-20 | Import a cloned volume group's identity (`vgimportclone`)                              | Positive | `TestManager_ImportClonedVolumeGroup`                           |
| U-21 | Import propagates a runner error                                                       | Negative | `TestManager_ImportClonedVolumeGroup_WrapsRunnerError`          |
| U-22 | Rename a logical volume after import                                                   | Positive | `TestManager_RenameLogicalVolume`                               |
| U-23 | Full resolution sequence: refresh, probe, import, rename, in order                     | Positive | `TestManager_ResolveClonedVolumeGroup_ResolvesAForeignIdentity` |
| U-24 | A device already carrying this volume's own identity, or a blank device, is left alone | Negative | `TestManager_ResolveClonedVolumeGroup_NoOps`                    |
| U-25 | The structural (pool) logical volume is never renamed                                  | Boundary | `TestManager_ResolveClonedVolumeGroup_PreservesStructuralLVs`   |
| U-26 | A failed cache refresh is non-fatal, and the content probe recovers                    | Negative | `TestManager_ResolveClonedVolumeGroup_SurvivesAFailedRescan`    |
| U-27 | A genuine probe failure is returned, not swallowed                                     | Negative | `TestManager_ResolveClonedVolumeGroup_WrapsAProbeFailure`       |

### Identity Probes and Rescan (`atlas-lib/lvm/identity_test.go`)

| #    | Scenario                                                                | Type     | Test                                                   |
|------|-------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-28 | Resolve a device's volume group by content, not by name                 | Positive | `TestManager_VolumeGroup`                              |
| U-29 | A real probe failure is an error, not folded into "blank"               | Negative | `TestManager_VolumeGroup_PropagatesRealProbeError`     |
| U-30 | List every logical volume in a volume group                             | Positive | `TestManager_ListLogicalVolumes`                       |
| U-31 | Listing propagates a runner error                                       | Negative | `TestManager_ListLogicalVolumes_PropagatesRunnerError` |
| U-32 | Detect whether a specific logical volume exists (orphaned-VG detection) | Positive | `TestManager_HasLogicalVolume`                         |
| U-33 | Detection propagates a runner error                                     | Negative | `TestManager_HasLogicalVolume_PropagatesRunnerError`   |
| U-34 | Rescan is scoped to exactly the given devices                           | Positive | `TestManager_Rescan`                                   |
| U-35 | Rescan propagates a runner error                                        | Negative | `TestManager_Rescan_PropagatesRunnerError`             |

### Orphaned Device-Mapper Node Cleanup (`atlas-lib/lvm/dm_test.go`)

| #    | Scenario                                                              | Type     | Test                                                                                             |
|------|-----------------------------------------------------------------------|----------|--------------------------------------------------------------------------------------------------|
| U-36 | Device-mapper's dash-escaping is applied before matching (design §7)  | Positive | `TestEscapeDMName`                                                                               |
| U-37 | No matching orphaned nodes: a no-op                                   | Negative | `TestManager_RemoveOrphanedDMNodes` (no matching nodes)                                          |
| U-38 | Matching nodes removed, unrelated nodes left alone                    | Positive | `TestManager_RemoveOrphanedDMNodes` (matches escaped names and removes them)                     |
| U-39 | A dependent node stuck on one pass clears once its dependency is gone | Boundary | `TestManager_RemoveOrphanedDMNodes` (a node stuck on pass one clears once its dependent is gone) |
| U-40 | `dmsetup ls` itself failing is a real error                           | Negative | `TestManager_RemoveOrphanedDMNodes` (dmsetup ls itself fails)                                    |

---

## 2. `atlas-lib/lvm/vdo`: the VDO Provisioning Handler

### VolumeProvisioning Registration (design §3) (`atlas-lib/lvm/vdo/volume_test.go`)

| #    | Scenario                                                                                                | Type     | Test                                                                |
|------|---------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------------|
| U-41 | `CreateVolumeArgs` contributes `--type vdo` flags for compression, deduplication, or both               | Positive | `TestVolumeHandler_CreateVolumeArgs`                                |
| U-42 | Neither flag set: no flags contributed                                                                  | Negative | `TestVolumeHandler_CreateVolumeArgs` (neither, contributes nothing) |
| U-43 | `Handles` agrees with `CreateVolumeArgs` on which definitions this handler owns (either flag, not both) | Boundary | `TestVolumeHandler_Handles`                                         |
| U-44 | Importing this package registers the handler, and `CreateLogicalVolume` actually reaches it             | Positive | `TestRegisteredHandlerReachesCreateLogicalVolume`                   |
| U-45 | Toggle compression/deduplication on an existing, active pool                                            | Positive | `TestUpdateVolume`                                                  |
| U-46 | Toggling propagates a runner error                                                                      | Negative | `TestUpdateVolume_WrapsRunnerError`                                 |

## 3. `atlas-lib/lvm/vdo`: the Stack Lifecycle (design §3)

### Create, Attach, and Reactivate (`atlas-lib/lvm/vdo/stack_test.go`)

| #    | Scenario                                                                    | Type     | Test                                                          |
|------|-----------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-47 | `DevicePath` derives the mount path from `lvolID` alone                     | Positive | `TestDevicePath`                                              |
| U-48 | Fresh device: full pvcreate/vgcreate/lvcreate sequence, VDO flags present   | Positive | `TestCreateOrAttach_FreshDevice`                              |
| U-49 | Existing, complete volume group: reactivated, never recreated               | Positive | `TestCreateOrAttach_ExistingVolumeGroupReactivates`           |
| U-50 | Orphaned volume group (interrupted create, zero LVs): removed and recreated | Boundary | `TestCreateOrAttach_OrphanedVolumeGroupIsRemovedAndRecreated` |

### Deactivate and Remove: the Unreachable-Device Fallback (design §3, §7)

| #    | Scenario                                                                    | Type     | Test                                                   |
|------|-----------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-51 | Deactivate succeeds normally                                                | Positive | `TestDeactivate_Success`                               |
| U-52 | Deactivate falls back to `dmsetup` cleanup only on "volume group not found" | Boundary | `TestDeactivate_UnreachableDeviceFallsBackToDMCleanup` |
| U-53 | Deactivate does not swallow an unrelated error into the fallback path       | Negative | `TestDeactivate_OtherErrorIsNotSwallowed`              |
| U-54 | Remove succeeds normally                                                    | Positive | `TestRemove_Success`                                   |
| U-55 | Remove falls back to `dmsetup` cleanup on any failure                       | Boundary | `TestRemove_UnreachableDeviceFallsBackToDMCleanup`     |

### Grow, Clone Resolution, and Feature Toggling

| #    | Scenario                                                                               | Type     | Test                                             |
|------|----------------------------------------------------------------------------------------|----------|--------------------------------------------------|
| U-56 | Full grow sequence: expand PV, expand pool, read pool size, extend VDO LV to that size | Positive | `TestGrow`                                       |
| U-57 | A foreign volume-group identity is re-stamped and the source's LV renamed              | Positive | `TestResolveClone_ForeignVolumeGroupIsReStamped` |
| U-58 | A device already carrying this volume's own identity is left alone                     | Negative | `TestResolveClone_OwnIdentityIsANoOp`            |
| U-59 | `SetFeatures` builds the right `lvchange` command against the pool                     | Positive | `TestSetFeatures`                                |

## 4. CSI Driver: Topology and `PersistentVolume` Pinning

`### vdoCapableSegment (design §4)` (`csi-driver/pkg/spdk/controllerserver_test.go`)

| #    | Scenario                                                                   | Type     | Test                    |
|------|----------------------------------------------------------------------------|----------|-------------------------|
| U-60 | Either client-side flag set contributes the `vdo-capable` topology segment | Positive | `TestVDOCapableSegment` |
| U-61 | Neither flag set: no segment contributed                                   | Negative | `TestVDOCapableSegment` |

---

## 5. Manual Scenarios (Live Cluster)

`csi-driver/pkg/util/vdo.go` itself has no direct unit tests (design §9): its
only logic is a one-line delegation per RPC concern into already-tested
`atlas-lib/lvm/vdo` functions. What unit tests structurally cannot reach is
whether `dm-vdo` is actually present and behaves as assumed, and whether a real
HA volume's duplicate local device nodes actually produce the ambiguity design
§7 describes. Every finding in design §7 came from the scenarios below.

### M-01: Create a pool with both flags, provision a volume

**Design reference:** §2, §4

**What to verify:** a `StoragePool` with `clientCompression: true` and
`clientDeduplication: true` produces a `StorageClass` carrying
`client_compression`/`client_deduplication = "True"` and an `allowedTopologies`
requiring `vdo-capable=true`. A PVC against it schedules only onto a node that
has advertised `vdo-capable=true`. The pod's mount is the VDO logical device
(`/dev/vdo-<lvolID>/<lvolID>`), not the raw NVMe-oF path. `vdostats` shows
`VDOCompression`/`VDODeduplication` both enabled.

**Test concept:**
1. Create the pool, PVC, and pod.
2. Confirm the `StorageClass` and `PersistentVolume.spec.nodeAffinity` (design §4).
3. Confirm the mount device and `vdostats` on the node.
4. Write compressible and duplicate data, then confirm `vdostats` savings are non-zero.

### M-02: Reattach on pod recreate (no data loss)

**Design reference:** §3 (`CreateOrAttach`, `Deactivate`)

**What to verify:** deleting and recreating the pod on the same node reattaches
the existing VDO device rather than recreating it, and data survives with a
matching checksum. This is the live-cluster proof for the bug design §7
describes: an earlier version called the destructive `Remove` from
`NodeUnstageVolume` and destroyed data on exactly this sequence.

**Test concept:**
1. Write and checksum data through the pod.
2. Delete the pod, recreate it against the same PVC.
3. Confirm no `pvcreate`/`vgcreate`/`lvcreate` ran (log inspection), and the
   checksum still matches.

### M-03: Expand

**Design reference:** §3 (`Grow`)

**What to verify:** growing the PVC grows the VDO pool and the VDO logical
volume online, filesystem included, with data intact throughout.

### M-04: Clone and snapshot restore co-located with a still-live source

**Design reference:** §3 (`ResolveClone`)

**What to verify:** a direct PVC clone and a snapshot restore, each scheduled
onto the same node as their still-live source, each resolve to their own
volume-group identity (`vgimportclone` + rename) and mount with data matching
the source exactly, with no cross-contamination between source, clone, and
restore.

### M-05: Storage-side disconnect while the node stays up

**Design reference:** §3 (`Deactivate`'s unreachable-device fallback), §7

**What to verify:** severing a VDO volume's NVMe-oF connection at the storage
side while the node and pod stay up eventually surfaces as a real I/O failure
(VDO fences itself read-only rather than corrupting data), and deleting the pod
afterward cleans up fully automatically, with no orphaned `dm-vdo` stack left
behind. This is the live-cluster proof for design §7's device-mapper
dash-escaping finding.

### M-06: XFS on top of VDO

**Design reference:** §5 (`xfsStripeOptions` skip)

**What to verify:** `mkfs.xfs` against a VDO volume runs with no stripe-alignment
flags, and the volume behaves identically to `ext4` on VDO for the rest of this
plan's scenarios.

---

## Coverage Summary

61 unit scenarios across `atlas-lib/lvm`, `atlas-lib/lvm/vdo`, and the CSI
driver's topology segment, plus 6 manual live-cluster scenarios covering what
unit tests cannot reach (design §9).

## What Is Not Yet Covered

| Gap                                                                     | Reason                                                                                                                                              |
|-------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `csi-driver/pkg/util/vdo.go`'s own delegation lines                     | No direct unit test file exists for this wiring. Every branch is a one-line call into already-tested `atlas-lib/lvm/vdo` functions (design §5, §9). |
| `advertiseVDOCapability`'s marker-file poll and label patch (design §4) | No unit or manual scenario in this plan exercises the DaemonSet `postStart` install path or the polling goroutine directly.                         |
| Server-side and client-side compression enabled together on one pool    | Open question (design §8, Q1). No defined expected behavior to test against yet.                                                                    |
| A non-RHEL-family node's capability install failing gracefully          | Open question (design §8, Q3). Current behavior is whatever `dnf`'s absence produces, not a validated path.                                         |
| Minimum VDO pool size                                                   | No enforcement exists (design §8, Q4), so there is nothing to assert beyond dm-vdo's own error text.                                                |
