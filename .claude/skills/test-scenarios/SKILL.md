---
name: test-scenarios
description: Enumerate an exhaustive, ID'd test scenario matrix for a feature, CRD, controller, or subsystem: systematically paired positive and negative cases, expanded across the coverage axes that actually break this product (single- vs multi-namespace, single-node / three-node / larger clusters, single- vs multi-cluster and cross-cluster). Use when asked for test scenarios, test cases, a coverage matrix, or "what could go wrong with X," and as the enumeration step of the design-doc skill's test plan.
---

# Test Scenario Enumeration

Produces the scenario matrix that goes into a test plan: numbered rows, each with
a `Type`, grouped per unit under test. Two rules define the output:

1. **Positives and negatives are paired, not sampled.** Every positive behavior
   gets its negative siblings derived mechanically. See
   `references/negatives.md`. A group of positive rows with no negatives is an
   unfinished group, not a short one.
2. **Scenarios are expanded across coverage axes, not written for one happy
   topology.** Cluster size, namespace scope, and cluster count change which code
   paths execute and are where this product actually breaks. See
   `references/axes.md`.

Table format, ID scheme (`U-`/`I-`/`E-`/`M-nn`), and `Type` vocabulary
(`Positive` / `Negative` / `Boundary` / `Regression`) are defined once, in
`../design-doc/references/conventions.md` § "Test scenario matrices." Read it
before emitting a matrix, and do not restate the format here. Scenario wording
follows the `house-style` skill: American English, the Oxford comma, and the
`simplyblock` brand spelling apply to a table cell as much as to a paragraph.

## Scope: this repository's components only

Scenarios cover what this repository builds and deploys: the Kubernetes API
surface, the operator's reconcilers and webhooks, the CSI driver, and the Helm
chart's behavior. **The control plane (`sbcli`), SPDK, and every other repository
are dependencies, not subjects.** They have their own suites, and a scenario
written here against their internals cannot be run from this repository, cannot
be kept honest, and quietly claims coverage that nobody owns.

That boundary is a reframing, not a blind spot. A backend behavior that matters
becomes a scenario **at the boundary**, asserted against a mock control plane:

| Not a scenario here                                     | The scenario here                                                                                                                                         |
|---------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `bdev_lvol_create` passes `persist_reservation` through | The CSI driver sends `"pr": true` in the create body, and surfaces a clean error when the endpoint rejects it                                             |
| The group snapshot is atomic across members             | The operator issues one group call for all `n` members, and fails the whole request rather than committing a partial group when the call returns an error |
| A node reports `nfs_capable` correctly                  | The reconciler marks a node ineligible when the field is absent, `false`, or the endpoint 404s                                                            |
| SPDK holds a freeze for the length of a copy            | The reconciler issues exactly one freeze-inducing call per migration, and reports the failure when the call times out                                     |

The pattern is the same each time: the dependency's behavior becomes an input,
and what is asserted is this repository's response to it, including the
responses to a 4xx, a 5xx, a timeout, a missing field, and an endpoint that does
not exist yet.

An unimplemented backend capability is a **design dependency**, recorded in the
design document's Backend API Requirements table as an external blocker, not a
row in the test plan.

## Workflow

### 1. Establish the surface

Determine, from the design doc and the code, before enumerating anything:

- **The boundary.** Which decisions this repository makes, and which it only
  reacts to. Everything on the far side of the control-plane API is a dependency
  whose responses are inputs (see Scope above).
- **The behaviors.** One per decision the feature makes: each becomes a group
  of rows. Take them from the design's numbered sections so every group can cite
  `(§n.m)`. Grep the controller for the branch points the design does not mention.
- **The scope of each object involved.** Cluster-scoped or namespaced? Which
  references cross a namespace boundary? Which cross a `StorageCluster`
  boundary? Which cross a Kubernetes-cluster boundary?
- **The decision inputs.** Node lists, latency or capacity metrics, annotations,
  spec fields, control-plane responses. Every input is a source of negatives and
  boundaries.
