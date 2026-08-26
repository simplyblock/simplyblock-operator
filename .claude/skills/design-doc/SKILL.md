---
name: design-doc
description: Author or update a simplyblock-operator design document in operator/docs/designs/ plus its companion test plan in operator/docs/tests/. Use when the user asks for a design document, design doc, RFC, architecture write-up, or test plan for a feature, CRD, controller, or GitHub issue in this repo, or asks to bring an existing design doc or test plan back in sync with the code. Breaking a settled design into issue-ready work items is the separate `work-plan` skill.
---

# Design Documents and Test Plans

Every substantial operator feature gets three documents, each answering a
question the others must not. This skill owns the first two:

| Document   | Path                                      | Answers                                                                                                         |
|------------|-------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| Design doc | `operator/docs/designs/design-<slug>.md`  | What are we building, why, and how does it behave                                                               |
| Test plan  | `operator/docs/tests/test-plan-<slug>.md` | How do we know it works, and what is still uncovered — the numbered scenario matrix lives here and nowhere else |
| Work plan  | `operator/docs/tasks/work-plan-<slug>.md` | What has to be done, in what order, and what can run at the same time — the **`work-plan`** skill, not this one |

The design doc and the test plan are written together unless the user explicitly
asks for only one. A design doc whose "Testing Strategy" section has no companion
test plan is incomplete.

**The work plan is a different skill because it has a different trigger.** A
design is reviewed and revised before it is sound; work items cite its sections
and its decisions, so splitting one that is still moving produces packages a
later revision merges or deletes. `work-plan` is invoked once the design has
stopped changing shape, and it applies its own readiness test before splitting
anything. §5 below is the hand-off.

Reference material in this skill:

- `references/conventions.md` — house style: metadata block, section numbering,
  diagrams, tables, cross-references, naming. **Read this before writing.**
- `references/design-template.md` — the design doc skeleton, section by section.
- `references/test-plan-template.md` — the test plan skeleton.

Scenario enumeration itself lives in the **`test-scenarios`** skill (coverage
axes, positive/negative derivation, exhaustiveness audit). This skill owns the
documents; that one owns the matrix content.

Wording is owned by the **`house-style`** skill: American English, the Oxford
comma, the lowercase `simplyblock` brand, product-name spelling, punctuation, and
the impersonal third-person voice. Follow it while writing, and run its gate over
what the change touched before handing the work back.

The canonical examples in the repo, in rough order of usefulness:

- `design-issue-130-auto-rebalancing.md` — the fullest example (phasing, algorithm,
  pseudocode, metrics provider interface, backend API protocol). Its
  issue-numbered filename is legacy — new docs are named `design-<slug>.md` and
  name their issues in the metadata block only.
- `design-node-removal-draining.md` — clean state machine + sub-phase controller doc.
- `design-primary-node-placement.md` — layered-algorithm doc with a strong `## Overview`.
- `design-storageclusterops.md`, `design-storagenodeset-storagenode.md` — CRD-introduction docs.
- `test-plan-storagenode-ops.md`, `test-plan-storageclusterops.md`, `test-plan-drain-remove.md`.

## Workflow

### 1. Ground the design in the actual code first

Never write a design doc from the prompt alone. These docs name real Go types,
real endpoints, and real test files, and are read as documentation of the system.
Before drafting, establish:

- **The issue.** If a GitHub issue number is given, read it (`gh issue view <N>`)
  for the requirement, the acceptance criteria, and prior discussion.
- **What already exists.** Grep `operator/api/v1alpha1/` for the CRDs and spec
  structs you intend to touch, `operator/internal/controller/` for the reconcilers,
  `operator/internal/webapi/` for the control-plane client methods and endpoints.
  A design that invents a field which already exists under another name is worse
  than no design.
- **What is already implemented.** This decides the `**Status:**` line and the
  per-section `(Implemented)` / `(Planned)` markers. Check git log and the
  controller code — do not assume a design is aspirational just because it is new.
- **Neighboring designs.** Read the design docs that overlap yours and reuse
  their vocabulary, CR names, annotation keys, and event reasons. Link to them
  rather than restating their mechanisms.
- **Existing tests.** For the test plan you need real file names and real
  `TestXxx` function names: `ls operator/internal/controller/*_unit_test.go`,
  then grep `^func Test` in the relevant files. Never invent a test name and
  list it under "implemented."
- **Existing scenario IDs**, when updating a test plan — the highest `U-`, `I-`,
  `E-`, and `M-` number already assigned, so new scenarios append instead of
  colliding.

