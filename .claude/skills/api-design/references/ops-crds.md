# The `<Entity>Ops` shape

`design-crd-model.md` §3 argues why the pattern exists. This is what one looks like,
taken from the three that exist: `StorageClusterOps`, `StorageNodeOps`,
`ReplicationOps`.

## Spec

```go
type StorageNodeOpsSpec struct {
    // StorageNodeRef names the StorageNode this operation acts on. Immutable.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storageNodeRef is immutable"
    StorageNodeRef string `json:"storageNodeRef"`

    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove;migrate
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="action is immutable"
    Action string `json:"action"`

    // TargetWorkerNode is the worker to migrate to. Only for action=migrate.
    // +optional
    TargetWorkerNode string `json:"targetWorkerNode,omitempty"`

    // Drain carries the parameters of a drain-remove.
    // +optional
    Drain *DrainOpsSpec `json:"drain,omitempty"`
}
```

Four rules the existing kinds follow:

- **Exactly one target,** named as `<entity>Ref`. An operation over a set is a
  different design (a fleet-level entity field, or one Ops per target), because
  partial success across a set has no representable outcome in one status.
- **The action is a typed, enumerated verb.** All three kinds validate it with
  `Enum`, which is what turns an unknown action into an admission rejection
  instead of a `Failed` phase found later.
- **Per-action parameters are optional, and grouped:** a nested struct when
  there are more than one or two (`Drain *DrainOpsSpec`). A flat spec with ten
  optional fields, nine irrelevant to the chosen action, is unreadable and
  unvalidatable.
- **The whole spec is immutable.** The object is a request. None of the three
  kinds enforces this today — eight doc comments claim it and no CEL
  rule backs any of them, which leaves the reconciler to detect a mid-flight
  rewrite it could have refused. Add the rules on the next change to each.

A parameter that only applies to one action should say so in its doc comment, and
where CEL can express it, refuse the combination:

```go
// +kubebuilder:validation:XValidation:rule="self.action != 'migrate' || has(self.targetWorkerNode)",message="targetWorkerNode is required for action=migrate"
```

## Status

```go
type StorageNodeOpsStatus struct {
    Phase    StorageNodeOpsPhase    `json:"phase,omitempty"`
    SubPhase StorageNodeOpsSubPhase `json:"subPhase,omitempty"`
    Message  string                 `json:"message,omitempty"`

    // Triggered records that the side effect of the current step was issued,
    // so a restart does not repeat it.
    Triggered bool `json:"triggered,omitempty"`

    VolumesMigrated int `json:"volumesMigrated,omitempty"`
    VolumesPending  int `json:"volumesPending,omitempty"`

    StartedAt   *metav1.Time `json:"startedAt,omitempty"`
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}
```

Add for a new kind: `ObservedGeneration int64`, and `PhaseDeadline *metav1.Time`
when the phases are bounded (see the `reconciler-patterns` skill — the deadline
is what makes a per-phase timeout survive a restart).

`Message` carries the reason a phase is what it is, and it is the field a
`printcolumn` shows. It is not a log: one sentence, the current reason, replaced
as the phase moves.

## The lock on the entity

The operation is one side of a pair. The entity carries the other:

```go
// ActiveOpsRef names the operation currently running against this cluster.
// Empty when none is.
// +optional
ActiveOpsRef string `json:"activeOpsRef,omitempty"`
```

The Ops controller acquires it before the first side effect, and releases it on
every terminal path, only if it owns it. That is what makes "one operation at a
time" enforceable rather than hoped for — see `reconciler-patterns` §6.

## Printcolumns

Six, in this order, on all three kinds: target, action, phase, sub-phase,
message, age. An operator watching a drain should need `kubectl get snops` and
nothing else.

## Adding an action to an existing kind

1. **Add the verb to the `Enum`** on `action`. Adding an enum value is
   backward-compatible; removing one is not.
2. **Add its parameters** as an optional nested struct, with a CEL rule tying
   them to the action where possible.
3. **Add the phase or sub-phase constants** the action needs, typed.
4. **Dispatch it** in the controller's action switch, and fail unknown actions
   terminally — admission catches them first, but an older CRD in a cluster may
   not have the new enum.
5. **Regenerate and sync:** `make -C operator manifests generate`, then
   `make helm-sync`.
6. **Design document and test plan** in the same change: the action's happy path,
   its rejection cases, and the restart case per phase.

## What is still missing across all three kinds

| Gap                                                  | Consequence                                                                                                                              |
|------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| No CEL immutability on target, action, or parameters | A running operation can be rewritten under the reconciler                                                                                |
| No `observedGeneration`                              | Nothing distinguishes a status from the spec it was computed from                                                                        |
| No `phaseDeadline`                                   | A phase has no bound that survives a restart                                                                                             |
| No TTL or auto-cleanup of finished operations        | Terminal Ops objects accumulate as audit records with no retention policy; the `StorageClusterOps` test plan records this as a known gap |
