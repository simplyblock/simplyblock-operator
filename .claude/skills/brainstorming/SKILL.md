---
name: brainstorming
description: Explore an idea for this product before any of it is built — deciding which component owns the behavior, whether it is desired state or a one-shot operation, what it would do to a live cluster with data on it, and which of two or three genuinely different approaches to take. Ends by handing a settled decision to the design-doc skill, or by concluding not to build the thing. Use when asked to brainstorm, discuss or explore an idea, weigh approaches, or answer "what if," "how should we," "is it worth," or "can we" — and before writing a design document, a CRD, or a controller.
---

# Brainstorming

Brainstorming produces a shared understanding of what to build and why. It does
not produce files.

## The gate

**Write nothing until the human has agreed what is being built.** No design
document, no CRD, no scaffold, no branch, no `make` target. The temptation in this
repository is specific: `design-doc` is right there, its template is inviting, and
starting it feels like progress. It is the opposite. A design document written
before the shape is settled has to be argued down instead of filled in, and its
section numbers are cited by a work plan the moment it exists.

Reading is not writing. Grep the code, read the neighboring designs, check what
the control plane already does. That is how the conversation gets grounded, and it
is what the first turn should be spent on.

## Three paths

Classify the request. When it is unclear, ask which of these it is rather than
guessing, because the paths end in different places.

| Path              | Looks like                                                                         | Ends in                                             |
|-------------------|------------------------------------------------------------------------------------|-----------------------------------------------------|
| **Spike**         | "can we," "does X work," "is it fast enough," "what does sbcli return for"         | a finding, reported in chat. No document            |
| **Bounded**       | a field on an existing CRD, a flag, one reconciler's behavior, a chart value       | a short design in chat, then the implementing skill |
| **Architectural** | a new CRD, a new controller, a data-path change, a contract between two components | `design-doc`, then `work-plan`                      |

**A path only ever upgrades.** When a bounded change turns out to need a new
status field, a webhook, and a migration, it became architectural the moment that
was discovered, so say so and switch. Nothing downgrades because it is taking
longer than expected.

**"Simple" does not skip the path, it shortens it.** A bounded change still gets
its two-paragraph design and its approval, because the cost of the design is two
paragraphs and the cost of building the wrong bounded thing is a shipped field
that cannot be renamed.

## Three questions before any options

Generic brainstorming jumps to approaches. In this product, three questions decide
whether the approaches are even the right set, and getting one wrong is expensive
in a way that no amount of good design later recovers.

### 1. Which component owns the behavior?

| Owner                       | Owns                                                                                      |
|-----------------------------|-------------------------------------------------------------------------------------------|
| The control plane (`sbcli`) | Volume, pool, and node lifecycle; migration and rebalancing mechanics; the storage engine |
| `atlas-lib`                 | Node-level primitives and the control-plane client, shared by both consumers              |
| The operator                | Kubernetes-shaped logic: CRDs, reconcilers, webhooks, what the cluster should look like   |
| The CSI driver              | The CSI RPC surface and the node's data path                                              |

The most expensive brainstorming mistake here is designing operator behavior for
something the control plane owns. It is also the easiest to make, because the
operator can always *call* the control plane, so any idea can be made to fit. Ask
instead where the behavior belongs when both are working correctly. If the answer
is the control plane, the output of the brainstorm is a control-plane requirement
and possibly an external prerequisite, not an operator design.

### 2. Is it desired state, or a one-shot operation?

Desired state converges and is safe in Git. An operation happens once and is
indistinguishable from never having happened once it is done. "Restart this node"
is not a state. This is the Entity and Ops split, and `api-design` owns the
mechanics, but the decision is made here, because it changes what the thing *is*,
not how it is spelled.

### 3. What would it do to a live cluster with data on it?

Not "how do we roll it out," but what the idea does when it is wrong, mid-flight, on
a cluster serving I/O. The data path has taught this repository that ordering is
load-bearing: freeze counts during a cutover, draining before a transfer,
reconnecting a journal after a node moves. An idea that makes any of those
concurrent, faster, or batched is not a performance idea, it is a correctness
change, and the brainstorm has to treat it as one.