### 2. Settle the essentials

Fill these in from the request and the research. Ask the user only for what you
genuinely cannot determine, and ask it in one batch:

- Title and file slug.
- GitHub issue(s), if any.
- Author name (default to the current git user; check `git config user.name`).
- Status, and whether the work is phased (a phased design gets a
  `## Phasing Overview` table right under the metadata block).
- Scope boundaries — the Non-Goals list is where reviews are won or lost, so
  confirm anything ambiguous about what is *out* of scope.
- Nothing about work breakdown. Whether the design is ready to be split, and
  into what, is `work-plan`'s question and is asked after this document settles.

### 3. Write the design doc

Follow `references/design-template.md`. Rules that matter more than the rest:

- **Sections are numbered and stable.** `## 1. Background` through
  `## N. Open Questions`, with `---` between top-level sections and a Table of
  Contents linking every one. Cross-reference sections as `§5.2` in prose.
- **Include only the sections the design needs**, in template order. A doc with
  no state machine should not carry an empty State Machine section; a doc with
  three CRDs needs an `## API Design — New CRDs` section instead of a thin
  `## Data Model Changes`.
- **Show real Go and real YAML.** Spec additions appear as annotated Go structs
  with kubebuilder markers and the doc comments they will actually carry; CR
  examples appear as YAML.
- **The document describes the system, not the discussion that produced it.**
  The general rule is the `house-style` skill's — reference documentation, prose
  over lists, the writer out of the page. Three corollaries are specific to a
  design document, and the corpus violates each of them somewhere:
  - **It is not a changelog of its own evolution.** `Key change from initial
    design:` is a sentence about the document; write the mechanism as it stands
    and let the date line and git carry the rest.
  - **A rejected alternative earns a place only when a reader would otherwise
    propose it again.** Then it is a decision with its reason — "per-member
    parity is not planned; the resiliency story is per-member erasure coding" —
    never a story about how the decision was reached.
  - **Status sets the tense.** An `Implemented` document says what the code does.
    A `Draft` says what the design requires. Neither says what someone intends to
    do: an intention that is not yet true is an Open Question or a phase marked
    as not-yet-true.
- **Every claim about behavior must be traceable** to code, an issue decision,
  or an explicit open question. When something is undecided, say so in
  `## Open Questions` — as a `| # | Question | Owner |` table when other teams
  owe the answer, otherwise as `**Qn: <question>**` prose — and do not paper over
  it with prose that reads as settled.
- **Backend API dependencies get their own table** (method, endpoint, notes),
  including idempotency requirements. Flag endpoints the control plane does not
  yet provide as such — these are the design's external blockers.
- **Observability is not optional.** Kubernetes events (event, type, reason) and
  Prometheus metrics (metric, labels, description) as two tables.
- The `## Testing Strategy` section is a **pointer, not a catalog** — a few
  lines on what each class of test must prove, the harness each needs, where the
  risk concentrates, and a link to the test plan. No scenario tables, no IDs:
  scenarios live in the test plan and only there, because a duplicated list is a
  list that goes stale on one side.

### 4. Write the test plan

Follow `references/test-plan-template.md`. **Enumerate the scenarios with the
`test-scenarios` skill** — it owns the exhaustiveness method: paired
positive/negative derivation and expansion across the coverage axes
(single- vs multi-namespace, single-node / three-node / larger clusters,
single- vs multi-cluster and cross-cluster). Invoke it with the design doc and
the behaviors in hand, then lay its output into this template. Rules that matter
most:

- Open with the `Related design:` back-link and the ID/type legend.
- **This document owns the scenario matrix.** One section per test class, opening
  with one sentence on the harness boundary, then `###` groups per unit under
  test — each naming its test file and citing the design section it verifies
  (`### Latency Deviation (§5.2)`). Rows are
  `| # | Scenario | Type | Test |`: a permanent ID, the scenario, one of
  `Positive` / `Negative` / `Boundary` / `Regression`, and the verbatim test
  function name — or `—` when nothing covers it yet. Numbering runs continuously
  across the groups of a class. See `references/conventions.md` § "Test scenario
  matrices" for the full rules; `design-issue-130-auto-rebalancing.md` §15 is the
  style reference.
- **Grep the test names.** Every function in the `Test` column must exist. One
  function may satisfy several IDs; a test satisfying no listed scenario means a
  row is missing, so add the row.
