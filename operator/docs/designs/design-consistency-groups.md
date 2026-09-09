# Design Document: Consistency Groups

**Status:** Draft  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-09  
**Test Plan:** [`tests/test-plan-consistency-groups.md`](../tests/test-plan-consistency-groups.md)

---

## Phasing Overview

| Phase       | Status  | Scope                                                                                                                                           | Sections           |
|-------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| **Phase 1** | Planned | Group membership at provisioning, and policy attachment through the `ReplicationPolicy` CRD, so a group replicates as one crash-consistent unit | §4, §5, §6, §7, §8 |
| **Phase 2** | Planned | Kubernetes-native group snapshots through the CSI GroupController service and `VolumeGroupSnapshot` objects                                     | §5.4, §8.2, §9.2   |

Phase 1 is independently shippable: it delivers group-consistent replication and disaster recovery driven by a PVC label and one optional policy field, using the backend snapshot cadence that a `ReplicationPolicy` already schedules. No `VolumeGroupSnapshot` object is involved. Phase 2 adds the Kubernetes-native snapshot and restore surface on top of the same backend group, and it depends on the CSI GroupController work and the external-snapshotter group feature being enabled.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                     | Kind                    | Blocks                             | Status                                                                         |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|------------------------------------|--------------------------------------------------------------------------------|
| P0-1 | Backend group-first REST surface: standalone consistency-group create, member remove, group snapshot take and delete, and policy attach and detach, all scoped to a cluster                      | Control plane (`sbcli`) | Phase 1 and Phase 2                | Not shipped                                                                    |
| P0-2 | Volume-create accepts a `consistency_group` field and, inside one atomic create, ensures the group, joins the volume, and enforces placement                                                     | Control plane (`sbcli`) | Phase 1 group birth and membership | Not shipped                                                                    |
| P0-3 | Group-wide fail-over generation resolution: every member of a consistency group fails over to the same replicated generation                                                                     | Control plane (`sbcli`) | Phase 1 disaster recovery          | Shipped (verified 2026-09-07)                                                  |
| P0-4 | `bdev_lvol_snapshot_group`: one frozen SPDK call that snapshots every member of a logical volume store at a single point in time                                                                 | Storage plane (SPDK)    | Group snapshots in both phases     | Shipped (verified 2026-09-07)                                                  |
| P0-5 | external-snapshotter `VolumeGroupSnapshot` CRDs (`v1beta1`) installed, and the `CSIVolumeGroupSnapshot` feature gate enabled on both the `snapshot-controller` and the `csi-snapshotter` sidecar | Ecosystem / Kubernetes  | Phase 2                            | Images at `v8.2.0` support it, but the CRDs and the gate are not enabled today |

