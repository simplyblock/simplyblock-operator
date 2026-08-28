---
name: reconciler-patterns
description: How a controller in this repository is written — never blocking in Reconcile, multi-step operations as a persisted phase and subPhase driven by atlas-lib/statemachine, write-ahead before a side effect, generation and resourceVersion discipline, mutual exclusion through a lock field, requeue against error, terminal no-op, finalizers, and the events a blocked decision owes. Use when writing or reviewing a reconciler, adding a phase to one, or debugging one that stalls, loops, or repeats a side effect.
---

# Reconciler patterns

23 reconcilers live in `operator/internal/controller`. The rules below are what
they have in common, what the incidents came from when one of them was skipped,
and where the canonical implementation of each already exists.

References:

- `references/state-machines.md` — `phase` and `subPhase`, `atlas-lib/statemachine`,
  deadlines, converting a hand-rolled switch.
- `references/concurrency.md` — generation, resourceVersion, conflicts, locks,
  cache staleness, per-cluster state.
- `scripts/check-reconcilers.py` — checks the invariants below that can be
  checked, and follows the call graph out of `Reconcile` to do it:

  ```bash
  .claude/skills/reconciler-patterns/scripts/check-reconcilers.py --changed
  .claude/skills/reconciler-patterns/scripts/check-reconcilers.py --graph Reconcile
  ```

  `scripts/testdata/violations.go.txt` violates every rule once, so a rule that
  stops firing is visible: running the checker over it must report five errors
  and two warnings.

## The invariants

### 1. Never block in `Reconcile`

No `time.Sleep`, no polling loop, no waiting on a channel or a Job to finish.
Compute what is true now, write it, and return, either with an error or with a
`RequeueAfter`.

**A `time.Sleep` grep is not how this is verified, and believing otherwise is how
the current violation survived.** There is no `time.Sleep` in any controller, and
`StorageNodeSetReconciler.Reconcile` still blocks for two minutes: it calls
`maybeActivateCluster`, which waits on `<-time.After` through the
`waitForNodeOnlineSleepFn` variable and then blocks again in
`utils.WaitForClusterActive`. Waiting spelled as a channel receive, a poll, or a
`WaitGroup` is the same stalled worker. `scripts/check-reconcilers.py` follows the
calls and reports both paths; a grep reports neither.

Blocking is not a slow reconcile, it is a stalled controller: a worker holds its
key while it sleeps, and the default concurrency is one, so every other object of
that kind waits behind it. A sleeping reconcile also survives no restart — the
process is gone and the wait never happened, which is how "the operator was up
but nothing progressed" outages start.

Waiting is expressed as **state plus a requeue**. Waiting on a Job is
`.Owns(&batchv1.Job{})`, so its terminal event wakes the reconcile instead of a
poll.

### 2. A multi-step operation lives in the resource, not in memory

`status.phase` for the operation, `status.subPhase` for the steps inside a phase.
11 CRDs already carry a `phase`; `StorageNodeOps` carries a typed `subPhase`, and
`StorageNode.status.actionStatus.subPhase` carries an untyped one — **new fields
are typed** (`type FooPhase string` with the constants next to it), because an
untyped phase makes an impossible value a compile-time non-event.

The process holds no memory between passes and may not be the same process. Every
decision a later pass needs has to be readable from the resource: which step is
active, what was already triggered, when the step must be given up on.

### 3. Declare the state graph; do not switch on strings

`atlas-lib/statemachine` exists for this and has no adoption yet. It takes the
CRD's own phase type as its state type, validates every transition against a
declared graph, carries a per-state deadline, and round-trips through
`Snapshot`/`Restore` so the phase survives a restart. Its package documentation
carries a complete worked reconciler — read it before writing a new machine:

```bash
go doc github.com/simplyblock/atlas/statemachine
```

New multi-step controllers use it. `storagenodeops_controller.go:292` is the
hand-rolled `switch ops.Status.SubPhase` to convert first, and
`references/state-machines.md` says how.

An illegal transition should be an error, not a silent status write. That is what
a declared graph buys: `Pending → Completed` fails at `TransitionTo` instead of
skipping the work in between.

**An Ops controller declares one graph per action**, with
`statemachine.MultiConfig[subPhase]` keyed by `statemachine.Action`. An Ops kind
has one `subPhase` field whose enum is the union of every action's steps, so a
single graph cannot express that `Promoting` belongs to `migrate` and never to
`remove` — a per-action graph can, and makes the wrong one an
`IllegalTransitionError`. See `references/state-machines.md`.

### 4. Write ahead of the side effect

Before a call that changes something outside the cluster, persist the intent —
`status.triggered = true`, the sub-phase, the ID being acted on — then make the
call. A reconcile that dies between the two must find, on the next pass, that the
call may already have happened.

Every retried mutating call has to be idempotent, or guarded by a read that tells
whether it landed. `drainSuspend` reads the node's backend status before POSTing
suspend, which is the pattern: **ask, then act**.

Canonical: `storagenodeops_controller.go:204`,
`storageclusterops_noderollingrestart.go:50`.

