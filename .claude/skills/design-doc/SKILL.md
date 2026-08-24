---
name: design-doc
description: Author or update a simplyblock-operator design document in operator/docs/designs/ plus its companion test plan in operator/docs/tests/. Use when the user asks for a design document, design doc, RFC, architecture write-up, or test plan for a feature, CRD, controller, or GitHub issue in this repo, or asks to bring an existing design doc / test plan back in sync with the code.
---

# Design Documents and Test Plans

Every substantial operator feature gets two documents that travel together:

| Document   | Path                                      | Answers                                                                                                         |
|------------|-------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| Design doc | `operator/docs/designs/design-<slug>.md`  | What are we building, why, and how does it behave                                                               |
| Test plan  | `operator/docs/tests/test-plan-<slug>.md` | How do we know it works, and what is still uncovered — the numbered scenario matrix lives here and nowhere else |

Write both unless the user explicitly asks for only one. A design doc whose
"Testing Strategy" section has no companion test plan is incomplete.

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

### 5. Run the house style gate — always, on the files just written

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
  comma inside a phrase that was cited rather than quoted — `"e.g.,"` becomes
  `"e.g.,,"`. Both happen; both are the writer's to catch.
- **Decide each warning.** Em dashes are house style here and the gate warns on
  every one, so a design document produces hundreds. Read them for the other
  findings mixed in.
- **Do not retrofit** documents this change did not otherwise touch. A pre-existing
  finding in a neighboring doc is not this change's work.

### 6. Cross-link and verify

- Design doc metadata block links to the test plan; test plan header links back
  to the design doc. Use relative paths (`../tests/…`, `../designs/…`) and check
  they resolve from the file's own directory.
- Verify TOC anchors match the headings (GitHub slugifies to lowercase, spaces to
  hyphens, punctuation dropped — an em dash becomes an extra hyphen).
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

### Before handing the work back

1. All seven house style gates pass on every file this change created or edited.
2. Both documents exist and link to each other.
3. Every TOC anchor and relative link resolves.
4. Every `§` reference is unambiguous about which document it means.
5. Every count in the text matches the file.
6. Every test function named in the plan's `Test` column exists (grepped, not
   recalled).

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
