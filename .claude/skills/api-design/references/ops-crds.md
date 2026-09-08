# The `<Entity>Ops` shape

`design-crd-model.md` §3 argues why the pattern exists and §3.1 fixes the shape.
This is what one looks like. The Go below is the target shape, not a transcript of
the three that exist: `StorageClusterOps`, `StorageNodeOps`, and `ReplicationOps`
each diverge from it, and "What is still missing" at the end says how.

## Spec

```go
type StorageNodeOpsSpec struct {
    // StorageNodeRef names the StorageNode this operation acts on. Immutable.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storageNodeRef is immutable"
    StorageNodeRef string `json:"storageNodeRef"`

    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="action is immutable"
    Action StorageNodeOpsAction `json:"action"`

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
- **The action is a typed, enumerated verb:** a `<Kind>Action` type carrying the
  `Enum` marker, with its constants beside it:

  ```go
  // +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove;migrate
  type StorageNodeOpsAction string
  ```

  The `Enum` turns an unknown action into an admission rejection rather than a
  `Failed` phase found later, and the named type is what lets the controller key a
  `statemachine.MultiConfig` on it without stringly typed plumbing. All three
  existing kinds carry the marker on a bare `string` and predate the type.
- **Per-action parameters are optional, and grouped:** a nested struct when
  there are more than one or two (`Drain *DrainOpsSpec`). A flat spec with ten
  optional fields, nine irrelevant to the chosen action, is unreadable and
  unvalidatable.
- **The whole spec is immutable.** The object is a request. None of the three
  kinds enforces this today: eight doc comments claim it and no CEL
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
    Phase StorageNodeOpsPhase        `json:"phase,omitempty"`
    Step  StorageNodeOpsStepSnapshot `json:"step,omitempty"`

    Message string `json:"message,omitempty"`

    // Triggered records that the side effect of the current step was issued,
    // so a restart does not repeat it.
    Triggered bool `json:"triggered,omitempty"`

    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    VolumesMigrated int `json:"volumesMigrated,omitempty"`
    VolumesPending  int `json:"volumesPending,omitempty"`

    StartedAt   *metav1.Time `json:"startedAt,omitempty"`
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}
```

**`status.step` is the serialized state machine, not a name.** It holds a
`statemachine.Snapshot`, meaning a state and the deadline that state expires at.
Persisting only the state loses the deadline, and a restored machine that never
times out is a stalled operation nothing detects:

```go
// StorageNodeOpsStep is one step of a running node operation. The enum is the
// union of every action's steps; which steps belong to which action is declared
// by the graph rather than by this type.
// +kubebuilder:validation:Enum=Validating;Suspending;Migrating;Verifying;Removing;Preparing;Restarting;Promoting
type StorageNodeOpsStep string

// StorageNodeOpsStepSnapshot is the durable position of the action's machine.
type StorageNodeOpsStepSnapshot struct {
    // +optional
    State StorageNodeOpsStep `json:"state,omitempty"`
    // +optional
    Deadline *metav1.Time `json:"deadline,omitempty"`
}
```

`status.phase` carries no deadline of its own, because a time limit belongs to a
step. `subPhase` is the pre-rename spelling of `status.step` and is a bare string
in the kinds that shipped before the rule.

**Every action is driven by a declared graph**, and an Ops kind with more than one
action declares one per action through `statemachine.MultiConfig[Step]`. The
mechanism, including how the graph is built per reconcile and what
`FromSnapshot` restores, is the `reconciler-patterns` skill's
`references/state-machines.md`. What matters for the API is that `status.step`
exists, is the snapshot object, and is the only place the machine's position
lives.

`status.triggered` is not part of §3.1 and its future is unsettled: a step machine
records which state an operation is in, not whether that state's side effect
already fired, and `design-storagecluster.md` §13, Q7 is the open question. Keep
the flag until it is answered.

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
time" enforceable rather than hoped for. See `reconciler-patterns` §6.

## Printcolumns

Six, in this order, on all three kinds: target, action, phase, step, message,
age. The step column reads `.status.step.state`. An operator watching a drain
should need `kubectl get snops` and nothing else.

## Adding an action to an existing kind

1. **Add the verb to the `Enum`** on `action`. Adding an enum value is
   backward-compatible, and removing one is not.
2. **Add its parameters** as an optional nested struct, with a CEL rule tying
   them to the action where possible.
3. **Add the step constants** the action needs, to the `<Kind>Step` enum.
4. **Declare the action's graph** in the kind's `statemachine.MultiConfig`, or
   declare none if it genuinely runs in one step. An unknown action is
   `ErrUnknownAction` and fails the operation terminally. Admission catches them
   first, but an older CRD in a cluster may not have the new enum.
5. **Regenerate and sync:** `make -C operator manifests generate`, then
   `make helm-sync`.
6. **Design document and test plan** in the same change: the action's happy path,
   its rejection cases, and the restart case per phase.

## What is still missing across all three kinds

| Gap                                                             | Consequence                                                                                                                              |
|-----------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| No CEL immutability on target, action, or parameters            | A running operation can be rewritten under the reconciler                                                                                |
| No `observedGeneration`                                         | Nothing distinguishes a status from the spec it was computed from                                                                        |
| No `status.step` on `StorageClusterOps`, and none is a snapshot | A step has no bound that survives a restart, and two actions carry their position in a message field or a per-action block               |
| No state machine backs any action                               | `atlas-lib/statemachine` has no consumer, so no transition is validated and an illegal one is an accepted status write                   |
| `action` is a bare `string` on all three kinds                  | The `Enum` marker validates the value, but nothing gives the action a type to key a `MultiConfig` on                                     |
| No TTL or auto-cleanup of finished operations                   | Terminal Ops objects accumulate as audit records with no retention policy. The `StorageClusterOps` test plan records this as a known gap |
