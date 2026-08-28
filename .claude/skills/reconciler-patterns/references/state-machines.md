# Multi-step operations

## The two levels

| Field             | Holds                        | Example                                                          |
|-------------------|------------------------------|------------------------------------------------------------------|
| `status.phase`    | The operation's own progress | `Pending`, `Running`, `Succeeded`, `Failed`                      |
| `status.subPhase` | The steps inside a phase     | `Validating`, `Suspending`, `Migrating`, `Verifying`, `Removing` |

A phase is what a user asks about, and a sub-phase is how the controller gets there.
Drain-remove is the shape: one `Running` phase, five sub-phases, each a single
reconcile pass that returns.

Both are **typed**, with their constants next to the type:

```go
// StorageNodeOpsSubPhase is the step a running node operation is on.
type StorageNodeOpsSubPhase string

const (
    StorageNodeOpsSubPhasePreparing  StorageNodeOpsSubPhase = "Preparing"
    StorageNodeOpsSubPhaseMigrating  StorageNodeOpsSubPhase = "Migrating"
    // …
)
```

`StorageNode.status.actionStatus.subPhase` is a bare `string` and predates the
rule. Do not copy it: an untyped phase turns an impossible value into a runtime
surprise, and a typed one makes the state graph below expressible.

Add `+kubebuilder:printcolumn` for both, so `kubectl get` shows where an
operation is without a `describe`.

## The state machine

`atlas-lib/statemachine` takes the CRD's phase type as its state type. The graph
is a map literal, so a duplicated state is a compile error and an edge to an
undeclared state fails at `New` rather than on the unhappy path.

```bash
go doc github.com/simplyblock/atlas/statemachine   # the complete worked example
```

What it gives a reconciler, and why each part matters:

| Call                              | Use                                                                                                                                                                                                     |
|-----------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Config{Initial, States}`         | The graph, declared once. `To: []phase{…}` per state, and a terminal state declares no exits                                                                                                            |
| `StateDef.OnEnter`                | The side effects of entering the phase, returning how long the phase may last                                                                                                                           |
| `NewFromSnapshot(ctx, cfg, snap)` | Build the machine from `status.phase` and `status.phaseDeadline` at the top of the reconcile. **Runs no entry hooks,** because the transition already happened and the backend call must not fire twice |
| `IsTerminal()`                    | The terminal no-op, without a `switch` listing the terminal phases                                                                                                                                      |
| `TimeoutReached()`                | The phase ran out of time, possibly while the operator was down, because the deadline came back from the resource and not from a live timer                                                             |
| `TransitionTo(ctx, to)`           | The transition, validated against the graph                                                                                                                                                             |
| `Snapshot()`                      | What to write back into status: the state and its deadline                                                                                                                                              |
| `RequeueAfter()`                  | The requeue value, bounded by the phase deadline. `false` when there is no deadline **or it already passed.** An expired phase needs handling now, not another requeue                                  |
| `Close()`                         | Deferred, because the machine owns contexts                                                                                                                                                             |

Two CRD fields carry it: the existing `status.phase`, plus a
`status.phaseDeadline *metav1.Time`. No CRD has the second one yet, and it is what
makes a per-phase timeout survive a restart, and it answers the drain design's
open question about the right action timeout by making the bound per phase
instead of per action.

The machine is built **per reconcile**, not stored on the reconciler: its hooks
close over the object being reconciled, and a `Machine` is not safe for
concurrent use.

`NewFromSnapshot` reads an empty `status.phase` as "nobody has reconciled this
yet" and starts at `Config.Initial`, so the reconcile does not open with an
`if ops.Status.Phase != ""` guard. That rule is about the *empty* value only: an
unrecognized non-empty phase is a downgrade or a hand-edited resource and stays an
error.

## One graph per action

An Ops kind is one CRD serving several operations, and the steps of one are not
the steps of another. `StorageNodeOps` is the case: six actions, one
`status.subPhase` field, and an enum whose values are the union of two disjoint
workflows.

```
remove:  Validating ── Suspending ── Migrating ── Verifying ── Removing
migrate: Preparing  ── Restarting ── Promoting
shutdown, restart, suspend, resume: no sub-phases at all
```

Written as one graph that union is unenforceable, since nothing stops a `remove` op
from reporting `Promoting`. `statemachine.MultiConfig[subPhase]` declares them
together, keyed by `statemachine.Action`, and hands back an ordinary `Machine` for
the action in hand:

```go
sm, err := r.subPhasesFor(ops).FromSnapshot(ctx, statemachine.Action(ops.Spec.Action), snap)
```

Four things this settles, each of which a `switch` gets wrong:

- **The action is not machine state.** `spec.action` is immutable, so it is
  re-read from the resource every pass. Nothing about it goes in `status`, and the
  snapshot stays one state and one deadline.
- **Every graph is validated, not just the selected one.** A `switch` only
  validates the branch it takes, so a bad edge in the `migrate` graph waits for
  someone to migrate. Here, constructing a machine for *any* action proves all of
  them closed.
- **An unknown action is an error, not a missing `default`.** `ErrUnknownAction`,
  because `spec.action` survives a downgrade, where a `switch` with no `default`
  silently stalls the operation.
- **An action with no steps declares no graph.** `MultiConfig` is a map, so ask it
  rather than reading the error:

  ```go
  if _, ok := subPhases[action]; !ok {
      return ctrl.Result{}, nil // shutdown and resume run in a single step
  }
  ```

The **outer `phase` stays a plain `Config`:** `Pending → Running →
Succeeded/Failed` is identical for all six actions. Folding it into each action's
map would copy that spine six times and a fix would land in one of them. Such a
controller runs two machines: one for the phase, one for the sub-phase.

There is no `Must` on `MultiConfig`, deliberately: a malformed graph is a program
bug worth panicking on, but an unrecognized `spec.action` is user input, and
panicking a controller on it is not.

## Converting a hand-rolled switch

`storagenodeops_controller.go:292` is the current shape:

```go
switch ops.Status.SubPhase {
case StorageNodeOpsSubPhasePreparing:  return r.prepare(ctx, …)
case StorageNodeOpsSubPhaseMigrating:  return r.migrate(ctx, …)
// …
}
```

It works, and it has three gaps the machine closes: nothing declares which
transitions are legal, nothing bounds how long a step may take, and the resume
path cannot tell entering a step from still being in it. Convert in this order:

1. **Write the graph** from the existing cases, with the edges the code actually
   takes. Any edge that surprises you is either a bug or an undocumented
   behavior, so resolve it before continuing. Where the switch also branches on
   `spec.action`, that is one graph per action: a `MultiConfig`, not one graph
   with every action's steps in it.
2. **Move each `case` body into an `OnEnter` hook**, returning the step's bound.
   Keep the polling that does not transition *outside* the hook: most passes of a
   `Migrating` step only read the backend and update counters, and re-entering
   the state would repeat its side effect.
3. **Add `status.phaseDeadline`**, regenerate the CRD (`make -C operator manifests`,
   then `make helm-sync`).
4. **Build from the snapshot, check terminal, check timeout, advance, snapshot,
   write, requeue**, in that order, which is the shape in the package
   documentation.
5. **A test per transition**, plus one per interrupted step: restore from that
   phase and assert the hook did not run again. See `test-scenarios`.

## Not every reconcile is a transition

The common case for a long-running phase is: read the backend, update counters,
write status, requeue. The machine moves only when the backend reports something
new. Calling `TransitionTo` with the current state is not a refresh, because a self-edge
would re-run the entry hook and repeat its side effect, which is why the worked
example declares no self-edges.
