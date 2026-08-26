# Test Plan: Node-Side Volume Stack

Related design: [`designs/design-node-volume-stack.md`](../designs/design-node-volume-stack.md)
Harness: [`csi-driver/pkg/util`](../../../csi-driver/pkg/util), [`csi-driver/pkg/spdk`](../../../csi-driver/pkg/spdk), [`csi-driver/e2e`](../../../csi-driver/e2e)

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) and SPDK behavior is a dependency, faked at
the boundary. LVM, device-mapper, VDO, and the NVMe-oF kernel driver are host
dependencies, faked through a command runner and a sysfs fixture for the unit
class and exercised for real in the end-to-end class.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake host
surface, mock HTTP), `I-` integration (the runner against a real temporary
directory and a faked host, no Kubernetes), `E-` end-to-end (live cluster, real
data path), `M-` manual (needs failure injection not yet automated). Types are
Types are
`Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column names the
implementing function, or `—` when the scenario is not yet covered. Every `—`
is accounted for in §8 What Is Not Yet Covered.

Section references written as `§n` mean this plan. References to the design are
written `design §n`.

---

## Axes Selected

| Axis                        | Applies       | Reading for this feature                                                                                                                                                                                                                             |
|-----------------------------|---------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A. Storage cluster topology | Yes, reframed | Node count is visible to the node service only as the number of NVMe-oF endpoints the control plane returns, so the axis is exercised as path count: one path (single-node cluster), several paths in priority order, and one of several unreachable |
| B. Namespace scope          | Yes           | LVM object names are derived from the logical volume's UUID, so the same PVC name in two namespaces must produce distinct volume groups (design §5.4)                                                                                                |
| C. Cluster count            | Yes           | Two `StorageCluster`s in one Kubernetes cluster put two volumes on one host whose stack records and LVM names must not collide                                                                                                                       |
| D. Failure domains          | No            | The node service performs no placement. Node eligibility is design §10 and is covered by the Phase 4 block in §5                                                                                                                                     |
| E. Object scale             | Yes           | Plan length (one layer through five), and stack count per host (zero, one, many)                                                                                                                                                                     |
| F. Lifecycle and timing     | Yes, primary  | A crash at each point in `Up`, a node reboot, an unstage after total path loss, and a redundant RPC after success are the axis this design exists to get right                                                                                       |
| G. Trigger and actor        | Yes           | Six entry points drive the runner differently (design §7.5), and which verb each may call is the contract                                                                                                                                            |
| H. Component version skew   | Yes           | A volume staged before Phase 1, a VDO volume staged by PR #402, and a record naming a layer the running version does not know                                                                                                                        |

---

## 1. Unit Tests

No host and no cluster. Layers are faked as recorders that log the verb, the
index, and the artifact they received, so ordering and unwind rules are table
tests. LVM and NVMe behavior is faked through a command runner and a sysfs
fixture of the kind `csi-driver/pkg/util/initiator_device_test.go` already
builds. Numbering runs continuously across the groups below.

### Plan Construction (design §3)

File: `csi-driver/pkg/spdk/plan_test.go` (new)

| #    | Scenario                                                                                                                                     | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | `volumeMode: Block` with no filesystem parameters yields `fabric` alone                                                                      | Positive | —    |
| U-02 | `fsType=ext4` yields `fabric` → `filesystem`                                                                                                 | Positive | —    |
| U-03 | `fsType=xfs` yields `fabric` → `filesystem`, with the XFS feature options set                                                                | Positive | —    |
| U-04 | `client_compression` set alone yields `fabric` → `lvmPV` → `lvmVolume(vdo)` → `filesystem`                                                   | Positive | —    |
| U-05 | `client_deduplication` set alone yields the same plan: a dedup-only volume still needs a working `kvdo` module                               | Positive | —    |
| U-06 | Both client-side parameters set yields one `lvmVolume(vdo)` layer, not two                                                                   | Boundary | —    |
| U-07 | Neither client-side parameter set yields no LVM layer at all, asserted by plan length                                                        | Negative | —    |
| U-08 | An unset `fsType` defaults to the plain filesystem plan rather than raw block                                                                | Boundary | —    |
| U-09 | An unrecognized `fsType` is rejected at plan construction, not at `mkfs`                                                                     | Negative | —    |
| U-10 | A malformed volume handle is rejected before any layer is constructed                                                                        | Negative | —    |
| U-11 | `nsId` absent or below one is rejected, preserving today's initiator-factory validation                                                      | Negative | —    |
| U-12 | Plan construction is a pure function: the same context and capability yield an equal plan twice, with no host access                         | Positive | —    |
| U-13 | The plan derived from a volume context is identical to the plan recorded for that volume, so a re-derivation never disagrees with the record | Positive | —    |

### Runner Ordering and Unwind (design §7)

File: `atlas-lib/volstack/runner_test.go` (new)

| #    | Scenario                                                                                                                                                 | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-14 | `Up` calls `Ensure` bottom to top, each layer receiving the artifact the layer below returned                                                            | Positive | —    |
| U-15 | `Up` calls `Observe` before `Ensure` on every layer                                                                                                      | Positive | —    |
| U-16 | `Ensure` failing at index 2 releases index 1 and index 0, in that order                                                                                  | Negative | —    |
| U-17 | `Ensure` failing at index 2 calls `Destroy` on nothing at all, asserted by a zero call count on every fake layer                                         | Negative | —    |
| U-18 | `Observe` failing at index 2 unwinds identically to `Ensure` failing there                                                                               | Negative | —    |
| U-19 | `Ensure` failing at index 0 releases nothing and returns the error                                                                                       | Boundary | —    |
| U-20 | A `Release` that fails during an unwind does not stop the unwind of the layers below it                                                                  | Negative | —    |
| U-21 | `Down` calls `Release` top to bottom                                                                                                                     | Positive | —    |
| U-22 | `Down` calls `Destroy` on nothing, asserted by a zero call count                                                                                         | Negative | —    |
| U-23 | `Down` skips `Release` on a layer whose `Observe` reports `StateAbsent`                                                                                  | Negative | —    |
| U-24 | `Down` still removes the stack record when a layer's `Release` left its object present, which is the shared-subsystem case                               | Positive | —    |
| U-25 | A plan of one layer brings up and down correctly                                                                                                         | Boundary | —    |
| U-26 | An empty plan is rejected at construction rather than running as a no-op `Up`                                                                            | Boundary | —    |
| U-27 | `Up` is idempotent: a second `Up` over a fully ready stack performs no `Ensure` that changes anything, asserted by the fakes' recorded state transitions | Positive | —    |
| U-28 | `Down` is idempotent: a second `Down` after a complete one is a no-op and does not error on the missing record                                           | Positive | —    |
| U-29 | The delete path calls `Down` and then `Destroy`, top to bottom, in that order                                                                            | Positive | —    |

### State Classification (design §4.2)

File: `atlas-lib/volstack/layers/state_test.go` (new)

| #    | Scenario                                                                                                                                                           | Type       | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-30 | A device carrying no LVM signature classifies as `StateAbsent`                                                                                                     | Positive   | —    |
| U-31 | A volume group with its logical volume present classifies as `StateReady`                                                                                          | Positive   | —    |
| U-32 | A volume group present but reporting zero logical volumes classifies as `StatePartial`, not `StateReady`                                                           | Boundary   | —    |
| U-33 | A volume group present, complete, and not activated classifies as `StateInactive`, not `StateAbsent`                                                               | Boundary   | —    |
| U-34 | A device whose on-disk volume group belongs to another volume classifies as `StateForeign`                                                                         | Negative   | —    |
| U-35 | `Ensure` on `StateInactive` activates and issues no `vgcreate`, `lvcreate`, or `mkfs`, asserted by the command runner's recorded calls                             | Negative   | —    |
| U-36 | `Ensure` on `StateForeign` re-identifies before activating, asserted by the order of the recorded calls                                                            | Positive   | —    |
| U-37 | `Ensure` on `StatePartial` completes the object and does not recreate the volume group                                                                             | Positive   | —    |
| U-38 | An LVM probe whose output carries a `WARNING:` line ahead of the field value still classifies correctly, which a byte-level clone produces (pins PR #402 defect 7) | Regression | —    |
| U-39 | A probe that fails outright classifies as `StateAbsent` rather than propagating an error, matching the "nothing to resolve" reading                                | Boundary   | —    |
| U-40 | An unformatted device classifies as `StateAbsent` for the filesystem layer, and a formatted one as `StateInactive` when unmounted                                  | Positive   | —    |

### Artifact and Geometry Propagation (design §4.3)

File: `atlas-lib/volstack/artifact_test.go` (new)

| #    | Scenario                                                                                                                                                          | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-41 | A `fabric` layer over a backend with known striping reports that geometry upward                                                                                  | Positive   | —    |
| U-42 | An `lvmVolume(vdo)` layer reports the zero `Geometry`, because VDO virtualizes blocks                                                                             | Positive   | —    |
| U-43 | The `filesystem` layer passes `mkfs.xfs` no stripe alignment when the layer below reports the zero `Geometry` (replaces PR #402's `xfsStripeOptions` conditional) | Regression | —    |
| U-44 | The `filesystem` layer passes `mkfs.xfs` the XFS feature options regardless of geometry, because feature-bit compatibility is unrelated to layout                 | Positive   | —    |
| U-45 | An `lvmVolume(striped)` layer reports its own stripe count and chunk size, and the filesystem above aligns to those rather than to the StorageClass parameters    | Positive   | —    |
| U-46 | A `Geometry` with a stripe count but no chunk size is treated as unknown rather than half-applied                                                                 | Boundary   | —    |
| U-47 | A fan-in layer reports its member devices in the recorded order, and a differently ordered member list produces a different artifact                              | Positive   | —    |

### The Stack Record (design §6)

File: `atlas-lib/volstack/record_test.go` (new)

| #    | Scenario                                                                                                                                                                                                                  | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-48 | The record is written before the first `Ensure` runs, asserted by the fakes observing the file already present                                                                                                            | Positive | —    |
| U-49 | The record holds layer parameters and no device path, so a reconnect that renames the device leaves it valid                                                                                                              | Positive | —    |
| U-50 | A per-layer marker is written before that layer's `Ensure`, not after                                                                                                                                                     | Positive | —    |
| U-51 | The record is removed only after the last `Release` succeeds                                                                                                                                                              | Positive | —    |
| U-52 | A `Release` that fails leaves the record in place, so the stack stays discoverable                                                                                                                                        | Negative | —    |
| U-53 | An absent record resolves to the legacy plan `fabric` → `filesystem`                                                                                                                                                      | Negative | —    |
| U-54 | A record naming an unknown layer fails the unstage with the layer named, rather than skipping the layer                                                                                                                   | Negative | —    |
| U-55 | A truncated or malformed record fails with an error and does not resolve to the legacy plan, because a partial record is not an absent one                                                                                | Boundary | —    |
| U-56 | Two volumes from two `StorageCluster`s produce distinct record filenames, because the volume handle carries the cluster ID                                                                                                | Positive | —    |
| U-57 | The same PVC name in two namespaces produces distinct record filenames and distinct LVM names                                                                                                                             | Positive | —    |
| U-58 | A record filename is filesystem-safe for every volume handle the driver accepts                                                                                                                                           | Boundary | —    |

### LVM Naming and Primitives (design §5.3, §5.4)

File: `atlas-lib/lvm/lvm_test.go` (moved from `csi-driver/pkg/util/vdo.go`'s tests)

| #    | Scenario                                                                                                                                                  | Type       | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-60 | The volume group name is derived from the logical volume's UUID and nothing host-specific, so it is reproducible on another host                          | Positive   | —    |
| U-61 | The derived name is within LVM's length and character limits for every accepted UUID form                                                                 | Boundary   | —    |
| U-62 | Two volumes on one host derive distinct volume group names                                                                                                | Positive   | —    |
| U-63 | The device-mapper force path escapes the volume group name by doubling dashes before matching (pins PR #402 defect 9)                                     | Regression | —    |
| U-64 | The force path matches nothing and returns cleanly when no device-mapper node for the group exists                                                        | Negative   | —    |
| U-65 | Every LVM invocation carries `DM_DISABLE_UDEV=1`, because no udev daemon runs in the node container (pins PR #402 defect 4)                               | Regression | —    |
| U-66 | Every LVM invocation is scoped to the device under management rather than scanning every visible device                                                   | Positive   | —    |
| U-67 | `Grow` extends by the additive percentage form, so the computed target is never smaller than the current size (pins PR #402 defect 6)                     | Regression | —    |
| U-68 | `Grow` on a volume already at its target size succeeds without issuing an extend, which is kubelet's post-success retry (pins PR #402's open polish item) | Regression | —    |
| U-69 | An LVM command that exceeds its timeout returns an error rather than blocking the caller                                                                  | Negative   | —    |

### Co-Tenant Detach (design §8)

File: `csi-driver/pkg/util/initiator_device_test.go`, extended

| #    | Scenario                                                                                                                                  | Type     | Test                                                    |
|------|-------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------|
| U-70 | A subsystem that can hold several namespaces is not disconnected when one volume releases, and the paths stay up                          | Positive | —                                                       |
| U-71 | A subsystem that cannot hold several namespaces is disconnected on release                                                                | Positive | `TestDisconnectGlobOnLastNamespace`                     |
| U-72 | A subsystem currently holding co-tenants is not disconnected                                                                              | Positive | `TestDisconnectGlobOnRealNode`                          |
| U-73 | A subsystem that can be shared but currently holds one namespace is still not disconnected, which the current count-based gate gets wrong | Boundary | —                                                       |
| U-74 | The capability question failing to resolve leaves the fabric untouched and returns the error, rather than assuming either answer          | Negative | —                                                       |
| U-75 | Releasing a volume on a shared subsystem removes that volume's stack record and leaves the co-tenant's record alone                       | Negative | —                                                       |
| U-76 | `Destroy` on one volume's LVM objects issues no command naming a co-tenant's volume group                                                 | Negative | —                                                       |
| U-77 | A namespace device belonging to a neighboring namespace is not selected as this volume's device                                           | Negative | `TestMatchNamespaceDeviceRejectsNeighbouringNamespaces` |

### Optional Interface Dispatch (design §4.4, §9)

File: `atlas-lib/volstack/optional_test.go` (new)

| #    | Scenario                                                                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-78 | `Heal` visits only the layers implementing `Healer`, bottom to top                                                                 | Positive | —    |
| U-79 | `Heal` skips a layer whose `Healthy` returns true, asserted by a zero `Heal` call count on it                                      | Negative | —    |
| U-80 | A plan whose layers implement no `Healer` heals as a no-op and returns no error                                                    | Boundary | —    |
| U-81 | `Heal` on a layer receives the artifact of the layer below as re-derived after that layer was healed, not the artifact from before | Positive | —    |
| U-82 | `Grow` visits only the layers implementing `Grower`, bottom to top                                                                 | Positive | —    |
| U-83 | A plan whose layers implement no `Grower`, which is the pNFS client shape, grows as a no-op                                        | Boundary | —    |
| U-84 | `Grow` stops and reports when a lower layer's grow fails, and does not attempt the layers above it                                 | Negative | —    |
| U-85 | `Heal` never calls `Ensure`, asserted by a zero call count, because the data already exists                                        | Negative | —    |

---

## 2. Integration Tests

The runner against a real temporary directory for the stack record and a faked
host surface for the layers. No Kubernetes and no `envtest`: nothing in Phases 1
through 3 reconciles. The value of this class is crash simulation, which the unit
class cannot express because it needs a record that survives the process.

### Crash and Resume (design §6, §7)

File: `atlas-lib/volstack/resume_test.go` (new)

| #    | Scenario                                                                                                                                            | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `Up` interrupted after the record is written and before the first `Ensure`: a fresh `Down` finds the record and releases nothing, leaving no orphan | Positive | —    |
| I-02 | `Up` interrupted after `fabric`'s `Ensure` and before its marker would have been cleared: a fresh `Down` releases the fabric                        | Positive | —    |
| I-03 | `Up` interrupted at each layer index in turn: a fresh `Down` releases exactly the layers that were reached                                          | Positive | —    |
| I-04 | `Up` interrupted mid-`Ensure` on a layer that had created its object: the next `Up` observes `StatePartial` and completes it                        | Positive | —    |
| I-05 | `Down` interrupted after two layers were released: a second `Down` releases the rest and removes the record                                         | Positive | —    |
| I-06 | The whole host restarts with a record present and every layer inactive: `Up` reactivates and issues no create or format command                     | Positive | —    |
| I-07 | A record present for a volume that has no staging path and no pod on this host is reported as orphaned rather than released automatically           | Negative | —    |
| I-08 | The record directory is unwritable: `Up` fails before its first side effect rather than proceeding unrecorded                                       | Negative | —    |
| I-09 | Two records for the same volume handle cannot exist, and a second `Up` reuses the first                                                             | Boundary | —    |
| I-10 | 100 records on one host are enumerated and classified without exceeding the enumeration's bound, and the time is recorded                           | Boundary | —    |

### Verb Contract Under a Dead Foundation (design §7.4)

File: `atlas-lib/volstack/deadfoundation_test.go` (new)

| #    | Scenario                                                                                                                                                           | Type       | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| I-11 | Every layer's `Release` succeeds when the layer below reports `StateAbsent`                                                                                        | Positive   | —    |
| I-12 | `lvmVolume`'s `Release` falls back to the device-mapper force path when the LVM command fails on every retry, and the fallback is recorded (pins PR #402 defect 8) | Regression | —    |
| I-13 | `filesystem`'s `Release` unmounts a dead mount rather than erroring on it                                                                                          | Positive   | —    |
| I-14 | A layer with no force path whose command depends on a dead foundation reports the failure and leaves the record in place                                           | Negative   | —    |
| I-15 | `Down` over a stack whose every layer is already gone completes and removes the record                                                                             | Boundary   | —    |

---

## 3. E2E Tests

A live simplyblock cluster and a real data path. Every row that touches data
asserts correctness by checksum across the operation, not merely that I/O
continued. The suite is Ginkgo, and new blocks are named in the existing
`SPDKCSI-` style.

### No Behavior Change (design §16)

The Phase 1 claim is that nothing observable changes, so the existing blocks are
the assertion and must pass unmodified.

| #    | Scenario                                                                   | Type     | Test                                |
|------|----------------------------------------------------------------------------|----------|-------------------------------------|
| E-01 | An ext4 and an XFS volume stage, publish, and unstage through the runner   | Positive | `SPDKCSI-FILESYSTEM`                |
| E-02 | A raw block volume stages through a one-layer plan                         | Positive | `SPDKCSI-RAWBLOCK`                  |
| E-03 | Data survives a pod delete and an immediate re-mount, verified by checksum | Positive | `SPDKCSI-VOLUME-PERSIST`            |
| E-04 | A volume reconnects after path loss and the mount recovers                 | Positive | `SPDKCSI-RECONNECT`                 |
| E-05 | A volume recovers after total path loss                                    | Positive | `SPDKCSI-RECONNECT-FULLLOSS`        |
| E-06 | The guardian repairs a broken volume under a running pod                   | Positive | `SPDKCSI-RECONNECT-GUARDIAN`        |
| E-07 | An unmanaged subsystem on the host is left alone                           | Negative | `SPDKCSI-RECONNECT-UNMANAGED`       |
| E-08 | A clone and a snapshot restore stage alongside their live source           | Positive | `SPDKCSI-CLONE`, `SPDKCSI-SNAPSHOT` |
| E-09 | Volumes from two `StorageCluster`s stage on one host                       | Positive | `SPDKCSI-MULTICLUSTER`              |
| E-10 | An invalid request is rejected as it is today                              | Negative | `SPDKCSI-NEGATIVE`                  |

### Stacked Plans (design §3, §5)

| #    | Scenario                                                                                                                                             | Type       | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| E-11 | A VDO-backed ext4 volume stages, and `lvs` reports compression and deduplication enabled                                                             | Positive   | —    |
| E-12 | A VDO-backed XFS volume stages with no stripe alignment passed to `mkfs.xfs`                                                                         | Positive   | —    |
| E-13 | A pod delete and recreate on the same node reattaches the same VDO device, and the checksum matches (pins PR #402 defect 5, the destructive unstage) | Regression | —    |
| E-14 | A node reboot reattaches every stack on the host, with a checksum per volume and the `kvdo` module usage count matching the stack count              | Positive   | —    |
| E-15 | A clone and its source coexist on one node with independent volume group identities, both checksums matching their sources                           | Positive   | —    |
| E-16 | An unclean NVMe-oF disconnect under a live pod, followed by a pod delete, leaves no orphaned device-mapper stack and needs no manual intervention    | Regression | —    |
| E-17 | A PVC expansion grows the LVM stack and then the filesystem online, with data intact throughout                                                      | Positive   | —    |
| E-18 | A second expansion request after a successful one is a no-op and logs no error                                                                       | Boundary   | —    |
| E-19 | A volume whose plan needs a capability the node lacks fails to stage there with the reason visible on the PVC                                        | Negative   | —    |
| E-20 | Two volumes on a shared subsystem: unstaging one leaves the other serving I/O with no error                                                          | Positive   | —    |
| E-21 | Deleting the PVC of one volume on a shared subsystem leaves the co-tenant's volume group intact                                                      | Negative   | —    |
| E-22 | The same PVC name in two namespaces, both staged on one node, produce two distinct volume groups and two distinct datasets                           | Positive   | —    |
| E-23 | A volume staged before the upgrade unstages cleanly after it, with no record present                                                                 | Positive   | —    |
| E-24 | A VDO volume staged by the pre-Phase-2 code unstages cleanly after Phase 2, without destroying data                                                  | Regression | —    |
| E-25 | A single-path volume, from a single-node storage cluster, stages and unstages                                                                        | Boundary   | —    |
| E-26 | A multipath volume with one endpoint unreachable stages on the reachable paths and reports the unavailable one                                       | Negative   | —    |
| E-27 | Sustained fio across a stage, heal, and expand cycle: no I/O errors and verify-mode data integrity                                                   | Positive   | —    |

---

## 4. Unit Tests — Phase 4 (Planned)

Node requirements derived from the plan (design §10). No `Type` and no `Test`
column: both are decided when the phase is scoped, and design §17 Q6 records that
the phase is planned rather than committed. This is the first block with an
`envtest` surface, because the StoragePool controller composes the topology terms.

| #       | Scenario                                                                                                                                           |
|---------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| U-P4-01 | A plan containing a host-local layer yields that layer's capability in the volume's accessible topology                                            |
| U-P4-02 | A plan containing no host-local layer yields no capability segment, which is the pNFS client shape                                                 |
| U-P4-03 | Two host-local layers in one plan yield both capabilities, composed into one topology term so they are required together                           |
| U-P4-04 | A plan-derived segment produces the same key and value the hand-written DHCHAP segment produces, so existing volume node affinity stays valid      |
| U-P4-05 | A capability key is present in the node's reported topology at plugin registration regardless of the label's current value (pins PR #402 defect 1) |
| U-P4-06 | A layer reporting `PinsToNode` false contributes no node affinity even when it reports a capability                                                |
| U-P4-07 | The StoragePool controller derives the same topology terms from the plan that the CSI controller derives                                           |

---

## 5. Manual Scenarios and Test Concepts

### M-01 — Concurrent staging of two LVM-backed volumes on one host

**Design reference:** design §12

**What to verify:** two `Ensure` calls whose LVM commands genuinely overlap do
not corrupt either volume group and do not leave either stack partial.

**Current behavior:** unknown, and unexercised rather than proven safe. PR #402's
multi-instance validation ran its two stage sequences sequentially rather than
overlapping, so LVM's internal locking under truly concurrent `vgchange` and
`pvscan` has never been exercised on a real host. Per-volume locking does not
serialize them, because the contention is on LVM's host-wide locks and its device
scan.

**Open question:** design §17 Q2. The lock scope per layer is undecided, so this
scenario is as much a measurement as a test.

**Test concept:**
1. Provision two PVCs whose StorageClass sets `client_deduplication`, both
   scheduled to one node.
2. Start both pods in one `kubectl apply`, so both `NodeStageVolume` calls arrive
   within the same second. Confirm from the node plugin's log that the two LVM
   command sequences actually interleave, rather than asserting they did.
3. Write and checksum distinct data in each pod.
4. Assert both volume groups exist with the expected logical volume, both
   checksums match, and no LVM command reported a lock or a metadata error.
5. Repeat with ten volumes to widen the overlap window.

### M-02 — A host crash between the record write and the first `Ensure`

**Design reference:** design §6

**What to verify:** the record written ahead of the first side effect is what
makes an orphan discoverable, and a crash in that window leaves nothing attached
that no record names.

**Test concept:**
1. Build a node plugin whose runner panics after writing the record and before
   the first `Ensure`, gated behind an environment variable.
2. Stage a volume with a four-layer plan, triggering the panic.
3. Assert the record exists, no NVMe-oF path is attached, and no device-mapper
   node exists.
4. Restart the plugin, unstage, and assert the record is gone.
5. Repeat with the panic moved to each later point in `Up`, asserting that
   everything below the panic point is released by the unstage.

### M-03 — A namespace joins a shared subsystem between the check and the release

**Design reference:** design §8

**What to verify:** the capability gate, not the neighbor count, is what prevents
a destructive disconnect, so a namespace arriving in that window changes nothing.

**Current behavior:** `selectDisconnectTarget` counts the namespace devices the
by-id glob currently matches, so a subsystem that has just become the last one is
disconnected. A namespace joining immediately afterward loses its paths.

**Test concept:**
1. Provision two volumes on one subsystem with `max_namespace_per_subsys` above
   one, both staged on one node.
2. Unstage the first, and while its `Release` runs, stage a third volume onto the
   same subsystem.
3. Assert the second volume never loses a path, and that the third stages
   successfully.
4. Repeat with the second volume unstaged first, so the window opens on the last
   remaining namespace.

### M-04 — A record naming a layer the running plugin does not know

**Design reference:** design §13, design §17 Q1

**What to verify:** a downgrade fails loudly rather than skipping an object
nobody will release.

**Test concept:**
1. Stage a volume with a plan containing a layer, then hand-edit its record to
   name a layer the plugin does not implement.
2. Unstage, and assert the RPC fails with the unknown layer named, and that the
   record is left in place.
3. Assert the objects the plugin does know about are not released either, because
   a teardown that releases half a stack is worse than one that refuses.

---

## 6. Axis Coverage

| Axis                    | Values covered                                                                                                        | IDs                                 | Not covered                                                       |
|-------------------------|-----------------------------------------------------------------------------------------------------------------------|-------------------------------------|-------------------------------------------------------------------|
| A. Path count           | One path, several in priority order, one of several unreachable                                                       | E-25, E-04, E-26                    | More paths than the cluster has nodes                             |
| B. Namespace scope      | Single, two namespaces with the same PVC name                                                                         | U-01 … U-13, U-58, E-22             | Namespace deleted mid-stage                                       |
| C. Cluster count        | One `StorageCluster`, two in one Kubernetes cluster                                                                   | E-01, U-57, E-09                    | Two Kubernetes clusters, which this feature cannot see            |
| D. Failure domains      | —                                                                                                                     | —                                   | Excluded: the node service performs no placement (§Axes Selected) |
| E. Object scale         | Plan of one layer, of four, of five; zero, one, and 100 stacks per host                                               | U-25, U-04, U-26, I-10, E-14        | More than 100 stacks per host                                     |
| F. Lifecycle and timing | Crash at each `Up` index, crash mid-`Release`, node reboot, redundant post-success RPC, unstage after total path loss | I-01 … I-06, U-68, E-14, E-16, M-02 | Kubelet restart between `Up` and the record's removal             |
| G. Trigger and actor    | Stage, unstage, publish-heal, expand, restage, delete                                                                 | U-14, U-21, U-78, U-82, U-85, U-29  | `NodeGetVolumeStats` over a stacked plan                          |
| H. Version skew         | Pre-Phase-1 volume, pre-Phase-2 VDO volume, unknown layer in a record                                                 | E-23, U-53, E-24, U-54, U-55, M-04  | A record written by a version two phases ahead                    |

---

## 7. Coverage Summary

| Class          | Scenarios | Covered | Not covered                           |
|----------------|-----------|---------|---------------------------------------|
| Unit           | 85        | 3       | U-01 … U-70, U-73 … U-76, U-78 … U-85 |
| Integration    | 15        | 0       | I-01 … I-15                           |
| E2E            | 27        | 10      | E-11 … E-27                           |
| Unit — Phase 4 | 7         | 0       | U-P4-01 … U-P4-07                     |
| Manual         | 4         | 0       | M-01 … M-04                           |

The three covered unit scenarios are existing tests whose behavior this design
preserves rather than introduces: `TestDisconnectGlobOnLastNamespace` (U-71),
`TestDisconnectGlobOnRealNode` (U-72), and
`TestMatchNamespaceDeviceRejectsNeighbouringNamespaces` (U-77). The ten covered
end-to-end scenarios are the existing suite, which is the assertion that Phase 1
changes nothing observable (design §16).

---

## 8. What Is Not Yet Covered

Nothing in this design is implemented, so the gap list is stated as ranges rather
than as 130 identical rows. The reason column says what each range waits on
rather than repeating "not implemented."

| #                 | Gap                                                   | Reason                                                                                                                                          |
|-------------------|-------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-13       | Plan construction                                     | Phase 1. The plan type does not exist, and `plan_test.go` is created with it                                                                    |
| U-14 … U-29       | Runner ordering and unwind                            | Phase 1. The highest-value block: U-17 and U-22 are what make the release-never-destroy rule checkable                                          |
| U-30 … U-40       | State classification                                  | Phase 1 for the filesystem states, Phase 2 for the LVM states                                                                                   |
| U-41 … U-47       | Artifact and geometry                                 | Phase 1 for the plain plan, Phase 2 for U-42, U-43, and U-45                                                                                    |
| U-48 … U-59       | The stack record                                      | Phase 1                                                                                                                                         |
| U-60 … U-69       | LVM naming and primitives                             | Phase 2. Six of these pin defects PR #402 fixed and must be ported with the code, not rewritten from the design                                 |
| U-70, U-73 … U-76 | The strengthened co-tenant gate                       | Phase 1. U-73 fails against the current count-based gate, which is the point of the row                                                         |
| U-78 … U-85       | Optional interface dispatch                           | Phase 3                                                                                                                                         |
| I-01 … I-15       | Crash, resume, and dead-foundation contracts          | Phase 1 for I-01 through I-11 and I-13 through I-15, Phase 2 for I-12                                                                           |
| E-11 … E-27       | Stacked plans on a live cluster                       | Phase 2 for E-11 through E-18 and E-24, Phase 1 for E-20 through E-23 and E-25 through E-27, Phase 4 for E-19                                   |
| U-P4-01 … U-P4-07 | Plan-derived node requirements                        | Phase 4, which design §17 Q6 records as planned rather than committed                                                                           |
| M-01 … M-04       | Concurrency and crash injection                       | Needs failure injection the suites do not have. M-01 is a risk in shipped code once PR #402 merges, not only in this design                     |
| —                 | Two Kubernetes clusters                               | The node service has no cross-cluster surface. Excluded, not deferred                                                                           |
| —                 | Failure domains and placement topology                | The node service performs no placement (§Axes Selected)                                                                                         |
| —                 | More than 100 stacks per host                         | No bound is claimed above that, so no row asserts one. A bound belongs in the design before a test asserts it                                   |
| —                 | Kubelet restart between `Up` and the record's removal | The record is designed to survive it (design §6), but the injection point is inside kubelet rather than inside the plugin                       |
| —                 | `NodeGetVolumeStats` over a stacked plan              | `statfs` on the mount point is indifferent to what is underneath it, so no layer participates. Revisit if a layer ever reports its own capacity |
| —                 | Block mode combined with an LVM layer                 | Out of scope (design §2), and representable rather than untested                                                                                |
