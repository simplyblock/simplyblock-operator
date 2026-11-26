# Design Document: Fencing a Superseded Replication Source

**Status:** Phase 1 Implemented  
**Author:** Ebrahim Savari  
**Date:** 2026-11-17  
**Test Plan:** [`tests/test-plan-volumegroupreplicationops.md`](../tests/test-plan-volumegroupreplicationops.md)

---

## Phasing Overview

| Phase                    | Status      | Scope                                       | Target                                                     |
|--------------------------|-------------|---------------------------------------------|------------------------------------------------------------|
| **Phase 1** (§4, §5, §6) | Implemented | `VolumeGroupReplicationOps`, action `Fence` | A consistency group, named by its `VolumeGroupReplication` |
| **Phase 2** (§5.4)       | Planned     | `PersistentVolumeOps`, action `Fence`       | One volume, named by its `PersistentVolume`                |

Each phase ships on its own. Phase 1 covers every application protected as a
consistency group. Phase 2 covers a volume protected on its own, through a
`VolumeReplication`.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                           | Kind                    | Blocks              | Status                                                             |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------|--------------------------------------------------------------------|
| P0-1 | `superseded=true` on `POST .../consistency-groups/{group}/replication/demote` and on `POST .../volumes/{volume}/replication/demote`: stop replication, mark demoted, no final snapshot, paths left up, idempotent (§8) | Control plane (`sbcli`) | Phase 1, Phase 2    | Implemented on `feat/cp-split-peer-steps` (b1387b0d), not released |
| P0-2 | The new primary refuses a replicated copy of a volume it failed over (`LandingRequest.source_lvol_id`), so a source that ships before its fence lands nothing (§3)                                                     | Control plane (`sbcli`) | Phase 1 correctness | Implemented on the same branch (cf31a5d6)                          |
| P0-3 | The csi-addons `VolumeGroupReplication` and `VolumeGroupReplicationContent` kinds served on the site                                                                                                                   | Ecosystem               | Phase 1             | Available wherever the DR stack is installed                       |