Without P0-1 and P0-2 the operator has nothing to attach to and the CSI provisioner has no field to send, so Phase 1 cannot start. P0-3 and P0-4 are the two backend capabilities that are already live, and they are what make a group's snapshots and its fail-over crash-consistent rather than a set of independent per-volume operations. Without P0-5 the Phase 2 CSI GroupController has no Kubernetes objects to reconcile, so Phase 1 (label plus policy field, no `VolumeGroupSnapshot`) is the whole feature until the gate is turned on.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Data Model Changes](#4-data-model-changes)
5. [Group Lifecycle](#5-group-lifecycle)
6. [State Machine — Policy Attachment](#6-state-machine--policy-attachment)
7. [Controller Design](#7-controller-design)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Configuration](#9-configuration)
10. [Failure Modes and Fallback](#10-failure-modes-and-fallback)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration Strategy](#13-migration-strategy)
14. [Open Questions](#14-open-questions)

Appendices:

- [Appendix A: `replicationpolicy_types.go`](#appendix-a-replicationpolicy_typesgo)

---

## Overview

A consistency group is a set of volumes that snapshot and fail over as one crash-consistent unit. A single frozen backend call (`bdev_lvol_snapshot_group`) takes one snapshot of every member at the same point in time, so a database whose data and write-ahead log live on separate volumes can be restored to a coherent state rather than to two moments that do not agree.

This design gives the group a Kubernetes-native identity and lifecycle without adding a new CRD. A group is named by a **label on its member PVCs**, `storage.simplyblock.io/consistency-group`, which sits in the same family as the shipped `storage.simplyblock.io/replication-policy` annotation. The group is born from the first volume that carries the label, its members are pinned to one storage node and logical volume store so the frozen snapshot is possible, and it dies with its last member.

Two channels carry intent, and each carries exactly one thing. The **label** decides membership, read by the CSI provisioner at volume creation and passed to the backend. The **`ReplicationPolicy` CRD** decides replication, through a new optional `spec.consistencyGroupName` field that attaches a policy to an existing group. Membership belongs to the group and replication follows from membership, which is the inverse of the per-volume model that a `ReplicationPolicy` uses today.

| Concern                        | Channel                                              | Decided when                             |
|--------------------------------|------------------------------------------------------|------------------------------------------|
| Group membership               | PVC label `storage.simplyblock.io/consistency-group` | Volume creation, one-way                 |
| Group placement                | First member's node and logical volume store         | Volume creation, immutable for the group |
| Replication of the group       | `ReplicationPolicy.spec.consistencyGroupName`        | When the policy CR names the group       |
| Snapshot generations (Phase 1) | The attached policy's cadence                        | Backend, on the policy interval          |
| Snapshot generations (Phase 2) | `VolumeGroupSnapshot` object                         | On demand, from Kubernetes               |

A reader who stops here has the whole model: label for membership, policy field for replication, and the group living exactly as long as its members.

---

## 1. Background

Cross-cluster replication is already policy-driven. `design-snapshot-replication-policy.md` established the three-tier hierarchy the operator reconciles: a `ReplicationPair` manages a backend replication target, a `ReplicationPolicy` manages a backend replication policy and its cadence, and one `ReplicationSlot` per PVC tracks the per-volume replication state. A volume joins a policy through the `storage.simplyblock.io/replication-policy` annotation on its StorageClass or PVC, and the operator creates a `ReplicationSlot` for each bound PVC.

Every one of those operations is **per volume**. A `ReplicationPolicy` takes a snapshot of each of its volumes on its own schedule, and a fail-over resolves each volume to its own newest replicated snapshot. For volumes that hold independent data this is correct. For a set of volumes that hold one application's state it is not: two volumes snapshotted a few seconds apart, or failed over to two different replicated generations, restore an application to a state that never existed.

The backend already has the primitive that fixes this. `bdev_lvol_snapshot_group` freezes I/O across a set of volumes on one logical volume store, snapshots them all, and unfreezes, producing one crash-consistent generation. The backend records a consistency group, its membership epochs, and a monotonic generation counter, and group-wide fail-over generation resolution is live (P0-3). What is missing is a Kubernetes-native way to declare which volumes form a group and to point replication at that group. This design supplies it.

The one operator-side attempt so far, a boolean `enableConsistencyGroup` on `ReplicationPolicy` that made the policy own and create the group, was reverted. It encoded the wrong ownership: a policy that creates a group cannot express a group that has no policy, a group replicated by two policies, or a group that outlives a policy. §13 covers the shift from that model.

---

## 2. Goals and Non-Goals

### Goals

- A set of volumes snapshots as one crash-consistent generation, and restores from one generation, verified by hashed cross-volume data rather than by timestamps.
- Group membership is declared by a PVC label and requires no new CRD.
- A group is born from its first member volume and deleted with its last, with no separate create or delete step for the common Kubernetes path.
- Members are colocated on one storage node and logical volume store, which the frozen group snapshot requires, and a volume that cannot be colocated fails creation loudly, because a member off the group's store cannot be part of the frozen snapshot.
- Membership is one-way: a volume's membership window is fixed at creation and closes permanently on removal, so generation math never reasons about gaps in one volume's history.
- A `ReplicationPolicy` attaches to a group through one optional field, and the attach and detach lifecycle is observable through events and conditions on the policy CR.
- A policy naming a group that does not exist is rejected at creation by a validating webhook, so a typo fails at `kubectl apply` rather than parking the policy in a waiting state, while a backend that is unreachable at admission fails open rather than blocking policy creation.
- Every backend call the operator retries is idempotent, and every blocked or waiting reconcile state emits an event.

### Non-Goals

- **Per-member replication policies.** A group is replicated by at most one `ReplicationPolicy`, because the crash-consistent guarantee is a property of the group and not of any single member.
- **Re-adding a removed volume to a group.** A removed volume never rejoins. Re-establishing a member is done by creating a new volume that carries the label, handled in §5.3.
- **Group-wide restore as a single Kubernetes operation.** Restore is per member, each from the matching member snapshot of one generation. The group guarantees the generations are mutually consistent, and reassembling the set is the consumer's operation (§5.4).
- **Ad-hoc groups over arbitrary volumes.** A group's members must share one logical volume store, so a label applied to volumes scattered across nodes cannot form a group. Placement is decided at provisioning (§5.2).
- **A ConsistencyGroup CRD.** The group's Kubernetes identity is the label, and its lifecycle is driven by volume creation and deletion. A CRD would add a second source of truth for membership that the label already owns.

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Control Plane                         │
│                                                                         │
│   ┌──────────────────────────────┐   ┌───────────────────────────────┐ │
│   │   CSI provisioner (spdkcsi)  │   │   ReplicationPolicyReconciler │ │
│   │  1. read PVC label           │   │  1. resolve group by name     │ │
│   │     storage.simplyblock.io/  │   │  2. wait until group exists   │ │
│   │     consistency-group        │   │  3. attach / detach policy    │ │
│   │  2. pass consistency_group   │   │  4. write Ready + GroupAttached│ │
│   │     to volume create         │   │     conditions and events     │ │
│   └──────────────────────────────┘   └───────────────────────────────┘ │
│                                                                         │
│   ┌──────────────────────────────┐  Phase 2                            │
│   │  CSI GroupController (spdkcsi)│  advertises GROUP_CONTROLLER_SERVICE│ │
│   │  CreateVolumeGroupSnapshot    │  driven by the csi-snapshotter      │ │
│   │  DeleteVolumeGroupSnapshot    │  sidecar and VolumeGroupSnapshot CRs│ │
│   │  GetVolumeGroupSnapshot       │                                     │ │
│   └──────────────────────────────┘                                     │
│                                                                         │
│  PVC label  storage.simplyblock.io/consistency-group                    │
│  ReplicationPolicy  spec.consistencyGroupName  status.conditions        │
│  VolumeGroupSnapshot / VolumeGroupSnapshotContent   (Phase 2)           │
└────────────────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼──────────────────────────────────────────────────────────┐
│                        simplyblock Backend API                          │
│  POST   /api/v2/clusters/{id}/storage-pools/{pid}/volumes                │
│           body carries consistency_group  (ensure group, join, place)   │
│  POST   /api/v2/clusters/{id}/consistency-groups/{gid}/attachments       │
│  DELETE /api/v2/clusters/{id}/consistency-groups/{gid}/attachments/{pid} │
│  POST   /api/v2/clusters/{id}/consistency-groups/{gid}/snapshots         │
│  DELETE /api/v2/clusters/{id}/consistency-groups/{gid}/members/{lvid}     │
└──────────────────────────────────────────────────────────────────────────┘
              │ JSON-RPC
┌─────────────▼──────────────────────────────────────────────────────────┐
│                        Storage node (SPDK)                              │
│  bdev_lvol_snapshot_group   one frozen snapshot per member (P0-4)       │
└──────────────────────────────────────────────────────────────────────────┘
```

**The label is the only membership source of truth.** The CSI provisioner reads it and passes it to the backend, which decides the group. The operator never writes the label and never decides membership. This keeps the two channels from disagreeing: one object (the PVC) declares membership, and one object (the `ReplicationPolicy`) declares replication.

**A validating webhook checks the group at admission, and the reconciler owns the rest.** A group exists only in the backend, so the webhook (§7.6) resolves the named group through the backend API and rejects a policy that names one that does not exist, failing a typo at `kubectl apply`. The check is point-in-time: a group deleted after admission is the reconciler's concern, which holds the policy in `WaitingForGroup` until a member re-creates the group. §6 is the state machine.

**Phase 2 is a second, independent driver of the same backend group.** The CSI GroupController translates a `VolumeGroupSnapshot` into a backend group snapshot call. It never creates or mutates the group, because membership and placement were fixed at provisioning. It looks the group up, verifies the resolved member set, and takes one generation.

---

## 4. Data Model Changes

No new CRD. One optional field is added to `ReplicationPolicySpec`, and the reconciler begins writing the `Ready` and `Conditions` fields that the type already declares but never sets today.

### 4.1 ReplicationPolicy Spec — `consistencyGroupName`

```go
// ConsistencyGroupName attaches this policy to the consistency group of the
// same name, so the group's volumes replicate as one crash-consistent unit
// rather than each on its own schedule. The group is named by the
// storage.simplyblock.io/consistency-group label on its member PVCs and is
// created by the first labeled volume, so this reference names a backend
// object, not a Kubernetes kind: it is validated by format here, and a
// validating webhook rejects the policy at creation when no group of this
// name exists (§7.6). A group is replicated by at most one policy.
// +optional
ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
```

The field is a name, not a `*Ref`, because it does not resolve to a Kubernetes object. It follows the `sourceClusterID` precedent in this API group in taking a format rule on the type rather than a Kubernetes-object reference. It departs from that precedent in one way: a validating webhook confirms the named group exists in the backend at creation (§7.6), because a policy that names a nonexistent group is almost always a typo, and failing it at `kubectl apply` is cheaper than parking it in a waiting state a user has to notice. The check is point-in-time and additive: group existence is mutable (a group dies with its last member, §5.4), so the reconciler still handles a group that disappears after admission (§6). The field is mutable, and a change is a detach followed by an attach (§6), which the reconciler walks through conditions rather than performing silently.

**Under consideration: `*string` rather than `string`.** The appendix declares the field as a plain `string`, where the empty value means unset. A `*string` would carry the same meaning (an empty string and a nil pointer both read as no group), but it states the field's optionality more obviously at the call site, since a nil check is unambiguous where an empty-string check is a convention. This is a spelling choice, not a behavior change, and it is not yet adopted: the appendix keeps `string` until the decision is taken.

The whole type as it will be written is in [Appendix A](#appendix-a-replicationpolicy_typesgo).

### 4.2 ReplicationPolicy Status

The design starts writing status fields the type already declares but the reconciler never sets today, and it adds one status field and the condition-merge markers those writes require:

- `status.ready` is set true once the backend policy exists and, when `consistencyGroupName` is set, once the group attachment has been made. It is false while the policy waits for its group.
- `status.conditions` gains a `GroupAttached` condition alongside the `Ready` condition. The reconciler writes conditions for the first time (§11), because the attach lifecycle is the first policy behavior that a user or another controller waits on. The field gains the `+listType=map` and `+listMapKey=type` markers so a server-side-apply patch merges conditions by type rather than replacing the list.
- `status.observedGeneration` is added so a spec edit can be waited on, which the state machine (§6) and its tests depend on: it is written on every status patch.

The shipped `spec.mode` enum uses lowercase values (`failover`, `migration`) rather than the PascalCase this API group defines. That is a pre-existing shipped field, and changing its casing is a breaking change out of this design's scope, so the appendix reproduces it as it stands.

### 4.3 Backend records (informative, owned by `sbcli`)

The backend, not the operator, owns the consistency-group record, its membership epochs (`joined_seq` and `removed_seq` per member), the monotonic `last_group_seq` generation counter, and the pinned placement (node and logical volume store). The operator reads group existence and attachment state through the API (§8) and never persists group state itself. These records are listed here so the API contract in §8 is legible, not because the operator writes them.

---

## 5. Group Lifecycle

### 5.1 Birth and membership at provisioning

A group is born from the first volume that carries the `storage.simplyblock.io/consistency-group` label. The CSI provisioner reads the label with the volume context that `--extra-create-metadata` already supplies, and passes `consistency_group=<name>` to the backend volume-create call (P0-2). Inside one atomic create the backend ensures the group exists, joins the volume, and enforces placement:

```
PVC labeled storage.simplyblock.io/consistency-group: db-group
   │ CSI CreateVolume, provisioner reads the label
   ▼
volume-create REST call carries consistency_group=db-group
   │
   ▼
backend, atomically inside volume create:
   first labeled volume  ← create the group, pin it to this volume's node
                           and logical volume store, open the member epoch
   later labeled volume  ← place on the pinned node (mandatory), share the
                           NVMe subsystem when possible (best effort),
                           open the member epoch
```

**Membership is fixed at creation.** A label added to a PVC after its volume exists does not join the volume to the group, because the join happens only in the create path. This is a deliberate constraint that keeps membership decidable from one event rather than from the mutable state of a label over time.

**Concurrent first volumes converge on one group.** Two volumes created at the same time with the same label must not create two groups. Ensure-group is idempotent by cluster and name (§8), and the volume that loses the race joins the group the winner created, under the winner's placement pin.

### 5.2 Placement is two-tier

The frozen group snapshot operates on one logical volume store, so every member must live on one store on one node.

- **Node and logical volume store colocation is mandatory.** The first member pins the group. A later labeled volume is placed on the pinned node, and if it cannot be placed there the volume create fails. It does not join the group unpinned, and it does not land on another node, because the frozen group snapshot operates on one store and cannot reach a member placed off it.
- **NVMe subsystem colocation is best effort.** A member shares the group's subsystem when the StorageClass is namespaced (`max_namespace_per_subsys` greater than one) and the subsystem has a free namespace slot. When it does not, the member is placed on the pinned node in its own subsystem. Subsystem sharing is an efficiency, not a correctness requirement.

### 5.3 Membership is one-way

A member's epoch opens at creation and closes permanently when the volume is removed from the group or deleted. The same volume never rejoins. The backend records membership as a `joined_seq` and `removed_seq` window per volume, and a one-way rule keeps those windows unambiguous: a generation contains a member if and only if the generation number falls inside the member's open window, and no member has more than one window to reason about.

Re-establishing a member is done by creating a new volume that carries the label, for example, a clone of the removed one. It joins as a new member with a fresh epoch under its own logical volume identity. The generation math never sees a volume leave and return.

### 5.4 Snapshots, restore, and death (Phase 2 for the Kubernetes-native path)

**Generations.** In Phase 1, generations are produced by the attached policy's cadence: the backend takes one group snapshot per interval, stamped with the next `group_seq`. In Phase 2, a `VolumeGroupSnapshot` produces a generation on demand. Both land in the same group as peer generations, and a group may carry both.

**Restore is per member.** Each member snapshot of one generation is an ordinary volume clone source. The group guarantees that the generations of one `group_seq` are a single crash-consistent cut, and the operator reassembles nothing: a restore creates one volume per member from that generation's matching member snapshot. This is the model the consistency-group regression test verifies, restoring every member from one generation and asserting a cross-volume prefix property over hashed records.

**Death with the last member.** A group lives exactly as long as its members. Deleting the last volume of a group deletes the group, as a cascade inside that volume's deletion: any attached policy is detached, the remaining generations are pruned, and the group record is removed. A `VolumeGroupSnapshot` deletion never removes the group, and only ever removes its own generation. Deleting the last member therefore destroys the group's remaining restore points, which is the intended symmetry of a group that exists only while its volumes do, and the backend logs and events that widening because it is the one place a volume deletion also deletes snapshots.

---

## 6. State Machine — Policy Attachment

A `ReplicationPolicy` with `spec.consistencyGroupName` set drives an attach lifecycle. The state is derived on each reconcile from the policy spec, the backend group's existence, and the backend attachment state. It is not a persisted phase field, because it is reconstructable from those three facts on any reconcile or restart.

The group exists at creation, because the webhook (§7.6) rejects a policy naming a group that does not. `WaitingForGroup` is therefore not a creation-time state: it is reached only when a group is deleted under a live policy (its last member removed, §5.4), after which the reconciler holds the policy there until a member re-creates the group.

```
  spec.consistencyGroupName set
    │
    ▼
  WaitingForGroup   ← backend reports no group of this name yet;
    │                 requeue, emit GroupAttachPending on the policy CR
    │  group exists
    ▼
  Attaching         ← POST .../consistency-groups/{gid}/attachments {policy_id}
    │  attach acknowledged
    ▼
  Attached          ← status.ready = true, GroupAttached = True, emit GroupAttached
    │
    │  spec.consistencyGroupName changed
    ▼
  Detaching         ← DELETE the old attachment, then re-enter Attaching for the new
    │                 group; emit GroupDetached, full re-replication follows
    ▼
  (Attaching for the new group)

  spec.consistencyGroupName cleared, or policy CR deleted
    │
    ▼
  Detaching         ← DELETE the attachment; emit GroupDetached
```

| Condition                                                               | Sub-phase                | Result                                                                                                                                                                                                                                                       |
|-------------------------------------------------------------------------|--------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Group deleted under a live policy (last member removed), not re-created | WaitingForGroup          | Requeue indefinitely with `GroupAttachPending` every reconcile, `status.ready = false`. No error, because a member may re-create the group. A group that never existed is rejected at admission (§7.6), so this state is only ever reached after a deletion. |
| Backend unreachable during attach                                       | Attaching                | Requeue with backoff, `GroupAttached = False` reason `BackendError`. No spec change, so the next reconcile retries the same attach.                                                                                                                          |
| Backend group deleted while attached (last member removed)              | Attached                 | Backend auto-detaches. The reconciler observes no attachment and no group, sets `GroupAttached = False` reason `GroupGone`, and re-enters WaitingForGroup rather than erroring.                                                                              |
| Operator restart mid-attach                                             | any                      | State is re-derived from spec plus backend state, so the attach resumes or is confirmed idempotently (§8).                                                                                                                                                   |
| `spec.consistencyGroupName` changed on a policy with active replication | Detaching then Attaching | Detach stops replication and deletes the internal replication snapshots on both sides, then the new group re-replicates in full. Surfaced by `GroupDetached` then `GroupAttached`, never silent.                                                             |

The reconciler never blocks. Every wait is a requeue, and every held decision emits an event on the policy CR (§11).

---

## 7. Controller Design

### 7.1 Location

`operator/internal/controllers/replication/replicationpolicy_controller.go`, extending the existing `ReplicationPolicyReconciler`. No new controller. This work relocates the replication controller family into a domain package, `internal/controllers/replication/`, following the layout the newer controllers already use (`internal/controllers/controlplane/`) rather than the flat `internal/controller/` the replication reconcilers live in today. The `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`, and `ReplicationOps` reconcilers, and their unit and integration test files, move together into the new package, so the consistency-group work lands in the package it belongs to rather than growing the flat one.

The attach lifecycle is folded into the existing `ReplicationPolicy` reconcile, after the backend policy is ensured and before the slot count is computed. Every status patch sets `status.observedGeneration` to the reconciled `metadata.generation`, using an optimistic-lock patch so a status computed from an older generation does not overwrite a newer one.

### 7.2 Reconciliation Trigger

The reconciler already requeues every 30 seconds (`replPolicyRequeueInterval`) and watches `ReplicationSlot` and `ReplicationPair`. The group attachment needs no new watch, because a group is a backend object and not a Kubernetes one: the periodic requeue is what re-checks whether a `WaitingForGroup` policy's group has appeared. A shorter requeue (10 seconds, matching the existing pair-not-ready wait) applies while in `WaitingForGroup`, so a group that appears is attached promptly.

### 7.3 Concurrency and Mutual Exclusion

A group is replicated by at most one policy, which the backend enforces on attach: a second policy attaching to an already-attached group receives a conflict, which the reconciler surfaces as `GroupAttached = False` reason `GroupAlreadyAttached` rather than retrying. Two policies naming the same group is a user error, made visible rather than resolved by a race.

### 7.4 Interaction with Existing Controllers

The `PVCAnnotationWatcher` that creates a `ReplicationSlot` per annotated PVC is unchanged. A consistency-group member is a volume like any other, and if its PVC also carries the `storage.simplyblock.io/replication-policy` annotation it gets a slot as usual. The group attachment is a policy-level operation and does not create or delete slots, so the two mechanisms do not race. When a policy is attached to a group, the group's replication is driven by the group attachment, and per-PVC slots for the same volumes are redundant: §14 carries the question of whether the operator should refuse the annotation on a labeled PVC or let both coexist.

### 7.5 RBAC

No new RBAC. The reconciler already holds `replicationpolicies`, `replicationpolicies/status`, and `replicationpolicies/finalizers`, and the attach calls go to the backend over HTTP, not to the Kubernetes API. The recorder needed for events (§11) uses the manager's existing event client. The webhook (§7.6) needs a `ValidatingWebhookConfiguration` and the manager's existing serving certificate, not a new Kubernetes role, because it too reaches the backend over HTTP rather than the Kubernetes API.

### 7.6 Validating Webhook

A validating webhook on `ReplicationPolicy` create and update rejects the object when `spec.consistencyGroupName` is set and no group of that name exists in the backend. It calls the same group-resolve endpoint the reconciler uses (`GET /api/v2/clusters/{id}/consistency-groups?name={name}`, §8.1) and admits the object only when the group resolves.

- **The webhook fails open.** Its `failurePolicy` is `Ignore`, so a backend that is unreachable at admission admits the policy rather than blocking every `ReplicationPolicy` apply on a backend blip. A name that slips through during an outage lands in the reconciler, which surfaces it as `WaitingForGroup` (§6, §10). The webhook makes the common typo cheap to catch, and it never becomes a cluster-wide outage amplifier.
- **It checks existence, not readiness.** Existence at admission is what the webhook decides, and it is decided once, because an admission decision is never revisited. Whether the group is later deleted is the reconciler's concern, which is why the webhook does not replace the `GroupGone` path (§6).
- **It imposes an ordering on a group's first use.** Because a group is born from its first labeled volume, a `ReplicationPolicy` naming a brand-new group is rejected until at least one labeled PVC has been provisioned. This matches the group-first model: the group is created first, and a policy attaches to it.

---

## 8. Backend API Requirements

The operator reaches the backend through the generic `webapi` client (`Do(ctx, method, endpoint, body)`), the same path the reconciler already uses for `replication/policies`. Every endpoint below is scoped to a cluster and interpolates the resolved cluster UUID.

### 8.1 Phase 1 — attachment (operator)

| Method   | Endpoint                                                                 | Notes                                                                                                                                                                                         |
|----------|--------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `GET`    | `/api/v2/clusters/{id}/consistency-groups?name={name}`                   | Resolve a group by name. Returns the group and its id, or empty when no group of that name exists yet. Idempotent. Called by both the reconciler and the validating webhook (§7.6).           |
| `POST`   | `/api/v2/clusters/{id}/consistency-groups/{gid}/attachments`             | Attach a policy to a group. Body `{policy_id}`. Idempotent: attaching an already-attached policy returns success, and attaching a group already attached to a different policy returns `409`. |
| `DELETE` | `/api/v2/clusters/{id}/consistency-groups/{gid}/attachments/{policy_id}` | Detach. Idempotent: detaching a policy that is not attached returns success. Stops replication and deletes the internal replication snapshots on both sides.                                  |

The group-birth path is not an operator call. The CSI provisioner sends `consistency_group` on the existing volume-create (`POST /api/v2/clusters/{id}/storage-pools/{pid}/volumes`), and the backend ensures the group, joins the volume, and enforces placement atomically (P0-2). Ensure-group is idempotent by cluster and name, which is what lets concurrent first volumes converge (§5.1).

### 8.2 Phase 2 — group snapshots (CSI GroupController)

| Method   | Endpoint                                                           | Notes                                                                                                                                                                                                                         |
|----------|--------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots`         | Take one generation. Returns `{group_seq, members: [{lvol_id, snapshot_id}]}`. Idempotent by an external snapshot name the sidecar supplies: a retry with a name the backend already stamped returns the existing generation. |
| `GET`    | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots/{seq}`   | Read a generation's readiness and member snapshot handles.                                                                                                                                                                    |
| `DELETE` | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots/{seq}`   | Delete one generation and all its member snapshots atomically. Never deletes the group.                                                                                                                                       |
| `DELETE` | `/api/v2/clusters/{id}/consistency-groups/{gid}/members/{lvol_id}` | Close a member's epoch, one-way. The add path does not exist: joining happens only at volume create.                                                                                                                          |

The CSI GroupController maps `CreateVolumeGroupSnapshot` to the take-generation call, returning `group_snapshot_id = {group_uuid}:{group_seq}` and per-member snapshot handles. `DeleteVolumeGroupSnapshot` maps to the delete-generation call. Both must treat a missing group or generation as success, because a group deleted with its last member (§5.4) leaves `VolumeGroupSnapshot` objects that later delete against nothing.

---

## 9. Configuration

### 9.1 Membership label

| Annotation                                 | Values                 | Effect                                                                                                                                |
|--------------------------------------------|------------------------|---------------------------------------------------------------------------------------------------------------------------------------|
| `storage.simplyblock.io/consistency-group` | A group name, on a PVC | The volume joins (or creates) the named group at provisioning. Read once, at volume create. A label added later has no effect (§5.1). |

The key sits in the `storage.simplyblock.io/` family with the shipped `storage.simplyblock.io/replication-policy` annotation. It is a label rather than an annotation because the Phase 2 `VolumeGroupSnapshot` selector matches on PVC labels, and annotations are not selectable in the Kubernetes API, so the one key must be a label to serve both the provisioning join and the snapshot selector.

### 9.2 Policy field

| Field                       | Type   | Default | Description                                                                                                                                                                       |
|-----------------------------|--------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.consistencyGroupName` | string | unset   | Attaches the policy to the named group. Mutable: a change is a detach then attach with full re-replication (§6). Unset means an ordinary per-volume policy, unchanged from today. |

---

## 10. Failure Modes and Fallback

| Failure                                                        | Detection                                       | Behavior                                                                                                                                                                     |
|----------------------------------------------------------------|-------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A labeled volume cannot be placed on the group's pinned node   | Backend rejects the volume create               | The `CreateVolume` fails, the PVC stays Pending with the backend error surfaced through the provisioner. The volume does not join the group unpinned.                        |
| A policy names a group that does not exist, at creation        | Validating webhook resolves no group            | The create or update is rejected (§7.6), so a typo fails at `kubectl apply`. The reconciler never sees the object.                                                           |
| The backend is unreachable when the webhook runs               | Webhook's backend call errors                   | The webhook fails open (`failurePolicy: Ignore`), the policy is admitted, and a bad name surfaces later as `WaitingForGroup`. A backend blip never blocks policy creation.   |
| A group is deleted under a live policy, then never re-created  | Reconciler resolves no group                    | `WaitingForGroup`, `GroupAttachPending` on every reconcile, `status.ready = false`. Correct behavior that looks like a hang, which is why it emits an event each time (§11). |
| Two policies name one group                                    | Backend `409` on the second attach              | `GroupAttached = False` reason `GroupAlreadyAttached`. Not retried, because it is a user error to resolve, not a transient fault.                                            |
| Backend unreachable during attach or detach                    | HTTP error from `webapi`                        | Requeue with backoff. The attach and detach calls are idempotent (§8), so a retry after a partial success converges.                                                         |
| Group deleted while a policy is attached                       | Reconciler observes no group                    | Backend auto-detaches on last-member deletion. The reconciler re-enters `WaitingForGroup` rather than erroring, so re-provisioning a member re-attaches the same policy.     |
| Phase 2: `VolumeGroupSnapshot` deleted after its group is gone | GroupController resolves no group or generation | Delete returns success. A missing handle is not an error, matching CSI snapshot-delete semantics.                                                                            |

Every path degrades to a defined state: a failed create leaves a Pending PVC, a missing group leaves a waiting policy, and a conflict leaves a visibly refused attachment. None leaves a group half-formed or a policy silently unreplicated.

---

## 11. Observability

**Baseline.** The `ReplicationPolicy` reconciler emits no Kubernetes events and writes no conditions today: the `status.conditions` field is declared and never set, and the reconciler surfaces blocked states only through logs and requeues. This design adds the first events and the first conditions on the policy CR, so the whole surface below is new work on the operator side. The group-internal metrics named at the end are the backend's to export, not the operator's.

### Kubernetes Events

Events land on the `ReplicationPolicy` CR. It is the object the user owns, it carries the attach intent in its spec, and it outlives every attach and detach it drives, which a member PVC or a backend group does not.

| Event                                                                        | Type    | Reason                 |
|------------------------------------------------------------------------------|---------|------------------------|
| The policy is waiting because its consistency group does not exist yet       | Normal  | `GroupAttachPending`   |
| The policy has been attached to its consistency group                        | Normal  | `GroupAttached`        |
| The policy has been detached from its consistency group                      | Normal  | `GroupDetached`        |
| The group is already replicated by another policy, so this attach is refused | Warning | `GroupAlreadyAttached` |
| The attach or detach call to the backend failed                              | Warning | `GroupAttachFailed`    |

`GroupAttachPending` is the load-bearing one: a policy waiting for a group that never gets a member is indistinguishable from a stalled controller without it, so the waiting state emits an event on every reconcile rather than only on entry.

### Prometheus Metrics

| Metric                                               | Labels              | Description                                                                                         |
|------------------------------------------------------|---------------------|-----------------------------------------------------------------------------------------------------|
| `simplyblock_replicationpolicy_group_attach_total`   | `cluster`, `result` | Counter of attach and detach outcomes, `result` one of `attached`, `detached`, `conflict`, `error`. |
| `simplyblock_replicationpolicy_group_attach_pending` | `cluster`           | Gauge of policies currently in `WaitingForGroup`.                                                   |

`simplyblock_replicationpolicy_group_attach_pending` is the alert: a value that stays above zero is a policy naming a group no volume ever creates, which is the silent misconfiguration this feature can produce. The counter's `conflict` result is the second signal, catching two policies pointed at one group.

The group's own health, its member count and current generation, is backend state. The backend exports it as `simplyblock_consistency_group_members` and `simplyblock_consistency_group_generation`, labeled by `cluster` and group, and the operator does not restate it.

---

## 12. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-consistency-groups.md`](../tests/test-plan-consistency-groups.md)

- **Unit:** the attach lifecycle as a pure reconcile against a fake client and a mock backend: `WaitingForGroup` when the group resolves empty, attach when it appears, detach on cleared field or deletion, the `409` conflict path, and re-derivation of state after a simulated restart. This is the operator's own coverage and the bulk of what this design can prove without a cluster.
- **Integration:** the reconcile loop against `envtest` and a mock backend, asserting the conditions and events land on the `ReplicationPolicy` CR and that a mutated `consistencyGroupName` walks detach then attach.
- **E2E:** the cross-volume consistency claim, which only a live cluster proves: provision labeled members, run a hashed round-robin writer across them, take generations, restore every member from one generation, and assert the group prefix property and a passing negative control against a mixed-generation restore. This mirrors the existing consistency-group regression script and is where the crash-consistency guarantee is actually verified.
- **Load / long-running:** group snapshot cadence under sustained write load, asserting the generation counter advances and no member's snapshot diverges by more than one write from the others.

The risk concentrates in the E2E cross-volume assertion and in the placement failure path (a member that cannot colocate must fail creation, not join unpinned). Those two must not be cut if the schedule slips. Phase 2 scenarios (`VolumeGroupSnapshot` create, restore, and delete-after-group-gone) become testable only once P0-5 enables the group feature gate.

---

## 13. Migration Strategy

The reverted `enableConsistencyGroup` boolean on `ReplicationPolicy` never shipped, so there are no operator objects to convert. The migration is one of model, from the backend's current policy-owned group to the group-first model this design requires.

- **Today (backend):** a `ReplicationPolicy` with a consistency-group flag creates and owns a group, and volume membership is set by attaching a volume to the policy.
- **Target:** a group is a standalone backend object born from its first labeled volume, and a policy attaches to it. Membership belongs to the group, and policy coverage follows from membership.

The backend refactor (P0-1, P0-2) makes `policy_id` optional on the group record, adds the standalone create and the attachment association, and adds the volume-create `consistency_group` field. Until it lands, the operator field in §4.1 has nothing to attach to, which is why Phase 1 is gated on P0-1 and P0-2. The group-wide fail-over resolution (P0-3) is already live and is unaffected by the ownership change, because it reads the group record and its epochs regardless of how the group was created.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                             | Owner              |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| 1   | **Annotation on a labeled PVC.** Should the operator refuse the `storage.simplyblock.io/replication-policy` annotation on a PVC that also carries the consistency-group label, or let a redundant per-PVC slot coexist with the group attachment? Refusing is cleaner but adds a webhook rule.                                                                                       | Operator team      |
| 2   | **Label key prefix.** The design uses `storage.simplyblock.io/consistency-group` to match the shipped replication annotation family, while the placement pins use the `simplyblock.io/` prefix. Confirm the `storage.simplyblock.io/` choice before it ships, because a label key cannot be changed without breaking every manifest that sets it.                                    | Operator team      |
| 3   | **Phase 2 selector versus fixed membership.** A `VolumeGroupSnapshot` carries a PVC label selector, but group membership is fixed at provisioning. When a selector resolves to a set that differs from the group's current membership, the GroupController must refuse with `FAILED_PRECONDITION`. Confirm this is the desired behavior rather than snapshotting the selector's set. | CSI / Backend team |
| 4   | **Backend attachment cardinality.** This design assumes at most one `ReplicationPolicy` and, separately, at most one backup policy per group. Confirm the backend enforces one replication attachment and returns `409` on a second, which §7.3 and §10 depend on.                                                                                                                   | Backend team       |

---

## Appendix A: `replicationpolicy_types.go`

The type as it is to be written, with the `consistencyGroupName` field added to the spec. This is the only full copy: the numbered sections quote the field, not the type.

```go
// ReplicationPolicySpec defines the desired replication schedule and retention.
type ReplicationPolicySpec struct {
	// PairRef is the name of the ReplicationPair that defines the source and target clusters.
	// Multiple ReplicationPolicies may reference the same pair with different schedules.
	// +kubebuilder:validation:Required
	PairRef string `json:"pairRef"`

	// Mode controls replication semantics.
	// failover: target is a DR standby; volumes are read-only on the target.
	// migration: planned online cutover to the target cluster.
	// +kubebuilder:validation:Enum=failover;migration
	// +kubebuilder:default=failover
	// +optional
	Mode string `json:"mode,omitempty"`

	// Interval is how often a replication snapshot is taken (e.g. "5m", "1h").
	// +kubebuilder:default="5m"
	// +optional
	Interval string `json:"interval,omitempty"`

	// SnapshotRetention is the minimum number of snapshots to retain on the target.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=3
	// +optional
	SnapshotRetention int32 `json:"snapshotRetention,omitempty"`

	// ConsistencyGroupName attaches this policy to the consistency group of the
	// same name, so the group's volumes replicate as one crash-consistent unit
	// rather than each on its own schedule. The group is named by the
	// storage.simplyblock.io/consistency-group label on its member PVCs and is
	// created by the first labeled volume, so this reference names a backend
	// object, not a Kubernetes kind: it is validated by format here and resolved
	// by the reconciler, which waits until the group exists. A group is
	// replicated by at most one policy.
	// +optional
	ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
}

// ReplicationPolicyStatus holds the observed state of a ReplicationPolicy.
type ReplicationPolicyStatus struct {
	// Ready is true when the backend ReplicationPolicy has been created, and,
	// when ConsistencyGroupName is set, once the group attachment has been made.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed
	// from, so a stale status can be told from a current one and a spec edit
	// can be waited on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BackendPolicyID is the UUID of the backend ReplicationPolicy resource.
	// +optional
	BackendPolicyID string `json:"backendPolicyID,omitempty"`

	// SlotCount is the number of ReplicationSlot CRs currently managed by this policy.
	// +optional
	SlotCount int32 `json:"slotCount,omitempty"`

	// ActiveOpsRef is the name of the currently running ReplicationOps CR.
	// Empty when no operation is in progress.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Conditions holds standard Kubernetes condition types, including Ready and
	// GroupAttached.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=repl
// +kubebuilder:printcolumn:name="Pair",type=string,JSONPath=".spec.pairRef"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=".spec.interval"
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.consistencyGroupName"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=".status.slotCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationPolicy defines the replication schedule and retention for volumes replicated
// between the clusters defined by a ReplicationPair.
// A StorageClass or PVC references a policy via the storage.simplyblock.io/replication-policy
// annotation. The operator automatically creates one ReplicationSlot per bound PVC.
// When ConsistencyGroupName is set, the policy instead replicates a consistency group as one
// crash-consistent unit.
// Deletion is blocked while any ReplicationSlots reference this policy.
type ReplicationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
	Status ReplicationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationPolicyList contains a list of ReplicationPolicy.
type ReplicationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationPolicy `json:"items"`
}
```
