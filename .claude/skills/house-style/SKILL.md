---
name: house-style
description: The writing house style of this repository and the gates that enforce it — American English, the Oxford comma, the lowercase simplyblock brand, product-name spelling, punctuation, and impersonal third-person voice. Applies to the design documents and test plans below operator/docs/ and to the prose in Go, Python, and YAML comments. Use when writing or editing any of those, and when a gate reports a finding that has to be resolved.
---

# House style

Ported from the simplyblock documentation repository
(`documentation/.claude/skills/documentation-writing`) and adapted for an
engineering repository. The rules that are about the words are unchanged, so a
sentence reads the same here as it does on the website. The rules that were about
`mkdocs` pages are gone, and the voice rule is narrower — see
**Deviations from the documentation repository** below.

## Scope

| Where                                                    | What is checked                                                |
|----------------------------------------------------------|----------------------------------------------------------------|
| `operator/docs/designs/*.md`, `operator/docs/tests/*.md` | Everything below                                               |
| Any `.md` in the repository                              | Everything below, when passed to the gate                      |
| `.go`, `.py`, `.yaml`                                    | **Comments and docstrings** — every rule except voice          |
| `.go`, `.py`, `.yaml`                                    | **The names they declare** — American English and brand casing |

**The development chart is in scope.**
`helm-charts/charts/simplyblock-operator/` is hand-written source — its
`Chart.yaml`, `values.yaml`, `operator_customresources.yaml`, and `templates/`
are checked like anything else, because its values keys and comments are what a
user reads and types.

Excluded around it:

- **The published chart repositories:** `helm-charts/charts/<version>/`,
  `helm-charts/charts/index.yaml`, and all of `csi-driver/charts/`. Those are
  packaged releases and their index, not sources.
- **The vendored subcharts** under the development chart's own `charts/`.
- **The three paths `make helm-sync` writes:** `crds/`, `templates/roles/`, and
  `templates/simplyblock-operator-webhook.yaml`. A finding there belongs to the
  operator's markers and types; fix it at the source and sync.

Two chart-specific rules keep that scope usable:

- **Commented-out configuration is not prose.** In YAML, a comment that is a
  mapping key, a list item, or a key with a single scalar value is read as
  configuration and skipped, so `# qos:`, `#   iops: 10000`, and
  `# pcieModel: "INTEL SSDPE2KX010T8"` produce no findings. A key followed by
  several words is prose, so `# uuid: the simplyblock cluster UUID` is still
  checked.
- **`Arm` is the company, `ARM` is the architecture.** The upstream Apache header
  names the company, so `Arm Limited`, `Arm Ltd`, and `Arm Holdings` are exempt
  from the terminology rule.

Also skipped: vendored and generated trees (`vendor/`, `.venv/`,
`site-packages/`, `__pycache__/`, `build/`, `dist/`, `.bin/`), test-run artifacts
(`fio-mig-*/`), `*.egg-info/`, files carrying a "generated, do not edit" marker,
and `zz_generated*`/`*.pb.go`. The gate scripts also exclude themselves: their
word lists carry every wrong spelling they look for, as data.

In a source file the code itself is not prose: an identifier is not a
misspelling, a struct tag is not punctuation, and a string literal is a value.
Only comments and Python docstrings are read, and `+kubebuilder:` markers,
`//go:` directives, `# noqa`, `# type:`, and the other tool directives are
skipped. Line and column numbers still refer to the real file, so `--fix` writes
back exactly where it reported.

## Running the gates

```bash
S=.claude/skills/house-style/scripts

$S/quality-gate.sh                       # all gates over operator/docs
$S/quality-gate.sh --changed             # only what changed vs. HEAD, incl. code
$S/quality-gate.sh --paths operator/docs/designs/design-foo.md
$S/quality-gate.sh american punctuation --changed
$S/quality-gate.sh identifiers            # names across the source trees
```

A gate fails on errors and never on warnings. Five checks rewrite their own
findings, and the diff is worth reading afterward, because none of them can tell
a product name from an identifier written without backticks:

```bash
python3 $S/check-simplyblock-spelling.py --fix <paths>
python3 $S/check-terminology.py --fix <paths>
python3 $S/check-american-english.py --fix <paths>
python3 $S/check-punctuation.py --fix <paths>
python3 $S/check-prose.py --fix <paths>
```

The `identifiers` gate has no `--fix` and never will: renaming a name means
renaming every reference to it, which a line-based rewrite cannot do. It also
defaults to the source trees (`operator`, `csi-driver`, `atlas-lib`, `shared`,
`scripts`, `test`, `helm-charts`) rather than to `operator/docs`.

