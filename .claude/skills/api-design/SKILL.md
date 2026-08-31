---
name: api-design
description: Designing this operator's CRDs and keeping them consistent with each other: the Entity and Ops split for imperative operations, when a new field beats a new kind, spec against status, the marker set every type carries (validation, enum, immutability, defaults, printcolumns, short names), typed phases, observedGeneration and conditions, and what counts as a breaking change once a field has shipped. Carries a checker that audits every type against those conventions. Use when adding a CRD, changing or reviewing one, adding a field or an action to an existing kind, or auditing the API surface for consistency.
---

# CRD and API design

The conceptual model is already written down: **read
`operator/docs/designs/design-crd-model.md` §3 first.** It argues the Entity and
Action split, the ownership spine, and the layering, and this skill does not
restate it. What follows is the mechanics: the shapes, the markers, and the
decisions that a design document leaves to the implementation.

References:

- `references/ops-crds.md`: the `<Entity>Ops` shape, field by field, and how an
  action is added to an existing one.
- `references/markers.md`: the marker set and what each is for.
- `references/consistency.md`: what the checker checks, the three spellings of
  immutability that work and the one that does not, and the current backlog.
- `scripts/check-crds.py`: audits the types against the conventions below and
  prints the marker adoption. Run it on any API change:

  ```bash
  .claude/skills/api-design/scripts/check-crds.py --changed
  ```

  `--design <doc.md>` runs the same audit over the Go in a design document's
  appendices, which is where a per-kind design states the type it specifies. A
  convention broken in a document becomes a convention broken in the API a release
  later, so the cheapest place to catch it is before anything is implemented. The
  `design-doc` skill's step 3a owns what that appendix has to contain.

  **Do not read adoption numbers out of this skill's prose.** They went stale
  within weeks the last time they were written by hand: short names were
  documented as absent while ten types carried one. `--adoption` reads them from
  the code.

## The first decision: field, action, or kind

| The change is                                                         | Model it as                                   | Because                                                                                       |
|-----------------------------------------------------------------------|-----------------------------------------------|-----------------------------------------------------------------------------------------------|
| Something the system should *be*                                      | A field on the entity CRD                     | Desired state converges and is safe in Git                                                    |
| Something to *do once* to one entity                                  | An action on that entity's `<Entity>Ops` kind | "Restart this node" is not a desired state, and once done it is indistinguishable from before |
| A new thing with its own lifecycle, watched and reconciled on its own | A new entity CRD                              | It has status of its own, and something else references it                                    |
| A new thing with no independent lifecycle                             | A field or a nested struct on its owner       | A CRD costs a controller, RBAC, a chart entry, and a place in the ownership spine             |

The bar for a new CRD is that **something watches it or references it by name**.
`ReplicationSlot` clears it, because the policy creates slots and reconciles each one.
A per-node tuning block does not: it is a nested struct on the node.

If a CRD's name ends in `Ops`, it is one-shot, it names exactly one target, and
it terminates. That is the whole contract, and it is mechanical on purpose, so an
operator never has to ask whether a scope supports operations, only which actions
its `Ops` kind accepts.

## Spec against status

- **Spec is the user's.** A controller never writes it. An operation that needs
  to remember what it is acting on copies the value into status
  (`status.actionStatus.nodeUUID`) rather than reading spec twice: that is what
  makes a mid-flight spec change detectable rather than silently followed.
- **Status is the controller's**, and it is reconstructable. Anything a later pass
  or a restart needs (the phase, the sub-phase, the write-ahead flag, the ID of
  the thing being operated on, and the deadline) lives there. See the
  `reconciler-patterns` skill.
- **A field that a user sets and a controller also sets does not exist.** Split
  it: `spec.enabled` against `status.active`, `spec.targetNodeUUID` against
  `status.boundNodeUUID`.
- **Progress counters belong in status** (`volumesMigrated`, `volumesPending`) and
  are what a `printcolumn` shows.
- **A boolean toggle is `enableXyz` or `disableXyz`**, chosen by the default so
  that the zero value is the default: `enableXyz` when the thing is off, and
  `disableXyz` when it is on. `skipXyz`, `withXyz`, `xyzEnabled`, and a bare
  `enabled` are rejected, and the checker reports them as
  `toggle-not-enable-disable`. See `references/consistency.md`.

## What every new type carries

