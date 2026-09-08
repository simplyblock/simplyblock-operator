# Deriving Negatives

A positive scenario is one row. The negatives around it are mechanical, so work the
mutations below against each positive rather than trying to imagine failures.
Every behavior group ends up with more negatives than positives. That ratio is
normal and is what makes a matrix exhaustive.

---

## The mutation set

For each positive scenario, ask each question. If the answer is "the code would
do something," it is a row.

### Preconditions

| Mutation                                                       | Row asserts                                                             |
|----------------------------------------------------------------|-------------------------------------------------------------------------|
| The precondition is absent                                     | The feature declines to act, with a stated reason                       |
| The precondition is *just* unmet (threshold, quota, count)     | `Boundary`: strict vs inclusive comparison, verified in both directions |
| The precondition was met, then stopped being met mid-operation | Detected and handled, not carried on stale                              |
| The gating flag is disabled                                    | The whole path is inert, nothing is written, and no call is made        |
| The gating flag is toggled *during* an operation               | Defined behavior: finish or abandon, but not corrupt                    |

### Inputs

| Mutation                                            | Row asserts                                                     |
|-----------------------------------------------------|-----------------------------------------------------------------|
| Required field unset                                | Rejected with a clear message, at admission if a webhook exists |
| Field set to zero, negative, or the empty string    | Rejected or defaulted, and the row says which                   |
| Field at its maximum, and one past it               | `Boundary`, including clamping behavior                         |
| Malformed value (bad regex, bad UUID, bad quantity) | Error surfaced, not a panic                                     |
| Immutable field mutated after creation              | Rejected, and the running operation is unaffected               |
| Unknown enum value / unknown action                 | Immediate terminal failure with the reason                      |
| Input list empty                                    | Empty result, no error                                          |
| Two inputs that conflict (annotation vs spec field) | The documented precedence, asserted in both orders              |

### Referenced objects

| Mutation                                            | Row asserts                                        |
|-----------------------------------------------------|----------------------------------------------------|
| Referent does not exist                             | Terminal failure with a not-found message          |
| Referent exists in a different namespace            | Rejected, never silently resolved locally          |
| Referent is being deleted (`deletionTimestamp` set) | Classified as the design says, with no half-action |
| Referent is of the wrong kind, or not managed by us | Excluded and surfaced, not skipped silently        |
| Referent's backend counterpart is gone              | Reconciled to a defined state, not retried forever |

### Dependencies (control plane, Prometheus, webhooks)

Every row below asserts **this repository's** response, never the dependency's
behavior. The dependency is faked, and what is under test is the reconciler or the
driver reacting to it.

| Mutation                                                      | Row asserts                                                         |
|---------------------------------------------------------------|---------------------------------------------------------------------|
| 4xx (403, 404, 409)                                           | Requeue vs fail: the design must say which, the row must prove it   |
| 5xx                                                           | Retried, no state advance, no duplicate side effect                 |
| Timeout / connection refused                                  | Same, plus the operation is not marked complete                     |
| Response body malformed or missing fields                     | Error, not a nil dereference                                        |
| Call succeeds but the response says the operation failed      | Failure path taken                                                  |
| The call is retried after an ambiguous timeout                | **Idempotency:** assert the mock's call count, not just the outcome |
| A response arrives after the operator gave up                 | Late response ignored, no double-commit                             |
| Dependency is degraded rather than down (slow, stale metrics) | Stale data detected or bounded, not trusted blindly                 |

### Concurrency

| Mutation                                       | Row asserts                                       |
|------------------------------------------------|---------------------------------------------------|
| A second operation targets the same object     | Blocks or queues, with no interleaving            |
| Operations target two different objects        | Independent, with no cross-locking                |
| The lock holder dies without releasing         | Lock reclaimable, with no permanent deadlock      |
| The spec changes under a running operation     | Detected (`status.…Ref != spec.…`) and handled    |
| A stale informer cache serves an old object    | No spurious re-run, no duplicated side effect     |
| Two controllers act on the same backend object | One loses cleanly, and the design names the order |

### Interruption

| Mutation                                         | Row asserts                                                       |
|--------------------------------------------------|-------------------------------------------------------------------|
| Operator restarts, once per sub-phase            | Resumes from persisted state, and the side effect is not repeated |
| The user cancels mid-operation                   | The target is restored to its pre-operation state                 |
| The CR is deleted mid-operation                  | Finalizer completes the cleanup, and nothing is orphaned          |
| The whole namespace is deleted mid-operation     | Terminal state reached, no orphaned backend objects               |
| A node reboots or a pod is evicted mid-operation | Recovery without manual intervention                              |

### Exhaustion and degradation

| Mutation                                                    | Row asserts                                                      |
|-------------------------------------------------------------|------------------------------------------------------------------|
| No eligible target remains                                  | Clear blocked state with the reason, plus an event or metric     |
| All candidates are excluded (pinned, cooling down, offline) | The exclusion reasons are individually asserted, then together   |
| Capacity or quota full                                      | Rejected before any partial side effect                          |
| The cluster is degraded                                     | The design's stance is proven: proceed or block, not "sometimes" |
| The cluster is at its fault-tolerance limit                 | The redundancy-reducing step is refused                          |

### Required no-ops

The most under-tested class. Whenever the answer is "nothing should happen," the
row must state **how absence is proven**:

- mock HTTP call count is zero for that endpoint,
- no event of that reason was recorded,
- the object's `resourceVersion` / annotation set is unchanged,
- the status field stayed empty rather than being set and cleared.

A "nothing happens" row that only asserts the end state cannot distinguish
"never acted" from "acted and reverted."

---

## Boundaries

Boundary rows are their own `Type` because they catch a different defect class
than negatives. For anything that compares, counts, selects, or windows:

- exactly at the threshold, and one ε either side (strict `>` vs `>=`), asserting
  that the documented one is what happens
- empty collection, one element, two elements
- zero, negative, and unset numerics
- `k = 0`, `k = 1`, `k > len(items)` (clamping)
- first and last element of an ordered selection
- the window edge: inside a cool-down, exactly at expiry, just after
- maximum name length, and one character past it (truncation plus collision
  avoidance).

---

## Data-path scenarios

For anything touching volumes, migration, replication, or attachment, "no I/O
errors" is not an assertion of correctness. Rows must state the verification:

- fio in verify mode, or checksums taken before and compared after
- write-during-operation followed by read-back of the same offsets
- the count of freeze/quiesce windows, when the design bounds it
- confirmation that no stale NVMe path, subsystem, or target remains connected
  after the operation.

A positive data-path row without an integrity assertion should be written as a
gap, not as a pass.

---

## Ratio check

Before returning a matrix, count per behavior group:

- zero negatives → the group is unfinished
- zero boundaries in a group that compares or counts → unfinished
- more positives than negatives overall → almost certainly unfinished
- every side effect appearing only in "it happened" rows and never in a
  "it must not happen" row → unfinished.
