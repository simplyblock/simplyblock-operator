# Design Document: Live Consistency-Group Subsystem Flip

**Status:** Partially Implemented  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-11-03  
**Test Plan:** [`tests/test-plan-cg-live-subsystem-flip.md`](../tests/test-plan-cg-live-subsystem-flip.md)

---

## Phasing Overview

| Phase       | Status                          | Scope                                                                                                                                                                                       | Sections       |
|-------------|---------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------|
| **Phase 1** | Implemented, not live-validated | Every CSI node stack shape, the LVM and VDO shapes included, staged behind the `dmLinear` indirection by default, plus the node swap service, its watchdog, and restart recovery            | §6             |
| **Phase 2** | Implemented, not live-validated | The `JoinConsistencyGroup` action on `PersistentVolumeOps`, created by the membership watcher, driving the migration, the join, the subsystem selection, and the coordinated namespace move | §4, §5, §7, §8 |
| **Phase 3** | Blocked on SPDK                 | Make-before-break: the namespace is exported by the target, gated `inaccessible` by ANA, before it leaves the source, so the window shrinks to the ANA switch and the reload (P0-7)         | §6.7           |

Phase 1 changes nothing a user sees: a volume staged after it carries one more
device-mapper node under its consumer, and nothing re-points that node yet.
Phase 2 depends on Phase 1 being deployed on every node that consumes a group
member, and on the control-plane API in Phase 0. Phase 3 depends on P0-7 alone.
Until it ships, the break-before-make window of §6 is the bound on the pause.

Phases 1 and 2 are built in this repository and in sbcli, and are covered by
unit tests, `envtest` suites, and sbcli's integration tier against FoundationDB.
Neither has run against a live cluster, a real `dmsetup`, or a real NVMe-oF
target, so every end-to-end and manual scenario in the test plan is open.

---

## Phase 0 — External Prerequisites

| #     | Prerequisite                                                                                                                                                                                                                                                     | Kind                    | Blocks                              | Status                                                                                                                  |
|-------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|-------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| P0-1  | A subsystem-move resource on the consistency group: create, prepare, move, commit, roll back, and read (§9)                                                                                                                                                      | Control plane (`sbcli`) | Phase 2                             | Implemented on the sbcli branch, not released                                                                           |
| P0-2  | The join plan (`POST .../members/plan`) carries the colocation outcome: the target subsystem, whether it is new, the members that would move, and a capacity refusal (§5.5)                                                                                      | Control plane (`sbcli`) | Phase 2                             | Implemented on the sbcli branch, not released                                                                           |
| P0-3  | An active subsystem move refuses a group snapshot, a group migration, a second move on the group, and any namespace-slot claim into its source or target subsystems. The target's slots are reserved inside the claim transaction                                | Control plane (`sbcli`) | Phase 2                             | Implemented on the sbcli branch, not released                                                                           |
| P0-4  | The move call refuses to start removing namespaces once the operation's absolute suspend deadline has passed (§6.5)                                                                                                                                              | Control plane (`sbcli`) | Phase 2                             | Implemented on the sbcli branch, not released                                                                           |
| P0-5  | A fresh subsystem is created on every node of the group's HA set, with listeners whose ANA state mirrors the group's current subsystem on that node. No single helper does this today. The work is spread over `add_lvol_on_node` and `publish_lvol_listeners`   | Control plane (`sbcli`) | Phase 2, fresh-subsystem path       | Partly implemented: the fresh subsystem exists on every HA node, and its listeners follow the publish rule (§5, Step 4) |
| P0-6  | A namespace removed from one subsystem can be added to another subsystem on the same node with the same UUID and NGUID, at an nsid shared by every HA node. `move_namespace` implements it and is tested against fakes only                                      | Storage plane (SPDK)    | Phase 2                             | Unknown on a live target                                                                                                |
| P0-7  | One bdev added as a namespace to a second subsystem while the first still exports it, through a shared-write claim (`SPDK_BDEV_CLAIM_READ_MANY_WRITE_SHARED`), with ANA keeping only one subsystem writable. Today, the namespace takes an exclusive-write claim | Storage plane (SPDK)    | Phase 3                             | Requested. The exclusive claim refuses it today (SPDK team, 2026-11-02)                                                 |
| P0-8  | The host kernel rescans a controller on the namespace-attribute-changed event over NVMe/TCP, and `nvme ns-rescan` is available in the CSI node image as the fallback                                                                                             | Node OS                 | Phase 2                             | Expected, not measured                                                                                                  |
| P0-9  | On a stretched cluster the move applies to the subsystem on both sites, the remote triplet included, with the closed site's listeners kept `inaccessible`                                                                                                        | Control plane (`sbcli`) | Phase 2 on stretched clusters       | Not shipped. The control plane refuses colocation on a stretched cluster                                                |
| P0-10 | A storage node restarted while a move is active restores the move's namespaces where the move record says they are. Today, the restart rebuilds them from the volume records, which name the source until commit                                                 | Control plane (`sbcli`) | Phase 2, node restart inside a move | Not shipped                                                                                                             |
| P0-11 | The control plane reports the host NQNs connected to a subsystem, so the operator can find a consuming host that is not a Kubernetes node (§8.2)                                                                                                                 | Control plane (`sbcli`) | Phase 2, hosts outside Kubernetes   | Not shipped                                                                                                             |