The full table with adoption numbers is in `references/markers.md`. The minimum:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snops
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.storageNodeRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
```

- **`subresource:status`** on every type: 17 of 17 have it. Without it a status
  write needs the spec RBAC and a status update bumps `generation`.
- **`printcolumn`** for the two or three fields that answer "what is happening"
  without a `describe`. There are 83 across the API today, and the Ops kinds are
  the model: target, action, phase, sub-phase, message, age.
- **`shortName`:** ten of seventeen types carry one, including the `scops`,
  `relpair`, and `repl` that their designs name. Seven do not, so a scenario
  asserting `kubectl get sc-something` on one of those cannot pass. Declare one
  whenever the design names one.
- **A typed enum for every closed set**, and the Go type to go with it. An
  `Enum` marker makes an unknown value a rejection at admission instead of a
  `Failed` phase discovered later, and a named Go type makes it a compile error
  before that. Both matter, and the checker reports each separately:
  `enumless-closed-set` for a named type whose constants the API server does not
  constrain, `untyped-phase` for a `Phase string` that should be a
  `<Kind>Phase`. Seven phases are still plain strings.
- **An enum value this group defines is PascalCase.** `Activate`, `RollingRestart`,
  `HostMaintenance`. Not `activate`, not `rolling-restart`, not `shutdown_called`.
  This is the spelling core Kubernetes uses for every enum it owns (`Pending`,
  `IfNotPresent`, `ClusterIP`, `Retain`), and it is already the spelling of every
  phase in this group, so a lowercase action verb beside a `Pending` phase is
  inconsistent within a single object. It is also what makes the Go constant and
  the wire value the same word, which is what lets `StorageNodeOpsActionMigrate`
  be read off `action: Migrate` without a lookup table.

  **The exception is a value that names something outside this group**, where that
  thing's own spelling wins: `ext4` and `xfs` are the kernel's names for
  filesystems, and a value mirrored from the control plane's vocabulary keeps the
  control plane's casing. The test is whether this group invented the word. The
  checker reports the rest as `enum-value-not-pascal-case`, and carries the
  foreign-value list.
- **`observedGeneration` in status, on every kind, written on every status
  update.** Both halves are the rule: a declared field nothing writes reports a
  definite-looking zero rather than nothing. Without it a stale status and a
  disagreeing one are the same observation, so a spec edit cannot be waited on and
  a test has to sleep instead. Status is written with an optimistic-lock patch,
  which is what stops a status computed from generation N landing over one
  computed from N+1. No type has it, in any of the seventeen, which is the group's
  largest single gap. `design-crd-model.md` §7.9 is the rule and
  `reconciler-patterns` is the mechanics.
- **A phase, and conditions when something waits.** Ten kinds report progress
  through `status.phase` and a message, three through `status.conditions`, and
  none through both. For a new type: a typed phase always, because that is what
  the printcolumns and the reconcilers read, and `conditions` in addition when a
  user or another controller waits on the object. A `conditions` field carries
  `+listType=map` and `+listMapKey=type` or server-side apply replaces the whole
  list instead of merging it, and none of the three existing ones do.

## Immutability is a marker, not a sentence

A doc comment reading `// Immutable.` enforces nothing. Six fields across
`ReplicationOps` and `ReplicationSlot` are immutable in prose only, and a mutable
action or target on a running operation is the "spec changed mid-flight" failure
the reconciler then has to detect and fail on, when admission could have refused
the edit. `check-crds.py` reports each as `unenforced-immutability`.

**The correct spelling is `// +k8s:immutable`, for both meanings.** It does not
look like a `+kubebuilder:` marker, which is why it is taken for inert
documentation. It emits two rules, and which of them apply depends on whether the
field is required:

- **On an optional field:** immutable **once set**. A field-level
  `self == oldSelf` rejects a change, and a parent-level
  `!has(oldSelf.X) || has(self.X)` rejects removal. Neither blocks a first
  assignment.
- **On a `Required` field:** immutable from creation, because the field can never
  be absent for the field-level rule to skip.

The hand-written `!has(oldSelf.x) || self.x == oldSelf.x` is the longer spelling of
the once-set case and guards only the value. `references/consistency.md` has the
generator source lines. Write CEL when the rejection message has to say something
the generated one cannot.

An `Ops` spec is immutable in its entirety: target, action, and parameters. The
object is a request, and a request that can be rewritten while it runs is a
different request.

## A reference to another kind is resolved at admission

