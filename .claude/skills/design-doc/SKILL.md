---
name: design-doc
description: Author or update a simplyblock-operator design document in operator/docs/designs/ plus its companion test plan in operator/docs/tests/. Use when the user asks for a design document, design doc, RFC, architecture write-up, or test plan for a feature, CRD, controller, or GitHub issue in this repo, or asks to bring an existing design doc or test plan back in sync with the code. Breaking a settled design into issue-ready work items is the separate `work-plan` skill.
---

# Design Documents and Test Plans

Every substantial operator feature gets three documents, each answering a
question the others must not. This skill owns the first two:

| Document   | Path                                      | Answers                                                                                                        |
|------------|-------------------------------------------|----------------------------------------------------------------------------------------------------------------|
| Design doc | `operator/docs/designs/design-<slug>.md`  | What are we building, why, and how does it behave                                                              |
| Test plan  | `operator/docs/tests/test-plan-<slug>.md` | How do we know it works, and what is still uncovered. The numbered scenario matrix lives here and nowhere else |
| Work plan  | `operator/docs/tasks/work-plan-<slug>.md` | What has to be done, in what order, and what can run at the same time. The **`work-plan`** skill, not this one |

The design doc and the test plan are written together unless the user explicitly
asks for only one. A design doc whose "Testing Strategy" section has no companion
test plan is incomplete.

**The work plan is a different skill because it has a different trigger.** A
design is reviewed and revised before it is sound, and work items cite its sections
and its decisions, so splitting one that is still moving produces packages a
later revision merges or deletes. `work-plan` is invoked once the design has
stopped changing shape, and it applies its own readiness test before splitting
anything. §5 below is the hand-off.

Reference material in this skill:

- `references/conventions.md`: house style, covering the metadata block, section numbering,
  diagrams, tables, cross-references, naming. **Read this before writing.**
- `references/design-template.md`: the design doc skeleton, section by section.
- `references/test-plan-template.md`: the test plan skeleton.

Scenario enumeration itself lives in the **`test-scenarios`** skill (coverage
axes, positive/negative derivation, exhaustiveness audit). This skill owns the
documents, and that one owns the matrix content.

Wording is owned by the **`house-style`** skill: American English, the Oxford
comma, the lowercase `simplyblock` brand, product-name spelling, punctuation, and
the impersonal third-person voice. Follow it while writing, and run its gate over
what the change touched before handing the work back.

**A design that shows a CRD is bound by the `api-design` skill, so read it before
writing the Go.** The structs in a design document are not illustrations: they are
what gets implemented, so a convention broken in the document is a convention
broken in the API a release later. That skill owns the marker set, the naming of a
boolean toggle, the PascalCase enum values, the spellings of immutability, and the
typed phase, and its `scripts/check-crds.py` audits the shipped types against all
of them. The document's own conformance is checked by reading, since a Go block in
Markdown is not a type the script can parse.

The canonical examples in the repo, in rough order of usefulness:

- `design-issue-130-auto-rebalancing.md`: the fullest example (phasing, algorithm,
  pseudocode, metrics provider interface, backend API protocol). Its
  issue-numbered filename is legacy. New docs are named `design-<slug>.md` and
  name their issues in the metadata block only.
- `design-storagenode.md`: an entity, its `Ops` companion, and the Kubernetes workload
  it runs as, in one document. The reference for several state graphs over one `Ops`
  kind, and for a document that absorbs the ones it supersedes.
- `design-primary-node-placement.md`: layered-algorithm doc with a strong `## Overview`.
- `design-storagecluster.md`: an entity and its `Ops` companion in one document,
  which is the shape `design-crd-model.md` §3.1 requires. Also the model for a
  document that specifies a target and confines every delta against the registered
  API to one migration section, rather than annotating each section with what ships.
  §10 is the observability reference.
- `design-node-volume-stack.md` §14, on the `design/volume-stack` branch: the other
  observability reference, and the clearer of the two on opening with the baseline.
- `test-plan-storagecluster.md`: the ID'd scenario matrix, including planned blocks
  that are excluded from the coverage counts.