## Traps specific to this product

Ideas that sound right, appeal for a good reason, and are wrong here.

| The idea                                 | Why it appeals                   | What is actually true                                                                                                                                                                                          |
|------------------------------------------|----------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| "This needs its own CRD"                 | it is a distinct concept         | the bar is that something *watches it or references it by name*. Otherwise, it is a field or a nested struct. A CRD costs a controller, RBAC, a chart entry, and a place in the ownership spine — `api-design` |
| "The reconciler can just wait for it"    | the code reads top to bottom     | that is a stalled controller at concurrency one. Waiting is state plus a requeue — `reconciler-patterns`                                                                                                       |
| "Add a helper in the operator"           | it is needed in the operator     | if the CSI driver could need it, it belongs in `atlas-lib`. A second copy is worse than an awkward import — `extract-to-atlas-lib`                                                                             |
| "Make it configurable"                   | it defers the decision           | a shipped field is an API forever. Everything is `v1alpha1` with no conversion webhook, so a rename has no migration path — `api-design`                                                                       |
| "Just an annotation for now"             | annotations feel informal        | an annotation a user sets is as much an API as a spec field, and less validated                                                                                                                                |
| "Parallelize it to make it faster"       | the operations look independent  | on the data path they usually are not. See question 3                                                                                                                                                          |
| "The operator should reconcile that too" | the operator has the credentials | breadth in the operator's role is the ceiling on everything it grants — `rbac-hardening`                                                                                                                       |
| "We can add the tests after"             | the shape is still moving        | for a bug, the failing test comes first, always — `regression-test`                                                                                                                                            |

Naming a trap is not a refusal. It is the thing to say out loud so the idea can be
reshaped into the version that works, which usually exists.

## Exploring options

**Two or three genuinely different approaches, or one and say so.** Three options
where two are strawmen is worse than one honest recommendation, because it dresses
a decision already made as a choice.

For each: what it is good at, what it is bad at, and what it forecloses. Then
recommend one and say why. A recommendation is what the human is asking for. A
menu with no opinion hands the work back.

Two options are easy to forget and belong on the list whenever they are real:

- **Narrow the requirement.** Often the expensive part of an idea serves a case
  nobody asked for.
- **Do not build it.** A brainstorm that ends in "this is not worth its
  complexity, and here is what it would have cost" is a success, and it is
  cheaper here than in any later phase.

## When a picture beats prose

Reach for one when the idea is about a shape: how components relate, how a phase
machine transitions, where a boundary sits. Ask whether the human would understand
it faster by seeing it, and draw it inline as an **ASCII box diagram**, the form
this repository already uses, because it renders in a terminal, in `less`, in a
diff, and in the design document it will later become. `design-doc`'s
`references/conventions.md` has the two recurring shapes.

Do not draw one per turn. A diagram that repeats what a sentence already said is
noise.

## The exit

Say which path was taken and what happens next.

- **Spike:** the finding, what it means, and the recommendation. Nothing is
  written unless the finding itself deserves a memory or an issue.
- **Bounded:** the design in two paragraphs, approved, then the implementing skill:
  `api-design`, `reconciler-patterns`, `extract-to-atlas-lib`, or `code-cleanup`.
- **Architectural:** hand to `design-doc` with the decisions that are settled, the
  non-goals the conversation established, and the questions that stay open. Those
  three are exactly what the design document's sections need, and a brainstorm
  that produced them has done its job. `work-plan` comes after the design settles,
  never straight from here.

**Hand over the open questions as open.** A brainstorm that resolves everything
has usually resolved something by assumption. Naming what is still undecided is
what lets `design-doc` put it in Open Questions and `work-plan` decide whether it
blocks a split.

## Why this skill has no script

Every other skill in this repository carries a checker, because every other skill
governs an artifact. This one governs a conversation, and there is nothing to
measure: no file to parse, no marker to reconcile, no count to compare. Adding
ceremony here would make brainstorming slower without making it better, which is
the one failure mode this skill cannot afford. The alternative to a good
brainstorm is not a bad brainstorm, it is someone skipping it and writing code.
