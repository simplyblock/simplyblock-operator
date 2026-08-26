# Design Document: Node-Side Volume Stack

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-25  
**Related Issues:**

- [#277](https://github.com/simplyblock/simplyblock-operator/issues/277) — client-side compression and deduplication via VDO, whose node-side wiring this design absorbs
- [PR #402](https://github.com/simplyblock/simplyblock-operator/pull/402) — the VDO implementation this design generalizes

**Test Plan:** [`tests/test-plan-node-volume-stack.md`](../tests/test-plan-node-volume-stack.md)

---

## Phasing Overview

| Phase                   | Status  | Scope                                                                                                                     | Behavior change                                                         |
|-------------------------|---------|---------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| **Phase 1** (§4–§8)     | Planned | The `blockdev` split, the layer contract, the runner, the stack record, and the `fabric` and `filesystem` layers          | None. RWO parity with today's node service                              |
| **Phase 2** (§5.3–§5.4) | Planned | The `lvmPV` and `lvmVolume` layers, the VDO call sites migrated onto the stack, the LVM primitives moved into `atlas-lib` | None. VDO parity with PR #402                                           |
| **Phase 3** (§9)        | Planned | `Healer` and `Grower`, so heal, restage, and expand walk the stack                                                        | Heal and expand become correct for every layer, not only the bottom one |
| **Phase 4** (§10)       | Planned | Node requirements derived from the plan on the controller side                                                            | Topology gating stops being hand-written per feature                    |

Phase 1 is shippable on its own because it changes no observable behavior: the
existing RWO plan is `fabric` → `filesystem`, and the runner performs exactly the
calls `NodeStageVolume` performs today. Phase 2 is shippable because it moves code
that is already validated on a live cluster. Phase 3 is the first phase that fixes
something, and Phase 4 is the only phase that touches the CSI controller service.

Phase 4 is planned rather than committed. It is in this document because the
pattern it replaces is already duplicated, and a design that leaves it out invites
the third copy.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                       | Kind       | Blocks  | Status                                                     |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|---------|------------------------------------------------------------|
| P0-1 | `lvm2` and `vdo` packages present in the CSI node image. `vdo` has no AArch64 build in the configured repositories, so LVM-backed plans are x86_64 only            | Ecosystem  | Phase 2 | On the PR #402 branch, not on `main`                       |
| P0-2 | `kmod-kvdo` built for the running host kernel, per node                                                                                                            | Node OS    | Phase 2 | Available on some hosts only, which is why §10 gates on it |
| P0-3 | `hostPID: true` on the CSI node DaemonSet, so the module load can `nsenter` the host namespace                                                                     | Kubernetes | Phase 2 | On the PR #402 branch                                      |
| P0-4 | No udev daemon runs inside the node container, so device-mapper's udev-sync handshake never completes and `DM_DISABLE_UDEV=1` is required for every LVM invocation | Ecosystem  | Phase 2 | Known, handled on the PR #402 branch                       |

Without P0-1 or P0-2 a node cannot run an LVM-backed plan at all. The consequence
is not a degraded volume but an unschedulable one: §10 keeps such volumes off such
nodes, and until Phase 4 lands the pod stays `Pending` with the failure visible on
the node plugin's log rather than on the PVC. P0-3 and P0-4 are environment facts
rather than decisions, and a node image that lacks either produces a layer whose
`Ensure` fails on its first command.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The Layer Contract](#4-the-layer-contract)
5. [Layer Catalog](#5-layer-catalog)
6. [The Stack Record](#6-the-stack-record)
7. [Bring-Up and Bring-Down](#7-bring-up-and-bring-down)
8. [Co-Tenant Namespaces](#8-co-tenant-namespaces)
9. [Heal and Grow](#9-heal-and-grow)
10. [Node Requirements and Capability Gating (Phase 4)](#10-node-requirements-and-capability-gating-phase-4)
11. [Package Layout](#11-package-layout)
12. [Concurrency and Locking](#12-concurrency-and-locking)
13. [Failure Modes and Fallback](#13-failure-modes-and-fallback)
14. [Observability](#14-observability)
15. [Testing Strategy](#15-testing-strategy)
16. [Migration Strategy](#16-migration-strategy)
17. [Open Questions](#17-open-questions)
- [Appendix A: `blockdev.Device`](#appendix-a-blockdevdevice)
- [Appendix B: `LockScope`](#appendix-b-lockscope)

---

## Overview

A persistent volume on a node is a stack of objects, each one built on the one
below it. The simplest stack is two objects deep: an NVMe-oF namespace, and a
filesystem mounted on it. Client-side deduplication adds an LVM physical volume
and a VDO logical volume between them. A striped pNFS export adds several
namespaces at the bottom, a striped logical volume above them, and an NFS export
at the top, and the client that mounts that export builds a different stack out of
the same pieces.

The node service does not model this. It has one extension point, the
`util.SpdkCsiInitiator` interface, which answers a single question ("connect this
and return a device path") and has exactly one implementation. Everything above
that seam is written inline, so each new capability is added as a conditional in
every RPC that touches the data path. Client-side VDO cost five such call sites
for one optional object, and the pNFS work adds four more objects on two different
stack shapes.

This design replaces the inline conditionals with a `Layer`: one reversible
transform, with four verbs, that can be composed into a **plan**. A plan is an
ordered list of layers, derived from the volume's parameters at stage time and
recorded on the host, and a **runner** brings it up bottom to top and takes it
down top to bottom. Raw block mode is not a branch in this model, it is a shorter
plan. A striped export and a VDO volume are the same LVM layer with a different
logical-volume type.

The four verbs are the load-bearing part, and they come from a defect that PR #402
found on a live cluster. `Release` gives up the host's hold on an object and keeps
its data, `Destroy` removes the object. Conflating them made an ordinary pod
delete and recreate run `vgremove` and silently destroy the volume's data. Once
the two are separate, `NodeUnstageVolume` calls only `Release`, a failed bring-up
unwinds with only `Release`, and `Destroy` has a different caller entirely.

---

## 1. Background

`NodeStageVolume` in `csi-driver/pkg/spdk/nodeserver.go` performs a fixed
sequence: parse the volume handle, refresh the connection from the control plane,
build a `util.SpdkCsiInitiator` from the volume context, call `Connect` for a
device path, then call `stageVolume`, which is a single
`FormatAndMountSensitiveWithFormatOptions` followed by an `xfs` and `ext4`
if-chain. `NodeUnstageVolume` reads the volume context back from the staging
parent directory, unmounts, and calls `Disconnect`.

`util.SpdkCsiInitiator` is the only seam, and it is a polymorphism seam rather
than a composition seam: `Connect(ctx) (string, error)` and `Disconnect(ctx)
error`, with `initiatorNVMf` as the sole implementation. It can answer "which
kind of thing is at the bottom of the stack" and nothing about what sits above.

Four capabilities are arriving at once, and none of them fits that seam:

- **Client-side compression and deduplication** (issue #277, PR #402) inserts an
  LVM physical volume and a VDO logical volume between the namespace and the
  filesystem.
- **Single-volume pNFS** ([`design-pnfs-rwx.md`](design-pnfs-rwx.md) §8, §10)
  needs two different stacks over one namespace: the MDS host formats, mounts, and
  exports it, and the client publishes an `eui64` alias and mounts the export over
  NFS.
- **Striped pNFS** ([`design-pnfs-striped.md`](design-pnfs-striped.md) §2.1, §2.2)
  puts *n* namespaces at the bottom and a striped logical volume above them, again
  in two shapes, with the client activating the same volume group read-only.
- **Raw, ext4, and XFS** already differ in whether the filesystem object exists at
  all and in what options create it.

PR #402 is the measurement of what the missing seam costs. Its VDO mechanics are
sound and validated end to end on a live cluster, including nine defects only
findable on real hardware. Its wiring repeats

```go
mountDevicePath := devicePath
if compression, deduplication, wantsVDO := vdoParams(vc); wantsVDO {
    lvolID := volumeID
    if spdkVol, perr := parseVolumeID(volumeID); perr == nil { lvolID = spdkVol.lvolID }
    // ...
}
```

in `NodeStageVolume`, `NodeUnstageVolume`, `NodeExpandVolume`, and
`restageVolume`, plus a fifth negative gate inside `stageVolume` that suppresses
the `xfs` stripe hints. That is one optional object costing five call sites,
before pNFS adds four more objects across two stack shapes.

Three of the defects PR #402 fixed are contract questions rather than
implementation slips, and §4 answers each of them once instead of per layer:

- `NodeUnstageVolume` called the destructive `RemoveVDO` (`vgremove`) on every
  unstage, including a routine pod delete and recreate on the same node, which
  silently destroyed the data. The fix introduced a non-destructive
  `DeactivateVDO`.
- `vgchange -an` fails on every retry when the backing NVMe-oF device is already
  gone, leaving a permanently orphaned device-mapper stack. The fix added a
  `dmsetup remove` force path, which then matched nothing until it accounted for
  device-mapper's dash escaping.
- A byte-level clone carries its source's LVM metadata, so two volumes on one host
  claim the same volume group. The fix, `ResolveClonedVDO`, runs `vgimportclone`
  and `lvrename` before touching the device.

---

## 2. Goals and Non-Goals

### Goals

- A single interface that composes the node-side objects of a persistent volume
  into an ordered plan, so a new object is one implementation rather than a
  conditional in every data-path RPC.
- Bring-up and bring-down that are convergent, not transactional: every verb is
  safe to re-enter, because `NodeStageVolume` is retried, a heal re-runs a live
  stack, and a teardown may resume after a crash.
- Idempotence at the layer and not only convergence at the runner: `Ensure`,
  `Release`, and `Destroy` leave the host in the state one application leaves it
  in when applied twice, and a verb whose object is already in its target state
  succeeds rather than reporting an error. A `vgremove` against a volume group
  that is already gone is a success, because the alternative wedges a delete path
  on an object nobody can remove. This is the property `unwind` rests on rather
  than a tension with it (§7.3): a failed bring-up releases best-effort and may
  itself be interrupted, so correctness comes from the next attempt converging.
- A structural separation between releasing a host's hold on an object and
  destroying the object, so the defect that made an ordinary pod restart destroy
  data cannot be written again.
- Bring-down that works when the layer below is already gone, which is the normal
  case after total path loss.
- Bring-down that never disconnects a subsystem another volume is using (§8).
- Heal, restage, and expand that decompose over the same plan, so they stop being
  correct only for the bottom layer.
- A record on the host of what was built, written before the first side effect, so
  a partially built stack is discoverable and removable after a crash.
- A stack that outlives the process that built it: after a csi-node pod restart
  mid-stage, the plan and how far it got are recoverable from the record and
  `Observe` alone. No layer keeps bring-up state only in process memory, and the
  record's directory outlives the pod (§6).
- No behavior change for volumes that stage today, and no behavior change for the
  VDO volumes PR #402 validated.
- Node capability and node pinning derived from the plan rather than hand-written
  per feature (Phase 4).

### Non-Goals

- **The pNFS layers themselves.** `alias`, `nfsExport`, and `nfsMount`, the
  read-only client activation, `fsid` allocation, and MDS selection belong to
  [`design-pnfs-rwx.md`](design-pnfs-rwx.md) and
  [`design-pnfs-striped.md`](design-pnfs-striped.md). This design defines the
  contract they implement and names them in §5 only to show that the contract
  fits.
- **A user-authored plan.** A plan is derived from a small set of named volume
  kinds and their options. A StorageClass parameter carrying a list of steps
  would be an API forever and an unbounded test surface.
- **Parallel layer execution.** §7 brings layers up one at a time. On this data
  path the objects that look independent usually are not, and the ordering is the
  correctness property.
- **Changing what VDO does.** Phase 2 moves PR #402's mechanics behind the
  contract. The commands it runs, the names it derives, and the force paths it
  falls back to are preserved, because they are what the live validation covered.
- **Replacing the volume-context stash.** `util.StashVolumeContext` keeps its
  current role. §6 adds a record of the plan beside it and does not merge the two.
- **Block-mode plus VDO.** PR #402 excludes it explicitly and this design does not
  add it. The plan for it is representable (`fabric` → `lvmPV` →
  `lvmVolume(vdo)`, with no `filesystem`), which makes it a scoping decision
  rather than an untested combination, but it is still out of scope.

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│                        csi-node (one per host)                         │
│                                                                        │
│  NodeStageVolume ────┐                                                 │
│  NodeUnstageVolume   │                                                 │
│  NodeExpandVolume    ├──▶  plan(VolumeContext, VolumeCapability, Role) │
│  NodePublishVolume   │              │                                  │
│  restage / heal ─────┘              ▼                                  │
│                              ┌─────────────┐                           │
│                              │   Runner    │  Up / Down / Heal / Grow  │
│                              └─────────────┘                           │
│                                │        │                              │
│              reads and writes  │        │  Observe / Ensure            │
│                                ▼        │  Release / Destroy           │
│              /var/lib/simplyblock/      │                              │
│                stacks/<handle>.json     ▼                              │
│                                  ┌─────────────┐                       │
│                                  │   Layers    │                       │
│                                  └─────────────┘                       │
└────────────────────────────────────────────┬───────────────────────────┘
                                             │
              ┌──────────────────────────────┼──────────────────────────┐
              ▼                              ▼                          ▼
      NVMe-oF fabric                 LVM / device-mapper        mkfs and mount
   (atlas-lib nvmeof, nvme)          (lvm2, kvdo, dmsetup)      (k8s mount-utils)
```

**The CSI RPCs no longer know the shape of the stack.** They build a plan, hand it
to the runner, and act on the artifact the top layer produces. `stageVolume`'s
`xfs` and `ext4` if-chain becomes the `filesystem` layer, and `vdoParams(vc)`
disappears from every RPC because the plan already carries the answer.

**The plan is built once per RPC and recorded once per volume.** Building it is a
pure function of the volume context and the volume capability, which is what makes
it unit-testable without a host. Recording it is what makes teardown possible when
the process that built it is gone (§6).

The plans the contract has to express, bottom to top. The last four differ from
each other only by the node's role for the volume, which is why the role is an
input to plan construction and not a property of the volume:

| Volume kind                                   | Plan                                                                       |
|-----------------------------------------------|----------------------------------------------------------------------------|
| Plain, raw block                              | `fabric`                                                                   |
| Plain, ext4 or XFS                            | `fabric` → `filesystem`                                                    |
| Client-side dedup or compression, ext4 or XFS | `fabric` → `lvmPV` → `lvmVolume(vdo)` → `filesystem`                       |
| pNFS single, MDS host                         | `fabric` → `filesystem` → `nfsExport`                                      |
| pNFS single, client node                      | `fabric` → `alias` → `nfsMount`                                            |
| pNFS striped, MDS host                        | `members(n)` → `lvmPV` → `lvmVolume(striped)` → `filesystem` → `nfsExport` |
| pNFS striped, client node                     | `members(n)` → `alias` → `lvmVolume(activate, read-only)` → `nfsMount`     |

Two results are worth reading off that table. Raw block mode is the plain plan
with its top layer absent rather than a conditional inside a stage function. And
the VDO volume and the striped export use the same `lvmVolume` layer with a
different logical-volume type, which is why §5.4 treats striping as a parameter
and not as a second implementation.

---

## 4. The Layer Contract

### 4.1 `Layer`

```go
// Layer is one reversible transform in a volume's node-side stack: it takes what
// the layer below exposes and exposes something for the layer above. Every method
// is safe to re-enter, because NodeStageVolume is retried, a heal re-runs a live
// stack, and a teardown may resume after a crash.
type Layer interface {
	// Name identifies the layer in logs and in the stack record. It is stable
	// across releases: a teardown after an upgrade replays a record an earlier
	// version wrote.
	Name() string

	// Observe reports what of this layer is present on the host without changing
	// anything. Ensure, Release, and Destroy all dispatch on what it found rather
	// than re-deriving the same facts.
	Observe(ctx context.Context, below Artifact) (State, error)

	// Ensure converges the layer and returns what the layer above consumes.
	Ensure(ctx context.Context, below Artifact) (Artifact, error)

	// Release drops this host's hold on the layer and keeps its data. It is the
	// only verb NodeUnstageVolume calls, and it has to succeed when the layer
	// below is already gone, which is the normal case after total path loss.
	Release(ctx context.Context, below Artifact) error

	// Destroy removes the layer's durable object. Only a deletion path calls it,
	// never an unstage.
	Destroy(ctx context.Context, below Artifact) error
}
```

**`Release` and `Destroy` are separate because conflating them destroys data.**
`NodeUnstageVolume` fires whenever no pod on this node needs the volume mounted,
which includes an ordinary pod delete and recreate against the same PVC on the
same node. A teardown path that removes durable objects there removes them on a
pod restart. The existing `defer initiator.Disconnect()` in `NodeStageVolume` is
already a `Release` and is safe for that reason. The same reflex applied to a
volume group is the defect PR #402 fixed.

Not every layer implements all four distinctly. `lvmPV` has nothing to release,
because a physical-volume signature is not something a host holds. `fabric` has
nothing to destroy, because the namespace belongs to the control plane. A verb
with nothing to do returns without error rather than returning "unsupported": the
runner calls all four uniformly, and a layer that has to be special-cased by the
runner is not a layer.

### 4.2 `State`

```go
// State is what Observe found. The distinctions matter because Ensure's response
// to each is different, and two of them are the difference between reactivating a
// volume and reformatting it.
type State int

const (
	// StateAbsent means nothing of this layer exists. Ensure creates it, which is
	// the only circumstance under which a layer may format anything.
	StateAbsent State = iota

	// StatePartial means an interrupted Ensure left an incomplete object. An LVM
	// volume group whose logical volume was never created reports zero logical
	// volumes and activates successfully while producing no usable device, so
	// "the group exists" is not the same question as "the layer is ready".
	StatePartial

	// StateForeign means the object exists but carries another volume's identity.
	// A byte-level clone copies its source's LVM metadata, so the clone's device
	// claims the source's volume group until vgimportclone renames it.
	StateForeign

	// StateInactive means the object is complete but not currently mapped on this
	// host. It is what Release leaves behind and what a node reboot leaves behind,
	// and Ensure reactivates rather than recreating.
	StateInactive

	// StateReady means present, complete, and usable.
	StateReady
)
```

**The `StateAbsent` and `StateInactive` distinction is the one that loses data
when it is wrong.** Every layer that can create a durable object must be able to
tell "this volume has never been set up here" from "this volume is set up and
merely not activated," because the first answer permits a `mkfs` or an `lvcreate`
and the second forbids it. PR #402's `CreateOrAttachVDO` encodes exactly this rule
in prose ("if the volume group already exists it is reactivated, never
recreated"), and lifting it into the type is what makes it checkable.

### 4.3 `Artifact` and `Geometry`

```go
// Artifact is what one layer hands to the layer above it. It carries what a
// higher layer can act on and nothing about how the layer below produced it.
type Artifact struct {
	// Devices are the block devices this layer exposes, in a defined order. A
	// fan-in layer exposes several; every other layer exposes one.
	Devices []blockdev.Device

	// Path is the filesystem path this layer mounted, empty until a layer mounts
	// one.
	Path string

	// Geometry is the stripe layout of Devices, for a layer above that aligns to
	// it.
	Geometry Geometry
}

// Geometry is a stripe layout: the per-stripe chunk size and the number of
// stripes data is spread across. The zero value means unknown, which is the
// correct answer for a device whose blocks are virtualized.
type Geometry struct {
	ChunkBytes int64
	Stripes    int
}
```

`Devices` carries a type rather than a path. `blockdev.Device`, its fields, and
why a path string is insufficient are [Appendix A](#appendix-a-blockdevdevice).

**`Geometry` exists because a filesystem's format options depend on what is
underneath it.** PR #402 discovered this as a special case: once VDO is in play the
`xfs` stripe hints must be suppressed, because VDO virtualizes and relocates
blocks and the filesystem is no longer laid out over the erasure-coded backend
device those hints were computed for. Applying them there is misleading rather
than merely useless. Expressed as a conditional, that is a fifth call site.
Expressed as a value, the VDO layer reports `Geometry{}` and the `filesystem`
layer passes no `-d su=,sw=` because there is nothing to align to.

The same field improves the striped case rather than merely unifying it. A striped
`lvmVolume` layer knows its own stripe count and chunk size, so the `filesystem`
layer above it receives real geometry instead of the `xfs_su` and `xfs_sw`
StorageClass parameters and their `16k`/`1` fallbacks.

### 4.4 Optional interfaces

A layer implements these when it has something to contribute. The runner type
asserts for each and skips the layers that do not.

```go
// Healer is implemented by a layer whose object can go bad under a live stack and
// be repaired in place. Heal never recreates: the data already exists.
type Healer interface {
	// Healthy is a read. It reports whether this layer is currently serving.
	Healthy(ctx context.Context, own Artifact) (bool, error)

	// Heal repairs the layer against the layer below, which may itself have just
	// been healed.
	Heal(ctx context.Context, below, own Artifact) error
}

// Grower is implemented by a layer that has to be enlarged when the volume behind
// it grows. Grow is convergent: a layer already at its target size succeeds
// without doing anything, because kubelet reissues NodeExpandVolume after it has
// already succeeded.
type Grower interface {
	Grow(ctx context.Context, below Artifact) (Artifact, error)
}

// NodeRequirements is implemented by a layer that constrains where the volume may
// be staged: it needs something from the node, or its durable state stays there.
// The volume carrying it can then be staged only on a node that can run the
// layer, and only on one node at a time (§10).
//
// Unlike Healer and Grower this is a declaration rather than an action, which is
// why it is a noun: the runner interrogates it instead of calling it.
type NodeRequirements interface {
	// NodeCapability is the label a node must carry, or the zero value when any
	// node will do.
	NodeCapability() Capability

	// PinsToNode reports whether this layer's durable state lives on the host.
	PinsToNode() bool
}
```

Optional rather than mandatory is deliberate. Three of the seven layers in §5 have
nothing to heal and four have nothing to grow, and a mandatory interface would
fill them with methods that return nil. The runner's assertion is also what keeps
`NodeExpandVolume` honest: a plan whose layers implement no `Grower` at all is a
plan that needs no node-side expansion, which is the correct answer for a pNFS
client.

`Grower` being convergent answers the last loose end PR #402 left open, where a
redundant `NodeExpandVolume` after a successful one logs an alarming but harmless
error on kubelet's reconciliation retry.

---

## 5. Layer Catalog

| Layer               | Ensure                                                                               | Release                                                             | Destroy                   | Optional                     |
|---------------------|--------------------------------------------------------------------------------------|---------------------------------------------------------------------|---------------------------|------------------------------|
| `fabric` (§5.1)     | Connect every endpoint in the control plane's priority order, wait for the namespace | Detach, disconnecting only when the subsystem cannot be shared (§8) | —                         | `Healer`                     |
| `members` (§5.2)    | *n* × `fabric` in the recorded order                                                 | Reverse order                                                       | —                         | `Healer`                     |
| `lvmPV` (§5.3)      | `pvcreate`, or re-identify when `StateForeign`                                       | —                                                                   | `pvremove`                | —                            |
| `lvmVolume` (§5.4)  | `vgcreate` and `lvcreate` of the configured type, or activate when `StateInactive`   | `vgchange -an`, with a `dmsetup` force path                         | `lvremove` and `vgremove` | `Grower`, `NodeRequirements` |
| `filesystem` (§5.5) | `mkfs` when unformatted, then mount                                                  | Unmount                                                             | —                         | `Healer`, `Grower`           |
| `alias`             | Publish the `eui64` symlink                                                          | Remove the symlink                                                  | —                         | —                            |
| `nfsExport`         | Write the export drop-in, `exportfs -ra`                                             | `exportfs -u`                                                       | Remove the drop-in        | `Grower`                     |
| `nfsMount`          | `mount -t nfs -o v4.1`                                                               | Unmount                                                             | —                         | `Healer`                     |

`alias`, `nfsExport`, and `nfsMount` are listed for completeness and are out of
scope here (§2). They are specified by
[`design-pnfs-rwx.md`](design-pnfs-rwx.md) §8 and §10 and
[`design-pnfs-striped.md`](design-pnfs-striped.md) §2.

### 5.1 `fabric`

Wraps the existing NVMe-oF connect. `Ensure` asks the control plane where the
volume lives, builds one target per endpoint, connects them in the control plane's
priority order, and waits for the namespace device, which is the flow
`atlas-lib`'s `nvmeof.ConnectPaths` and `nvmeof.WaitForDevice` implement and which
`csi-driver/pkg/util/initiator.go` implements today with `nvme-cli`. Which of the
two implements it is a separate migration and not this design's business. The
layer's contract is the same either way.

`Observe` maps the device state onto §4.2: no namespace device is `StateAbsent`,
a device present but not accessible is `StatePartial`, and a device that
`nvme.Device.Accessible` reports as serving is `StateReady`. `StateForeign` and
`StateInactive` do not arise, because a namespace carries no host-local identity
and cannot be present-but-deactivated.

`Ensure` reports the backend's stripe geometry in the `Artifact` when it is known,
which is what lets the `filesystem` layer above align to it in the plain plan.

`Release` is a detach rather than a disconnect and is specified in §8. `Destroy`
does nothing: the namespace belongs to the control plane and is removed by
`DeleteVolume`.

### 5.2 `members`

A composite layer holding *n* `fabric` layers, for a plan whose bottom is several
namespaces rather than one. `Ensure` runs them in the recorded order and returns
one `Artifact` whose `Devices` are their `blockdev.Device` values in that same
order. `Release`
reverses.

**Member order is contract, not convenience.** A stripe over the same members in a
different order is a different device, and
[`design-pnfs-striped.md`](design-pnfs-striped.md) §2.3 requires that the order be
recorded and replayed rather than re-derived from a set. `members` is where that
requirement is satisfied, and it is why the plan is recorded (§6) instead of
rebuilt from the current StorageClass.

`members` is also the reason this design needs no dependency graph. Fan-in is the
only non-linear shape any of the plans in §3 has, and a composite layer expresses
it without making the ordering of anything else implicit.

### 5.3 `lvmPV` (Phase 2)

`Ensure` on `StateAbsent` runs `pvcreate` against the device below. `Observe`
reads the device's on-disk LVM signature to answer which volume group it currently
belongs to, and reports `StateForeign` when that is a volume group belonging to
another volume, which is what a byte-level clone produces. `Ensure` on
`StateForeign` re-identifies the device with `vgimportclone` and `lvrename` before
anything above it activates.

`Release` does nothing, because a physical-volume signature is not a hold. That
asymmetry is why PVs are their own layer rather than part of §5.4: the two objects
have different lifetimes, and `vgremove` does not remove the physical volumes it
released.

**The clone collision is not VDO-specific.** Any layer whose object is identified
by on-disk content has it, and
[`design-pnfs-striped.md`](design-pnfs-striped.md) §2.3 specifies deterministic
volume-group names without addressing what happens when a clone and its source are
staged on one host. `StateForeign` is that specification, in one place, for both.

### 5.4 `lvmVolume` (Phase 2)

One volume group holding one logical volume, whose type is a parameter: linear,
`vdo`, or striped. `Ensure` on `StateAbsent` runs `vgcreate` over the physical
volumes below and then `lvcreate` of the configured type. `Ensure` on
`StateInactive` runs `vgchange -ay` and creates nothing. `Ensure` on
`StatePartial`, the volume group whose logical volume was never created, completes
the `lvcreate`.

`Release` runs `vgchange -an`, and falls back to removing the device-mapper nodes
directly when the backing device is gone and every LVM retry fails. That force
path has to escape the volume-group name the way device-mapper does, doubling
dashes, or it matches nothing.

`Destroy` runs `lvremove` and `vgremove`. Its callers are volume deletion and, for
a pNFS export, `DeleteExport`. It is never reached from `NodeUnstageVolume`.

`Grow` extends the logical volume to the new physical capacity of the group and
then matches the logical size, and succeeds without acting when the volume is
already at its target.

Naming is derived from the logical volume's UUID and nothing host-specific, so a
plan replayed on another host arrives at the same names. The lvol UUID is already
globally unique, stable, and inside LVM's length and character limits, which makes
it a better identifier than a hash of namespace and PVC name and is the convention
PR #402 established. [`design-pnfs-striped.md`](design-pnfs-striped.md) §2.3
should adopt it rather than deriving a separate one, because a failover that
recomputes a different name cannot find the export it is recovering.

The striped and the VDO plans differ only in the logical-volume type and its
options. The `Artifact` a striped volume reports carries its real `Geometry`, and
a VDO volume reports the zero value (§4.3).

### 5.5 `filesystem`

`Ensure` formats the device below when `blkid` shows it is unformatted, then mounts
it at the staging path. Formatting and mounting stay one layer because
`mount-utils`' `SafeFormatAndMount` couples them deliberately and splitting them
loses its protection against formatting a device that another process is about to
mount.

Format options come from the volume's parameters and from the `Artifact` below.
`xfs` receives the feature options unconditionally, because on-disk feature
compatibility across kernel versions has nothing to do with the backend's layout,
and receives stripe alignment only when the layer below reports a non-zero
`Geometry`. `ext4` receives the reserved-blocks adjustment when the parameter is
set, and nothing when it is unset, preserving today's behavior where an unset
parameter means `mkfs.ext4`'s own default rather than `tune2fs -m 0`.

`Healthy` detects the dead mount that total path loss leaves behind, which is the
`stagingMountDead` check the node service performs today: an `ENOTCONN`, `ESTALE`,
or `EIO`-class error from the mount point, plus the additional probe `ext4` needs
because it does not shut down when its backing device is removed and therefore
looks healthy from cache. `Heal` remounts without reformatting.

`Grow` resizes the filesystem, and is absent from the plan entirely for a raw
block volume, which is how `NodeExpandVolume`'s current block-device special case
disappears.

---

## 6. The Stack Record

Every volume with a plan has one file on the host:

```
/var/lib/simplyblock/stacks/<volume-handle>.json
```

It holds the plan, which is the ordered list of layer names and the parameters
each was constructed with, plus a per-layer marker recording that `Ensure` was
attempted. The file is written **before the first `Ensure` runs** and removed
after the last `Release` succeeds.

**Writing it first is what makes a partially built stack removable.** A crash
between a fabric connect and the recording of that connect leaves paths attached
that nothing will ever release, which is a failure mode this repository has
already paid for on a different code path. Ordering the write ahead of the side
effect is the same discipline the operator's reconcilers apply to control-plane
calls.

**The directory has to outlive the container.** `/var/lib/simplyblock` is a host
path mounted into the csi-node pod rather than container-local storage, because a
plugin restart is an ordinary event and the record is the only thing that tells
the restarted process what the previous one built. A layer that caches its
bring-up progress in process memory defeats the same property, which is why
`Observe` is the only way any verb learns what is present.

**The record holds parameters, not device paths.** A device path is not stable
across a reconnect, which is why LVM identifies its physical volumes by on-disk
metadata rather than by path. Layer parameters are derived from volume identity
and are stable, so a teardown re-derives the artifacts through `Observe` rather
than trusting a path an earlier process wrote down.

Given that, the per-layer markers are a diagnostic and an optimization rather than
a correctness mechanism: `Release` on a layer whose `Observe` reports
`StateAbsent` is already a no-op, so a teardown that ignored the markers entirely
would reach the same end state. They earn their place by making "what was
attempted, in what order" answerable after the fact, and by letting a teardown
skip the layers that were never reached.

**The plan is recorded rather than re-derived because the StorageClass is not a
record of the past.** A class can be edited or deleted after a volume is
provisioned, and teardown owes the truth about what was built. The same reasoning
covers member order (§5.2), which cannot be recovered from a set.

**An absent record means the legacy plan.** A volume staged by a version of the
node service that predates this design has no file, and unstaging it uses
`fabric` → `filesystem`, which is exactly what that version built. No migration
step runs on the node and no volume needs to be restaged (§16).

This is host-local state, which has a consequence and an alternative. The
consequence is that the record is unreachable from the operator, so an orphaned
stack is found by a host sweep rather than by a cluster-wide query. The
alternative is to keep the record on the operator and reach it over csi-link,
which makes it visible cluster-wide at the cost of an RPC on the unstage path and
a dependency on the operator being reachable during teardown. §17 Q3 carries the
decision. Phase 1 uses the host-local file, because unstage has to work when the
operator does not.

### 6.1 File Format

```go
// Record is the on-disk form of a stack, one file per volume under
// /var/lib/simplyblock/stacks/.
type Record struct {
	// Version is the schema version of this file and not the release that wrote
	// it. A reader that does not recognize it refuses the record rather than
	// guessing (§13), because a teardown driven by a misread plan is worse than a
	// teardown that stops and says why.
	Version int `json:"version"`

	// VolumeHandle is an lvol.VolumeHandle, "clusterID:poolID:volumeID". It
	// repeats the filename so that a record found on its own identifies itself.
	VolumeHandle string `json:"volumeHandle"`

	// Plan is the ordered layer list, bottom first. The order is most of why the
	// file exists: Up walks it forward and Down walks it back.
	Plan []Entry `json:"plan"`
}

// Entry is one layer as the plan named it.
type Entry struct {
	// Layer is the value Layer.Name() returns, stable across releases (§4.1).
	Layer string `json:"layer"`

	// Params is what the layer was constructed with, opaque to the runner: the
	// layer that declared them is the only thing that parses them. A new layer
	// therefore ships without this format changing.
	Params json.RawMessage `json:"params,omitempty"`

	// Members is the ordered sub-plan of a fan-in layer (§5.2) and is empty for
	// every other layer. It is a field of its own rather than part of Params
	// because the runner walks it, and member order is a runner concern.
	Members []Entry `json:"members,omitempty"`

	// Attempted records that Ensure was called on this layer. It is a diagnostic
	// and an optimization, never a correctness mechanism.
	Attempted bool `json:"attempted"`
}
```

A striped stack whose `filesystem` layer was never reached:

```json
{
  "version": 1,
  "volumeHandle": "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333",
  "plan": [
    {
      "layer": "members",
      "attempted": true,
      "members": [
        {"layer": "fabric", "params": {"nqn": "nqn.2023-05.io.simplyblock:lvol:aaaa"}, "attempted": true},
        {"layer": "fabric", "params": {"nqn": "nqn.2023-05.io.simplyblock:lvol:bbbb"}, "attempted": true}
      ]
    },
    {"layer": "lvmPV", "attempted": true},
    {"layer": "lvmVolume", "params": {"type": "vdo", "stripes": 2, "chunkBytes": 65536}, "attempted": true},
    {"layer": "filesystem", "params": {"fsType": "xfs"}, "attempted": false}
  ]
}
```

**The write is atomic, and that is what makes the ordering real.** Each write goes
to a temporary file in the same directory, is `fsync`ed, is renamed over the
target, and the directory is `fsync`ed after the rename. A torn file would be
worse than no file at all, because an absent record means the legacy plan and a
half-written one would be read as a plan nobody built. Every `Attempted` flip
rewrites the whole record the same way, which is one small local write per layer
and is affordable precisely because the record holds no device state.

**Params name secrets rather than carrying them.** A DHCHAP key is a credential
and this file outlives the pod that wrote it, so `fabric` records where to read its
secret and re-reads it on the teardown path, exactly as `NodeStageVolume` did. A
record that embedded the value would put a credential in cleartext on every node
that ever staged the volume. The file is mode `0600` and its directory `0700`
regardless.

**What the format deliberately omits** is device paths and the reason above,
geometry and sizes, which `Observe` re-derives and which a `Grow` would invalidate
anyway, and anything naming the node, because the file is already on the node and a
record recording its own origin invites treating a copy from elsewhere as
authoritative.

---

## 7. Bring-Up and Bring-Down

### 7.1 `Up`

```
record.write(plan)                        // before any side effect
below := Artifact{}
for i, layer := range plan {
    record.mark(layer)                    // before this layer's side effect
    state, err := layer.Observe(ctx, below)
    if err != nil { unwind(plan[:i], below); return err }
    above, err := layer.Ensure(ctx, below) // dispatches on state
    if err != nil { unwind(plan[:i], below); return err }
    below = above
}
return below                              // what the RPC acts on
```

### 7.2 `Down`

```
for i := len(plan) - 1; i >= 0; i-- {
    layer := plan[i]
    if !record.marked(layer) { continue }
    state, err := layer.Observe(ctx, below(i))
    if err != nil { return err }
    if state != StateAbsent {
        if err := layer.Release(ctx, below(i)); err != nil { return err }
    }
    record.unmark(layer)
}
record.remove()
```

`below(i)` re-derives layer *i*'s input by observing the layers beneath it, rather
than reading a device path out of the record (§6).

### 7.3 A failed bring-up releases and never destroys

`unwind` walks the layers already brought up, top-down, and calls `Release` on
each. It never calls `Destroy`.

**This is the rule the four verbs exist for.** A `mkfs` that fails must not
trigger a `vgremove`, because the volume group underneath it may hold data that a
misfiring format check failed to see. `Release` is safe in the same situation
because it gives up a hold and takes nothing away. The existing
`defer initiator.Disconnect()` is this rule already, applied to the one layer the
node service has today, and generalizing it correctly means generalizing it as
`Release`.

A stack left partly up by a failed `Up` is not an error state that needs
resolving. `NodeStageVolume` is retried, every verb is convergent, and the next
attempt observes what is there and continues. The record survives the failure, so
even a process that never retries leaves a removable stack behind.

### 7.4 `Down` tolerates a dead foundation

Bring-down proceeds top-down through layers whose foundation may already be gone,
which is the normal case rather than an edge case: total path loss removes the
namespace while the device-mapper stack above it is still mapped, and the pod is
deleted afterward. Each layer owns its own force path for that situation, and
§5.4's `dmsetup` fallback is the worked example. A layer that has no force path and
whose command depends on the layer below is a layer that will strand a stack.

**`Release` returning without error does not mean the object is gone.** §8 has a
`fabric` layer that legitimately leaves its device present, so `Down` asserts
nothing about the state a released layer is in, and removes the record either way.

### 7.5 Which RPC calls what

| RPC or path                    | Runner call             | Notes                                                                                                                    |
|--------------------------------|-------------------------|--------------------------------------------------------------------------------------------------------------------------|
| `NodeStageVolume`              | `Up`                    | Acts on the top artifact's `Path`, or `Devices[0].Path` for raw block                                                    |
| `NodeUnstageVolume`            | `Down`                  | `Release` only. `Destroy` is never reached from here                                                                     |
| `NodePublishVolume`            | `Heal`, then bind-mount | kubelet skips `NodeStage` when the volume is still referenced on the node, so publish is where a heal has to happen (§9) |
| `NodeExpandVolume`             | `Grow`                  | Bottom to top, skipping layers that implement no `Grower`                                                                |
| `restageVolume`                | `Heal`                  | Never `Up`, because the data exists and nothing may be formatted                                                         |
| `DeleteVolume`, `DeleteExport` | `Down`, then `Destroy`  | The only callers of `Destroy`                                                                                            |

---

## 8. Co-Tenant Namespaces

A simplyblock subsystem can hold several namespaces. The
`max_namespace_per_subsys` StorageClass parameter is what provisions one that way,
and it means two different volumes can arrive at one host behind a single NQN.

**Disconnecting a subsystem tears down every namespace on it.** So the `fabric`
layer's `Release` is a detach, not a disconnect. `atlas-lib`'s
`nvmeof.DetachDevice` answers the question and reports `SharedSubsystem`, and when
it is set the layer releases nothing at the fabric level and leaves the paths up
for the co-tenants.

**The gate is whether the subsystem *can* be shared, not whether it currently
is.** `nvme.Device.IsMultiNamespace` is that question. Enumerating the neighbors
describes only the moment they were counted: a namespace can join between the
check and the disconnect, and a correct "none right now" answer is still
destructive when it does. A subsystem provisioned to be shared is therefore never
disconnected on one volume's behalf, even while it happens to hold only that
volume.

The node service does not use that gate today. `selectDisconnectTarget` in
`csi-driver/pkg/util/initiator.go` counts the namespace devices the by-id glob
currently matches and disconnects when the count reaches one, which is the
weaker, enumerate-the-neighbors answer. Moving the decision behind `fabric`'s
`Release` is what replaces it, and the existing behavior for a subsystem that
genuinely still holds co-tenants has to be preserved while the gate is
strengthened.

Three consequences for the rest of this design:

- **`Down` cannot assert that a released layer is absent.** A `fabric` layer over
  a shared subsystem returns from `Release` with its device still present and
  still serving another volume. §7.4 states this as a rule and it is where the
  rule comes from. The stack record is removed regardless, because this volume's
  stack is down even though the fabric it stood on is not.
- **Ordering matters more, not less.** A device-mapper stack holding the namespace
  open is what makes even a legitimate disconnect fail. Every layer above
  `fabric` must be released first, which is what top-down teardown already
  guarantees, and which is why the force paths in §7.4 exist for the cases where
  it was not.
- **`Destroy` must stay per-volume.** Removing one volume's LVM objects must not
  touch a co-tenant's. Deriving every LVM name from the logical volume's UUID
  (§5.4) is what makes that true by construction rather than by care.

Reaping a subsystem whose controllers are all dead is a deliberate
`connector.Disconnect` and never a default, which matches `atlas-lib`'s existing
contract: `DetachDevice` returns the error rather than guessing when the question
needs a live controller to answer.

---

## 9. Heal and Grow

Heal and expand are where the current design's cost is highest, because both are
implemented for the bottom layer only. `healVolumeBeforePublish` reconnects the
namespace, `ensureDeviceConnected` checks for a device, and `restageVolume`
remounts, and none of them knows that an LVM or VDO object might sit between the
two. PR #402 patched `restageVolume` with a fourth copy of its conditional to
close exactly that gap.

**`Heal` walks the plan bottom to top.** For each layer that implements `Healer`,
the runner asks `Healthy` and calls `Heal` when the answer is no, passing the
artifact of the layer below, which may itself have just been healed. A layer that
implements no `Healer` is skipped and its artifact is re-derived through `Observe`,
so a healed foundation propagates upward.

Bottom to top is the only workable order. A remount over a namespace that has not
been reconnected fails, and a namespace reconnect underneath a filesystem that is
still holding a dead mount does not clear the dead mount.

**`Grow` walks the plan bottom to top as well**, and for the same reason: a
logical volume cannot be extended past a physical volume that has not been
resized, and a filesystem cannot be grown past its logical volume. Every `Grow` is
convergent, so kubelet's reconciliation retry after a successful expansion is a
sequence of no-ops rather than a sequence of alarming errors.

The three layers with something to heal are `fabric` (path reconnection and ANA
reconciliation, which the existing `MonitorConnection` and guardian machinery
already perform), `filesystem` (dead-mount detection and remount), and `nfsMount`
(`ESTALE` detection). The three with something to grow are `lvmVolume`,
`filesystem`, and `nfsExport`. Every other layer implements neither, which is the
argument for the interfaces being optional (§4.4).

---

## 10. Node Requirements and Capability Gating (Phase 4)

A layer whose durable state lives on the host makes two demands on scheduling. The
volume can be staged only on a node that can run the layer, and once it is staged
it can be staged nowhere else, because the state does not follow the pod.

Both demands are already implemented twice on the CSI controller side.
`dhchapAllowedNodeSegment` merges a DHCHAP topology segment into
`CreateVolume`'s `AccessibleTopology` so `external-provisioner` pins
`PersistentVolume.spec.nodeAffinity`, and PR #402 adds `vdoCapableSegment`
mirroring it exactly. The StoragePool controller composes the matching
`TopologySelectorTerm`s in `createStorageClassIfNotExists`. pNFS brings a third
demand, with the opposite polarity: an MDS host must be eligible, and a pNFS
client must *not* be pinned, because RWX is the point.

`NodeRequirements` (§4.4) makes the plan the single source of truth for both. The
controller service builds the plan for a `CreateVolume` request from the same pure
function the node service uses, asks each layer for its capability and its pinning
answer, and merges the results into `AccessibleTopology`. The StoragePool
controller derives its topology terms the same way.

Two demands then need one implementation rather than one per feature, and a
capability that is missing at admission is reported once rather than discovered as
a mount failure on the wrong node.

Capability advertisement itself follows PR #402: the node DaemonSet's `postStart`
hook installs and loads the kernel module through `nsenter`, writes a marker file,
and the node plugin reads the marker in the background and patches the node's
capability label. Generalizing it means a layer names its marker and its label
rather than each feature adding a pair.

**The registration race is real and belongs to the generalization.** A CSINode's
topology key set is captured once at plugin registration, seconds after the pod
starts, while the label is patched asynchronously afterward. A
`buildAccessibleTopology` that reports a capability key only when the label is
already `true` therefore reports it essentially never, which permanently breaks
the topology gate until the pod restarts. PR #402 found this as its first defect.
The key must be present at registration regardless of the label's current value,
and the label carries the answer.

---

## 11. Package Layout

Every primitive in §5 is a node-level primitive: fabric connect, `pvcreate`,
`lvcreate`, VDO, `mkfs`, `mount`, `exportfs`, and the `eui64` alias. None of them
is Kubernetes-shaped, and the striped pNFS design needs two different compositions
of the same set, one on the MDS host and one on every client. They belong in
`atlas-lib`.

| Package                      | Holds                                                                                               |
|------------------------------|-----------------------------------------------------------------------------------------------------|
| `atlas-lib/blockdev/`        | `Device`: what a Linux block device is, independent of what produced it (Appendix A)                |
| `atlas-lib/volstack/`        | `Layer`, `State`, `Artifact`, `Geometry`, the optional interfaces, the runner, and the stack record |
| `atlas-lib/volstack/layers/` | The layer implementations                                                                           |
| `atlas-lib/lvm/`             | The LVM and device-mapper primitives PR #402 wrote as `csi-driver/pkg/util/vdo.go`                  |
| `csi-driver/pkg/spdk`        | The plan: a pure function from `VolumeContext`, `VolumeCapability`, and `Role` to a layer list      |

Plan construction stays in the CSI driver because it is the one Kubernetes-shaped
part, and it is deliberately thin. The package name `volstack` is provisional
(§17 Q1).

**The role is an input, and resolving it is not part of the pure function.** A
pNFS volume has two plans over one namespace (§1), and §3's last four rows differ
by nothing else, so a plan function of the volume context and the capability alone
cannot select between them. Answering "is this node the MDS for this volume" means
asking something, which is what would cost the pure function its testability, so
it is resolved first and passed in: an impure step that reads whatever the pNFS
designs make authoritative, then `plan(vc, cap, role)` deriving the layer list
from three values and no host. Which values decide the role, and whether the
answer can change over a volume's life, belong to
[`design-pnfs-rwx.md`](design-pnfs-rwx.md) and
[`design-pnfs-striped.md`](design-pnfs-striped.md), exactly as the layers
themselves do (§2). That the plan takes it as an input belongs here.

**Phase 4 needs every role rather than the plan.** §10 has the controller service
building the plan for a `CreateVolume` request, and at that point no node has a
role yet. For a pNFS volume it therefore derives the requirements of both roles
and merges them, which is precisely where §10's note about the opposite polarity
lands: the MDS role contributes a capability and a pin, and the client role
contributes a capability and the absence of one.

**PR #402's `vdo.go` moves rather than being rewritten.** `runLVMCommand`,
`devicesArgs`, `pvVGName`, `vgExists`, `vgHasLV`, and the orphaned-node removal
are exactly the primitives §5.4 needs, and the striped pNFS layer needs the same
ones, which is the second consumer that makes the move mandatory rather than
tidy. The move is mechanical and is Phase 2 work, sequenced after PR #402 merges
rather than imposed on it: its value is in behavior validated on live hardware,
and rebasing that validation onto a package boundary buys nothing.

---

## 12. Concurrency and Locking

`util.VolumeLocks` serializes the node RPCs per volume ID today, and the runner
inherits that: one volume's stack is brought up, brought down, healed, or grown by
one goroutine at a time.

Per-volume locking is not sufficient for every layer. `lvmPV` and `lvmVolume`
invoke LVM commands that take LVM's own host-wide locks and scan every visible
device, and two volumes staging at the same moment on one host contend there
regardless of their volume IDs. The pNFS layers are worse: `/etc/exports` is one
file per host, and a striped export's volume group is active on the MDS host and
on every client at once.

**This is an open question and not a solved one (§17 Q2).** What is known is that
it is unexercised rather than proven safe: PR #402's multi-instance validation
happened to run its two stage sequences sequentially rather than overlapping, so
genuinely concurrent `vgchange` and `pvscan` calls have not raced on a real host.
That is a risk in shipped code once PR #402 merges, not only a risk in this
design.

A scope declared per layer is the candidate mechanism, specified in
[Appendix B](#appendix-b-lockscope). It is not part of the contract in §4, because
the granularity it carries depends on a measurement nobody has taken. Until §17 Q2
is answered the runner locks per volume, as the node RPCs do today.

---

## 13. Failure Modes and Fallback

| Failure                                           | Detection                                        | Behavior                                                                                                                                                                                        |
|---------------------------------------------------|--------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A layer's `Ensure` fails                          | Error returned                                   | Layers below are `Release`d top-down and never `Destroy`ed (§7.3). The RPC fails, kubelet retries, and the next `Up` converges from where this one stopped                                      |
| Backing device gone during `Release`              | Every LVM retry fails identically                | The layer's own force path runs, removing the device-mapper nodes directly with the volume-group name escaped as device-mapper escapes it (§5.4)                                                |
| An interrupted `Ensure` left an incomplete object | `Observe` reports `StatePartial`                 | `Ensure` completes the object instead of treating it as ready. A volume group with no logical volume activates successfully and produces no usable device, so the state has to be distinguished |
| A clone carries its source's LVM metadata         | `Observe` reports `StateForeign`                 | `Ensure` re-identifies the device before anything above it activates (§5.3)                                                                                                                     |
| Subsystem shared with another volume              | `nvmeof.DetachDevice` reports `SharedSubsystem`  | `fabric`'s `Release` leaves the paths up. The stack record is still removed (§8)                                                                                                                |
| csi-node pod restarted mid-bring-up               | A record exists, layers `Observe` short of ready | The restarted process re-reads the record and the next `Up` converges from there. Resuming needs no in-memory state (§6)                                                                        |
| Node rebooted with a stack recorded               | Every layer `Observe`s `StateInactive`           | `Ensure` reactivates. Nothing is created and nothing is formatted (§4.2)                                                                                                                        |
| Stack record absent at unstage                    | No file for this volume handle                   | The legacy plan is assumed: `fabric` → `filesystem` (§6)                                                                                                                                        |
| Stack record's version is not recognized          | `Version` is a schema this release does not read | The unstage fails and reports the version, on the same reasoning as an unknown layer name: a plan read under the wrong schema releases the wrong objects (§6.1)                                 |
| Stack record present but unparsable               | The file does not decode                         | The unstage fails and reports it rather than falling back to the legacy plan. That fallback answers an absent record, and a corrupt file is not evidence that a legacy stack was built (§6.1)   |
| Stack record present, plan unrecognized           | A layer name the running version does not know   | The unstage fails and reports the unknown layer, rather than silently skipping an object nobody will release. Layer names are stable across releases for this reason (§4.1)                     |
| Node lacks a layer's capability                   | The capability label is absent                   | Phase 4 keeps the volume off the node. Until then `Ensure` fails on its first command and the pod stays `Pending` (§10)                                                                         |
| Total path loss under a live stack                | `filesystem`'s `Healthy` reports the dead mount  | `NodePublishVolume` heals bottom-to-top before bind-mounting, so the pod does not inherit a dead mount (§9)                                                                                     |

---

## 14. Observability

The CSI node plugin emits no Kubernetes events and exposes no Prometheus metrics
today. Its entire observability surface is `klog`, which is why a stack that
stranded itself on a host is currently found by reading logs. Both tables below
are therefore new infrastructure in this design rather than additions to an
existing registry, and both are Phase 1 work: a layered bring-up that cannot be
observed per layer is harder to debug than the inline version it replaces, not
easier.

### Kubernetes Events

Events need a target object. The node plugin holds a `kubernetes.Interface` and a
shared `sbkube.Manager` that resolves the PV and the PVC for a volume handle
already, so the PVC is the target: it is the object a user owns and looks at, and
it outlives the pod.

| Event                                                            | Type    | Reason                         |
|------------------------------------------------------------------|---------|--------------------------------|
| A layer's `Ensure` failed and the stack was released back        | Warning | `VolumeStackEnsureFailed`      |
| A clone's device was re-identified before activation             | Normal  | `VolumeStackIdentityResolved`  |
| An interrupted object was completed rather than treated as ready | Normal  | `VolumeStackPartialCompleted`  |
| A shared subsystem was left connected for its co-tenants         | Normal  | `VolumeStackSubsystemShared`   |
| A `Release` fell back to its force path                          | Warning | `VolumeStackForceReleased`     |
| The node lacks a capability the plan requires                    | Warning | `VolumeStackCapabilityMissing` |
| A layer was healed under a live stack                            | Normal  | `VolumeStackLayerHealed`       |

### Prometheus Metrics

| Metric                                              | Labels           | Description                                                                                         |
|-----------------------------------------------------|------------------|-----------------------------------------------------------------------------------------------------|
| `simplyblock_csi_node_stack_layer_duration_seconds` | `layer`, `verb`  | Histogram of `Observe`, `Ensure`, `Release`, `Destroy`, `Heal`, and `Grow` durations per layer kind |
| `simplyblock_csi_node_stack_layer_errors_total`     | `layer`, `verb`  | Failed layer operations by kind                                                                     |
| `simplyblock_csi_node_stack_force_release_total`    | `layer`          | Releases that fell back to a force path, which is the signal that stacks are being stranded         |
| `simplyblock_csi_node_stack_observed_state_total`   | `layer`, `state` | `Observe` outcomes, so `StateForeign` and `StatePartial` are countable rather than anecdotal        |
| `simplyblock_csi_node_stacks`                       | `plan`           | Stack records present on this host, by plan shape                                                   |
| `simplyblock_csi_node_stack_records_orphaned`       | —                | Records whose volume no longer has a pod or a staging path on this node                             |

`simplyblock_csi_node_stacks` and `simplyblock_csi_node_stack_records_orphaned`
are what make the host-local record (§6) usable operationally: an orphan count
that is not zero is the alert, and the record names what to remove.

---

## 15. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-node-volume-stack.md`](../tests/test-plan-node-volume-stack.md)

- **Unit:** plan construction is a pure function and every plan in §3 must be
  derived from its volume context, capability, and role without a host. The four
  pNFS rows are the cases that matter, because they differ by role alone and a
  plan function that ignored it would return the MDS plan on a client. The runner's
  ordering, its unwind rule, and its refusal to `Destroy` on a failed `Up` are
  provable against fake layers that record their calls, which is where the
  highest-value coverage sits: a fake layer set makes "a failed `Ensure` at index
  2 releases index 1 and index 0, top-down, and destroys nothing" a table test.
  `State` classification per layer is testable against a faked host surface.
- **Integration:** the record's write-ahead ordering and its survival across a
  simulated crash, against a temporary directory rather than a cluster. The
  format (§6.1) adds a round-trip over every plan in §3, a refusal on an
  unrecognized version, and a refusal on a truncated file, which is the case a
  crash mid-rename must not be able to produce. There is
  no `envtest` component to this design in Phases 1 through 3, because nothing
  reconciles. Phase 4 adds the controller-side plan derivation and with it the
  first integration surface.
- **E2E:** every plan in §3 that is in scope, staged and unstaged on a live
  cluster with data written and checksummed across the cycle. The claims that
  only a live cluster can settle are the ones PR #402 had to settle by hand: a
  pod delete and recreate reattaches rather than reformats, a node reboot
  reattaches every stack on the host, a clone and its source coexist on one node,
  and an unclean disconnect leaves no orphaned device-mapper stack. The e2e suite
  is Ginkgo (`csi-driver/e2e`), and these are new `SPDKCSI-` blocks.
- **Load and long-running:** genuinely concurrent staging of several
  LVM-backed volumes on one host, which is the specific gap §12 names and which
  PR #402's validation did not reach. It decides §17 Q2: overlapping `pvscan` and
  `vgchange` either survive, and `lvmPV` and `lvmVolume` keep separate keys, or
  they do not, and both layers return the one key that serializes all LVM work.

Risk concentrates in §4.2 and §7.3. A `State` misclassification formats a volume
that had data, and an unwind that calls `Destroy` removes one. Those scenarios
must not be the ones cut when the schedule slips.

---

## 16. Migration Strategy

**Phase 1 changes no observable behavior.** The plan for every volume that stages
today is `fabric` → `filesystem`, and the runner performs the same connect,
format, and mount calls in the same order. The migration is that
`NodeStageVolume` stops performing them directly.

**Volumes staged before Phase 1 need no action.** They have no stack record, and
§6 defines an absent record as the legacy plan, which is the plan those volumes
were built with. Nothing is restaged, no node is drained, and a rolling upgrade of
the node DaemonSet is sufficient.

**Phase 2 moves PR #402's code rather than rewriting it.** The commands, the
derived names, the force paths, and the clone resolution move behind the contract
unchanged, because their value is validation on live hardware that a rewrite would
discard (§11).

**No VDO volume needs a legacy plan.** PR #402 and Phase 2 land in the same
release, so no released version ever stages a VDO volume without recording its
plan. The legacy plan of §6 is therefore always `fabric` → `filesystem`, and
nothing infers a plan from `client_compression` or `client_deduplication`. A
cluster tracking `main` between the two merges is the only way to reach a VDO
stack with no record, and such a volume is restaged rather than inferred.

**Phase 3 removes the special cases it replaces.** `healVolumeBeforePublish`,
`ensureDeviceConnected`, `restageVolume`, and `NodeExpandVolume`'s block-device
branch become runner calls, and the VDO conditionals PR #402 added to
`restageVolume` and `NodeExpandVolume` are deleted rather than ported.

**Phase 4 is additive on the controller side.** `dhchapAllowedNodeSegment` and
`vdoCapableSegment` are replaced by plan-derived segments that produce the same
topology keys and the same values, so existing `PersistentVolume.spec.nodeAffinity`
stays valid and no PV is rewritten.

---

## 17. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                            | Owner        |
|-----|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| 1   | **Package name.** `atlas-lib/volstack/` is provisional. The existing package names in `atlas-lib` are short concrete nouns (`nvme`, `nvmeof`, `lvol`, `nqn`), and this one is neither short nor a noun anybody uses out loud                                                                                                                                                        | —            |
| 2   | **Whether the LVM layers share one lock key or hold two.** [Appendix B](#appendix-b-lockscope) proposes the mechanism. What decides the granularity is empirical: concurrent `pvscan` and `vgchange` on one host is unexercised rather than proven safe, and the answer is a load-test result (§15). Until it is taken, the runner locks per volume                                 | —            |
| 3   | **Where the stack record lives.** Host-local (§6) works when the operator is unreachable and needs no RPC on the unstage path. Operator-side over csi-link is visible cluster-wide and finds orphans without a host sweep. Phase 1 takes the host-local file, and whether the operator-side record is an addition or a replacement is open                                          | —            |
| 4   | **Whether `Destroy` is reachable from a node RPC at all.** For an LVM stack the metadata dies with the logical volume the control plane deletes, so `Destroy` would only ever remove node-local remnants. For a pNFS export it does not, and `DeleteExport` genuinely destroys. If the answer is "only a deletion path," the node RPCs get a narrower contract than §4.1 gives them | —            |
| 5   | **`nfsExport` and `nfsMount` as layers.** §5 asserts they fit the contract. That is a claim this design cannot verify, because it does not build them. The pNFS designs are where it is settled, and a verb they cannot express is a finding against §4.1                                                                                                                           | pNFS designs |
| 6   | **Whether Phase 4 ships.** It is planned, not committed. The cost of leaving it out is a third hand-written copy of the topology pattern when pNFS lands                                                                                                                                                                                                                            | —            |

---

## Appendix A: `blockdev.Device`

The type `Artifact.Devices` holds (§4.3). It lives in `atlas-lib/blockdev` rather
than in `volstack`, because what it describes is a property of the host and not of
this contract.

```go
// Device is one Linux block device as the kernel presents it, independent of
// what produced it: an NVMe namespace, a device-mapper node, or a disk handed to
// a storage cluster at deployment.
type Device struct {
	Path              string // the canonical /dev path
	Name              string // the kernel name: "nvme0n1", "dm-3"
	Major, Minor      uint32
	LogicalBlockSize  uint32
	PhysicalBlockSize uint32
	SizeBytes         uint64
	ReadOnly          bool
}
```

**A path is not an identity, and it is not sufficient either.** `/dev/dm-3` and
`/dev/mapper/vg--name-lv` are one object under two strings whose escaping rules
§5.4 already has to reason about, and §6 says outright that a path does not
survive a reconnect. Carrying the major and minor numbers beside the path puts the
stable identifier where a layer above can compare on it.

**The block sizes are the `Geometry` argument again.** A `mkfs` aligns to the
logical block size as well as to the stripe layout, and `Geometry` does not carry
it: a virtualized device reports `Geometry{}` and still has a block size. Derived
at the call site, that is a fifth inspection of a path, which is the shape §4.3
exists to remove. Reported as a field, the `filesystem` layer reads it.

**`Device` is the intersection and not a union.** No NVMe field, no LVM field, and
no discriminator saying which produced it. A layer needing NVMe specifics resolves
`nvme.Device` from the path, and a `Device` that grew an `isNVMe` field would be
the conditional this contract replaces.

**It is a split rather than a new type.** `atlas-lib/nvme`'s `Namespace` already
carries the name, the device path, the major and minor numbers, the logical block
size, the capacity, and the read-only flag, resolved from sysfs and exercised
there. Those are facts about a block device rather than facts about NVMe.
`blockdev.Device` is that half named on its own, and `Namespace` keeps the
NVMe-specific remainder. The split is Phase 1 work because §4.3 depends on the
type, and it is independent of PR #402: it touches `nvme` and nothing PR #402
wrote.

**A second consumer puts it in `atlas-lib`.** Handing logical block devices to a
storage cluster at deployment needs the same value on the operator side, where
nothing represents a device today. A `Device`
defined inside `volstack` would be written a second time within one release.

**Resolution is deliberately absent.** The type is a value in Phase 1, following
the immutable-snapshot convention `atlas-lib/nvme` already holds to: a snapshot is
re-resolved rather than refreshed in place. A resolver reading these fields from
sysfs can be added beside it when a consumer needs one, and adding it changes
nothing a layer holds.

---

## Appendix B: `LockScope`

The candidate mechanism for §12's lock scopes. It is an appendix and not part of
§4 because §17 Q2 is open: the granularity the mechanism would carry is a
measurement, and the contract does not depend on the answer.

```go
// LockScope is implemented by a layer whose commands are not safe to run at the
// same time as the same commands for another volume. It names the lock the runner
// holds around every verb it dispatches to the layer.
type LockScope interface {
	// LockKey is the lock this layer needs held. A layer constructed for one
	// volume returns that volume's handle, which is per-volume scope. A constant
	// is host-wide scope, shared by every layer that returns it.
	LockKey() string
}
```

A layer that does not implement it is serialized per volume. `fabric` returns its
volume handle. `lvmPV` and `lvmVolume` return a key naming the LVM work, so two
volumes staging at once serialize through `pvscan` and `vgchange` while their
fabric connects still run concurrently. `nfsExport` returns a key naming
`/etc/exports`, which is one file per host.

**The scope is a key and not a named scope.** Whether `lvmPV` and `lvmVolume`
return one key or two follows from whether overlapping `pvscan` and `vgchange` are
safe. As a key that answer changes a returned value; as a choice between "per
volume" and "host-wide" it would change the contract.

The keys are acquired against a registry in `atlas-lib/locks`, generalized from
`csi-driver/pkg/util`'s `VolumeLocks`: the same map of mutexes, keyed by a string
instead of by a volume ID, with `locks.WithLock` scoping each acquisition to one
call so that a verb returning early or panicking cannot leave a key held.
`VolumeLocks` becomes a caller of it rather than a second implementation.

A verb runs entirely inside its own acquisition, so work owed to the lock happens
in the verb and cleanup before release is a `defer` there. `sync.Mutex` is not
reentrant, which makes a key held by a composite layer unavailable to its members
(§5.2): a member needing the LVM key cannot be run by a parent already holding it.

Adopting the mechanism adds one metric to §14,
`simplyblock_csi_node_stack_lock_wait_seconds`, labeled by `layer` and by scope
kind. It is never labeled by the key, because a volume handle is unbounded label
cardinality.