- `test-plan-storagenode.md`: the largest matrix in the corpus, and the reference
  for an axis coverage table whose blanks are argued rather than listed.

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
  controller code, and do not assume a design is aspirational just because it is
  new.
- **Neighboring designs.** Read the design docs that overlap yours and reuse
  their vocabulary, CR names, annotation keys, and event reasons. Link to them
  rather than restating their mechanisms.
- **Existing tests.** For the test plan you need real file names and real
  `TestXxx` function names: `ls operator/internal/controller/*_unit_test.go`,
  then grep `^func Test` in the relevant files. Never invent a test name and
  list it under "implemented."
- **Existing scenario IDs**, when updating a test plan: the highest `U-`, `I-`,
  `E-`, and `M-` number already assigned, so new scenarios append instead of
  colliding.

### 2. Settle the essentials

Fill these in from the request and the research. Ask the user only for what you
genuinely cannot determine, and ask it in one batch:

- Title and file slug.
- GitHub issue(s), if any.
- Author name (default to the current git user, from `git config user.name`).
- Status, and whether the work is phased (a phased design gets a
  `## Phasing Overview` table right under the metadata block).
- Scope boundaries: the Non-Goals list is where reviews are won or lost, so
  confirm anything ambiguous about what is *out* of scope.
- Nothing about work breakdown. Whether the design is ready to be split, and
  into what, is `work-plan`'s question and is asked after this document settles.

### 3. Write the design doc

Follow `references/design-template.md`. Rules that matter more than the rest:

- **Sections are numbered and stable.** `## 1. Background` through
  `## N. Open Questions`, with `---` between top-level sections and a Table of
  Contents linking every one. Cross-reference sections as `§5.2` in prose.
- **Include only the sections the design needs**, in template order. A doc with
  no state machine should not carry an empty State Machine section, and a doc with
  three CRDs needs an `## API Design — New CRDs` section instead of a thin
  `## Data Model Changes`.
- **Show real Go and real YAML.** Spec additions appear as annotated Go structs
  with kubebuilder markers and the doc comments they will actually carry, and CR
  examples appear as YAML. Both are bound by the `api-design` skill, and the two
  that get broken most often in a document are the enum values, which are
  PascalCase for anything this API group defines (`action: RollingRestart`, never
  `rolling-restart`), and the boolean toggles, which are `enableXyz` or
  `disableXyz` and nothing else. A YAML example is where a wrong enum value
  propagates, because it is what a reader copies.
- **A design that specifies a CRD ends with the whole type, in an appendix.** See
  step 3a.