- **The side effects.** Control-plane calls, CR creation, status writes, events,
  finalizers. Every side effect needs both an "it happened" and an "it must not
  happen" assertion somewhere in the matrix.

### 2. Select the coverage axes

Walk the applicability tests in `references/axes.md` and decide which axes apply.
**State the selected axes and the excluded ones with reasons** at the top of the
output, because an axis silently dropped reads as covered.

These three are presumed to apply unless the feature demonstrably cannot see
them:

- **Cluster topology:** single-node, three-node, larger (5+). A single-node
  cluster is where "pick another node" logic has no answer, three nodes is the
  FTT=1 minimum where one failure puts the cluster at its limit, and 5+ is where
  distribution, budget caps, and concurrent operations become observable.
- **Namespace scope:** single-namespace and multi-namespace. Multi-namespace is
  where name generation collides, watchers cross-wire, and tenant isolation
  breaks.
- **Cluster count:** one `StorageCluster`, several in one Kubernetes cluster,
  and for cross-cluster features two Kubernetes clusters with a real peer.

### 3. Enumerate positives per behavior

For each behavior, the documented outcome under valid input, at the base
topology first (the smallest cluster where the behavior is meaningful). State
the observable, never the implementation: a status field, a phase transition, an
event reason, an endpoint call, a returned selection, a checksum.

### 4. Derive the negatives

Apply every applicable mutation from `references/negatives.md` to each positive:
absent precondition, invalid or unset input, missing referent, RBAC denial,
control-plane 4xx / 5xx / timeout, concurrent conflict, cancellation mid-flight,
retry idempotency, resource exhaustion, degraded topology, stale state, and the
required no-op. Then the boundaries: exactly at the threshold, one ε either side,
empty, single element, zero, negative, and the clamped maximum.

Assert absence explicitly. "Nothing should happen" is a `Negative` row only if it
says how absence is proven: mock call count zero, no event emitted, object
unchanged.

### 5. Expand across the axes

Do not take the cartesian product. Cover in this order:

1. **Each-value coverage:** every value of every selected axis appears in at
   least one scenario. This is mandatory.
2. **Interaction pairing:** pair two axes explicitly when both feed the same
   decision: node count × pinned volumes (no eligible target), namespace count ×
   generated names (collision), cluster count × per-cluster state (the
   loop-variable capture class of bug), topology × failure domains (FTT
   headroom). Identify these by asking which axes reach the same function.
3. **Everything else is a declared gap:** list the untested combinations in the
   gap table with a reason. A capped matrix that says where it stopped is honest,
   one that stops silently claims coverage it does not have.

### 6. Emit

- The scenario groups, in the test-plan matrix format, IDs running continuously
  per class.
- An **axis coverage table**: one row per axis, the values, and the IDs covering
  each, so the expansion is auditable rather than asserted.
- The gap table: uncovered rows (`Test` = `—`) and untested axis combinations,
  each with a reason.

### 7. Self-audit before returning

Do not present a matrix that fails any of these:

- Every behavior group has both positive and negative rows.
- At least one `Boundary` row exists per behavior that compares, counts, or
  selects.
- Every selected axis value appears in ≥1 ID, and every excluded axis has a stated
  reason.
- Single-node and three-node cluster rows both exist for anything that selects,
  migrates, or drains.
- Multi-namespace rows exist for anything that generates names from user input or
  watches namespaced objects.
- Every group cites a design section, or flags that it tests undocumented
  behavior.
- Every row is falsifiable: someone could run it and get a clear pass or fail.
- Nothing is listed as covered by a test function whose name was not grepped.
- No scenario asserts the behavior of `sbcli`, SPDK, or another repository. Each
  one is runnable from this repository.

## When invoked from the design-doc skill

`design-doc` step 4 delegates the enumeration here. Return the matrix sections
and the axis coverage table ready to paste into the test plan, using the IDs
already in use in that plan (continue the numbering, never renumber). Do not
write the design doc's Testing Strategy section, which stays a pointer.