- **`Boundary` rows are the ones that get skipped.** Thresholds (`==` vs `>`),
  empty collections, single-element clusters, `k = 0`, clamped maxima — walk the
  checklist at the end of the template rather than trusting recall.
- **Include the axis coverage table** the `test-scenarios` skill produces: one
  row per axis with the IDs covering each value. It is what makes "exhaustive"
  checkable instead of claimed, and it turns the combinations you did not test
  into explicit gap rows.
- **Manual scenarios** get prose blocks (`### M-01 — <situation>`) with a design
  reference, **What to verify**, a numbered **Test concept**, and where relevant
  **Current behavior**, **Open question**, or a **Recommended fix** code block.
  This is the hand-off section — it must be executable from the page alone.
- **Close with Coverage Summary and What Is Not Yet Covered.** Every `—` in the
  matrix reappears in the gap table with its reason. An honest gap list is the
  point of the document; do not quietly omit hard scenarios.

### 5. Hand off to the work plan, do not write it here

A design that describes unimplemented work needs a work plan: dependency-ordered
work items, each ready to become one GitHub issue. That document is owned by the
**`work-plan`** skill and is deliberately not written in this step.

The reason is the review loop. A design goes back and forth before it is sound,
and work items cite its section numbers and its decisions. Splitting a design
that is still moving produces items that name sections which renumber under them
and packages that a later revision merges or deletes. The split waits until the
design has stopped changing shape.

So: say that the work plan is the next step and what would gate it, then stop.
Invoke `work-plan` when the design is settled, or when the user asks for the
breakdown regardless. `work-plan` carries the readiness test it applies.

### 6. Run the house style gate — always, on the files just written

**This is not optional and it is not the last thing.** Every document this skill
creates or edits goes through the gate before it is handed back, named
explicitly:

```bash
.claude/skills/house-style/scripts/quality-gate.sh --paths \
  operator/docs/designs/design-<slug>.md \
  operator/docs/tests/test-plan-<slug>.md
```

Name the paths rather than relying on `--changed`. The working tree usually
carries unrelated dirty files, and a run that reports findings in documents this
change never touched is a run whose output gets skimmed. `--changed` is the right
call only when the change spans more files than are convenient to list.

Then:

- **Clear every error.** All seven gates pass, or the work is not done.
- **Read the diff of every `--fix`.** The fixers cannot tell a product name from
  an identifier written without backticks, and the punctuation fixer will move a
  comma inside a phrase that was cited rather than quoted, turning `"e.g.,"` into
  `"e.g.,,"`. Both happen, and both are the writer's to catch.
- **Adjudicate every warning, one at a time.** A warning is not noise to skim
  past. Semicolons and em dashes in running prose read as machine-written, and a
  document built out of them does not read as though a person wrote it. The
  default disposition is to fix: two full stops in place of a semicolon, and a
  comma, a parenthesis, or a fresh sentence in place of an em dash.
  - **Code is exempt.** A warning inside a fenced block, a command, a path, a
    table of literals, or any other quoted value is not prose. Skip it and move
    on without comment.
  - **Every prose warning is either fixed or justified in writing.** The
    justification has to say why this sentence is the exception, not why the mark
    was convenient. Valid reasons are rare and specific: a semicolon separating
    items of a series that already carry commas, which house style keeps, or a
    mark that belongs to quoted source text or to a proper name. "It reads
    better" and "the rest of the corpus does it" are not reasons.
  - **Say what happened.** Report how many warnings the gate raised, how many
    were fixed, and list each one kept with its reason. A bare "the gate passed"
    hides the findings this step exists to surface, because the gate fails on
    errors and never on warnings.

  This is deliberately stricter than the "Em dashes stay" note in
  `house-style/SKILL.md`. For the documents this skill writes, the rule above
  wins.
- **Do not retrofit** documents this change did not otherwise touch. A pre-existing
  finding in a neighboring doc is not this change's work.

### 7. Cross-link and verify

- The three documents link to each other. The design's metadata block carries
  `**Test Plan:**` and, when one exists, `**Work Plan:**`. The test plan's header
  links back to the design, and the work plan's header links to both. Use relative
  paths (`../tests/…`, `../designs/…`, `../tasks/…`) and check that each resolves
  from its own file's directory.
- Verify TOC anchors match the headings (GitHub slugifies to lowercase, spaces to
  hyphens, punctuation dropped — an em dash becomes an extra hyphen). An appendix
  heading therefore reads `## Appendix A: \`Thing\``, with a colon: written with
  an em dash, the dropped dash leaves both of its spaces behind and the anchor
  needs two hyphens where a writer will type one.
