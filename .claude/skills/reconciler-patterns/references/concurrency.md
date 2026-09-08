# Generation, resourceVersion, and concurrency

## The three version fields, and what each answers

| Field                       | Written by                                | Answers                                  |
|-----------------------------|-------------------------------------------|------------------------------------------|
| `metadata.generation`       | the API server, on a **spec** change only | Has the user asked for something new?    |
| `status.observedGeneration` | the controller                            | Which spec did this status come from?    |
| `metadata.resourceVersion`  | the API server, on **every** write        | Is my copy of this object still current? |

### `metadata.generation`

It bumps on a spec write and not on a status write, which is what makes it safe
for a reconciler to update status inside `Reconcile`, because the update does not
retrigger a spec-driven pass. It also means a spec-change watch is the reliable
trigger for a change of intent, while status churn is not.

### `status.observedGeneration`

**No CRD in this repository declares it and no reconciler sets it.** That gap is
why nothing can currently tell a fresh status from a stale one: a `phase: Running`
may describe the spec that is there now, or the spec from before the user edited
it. New CRDs carry the field:

```go
// ObservedGeneration is the .metadata.generation this status was computed from.
// A status whose observedGeneration is behind .metadata.generation describes a
// spec that has since changed.
// +optional
ObservedGeneration int64 `json:"observedGeneration,omitempty"`
```

Set it in the same status write that reports the outcome, and read it before
trusting status, in the reconciler, in a webhook, and in a test.

Where a spec change must interrupt a running operation, the comparison is the
trigger for that decision. Where it must not, the running operation's own copy of
what it is acting on (`status.actionStatus.nodeUUID`, the ops CR's `spec`) is the
authority, and a mismatch is a detected mid-flight spec change. The drain design
carries exactly this case.

### `metadata.resourceVersion`

It is opaque and only ever compared for equality. A write with a stale
`resourceVersion` fails with a conflict, which is optimistic concurrency doing
its job: **something else changed the object, so the local copy is wrong.**

```go
err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
    var fresh v1alpha1.StorageNodeOps
    if err := r.Get(ctx, key, &fresh); err != nil {
        return err
    }
    fresh.Status.SubPhase = next          // recompute from the fresh object
    return r.Status().Update(ctx, &fresh)
})
```

Two rules follow, and there are 122 status writes in this repository against 4
files that observe them:

- **Recompute inside the retry.** Re-`Get` and then applying the *old* value is
  the bug the retry was supposed to prevent: it reverts whatever the other writer
  did.
- **Never clear `resourceVersion` to force a write.** It turns a detected
  conflict into a silent last-write-wins, and the write it loses is usually
  another controller's sub-phase.

Prefer a status **patch** over a read-modify-write when the change is a single
field and the object is large: fewer conflicts, and no chance of carrying a stale
neighbor field along.

## The informer cache is behind

`r.Get` reads a cache, not the API server. Two consequences:

- **A write is not immediately visible to the next read.** A reconcile that
  writes and then re-reads may see the old object. Carry the value forward in
  memory instead of re-reading it.
- **A stale cache can retrigger work that already finished.** This is why the
  terminal-state check comes first and why the write-ahead flag exists: the
  guard, not the cache, decides whether the side effect already happened. The
  existing unit tests name this case (`TestDrainCancellationSkipsWhenActionStillActive`).

## Mutual exclusion between operations

An imperative operation holds a lock field on its target for its whole life:
`StorageCluster.status.activeOpsRef`, and the pair and policy equivalents in
`ReplicationOps`. The rules:

- **Acquire before the first side effect**, and treat a lock held by someone else
  as a requeue, not an error.
- **Release on every terminal path:** success, failure, and the panic-adjacent
  paths a `defer` covers. A leaked lock blocks every later operation on that
  target and looks exactly like a hung controller.
- **Only the owner releases.** `releaseClusterLock` compares the reference before
  clearing it, or a slow loser clears the winner's lock.
- **A lock is not a queue.** A blocked operation stays `Pending` and requeues,
  nothing preserves submission order, and nothing should pretend to.

## Per-object state on the reconciler

State cached on the reconciler struct (a cool-down map, a last-seen timestamp,
or a per-cluster counter) needs the key that makes it unambiguous, and a closure
that captures it must capture the *value*:

```go
for _, cluster := range clusters {
    isCoolingDown := func(vol string) bool {
        return r.coolDown[cluster.UUID][vol].After(time.Now())   // ← cluster is the loop variable
    }
    …
}
```

Under Go 1.22 and later the loop variable is per-iteration, so this is no longer
the classic capture bug. The *keying* still is: a map indexed by volume
alone, or a counter without a cluster label, reads back another cluster's state.
`copyloopvar` in the lint config catches the old shape, and nothing catches the
missing key, which is why the autobalancing tests assert it explicitly
(`A-06` in the rebalancing plan).

Anything a second CPU or goroutine touches needs `-race` in the test that covers
it. The operator suite does not run with `-race` by default. See the
`regression-test` skill.