**Every field naming another object in this API group is checked by a validating
webhook on create, which rejects a reference that does not resolve.** `clusterRef`,
`nodeRef`, `poolRef`, `backupRef`, `creatorRef`, and the per-action target of every
`Ops` kind are all this. The webhook does one thing: it gets the named object, and
refuses the create when there is nothing there.

**The reason is the section above.** These references are immutable, almost without
exception, so a reference that is wrong at creation is wrong for the object's whole
life: the field cannot be edited, and the only remedy is to delete the object and write
it again. That is precisely what a rejected create asks for, except that the rejection
says so at once and at no cost, while admitting it produces an object parked in
`Pending` with a message whose only action is the deletion the rejection would have
required. A mutable reference is a weaker case for the same check, and still worth it.

**Existence is admission's, readiness is the controller's.** The two are different
questions and putting either in the other's place is wrong. Whether the named object
exists is fixed for an immutable field, so it is decided once, at create. Whether it is
*ready* — a cluster with no `status.uuid` yet, a pool still being created, a node that
is offline — is true at one moment and false at the next, and an admission decision is
never revisited, so deciding it there would decide it wrongly for most of the object's
life. A reference that resolves to an object that is not ready is therefore admitted,
and the controller holds in `Pending` with an event saying what it is waiting for.

**Do not do it when it is not possible.**

- The field does not name a Kubernetes object. A backend identifier is not a
  reference however much its name looks like one, and it takes a format rule on the
  type instead: `sourceClusterID` carries a control-plane UUID and is validated by
  `+kubebuilder:validation:Pattern`, not by a lookup. A `*Ref` suffix on such a field
  is the naming defect that hides this, so fix the name first.
- The object is core Kubernetes and legitimately arrives later. A claim a restore
  creates, or a `PersistentVolume` that provisioning has not produced yet, cannot be
  required to exist at admission.

**Do not do it when it is not necessary.**

- The operator writes the reference itself. An object the operator creates from a
  template names the parent that created it and always resolves, so the check buys
  nothing on that path — but write it anyway when a user *can* author the kind by
  hand, because that is the path it exists for.
- The field is in `status`. Status is the operator's, and a reference it wrote that
  does not resolve is a bug to fix rather than input to reject.

**What it costs, so nobody is surprised by it.** A reference checked at create imposes
an ordering on a single apply: a pool whose cluster is in the same directory is rejected
if it reaches the API server first. That is recoverable — `kubectl apply` is re-run, and
a chart or a `ClusterDeploymentConfig` orders the objects — and it is the price of
catching the misspelling that would otherwise be permanent. Say it in the design rather
than letting somebody discover it.

**Two mechanical notes.** The webhook needs `get` on every kind it resolves, which is
usually already in the manager's role. And a bare name means the same namespace, so a
reference from a cluster-scoped kind carries a namespace and a name: a label value
admits no `/`, and a cluster-scoped list makes a bare name ambiguous.

## Once it has shipped

A field that has reached a user's manifest or a Helm value is an API. Safe:
adding an optional field, adding an enum value, widening validation, adding a
printcolumn or a short name, adding a status field. Breaking: removing or
renaming a field, removing an enum value, narrowing validation, tightening a
default, changing a field's type or its meaning.

Everything is `v1alpha1` today and there is no conversion webhook, so a breaking
change has no migration path other than a manual one. That is the reason to spend
the effort on the marker and the name now: `simplyBlockAnnotations` in the chart's
`values.yaml` is a shipped key with the brand mis-cased, and it cannot be fixed
without breaking every manifest that sets it.

Naming: group `storage.simplyblock.io`, `PascalCase` kinds, `camelCase` JSON,
annotation keys `simplyblock.io/<kebab-case>`. American English and the brand as
one word. The `house-style` identifier gate checks both, and a manifest key is
the least forgiving place to get it wrong.

## Before handing an API change back

1. The change is a field, an action, or a kind, and the reason is stated.
1a. Every reference to another kind in this group is resolved by a webhook on
    create, or the design says which of the two exemptions applies.
2. Closed sets are typed enums, and immutable fields carry a CEL rule, not a
   comment.
3. Status carries `observedGeneration`, the phase, and whatever a restart needs.
4. Printcolumns answer "what is happening," and a short name exists if the
   design named one.
5. `check-crds.py --changed` reports no errors for the type being changed, and
   its warnings are either resolved or named as pre-existing backlog.
6. `make -C operator manifests generate`, then `make helm-sync`, then the
   hand-wired list in the `new-files` skill (the CRD kustomization entry is not
   generated).
7. The design document and its test plan are updated in the same change.