- **The document describes the system, not the discussion that produced it.**
  The general rule is the `house-style` skill's: reference documentation, prose
  over lists, and the writer out of the page. Three corollaries are specific to a
  design document, and the corpus violates each of them somewhere:
  - **It is not a changelog of its own evolution.** `Key change from initial
    design:` is a sentence about the document. Write the mechanism as it stands
    and let the date line and git carry the rest.
  - **A rejected alternative earns a place only when a reader would otherwise
    propose it again.** Then it is a decision with its reason ("per-member
    parity is not planned, because the resiliency story is per-member erasure
    coding") and never a story about how the decision was reached.
  - **These are reference designs, so nothing in one argues for itself.** A
    section states what the design is. It does not make a case for it against an
    option nobody is holding. The reader is not a reviewer to be persuaded, and a
    design that defends itself reads as a design that expects to lose. A section
    titled `Why X and Not Y`, or whose body is a list of what Y cannot do, is this
    mistake at section scale: `Why a Kind and Not a List` becomes
    `One Object Per Device`, and the paragraphs arguing that a list cannot be
    operated on become paragraphs saying that the object has an identity and a
    lifecycle.
  - **The exception is a design that is genuinely unintuitive, and the question it
    answers is "why is it that way?"** Asking instead why not the obvious
    alternative produces different prose from the same facts. The first framing
    states the constraint that shapes the design and stops
    ("`StorageClass.parameters` is immutable, so a class an older operator
    generated can never be rewritten"). The second reaches for a comparison, drags
    the alternative onto the page to knock it down, and leaves the reader holding
    an option they never had. Reach for the exception when a competent reader would
    stop and ask, not whenever a choice was made.
  - **Status sets the tense.** An `Implemented` document says what the code does.
    A `Draft` says what the design requires. Neither says what someone intends to
    do: an intention that is not yet true is an Open Question or a phase marked
    as not-yet-true.
- **Every claim about behavior must be traceable** to code, an issue decision,
  or an explicit open question. When something is undecided, say so in
  `## Open Questions`, as a `| # | Question | Owner |` table when other teams
  owe the answer and otherwise as `**Qn: <question>**` prose, and do not paper
  over it with prose that reads as settled.
- **Backend API dependencies get their own table** (method, endpoint, notes),
  including idempotency requirements. Flag endpoints the control plane does not
  yet provide as such, since these are the design's external blockers.
- **Observability is designed, not reported.** Two tables, Kubernetes events and
  Prometheus metrics, and both specify what the design needs rather than what
  happens to exist. A section reading "no metrics exist for either kind" is not an
  observability section. The two failure modes are that one, and an `Event` column
  of labels rather than conditions. `references/conventions.md` has the full rules,
  including how the event's target object is chosen and why one reason per
  condition beats one per variant.
- The `## Testing Strategy` section is a **pointer, not a catalog:** a few
  lines on what each class of test must prove, the harness each needs, where the
  risk concentrates, and a link to the test plan. No scenario tables, no IDs:
  scenarios live in the test plan and only there, because a duplicated list is a
  list that goes stale on one side.

### 3a. A CRD design ends with its type, whole, in an appendix

A per-kind design document closes with **one appendix per generated file**, named
for it: `## Appendix A: \`storagenode_types.go\``. The appendix holds the type as
it is to be written, in file order, and it is complete: the enums with their Go
constants, the nested structs, `Spec`, `Status`, the root type with every
`+kubebuilder:` marker it carries, and the `List`.

**Without it a design does not actually specify the type it is about.** Before this
rule, `design-storagecluster.md` argued twenty spec fields across five paragraphs
and showed three of them in Go, and both its status structs existed only as prose.
Neither it nor `design-storagenode.md` stated a single `printcolumn`, `shortName`,
or enum constant. A reader could not answer what type `status.resources.devices`
is, and an implementer had to invent the answer.

**The appendix is the only full copy, and the body quotes rather than repeats.**
Two copies of a struct agree on the day they are written and disagree a revision
later, and the body's is the copy that gets edited. So a section keeps the one or
two fields its argument turns on, as bare fields rather than a `type X struct {`
that would read as the whole thing, and says the type is in the appendix. That
split also gives each half its right register: the appendix carries the doc comment
the shipped file will carry, and the argument for it stays in the prose. A comment
reading "it is not a set of overrides on anything: with StorageNodeSet retired,
there is no fleet object left to override" is a design argument that has no business
in a `_types.go` file.

**A block belonging to another kind gets its own appendix**, headed with the file
it lands in, so nobody reads it as part of the kind this document owns.
`design-storagenode.md` Appendix C is that case: it specifies a group on
`StorageCluster` that the `StorageNodeSet` retirement requires.

**A type another design owns is referenced, not restated.** Say which design
declares it and move on. `design-storagecluster.md`'s Appendix A does this for the
rebalancing settings structs, which also keeps an unsettled naming decision out of
a document that has not taken it.

**Then run the gate over it:**

```bash
.claude/skills/api-design/scripts/check-crds.py --design operator/docs/designs/design-<slug>.md
```

It extracts the Go from the appendices and runs the same audit it runs against
the shipped types, so a wrong enum casing, a `skipXyz` toggle, a missing
`printcolumn`, or immutability claimed in prose is caught while the design is
still a document. It fails if the appendix declares no root kind, which is the
check that the appendix is a type rather than a sketch. Both existing designs
returned real findings on their first run, one in the document and one in the
checker.

### 4. Write the test plan

Follow `references/test-plan-template.md`. **Enumerate the scenarios with the
`test-scenarios` skill**, which owns the exhaustiveness method: paired
positive/negative derivation and expansion across the coverage axes
(single- vs multi-namespace, single-node / three-node / larger clusters,
single- vs multi-cluster and cross-cluster). Invoke it with the design doc and
the behaviors in hand, then lay its output into this template. Rules that matter
most:

- Open with the `Related design:` back-link and the ID/type legend.
- **This document owns the scenario matrix.** One section per test class, opening
  with one sentence on the harness boundary, then `###` groups per unit under
  test, each naming its test file and citing the design section it verifies
  (`### Latency Deviation (§5.2)`). Rows are
  `| # | Scenario | Type | Test |`: a permanent ID, the scenario, one of
  `Positive` / `Negative` / `Boundary` / `Regression`, and the verbatim test
  function name, or `—` when nothing covers it yet. Numbering runs continuously
  across the groups of a class. See `references/conventions.md` § "Test scenario
  matrices" for the full rules. `design-issue-130-auto-rebalancing.md` §15 is the
  style reference.
- **Grep the test names.** Every function in the `Test` column must exist. One
  function may satisfy several IDs, and a test satisfying no listed scenario means a
  row is missing, so add the row.
- **`Boundary` rows are the ones that get skipped.** Thresholds (`==` vs `>`),
  empty collections, single-element clusters, `k = 0`, and clamped maxima. Walk the
  checklist at the end of the template rather than trusting recall.
- **Include the axis coverage table** the `test-scenarios` skill produces: one
  row per axis with the IDs covering each value. It is what makes "exhaustive"
  checkable instead of claimed, and it turns the combinations you did not test
  into explicit gap rows.
- **Manual scenarios** get prose blocks (`### M-01: <situation>`) with a design
  reference, **What to verify**, a numbered **Test concept**, and where relevant
  **Current behavior**, **Open question**, or a **Recommended fix** code block.
  This is the hand-off section, and it must be executable from the page alone.
- **Close with Coverage Summary and What Is Not Yet Covered.** Every `—` in the
  matrix reappears in the gap table with its reason. An honest gap list is the
  point of the document, so do not quietly omit hard scenarios.

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

### 6. Run the house style gate, always, on the files just written

**This is not optional and it is not the last thing.** Every document this skill
creates or edits goes through the gate before it is handed back, named
explicitly:

```bash
.claude/skills/house-style/scripts/quality-gate.sh --paths \
  operator/docs/designs/design-<slug>.md \
  operator/docs/tests/test-plan-<slug>.md \
  2>&1 | tee /tmp/gate-<slug>.txt
```

**Capture the run and never truncate it.** A `| tail` or a `grep` that keeps the
summary turns this step into a no-op, because the summary says `passed` while the
warnings it hides are the ones this step exists to adjudicate. The file is what makes
one-at-a-time possible: the queue survives while it is worked through, and the next
run's output does not scroll the remainder away. `house-style/SKILL.md` has the
mechanics.

Name the paths rather than relying on `--changed`. The working tree usually
carries unrelated dirty files, and a run that reports findings in documents this
change never touched is a run whose output gets skimmed. `--changed` is the right
call only when the change spans more files than are convenient to list.

Then:

- **Clear every error.** All nine gates pass, or the work is not done.
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

  `house-style/SKILL.md` states the same rule. What is added here is the
  procedure: adjudicate one at a time, and write down the reason for each mark
  kept.
- **Do not retrofit** documents this change did not otherwise touch. A pre-existing
  finding in a neighboring doc is not this change's work.

### 7. Cross-link and verify

- The three documents link to each other. The design's metadata block carries
  `**Test Plan:**` and, when one exists, `**Work Plan:**`. The test plan's header
  links back to the design, and the work plan's header links to both. Use relative
  paths (`../tests/…`, `../designs/…`, `../tasks/…`) and check that each resolves
  from its own file's directory.
- Verify TOC anchors match the headings (GitHub slugifies to lowercase, spaces to
  hyphens, punctuation dropped, so an em dash becomes an extra hyphen). An appendix
  heading therefore reads `## Appendix A: \`Thing\``, with a colon: written with
  an em dash, the dropped dash leaves both of its spaces behind and the anchor
  needs two hyphens where a writer will type one.
- **Check that a `§n` reference means what the reader will assume.** In a test
  plan, a bare `§11` reads as the plan's own section 11, so write `design §11` for
  the design's. The two documents have overlapping numbering.
- **Recount anything the document counts.** A scenario total, a per-class count,
  a "17 of 17" claim: derive it from the file rather than from the edit that
  produced it, because a count written by hand goes stale on the next row.
- Keep tables and ASCII diagrams inside a reasonable width and make sure box
  borders line up. Misaligned diagrams are the most common defect in these docs.
- MegaLinter also runs a spell check over the repo. If the doc introduces new
  technical terms, add them to `.cspell.json`'s `words` list rather than
  rewording the doc.

### 8. Close the iteration: the gate, then a reference-voice pass

**Every iteration ends here, not only the first.** A revision made in answer to
review feedback is an iteration: rerun step 6's gate over the files it touched,
then read what the iteration added.

No gate checks this part and none can. Step 3's rule, that the document describes
the system and not the discussion that produced it, is easy to hold while writing a
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
- **A section that exists to win an argument.** Its title compares
  (`Why X and Not Y`, `X, Not Y`) or its body is a list of what the alternative
  cannot do. Retitle it for what the design *is* and keep only the paragraphs that
  say something about the design. Those that only say something about the
  alternative go. This is the one item on the list that survives a paragraph-level
  read, because every paragraph in such a section is locally fine and the section
  is the defect.
- **A justification that outlived its question.** A section written to answer an
  Open Question stays behind when the question is settled and deleted, and then
  reads as a defense of something nobody disputes. Whenever an Open Question is
  removed, grep the corpus for what cited it and check whether a section exists
  only as its answer.

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

1. All nine house style gates pass on every file this change created or edited.
2. A design specifying a CRD ends with the whole type in an appendix (step 3a),
   the body holds no second copy of any struct, and
   `check-crds.py --design <doc>` reports no error.
3. Every punctuation warning outside a code block is either fixed or listed with
   a reason that survives scrutiny.
4. Every document this change owes exists, and each links to the others.
5. Every TOC anchor and relative link resolves.
6. Every `§` reference is unambiguous about which document it means.
7. Every count in the text matches the file.
8. The Observability section designs events and metrics rather than reporting that
   none exist, its `Event` column holds conditions rather than labels, and it names
   the object its events land on.
9. Every test function named in the test plan's `Test` column exists (grepped,
   not recalled).
10. Every paragraph this iteration added has been read for the second voice of
   step 8, and every unsettled proposal sits in an appendix rather than in the
   body.
11. The reply says whether a work plan is the next step, and what still has to
    settle before the design can be split.

## Updating an existing document

Re-sync rather than rewrite. Preserve section numbering and existing anchors,
because other docs and commit messages reference them. Update `**Status:**`, extend the
date line to `**Date:** <original> (last updated <today>)`, flip section markers
from `(Planned)` to `(Implemented)`, move resolved items out of
`## Open Questions` into the body as decisions, and add newly discovered tests to
the test plan's implemented tables while striking them from the gap list. Say in
your reply which sections you changed and which open questions were resolved.

New scenarios append to the end of their class section with the next free ID.
Never renumber a matrix to keep it thematically ordered, because those numbers
are cited from review history and commit messages. An obsolete scenario keeps its
row, struck through with the ID that superseded it. When a test lands, fill in the
`Test` column and delete the matching row from the gap table. Those two edits
always happen together.

A re-sync is an iteration and ends at step 8, gate included. The voice pass
matters more here than on a first draft: a re-sync is written against feedback,
which is exactly the prose that carries an interlocutor.

When a re-sync changes what still has to be built, the design's work plan is
affected and is not this skill's to edit. Say which sections moved and hand the
consequences to `work-plan`, which owns whether the existing plan is amended or
superseded.
