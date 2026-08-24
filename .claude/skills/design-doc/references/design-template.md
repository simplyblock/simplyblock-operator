# Design Doc Template

Copy the skeleton, keep the section order, drop the sections the design does not
need, renumber accordingly. Bracketed `<…>` text is a placeholder; the
parenthesised italics under each heading say what belongs there and are not part
of the output.

Line breaks inside the metadata block are hard breaks — end each of those lines
with two spaces.

---

```markdown
# Design Document: <Feature Name>

**Status:** Draft  
**Author:** <Name (handle)>  
**Date:** <YYYY-MM-DD>  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/<N>  
**Test Plan:** [`tests/test-plan-<slug>.md`](../tests/test-plan-<slug>.md)

---

## Phasing Overview

<!-- Only for staged work. One row per phase; keep the columns that discriminate
     the phases (signal used, algorithm, scope), plus Status. Follow the table
     with one paragraph on what makes each phase independently shippable. -->

| Phase | Status | <Discriminator> | <Discriminator> |
|---|---|---|---|
| **Phase 1** (§5.1–§5.3) | **Implemented** | … | … |
| **Phase 2** (§5.4–§5.5) | Planned | … | … |

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Data Model Changes](#4-data-model-changes)
…

---

## Overview

<!-- Optional but strongly preferred for anything with more than one moving part.
     Two to five paragraphs: what this is, the core idea, and how the pieces
     resolve against each other. A decision table (tier/precedence/priority)
     belongs here. A reader who stops after this section should be able to
     describe the feature correctly. -->

---

## 1. Background

<!-- What the system does today, mechanism named concretely, and why it is
     insufficient. Name the existing primitives the design will reuse or
     replace (controllers, CRs, status fields, endpoints). No solution here. -->

---

## 2. Goals and Non-Goals

### Goals

<!-- Bullets, each independently verifiable, each traceable to a test plan
     scenario. Include the operational goals (observability, restart safety,
     no-oscillation, idempotency), not only the feature goals. -->

### Non-Goals

<!-- Bullets. This is the load-bearing half: adjacent problems this design
     deliberately does not solve, and where each is handled instead. -->

---

## 3. Architecture Overview

<!-- ASCII diagram: control-plane box, one inner box per controller with its
     numbered responsibilities, the CRs and the spec/status paths it reads and
     writes, then the backend API box listing the exact endpoints used.
     Follow with bolded prose notes for what the diagram cannot show
     (delegation boundaries, watch relationships, auth model, key revisions
     from an earlier version of the design). -->

---

## 4. Data Model Changes

<!-- Or "## 4. API Design — New CRDs" when the design introduces CRDs.
     One sub-section per type: annotated Go struct with kubebuilder markers and
     the real doc comments, then a YAML example, then the invariants
     (immutability, defaults, validation, ownership, finalizers).
     State explicitly when nothing else changes: "No other CRD changes." -->

### 4.1 <CRD> Spec — `<TypeName>` (new)

### 4.2 <CRD> Status — `<TypeName>` (new)

---

## 5. <Core Mechanism>

<!-- The heart of the design: the algorithm, the placement tiers, the
     measurement and weighting model, the annotation protocol. Numbered
     sub-sections. For an algorithm use `### Step N — <name>` prose steps
     followed by an `### Algorithm Summary` pseudocode block. Give the
     formulas, the units, and the defaults. -->

---

## 6. State Machine

<!-- Vertical ASCII flow of phases/sub-phases with the transition condition on
     the edge and the action taken as a trailing `←` note. Say where the state
     is persisted (`status.…`) and assert the reconcile discipline — no blocking
     loops, always requeue. Then the failure and cancellation table:
     | Condition | Sub-phase | Result |. Cover: user cancels mid-flight,
     operator restart, backend error, timeout. -->

---

## 7. Controller Design

<!-- Per controller: package path and file, reconcile trigger (watches, owned
     resources, poll interval), concurrency and mutual exclusion, interaction
     with the controllers it can race, the write-ahead / `Triggered` pattern
     used for restart safety, and the new RBAC rules verbatim. -->

### 7.1 Location
### 7.2 Reconciliation Trigger
### 7.3 Concurrency and Mutual Exclusion
### 7.4 Interaction with Existing Controllers
### 7.5 RBAC