Without P0-1 the control plane answers the fence as an ordinary demote, which
takes a final snapshot of stale data. The operator cannot tell the two apart from
the status code, so Phase 1 is not shippable against a control plane that lacks
P0-1. Without P0-2 the fence still holds once applied, and only a source that ships
between its return and its fence can land a stale generation on the new primary.
Without P0-3 there is nothing to fence: the operation fails with a message naming
the missing kind (§9).

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design: New CRD](#4-api-design-new-crd)
5. [The Fence](#5-the-fence)
6. [State Machine](#6-state-machine)
7. [Controller Design](#7-controller-design)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Failure Modes and Fallback](#9-failure-modes-and-fallback)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `volumegroupreplicationops_types.go`](#appendix-a-volumegroupreplicationops_typesgo)

---

## Overview

When a site is cut off and its applications are failed over to the other site,
the cut-off site keeps its own records of the groups it served. With one control
plane per site, nothing in those records says the groups were failed over: the
failover record is on the other site. When the site returns, its control plane
still treats each group as the active end of its replication.

The layer that ordered the failover knows the site was superseded, and it orders
the fence: the DR hub when it runs the failover, a person who failed over by hand,
or any other component that detects it. The storage layer never decides it.
`VolumeGroupReplicationOps` with `action: Fence` is that order, on the returning
site. It names the application's `VolumeGroupReplication`. The operator resolves
it to the consistency group on this site's control plane and calls the group
demote with `superseded=true`. Every member's replication is stopped and the member
is marked demoted, and no final group snapshot is taken.

The fence leaves the members' paths up. It arrives when the site reconnects, while
the application's old copy on that site may still run, and a guest whose disk paths
all become inaccessible queues its I/O indefinitely and cannot shut down: the
site's cleanup never completes, and Ramen never makes the site secondary. What the
old copy writes stays on this site and is overwritten when the site becomes the
secondary. The paths are cut by Ramen's own `DemoteVolume`, which comes once the
workload is unmounted.

The order can arrive before or after Ramen's own `DemoteVolume` for the same group,
and the returning site's old replication tasks can resume before either. The new
primary refuses the data of a volume it failed over (P0-2), so a demote or a
transfer that runs before the fence lands nothing, and the fence ends it.

---

## 1. Background

A Ramen failover with the source site unreachable promotes the group on the
target site from its replicated copies. The source's records are not updated,
because the source is unreachable and, with one control plane per site, is
managed by another control plane.

After the source returns, Ramen turns its `VolumeReplicationGroup` secondary, and
the CSI driver there calls `DemoteVolume` on the group. The source's control plane
finds no failover record of its own, so it runs an ordinary demote: it fences the
members and takes a final group snapshot that ships to the new primary. That
snapshot is stale data, and it lands on the new primary as one more generation of
the old group, which a later failover from "the newest complete generation" can
pick.

The control plane has the branch the returning source needs. `demote_group` and
`demote_lvol` demote a superseded source by fencing alone, with no final snapshot,
when they find the failover record. A shared control plane finds it. A control plane
per site does not, and the caller's `superseded=true` (P0-1) selects the branch
instead.

`PersistentVolumeOps` (`design-persistentvolumeops.md`) is the existing one-shot
operation on a single volume. Nothing in the operator's API acts on a
`VolumeGroupReplication`.

---

## 2. Goals and Non-Goals

### Goals

- A fence order for a consistency group on a site, expressed as a Kubernetes object
  that the DR hub can place in the site's `ManifestWork`, and that a person or
  another component can create directly.
- The order is applied once the site's control plane answers, however long the site
  was unreachable: an order that exists before the site returns is applied after it
  returns.
- The order is idempotent. A second order for the same group, or Ramen's
  `DemoteVolume` after it, finds the members demoted and succeeds.
- The operation reports, per member, whether it is fenced, and terminates.
- Restart-safe: an operator restart in the middle of a fence resumes it.

### Non-Goals

- **Detecting that a site was superseded.** The layer that orders a failover owns
  the fence. Neither this operator nor the control plane infers it, from object
  storage, from the peer site, or from timing.
- **Unfencing.** A fenced group comes back through a relocate, which demotes the
  current primary and promotes this site from replicated data.
- **Site-level fencing.** csi-addons `NetworkFence`, which Ramen creates from a
  `DRCluster`'s `clusterFence`, blocks a set of client addresses at the storage
  network. It does not stop a group's replication or mark it demoted, and the
  stretched-cluster design uses it for a different purpose
  (`design-stretched-sync-replication-ramen.md` §10).
- **The DR hub's side.** Which hub component writes the order into the site's
  `ManifestWork`, and when it removes it, belongs to the DR stack.
- **`ReplicationOps`.** It stays at `v1alpha1` and outside the replication redesign
  against the csi-addons kinds (`design-crd-model.md`).

---

## 3. Architecture Overview

```
  DR hub (or a person, or a detector)
        │  ManifestWork for site B, applied when B reconnects
        ▼
┌─ Site B: Kubernetes ─────────────────────────────────────────────────────────┐
│                                                                              │
│  VolumeGroupReplicationOps  ─────────►  VolumeGroupReplicationOps controller │
│   spec.volumeGroupReplicationRef         1. get the VolumeGroupReplication   │
│   spec.action: Fence                     2. get its VolumeGroupReplication-  │
│                                             Content; read the group handle   │
│  VolumeGroupReplication (csi-addons)     3. copy the handle into status      │
│   spec.volumeGroupReplication-           4. FenceGroup(handle) until 204     │
│     ContentName                          5. record the members, Succeeded    │
│  VolumeGroupReplicationContent                                               │
│   spec.volumeGroupReplicationHandle                                          │
│     = cg:<cluster>:<group>                                                   │
└──────────────────────────────────────────────┬───────────────────────────────┘
                                               │  site B's own control plane
                                               ▼
┌─ Site B: sbcli control plane ────────────────────────────────────────────────┐
│ POST /api/v2/clusters/{cluster}/consistency-groups/{group}/replication/      │
│      demote?superseded=true                                                  │
│   fence every member, stop its replication, cancel its queued transfers,     │
│   mark it demoted; no final group snapshot                                   │
│ GET  /api/v2/clusters/{cluster}/consistency-groups/{group}/members           │
└──────────────────────────────────────────────────────────────────────────────┘
          ▲ refused: a copy of a volume site A failed over (P0-2)
┌─ Site A: sbcli control plane (the new primary) ──────────────────────────────┐
│ POST /api/v2/_peer/clusters/{cluster}/landing-volumes  → 409                  │
└──────────────────────────────────────────────────────────────────────────────┘
```

**The order goes to the returning site only.** The new primary is not told
anything. Its refusal of the old source's copies (P0-2) is a check against its own
failover record, and it is what makes the order's arrival time irrelevant.

**The csi-addons objects are read, never written or watched.** The controller
reconciles its own kind and reads the two csi-addons objects with uncached reads
when an operation runs, so the operator starts the same way whether or not the
csi-addons kinds are served.

---

## 4. API Design: New CRD

The kind is new, so it is born at `v1alpha2`. Its whole type is in Appendix A. It
follows `VolumeGroupSnapshotOps`: one target named in this namespace, a PascalCase
action, an immutable spec, and a typed phase and step.

### 4.1 Spec

```go
VolumeGroupReplicationRef string                       `json:"volumeGroupReplicationRef"`
Action                    VolumeGroupReplicationOpsAction `json:"action"`
```

`volumeGroupReplicationRef` names the application's `VolumeGroupReplication`, the
name Ramen and the application's namespace already use for the group. It is not
resolved at admission. The `VolumeGroupReplication` is a csi-addons object outside
this API group, and the order is commonly written into a `ManifestWork` while its
site is unreachable, before anything on the site can be checked. A name that does
not resolve fails the operation with a message (§9). `Fence` is the one action.

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: VolumeGroupReplicationOps
metadata:
  name: fence-gitea
  namespace: gitea
spec:
  volumeGroupReplicationRef: gitea
  action: Fence
```

### 4.2 Status

`status.groupHandle` holds the handle the operation acts on, copied when it is
first resolved. Later passes act on the copy, so a `VolumeGroupReplication` that
Ramen deletes after the group is demoted does not change what the operation acts
on. `status.members` lists each member volume and whether it is fenced, read from
the control plane after the order is accepted.

### 4.3 Invariants

- The whole spec is immutable (`+k8s:immutable` on both fields).
- No finalizer: deleting the object while it runs abandons the order, and the
  control plane is left as the last accepted call left it.
- No owner reference to the `VolumeGroupReplication`: the order outlives it, since
  Ramen deletes the group's replication objects after a demote.

---

## 5. The Fence

### 5.1 Resolving the Group

The `VolumeGroupReplication` names its content object in
`spec.volumeGroupReplicationContentName`, and the content object carries the
driver's handle in `spec.volumeGroupReplicationHandle`, `cg:<cluster>:<group>`.
The cluster is this site's own, and the client reaches it through the site's
control plane. A handle of any other shape is refused (§9).

### 5.2 Ordering the Fence

The operation calls `FenceGroup` (atlas-lib `controlplane.Client`), which posts the
group demote with `superseded=true`. The control plane answers 204 once every member
is fenced and demoted, and an error otherwise. There is no "still converging"
answer: a superseded demote takes no final snapshot, so it has nothing to wait for.

A connection error or a 5xx is retried every 15 seconds: right after a site
reconnects, its storage nodes may not answer yet. A 4xx other than 409 fails the
operation. A 409 is retried.

### 5.3 Reporting the Members

After the 204, the operation reads the group's members and records each one with
`fenced: true`. The read is a report: the fence already happened, and a member
list that cannot be read leaves `status.members` empty and the phase `Succeeded`,
with the reason in the message.

### 5.4 Single Volumes (Phase 2)

A volume protected on its own has a `VolumeReplication` rather than a
`VolumeGroupReplication`. Phase 2 adds `Fence` to `PersistentVolumeOps`, which
already names one `PersistentVolume`. The operation reads the volume handle from the
PV and calls `FenceVolume`. Its steps are the same as §6, and
`design-persistentvolumeops.md` takes the action when Phase 2 starts.

---

## 6. State Machine

```
Pending
   │ admitted
   ▼
Running / Validating ──── VGR or content missing, handle malformed ──► Failed
   │ handle copied to status.groupHandle
   ▼
Running / Fencing ─────── 4xx other than 409 ─────────────────────────► Failed
   │ 204                   connection error, 5xx, 409: requeue every
   │                       15 s, until the step deadline ───────────► Failed
   ▼
Running / Reporting       members read (a failed read is reported in
   │                      the message, not as a failure)
   ▼
Succeeded / Completed
```

The position is persisted in `status.step` with its deadline. The `Fencing` step
expires 30 minutes after it is entered. The order is idempotent, so a restart in
any step repeats at most one call.

| Condition                                           | Step                   | Result                                                                            |
|-----------------------------------------------------|------------------------|-----------------------------------------------------------------------------------|
| Object deleted while running                        | any                    | The controller stops. The control plane keeps the state of the last accepted call |
| Operator restart                                    | any                    | Resumes from `status.step`                                                        |
| Site's control plane unreachable                    | `Fencing`              | Requeued every 15 seconds until the deadline, then `Failed`                       |
| `VolumeGroupReplication` deleted after `Validating` | `Fencing`, `Reporting` | No effect: the operation acts on `status.groupHandle`                             |

---

## 7. Controller Design

### 7.1 Location

`operator/internal/controllers/replication/volumegroupreplicationops_controller.go`,
a new package for operations on replication objects.

### 7.2 Reconciliation Trigger

The controller watches `VolumeGroupReplicationOps` only. Terminal operations are
not requeued. The csi-addons objects are read through the manager's API reader,
so no informer is started for a kind the site may not serve.

### 7.3 Concurrency and Mutual Exclusion

None is taken. Two operations for one group both call an idempotent endpoint and
both succeed. A fence and a relocate of the same group cannot overlap in practice,
because a relocate first requires the site to hold the primary, which a fenced site
does not.

### 7.4 Interaction with Existing Controllers

The CSI driver's `DemoteVolume` for the same group goes to the same control plane
and may run before or after the fence. The control plane orders the two (P0-2 and
the override of a started demote in P0-1), and this controller does not coordinate
with the driver.

### 7.5 RBAC

```go
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupreplicationops,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupreplicationops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications;volumegroupreplicationcontents,verbs=get
```

---

## 8. Backend API Requirements

| Method | Endpoint                                                                                              | Notes                                                                                            |
|--------|-------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| `POST` | `/api/v2/clusters/{cluster}/consistency-groups/{group}/replication/demote?superseded=true`            | P0-1. 204 when every member is fenced and demoted. Idempotent. 400 on a sync-replication cluster |
| `GET`  | `/api/v2/clusters/{cluster}/consistency-groups/{group}/members`                                       | Exists. Read for the report only (§5.3)                                                          |
| `POST` | `/api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/{volume}/replication/demote?superseded=true` | P0-1, Phase 2                                                                                    |

---

## 9. Failure Modes and Fallback

| Failure                                                         | Detection                   | Behavior                                                     |
|-----------------------------------------------------------------|-----------------------------|--------------------------------------------------------------|
| `VolumeGroupReplication` not found                              | `Get` returns not found     | `Failed`, message names it                                   |
| csi-addons kinds not served                                     | `Get` returns no kind match | `Failed`, message names the kind                             |
| Content object missing or its handle not `cg:<cluster>:<group>` | handle parse                | `Failed`, message quotes the handle                          |
| Control plane unreachable, 5xx, or 409                          | client error                | Requeue every 15 seconds. `Failed` at the `Fencing` deadline |
| Control plane answers 400 (a sync-replication cluster) or 404   | status code                 | `Failed`, message carries the control plane's detail         |
| Member list unreadable after the fence                          | client error                | `Succeeded`, the message says the members were not read      |

---

## 10. Observability

Events land on the `VolumeGroupReplicationOps` object.

| Reason          | Type    | Emitted when                                                                                      |
|-----------------|---------|---------------------------------------------------------------------------------------------------|
| `GroupFenced`   | Normal  | The control plane accepted the fence                                                              |
| `FenceRetrying` | Warning | The control plane could not be reached or refused with a retryable code. Once per distinct reason |
| `FenceFailed`   | Warning | The operation reached `Failed`                                                                    |

| Metric                                           | Type      | Labels                           | Meaning                                      |
|--------------------------------------------------|-----------|----------------------------------|----------------------------------------------|
| `simplyblock_replication_fence_total`            | Counter   | `result` (`succeeded`, `failed`) | Fence operations that terminated             |
| `simplyblock_replication_fence_duration_seconds` | Histogram | `result`                         | Time from `Validating` to the terminal phase |

---

## 11. Testing Strategy

Unit tests drive the reconciler with a fake client and a fake control-plane client:
the resolution of the handle, each terminal path, the retry on a retryable answer,
and restart from each step. The client calls themselves are covered in atlas-lib
against a test server. The risk concentrates in the order arriving around Ramen's
own demote, which only a two-site bed with a partition exercises. The scenarios
are in [the test plan](../tests/test-plan-volumegroupreplicationops.md).

---

## 12. Open Questions

**Q1: Does an operation that failed at the `Fencing` deadline need anything beyond a
new object?** The hub can replace a failed order. The alternative is no deadline at
all, retrying until the object is deleted, which keeps a site that returns after
hours covered by the original order.

**Q2: Who removes a completed operation?** The hub removes it from the site's
`ManifestWork`. An operation created by hand stays until deleted, as a
`VolumeGroupSnapshotOps` does.

---

## Appendix A: `volumegroupreplicationops_types.go`

```go
// volumegroupreplicationops_types.go declares VolumeGroupReplicationOps, the one-shot
// operation on an application's VolumeGroupReplication. Its one action, Fence,
// fences the consistency group on this site after the group was failed over to
// another site.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeGroupReplicationOpsAction is the operation to perform on the target.
// +kubebuilder:validation:Enum=Fence
type VolumeGroupReplicationOpsAction string

const (
	// VolumeGroupReplicationOpsActionFence fences every member of the group on
	// this site and stops its replication, with no final snapshot.
	VolumeGroupReplicationOpsActionFence VolumeGroupReplicationOpsAction = "Fence"
)

// VolumeGroupReplicationOpsPhase is the lifecycle phase of the operation.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type VolumeGroupReplicationOpsPhase string

const (
	VolumeGroupReplicationOpsPhasePending   VolumeGroupReplicationOpsPhase = "Pending"
	VolumeGroupReplicationOpsPhaseRunning   VolumeGroupReplicationOpsPhase = "Running"
	VolumeGroupReplicationOpsPhaseSucceeded VolumeGroupReplicationOpsPhase = "Succeeded"
	VolumeGroupReplicationOpsPhaseFailed    VolumeGroupReplicationOpsPhase = "Failed"
)

// VolumeGroupReplicationOpsStep is one step of a running operation.
// +kubebuilder:validation:Enum=Validating;Fencing;Reporting;Completed
type VolumeGroupReplicationOpsStep string

const (
	VolumeGroupReplicationOpsStepValidating VolumeGroupReplicationOpsStep = "Validating"
	VolumeGroupReplicationOpsStepFencing    VolumeGroupReplicationOpsStep = "Fencing"
	VolumeGroupReplicationOpsStepReporting  VolumeGroupReplicationOpsStep = "Reporting"
	// VolumeGroupReplicationOpsStepCompleted is the terminal step of an operation that succeeded.
	VolumeGroupReplicationOpsStepCompleted VolumeGroupReplicationOpsStep = "Completed"
)

// VolumeGroupReplicationOpsStepSnapshot is the step the operation is in and the
// deadline it expires at.
type VolumeGroupReplicationOpsStepSnapshot struct {
	// +optional
	State VolumeGroupReplicationOpsStep `json:"state,omitempty"`
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// VolumeGroupReplicationOpsSpec defines the requested operation. The whole spec
// is immutable: the object is a request.
type VolumeGroupReplicationOpsSpec struct {
	// VolumeGroupReplicationRef names the VolumeGroupReplication, in this
	// namespace, whose consistency group the operation acts on. A name that does
	// not resolve when the operation runs fails it.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	VolumeGroupReplicationRef string `json:"volumeGroupReplicationRef"`

	// Action is the operation to perform. Fence fences every member of the group
	// on this site and stops its replication, without taking a final snapshot.
	// Use it on a site whose application was failed over to another site while
	// this one was unreachable.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	Action VolumeGroupReplicationOpsAction `json:"action"`
}

// FencedMemberStatus is one member volume of the fenced group.
type FencedMemberStatus struct {
	// VolumeID is the member's volume ID on this site's control plane.
	VolumeID string `json:"volumeID"`

	// Fenced reports that the member is fenced and no longer replicates.
	// +optional
	Fenced bool `json:"fenced,omitempty"`
}

// VolumeGroupReplicationOpsStatus holds the observed state of the operation.
type VolumeGroupReplicationOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase VolumeGroupReplicationOpsPhase `json:"phase,omitempty"`

	// Step is the step the operation is in, and the deadline it expires at.
	// +optional
	Step VolumeGroupReplicationOpsStepSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// GroupHandle is the consistency group the operation acts on,
	// cg:<cluster>:<group>, read from the VolumeGroupReplication when the
	// operation started. Later steps act on this value, so deleting the
	// VolumeGroupReplication does not change it.
	// +optional
	GroupHandle string `json:"groupHandle,omitempty"`

	// Members lists the group's member volumes after the fence.
	// +optional
	Members []FencedMemberStatus `json:"members,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished, successfully or not.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vgrops
// +kubebuilder:printcolumn:name="GroupReplication",type=string,JSONPath=".spec.volumeGroupReplicationRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VolumeGroupReplicationOps is a one-shot operation on the consistency group of a
// VolumeGroupReplication on this site. It drives its action to completion, records
// the result, and is then inert. The Fence action fences every member of the group
// and stops its replication, for a site whose application was failed over to
// another site.
type VolumeGroupReplicationOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeGroupReplicationOpsSpec   `json:"spec,omitempty"`
	Status VolumeGroupReplicationOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeGroupReplicationOpsList contains a list of VolumeGroupReplicationOps.
type VolumeGroupReplicationOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeGroupReplicationOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeGroupReplicationOps{}, &VolumeGroupReplicationOpsList{})
}
```