**Run `--changed` before handing work back.** The repository predates these
gates, so a full run over `operator/docs` reports pre-existing findings in
documents nobody is editing. Fix what the current change touches; leave the rest
until it is cleaned up deliberately.

## Voice

Third person, present tense, declarative. Write about the system: "the reconciler
requeues," "the drain blocks until the pinned volume is released." State how the
code behaves rather than hedging about how it should.

No first or second person: no "we," "our," "us," "you," "your," "I," "my," and
none of their contractions. "We resume the node before we fail the action" is
"the operator resumes the node before it marks the action failed." This is gated
in Markdown only — a code comment is written for the next person to read the
function and may address them.

Name the actor. The operator, the reconciler, the CSI driver, the control plane,
the user: an active third-person sentence with a named actor is clearer than a
passive one that hides it. The passive is right when the actor is the reader or
an administrator, the actors that must not be named anyway: "the cluster is
created before the first volume is attached."

An imperative is how a procedure step or a test concept step is written and stays
as it is: "Start drain on node A," "Power off the host."

## Names, terms, and spelling

**The brand is lowercase**: `simplyblock`, mid-sentence, always. It is
capitalized where regular capitalization applies anyway (a heading, the start of
a sentence, link text) and as part of a product name: `Simplyblock Operator`,
`Simplyblock CSI`, `Simplyblock CLI`, `Simplyblock Management API`, and in a
release reference (`Simplyblock 25.10.2`). `Simplyblock Manager` is the old name
of the `Simplyblock Operator` and needs the sentence reworded, not the word
replaced.

**Every other product keeps the spelling its owner uses**: `Kubernetes`,
`OpenShift`, `NVMe`, `NVMe-oF`, `NVMe/TCP`, `Docker`, `Helm`, `Prometheus`,
`Grafana`, `Graylog`, `FoundationDB`, `MinIO`, `SPDK`, `QoS`, `systemd`, `K8s`.
The full list is `TERMS` in `scripts/check-terminology.py`; add a term there
rather than accepting a new spelling. `I/O` carries its slash, and a protocol has
no plural — write `NVMe devices`, never `NVMEs`.

Put an identifier, a field path, a CR kind, a command, or a value in backticks
and every spelling gate leaves it alone. That is the mechanism, not a workaround:
`nvme-cli`, `status.actionStatus.subPhase`, `simplyblock.io/host-id`.

**American English**, always: `color`, `canceled`, `analyze`, `behavior`,
`normalize`, `prioritize`, `initialization`, `labeled`, `center`, `gray`,
`afterward`, `toward`. `Fibre Channel` is the name of a standard and keeps its
spelling.

**A compound with two accepted spellings is written the house way** —
`datacenter` is one word. The list is `ONE_WORD_COMPOUNDS` in
`scripts/check-prose.py`.

**The Oxford comma** goes before the final `and`, `or`, or `nor` of a series of
three or more: "storage nodes, volumes, and snapshots." It belongs to a series
and nowhere else: a comma before an `and` that joins two sentences is ordinary
punctuation, and "graceful and ungraceful shutdowns" is two items.

## Names in code

A name outlives the sentence around it. A field called `behaviourMode` reaches
the CRD, the Helm values, and every manifest a user writes; a helper called
`analyseNode` is read by everyone who touches the package afterward. **The
spelling rules apply to identifiers exactly as they apply to prose.**

Checked: a Go function, method, type, interface, var, const, struct field, and
the name inside a `json:` or `yaml:` struct tag; a Python function, class,
parameter, and assignment; a YAML mapping key, which is the field name of a CR or
of a Helm chart. Names are split into words first, so `camelCase`, `PascalCase`,
`snake_case`, `SCREAMING_SNAKE_CASE`, and `kebab-case` read the same way and an
acronym stays whole: `parseHTTPColorCode` is `parse`, `HTTP`, `Color`, `Code`.

- **American English:** `analyzeNode`, `normalizedScore`, `MigrationStatusCanceled`,
  `sanitizeDNSLabel` — never `analyse`, `normalised`, `Cancelled`, `sanitise`.
- **The brand is one word:** `simplyblock` where it opens a lowerCamel name or
  follows a `_`, `Simplyblock` where it opens a word inside a camelCase or
  PascalCase name, `SIMPLYBLOCK` in an all-caps constant. So
  `NewSimplyblockClient`, never `NewsimplyBlockClient` or `SimplyBlockClient`.

Only the names a file **declares** are checked, never the ones it references: a
call into a dependency spells that dependency's name the way its owner does.

Three things to know before renaming:

- **Use a refactoring tool** (`gopls rename`, the IDE's rename), never a
  search-and-replace. The gate reports; it does not rewrite.
- **An exported name is an API change**, and the finding says so. For a CRD field
  or a Helm value the rename reaches users, so it needs a migration path or a
  deliberate decision to leave it — record that decision rather than silencing
  the gate.
- **A value is not a name.** `MigrationStatusCanceled = "cancelled"` is correct
  when `"cancelled"` is the literal the control plane sends: the constant is
  ours, the string is theirs.

A name forced by an external interface keeps that spelling. That is a decision to
note in the code, not a finding to fix.

Not gated: product names and initialisms inside identifiers. Go's convention
(`NVMeOF`, `HTTPClient`) and prose spelling pull in different directions there,
so it is left to review.

## Punctuation

- **A comma after an abbreviation or an opening connective:** `e.g.,`, `i.e.,`,
  `However,`, `By default,`, `Internally,`. Without it the reader parses the word
  as the subject and starts over. "Then" and "First" take none — they number
  steps.
- **A comma and a full stop go inside a closing quotation mark**; a colon and a
  semicolon stay outside. A value or an identifier takes backticks rather than
  quotation marks, which settles the question before it arises.
- **A mark sits against its word:** no space before a comma, none just inside a
  parenthesis.
- **A compound before a noun is hyphenated**, the same words as a noun are not:
  "a high-availability cluster," but "the cluster provides high availability." An
  adverb is never hyphenated to its adjective: "highly available."
- **A semicolon between two clauses** is a warning: two full stops read easier.
  It stays where it separates items of a series that already carry commas.
- **The subject of a list item is bold with its colon inside the asterisks:**
  `- **Foo:** the text`, never `- **Foo** - the text`.
- **An item continues the sentence above it.** After a heading, a full stop, or a
  colon the item opens a new sentence in upper case; after a line that runs on
  into the list, the item is the rest of that sentence and stays lower case.
- No repeated words, no double spaces, no trailing whitespace, and one final
  newline.

## Short sentences

One idea per sentence. The house median is 16 words and nine in ten stay under
27; past 30, the split is almost always already there at a comma or a colon.
Three signals that one is due: three or more commas before the main verb, a chain
of "which … that … where …" hanging off one noun, or a subordinate clause opening
the sentence and running past the second comma.

Put the fact first and the condition after it. Short does not mean clipped — vary
the length around a short average.

## Deviations from the documentation repository

These are deliberate, and they are why this skill is a copy rather than a
reference:

- **The passive voice is not mandated.** Customer documentation describes a
  system to an operator; a design document explains a mechanism to an engineer,
  and naming the actor is what makes it precise. The pronoun ban stays, the
  passive default does not.
- **The voice gate is Markdown-only, and skips instructions.** Code comments
  address the next reader of the function by design, and so do the files under
  `.claude/` and `.github/`, `CLAUDE.md`, `CONTRIBUTING.md`, and `README.md`: an
  instruction addresses whoever follows it. The rule is about the documents that
  get written.
- **Em dashes stay.** The documentation style replaces them with parentheses or
  a full stop. Design documents here use them in headings, in table cells, and
  for asides, and the corpus is consistent about it. The punctuation gate still
  reports every one as a warning, which is why that gate is loud: a run over a
  design document produces hundreds of them and fails on none. Read them for the
  other findings mixed in, not for the dashes.
- **No `mkdocs` gate.** No frontmatter (`title`, `weight`, `description`), no
  admonitions, content tabs, macros, or snippet includes, so
  `check-mkdocs-syntax.py` was not ported. Generic Markdown structure — nested
  list indentation, table separators, blank lines before lists and fences — is
  covered by MegaLinter's markdownlint, configured in `.mega-linter.yml`.
- **Code fences carry no `title=` attribute.** A fence declares its language
  (`go`, `yaml`, `bash`, `json`, `plain`), and a bare fence is correct for an
  ASCII diagram or pseudocode.
- **The existing corpus is not retrofitted.** Documents written before these
  gates carry British spellings and other findings. They are fixed deliberately,
  not as a side effect of editing something nearby.

- **The identifier gate is new here.** The documentation repository has no code
  to name, so `check-identifiers.py` has no upstream counterpart.

Document structure — section numbering, the metadata block, tables, diagrams — is
not here. It lives in the `design-doc` skill, which owns those documents.

## The last pass

1. **Run the gates** (`--changed`) and clear every error in what the change
   touches. Read the diff of every `--fix`.
2. **Read the warnings and decide each one.** A warning is a question, not a
   verdict: a missing Oxford comma in a two-item pair is not missing, and an em
   dash here is house style.
3. **Read the new text once, start to finish, for readability.** Split what runs
   long. Cut the word that repeats the previous sentence. Put the fact first and
   the condition after it. No fact may enter and no fact may leave in this pass:
   if a sentence turns out to be wrong, that is a content change, made
   deliberately and separately.