### 5. Generation and resourceVersion

- **`metadata.generation` bumps only on a spec change.** A status write does not
  retrigger a spec-driven reconcile, which is what makes a status update safe
  inside `Reconcile`.
- **Record `status.observedGeneration`.** No CRD in this repository has the field
  and no reconciler sets it, which means nothing can distinguish "status reflects
  the current spec" from "status is stale, the spec moved." New CRDs carry it;
  set it in the same status write that reports the outcome, and compare it to
  `metadata.generation` before trusting status.
- **`resourceVersion` is optimistic concurrency, not a nuisance.** A conflict
  means someone else wrote the object, so the local copy is stale: re-read,
  recompute, write again — `retry.RetryOnConflict`. There are 122 status writes
  and 4 files that do this. Never re-`Get` and blindly overwrite: that is how a
  concurrently written sub-phase gets reverted.
- **Never write spec from a controller** that also owns status. Spec is the
  user's; status is the controller's.

Details and the per-cluster keying traps: `references/concurrency.md`.

### 6. One operation at a time, through a lock field

An imperative operation takes a lock on its target — `status.activeOpsRef` on the
`StorageCluster` or the pair — and releases it on **every** terminal path,
including the failures. A release only clears a lock it owns.

Canonical: `storageclusterops_controller.go:568` (`releaseClusterLock`),
`replicationops_controller.go:566`.

### 7. Requeue against error

| Situation                                | Return                                                                                            |
|------------------------------------------|---------------------------------------------------------------------------------------------------|
| Work finished, nothing to watch          | `ctrl.Result{}, nil`                                                                              |
| Waiting on something outside the cluster | `ctrl.Result{RequeueAfter: <named constant>}, nil`                                                |
| A transient failure worth retrying       | `ctrl.Result{}, err` — controller-runtime backs off exponentially and logs it                     |
| A permanent failure                      | record it in status, emit an event, return `ctrl.Result{}, nil` — retrying a 400 forever is noise |

Classify with `atlas-lib/errs/class`, which answers exactly that question and is
used in 4 files. Do not decide by feel, and do not return an error for a
condition the operator cannot fix by trying again.

**Name the interval.** `controlPlaneRequeueInterval`, `syncNodeStatusInterval`,
and six others exist; 86 call sites still write `RequeueAfter: 10 * time.Second`
inline. A named constant per controller is the whole ask — it is the only way the
cadence of a controller can be read or tuned.

### 8. Terminal, finalizer, event

- **A terminal phase re-reconciles to nothing.** First thing after the `Get`:
  if the phase is `Succeeded` or `Failed`, return. `storageclusterops_controller.go:84`.
- **`client.IgnoreNotFound(err)` after the `Get`.** A deleted object is not an
  error, and `reconcile_contract_test.go` asserts it.
- **A finalizer is removed on every path**, including the failure paths and the
  path where the object was never provisioned.
- **Every blocked or skipped decision owes an event or a metric.** A reconcile
  that declines to act and says nothing is indistinguishable from one that never
  ran — this is the single most common reason an incident takes hours instead of
  minutes.

## Traps that produced real incidents

- **A liveness check that lists pods through the API server.** A three-second
  API blip made a health probe report the storage plane dead and take three nodes
  offline. A health signal must not depend on the control plane it is reporting on.
- **An offline object that nothing re-probes.** Marking a node offline is a
  decision that needs its own requeue; without one the object stays offline until
  a human notices, which is how a three-second blip became a nine-hour outage.
- **A long operation the controller starts and stops watching.** A ten-minute
  rebalance blocked every migration behind it and the pile-up overlapped later
  cutovers. Anything long-running needs a phase, a deadline, and mutual exclusion
  against the operations it starves.
- **An RPC timeout shorter than the operation.** A 5s read timeout on a 5.01s
  call made the peer unfreeze while the operation actually succeeded, and the
  retry copied nothing. A timeout is part of the protocol: it belongs next to the
  call, sized for the work, not inherited from a default.
- **A per-cluster map keyed by a loop variable.** Cool-down state recorded for
  cluster A read back for cluster B. `references/concurrency.md`.

## Before handing a reconciler back

0. `scripts/check-reconcilers.py --changed` reports no errors, and its warnings
   are resolved or named as pre-existing. It cannot check items 2, 3, 5, 8, or 9
   below — those stay a reading job.
1. No sleep, no blocking wait, no unbounded loop, on any path the call graph
   reaches and not only in the reconcile body.
2. Every step it can be interrupted in is readable from status.
3. Every external call is either idempotent or preceded by a read.
4. Terminal state returns immediately; the finalizer is removed on all paths.
5. The lock, if there is one, is released on all paths, only by its owner.
6. Requeue intervals are named constants; requeue-against-error is deliberate.
7. `observedGeneration` is set, and status writes handle conflict.
8. Every refusal to act emits an event.
9. Tests: a unit test per phase transition, plus the restart case — see the
   `test-scenarios` and `regression-test` skills.