---

## 8. Backend API Requirements

<!-- | Method | Endpoint | Notes |. Mark which endpoints already exist and which
     the control plane must add — the latter are the design's external blockers.
     State the idempotency requirement for every mutating call the reconciler
     may retry. Document multi-step protocols (create → continue → poll) with
     the request/response bodies and the timeout behaviour. -->

---

## 9. Configuration

<!-- Cluster-level fields (| Field | Type | Default | Description |), then
     per-object annotation overrides (| Annotation | Values | Effect |).
     Say for each whether it is reconfigurable at runtime or immutable after
     creation, and what happens to in-flight work when it changes. -->

---

## 10. Failure Modes and Fallback

<!-- | Failure | Detection | Behaviour |. What happens when the backend is
     unreachable, when a dependency is missing, when metrics are stale, when
     the webhook times out. Every path must degrade to a defined state —
     name it. -->

---

## 11. Observability

### Kubernetes Events

| Event | Type | Reason |
|---|---|---|

### Prometheus Metrics

| Metric | Labels | Description |
|---|---|---|

<!-- Metric names are `simplyblock_<subsystem>_<thing>[_total|_seconds]`.
     Every blocked, failed, or skipped decision the operator makes needs either
     an event or a metric — otherwise it is undebuggable in the field. -->

---

## 12. Testing Strategy

<!-- A pointer, not a catalogue. The numbered scenario matrix lives in the test
     plan and only there — do not restate scenarios here, and do not assign IDs
     here. Three to six lines:
       - which classes apply and what each must prove at the level of a claim
         ("the eligibility filter's five exclusion reasons", "restart safety of
         every sub-phase", "no I/O error under sustained fio during cutover");
       - the harness each class needs (fake client + mock HTTP / `envtest` /
         live cluster with fio) and anything that does not exist yet;
       - where the risk concentrates — the sections whose scenarios must not be
         cut when the schedule slips;
       - the link. -->

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-<slug>.md`](../tests/test-plan-<slug>.md)

- **Unit** — <what must be proven without a cluster>
- **Integration** — <what needs the reconcile loop against `envtest` + mock backend>
- **E2E** — <what needs a live cluster, and the data-path correctness assertion>
- **Load / long-running** — <scale and duration claims, if any>

<!-- If the work is phased, say which classes only become testable in a later
     phase and why (§<n>). -->

---

## 13. Migration Strategy

<!-- Only when the design changes existing behaviour or CRDs: the phases, what
     stays backwards compatible, when the legacy path is removed, and how
     existing objects are converted. Mark completed phases `✅ Complete`. -->

---

## 14. Open Questions

<!-- Table form, preferred whenever answers are owed by other teams: -->

| # | Question | Owner |
|---|---|---|
| 1 | **<Short title>:** <what is undecided and what depends on it> | SPDK/Backend team |
| 2 | ~~**<Short title>**~~ **Resolved.** <the decision that was taken> | Resolved |

<!-- Or prose form: `**Qn: <question>**` followed by a paragraph of context and
     the options. Pick one form per document and keep it.

     Undecided is a legitimate state; pretending otherwise is not. Resolved
     questions are struck through in place — or promoted into the body as
     decisions — never silently deleted. -->
```

---

## Which sections a design actually needs

| Design shape | Load-bearing sections |
|---|---|
| New CRD + controller | Overview, API Design, State Machine, Controller Design, Mutual Exclusion, Observability, Migration Strategy |
| Algorithm / policy change | Overview, Core Mechanism (formulas, weights, defaults), Configuration, Failure Modes, Cool-down or hysteresis |
| Long-running operation (drain, restart, migrate) | State Machine, sub-phase persistence, resume-on-failure, cancellation, Backend API idempotency |
| Placement / scheduling | Tier or precedence table in Overview, per-tier gates, opt-out annotations, Failure Modes and Fallback |
| Cross-component protocol (CSI ↔ operator ↔ control plane) | Architecture diagram with the trust boundary, Backend API Requirements, auth model, version skew |
| Small, self-contained mechanism | H1 + Overview + mechanism + examples + failure mode is enough — `design-dhchap.md` is 129 lines and complete |