- **Check that a `§n` reference means what the reader will assume.** In a test
  plan, a bare `§11` reads as the plan's own section 11; write `design §11` for
  the design's. The two documents have overlapping numbering.
- **Recount anything the document counts.** A scenario total, a per-class count,
  a "17 of 17" claim — derive it from the file rather than from the edit that
  produced it, because a count written by hand goes stale on the next row.
- Keep tables and ASCII diagrams inside a reasonable width and make sure box
  borders line up; misaligned diagrams are the most common defect in these docs.
- MegaLinter also runs a spell check over the repo. If the doc introduces new
  technical terms, add them to `.cspell.json`'s `words` list rather than
  rewording the doc.

### 8. Close the iteration: the gate, then a reference-voice pass

**Every iteration ends here, not only the first.** A revision made in answer to
review feedback is an iteration: rerun step 6's gate over the files it touched,
then read what the iteration added.

No gate checks this part and none can. Step 3's rule — the document describes the
system, not the discussion that produced it — is easy to hold while writing a
section and easy to lose while answering a question about one, because prose
written in reply to a reviewer carries the reviewer into the page. Read every
paragraph the iteration added and look for that second voice:

- **A rebuttal of a position the document never takes.** A paragraph arguing
  against an alternative that appears nowhere else in the document. A reader
  arriving cold finds the design defending itself against nothing.
- **Comparative phrasing whose other half was spoken, not written.** `rather than
  merely supporting it`, `which is what settles it`, or a `not X but Y` whose X
  appears in no other section. The reader never saw X.
- **The decision's history in place of the decision.** `originally`, `this used
  to`, `we considered`. Step 3's changelog corollary, arriving one edit at a time.
- **A defense that grew with the review.** A paragraph carrying three reasons for
  one choice usually accumulated one per round of feedback. Keep the reason the
  design turns on.
- **A rhetorical question.** The document answers questions. It does not ask them
  outside `## Open Questions`.

**The fix is not to strip the rationale.** The corpus voice is "**X is Y because
Z**" and that voice stays. What goes is the other half of an argument: state the
constraint that makes the alternative wrong instead of arguing against the
alternative. A rejected alternative that genuinely has to be recorded already has
two homes, a Non-Goal or an Open Question, and step 3's corollary sets the bar for
earning one.

**An unsettled proposal belongs in an appendix, not in the body.** A mechanism the
document has not committed to reads as contract when it sits among the sections
that are. Give it an appendix, say in the body what holds until it is adopted, and
point at it from the Open Question that owns the decision.

Report what the pass found: which paragraphs were rewritten, or that none were.

### Before handing the work back

1. All seven house style gates pass on every file this change created or edited.
2. Every punctuation warning outside a code block is either fixed or listed with
   a reason that survives scrutiny.
3. Every document this change owes exists, and each links to the others.
4. Every TOC anchor and relative link resolves.
5. Every `§` reference is unambiguous about which document it means.
6. Every count in the text matches the file.
7. Every test function named in the test plan's `Test` column exists (grepped,
   not recalled).
8. Every paragraph this iteration added has been read for the second voice of
   step 8, and every unsettled proposal sits in an appendix rather than in the
   body.
9. The reply says whether a work plan is the next step, and what still has to
   settle before the design can be split.

## Updating an existing document

Re-sync rather than rewrite. Preserve section numbering and existing anchors —
other docs and commit messages reference them. Update `**Status:**`, extend the
date line to `**Date:** <original> (last updated <today>)`, flip section markers
from `(Planned)` to `(Implemented)`, move resolved items out of
`## Open Questions` into the body as decisions, and add newly discovered tests to
the test plan's implemented tables while striking them from the gap list. Say in
your reply which sections you changed and which open questions were resolved.

New scenarios append to the end of their class section with the next free ID —
never renumber a matrix to keep it thematically ordered, because those numbers
are cited from review history and commit messages. An obsolete scenario keeps its
row, struck through with the ID that superseded it. When a test lands, fill in the
`Test` column and delete the matching row from the gap table — those two edits
always happen together.

A re-sync is an iteration and ends at step 8, gate included. The voice pass
matters more here than on a first draft: a re-sync is written against feedback,
which is exactly the prose that carries an interlocutor.

When a re-sync changes what still has to be built, the design's work plan is
affected and is not this skill's to edit. Say which sections moved and hand the
consequences to `work-plan`, which owns whether the existing plan is amended or
superseded.
