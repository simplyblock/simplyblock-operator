# Design Document: The CRD Model

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-19 (last updated 2026-08-28)  
**API group:** `storage.simplyblock.io/v1alpha1`  
**Diagram:** [`assets/crd-overview.jpg`](assets/crd-overview.jpg)

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Entity and Action: The Central Split](#3-entity-and-action-the-central-split)
4. [Reading the Diagram](#4-reading-the-diagram)
5. [The Ownership Spine](#5-the-ownership-spine)
6. [Bootstrap and Discovery](#6-bootstrap-and-discovery)
7. [Kind Inventory](#7-kind-inventory)
8. [Layers](#8-layers)
9. [Migration Strategy](#9-migration-strategy)

---

## Overview

The API group `storage.simplyblock.io/v1alpha1` registers seventeen custom
resource definitions today, thirteen of which are in scope here, and the target
model drawn in
[`assets/crd-overview.jpg`](assets/crd-overview.jpg) has roughly thirty boxes.
This document is the map: which categories a kind can belong to, what its name
has to look like once that category is chosen, which resource owns which, and
where the drawn model differs from the registered one.

Two rules decide most of the rest. **A resource is either an entity or an
action**, teal or blue in the diagram, and the category fixes the shape of the
whole kind (§3). **Ownership is a tree**, and that tree is simultaneously the
creation order, the deletion order, and the mental model of the system (§5).
Every other edge in the diagram is a reference, which looks similar on a drawing
and behaves nothing like it when something is deleted.

---

## 1. Background

The API group grew one feature at a time. Node management arrived with
`StorageNodeSet`, `StorageNode`, and `StorageNodeOps`
([`design-storagenode.md`](design-storagenode.md)),
cluster-wide operations with `StorageClusterOps`
([`design-storagecluster.md`](design-storagecluster.md)), rebalancing with
`VolumeMigration`
([`design-auto-rebalancing.md`](../design-auto-rebalancing.md)),
and data protection with `BackupPolicy`, `StorageBackup`, `BackupRestore`, and
`BackupImport`. Each of those designs is sound about its own kinds. None of them
states the conventions the next kind is supposed to follow, so a new kind gets
designed by analogy with whichever neighbor its author happened to read last.

The drift that produces is visible in the registered types:

- **The backup kinds disagree about the `Storage` prefix.** `StorageBackup`
  carries it. `BackupPolicy`, `BackupRestore`, and `BackupImport` do not.
- **`Task` carries no prefix at all**, and its name says nothing about the
  cluster whose backend tasks it mirrors.
- **`VolumeMigration` is a one-shot operation whose name does not end in `Ops`**,
  which is the one signal a reader has for telling the two categories apart
  without opening the type.
- **Seven of the thirteen kinds carry no `shortName`**, so a `kubectl` command
  written against one of them cannot be abbreviated the way the neighboring
  kinds can.
- **Boolean toggles are spelled five different ways.** Four fields use the
  `enableXyz` form, while others use `skipKubeletConfiguration`,
  `migrationEnabled`, `withCompression`, `dhchap`, and a bare `enabled` (§7.5).
- **No `Ops` kind is backed by a state machine.** `atlas-lib/statemachine`
  exists, is tested, and has no consumer. `StorageNodeOps` drives its steps from a
  hand-rolled `switch`, `StorageClusterOps` carries one action's steps in a
  human-readable message field, and `VolumeMigration` merges phase and step into a
  single enum (§3.1).
- **Annotation and label keys disagree about their prefix.** Twenty-eight
  distinct keys sit on a bare `simplyblock.io`, twenty-six on
  `storage.simplyblock.io`, and nothing separates the two groups except when each
  was written (§7.3).

None of these is expensive to fix while the group is at `v1alpha1`, and each one
becomes permanent the moment a version ships that users are entitled to keep.

---

## 2. Goals and Non-Goals

### Goals

- State the two categories a kind can belong to, and the naming rule that
  follows from each (§3).
- State the one prefix every annotation and label this group defines is keyed
  under (§7.3).
- State the three fields every `Ops` kind carries and the state machine that
  drives them, so that an action is a declared graph rather than a `switch` (§3.1).
- State the lock every entity carries, and why one active operation per entity
  costs nothing (§3.2).
- State the two spellings a boolean toggle may have, and why the choice between
  them follows from the default rather than from taste (§7.5).
- State the one casing every enum value this group defines carries, and the one
  exception to it (§7.8).
- State that every kind reports the generation its status was computed from, and
  that writing it is not optional (§7.9).
- State that backend state reaches a controller by push, so that no per-kind
  design specifies a poll (§7.7).
- State the ownership spine as a single tree, and say what a solid edge costs
  and what a dashed one does not (§5).
- Record the target model as drawn, including the kinds that do not exist yet,
  so that a proposed kind can be looked up rather than argued about (§7, §8).
- Make the difference between the drawing and the registered API explicit and
  countable, so that neither is quietly assumed to be the other (§7.1).
- Name every rename, retirement, and missing owner reference the target model
  implies, with the consequence of each (§9).
- State which package each kind's controller lives in, so that the code is
  organized by the same concerns the model is (§7.10).

### Non-Goals

- **Not an API reference.** Field-level documentation lives in
  `operator/api/v1alpha1/*_types.go` and in the generated CRDs under
  `operator/config/crd/bases/`.
- **Not the marker mechanics.** Which markers a type carries, how immutability
  is actually enforced, and what counts as a breaking change belong to the
  `api-design` skill, which cites §3 of this document for the model and does not
  restate it.
- **Not a per-kind design.** A kind's fields, phases, controller, and external
  prerequisites belong in that kind's own design document, which for an entity
  covers its `<Entity>Ops` companion as well (§3). Where one exists, §7 links to
  it.
- **Not replication or disaster recovery.** The four registered replication
  kinds, `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`, and
  `ReplicationOps`, are excluded from every inventory, layer, and migration table
  below. That subsystem is being redesigned against the CSI Addons specification,
  whose `VolumeReplication` and `VolumeGroupReplication` kinds already carry the
  per-volume and per-group replication contract that a backup tool or a DR
  orchestrator understands, in the same way §8.5 takes `VolumeGroupSnapshot` from
  upstream rather than inventing a private kind. Until that design lands, nothing
  here should be read as settling how replication is modeled, and the conventions
  in §3 apply to whatever it produces.
- **Not a schedule.** What gets built in which order is a work plan (§9).
- **No companion test plan.** This document specifies no runtime behavior, so
  there is nothing for a scenario matrix to assert. What is checkable about it
  is conformance of the registered types to the conventions, and that is what
  `.claude/skills/api-design/scripts/check-crds.py` audits (§7.4).

---

## 3. Entity and Action: The Central Split

Every Kubernetes API designer eventually hits the same wall. The declarative
model has no natural place for "do this once, now," because once the thing is
done the desired state is indistinguishable from the state before it. Restarting
a node is the canonical example.

The usual workarounds are all bad. **An `action` field on the entity** allows
only one action at a time per object, keeps no history, and cannot distinguish
"in progress" from "done" without bookkeeping fields that then accumulate on the
entity's schema forever. **An annotation trigger** is untyped and unvalidated,
has no status, and presents no RBAC surface. **A `kubectl` plugin** is invisible
to GitOps and to anything else that watches the API.

The operator splits the two concerns into two categories of kind instead.

**Entity kinds describe steady state.** `StorageCluster`, `StorageNode`,
`StoragePool`, and `ControlPlane` declare what the storage system should look
like, and controllers converge toward it. They are safe to keep in Git.

**Action kinds describe a single operation against exactly one entity.** Named
`<Entity>Ops` by convention, they carry a target reference, an action verb, and
the action's parameters. They move through phases to a terminal state and stay
around afterward as an audit record. They are not meant to be checked into Git,
in the same way a `Job` that reindexes a database is not.

This is the shape Kubernetes itself uses for `Deployment` against `Job`, and it
buys the same properties:

- **Concurrency control is expressible.** Only one operation may be active per
  entity, which is enforceable precisely because the operation is a first-class
  object that can be counted. The entity carries the lock, and every entity
  carries it under the same name (§3.2).
- **Progress is observable.** A long drain reports where it is, survives an
  operator restart, and can be inspected with `kubectl get`.
- **Operations are auditable.** Who triggered a node removal, when, with which
  parameters, and how it ended is in the API server, subject to normal RBAC and
  audit logging.
- **Entity schemas stay clean.** The imperative bookkeeping lives on the
  operation rather than on the thing being operated.

The `<Entity>Ops` naming is mechanical on purpose. If a kind's name ends in
`Ops`, it is one-shot, it names one target, and it terminates. That is the whole
contract, and it means a reader never has to open a type to know which half of
the model it belongs to.

**An `Ops` kind exists where an entity has operations that are not expressible
as desired state, which is not everywhere.** Every level of the cluster spine
has one, because every level has a restart, a drain, or a rolling variant of
both. The volume and the backup have one. Purely
declarative or observational kinds do not: `ClusterDeploymentConfig` is applied
rather than operated, and a policy is edited rather than acted upon. Of the ten
entity kinds this group owns in the target model (§7), four have no `Ops`
companion, and that is the intended state rather than a gap.

**An `Ops` kind is specified in its entity's design document rather than in one
of its own.** Which operations an entity has, which of them cannot be expressed
as desired state, what each does to the thing it targets, and which lock they
contend for are all questions about the entity, and answering them in two
documents puts an entity's invariants in one file and the operations that suspend
them in another. `StorageNode` and `StorageNodeOps` are specified together in
[`design-storagenode.md`](design-storagenode.md), which is the shape to follow.
[`design-storagecluster.md`](design-storagecluster.md) is the same shape for the
level above, and supersedes the `StorageClusterOps`-only document that preceded
it.

Two `Ops` kinds have no entity in this group to be specified beside.
`OperatorOps` targets the operator process rather than a resource, so it is
specified with the `ClusterDeploymentConfig` its discovery action produces and
its deployment action consumes (§6). `PersistentVolumeOps` targets a core
Kubernetes kind this group does not define, so it carries a document of its own
(§8.4).

**Ownership between the two categories runs one way, with one exception.** An
`Ops` never owns its target, because deleting the record of an operation must
never delete the thing it operated on. The reverse happens deliberately: an
entity owns an operation it created for itself, so that
`controllers/node/storagenode_controller.go` sets a controller reference on the `StorageNodeOps`
it raises. The operation is a subordinate of its creator and is garbage-collected
with it, which is what the ownership edge is for.

**One operation creates another, and scope decides how that edge is built.** A
`StorageNodeOps` draining a node raises one `PersistentVolumeOps` per volume it
moves, and that kind is cluster-scoped while the operation raising it is
namespaced. Kubernetes garbage-collects a cluster-scoped object with a namespaced
owner rather than keeping it, so the edge is built by hand: `spec.creatorRef`
carries the creator's kind, namespace, name, and UID, the
`storage.simplyblock.io/managed-by` label carries the managing kind so the set can
be selected on (§7.3), and the creator's finalizer performs the cascade the
garbage collector cannot.
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §11.1 specifies
all three. The `StorageClass` a `StoragePool` writes is the same problem for the
same reason, carrying the same label and deleted by the same kind of finalizer,
and §5 is where the spine records it.

### 3.1 The shape of an Ops kind

An `Ops` kind carries three fields beyond its target reference, and none of them
is a per-kind choice.

| Field          | Type                        | Holds                                                                                         |
|----------------|-----------------------------|-----------------------------------------------------------------------------------------------|
| `spec.action`  | `<Kind>Action`              | Which operation this object is. Immutable, enum-marked, and the discriminator for the rest    |
| `status.phase` | `<Kind>Phase`               | The operation's own progress, which is what a user asks about                                 |
| `status.step`  | `statemachine.KubeSnapshot` | The serialized position of the action's state machine, which is how the controller gets there |

`spec.action` and `status.phase` are typed with their constants beside the type,
so an impossible value is a compile error rather than a status write nobody
rejected. `status.phase` is the same small set everywhere: `Pending` before the
operation holds its target's lock, `Running` while it works, and `Succeeded` or
`Failed` when it stops. Every kind adds `Aborted`, which `VolumeMigration` already
carries and which is terminal but distinct from `Failed`, because an operation that
was stopped did not go wrong.

**`spec.abort` is how a running operation is stopped, and it is the only way.** It
is a bool, and it is the one mutable field on a spec that is otherwise immutable in
its entirety, because it is the one thing about an operation that can legitimately
be decided after it started. Which steps it is expressible from is declared by the
action's graph rather than by the field, so an abort arriving after the point of no
return is an `IllegalTransitionError` the controller reports while the operation
runs on, rather than a half-undone operation. A controller reaches `Aborted` on its
own for the same reason a user asks for it, which is that the operation stopped
without going wrong: a target that disappears mid-flight ends the operation there.

**Deleting the object withdraws the record, and admission decides whether that is
safe.** A deletion says the record is no longer wanted, which is a different thing
from wanting the operation to stop. Every `Ops` kind therefore carries a validating
webhook on `DELETE`, and it consults the same graph `spec.abort` does:

| The operation is                             | The delete is                                                     |
|----------------------------------------------|-------------------------------------------------------------------|
| Terminal                                     | Admitted, and the object goes                                     |
| Running, and its step declares an abort edge | Admitted, and the finalizer §3.2 requires unwinds before clearing |
| Running, and its step declares no abort edge | **Refused**, naming the step and what it is waiting for           |

**The refusal is the point.** A step with no abort edge is one that has already done
something the graph cannot undo, and the operation is what finishes it: a node
suspended with nothing left to resume it, a device detached with nothing left to
re-attach it, or a restore that created a logical volume nothing will bind. Deleting
the object in that state does not stop any of that work, it removes the only record
of it, so admission refuses rather than letting a `kubectl delete` strand the system.
The webhook is the stronger of the two guards, because it also catches the
`--force --grace-period=0` that the finalizer alone does not.

**Deletion is therefore never a way to express something `spec.abort` could not.**
The graph is the single authority over when an operation may stop, both channels ask
it, and the one that carries a reason forward is `spec.abort`: it leaves an `Aborted`
object to read, where a delete leaves nothing. §3 counts auditability among the four
things this split buys, so stopping an operation and then removing its record is two
steps in that order, and skipping the first is what admission refuses.

**`status.step` is the serialized `statemachine.Snapshot`, not a bare state
name.** A snapshot is everything needed to reconstruct the machine in a later
process, which is a state and the deadline that state expires at. Persisting only
the state loses the deadline, and a restored machine that never times out is a
stalled operation nothing detects. So the field is an object.

**Every kind stores it in the same object, `statemachine.KubeSnapshot`.** A CRD
cannot hold `statemachine.Snapshot[S]` itself. controller-gen v0.21.0 fails with
`unsupported AST kind *ast.IndexExpr` on a field typed `Snapshot[Step]`, on a
defined type over that instantiation, and on an alias to it alike, and the one
spelling that does parse then fails on the type parameter having no schema. A
generic snapshot cannot reach a schema by any spelling, and no marker changes
that. A non-generic struct can, and there is exactly one:

```go
// KubeSnapshot is the durable position of a state machine as a custom resource
// stores it: a state, and the instant it expires. The two travel together by
// construction, because persisting one without the other yields a state that can
// never time out.
type KubeSnapshot struct {
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}
```

A kind embeds it and declares its own steps beside it:

```go
// StorageNodeOpsStep is one step of a running node operation. The enum is the
// union of every action's steps; which steps belong to which action is declared
// by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Awaiting;Validating;Suspending;MigratingVolumes;Verifying;Removing;Preparing;Relocating;AwaitingNode;Promoting;Holding;ShuttingDown;Releasing;AwaitingHost;Restarting;Cleanup
type StorageNodeOpsStep string

type StorageNodeOpsStatus struct {
	// Step is the position of the running action's state machine.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Awaiting','Validating','Suspending','MigratingVolumes','Verifying','Removing','Preparing','Relocating','AwaitingNode','Promoting','Holding','ShuttingDown','Releasing','AwaitingHost','Restarting','Cleanup']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`
}
```

`StorageNodeOps` is the worked example because it has the most steps, and the
declaration above is its real one rather than a sketch:
[`design-storagenode.md`](design-storagenode.md) Appendix B is the copy that
governs, and a divergence between the two is this document's to correct.

**The typing lives at the boundary rather than in the stored field.**
`statemachine.ToKube` and `statemachine.FromKube` are generic *functions*, which
controller-gen never parses, so the restriction that bars a generic type does not
reach them. `FromKube[StorageNodeOpsStep](ops.Status.Step)` yields a typed
snapshot and the machine is typed from there on, and a step string the graph does
not declare is rejected by `Machine.Restore` with `ErrUnknownState`, which is
where the graph is known.

**A shared type costs the step field its `Enum` marker, and that is the whole
price.** A marker cannot reach a field of a type declared in another module, so
the closed set is stated as a CEL rule at the use site instead. `kubectl explain`
reports `state: string` rather than listing the values, and a rejection carries
the rule's message rather than the API server's enum message. The alternative was
one near-identical `<Kind>StepSnapshot` per `Ops` kind, a copy per kind of a type
whose entire content is two fields and whose one invariant is that both are
written together, which is exactly the invariant a copy loses first.

**A stored state is validated on the way in, and by the graph rather than by the
schema.** `Machine.Restore` rejects a state the graph does not declare with
`ErrUnknownState`, naming both the value and the states that were declared, and
`NewFromSnapshot` and `MultiConfig.FromSnapshot` propagate it. That is the check
that matters, because it is the only one that knows which action's graph is in
play: a step belonging to another action passes the CEL rule, which is the union
over the kind, and fails here. An empty state is the exception and is not an
error, since a resource nobody has reconciled restores to the graph's initial
state.

**What a controller does with that error is fail the operation, not retry it.**
An unrecognized step is a downgrade, a hand-edited resource, or a rename that
shipped without a conversion, and none of them resolve by reconciling again. The
operation is terminal with the error in `status.message`, which leaves the audit
record saying what was found and what was expected.

**The step values now live in three places, and a test is what keeps them
level.** The graph declares them, the kind's `Enum` marker constrains the step
type, and the CEL rule constrains the stored string.
`statemachine.DeclaredStates` and `statemachine.DeclaredMultiStates` return a
graph's own list, sorted, so the assertion that all three name the same set is a
unit test rather than a review item. Every `Ops` kind owes that test.

`status.phase` carries no deadline. Time limits belong to a step, because a step
is a single bounded piece of work, whereas `Running` lasts as long as the action
does.

Both `status.phase` and `status.step.state` carry a `+kubebuilder:printcolumn`,
because the point of splitting them is that `kubectl get` answers where an
operation is without a `describe`.

**The machine is [`atlas-lib/statemachine`](../../../../atlas-lib/statemachine)
rather than a `switch`.** Every action is backed by a declared graph, which is
what makes an illegal transition an error instead of an accepted status write: a
step that belongs to a different action fails at `TransitionTo` rather than
being recorded.

**An `Ops` kind with more than one action declares one graph per action, through
`statemachine.MultiConfig[Step]`.** One kind has one `status.step` field, so its
enum is the union of every action's steps, and a single graph cannot express that
a step belongs to one action and never to another. A per-action graph can.
`MultiConfig` also validates every declared graph rather than only the selected
one, so a bad edge in a rarely used action is caught by any test that builds a
machine, and it returns `ErrUnknownAction` for an action it does not declare
rather than stalling on a missing `switch` default. An action that genuinely runs
in one step declares no graph, and the map is asked rather than the error read.

**The outer phase stays an ordinary `Config`**, because `Pending` to `Running` to
a terminal state is identical for every action. Folding it into each action's
graph would copy that spine once per action, and a later fix would land in one
copy. Such a controller runs two machines, one for the phase and one for the
step.

**An entity whose reconcile is multi-step carries `status.step` too.** The step
field and the machine are not exclusive to `Ops` kinds: what makes them necessary
is a reconcile that cannot finish in one pass, and an entity's creation or teardown
often cannot. The difference is that an entity has no `spec.action` to key a
`MultiConfig` on, so it declares one ordinary `Config`. `StorageCluster`'s creation
path is the case, and its `status.phase` is a genuine lifecycle phase rather than
the outer `Pending` to `Succeeded` spine an operation has.

`status.step` was called `subPhase` in the kinds that shipped before this rule.
`subPhase` reads as a subdivision of `phase` while the two are independent: the
step machine is per-action, the phase machine is not, and a step is not contained
in a phase. §9.5 is what the rename costs.

**No kind meets this rule yet.** `atlas-lib/statemachine` has no consumer in
either the operator or the CSI driver, and the three registered `Ops` kinds all
drive their steps by hand.

### 3.2 The lock an entity carries

**Every entity with an `Ops` companion carries `status.activeOpsRef`**, a string
naming the operation currently allowed to act on it, and empty when none is. The
field has the same name and the same meaning on every kind, so a reader, a script,
and a dashboard learn it once rather than per kind. Four kinds carry it today:
`StorageCluster`, `StorageNode`, and the two replication kinds (§2).

**One operation at a time per entity, and that is the design rather than a limit
to work around.** A second operation is admitted by the API server, acquires
nothing, and stays at `status.phase: Pending` until the lock frees, which is what
makes queueing the default rather than a feature anything had to build. `Pending`
is the phase an operation starts in (§3.1), so an operation that has to wait and
one that has not been reconciled yet are in the same state, and neither has issued
a side effect. Nothing bounds how long an operation waits, and waiting is never a
failure.

Three properties make the field a lock rather than a note:

- **Acquisition is an optimistic-lock patch, not a plain one.** Two operations can
  both read an empty `activeOpsRef` and both conclude the lock is free. Patching
  with `MergeFromWithOptimisticLock` succeeds for exactly one of them at a given
  `resourceVersion` and returns 409 to the rest, which is what makes the
  check-then-act sequence safe.
- **Release is idempotent and checks ownership.** It clears the field only when it
  still names the calling operation, so a late release cannot steal the lock from
  whoever acquired it next.
- **Release runs on every terminal path, including deletion.** The terminal
  transition, the terminal-phase branch of a later reconcile, and a finalizer.
  The finalizer is the one that matters most: `kubectl delete` on a running
  operation would otherwise leave the entity locked by an object that no longer
  exists.

**A queued operation is woken by the entity, not by its own requeue.** A
controller that watches only its `Ops` kind leaves the queue waiting out a requeue
interval after the lock frees, so each `Ops` controller maps an event on the
entity back to every operation targeting it.

**The lock is per entity, and one level's lock does not imply another's.** A
cluster-wide operation and an operation on one of that cluster's nodes take
different locks and can both be running. What actually keeps them apart is each
controller's readiness gate on the level above, which is a consequence rather than
a rule, and
[`design-storagenode.md`](design-storagenode.md) §16 Q5 is whether it should be
made one.

### 3.3 The events an Ops kind emits

Six reasons, the same six on every `Ops` kind, so that a dashboard, an alert, and a
runbook are written once against the category rather than once per kind.

| Reason                 | Emitted when                                       | Phase at the time |
|------------------------|----------------------------------------------------|-------------------|
| `OperationQueued`      | The target's lock is held by another operation     | `Pending`         |
| `OperationStarted`     | The lock is acquired and the first step is entered | `Running`         |
| `OperationSucceeded`   | The action reached its terminal step               | `Succeeded`       |
| `OperationFailed`      | A step failed, or an unwind finished after one did | `Failed`          |
| `OperationAborted`     | `spec.abort` was observed and the unwind finished  | `Aborted`         |
| `StepDeadlineExceeded` | A step outlived `status.step.deadline`             | `Running`         |

**`OperationQueued` reports the phase's second meaning rather than the phase.**
`Pending` is where an operation starts, so an operation nobody has reconciled yet
and one waiting on a lock it cannot take are the same value in `status.phase`
(§3.2). The event is the only thing that separates them, which is why waiting emits
one and starting does not: a `Pending` object with a `OperationQueued` event is
blocked, and a `Pending` object without one is new.

**The set is closed even where a member cannot fire yet**, and the two cases are
different in kind. `StepDeadlineExceeded` is reachable on every kind by
construction, because every step carries a deadline and the machine enforces it
(§3.1), so a kind omitting it is a kind that fails to report the failure it is
guaranteed to be able to have. `OperationAborted` is different: a kind whose graph
declares no abort edge today cannot reach it, and it is declared anyway, because a
reason that appears when an action gains an edge is a reason every consumer already
handles rather than one they learn about from an unrecognized string.

**A kind adds reasons of its own beside these, and never renames one of these.** The
six are what an operation does as an operation. What it does to its target is the
kind's own vocabulary, which is why `TaskCanceled`, `DrainBlocked`, and
`ReplacementAmbiguous` sit beside them rather than in place of them.

---

## 4. Reading the Diagram

The diagram uses two visual axes and nothing else.

### 4.1 Box color

| Color  | Category           | Meaning                                                                                                                                                |
|--------|--------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| Teal   | **Entity**         | A thing that exists. Declarative and long-lived, `spec` is desired state, `status` is observed state, and a controller continuously reconciles the two |
| Blue   | **Action**         | A thing that happens. Imperative and one-shot, it names a target and an action, runs to completion, and records the outcome                            |
| Orange | **Not a resource** | The operator process itself, included because several relationships originate from it rather than from a resource                                      |

Teal covers more than this API group. `PersistentVolumeClaim`,
`PersistentVolume`, `StorageClass`, and `VolumeSnapshot` are core Kubernetes and
ecosystem types, drawn in the entity color because they are entities in exactly
the sense above and the model is built on them rather than around them (§5).

### 4.2 Arrow style

| Arrow  | Relationship  | Meaning                                                                                                                                                                                                            |
|--------|---------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Solid  | **ownership** | A Kubernetes owner reference. The child is created by the parent's controller and garbage-collected when the parent is deleted. `owns (1:n)` marks a fan-out                                                       |
| Dashed | **acts on**   | A reference or an effect, not a lifecycle relationship. Deleting the source does not delete the target. Covers `controls`, `manages`, `references`, `installs`, `generates`, `restores`, `backups`, and `defaults` |

The distinction is operational rather than decorative. Solid edges determine what
disappears on a `kubectl delete`, and dashed edges do not. A `StorageNodeOps`
pointing at a `StorageNode` never owns it, because deleting the operation must
never delete the node it operated on.

### 4.3 What the drawing is, and where it diverges

The diagram describes the **target model**. Some of its boxes are registered
kinds, others are proposals, and §7.1 is the inventory that says which is which.
It is a statement of where the API is going rather than an inventory of what
exists.

The labels and arrows match the model below. One group of boxes does not belong
to it: `ReplicationPair`, `ReplicationPolicy`, and `ReplicationPairOps` are
drawn, while replication is out of scope until it is redesigned against the CSI
Addons specification (§2). They stand in for whatever that design produces rather
than for kinds this model settles.

---

## 5. The Ownership Spine

Strip the diagram down to its solid arrows and a single tree remains.

```
ControlPlane                                       (singleton, one per namespace)
    │
StorageCluster                                     (one simplyblock backend cluster)
    ├─owns─► StorageNode                           (one per worker node × NUMA socket)
    │            └─owns─► StorageDevice            (one per NVMe device)
    └─owns─► StoragePool                           (capacity and QoS tenancy unit)

PersistentVolumeClaim ──owns──► PersistentVolume
```

**It bottoms out in core Kubernetes types.** The operator's own resources stop at
the pool. A volume is a `PersistentVolumeClaim` and a `PersistentVolume`, because
Kubernetes already has that type and every workload in the cluster already speaks
it. Introducing a `SimplyblockVolume` would buy a richer status, at the cost of
every Helm chart, operator, and application that already provisions storage the
standard way.

**The join between the two halves is the `StorageClass`, and it is not in the
tree.** The pool controller creates one `StorageClass` per pool, named by
`simplyblockStorageClassName(namespace, clusterName, poolName)`, and that class
is what a claim references to land its volume in that pool. It cannot be an owned
child, because a `StorageClass` is cluster-scoped and a `StoragePool` is
namespaced, and Kubernetes garbage-collects a cluster-scoped object with a
namespaced owner rather than keeping it. So the relationship is built by hand
instead: the class carries `storage.simplyblock.io/{namespace,cluster,pool}`
labels alongside the `storage.simplyblock.io/managed-by` key that grants the right
to destroy it (§7.3), and the pool's finalizer deletes it. This is the one place
the spine is maintained by a controller rather than by the garbage collector, and
it is a consequence of scope rather than a choice.

**The cluster is not the root.** `ControlPlane` sits above it, which is a
consequence of the deployment model (§6).

**Most of these edges are aspirational.** Today only the node level is a real
owner reference. `StoragePool`, `StorageNodeSet`, and `Task` each reference their
cluster by a `spec.clusterName` string, so deleting a `StorageCluster` does not
cascade to any of them. §9.3 lists the edges and what each one costs to
establish.

---

## 6. Bootstrap and Discovery

The top-left corner of the diagram reads differently from the rest, because the
actor is the operator process rather than a resource.

The simplyblock control plane, meaning FoundationDB together with the management
API, is a prerequisite for everything else. The operator talks to it to create
clusters, provision nodes, and place volumes. It may already exist outside the
Kubernetes cluster, or it may need to be brought up alongside the operator, which
is why the edge reads `installs/reuses`. `ControlPlane` is a singleton, one per
namespace and named `simplyblock`, that either points at an existing deployment
or represents one the operator brought up. Its `status.phase` reports `Installing`
until the control plane answers, `Available` afterward, and `Degraded` or
`Unavailable` for the two ways it stops answering
([`design-controlplane.md`](design-controlplane.md) §3.3). Nothing downstream
reconciles meaningfully before `Ready`.

The other two edges out of the operator box:

- **`installs SimplyblockDriver`.** The CSI driver, meaning the node plugin
  and the controller plugin, is what actually attaches volumes to workloads. In
  the target model its deployment is a resource the operator reconciles rather
  than a separate Helm release, so that driver version and cluster version stay
  coupled instead of drifting between two release cadences.
- **`generates ClusterDeploymentConfig`.** An `OperatorOps` with a discovery
  action inspects the workers of the Kubernetes cluster and writes what it found
  as a `ClusterDeploymentConfig`. That document describes a whole deployment in
  one object: the environment, whether it is an edge cluster, the cluster
  template, and the node sets with their sizing and their groups of workers,
  interfaces, and devices. An example is
  [`assets/example-cluster-config.yaml`](assets/example-cluster-config.yaml).

**`ClusterDeploymentConfig` is ephemeral, and every kind it expands into is
self-describing.** The expansion is a copy, not a projection: what a
`StorageCluster` and each `StorageNode` need is written into them once, and
nothing reads the document afterward. So it can be edited or deleted the moment
the expansion is done, deleting it degrades nothing, and editing it reaches only
the objects created after the edit. A resource already carrying data was placed
against the values it was created with, and rewriting those retroactively would
describe a layout that is not on the disks.
[`design-storagenode.md`](design-storagenode.md) §3.1 is what that costs the node,
which is a complete per-node configuration and immutability on most of it.

Discovery inspects the workers of a Kubernetes cluster and reports what they have
free, which is a node-local question and not a backend one. It produces a
description of a deployment that an administrator reviews, approves by setting
`spec.approved`, and applies. The operator expands the approved document into the
cluster and its nodes. What the approval is and what a
config disagreeing with a running deployment does are
[`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §5 and §6:
the label is a spec field a webhook enforces, and expansion creates or adds and
never reconciles a difference.

`Storage(Edge)Cluster` carries a parenthesized `Edge` because the same kind is
intended to cover edge deployments, which differ in topology and scale rather
than in kind. The registered kind is `StorageCluster`.

---

## 7. Kind Inventory

### 7.1 Registered today

Seventeen kinds are registered in `storage.simplyblock.io/v1alpha1`, verified
against `operator/config/crd/bases/`. The four replication kinds are out of scope
for this document (§2), leaving the thirteen below, one CRD per row.

| Kind                | Short name today | Category | In the target model                                                             | Note                                                                                                       |
|---------------------|------------------|----------|---------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------|
| `ControlPlane`      | —                | Entity   | Reworked ([`design-controlplane.md`](design-controlplane.md))                   | Singleton, named `simplyblock`, one per namespace                                                          |
| `StorageCluster`    | —                | Entity   | Reworked ([`design-storagecluster.md`](design-storagecluster.md))               | Drawn as `Storage(Edge)Cluster`, because edge deployments differ in topology and scale rather than in kind |
| `StorageClusterOps` | `scops`          | Action   | Reworked ([`design-storagecluster.md`](design-storagecluster.md))               | Holds `StorageCluster.status.activeOpsRef`                                                                 |
| `StorageNodeSet`    | —                | Entity   | **Retired** (§9.2)                                                              | The fleet template becomes `ClusterDeploymentConfig.nodeSets[]`                                            |
| `StorageNode`       | `sn`             | Entity   | Reworked ([`design-storagenode.md`](design-storagenode.md))                     | Reparented to the cluster: `spec.storageNodeSetRef` becomes a cluster reference                            |
| `StorageNodeOps`    | `snops`          | Action   | Reworked ([`design-storagenode.md`](design-storagenode.md))                     | Fans out one `PersistentVolumeOps` per volume, by `spec.creatorRef` (§3)                                   |
| `StoragePool`       | —                | Entity   | Reworked ([`design-storagepool.md`](design-storagepool.md))                     | Gains a `StoragePoolOps` companion                                                                         |
| `Task`              | —                | Entity   | **Not reworked.** The mirror is not part of the target model (§9.1)             | Observation only, and one object per task rather than per query                                            |
| `VolumeMigration`   | `vmig`           | Action   | **Absorbed** ([`design-persistentvolumeops.md`](design-persistentvolumeops.md)) | The one action kind whose name does not end in `Ops`                                                       |
| `BackupPolicy`      | —                | Entity   | **Renamed** ([`design-storagebackup.md`](design-storagebackup.md))              | Schedules and retains the backups of the claims it selects                                                 |
| `StorageBackup`     | —                | Entity   | Reworked ([`design-storagebackup.md`](design-storagebackup.md))                 | One point-in-time copy of a volume                                                                         |
| `BackupRestore`     | `br`             | Action   | **Absorbed** ([`design-storagebackup.md`](design-storagebackup.md))             | Becomes the `Restore` action                                                                               |
| `BackupImport`      | `bi`             | Action   | **Retired** ([`design-storagebackup.md`](design-storagebackup.md))              | The store is the inventory, so nothing is imported                                                         |

Eight of the thirteen are entities and five are actions. One kind is renamed, two
are absorbed into two new `Ops` kinds, and two are retired outright. Of the eight
that survive under their own name, seven are reworked against the conventions of §3
and §7, and `Task` alone is left as it is. Every row names the design that owns it,
because no kind in this group reaches the target model untouched.

### 7.2 Target-model additions

Kinds the diagram proposes and the API group does not yet register. Each row
names the design that owns the specification, where one exists.

| Kind                      | Category | Layer           | Target or parent           | What it adds                                                                                   | Specified in                                                                |
|---------------------------|----------|-----------------|----------------------------|------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------|
| `OperatorOps`             | Action   | Bootstrap       | The operator process       | Operator-level operations, today the discovery run that produces a config                      | [`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §7 |
| `ClusterDeploymentConfig` | Entity   | Bootstrap       | Generated by `OperatorOps` | A whole deployment as one reviewable document, including `nodeSets[]` (§6)                     | [`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md)    |
| `ControlPlaneOps`         | Action   | Bootstrap       | `ControlPlane`             | Control-plane maintenance                                                                      | [`design-controlplane.md`](design-controlplane.md) §6                       |
| `SimplyblockDriver`       | Entity   | Bootstrap       | Installed by the operator  | Couples CSI driver version to cluster version instead of to a separate Helm release            | [`design-simplyblockdriver.md`](design-simplyblockdriver.md)                |
| `StorageDevice`           | Entity   | Cluster         | Owned by `StorageNode`     | Per-device capacity, health, and conditions, which are a `status.devices` summary string today | [`design-storagedevice.md`](design-storagedevice.md)                        |
| `StorageDeviceOps`        | Action   | Cluster         | `StorageDevice`            | Per-device operations, the narrowest blast radius in the spine                                 | [`design-storagedevice.md`](design-storagedevice.md) §6                     |
| `StoragePoolOps`          | Action   | Tenancy         | `StoragePool`              | Pool-level operations                                                                          | [`design-storagepool.md`](design-storagepool.md) §7                         |
| `PersistentVolumeOps`     | Action   | Volume          | `PersistentVolume`         | Per-volume operations, absorbing `VolumeMigration`                                             | [`design-persistentvolumeops.md`](design-persistentvolumeops.md)            |
| `StorageBackupOps`        | Action   | Data protection | `StorageBackup`            | Turns restore into an action of the backup it restores                                         | [`design-storagebackup.md`](design-storagebackup.md) §6                     |
| `NFSExport`               | Entity   | Volume          | Owned by the operator      | The authoritative per-volume record for an RWX export and its bound metadata server            | `design-pnfs-rwx.md` §7.1, on the `pnfs-design` branch                      |

`NFSExport` is not drawn, and is listed here because its design settles it as an
operator-owned CRD, which makes it a member of this model.

`SimplyblockDriver` is named for the brand rather than for the interface because
`CSIDriver` is already a kind in core `storage.k8s.io/v1`, and two kinds of that
name in two groups is an ambiguity every reader has to resolve by group. The two
are not the same object either: the core kind is the cluster's registration
record for a driver, and this one is the deployment of simplyblock's, which
produces that record among the rest of what it installs.

Two boxes in the diagram are not simplyblock kinds at all and must not become
ones. `VolumeGroupSnapshot` is the upstream ecosystem kind (§8.5). `StorageClass`, `PersistentVolumeClaim`,
`PersistentVolume`, and `VolumeSnapshot` are core or ecosystem types the model
builds on (§4.1).

### 7.3 Annotation and label keys

A kind's name is not the only identifier this group hands to users. Annotations
and labels are API surface too, and they are harder to change than a field is,
because nothing validates them and nothing reports that a key stopped being read.

**Every annotation and label this group defines is keyed
`storage.simplyblock.io/<name>`**, matching the API group itself. One prefix means
a `kubectl get -o yaml` shows at a glance which metadata belongs to simplyblock,
an admission or RBAC policy can match all of it with one prefix, and a reader
never has to recall which subdomain a given feature happened to pick.

Keys that deliberately mirror an upstream convention belong to that convention
rather than to this group, and stay where it puts them: the CSI topology key under
`topology.simplyblock.io`, and the scrape keys under `prometheus.simplyblock.io`.
Those are the exceptions, and a new one needs the same argument.

Most keys in the tree predate the rule. Twenty-eight distinct keys sit on the bare
`simplyblock.io` prefix, covering the QoS parameters, the placement hints, the
rebalancer's bookkeeping, and the drain and realignment triggers, against
twenty-six already correct. What is in the tree is countable rather than
remembered:

```bash
grep -rhoE '\b[a-z0-9.-]*simplyblock\.io/[a-zA-Z0-9._-]+' \
  --include='*.go' --include='*.yaml' operator atlas-lib csi-driver helm-charts \
  | grep -v 'simplyblock\.io/v1alpha1' | sort -u
```

§9.4 is what moving them costs.

**An object this operator creates without owning carries exactly one key saying so,
`storage.simplyblock.io/managed-by`.** A cluster-scoped object cannot be owned by a
namespaced one, so an owner reference is unavailable for a `StorageClass` written for
a `StoragePool` ([`design-storagepool.md`](design-storagepool.md) §4.4) or a
`PersistentVolumeOps` fanned out by a `StorageNodeOps`
([`design-persistentvolumeops.md`](design-persistentvolumeops.md) §11.1), and this
key is what replaces it. It is the only key with that meaning: `creator`, `owner`,
`created-by`, and any per-kind variant are all the same fact under a different name,
and one name means one selector finds every object this operator is responsible for
and one RBAC or admission rule matches them.

**Its value is the lowercased kind of the controller that manages the object**,
`storagecluster` or `storagenodeops`, rather than the managing object's name. A label
value admits no `/`, so a namespace and a name do not fit in one, and a name alone does
not identify an object in a cluster-scoped list. Where the individual object matters,
a field carries it: `PersistentVolumeOps` has `spec.creatorRef` with a kind, a
namespace, a name, and a UID, so the label narrows the search and the reference settles
it. Where no such field exists, the kind is the whole of what is needed, because the
question the label answers is whether this operator may delete the object rather than
which object asked for it.

**It is a label, not an annotation.** Its purpose includes selection, a watch mapping
and a `List` select on labels only, and the prefix rule above applies to either map.

**It grants permission rather than recording provenance.** A controller may delete an
object carrying its own value and must refuse to delete one carrying another's or none
at all, which is the rule the pool's finalizer turns on
([`design-storagepool.md`](design-storagepool.md) §6). That is also why the value is
stable: relabeling an object transfers the right to destroy it.

`simplyblock.io/managed-by` is in the tree on the bare prefix with a component name as
its value, which is the same value convention. It is one of the twenty-eight keys §9.4
moves.

### 7.4 Auditing conformance

Nothing in this document is enforced by the type system, so it is audited
instead:

```bash
.claude/skills/api-design/scripts/check-crds.py --changed
```

The checker reports the conventions a registered type violates, including closed
sets without an `Enum` marker, phases that are plain strings rather than a
`<Kind>Phase` type, immutability that is asserted in a doc comment rather than in
a marker, and boolean toggles named anything other than `enableXyz` or
`disableXyz` (§7.5). It does not cover the key prefix of §7.3, which is checked by
the `grep` there instead.

### 7.5 Boolean toggle fields

A property that turns something on or off is named **`enableXyz`** when the thing
is off by default, and **`disableXyz`** when it is on. Those are the only two
spellings. `skipXyz`, `withXyz`, `noXyz`, `xyzEnabled`, and a bare `enabled` are
not alternatives to them.

**The form follows the default.** A Go `bool` zero-values to `false`, so only the
negative spelling makes an unset field mean the default when the default is on.
`enableXyz *bool` with `nil` read as `true` puts the default in the controller
instead of the type, where nothing about the field says what happens when it is
omitted. Choosing the form by the default keeps the zero value and the default the
same thing.

The prefix form is also the one Kubernetes uses. Core `PodSpec` has
`enableServiceLinks`, and the kubelet's own configuration carries eight
`enableXyz` fields and no suffixed ones. The suffix reads adjectivally, which is
the register core Kubernetes reserves for observations rather than requests, and
`status` is where those belong: `ready`, `paused`, `started`, `attached`.

Two consequences follow, and both are easy to get wrong:

- **The rule is about toggles, not about every boolean.** A field naming a fact
  about the world (`ubuntuHost`, `openShiftCluster`) is not enabling a capability
  and keeps its name. A status boolean is an observation and is never a toggle.
- **An `Ops` spec's booleans are action modifiers, not toggles.** An entity's spec
  answers whether a capability is on for that cluster or pool, which is what
  `enableXyz` names. An `Ops` spec answers what one operation should do, which is
  a different question: `force`, `deleteSource`, `reattachVolume`, and
  `refreshSNodeAPI` each qualify a single request rather than switch anything on,
  and `enableForcedExecution` would not improve any of them. Every boolean under an
  `Ops` spec, including its nested per-action parameter blocks, is outside the
  rule.
- **This is stricter than upstream.** Core Kubernetes has `insecureSkipTLSVerify`
  and `unschedulable`, so a `skip` prefix and a bare negative both exist there.
  They are not accepted here, because a single spelling is what makes the rule
  checkable at all.

Eleven fields violate the rule today, and
`.claude/skills/api-design/scripts/check-crds.py` reports them as
`toggle-not-enable-disable` (§7.4). §9.6 is what fixing them costs.

### 7.6 Immutability

A field a live system cannot tolerate being changed carries `+k8s:immutable`. The
marker generates two rules in controller-gen v0.21.0, which is the version this
repository generates with: a field-level `self == oldSelf`, and for a field outside
`required` a parent-level `!has(oldSelf.X) || has(self.X)`.

| Field is    | Rules generated | Semantics                                                             |
|-------------|-----------------|-----------------------------------------------------------------------|
| `+optional` | both            | Immutable once set: fillable later, then unchangeable and unremovable |
| `Required`  | field-level     | Immutable from creation, since the field is never absent              |

The parent rule covers the case the field rule cannot. A field-level transition
rule is evaluated only where the field is present, so a cleared field would
otherwise be re-settable to any value. A first assignment matches neither rule,
which is what makes the optional case once-set.

One marker therefore expresses both meanings, and the field's own optionality
selects between them. A hand-written
`+kubebuilder:validation:XValidation:rule="!has(oldSelf.X) || self.X == oldSelf.X"`
states the once-set intent at greater length and guards only the value, so it is
the weaker spelling. Twenty-nine fields carry the marker and seven carry the
type-level rule, and `.claude/skills/api-design/scripts/check-crds.py` reports a field that
claims immutability in a doc comment and enforces none of it as
`unenforced-immutability` (§7.4).

### 7.7 Backend state arrives by push

A controller learns about control-plane state from a Server-Sent-Events stream,
not by calling the HTTP API on a timer. Every v2 resource type is streamable, so
every kind this group models reads its backend state from a subscription: the
control plane honors `?watch=true` on the type, the operator holds the streamed
objects in an in-memory store, and reconcilers read that store.

**None of it is shipped, and every design here depends on it.** The subscriptions
arrive with the control plane's SSE work rather than with any design in this group,
which is why a `?watch=true` row in a backend table is an external dependency rather
than an endpoint somebody can call today.
`design-sse-push-notifications.md`, on the `sse` branch, owns the mechanism, the
verified wire contract, and the adoption phases, alongside the
`operator/internal/cpinformer` implementation of the store and the subscription
manager.

**No design in this group specifies a poll.** A `RequeueAfter` still appears where
a controller is waiting on something the stream does not carry, and as a slow
backstop for a stream that died, but it is never the path by which a change is
noticed. A per-kind design that says "poll until" is specifying the mechanism this
one replaces.

Two properties of the stream change how a design is written, and both are
consequences of it being level-triggered rather than an edit log:

- **Delivery coalesces.** A slow subscriber receives current state, not a backlog,
  so intermediate states may never be delivered. A step that waits for a specific
  transition is therefore wrong. The completion condition of an `Ops` step (§3.1)
  is a predicate over current state that also holds for every state beyond it, so
  that a node observed as `online` satisfies a step waiting for `offline` on its
  way there.
- **Reconnects are routine and carry a snapshot.** The stream has no replay and a
  one-hour lifetime cap, so every controller sees a full snapshot regularly. That
  is the resync, and it is the server's rather than the operator's.

The scope of a stream is the resource's path parameters, which decides how many
streams a deployment opens: one for clusters, one per cluster for nodes, pools,
and tasks, and one per pool for volumes and snapshots.

### 7.8 Enum value casing

**Every value of an enum this group defines is PascalCase.** `Activate`,
`RollingRestart`, `HostMaintenance`, `ControlPlane`. Not `activate`, not
`rolling-restart`, not `shutdown_called`.

This is the spelling core Kubernetes uses for every enum it owns, in `PodPhase`,
`PullPolicy`, `ServiceType`, and `PersistentVolumeReclaimPolicy`, and it is
already the spelling of every phase in this group. What makes the rule worth
writing down is that the group does not follow it for anything else: five kinds
spell their phases `Pending;Running;Succeeded;Failed` and their action verbs
`activate;expand;shutdown`, so one object reports two casings and nothing in the
API says which a reader should expect where.

The consequence that costs the most is smaller and less obvious. A PascalCase
value is the same word as its Go constant, so `StorageNodeOpsActionMigrate`
reads straight off `action: Migrate`. A kebab-case value makes the two spellings
diverge, and every reader has to check the constant to find out which hyphenation
the API wants.

**A value that names something outside this group keeps that thing's spelling.**
`ext4` and `xfs` are the kernel's names for filesystems, and `Ext4` would be a
spelling nothing else in the world uses. The same holds for a wire protocol and
for a vocabulary an external API already defines. The test is whether this group
invented the word.

**Reflecting a value is not defining one.** `StorageNode.status.status` holds
`online`, `in_creation`, and `in_restart` because those are the control plane's
strings, and it carries no `Enum` marker for that reason. So do the path segments
of a control-plane endpoint. Neither is covered by this rule.

Ten enums violate it today, six of them in the replication kinds this document
excludes (§2). `.claude/skills/api-design/scripts/check-crds.py` reports them as
`enum-value-not-pascal-case` (§7.4), and §9.7 is what fixing the other four costs.

### 7.9 Observed generation

**Every kind carries `status.observedGeneration`, and every reconcile that writes
status sets it** to the `metadata.generation` the status was computed from. Both
halves are the rule. A field that is declared and never written is worse than an
absent one, because it reports a definite-looking zero rather than nothing, and
this group already has four such fields on one kind, which
[`design-storagecluster.md`](design-storagecluster.md) §3.3 removes.

**Without it a stale status and a disagreeing one are the same observation.** A
user who edits a spec and watches `status` cannot tell whether what they are
reading reflects the edit or predates it, and neither can another controller
waiting on the object. `observedGeneration == metadata.generation` is the only
thing that says the answer is current. That is why a `kubectl wait` on a status
field is unreliable against a kind that lacks it, and why a test asserting a
status after a spec change has to sleep instead of waiting on a condition.

**Status is written with an optimistic-lock patch, which is what keeps the field
honest.** A patch carrying the `resourceVersion` it was computed from is rejected
with 409 when the object has moved on, so a status derived from generation N can
never land on top of one derived from N+1 by a reconciler that started earlier and
finished later. Without the lock the field would record the generation a slow
reconcile read, on a status that has since been overwritten, which is a worse lie
than not recording it at all. This is the same primitive the operation lock uses
(§3.2) and the same one a single-shot creation claim uses, so it is one mechanism
doing three jobs rather than three mechanisms.

**An `Ops` kind carries it too, and the low number of times it moves is the
point.** An operation's spec is immutable except for its abort field (§3.1), so
`observedGeneration` advances at most twice in the object's life. The second
advance is precisely the signal that the abort has been observed, which is
otherwise invisible: a user who sets `spec.abort` and sees an unchanged status
cannot tell a controller that has not looked from one that looked and declined.

No registered type carries the field, in any of the seventeen, which
`.claude/skills/api-design/scripts/check-crds.py` reports as
`no-observed-generation` (§7.4).

### 7.10 Where a kind's controller lives

The controllers are one package per domain under
`operator/internal/controllers/`, and the domains are the ones this document
already draws. A kind's design document, its controller package, and its band in
§8 name the same thing.

| Package                     | Kinds                                                                                               | Design                                                                                                                          |
|-----------------------------|-----------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------|
| `controllers/controlplane/` | `ControlPlane`, `ControlPlaneOps`                                                                   | [`design-controlplane.md`](design-controlplane.md)                                                                              |
| `controllers/driver/`       | `SimplyblockDriver`                                                                                 | [`design-simplyblockdriver.md`](design-simplyblockdriver.md)                                                                    |
| `controllers/deployment/`   | `ClusterDeploymentConfig`, `OperatorOps`                                                            | [`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md)                                                        |
| `controllers/cluster/`      | `StorageCluster`, `StorageClusterOps`                                                               | [`design-storagecluster.md`](design-storagecluster.md)                                                                          |
| `controllers/node/`         | `StorageNode`, `StorageNodeOps`, `StorageDevice`, `StorageDeviceOps`, and the storage-node workload | [`design-storagenode.md`](design-storagenode.md), [`design-storagedevice.md`](design-storagedevice.md)                          |
| `controllers/pool/`         | `StoragePool`, `StoragePoolOps`                                                                     | [`design-storagepool.md`](design-storagepool.md)                                                                                |
| `controllers/volume/`       | `PersistentVolumeOps`, the auto-rebalancer, and the claim controller                                | [`design-persistentvolumeops.md`](design-persistentvolumeops.md), [`design-auto-rebalancing.md`](../design-auto-rebalancing.md) |
| `controllers/backup/`       | `StorageBackupPolicy`, `StorageBackup`, `StorageBackupOps`                                          | [`design-storagebackup.md`](design-storagebackup.md)                                                                            |
| `controllers/replication/`  | The four replication kinds, which are out of scope here (§2)                                        | —                                                                                                                               |

**The device controller is in `node` rather than a package of its own**, because
a device object cannot discover itself: the node's reconciler lists the control
plane's devices and projects them
([`design-storagedevice.md`](design-storagedevice.md) §5.1). Splitting them would
put one half of one loop in each package.

**A shared test package, not a shared controller package.** The four helpers the
suites use, and the `envtest` bootstrap, are a real package rather than a
`_test.go` file, because a `_test.go` file cannot be imported across packages.
There is no shared *controller* package: what looked like one held only the
auto-rebalancer's Job scaffolding, which belongs to `volume` and which
`replication` imports.

### 7.11 Short names

Every kind in the target model declares a `shortName`, and this is the roster.
Seven of the thirteen registered kinds carry none today (§1), so most rows here are
additive and each kind's own design is where the marker is written.

| Kind                      | Short name | Assigned by                                                                 |
|---------------------------|------------|-----------------------------------------------------------------------------|
| `ClusterDeploymentConfig` | `cdc`      | [`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md)    |
| `OperatorOps`             | `oops`     | [`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §7 |
| `ControlPlane`            | `cp`       | [`design-controlplane.md`](design-controlplane.md) §3                       |
| `ControlPlaneOps`         | `cpops`    | [`design-controlplane.md`](design-controlplane.md) §6                       |
| `SimplyblockDriver`       | `sbd`      | [`design-simplyblockdriver.md`](design-simplyblockdriver.md) §3             |
| `StorageCluster`          | `stc`      | [`design-storagecluster.md`](design-storagecluster.md) §3                   |
| `StorageClusterOps`       | `scops`    | [`design-storagecluster.md`](design-storagecluster.md) §5                   |
| `StorageNode`             | `sn`       | [`design-storagenode.md`](design-storagenode.md) §3                         |
| `StorageNodeOps`          | `snops`    | [`design-storagenode.md`](design-storagenode.md) §6                         |
| `StorageDevice`           | `sd`       | [`design-storagedevice.md`](design-storagedevice.md) §4                     |
| `StorageDeviceOps`        | `sdops`    | [`design-storagedevice.md`](design-storagedevice.md) §6                     |
| `StoragePool`             | `sp`       | [`design-storagepool.md`](design-storagepool.md) §3                         |
| `StoragePoolOps`          | `spops`    | [`design-storagepool.md`](design-storagepool.md) §7                         |
| `PersistentVolumeOps`     | `pvops`    | [`design-persistentvolumeops.md`](design-persistentvolumeops.md) §4         |
| `StorageBackupPolicy`     | `sbp`      | [`design-storagebackup.md`](design-storagebackup.md) §4                     |
| `StorageBackup`           | `sb`       | [`design-storagebackup.md`](design-storagebackup.md) §5                     |
| `StorageBackupOps`        | `sbops`    | [`design-storagebackup.md`](design-storagebackup.md) §6                     |

**The `Ops` names are the entity's name with `ops` appended**, which is what makes
the pair guessable from either half: `sn` and `snops`, `sp` and `spops`, `sd` and
`sdops`. `scops` and `pvops` predate the rule and keep their spelling, since both
are registered or specified against and a short name is what scripts are written
with.

**`StorageCluster` is `stc` rather than `sc`, because `sc` is `StorageClass`'s in
core `storage.k8s.io/v1`.** Two kinds may declare the same short name and the
RESTMapper resolves it by discovery order, so a duplicate would never reach this
kind. The operator writes one `StorageClass` per pool (§5), which puts both in
every cluster, and `scops` is unaffected because nothing else claims it. The same
check applies to any name added later: `cp`, `sp`, `sd`, `sb`, and `sbd` collide
with nothing in core or in the CSI and snapshot groups this product deploys beside.

**Two registered kinds get none, and both by decision.** `StorageNodeSet` is
retired (§9.2) and `Task` is not part of the target model (§9.1), so a short name
on either is API surface added to a kind that is going away. They are the two blanks
in §7.1 that stay blank.

`NFSExport` is specified on another branch (§7.2), and its short name belongs to
that design rather than to this table.

### 7.12 Metric names

Every metric this operator exports is named
`simplyblock_<entity>_<item>_<agg>`. The entity is the lowercased kind the metric
is about, which is the same spelling `storage.simplyblock.io/managed-by` carries
(§7.3). The item is what is measured. The aggregation is one of six words, and no
metric ends in anything else.

| Aggregation | For                                                  | Example                                              |
|-------------|------------------------------------------------------|------------------------------------------------------|
| `total`     | A counter, and only a counter                        | `simplyblock_storagenode_operations_total`           |
| `seconds`   | A duration                                           | `simplyblock_storagepool_operation_duration_seconds` |
| `bytes`     | A size                                               | `simplyblock_storagedevice_capacity_bytes`           |
| `count`     | A gauge counting things                              | `simplyblock_storagepool_volumes_count`              |
| `state`     | A gauge that is 1 for the current value of a label   | `simplyblock_storagecluster_phase_state`             |
| `info`      | A gauge that is 1 and carries the fact in its labels | `simplyblock_controlplane_version_info`              |

**`total` means a counter, which is what keeps a ratio honest.** A gauge ending in
`total` reads as something a `rate()` applies to, and two of them existed: a count
of a node's devices and a count of the workers a driver expects. Both are gauges and
both are now `count`, so `_total` in this group never names a value that can go down.

**An operation's metrics are the entity's.** A `StorageNodeOps` records against
`simplyblock_storagenode_operations_total` rather than a subsystem of its own,
because the question an operator asks is what has happened to a node, and the
`action` label already says which operation it was. That keeps everything about one
kind under one prefix, which is what a dashboard selects on.

---

## 8. Layers

Reading the diagram left to right, it groups into five bands.

### 8.1 Bootstrap layer

`ControlPlane`, `SimplyblockDriver`, `ClusterDeploymentConfig`, and the
operator process itself, with `ControlPlaneOps` and `OperatorOps` as the
corresponding actions. The band establishes that a control plane is reachable, a
CSI driver is installed, and a deployment has been described. §6 covers it in
full, because bootstrap is the one band where the operator process rather than a
resource is the actor.

### 8.2 Cluster and fleet layer

`StorageCluster` is the unit of a simplyblock backend cluster, carrying the
erasure-coding layout, the fabric type, the HA mode, the port ranges, the
capacity thresholds, and the policies for volume migration and auto-rebalancing.
Much of its spec is immutable once set, because those fields describe on-disk and
on-wire layout that cannot be changed under a live cluster. Everything mutable at
the cluster level that is not expressible as desired state, meaning activate,
expand, shut down, restart, and roll the nodes one by one, goes through
`StorageClusterOps`. A cluster-wide operation touches every node underneath it,
so only one may be active at a time, and `status.activeOpsRef` names the holder.

Beneath it, `StorageNode` and then `StorageDevice` narrow the scope from the
cluster to one worker node and NUMA socket, and then to a single NVMe device.
Each level has its own `Ops` companion, so an operation can be scoped as tightly
as it needs to be: restart one device, drain one node, or roll the whole fleet.
Choosing between them is a blast-radius decision. The same physical outcome is
often reachable from more than one level, and the narrowest resource that
achieves it is the one to use.

### 8.3 Tenancy layer

`StoragePool` carves a cluster into units with their own capacity limits and QoS
ceilings for IOPS and throughput. It is the boundary a `StorageClass` is
generated for, and therefore the join point between the storage administrator's
world and the application developer's world (§5).

Both directions of that join exist, and only one of them is drawn. The pool is
the creating end: `createStorageClassIfNotExists` in the pool controller makes
exactly one class per pool, and the pool's finalizer deletes it again (§5). What
the class carries back is what the diagram shows as `references`, and it is not a
Kubernetes reference at all. A `StorageClass` is the CSI provisioning contract
rather than a description of storage: it names a `provisioner` and hands it an
opaque `parameters` map, and Kubernetes interprets neither. Simplyblock's class
points `provisioner` at the CSI driver and puts `cluster_id` and `pool_name` in
`parameters`, so a pool's identity reaches the driver as two strings the API
server does not validate and the garbage collector does not track. The
`storage.simplyblock.io/{namespace,cluster,pool}` labels alongside them are how
the operator finds its own classes again, which is correlation rather than
reference as well. The whole join is therefore held together by a naming
convention, an opaque parameter map, and a finalizer, rather than by the API.

Because `StorageClass.parameters` is immutable in the Kubernetes API, a pool's
`spec.storageClassParameters` is immutable as well, enforced by `+k8s:immutable`
rather than by a doc comment. Changing a pool's defaults means creating a new
pool.

### 8.4 Volume layer

Core Kubernetes types plus one action kind of the operator's own.
`PersistentVolumeOps` covers per-volume operations, of which today's
`VolumeMigration`, moving a volume's backing logical volume to a different
storage node, is exactly the shape. `NFSExport` belongs to this band as well,
though it is not drawn (§7.2).

### 8.5 Data-protection layer

`StorageBackupPolicy`, `StorageBackup`, and `StorageBackupOps` are the backup
chain. The policy schedules and retains against a claim, a backup is one
point-in-time copy of a volume, and the operations kind restores a backup into a
claim.

Group snapshots are **not** a simplyblock kind. The upstream
`VolumeGroupSnapshot` in `snapshot.storage.k8s.io` already selects several claims
by label and snapshots them atomically, which is the missing piece when an
application's state spans several claims and snapshotting them independently
produces a torn image. Implementing the upstream feature rather than a private
mechanism keeps the group visible to any backup tool that understands CSI, and
that is the decision taken in
`design-pnfs-striped.md` §5, on the `pnfs-design` branch, which also owns the CSI
group controller service that backs it. The operator chart does not ship the
`VolumeGroupSnapshot`, `VolumeGroupSnapshotClass`, and `VolumeGroupSnapshotContent`
CRDs yet, and shipping them is phased work in that design rather than an existing
capability.

---

## 9. Migration Strategy

Every change in this section is breaking. A CRD's `kind` is part of its identity,
so a rename is a new CRD, a conversion path for existing objects, and 
deprecation of the old one, rather than an edit to a field. The group is at
`v1alpha1`, which is the version where that is affordable, and it stops being
affordable at the first version users are entitled to keep.

### 9.1 Renames, absorptions, and retirements

| Today             | Target                | Change     | Consequence                                                                                                                                                                     |
|-------------------|-----------------------|------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Task`            | Not reworked          | —          | The mirror is out of the target model. What becomes of the registered kind is re-sync rather than a migration                                                                   |
| `BackupPolicy`    | `StorageBackupPolicy` | Rename     | Existing policies must be converted, because a policy is user-authored state                                                                                                    |
| `VolumeMigration` | `PersistentVolumeOps` | Absorption | The action becomes `Migrate`. The kind becomes cluster-scoped, so the fan-out's owner reference becomes `spec.creatorRef`, the `managed-by` label, and a finalizer cascade (§3) |
| `BackupRestore`   | `StorageBackupOps`    | Absorption | The action becomes `Restore`, and the kind's spec becomes that action's parameters                                                                                              |
| `BackupImport`    | Retired               | Retirement | The store is the inventory, so a backup another cluster wrote is discovered rather than imported                                                                                |

**What becomes of the registered `Task` kind is not decided here.** The mirror kind is
out of the target model, which settles that nothing is built. It does not settle whether
the CRD that exists today is retired the way §9.2 retires `StorageNodeSet`, or left
registered and unreworked. The two differ in what a user sees: retiring it removes
objects somebody may be reading, and leaving it keeps a kind no design covers.

### 9.2 Retiring StorageNodeSet

The fleet template stops being a resource and becomes
`ClusterDeploymentConfig.nodeSets[]`, so the spine loses a level and
`StorageCluster` owns `StorageNode` directly (§5). Three things move.

**The per-node configuration moves into the deployment config.**
`StorageNodeSet.spec.nodeConfigs[workerNode]` is the single source of truth for
per-node configuration today, written into each `StorageNode` by the set's
controller rather than edited by users. Its equivalent is a group entry under
`nodeSets[]`, carrying the worker list, the management and data interfaces, and
the devices. The set-wide limits are `nodeSets[].sizing`, grouped per node set
inside the config because they are what a node set is for.

**The node's parent reference changes.** `StorageNode.spec.storageNodeSetRef` is
required today. It becomes a reference to the owning `StorageCluster`, and the
node set survives only as the name of the group the node was declared in.

**The fleet machinery reparents to the cluster.** This is the substantive part of
the retirement. The set's controller currently owns, by controller reference, the
DaemonSet that runs the storage nodes, the storage-node Services and their
Endpoints, the certificates, the ServiceAccount, and the per-node ConfigMaps. All
of them become children of `StorageCluster`, and their configuration becomes one
`spec.storageNodes` group on that kind. Until that reparenting is done, the
retirement cannot proceed, because deleting a `StorageNodeSet` today is what
tears those objects down.
[`design-storagenode.md`](design-storagenode.md) §5 specifies the workload and
its ownership, and its §15.3 is the retirement's full inventory.

**The drain coordinator moves with it.** `StorageNodeSet.status.drainCoordination`
carries the eight-phase workflow that shuts a storage node down for a Kubernetes
node drain and brings it back, driven by a controller of its own. It becomes a
`StorageNodeOps` action, specified in
[`design-storagenode.md`](design-storagenode.md) §10.

### 9.3 The ownership edges that do not exist yet

Of the solid edges in §5, one is real. The others are drawn ownership that is
implemented as a name reference, which means deletion does not cascade and the
garbage collector is not maintaining the tree.

| Edge                                   | Today                                             | To establish                                                                                                                                                               |
|----------------------------------------|---------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `StorageNodeSet` owns `StorageNode`    | Controller reference, real                        | Reparent to `StorageCluster` with the retirement (§9.2)                                                                                                                    |
| `StorageCluster` owns `StorageNodeSet` | `spec.clusterName` string                         | Removed by the retirement rather than established                                                                                                                          |
| `StorageCluster` owns `StoragePool`    | `spec.clusterName` string, immutable              | Established. [`design-storagepool.md`](design-storagepool.md) §6 decides it: the pool's finalizer holds while volumes are bound, so the cascade cannot destroy tenant data |
| `StorageNode` owns `StorageDevice`     | A `status.devices` summary string, `online/total` | Established by [`design-storagedevice.md`](design-storagedevice.md) §5.3, which also argues why the summary stays beside the objects                                       |
| `ControlPlane` owns `StorageCluster`   | Not drawn as ownership, `ControlPlane` manages it | Left as a reference deliberately, because a control plane may be reused rather than installed (§6)                                                                         |

The pool row had the one real decision behind it, and
[`design-storagepool.md`](design-storagepool.md) §6 takes it. Making the edge an
owner reference means deleting a `StorageCluster` deletes its pools, which would
delete their `StorageClass` objects and leave bound claims pointing at a class
that no longer exists. What makes it safe is that a pool with bound volumes
refuses to finish deleting: the cascade starts, the pool holds in `Terminating`,
and the cluster holds behind it, so a `kubectl delete storagecluster` visibly does
not complete rather than silently destroying tenant data.

### 9.4 Annotation and label key prefixes

The twenty-eight keys on the bare `simplyblock.io` prefix move to
`storage.simplyblock.io` (§7.3). This breaks more quietly than a kind rename does,
because no API server rejects the old key: a claim annotated
`simplyblock.io/backup-policy` simply stops having a backup policy, and nothing
anywhere reports that it used to. So each key needs a release in which both spellings
are read and the old one is deprecated in an event or a condition, before the old
one stops being read at all. Each key is an independent change of the same shape,
so the count governs sequencing rather than difficulty.

### 9.5 Adopting the Ops shape

Every registered `Ops` kind diverges from §3.1, and each diverges differently, so
this is three separate pieces of work rather than one sweep.

| Kind                | Today                                                                                                        | To reach §3.1                                                                                    |
|---------------------|--------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| `StorageNodeOps`    | Typed `status.subPhase` with the union of two disjoint workflows, driven by a hand-rolled `switch`           | Rename the field to `step`, declare the two graphs as one `MultiConfig`, and delete the `switch` |
| `StorageClusterOps` | No step field at all. One action carries its steps in `status.message`, another in a per-action status block | Add `status.step`, declare a graph per action, and retire both improvised carriers               |
| `VolumeMigration`   | One enum merging phase and step, so `Validating` sits beside `Completed`                                     | Split the enum into `phase` and `step` as the kind is absorbed into `PersistentVolumeOps` (§9.1) |

**The rename is the cheap half and the graphs are the expensive half.** Both are
status, so no user-authored object has to be converted, and the only readers are
the operator itself and whatever tooling greps `kubectl -o jsonpath`. Declaring
the graphs means the controller stops choosing its next step inline, which changes
where every side effect is invoked from.

The rename is more than a key change, because `subPhase` is a string and
`status.step` is an object (§3.1). A converting reconcile reads the old string
into `step.state` and leaves `step.deadline` absent, which restores as a step with
no deadline. That is the pre-rename behavior, so an operation in flight across the
upgrade keeps running rather than expiring immediately.

`StorageNodeOps` is the one to convert first. It is the only kind that already has
a typed step enum and disjoint per-action workflows, which is exactly the shape
`MultiConfig` exists for, and the package documentation uses it as its worked
example.

### 9.6 Renaming the boolean toggles

Eleven spec fields across five kinds do not match §7.5. Every one is user-authored
spec rather than status, which makes this the most breaking section of this
document: a renamed spec field is silently ignored on an object that still sets the
old name, so a cluster whose `StoragePool` sets `dhchap: true` loses
authentication rather than failing to apply.

| Today                      | Default | Target                       | On                                           |
|----------------------------|---------|------------------------------|----------------------------------------------|
| `skipKubeletConfiguration` | off     | `enableKubeletConfiguration` | `StorageNodeSetSpec`, `StorageNodeOverrides` |
| `migrationEnabled`         | on      | `disableMigration`           | `VolumeAutoPlacementSettings`                |
| `latencyBenchmarkEnabled`  | off     | `enableLatencyBenchmark`     | `VolumeAutoPlacementSettings`                |
| `enabled`                  | on      | `disableVolumeMigration`     | `VolumeMigrationSettings`                    |
| `enabled`                  | on      | `disableDataRealignment`     | `DataRealignmentSettings`                    |
| `enabled`                  | off     | `enableVolumeAutoPlacement`  | `VolumeAutoPlacementSettings`                |
| `withCompression`          | off     | `enableCompression`          | `BackupSpec`                                 |
| `encryption`               | off     | `enableEncryption`           | `StorageClassParameters`                     |
| `replicate`                | off     | `enableReplication`          | `StorageClassParameters`                     |
| `dhchap`                   | off     | `enableDHCHAP`               | `StoragePoolSpec`                            |

`skipKubeletConfiguration` inverts as well as renames, since the negative it
carries today is the opposite of the negative §7.5 would give it. Reading the old
field and writing the new one during a deprecation window therefore means negating
it, which is the one row where a mechanical rename produces the wrong behavior.

The three `enabled` fields are the awkward ones. Each is a bare toggle inside a
settings struct, so the name it needs is the parent's subject rather than
something already in the field, and `spec.volumeAutoPlacement.enabled` becomes
`spec.volumeAutoPlacement.enableVolumeAutoPlacement` unless the struct is
flattened at the same time. Whether to flatten is not decided here.

Two further fields are toggles the checker cannot see, because neither carries a doc
comment saying so: `snapshotBackups` and `localTesting` on `BackupSpec`. They need
the same rename and have to be found by reading.

The chart carries the same names, so `helm-charts` moves with the API:
`skipKubeletConfiguration` appears in `values.yaml`, and `multiCluster.enable` is a
third spelling that exists only there.

### 9.7 Recasing the enum values

Four enums in scope carry values that are not PascalCase (§7.8). Each is
user-authored spec or a status string the operator alone writes, and the two halves
break differently.

| Today                                                              | Target                                                           | On                        |
|--------------------------------------------------------------------|------------------------------------------------------------------|---------------------------|
| `activate;expand;shutdown;start;restart;node-rolling-restart`      | `Activate;Expand;Shutdown;Start;Restart;RollingRestart`          | `StorageClusterOpsAction` |
| `shutdown;restart;suspend;resume;remove;migrate`                   | `Shutdown;Restart;Suspend;Resume;Remove;Migrate;HostMaintenance` | `StorageNodeOpsAction`    |
| `controlplane;prometheus;uniform`                                  | `ControlPlane;Prometheus;Uniform`                                | `MetricsBackend`          |
| `detected;shutdown_called;draining;restart_called;complete;failed` | Retired with `StorageNodeSet` (§9.2)                             | `NodeDrainState.Phase`    |

**The two action enums are the breaking ones**, because an operation is created by
a user or by a tool and a value outside the enum is rejected at admission. That is
the good failure: a `StorageClusterOps` with `action: activate` stops being
accepted rather than being accepted and ignored, so nobody discovers the rename by
finding an operation that never ran. A deprecation window that accepts both
spellings is possible by widening the `Enum` marker and normalizing in the
controller, and it is worth the trouble only for the actions, which appear in
scripts and runbooks.

`MetricsBackend` is a `StorageCluster` spec field and breaks the same way.
`NodeDrainState.Phase` needs nothing: it is status the operator alone writes, and
the kind carrying it is retired.

The four are specified with their kinds:
[`design-storagecluster.md`](design-storagecluster.md) §5.3 and
[`design-storagenode.md`](design-storagenode.md) §6.3.
