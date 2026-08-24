# Test Plan Template

The test plan is the **only** home for test scenarios. The design doc points
here; nothing about a scenario is restated there. Table style follows
`design-issue-130-auto-rebalancing.md` §15 — numbered rows, a `Type` column,
grouped per unit under test, each group citing the design section it verifies.

Copy the skeleton, drop what does not apply. Bracketed `<…>` is a placeholder;
the HTML comments say what belongs there and are not part of the output.

---

````markdown
# Test Plan: <Feature Name>

Related design: [`designs/design-<slug>.md`](../designs/design-<slug>.md)
Harness: [`<path/to/suite>`](../../../<path/to/suite>)

Scenario IDs are permanent: `U-` unit (no cluster — pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest` + a mock backend), `E-` end-to-end (live cluster, real data path),
`M-` manual (needs failure injection or orchestration not yet automated).
Types are `Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column
names the implementing function, or `—` when the scenario is not yet covered —
every `—` also appears in §8 What Is Not Yet Covered.

---

## 1. Unit Tests

<!-- One sentence on the harness boundary, e.g.: All pure functions in
     `<file>.go` and the controller helper methods are covered without external
     dependencies. Numbering runs continuously across the groups below. -->

### <Unit Under Test> (§<n>.<m>)

File: `<internal/controller/<name>_unit_test.go>`

| # | Scenario | Type | Test |
|---|---|---|---|
| U-01 | `<function>` — <inputs and asserted outcome> | Positive | `TestXxx` |
| U-02 | <the exclusion or error case> | Negative | `TestXxx` |
| U-03 | Threshold boundary: `deviation == threshold` is NOT hot (strict `>`); `threshold + ε` IS | Boundary | `TestYyy` |
| U-04 | <regression: pins the bug from #<issue>> | Regression | — |

### <Named Package or Component> (§<n>)

File: `<internal/<pkg>/<pkg>_test.go>`

<!-- Add a Function column when the rows are organised per function. -->

| # | Function | Scenario | Test |
|---|---|---|---|
| A-01 | `<Function>` | <case; case; case> | `TestFunction` |

---

## 2. Integration Tests

<!-- Harness sentence, e.g.: Run the full controller reconciliation loop against
     a mock backend HTTP server and a real Kubernetes API via `envtest`. -->

### <Capability> (§<n>)

| # | Scenario | Type | Test |
|---|---|---|---|
| I-01 | <trigger> → <status transition, event emitted, endpoint called> | Positive | `TestZzz` |
| I-02 | <error injected> → <requeue, no state advance> | Negative | — |

---

## 3. E2E Tests

<!-- Harness sentence, e.g.: Run against a real SimplyBlock cluster with real
     fio workloads. Anything touching the data path needs a row that asserts
     I/O *correctness* (checksums, fio verify mode), not merely that I/O
     continued. Destructive rows say so. -->

### <Operation> (§<n>)

| # | Scenario | Type | Test |
|---|---|---|---|
| E-01 | <live-cluster action> → <observable outcome within N cycles> | Positive | `<suite/case>` |
| E-02 | Continuous I/O during <operation> → no errors, checksums intact | Positive | — |
| E-03 | Operator restart mid-<operation> → <invariant preserved> | Negative | — |

---

## 4. <Class> — Phase <N> (Planned)

<!-- Scenarios that only become testable once later work lands. No Type and no
     Test column — both are decided when the phase is scoped. -->

| # | Scenario |
|---|---|
| U-P2-01 | <case> |

---

## 5. Manual Scenarios and Test Concepts

<!-- The hand-off section: scenarios that need failure injection or orchestration
     we cannot automate yet. Each block is self-contained enough to implement
     without reading the design doc. Bold lead-ins as needed. -->

### M-01 — <Scenario title, phrased as the situation — "The suspended node fails mid-drain">

**Design reference:** §<n>.<m>

**What to verify:** <the invariant that must hold, in behavioural terms>

**Current behaviour:** <what the code does today, if it differs or is unprotected>

**Open question:** <a decision the test cannot be written without>

**Test concept:**
1. <setup>
2. <the injection or action>
3. <the assertion, as an observable — status field, event, CR count, I/O result>

**Recommended fix:**
```go
// only when the scenario exposes a real gap in the implementation
```

---

## 6. Axis Coverage

<!-- Which topologies the matrix above actually exercises — produced by the
     `test-scenarios` skill. This is what makes "exhaustive" checkable instead of
     claimed: an axis value with no IDs is a gap, not an omission. -->

| Axis | Values covered | IDs | Not covered |
|---|---|---|---|
| Cluster topology | 1 node, 3 nodes, 5+ nodes | U-08, I-05, E-09 | asymmetric node sizes |
| Namespace scope | single, multi (shared StorageClass) | U-16, I-07 | namespace deletion mid-operation |
| Cluster count | 1 StorageCluster, 2 in one k8s cluster | A-06, E-10 | cross-cluster — feature is single-cluster (§<n>) |
| Object scale | 0, 1, 100+ | U-12, U-25, E-11 | — |
| Lifecycle / restart | mid-phase restart, terminal re-reconcile | I-08, I-11 | control-plane restart |

---

## 7. Coverage Summary

<!-- Optional below ~20 scenarios, expected above. One row per class. -->

| Class | Scenarios | Covered | Not covered |
|---|---|---|---|
| Unit | 31 | 28 | U-19, U-24, U-30 |
| Integration | 12 | 0 | I-01 … I-12 |
| E2E | 10 | 6 | E-07, E-08, E-09, E-10 |

---

## 8. What Is Not Yet Covered

<!-- Every `—` from the matrices above, with the reason it is not covered. -->

<!-- Every `—` from the matrices, plus every "Not covered" cell from §6. -->

| # | Gap | Reason |
|---|---|---|
| U-30 | <scenario> | <feature not implemented / API contract undefined / needs failure injection we lack> |
| — | <axis combination from §6, e.g. asymmetric node sizes> | <reason> |
````

---

## Scenario checklist

Walk this list against the design before declaring the plan complete. Each item
becomes a numbered row with a `Type`, or a row in "What Is Not Yet Covered."
Nothing on this list may simply go unmentioned — most `Boundary` and `Negative`
rows come from here. The `test-scenarios` skill's `references/axes.md` and
`references/negatives.md` are the long form of this list; use them when the
matrix needs to be exhaustive rather than adequate.

**Per state / sub-phase**

- Happy path advances to the next state.
- Terminal state re-reconcile is a no-op (`Succeeded`, `Failed`).
- Operator restart in this state resumes correctly and does not duplicate the
  side effect (the `Triggered` / write-ahead guard).
- User cancels or clears the action in this state — is the target restored?

**Control-plane interaction**

- Mutating call returns 4xx (403, 404, 409) → requeue vs fail, and which.
- Mutating call returns 5xx → retried, no state advance.
- Connection refused / timeout mid-call → no lost or duplicated operation.
- Retried call is idempotent (assert the mock's call count).
- 404 on a delete/remove treated as success.

**Concurrency**

- Two operations targeting the same object — second blocks, does not interleave.
- Operations on two different objects run independently, no cross-locking.
- Spec mutated mid-operation (target changed under a running action).
- Stale informer cache does not trigger a spurious re-run.

**Data correctness**

- Generated resource names are valid DNS labels, ≤63 chars, collision-free on
  long inputs.
- Filtering rules (system volumes, pinned objects, unmanaged objects) exclude
  and surface exactly what the design says.
- Defaults applied when optional fields are unset; immutable fields rejected.

**Kubernetes surface**

- Events emitted with the reasons the design's Observability table promises.
- Status conditions and phases match the state machine.
- RBAC is sufficient — the controller does not fail on a forbidden verb.
- Finalizer removed on every terminal path, including the failure paths.

**Boundaries** — the rows most often missing

- Exactly at a threshold (`==`), and one ε either side.
- Empty collection, single element, two elements.
- Zero, negative, and unset numeric inputs.
- Clamped maximum (`k > len(items)`), and `k = 0`.

**Under load and over time**

- Sustained fio workload across the operation: no I/O errors and verified data
  integrity — say how correctness is checked, not just that I/O continued.
- Scale: many objects (100+ volumes/CRs) — no limits hit, note time-to-complete.
- Idle cost: reconcile churn and API request rate when nothing is happening.