Without P0-1 through P0-6 a join still works the way it does today: the volume
migrates to the pinned node and joins, and its namespace stays in its own
subsystem, which `design-consistency-groups.md` §4.2 already accepts as valid.
Without P0-9 the operator refuses colocation on a stretched cluster and the
join completes without it.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API: The JoinConsistencyGroup Action](#4-api-the-joinconsistencygroup-action)
5. [Subsystem Selection](#5-subsystem-selection)
6. [Host Device Swap](#6-host-device-swap)
7. [State Machine](#7-state-machine)
8. [Controller Design](#8-controller-design)
9. [Backend API Requirements](#9-backend-api-requirements)
10. [Configuration](#10-configuration)
11. [Failure Modes](#11-failure-modes)
12. [Observability](#12-observability)
13. [Testing Strategy](#13-testing-strategy)
14. [Migration Strategy](#14-migration-strategy)
15. [Open Questions](#15-open-questions)

- [Appendix A: `persistentvolumeops_types.go`](#appendix-a-persistentvolumeops_typesgo)
- [Appendix B: `swap.proto`](#appendix-b-swapproto)

---

## Overview

A volume joins a consistency group when its PVC gains the label
`storage.simplyblock.io/consistency-group`. A member has to live on the group's
pinned node and logical volume store, because the group snapshot runs on one
store. This design adds the second half: the member's NVMe namespace is also
moved into the group's NVMe subsystem, and the move happens while the pod keeps
running, with its I/O paused for a bounded window and never failed.

The label event starts one `PersistentVolumeOps` with action
`JoinConsistencyGroup`. The operation runs four stages in order:

1. **Migrate:** when the volume is off the pinned node, a child `Migrate`
   operation moves it there with live migration. Its subsystem moves with it.
2. **Join:** the control plane records the membership.
3. **Select:** the control plane picks the subsystem the group ends up in (§5).
   When the group's subsystem has a free slot, only the new member moves. When
   it is full, the whole group moves to another subsystem on the same logical
   volume store that has room for every member, or to a fresh one. The
   operation is refused only when the node has reached its subsystem limit.
4. **Flip:** every moving namespace leaves its subsystem and enters the target,
   while each host that consumes one of them holds its I/O (§6).

The host side is what makes the flip live. A namespace in another subsystem
appears to Linux as a new block device, because native NVMe multipath groups
paths per subsystem. So the CSI node stages every volume, VDO volumes included,
on a `dmLinear` device-mapper node (`/dev/mapper/sb-<lvol>`), and the consumer
(the filesystem, the LVM physical volume under VDO, or the pod's raw block
device) only ever opens that node. During the flip the node suspends the
mapping, the control plane moves the namespaces, and the node reloads the
mapping onto the new device and resumes it. Queued I/O continues on the new
path.

| The group's subsystem | Another subsystem on the store has room for all members | Node below its subsystem limit | Outcome                                                           |
|-----------------------|---------------------------------------------------------|--------------------------------|-------------------------------------------------------------------|
| Has a free slot       | (not asked)                                             | (not asked)                    | The new member moves into it                                      |
| Full                  | Yes                                                     | (not asked)                    | The whole group moves into that subsystem                         |
| Full                  | No                                                      | Yes                            | The whole group moves into a fresh subsystem                      |
| Full                  | No                                                      | No                             | Refused with `SubsystemLimitReached`. The volume does not migrate |

A label changed from one group to another starts the same action for the new
group. The volume first leaves the old one (§8.5): when it shares the old
group's subsystem, it is split into a fresh subsystem of its own with the same
host swap, and the control plane then closes its membership. The four stages
above follow for the new group. A volume never rejoins a group it has left
(`design-consistency-groups.md` §4.3).

---

## 1. Background

`design-consistency-groups.md` §4.5 lets a volume join a group after it was
provisioned. The CSI controller's membership watcher
(`csi-driver/internal/csi/controller/cg_membership_watcher.go`) watches PVCs for
the label and calls the control plane's members endpoint. A join refused for
placement becomes a `VolumeMigration` named `cg-join-<lvol>` to the pinned
node, and the watcher joins on the next pass once it completed. After the join
it can call `POST .../members/{lvol}/colocate`, gated by `SPDKCSI_CG_COLOCATE`,
which is off by default.

The colocation step is built on the control-plane side and switched off. The
sbcli module `cg_colocation.py` plans the join (`plan_late_join`), finds the
group's subsystem (`group_subsystem_nqn`), and moves a namespace between two
subsystems on every HA node (`move_namespace`). The move is break-before-make:
the namespace is removed from its subsystem and added to the target with the
same UUID and NGUID. It is refused for any volume a host is connected to, and
`NAMESPACE_MOVES_ENABLED` is `False`. When the group's subsystem is full it
flips one non-member namespace out to make room, and it refuses when every
namespace there belongs to a group. A move through a fresh subsystem is not
built.

The node side has the first piece. `atlas-lib/volstack/layers/dmlinear.go`
maps the whole namespace device through `dm-linear`, and its `Heal` re-points
the mapping with `dmsetup suspend`, `reload`, and `resume`
(`atlas-lib/devmapper`). Only the `Plain` and `RawBlock` shapes have an indirect
form, and only when the node runs with `SPDKCSI_DM_INDIRECTION`. The LVM shapes,
which carry VDO, have none: their three LVM layers are not healers and sit
directly on the fabric device. Nothing on the node triggers a swap. `Heal` runs
only from `NodePublishVolume` and from a restage of a dead mount, and no
service on the node accepts a request to swap.

Three things block a live flip today:

- **The client cannot follow the namespace.** Every volume staged as `Plain`,
  `RawBlock`, or an LVM shape opens the `nvmeXnY` device directly, and that
  device disappears when its namespace leaves the subsystem.
- **Nothing coordinates the two sides.** The control plane removes the
  namespace without knowing whether any host has paused its I/O, and in-flight
  I/O on a removed namespace fails rather than queues.
- **A full subsystem stops the join.** Only a non-member can be flipped out, and
  only into the new member's own subsystem.

---

## 2. Goals and Non-Goals

### Goals

- A PVC labeled with a group name ends with its volume on the group's pinned
  node and logical volume store, a member of the group, and in the same NVMe
  subsystem as every other open member, without a pod restart.
- A consumer of any moving namespace sees no I/O error during the flip. Its I/O
  is held for at most the operation's suspend timeout, 10 seconds by default.
- When the group's subsystem is full, the whole group moves to another
  subsystem on the same logical volume store with enough free slots, or to a
  fresh one. No volume outside the group is moved.
- The operation is refused before anything moves when the pinned node cannot
  hold the subsystems the operation needs, and the refusal names the limit.
- Every shape the CSI node stages, VDO and raw block included, can be flipped.
- Every step survives an operator restart, a CSI node restart, and a
  control-plane error, and either finishes the flip or leaves every namespace
  in its source subsystem with every host resumed.
- Every wait and every refusal is visible as an event on the PVC.

### Non-Goals

- **Moving data.** The flip changes how a namespace is exported, never where its
  data lives. The volume reaches the pinned store by live migration
  (`design-persistentvolumeops.md` §5), which this design only invokes.
- **Moving a member out of the group's subsystem when its label is removed.**
  Sharing a subsystem is an efficiency, and a detached member stays where it is.
- **Flipping a non-member out to make room.** A consumer that asked for nothing
  is never paused. The `choose_flip_victim` path in sbcli is not used by this
  flow.
- **Striped volumes.** A volume staged through the `members` layer spans several
  logical volumes. A group refuses it at join, as today.
- **Hosts outside Kubernetes.** A volume whose subsystem has a connected host
  with no CSI node peer on the link cannot be flipped. Once P0-11 ships, the operation fails
  before anything is suspended (§11).
- **Retrofitting the indirection under a staged volume.** A volume staged
  without `dmLinear` gains it at its next stage, never under a live consumer
  (§14).

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Control Plane                          │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ CSI controller: membership watcher (spdkcsi-cg-membership)         │  │
│  │  1. PVC gains storage.simplyblock.io/consistency-group             │  │
│  │  2. creates PersistentVolumeOps cg-join-<lvol>-<group>             │  │
│  │     action JoinConsistencyGroup, creatorRef unset                  │  │
│  │  3. label removed: DELETE .../members/{lvol} (unchanged)           │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ Operator: PersistentVolumeOps reconciler (controllers/volume)      │  │
│  │  1. Planning: plan + capacity preflight                            │  │
│  │  2. Migrating: child PersistentVolumeOps action Migrate            │  │
│  │  3. Joining → SelectingSubsystem → PreparingTarget                 │  │
│  │  4. AttachingPaths → Suspending → MovingNamespaces → Resuming      │  │
│  │  5. Committing → ReleasingPaths                                    │  │
│  │  lock: storage.simplyblock.io/active-ops on every PV it moves      │  │
│  └───────────────┬───────────────────────────────────┬────────────────┘  │
│                  │ csi-link hub (atlas-lib/link)     │                   │
│  PersistentVolumeOps  spec.joinConsistencyGroup  status.joinConsistency  │
└──────────────────┼───────────────────────────────────┼───────────────────┘
                   │ gRPC SwapService                  │ HTTP (webapi)
┌──────────────────▼───────────────┐  ┌────────────────▼───────────────────┐
│ CSI node plugin, per host         │  │ simplyblock control plane          │
│  Describe / Prepare / Suspend /   │  │  POST .../members/plan             │
│  Resume / Release                 │  │  POST .../members                  │
│  dmLinear sb-<lvol> under every   │  │  POST .../subsystem-moves          │
│  shape, watchdog, restart scan    │  │  POST .../{move}/prepare|move|     │
└───────────────────────────────────┘  │        commit|rollback             │
                                       └────────────────────────────────────┘
```

**One orchestrator owns both sides.** The control plane cannot reach a CSI
node, and a CSI node does not know when the control plane will move a
namespace. The operator talks to both: to the control plane over its existing
HTTP client, and to each CSI node over the csi-link hub, where every CSI node
is already a registered peer (`link.NodePeer(node)`). The move is therefore a
step machine in the operator, persisted in the `PersistentVolumeOps` status, and
each side only executes single, idempotent steps.

**The watcher stays thin.** It turns a label into one operation and a label
removal into one detach. Every decision past that point is the operator's.

---

## 4. API: The JoinConsistencyGroup Action

`PersistentVolumeOps` (`design-persistentvolumeops.md`) gains a third action.
The joining volume is the operation's target, because the join is something done
once to that volume, and the group is a control-plane object with no
Kubernetes kind. The group's other members are affected, and the operation locks
them (§8.3) rather than naming them.

```go
// +kubebuilder:validation:Enum=Migrate;Relocate;JoinConsistencyGroup
type PersistentVolumeOpsAction string

// JoinConsistencyGroup parameterizes action JoinConsistencyGroup.
// +optional
JoinConsistencyGroup *JoinConsistencyGroupSpec `json:"joinConsistencyGroup,omitempty"`
```

`spec.joinConsistencyGroup.groupName` names the group by its control-plane name,
which is the label's value. It is not a Kubernetes reference, so the admission
webhook checks its format and the controller resolves it (§7, `Planning`).
`spec.joinConsistencyGroup.suspendTimeoutSeconds` bounds how long any host may
hold I/O, from 1 to 30 seconds and 10 by default. The spec is immutable in its
entirety, as for every action of the kind.

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: PersistentVolumeOps
metadata:
  name: cg-join-6f0c2b7e-0b8e-4b7e-9d0a-3c1f8f6a2d11-3f9a0c12
  labels:
    storage.simplyblock.io/purpose: consistency-group-join
    app.kubernetes.io/managed-by: spdkcsi
spec:
  persistentVolumeName: pvc-41f6e8a2-5d4c-4f63-9a32-0b8f1c2d7e90
  action: JoinConsistencyGroup
  joinConsistencyGroup:
    groupName: orders-db
    suspendTimeoutSeconds: 10
status:
  phase: Running
  step:
    state: Resuming
  joinConsistencyGroup:
    groupUUID: 2d6c9f0e-7a41-4c1b-8e5d-9f3a0b6c1d24
    moveID: 8a1f3c5e-2b7d-4e90-a6c4-1d3e5f7a9b02
    targetNQN: nqn.2023-02.io.simplyblock:cluster:lvol:5c0e...
    targetIsNew: true
    namespacesAt: Target
    suspendDeadline: "2026-11-02T10:14:37Z"
    volumes:
      - volumeUUID: 6f0c2b7e-0b8e-4b7e-9d0a-3c1f8f6a2d11
        persistentVolumeName: pvc-41f6e8a2-5d4c-4f63-9a32-0b8f1c2d7e90
        sourceNQN: nqn.2023-02.io.simplyblock:cluster:lvol:6f0c...
        targetNSID: 3
    hosts:
      - node: worker-2
        instanceUID: 0b9c5a1e-6f3d-4a2b-8c7e-5d1f0a3b9c64
        prepared: true
        suspended: true
        resumed: false
```

The status records everything a restarted reconciler needs to finish or undo
the flip: the move's control-plane identity, the target, which side the
namespaces are on now, the deadline every host was given, and per host whether
each step landed. `hosts[].instanceUID` is the CSI node process the host's step
was acknowledged by. A changed value means the node restarted, and the
reconciler re-verifies that host before moving any namespace (§6.6). Appendix A
has the full additions to the type.

A `PersistentVolumeOps` with action `JoinConsistencyGroup` can also be written
by hand, which is how an existing member outside its group's subsystem is
brought into it. The watcher creates one for a labeled volume that belongs to no
group, or to a group other than the label's, and only when none exists for the
volume and that group (§8.1). Its name is `cg-join-<lvol>-<group>`, where
`<group>` is the first eight hex digits of the SHA-256 of the group name, so a
volume relabeled to a second group gets a second operation. When the volume is
a member of another group, `status.joinConsistencyGroup.leave` records the
group it leaves and how far the leave got (§8.5).

---

## 5. Subsystem Selection

The control plane decides the target, inside the transaction that reserves its
slots, because only the control plane has a consistent view of every
subsystem's occupancy. The operator asks for the decision twice: once as a
preflight before the volume migrates (§5.5), and once to commit to it (§7,
`SelectingSubsystem`). This section specifies the rule the control plane
applies, so that the operator's tests can fake it faithfully.

### Step 1 — Inputs

The group `G` is pinned to node `N`, logical volume store `L`, and pool `P`.
`M` is the set of open members, and `J` is the joining volume, already on `N`
after the migration. A candidate subsystem `s` is a shared subsystem
(`max_namespace_per_subsys > 1`) on `N`, aligned to `P`, with no active
migration or subsystem move. Its capacity is
`cap(s) = min(max_namespace_per_subsys, MAX_NAMESPACES_PER_SUBSYSTEM)`, where the
constant is 50, and `used(s)` counts its live namespaces.

### Step 2 — The group's subsystem has room

The group's subsystem `g` is the one holding the most open members on `N`
(`group_subsystem_nqn`). When `G` has no member in a shared subsystem, `J`'s own
subsystem becomes the group's and nothing moves. When `used(g) < cap(g)`, the
moving set is `{J}` and the target is `g`.

### Step 3 — Another subsystem has room for the whole group

Otherwise, the moving set for a candidate `s` is every member of `M ∪ {J}` that
is not already in `s`. A candidate fits when `cap(s) − used(s)` is at least the
size of its moving set. Among the candidates that fit, the target is the one
with the smallest moving set, so the fewest consumers are paused. Ties go to the
fuller candidate, which keeps free subsystems free, and then to the lower NQN,
so a retried decision is deterministic. `J`'s own subsystem is a candidate like
any other.

### Step 4 — A fresh subsystem

When no candidate fits, the target is a new subsystem on `N`, created with
capacity `MAX_NAMESPACES_PER_SUBSYSTEM` on every node of the group's HA set
(P0-5). The moving set is `M ∪ {J}`. Its listeners follow the rule every new
subsystem is published with: `optimized` on the group's primary node and
`non_optimized` on the other nodes of the HA set. That matches the group's
subsystem `g` whenever `g`'s leadership has not moved since it was published. A
stretched cluster, whose closed site needs `inaccessible` listeners, is refused
before this step (P0-9).

### Step 5 — The node's subsystem limit

A fresh subsystem is allowed when the count of primary subsystems on `N`
(`count_lvol_subsystems`) is below `max_subsystems_for_node(N)` after the
subsystem `J` brings with it. Otherwise, the operation is refused with
`SubsystemLimitReached`, the control plane's 409, and the message names the node
and its limit. This is the only refusal for capacity.

The preflight in `Planning` applies the same rule to the state the node will
have after the migration, counting `J`'s subsystem as already on `N`. A refused
preflight fails the operation before the migration starts, so a volume is never
moved to a node that cannot take it into the group.

A source subsystem left with no namespace after the move is deleted at commit,
on every HA node, so a flip into a fresh subsystem can leave the node's
subsystem count unchanged.

### Algorithm Summary

```
select_target(G, J):
    g := group_subsystem_nqn(G)
    if g == "":                       return Target(J.nqn, moving={})
    if used(g) < cap(g):              return Target(g, moving={J})
    best := none
    for s in candidates(N, P) where s not migrating and s not moving:
        moving := (M ∪ {J}) − members_in(s)
        if cap(s) − used(s) >= |moving|:
            best := min(best, (|moving|, −used(s), s.nqn))
    if best != none:                  return Target(best.s, moving=best.moving)
    if count_subsystems(N) < max_subsystems(N):
                                      return Target(new, moving=M ∪ {J})
    refuse SubsystemLimitReached(N, max_subsystems(N))
```

### 5.5 The Preflight

`POST .../members/plan` today returns the steps `migrate`, `join`, and
`colocate`, the migration scope, and the target node. P0-2 adds a `colocation`
object computed by Steps 2 to 5 against the predicted state: the target NQN or
`new`, the moving volume IDs, and a refusal reason. The preflight reserves
nothing. `SelectingSubsystem` repeats the decision against the current state and
reserves the target's slots in the same transaction, so a concurrent create
cannot take them between the decision and the move.

---

## 6. Host Device Swap

### 6.1 Every Shape Behind the Indirection

Every shape the CSI node stages gets an indirect form, with `dmLinear` directly
above `fabric`. The consumer, whatever it is, opens `/dev/mapper/sb-<lvol>` and
never the `nvmeXnY` device.

| Shape               | Layers today                                                                          | Indirect form                                                                                            |
|---------------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------|
| `RawBlock`          | `fabric`                                                                              | `fabric` → `dmLinear` (exists, flag-gated)                                                               |
| `Plain`             | `fabric` → `filesystem`                                                               | `fabric` → `dmLinear` → `filesystem` (exists, flag-gated)                                                |
| `LVM` (VDO)         | `fabric` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem` | `fabric` → `dmLinear` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem` (new) |
| `LVMRawBlock` (VDO) | the `LVM` row without `filesystem`                                                    | the indirect `LVM` row without `filesystem` (new)                                                        |
| `Striped`           | `members` → the `LVM` row                                                             | none (§2, Non-Goals)                                                                                     |

For the VDO shapes, the physical volume label is written through the mapping, so
LVM identifies its physical volume as `/dev/mapper/sb-<lvol>`. Every LVM
invocation is already scoped with `--devices` (`atlas-lib/lvm/lvm.go`), and the
scope names the mapping, so LVM never scans the `nvmeXnY` device that carries
the same label. The VDO volume, its pool, and the volume group are untouched by
a swap: only the bottom mapping's table changes, and the layers above it hold
the mapping open throughout.

`SPDKCSI_DM_INDIRECTION` defaults to on, and the operator sets it to `false` only
when the SimplyblockDriver sets `spec.disableDeviceMapperIndirection`. A join that
meets a volume staged without the layer names that field when it is set, since a
restage would leave the layer out again. The node plugin also probes the host at
start (`modprobe dm_mod`, then `dmsetup targets` must list `linear`). A host without
it stages directly whatever the variable says, and the plugin logs a warning naming
the node and what is lost. The stack record still decides the shape
of a staged volume, so a volume never gains or loses the layer under a live
consumer (§14).

### 6.2 The Swap Service

The CSI node registers a `SwapService` on its link session and advertises the
capability `volume-swap` in its hello. Appendix B has the protocol. Every call
names volumes by their CSI volume handle and is idempotent.

| Call                         | Effect                                                                                                                                      | Answer when repeated                      |
|------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------|
| `Describe(handles)`          | Reports, per volume: staged, swappable (the record has `dmLinear`), suspended, and the NQN and namespace UUID the mapping points at         | Same                                      |
| `Prepare(handles, target)`   | Connects the target subsystem's paths from the connect entries the control plane returned for it                                            | Already connected                         |
| `Suspend(handles, deadline)` | Suspends each volume's `sb-<lvol>` mapping and starts the watchdog for `deadline`                                                           | Already suspended, with the same deadline |
| `Resume(handles, nqn)`       | Finds each namespace by UUID under `nqn`, reloads the mapping onto it, resumes it, and rewrites the `fabric` parameters in the stack record | Already resumed on that device            |
| `Release(nqn)`               | Disconnects the subsystem when no staged volume on the host points at it                                                                    | Already disconnected                      |

### 6.3 Suspend

`Suspend` runs `dmsetup suspend --nolockfs` on each mapping. Without `--noflush`
the kernel completes every in-flight request on the current device before the
call returns, so no request is outstanding on a namespace that is about to be
removed. New requests queue in device-mapper. `--nolockfs` skips the filesystem
freeze of the `Plain` shape: the swap is a path change, not a consistency point,
and a freeze would flush dirty pages inside the window. The VDO shapes have no
filesystem on the mapping, so the flag changes nothing there.

The current `devmapper.Swap` suspends, reloads, and resumes in one call with no
flags. The service splits it into a suspend and a reload-and-resume, because the
control plane's move runs between them.

### 6.4 Resume

`Resume` locates the namespace by its UUID among the controllers of the given
NQN, the same identity-first lookup `fabric` already uses. When the namespace
has not appeared, the node issues `nvme ns-rescan` on that controller and polls
until the deadline. It then reloads the mapping with the new device's
`major:minor`, resumes it, and rewrites the `fabric` layer's recorded `nqn` and
`nsid`, so a later restage or heal connects to the subsystem the namespace is in.
A reload refused by the kernel leaves the mapping suspended and returns the
error, and the reconciler decides between retrying and rolling back (§7).

The same call is the rollback: with the source NQN it resumes the volume on its
source subsystem once the control plane put the namespace back there.

A rollback is only ever decided while the hosts are known to be held. Once the
deadline has passed, a host's watchdog may have resumed it on the target, so the
reconciler first asks the control plane where the namespaces are, and a move
that happened goes forward from there: each host is resumed on the target as it
can be reached, and the move is committed, whatever step asked for the rollback
and even if the operation ends Failed. Status alone cannot answer, because a
pass can die between the move call and the write that records it (§11).

The kernel admits a namespace only while no other subsystem on the host holds
its identity (UUID and NGUID), and logs `ignoring nsid N because of duplicate
IDs` otherwise. A controller of the source subsystem that is not live, such as
one still reconnecting to a node a live migration left, never processes the
namespace's removal and keeps holding the identity, so the target's copy would
be ignored however often it is rescanned. On the first miss, `Resume` therefore
disconnects the source's controllers that are neither live nor being deleted,
through their `delete_controller` attribute, before it rescans the target. Such
a controller carries no I/O. A resume onto the source itself, which is the
rollback, drops nothing.

### 6.5 The Deadline and the Watchdog

The operator computes one absolute deadline before `Suspending`: now plus
`suspendTimeoutSeconds`. It sends the deadline to every host in `Suspend` and to
the control plane in the move call. The control plane refuses to begin removing
namespaces once the deadline has passed (P0-4). It checks the deadline once, before
the first removal: a move that has started runs to the end, or restores every
source and reports `namespaces_at: source`. The node's watchdog fires at the
deadline plus a 5-second grace, which absorbs clock skew between the operator,
the control plane, and the host.

When the watchdog fires on a mapping that is still suspended, the node resolves
it alone:

1. It looks for the namespace UUID under the target NQN, then under the source
   NQN, and reloads onto whichever holds it.
2. It resumes the mapping.
3. When neither holds the namespace, it resumes on the current table, so the
   consumer gets I/O errors rather than an indefinite hang, and it emits
   `SubsystemSwapWatchdogFired` as a Warning on the PVC.

### 6.6 Node Restart While Suspended

A suspended mapping is kernel state and outlives the CSI node process, while the
watchdog does not. At startup the node lists every `sb-*` mapping, and resolves
each suspended one as the watchdog would. The reconciler sees the restart
through the host's link instance UID. Before `MovingNamespaces`, it re-runs
`Describe` on every host. A host whose instance UID changed since `Suspending`,
or that reports a volume no longer suspended, sends the operation to the
rollback path, because that host may already have resumed on the source device.

### 6.7 Make-Before-Break (Phase 3)

SPDK allows one writer per bdev, and a namespace takes an exclusive-write claim
on its bdev, so the same bdev cannot be added to a second subsystem today
(P0-7). The writer limit holds without that exclusivity when ANA decides which
subsystem accepts I/O, and Phase 3 rests on that:

1. **Add, gated:** the control plane adds the namespace to the target under a
   shared-write claim, with the target's ANA state `inaccessible`. The target
   rejects every I/O, so the source stays the only writer.
2. **Appear:** the host already holds the target's paths from `AttachingPaths`,
   sees the target device, and `Prepare` waits for it.
3. **Switch:** the host suspends, the control plane sets the source
   `inaccessible` and the target `optimized`, and the host reloads and resumes.
   The window is two ANA changes and a table reload, with no namespace removal
   and no rescan inside it.
4. **Remove:** the control plane removes the source namespace after `Resuming`.

ANA state is set per listener, so it covers every namespace of a subsystem. A
shared source subsystem holds volumes that are not moving, so the switch has to
act on the moving namespaces alone, through their own ANA group (`anagrpid`).
The step machine keeps its shape: `MovingNamespaces` becomes an add before
`Suspending` and a remove after `Resuming`, and the switch runs inside the
window.

Until P0-7 ships, the move stays break-before-make. Its window is the control
plane's remove-and-add time plus the host's rescan (§6.4), and
`simplyblock_cg_subsystem_move_window_seconds` measures it.

---

## 7. State Machine

The action declares its own graph (`statemachine.MultiConfig` keyed by the
action), persisted in `status.step`. Each step is persisted before its side
effect, and every side effect is a call that answers the same when repeated.
`Migrating`, `AttachingPaths`, and `Committing` reuse the kind's existing step
names.

```
  PVC labeled; watcher creates the operation
    │
    ▼
  Planning            ← resolve the group; read the volume's current group
    │  a member of another group ──────────────────────▶ Leaving (§8.5)
    │  POST .../members/plan; preflight (§5.5)
    │  on the pin                          │  off the pin
    │                                      ▼
    │                                Migrating          ← child PersistentVolumeOps Migrate
    │                                      │  child Succeeded
    ▼                                      ▼
  Joining             ← POST .../members (idempotent)
    │
    ▼
  SelectingSubsystem  ← POST .../subsystem-moves; nothing to move → Succeeded
    │
    ▼
  PreparingTarget     ← POST .../{move}/prepare (creates a fresh subsystem)
    │  no moving volume is attached
    │  ─────────────────────────────────────────────▶ MovingNamespaces
    ▼
  AttachingPaths      ← Describe + Prepare on every consuming host
    │  every host prepared and swappable
    ▼
  Suspending          ← persist suspendDeadline; Suspend on every host
    │  every host suspended
    ▼
  MovingNamespaces    ← Describe again; POST .../{move}/move with the deadline
    │  namespacesAt = Target
    ▼
  Resuming            ← Resume(target NQN) on every host
    │
    ▼
  Committing          ← POST .../{move}/commit (records switched, empty sources deleted)
    │
    ▼
  ReleasingPaths      ← Release(source NQN) on every host
    │
    ▼
  Succeeded
```

A volume leaving another group passes through `Leaving` twice (§8.5). The
first pass, when the volume shares the old group's subsystem, runs
`SelectingSubsystem` through `ReleasingPaths` against the old group with a
leave move, which splits the volume into a fresh subsystem. The second pass
detaches the volume from the old group, then plans the join and continues at
`Migrating` or `Joining`. A volume alone in its subsystem skips the swap and
runs the second pass at once.

The rollback path is the same `Resuming` step pointed at the source. Once a
failure after `AttachingPaths` is classified, the reconciler calls
`POST .../{move}/rollback` when any namespace may have left its source, records
`namespacesAt: Source`, and runs `Resuming` with the source NQN on every host.
It then fails the operation with `ReleasingPaths` disconnecting the target's
paths, and the control plane deletes a fresh target subsystem that is empty.

`spec.abort` is honored from `Planning`, `Migrating` (forwarded to the child),
`SelectingSubsystem`, `PreparingTarget`, and `AttachingPaths`. From `Suspending`
on, the window is bounded by the deadline, and the operation finishes or rolls
back by itself. A join that already happened stays: membership is one-way
(`design-consistency-groups.md` §4.3). `Leaving` is not abortable either,
because its second pass detaches the volume, which is also one-way.

The admission webhook refuses to delete the operation while its step is
`Joining`, `Suspending`, `MovingNamespaces`, `Resuming`, or `ReleasingPaths`,
the steps whose side effect cannot be abandoned halfway. A delete that reaches
the finalizer anyway still runs the commit or the rollback below.

| Condition                                                                                 | Step                 | Result                                                                                                                                                                                           |
|-------------------------------------------------------------------------------------------|----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Group name resolves to nothing                                                            | `Planning`           | Held as `Pending` with `ConsistencyGroupPending`, requeued                                                                                                                                       |
| Volume in another group, another pool, or a sibling in another group                      | `Planning`           | `Failed`, `ConsistencyGroupJoinRefused`                                                                                                                                                          |
| Leaving: the node cannot hold the fresh subsystem the split needs                         | `SelectingSubsystem` | `Failed`, `SubsystemLimitReached`. The volume stays a member of its old group, in the old group's subsystem                                                                                      |
| Leaving: the split is rolled back                                                         | any swap step        | `Failed` after the rollback path. The volume stays a member of its old group, in the old group's subsystem                                                                                       |
| Leaving: the detach returns 5xx                                                           | `Leaving`            | Retried. The volume is already in its own subsystem                                                                                                                                              |
| Preflight refuses for capacity                                                            | `Planning`           | `Failed`, `SubsystemLimitReached`. Nothing migrated                                                                                                                                              |
| Child migration fails or aborts                                                           | `Migrating`          | `Failed`, `ConsistencyGroupMigrationFailed`. The volume stays where the child left it                                                                                                            |
| Members endpoint returns 409                                                              | `Joining`            | `Failed`, `ConsistencyGroupJoinRefused`                                                                                                                                                          |
| Selection refused for capacity                                                            | `SelectingSubsystem` | `Failed`, `SubsystemLimitReached`. The volume is a member in its own subsystem                                                                                                                   |
| The control plane refuses colocation, as on a stretched cluster                           | `SelectingSubsystem` | `Succeeded`, `ConsistencyGroupColocationRefused`. The volume is a member in its own subsystem                                                                                                    |
| Prepare returns 5xx or times out                                                          | `PreparingTarget`    | Retried with backoff. No host touched                                                                                                                                                            |
| A consuming host has no link peer, lacks `volume-swap`, or reports a volume not swappable | `AttachingPaths`     | `Failed` after `ReleasingPaths`, `SubsystemSwapUnavailable` naming the PVC to restart                                                                                                            |
| A host fails `Suspend`                                                                    | `Suspending`         | Rollback path. Nothing was moved                                                                                                                                                                 |
| A host restarted or resumed before the move                                               | `MovingNamespaces`   | Rollback path                                                                                                                                                                                    |
| The control plane refuses the move as past the deadline                                   | `MovingNamespaces`   | Rollback path                                                                                                                                                                                    |
| The move fails partway and the control plane restored the source                          | `MovingNamespaces`   | Rollback path, `namespacesAt: Source`                                                                                                                                                            |
| A host cannot find the namespace before the deadline                                      | `Resuming`           | The reconciler rolls back the move and resumes on the source. The watchdog covers a reconciler that cannot                                                                                       |
| Commit returns 5xx                                                                        | `Committing`         | Retried. The namespaces are already on the target and every host has resumed                                                                                                                     |
| Operator restarts in any step                                                             | any                  | Resumes at the persisted step. Every call is repeated safely                                                                                                                                     |
| Operation deleted while running                                                           | any                  | Refused by admission in the five steps above. Otherwise, the finalizer runs the rollback path when `namespacesAt` is unknown or `Source`, the commit when it is `Target`, and releases the locks |

---

## 8. Controller Design

### 8.1 The Membership Watcher

The watcher keeps its trigger: a PVC informer, Add and Update events, and a
5-minute resync. Its `join` path changes. For a bound PVC whose label names a
group and whose volume belongs to no group, it ensures a `PersistentVolumeOps`
named `cg-join-<lvol>-<group>` exists, with action `JoinConsistencyGroup` and the
label's group name, and reports its phase as an event, as it reports a
`VolumeMigration` phase today. A failed operation is left in place, and the
watcher does not recreate it until it is deleted, which is the retry a user asks
for. Label removal still detaches directly.

A label changed to name another group is the second trigger. The watcher
requests a join of the new group, `cg-join-<lvol>-<group>`, and the operation
leaves the old group first (§8.5). The volume's create event is
`ConsistencyGroupMoveStarted` instead of `ConsistencyGroupJoinStarted`. When the
operation for the label's group has already succeeded and the volume is still
in another group, the volume was relabeled back to a group it left, which the
one-way rule refuses, and the watcher reports `ConsistencyGroupConflict`. With
`SPDKCSI_CG_JOIN_OPERATION` off, a relabel is a `ConsistencyGroupConflict`, as
before.

The trigger is therefore a label added to an existing volume or changed on one,
and nothing else.
A volume provisioned with the label is placed on the pinned node and joins the
group at creation (`design-consistency-groups.md` §4.1), so it is a member
before the watcher first sees it, and the watcher never creates an operation for
it. When the group's subsystem was full at that moment, the new member keeps its
own subsystem, which §4.2 of that design accepts, and is brought into the
group's subsystem only by an operation written by hand. The same holds for a
volume whose operation failed after `Joining`: it is a member, so deleting the
failed operation does not make the watcher retry it.

The watcher's direct `JoinConsistencyGroupMember` and `ColocateConsistencyGroupMember`
calls, and the `VolumeMigration` it requests, are no longer used when
`SPDKCSI_CG_JOIN_OPERATION` is on (§10).

### 8.2 The Reconciler

The action lives in `operator/internal/controllers/volume`, beside `Migrate` and
`Relocate`, as a new graph in `graphs.go` and its steps in a new
`join.go`. It reaches CSI nodes through the csi-link hub's registry
(`Registry.Conn(link.NodePeer(node))`), the way `operator/internal/csilink/flows.go`
already calls each node's `FlowService`. The
consuming hosts of a volume are the nodes of its `VolumeAttachment` objects,
narrowed to the volumes each host has staged, as its swap service reports them.
The design also cross-checks them against the host NQNs connected to the source
subsystem, so that a host outside Kubernetes fails `AttachingPaths`. The control
plane does not report those NQNs yet (P0-11), so the cross-check is not built,
and such a host is not detected.

Attachment is a kubelet decision the operation does not stop, so the host list
recorded at `SelectingSubsystem` is not final. `AttachingPaths` re-reads the
attachments on every pass: a host that attached since and has a moving volume
staged is recorded, prepared, and held like the others. A host whose attached
volume is not staged holds no path and is left out: the control plane refuses
its connect while the move exists (§9), and it attaches to the target after the
commit. `Suspending`, before the first host is held, and `MovingNamespaces`,
last before the move, look once more: a consumer that appeared since, or a
host that cannot say, is a path the operation cannot hold, and the operation
rolls back rather than move the namespaces under it.

The prepare call is made once per consuming host and names that host's NQN,
because the control plane returns connect entries, a DHCHAP secret among them,
only for a named host. The NQN is the kernel default the host's CSI node plugin
publishes on its Node (`storage.simplyblock.io/nvme-host-nqn`), or the form
derived from the Node's UID before the plugin has published one. A prepare that
returns no path fails `AttachingPaths` before the host is asked to connect.

Every external wait is a requeue with a named interval. No step blocks, and the
suspend window is enforced by the deadline in status, not by a timer in memory.

### 8.3 Mutual Exclusion

From `SelectingSubsystem` on, the operation holds the volume lock
(`design-persistentvolumeops.md` §6) on every PV in every source subsystem of
the moving set and in the target subsystem. Lock acquisition is all or nothing:
a PV held by another operation leaves this one `Running` with `deferredSince`
set and a `SubsystemSwapDeferred` event. During `Migrating` the parent holds no lock, and the child takes its own.

The control plane enforces the same exclusion for actors outside Kubernetes
(P0-3): a group snapshot, a group migration, a second move, a slot claim into
the source or target subsystems, and a host's connect to any of them are
refused while the move record exists.

On the host, the swap service shares a volume's mapping and stack record with
the node's own stage, unstage, publish, and expand of it. Each of its calls on a
volume runs under the node's per-volume lock, the one those RPCs take, so a
suspend never interleaves with a stage and a resume never with a heal or a
grow. The lock is held for a call, never across the window; for the window the
node asks the service whether it holds the volume, and refuses those four RPCs
with `Unavailable` while it does (§6.3). An expand or a publish inside the
window would otherwise reload and resume the mapping under the swap, letting
I/O through to a namespace that is leaving, and an unstage would remove it or
block on it. Kubelet retries the refused RPC after the window.

### 8.4 RBAC

The CSI controller already has `get` and `create` on `volumemigrations`. It needs
the same on `persistentvolumeops`:

```go
{
    APIGroups: []string{"storage.simplyblock.io"},
    Resources: []string{"persistentvolumeops"},
    Verbs:     []string{"get", "list", "watch", "create"},
},
```

The operator already reconciles `persistentvolumeops` and already reads
`volumeattachments`. It needs nothing new on the Kubernetes side.

### 8.5 Moving Between Groups

A volume belongs to at most one group, and a group's members share one node,
one logical volume store, and, after a join, one subsystem. A volume that moves
from group G1 to group G2 therefore has to stop sharing G1's subsystem before it
can leave G1's node: a live migration moves a subsystem with everything in it,
and the control plane refuses one that would drag another group's members along
(`cg_colocation.plan_late_join`). The operation for G2 does this in `Leaving`,
before it plans the join.

1. **Split.** `Planning` reads the volume's current group. When it is not the
   label's group, the operation records it in `status.joinConsistencyGroup.leave`
   and enters `Leaving`, which asks the control plane for a leave move on G1
   (`POST .../subsystem-moves` with `purpose: leave`). The control plane answers
   an empty move when the volume shares its subsystem with nobody. Otherwise
   the target is a fresh subsystem on the volume's node holding only that
   volume, and the swap of §6 and §7 runs against G1 exactly as a join's does:
   every host consuming the volume holds its I/O while its namespace changes
   subsystem. G1's other members are not touched and keep their subsystem.
2. **Detach.** Back in `Leaving`, the operation records the split, releases the
   locks the swap took, and detaches the volume from G1
   (`DELETE .../members/{lvol}`). The membership closes at G1's current
   generation, and every generation that contains the volume stays restorable.
3. **Join.** The operation plans the join with G2 and continues as any late
   join does: `Migrating` when G2 is on another node or store, then `Joining`,
   then the selection and the flip into G2's subsystem.

The split is the only step of the leave a rollback can take back. A split that
rolls back fails the operation with the volume still a member of G1, in G1's
subsystem, and deleting the failed operation lets the watcher retry. Once
detached, the volume is in no group until the join with G2 succeeds, so a
failure after the detach leaves it standalone in its own subsystem, and the
watcher's retry is a plain late join of G2.

---

## 9. Backend API Requirements

Paths are under `/api/v2/clusters/{cluster}/consistency-groups/{group}`, except
the connect row, which is under `/api/v2/clusters/{cluster}`.

| Method   | Endpoint                                     | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
|----------|----------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST`   | `/members/plan`                              | Exists. P0-2 adds `colocation: {target_nqn, target_is_new, moving_lvol_ids, refusal}`. Reserves nothing                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `POST`   | `/members`                                   | Exists. Idempotent, 409 on refusal                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `POST`   | `/subsystem-moves`                           | New. Body `{lvol_id, request_id, purpose}`. `purpose: leave` splits an open member into a fresh subsystem on its node, alone, and answers an empty move when it shares its subsystem with nobody (§8.5). With `purpose: join`, the default, it runs §5 and reserves the target's slots in the claim transaction. Returns the move: `id`, `target_nqn`, `target_is_new`, and per volume `lvol_id`, `source_nqn`, `ns_uuid`, `nguid`, `target_nsid`. The same `request_id` returns the same move. 409 `SubsystemLimitReached`, 409 when a move or migration is active |
| `POST`   | `/subsystem-moves/{id}/prepare`              | New. Creates the target on every HA node when new, with listeners (P0-5). Returns the target's connect entries for a given `host_nqn`. Idempotent                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `POST`   | `/subsystem-moves/{id}/move`                 | New. Body `{deadline}`. Removes every moving namespace from its source and adds it to the target at `target_nsid` on every HA node. Refuses with 409 before removing anything when `deadline` has passed. On a partial failure restores the source and returns 409 with `namespaces_at: source`. A repeat after success returns 200 with `namespaces_at: target`                                                                                                                                                                                                    |
| `POST`   | `/subsystem-moves/{id}/commit`               | New. Switches every moving volume's `nqn`, `ns_id`, `namespace`, and `max_namespace_per_subsys` in one transaction, deletes empty source subsystems, and clears the move. Idempotent                                                                                                                                                                                                                                                                                                                                                                                |
| `POST`   | `/subsystem-moves/{id}/rollback`             | New. Puts every namespace back in its source when it is not there, deletes an empty fresh target, releases the reservation, and clears the move. Idempotent                                                                                                                                                                                                                                                                                                                                                                                                         |
| `GET`    | `/subsystem-moves/{id}`                      | New. The move with `namespaces_at`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `DELETE` | `/members/{lvol}`                            | Exists. Closes the volume's membership. The operator treats a 404 as already detached                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `GET`    | `/storage-pools/{pool}/volumes/{id}/connect` | Exists (cluster path). Returns the source NQN until commit and the target after it. Refuses with 409 while a move holds the volume's subsystem, the same guard the attach has: a host that connected then would hold a path the move never suspends. The CSI node reads the 409 as transient, attaches from nothing older, and kubelet retries the stage after the window                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

---

## 10. Configuration

| Field                                                     | Type  | Default | Description                                                                                                              |
|-----------------------------------------------------------|-------|---------|--------------------------------------------------------------------------------------------------------------------------|
| `spec.joinConsistencyGroup.suspendTimeoutSeconds`         | int32 | 10      | Longest time any host holds I/O. 1 to 30. Immutable                                                                      |
| `SPDKCSI_DM_INDIRECTION` (CSI node)                       | bool  | `true`  | Stage fresh volumes of every shape behind `dmLinear`. Read at stage time only                                            |
| `spec.disableDeviceMapperIndirection` (SimplyblockDriver) | bool  | `false` | `true` renders `SPDKCSI_DM_INDIRECTION=false` on the node plugin                                                         |
| `SPDKCSI_CG_JOIN_OPERATION` (CSI controller)              | bool  | `true`  | The watcher creates `JoinConsistencyGroup` operations. Off, it falls back to today's join and pre-join `VolumeMigration` |

`SPDKCSI_CG_COLOCATE` and `SPDKCSI_CG_CLIENT_SWAP_READY` are removed once
`SPDKCSI_CG_JOIN_OPERATION` is on by default, because the operation reads each
host's capability instead of an assertion about all of them.

---

## 11. Failure Modes

| Failure                                            | Detection                             | Behavior                                                                                                                                  |
|----------------------------------------------------|---------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| Control plane unreachable before `Suspending`      | HTTP error                            | Retried with backoff. No host is touched                                                                                                  |
| Control plane unreachable during the window        | HTTP error in `MovingNamespaces`      | The reconciler retries until the deadline, then rolls back. The control plane refuses a late move (P0-4). The watchdog resumes every host |
| Operator dies between the move call and its status write | `MovingNamespaces` re-entered with no namespace location, or any rollback or failure after the deadline | The reconciler asks the control plane where the namespaces are before it decides (`GET /subsystem-moves/{id}`). A move that happened is never taken back once the window has closed: the watchdog may have resumed a host on the target, so the operation resumes each host there as it can be reached, and commits, even when it ends Failed. Live 2026-11-10: without this, a rollback removed the namespace under a resumed host and the pod saw I/O errors |
| A host unreachable over the link during the window | gRPC error                            | Rollback. The unreachable host's watchdog resolves it by namespace UUID                                                                   |
| The namespace never appears on the target          | `Resume` times out                    | Rollback to the source, and the operation fails with `SubsystemSwapRolledBack`                                                            |
| The kernel refuses the reload                      | `Resume` error                        | The mapping stays suspended, the reconciler rolls back, and the reload onto the source device is retried                                  |
| A connected host outside Kubernetes                | Not detected until P0-11              | The host is never suspended, and its I/O to a moving namespace fails during the window                                                    |
| A host attaches a moving volume during the operation | Attachments re-read at `AttachingPaths`, `Suspending`, and before the move | Before the window it is recorded and held like the others. At `Suspending` or inside the window it is a rollback. Its connect is refused by the control plane while the move exists, so a host that has not staged yet holds nothing and is left out |
| A stage, unstage, publish, or expand lands inside the window | The node's gate (§8.3)               | Refused `Unavailable` and retried by kubelet after the window; the mapping is never reloaded, resumed, or removed under the swap           |
| A member staged without `dmLinear` is attached     | `Describe` reports not swappable      | `Failed` at `AttachingPaths`, the event names the PVC whose pod needs a restart                                                           |
| The pinned node at its subsystem limit             | Preflight or selection 409            | `Failed`, `SubsystemLimitReached`, before the migration when caught by the preflight                                                      |
| CSI node restarts while suspended                  | Changed instance UID, or startup scan | The node resolves alone. The reconciler rolls back if it had not moved yet                                                                |

---

## 12. Observability

The membership watcher already emits `ConsistencyGroupJoined`,
`ConsistencyGroupPending`, `ConsistencyGroupJoinRefused`,
`ConsistencyGroupMigrating`, `ConsistencyGroupMigrated`,
`ConsistencyGroupMigrationFailed`, `ConsistencyGroupColocationDeferred`,
`ConsistencyGroupDetached`, and `ConsistencyGroupConflict` on the PVC, and the
CSI node emits the `VolumeStack*` reasons of `design-node-volume-stack.md` §14.
Both surfaces gain rows here. The metrics are new.

### Kubernetes Events

Events go to the joining volume's PVC, which the user labeled and watches, and
to the `PersistentVolumeOps`, which is the audit record of the operation. Events
about a host's swap go to the PVC of the volume that was swapped, which is not
always the joining one.

| Event                                                                                                           | Type    | Reason                              |
|-----------------------------------------------------------------------------------------------------------------|---------|-------------------------------------|
| The watcher created the operation for the label                                                                 | Normal  | `ConsistencyGroupJoinStarted`       |
| The watcher created the operation for a label changed from one group to another                                 | Normal  | `ConsistencyGroupMoveStarted`       |
| The volume left the subsystem it shared with its old group's members, into one of its own                       | Normal  | `ConsistencyGroupSplit`             |
| The volume left its old group                                                                                   | Normal  | `ConsistencyGroupLeft`              |
| The label names a group the volume already left, which the one-way rule refuses                                 | Warning | `ConsistencyGroupConflict`          |
| The group needs a fresh subsystem and the node is at its subsystem limit                                        | Warning | `SubsystemLimitReached`             |
| Every moving namespace is in the group's subsystem and every host resumed                                       | Normal  | `ConsistencyGroupColocated`         |
| A consuming host cannot swap, naming the host and the PVC to restart                                            | Warning | `SubsystemSwapUnavailable`          |
| The operation is waiting for another operation's lock on a volume it moves                                      | Normal  | `SubsystemSwapDeferred`             |
| A host held the volume's I/O for the flip, with the measured window                                             | Normal  | `SubsystemSwapCompleted`            |
| The flip was rolled back and the volume resumed on its source subsystem                                         | Warning | `SubsystemSwapRolledBack`           |
| The volume joined and the control plane refused to move the group into one subsystem, as on a stretched cluster | Warning | `ConsistencyGroupColocationRefused` |
| A node's watchdog resumed a mapping the operator did not                                                        | Warning | `SubsystemSwapWatchdogFired`        |

### Prometheus Metrics

| Metric                                            | Labels                                                      | Description                                                         |
|---------------------------------------------------|-------------------------------------------------------------|---------------------------------------------------------------------|
| `simplyblock_cg_subsystem_moves_total`            | `cluster`, `result` (`succeeded`, `rolled_back`, `refused`) | Subsystem moves by outcome                                          |
| `simplyblock_cg_subsystem_moved_namespaces_total` | `cluster`                                                   | Namespaces moves took to their target                               |
| `simplyblock_cg_subsystem_move_window_seconds`    | `cluster`                                                   | Histogram of the time from the first `Suspend` to the last `Resume` |
| `simplyblock_csi_node_swap_suspended`             | `node`                                                      | Gauge of mappings currently suspended                               |
| `simplyblock_csi_node_swap_rescan_seconds`        | `node`                                                      | Histogram of the time from `Resume` to the namespace's appearance   |
| `simplyblock_csi_node_swap_watchdog_total`        | `node`, `outcome` (`target`, `source`, `none`)              | Watchdog resolutions                                                |

The three `simplyblock_csi_node_swap_*` metrics are registered on the CSI node's default registry. The CSI node serves no `/metrics` endpoint yet, so they are recorded and not scraped.

The window histogram is the measure of the goal and the alert for its breach:
anything past `suspendTimeoutSeconds` is an application stall nobody asked for.
The suspended gauge reading above zero for longer than the largest timeout is
the alert for a mapping left suspended, the one failure that hangs a pod. The
rescan histogram decides whether the node should rescan at once instead of
waiting for the event (§15).

---

## 13. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-cg-live-subsystem-flip.md`](../tests/test-plan-cg-live-subsystem-flip.md)

- **Unit:** the selection rule as faked by the mock control plane, every step
  transition and its restart, the rollback from every step after
  `AttachingPaths`, the deadline arithmetic, the watcher's operation request,
  the indirect LVM plans, and the swap service against a fake `dmsetup` and a
  sysfs fixture.
- **Integration:** the action's reconcile loop against `envtest`, a mock control
  plane, and fake link peers, including the lock across every moved PV.
- **E2E:** a live cluster with fio in verify mode across each flip, for each
  shape, with the group subsystem free, full, and the node at its limit.
- **Manual:** node and operator restarts inside the suspend window, and a
  control plane that stops answering mid-move.

The risk concentrates in §6.5 and §6.6: a mapping left suspended hangs a pod,
and a host resumed on the source while the namespace moves fails its I/O.
Those rows are not cut.

---

## 14. Migration Strategy

- **Staged volumes keep their shape.** A volume staged without `dmLinear` cannot
  be flipped. When it is attached and in a moving set, the operation fails at
  `AttachingPaths` and names the PVC. Restarting its pod restages it with the
  indirection, and deleting the failed operation lets the watcher retry.
- **The pre-join `VolumeMigration` path.** With `SPDKCSI_CG_JOIN_OPERATION` on,
  the watcher no longer creates `cg-join-*` `VolumeMigration` objects. Existing
  ones run to completion and are left as they are.
- **The sbcli colocate endpoint.** `POST .../members/{id}/colocate` and the
  flip-victim logic stay for unattached volumes and are not called by this flow.
  `NAMESPACE_MOVES_ENABLED` stays off. The subsystem-move resource is the only
  path that moves an attached namespace.
- **Labels set before the action exists.** Every existing member is left where
  it is, inside or outside its group's subsystem. A member outside it is brought
  in only by a `JoinConsistencyGroup` operation written by hand.

---

## 15. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                            | Owner                                                                                                                                                                                                                     |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 1   | **Dual export:** whether SPDK can open a namespace's bdev with the shared-write claim, so a second subsystem can add it while ANA keeps the first the only writable one, and whether the ANA switch can act per namespace through ANA groups (P0-7, §6.7). The first answer (2026-11-02) was that the exclusive claim refuses a second export today | SPDK/Backend team Independently of SPDK, the Linux host refuses one namespace identity in two subsystems at once (§6.4), so make-before-break also needs the target's copy to carry a different identity until the switch |
| 2   | **Rescan latency:** how long the kernel takes to surface a namespace added to a connected subsystem over NVMe/TCP. It sets the default timeout, and whether `Resume` rescans at once                                                                                                                                                                | Geoffrey                                                                                                                                                                                                                  |
| 3   | **Fresh subsystem size:** a fresh subsystem takes 50 slots. Smaller sizes save target memory but make a later full-group move more likely                                                                                                                                                                                                           | SPDK/Backend team                                                                                                                                                                                                         |
| 4   | **Multi-host consumers:** an RWX raw-block volume consumed by several hosts multiplies the hosts that must all suspend. Whether a cap on hosts per flip is needed                                                                                                                                                                                   | Geoffrey                                                                                                                                                                                                                  |
| 5   | **Stretched clusters:** whether P0-9 moves the subsystem on both sites inside one window, or the closed site follows at its next promotion                                                                                                                                                                                                          | SPDK/Backend team                                                                                                                                                                                                         |
| 6   | ~~**DHCHAP:** the prepare call names no host, so a subsystem gated by host authentication gets no per-host secret~~ **Resolved.** The operator prepares each consuming host with its own host NQN (§8.2)                                                                                                                                            | Resolved                                                                                                                                                                                                                  |

---

## Appendix A: `persistentvolumeops_types.go`

The additions to the type that `design-persistentvolumeops.md` Appendix A
specifies. Everything else in the file is unchanged.

```go
// PersistentVolumeOpsAction is the operation a PersistentVolumeOps performs.
// +kubebuilder:validation:Enum=Migrate;Relocate;JoinConsistencyGroup
type PersistentVolumeOpsAction string

const (
	// PersistentVolumeOpsActionJoinConsistencyGroup moves the volume onto its
	// consistency group's node, adds it to the group, and brings it into the
	// group's NVMe-oF subsystem while its consumers keep running.
	PersistentVolumeOpsActionJoinConsistencyGroup PersistentVolumeOpsAction = "JoinConsistencyGroup"
)

// PersistentVolumeOpsStep is one step of a running volume operation.
// +kubebuilder:validation:Enum=Validating;Migrating;Verifying;Replicating;Committing;AwaitingCutover;AttachingPaths;Proceeding;Planning;Joining;SelectingSubsystem;PreparingTarget;Suspending;MovingNamespaces;Resuming;ReleasingPaths;Leaving
type PersistentVolumeOpsStep string

const (
	// PersistentVolumeOpsStepPlanning resolves the group and checks that the
	// volume can join it and that the group's node can hold the subsystems the
	// join needs.
	PersistentVolumeOpsStepPlanning PersistentVolumeOpsStep = "Planning"
	// PersistentVolumeOpsStepJoining adds the volume to the group.
	PersistentVolumeOpsStepJoining PersistentVolumeOpsStep = "Joining"
	// PersistentVolumeOpsStepSelectingSubsystem chooses the subsystem the group
	// ends up in and reserves its slots.
	PersistentVolumeOpsStepSelectingSubsystem PersistentVolumeOpsStep = "SelectingSubsystem"
	// PersistentVolumeOpsStepPreparingTarget creates the target subsystem when
	// it is new.
	PersistentVolumeOpsStepPreparingTarget PersistentVolumeOpsStep = "PreparingTarget"
	// PersistentVolumeOpsStepSuspending holds the I/O of every moving volume on
	// every node consuming it.
	PersistentVolumeOpsStepSuspending PersistentVolumeOpsStep = "Suspending"
	// PersistentVolumeOpsStepMovingNamespaces moves every moving volume's
	// namespace into the target subsystem.
	PersistentVolumeOpsStepMovingNamespaces PersistentVolumeOpsStep = "MovingNamespaces"
	// PersistentVolumeOpsStepResuming points every consuming node at the
	// subsystem the namespaces are in and releases their I/O.
	PersistentVolumeOpsStepResuming PersistentVolumeOpsStep = "Resuming"
	// PersistentVolumeOpsStepReleasingPaths disconnects the subsystem the
	// volumes left on every node that no longer uses it.
	PersistentVolumeOpsStepReleasingPaths PersistentVolumeOpsStep = "ReleasingPaths"
	// PersistentVolumeOpsStepLeaving takes a volume out of the consistency
	// group it is a member of, so that it can join the group its claim now
	// names: the volume first leaves the NVMe-oF subsystem it shares with that
	// group's members, then the group itself.
	PersistentVolumeOpsStepLeaving PersistentVolumeOpsStep = "Leaving"
)

// SubsystemSide is which subsystem a moving volume's namespace is exported by.
// +kubebuilder:validation:Enum=Source;Target
type SubsystemSide string

const (
	// SubsystemSideSource is the subsystem the namespace was in before the
	// operation.
	SubsystemSideSource SubsystemSide = "Source"
	// SubsystemSideTarget is the subsystem the operation moves it to.
	SubsystemSideTarget SubsystemSide = "Target"
)

// JoinConsistencyGroupSpec parameterizes the JoinConsistencyGroup action.
type JoinConsistencyGroupSpec struct {
	// GroupName is the consistency group to join, by the name its members'
	// claims carry in the storage.simplyblock.io/consistency-group label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +k8s:immutable
	GroupName string `json:"groupName"`

	// SuspendTimeoutSeconds is the longest time any node may hold the I/O of a
	// moving volume while its namespace changes subsystem. A node that has not
	// been released by then resumes the volume on its own.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	// +kubebuilder:default=10
	// +optional
	// +k8s:immutable
	SuspendTimeoutSeconds *int32 `json:"suspendTimeoutSeconds,omitempty"`
}

// MovingVolume is one volume whose namespace changes subsystem.
type MovingVolume struct {
	// VolumeUUID is the volume's backend UUID.
	// +kubebuilder:validation:Required
	VolumeUUID string `json:"volumeUUID"`

	// PersistentVolumeName is the volume's PersistentVolume, when it has one.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// SourceNQN is the subsystem the namespace leaves.
	// +optional
	SourceNQN string `json:"sourceNQN,omitempty"`

	// TargetNSID is the namespace ID the volume takes in the target subsystem.
	// +optional
	TargetNSID int32 `json:"targetNSID,omitempty"`
}

// SwapHost is one node consuming a moving volume, and how far its part of the
// swap got.
type SwapHost struct {
	// Node is the Kubernetes node.
	// +kubebuilder:validation:Required
	Node string `json:"node"`

	// InstanceUID identifies the CSI node process that acknowledged the steps
	// below. A different value means the process restarted since.
	// +optional
	InstanceUID string `json:"instanceUID,omitempty"`

	// Prepared is set once the node holds paths to the target subsystem.
	// +optional
	Prepared bool `json:"prepared,omitempty"`

	// Suspended is set once the node holds the I/O of every moving volume it
	// consumes.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// Resumed is set once the node released that I/O on the subsystem in
	// status.joinConsistencyGroup.namespacesAt.
	// +optional
	Resumed bool `json:"resumed,omitempty"`

	// Released is set once the node disconnected the subsystem it no longer
	// uses.
	// +optional
	Released bool `json:"released,omitempty"`
}

// JoinConsistencyGroupStatus is the progress of a JoinConsistencyGroup.
type JoinConsistencyGroupStatus struct {
	// GroupUUID is the group's backend UUID, resolved from spec.groupName.
	// +optional
	GroupUUID string `json:"groupUUID,omitempty"`

	// MigrationName is the Migrate operation this one created to bring the
	// volume onto the group's node, when it needed one.
	// +optional
	MigrationName string `json:"migrationName,omitempty"`

	// Joined is set once the volume is a member of the group.
	// +optional
	Joined bool `json:"joined,omitempty"`

	// MoveID is the backend's identity for the subsystem move.
	// +optional
	MoveID string `json:"moveID,omitempty"`

	// TargetNQN is the subsystem the group ends up in.
	// +optional
	TargetNQN string `json:"targetNQN,omitempty"`

	// TargetIsNew is set when the target subsystem was created for this move.
	// +optional
	TargetIsNew bool `json:"targetIsNew,omitempty"`

	// NamespacesAt is the subsystem the moving namespaces are exported by now.
	// +optional
	NamespacesAt SubsystemSide `json:"namespacesAt,omitempty"`

	// SuspendDeadline is the time by which every node resumes, whatever
	// happened.
	// +optional
	SuspendDeadline *metav1.Time `json:"suspendDeadline,omitempty"`

	// Volumes are the volumes whose namespaces change subsystem.
	// +listType=map
	// +listMapKey=volumeUUID
	// +optional
	Volumes []MovingVolume `json:"volumes,omitempty"`

	// Hosts are the nodes consuming any of those volumes.
	// +listType=map
	// +listMapKey=node
	// +optional
	Hosts []SwapHost `json:"hosts,omitempty"`

	// Leave is how far the volume got in leaving the group it was a member of
	// when the operation started. Absent when it was in no group.
	// +optional
	Leave *ConsistencyGroupLeaveStatus `json:"leave,omitempty"`
}

// ConsistencyGroupLeaveStatus is how far a volume got in leaving the
// consistency group it was a member of. Leaving a group is permanent: a volume
// that left keeps its place in the generations taken while it was a member, and
// never rejoins that group.
type ConsistencyGroupLeaveStatus struct {
	// GroupUUID is the backend UUID of the group the volume leaves.
	// +kubebuilder:validation:Required
	GroupUUID string `json:"groupUUID"`

	// SplitNQN is the NVMe-oF subsystem the volume was moved into when it
	// shared its subsystem with that group's members. It is empty when the
	// volume shared its subsystem with no other volume.
	// +optional
	SplitNQN string `json:"splitNQN,omitempty"`

	// Split is set once the volume no longer shares an NVMe-oF subsystem with
	// the members of the group it leaves.
	// +optional
	Split bool `json:"split,omitempty"`

	// Detached is set once the volume is no longer a member of that group.
	// +optional
	Detached bool `json:"detached,omitempty"`
}

// Added to PersistentVolumeOpsSpec, with the rule that pairs it with the action:
// +kubebuilder:validation:XValidation:rule="self.action == 'JoinConsistencyGroup' ? has(self.joinConsistencyGroup) : !has(self.joinConsistencyGroup)",message="joinConsistencyGroup is required for action JoinConsistencyGroup and must be absent otherwise"
//
//	// JoinConsistencyGroup parameterizes action JoinConsistencyGroup.
//	// +optional
//	JoinConsistencyGroup *JoinConsistencyGroupSpec `json:"joinConsistencyGroup,omitempty"`

// Added to PersistentVolumeOpsStatus:
//
//	// JoinConsistencyGroup is the progress of a JoinConsistencyGroup.
//	// +optional
//	JoinConsistencyGroup *JoinConsistencyGroupStatus `json:"joinConsistencyGroup,omitempty"`
//
// with status.step's rule extended by the eight new step names.

// Added to the root type's print columns:
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.joinConsistencyGroup.groupName"
```

---

## Appendix B: `swap.proto`

The service the CSI node registers on its link session, in
`atlas-lib/volstack/swaprpc/swapv1/swap.proto`.

```proto
syntax = "proto3";

package simplyblock.volstack.swap.v1;

// SwapService re-points a staged volume's device-mapper node at the device
// its namespace has under another NVMe-oF subsystem. Every call is idempotent.
service SwapService {
  rpc Describe(DescribeRequest) returns (DescribeResponse);
  rpc Prepare(PrepareRequest) returns (PrepareResponse);
  rpc Suspend(SuspendRequest) returns (SuspendResponse);
  rpc Resume(ResumeRequest) returns (ResumeResponse);
  rpc Release(ReleaseRequest) returns (ReleaseResponse);
}

message VolumeState {
  string volume_handle = 1;
  bool staged = 2;
  bool swappable = 3;
  bool suspended = 4;
  string nqn = 5;
  string namespace_uuid = 6;
}

message DescribeRequest { repeated string volume_handles = 1; }
message DescribeResponse { repeated VolumeState volumes = 1; }

message ConnectEntry {
  string transport = 1;
  string address = 2;
  int32 port = 3;
  int32 nr_io_queues = 4;
  int32 ctrl_loss_tmo = 5;
  int32 reconnect_delay = 6;
  int32 fast_io_fail_tmo = 7;
  int32 keep_alive_tmo = 8;
}

message PrepareRequest {
  repeated string volume_handles = 1;
  string target_nqn = 2;
  repeated ConnectEntry entries = 3;
}
message PrepareResponse { repeated VolumeState volumes = 1; }

message SuspendRequest {
  repeated string volume_handles = 1;
  // Unix seconds. The node resumes each volume on its own once this has passed.
  int64 deadline = 2;
}
message SuspendResponse { repeated VolumeState volumes = 1; }

message ResumeRequest {
  repeated string volume_handles = 1;
  // The subsystem whose namespace each volume is re-pointed at.
  string nqn = 2;
}
message ResumeResponse { repeated VolumeState volumes = 1; }

message ReleaseRequest { string nqn = 1; }
message ReleaseResponse { bool disconnected = 1; }
```
