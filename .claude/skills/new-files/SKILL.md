---
name: new-files
description: What every new file in this repository opens with: no license header of any kind, and a comment stating what is in the file and why it lives there. Covers Go, Python, YAML, shell, Makefile, and Markdown, plus where a new file belongs and what to regenerate after adding one. Use whenever a file is created, and when reviewing a file that was just added.
---

# New files

Two rules decide the top of every new file. The first is absolute, the second is
where the value is. Before either applies, one question decides whether the file
should exist at all.

## 0. Does the file need to exist?

**A file is a unit of separation of concerns, and a new one is the claim that a
concern is separate.** Make that claim deliberately: the file boundary is what
the next reader will treat as the seam of the design, and every later change
either respects it or works around it.

The opening comment of rule 2 is the test. If its honest version needs an
`and also`, the concern is not one concern: the file is either two files, or a
part of one that already exists. Both failures cost something different, and

- **Splitting what belongs together** scatters one concern across files that each
  need the same context, and the reader has to reassemble it. A 900-line file
  with one subject is easier to work in than three files with none.
- **Merging what does not** produces the file whose comment needs the
  `and also`, and it grows by accretion, because anything can plausibly go in a
  file that is about two things already.

Where a concern already lives, extend it. `atlas-lib` is the worked example of
the boundaries this repository draws: one package per cohesive concern
(`nvme` discovers devices, `nvmeof` connects fabrics, `errs/class` decides what a
caller does about a failure), each saying in its package comment why the seam is
there.

The duplicate case is checked mechanically. `dupl` (Go) and MegaLinter's
`COPYPASTE` (`.jscpd.json`) both fail on a new file that clones a helper the
repository already has, so search for the behavior before writing it. A second
copy is the strongest evidence that the concern already has a home.

## 1. No license header. None.

**A new file carries no license header, no copyright line, and no SPDX
identifier.** Not Apache, not a shortened variant, not a single
`// Copyright 2025 simplyblock` line. The file starts with what it is about.

That is what the repository already does: of 318 hand-written Go files, 244 have
no header. The 74 that do are history, and knowing which is which matters,
because two of them will try to add one to *your* file:

| Where headers exist                                                                                                 | Why                                                                                        | What to do                                                                                                        |
|---------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------|
| `operator/api/v1alpha1/*_types.go` (17), `operator/internal/controller/*` (29), `operator/test/e2e`, `operator/cmd` | scaffolded by `kubebuilder create api` / `create webhook`, which injects the Apache header | **delete the header** from a newly scaffolded file before committing it                                           |
| `operator/api/**/zz_generated.deepcopy.go`                                                                          | `controller-gen object:headerFile="hack/boilerplate.go.txt"` writes it                     | leave it: generated output, and never copy that boilerplate into a hand-written file                              |
| `csi-driver/pkg/**`, `csi-driver/e2e/**` (23)                                                                       | inherited from the upstream Arm/Intel SPDK-CSI code                                        | leave the header where it is. Stripping someone else's copyright from inherited code is not a formatting decision |
| `atlas-lib` (0 of 97)                                                                                               | written here, from scratch                                                                 | the pattern to copy                                                                                               |

Editing a file that already has a header is not an invitation to remove it. Only
new files are in question.

## 2. Open with what is in the file

**The first thing in a new file is a comment saying what is found in it.** Not
the file name restated, not a list of its functions, but the subject and why it
lives here.

`references/openings.md` has the per-language mechanics and worked examples from
the repository. The substance is the same in all of them:

- **What lives here.** One sentence, concrete. "The runner: drives component
  lifecycles, then runs detectors over the evidence."
- **Why it lives here rather than somewhere else.** This is the sentence that
  earns the comment. `atlas-lib/errs/class` does not say "classifies errors." It
  says both consumers faced the same question and used to answer it separately,
  and that answering it once is the point. A reader who knows *why* the boundary
  is here will not put the next function in the wrong file.
- **The non-obvious constraint**, when there is one: an invariant the code
  depends on, an ordering that matters, an alternative that was tried and
  rejected. `initiator_device_test.go` explains that it runs against real
  symlinks because a faked matcher would only ever test the fake.

What it never contains: an author, a date, a ticket number, a changelog, or a
summary of every symbol in the file. Git has the first three, the code is the
last.

Length follows the file. A 60-line helper needs one or two sentences, and a package
entry point earns a paragraph and an indented list. When a file resists the
one-sentence version, it is usually doing two things, and the comment has found
a design problem worth fixing instead of describing.

The comment is prose, so the `house-style` skill applies to it: American
English, the lowercase `simplyblock` brand, the Oxford comma, and the gates that
check all three (comments in Go, Python, and YAML are checked).

## Where the file goes, and what to run after

| New file                     | Directory                                                | After adding it                                                                                           |
|------------------------------|----------------------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| CRD type                     | `operator/api/v1alpha1/<kind>_types.go`                  | `make -C operator manifests generate`, then `make helm-sync`                                              |
| Reconciler                   | `operator/internal/controller/<kind>_controller.go`      | register it in `cmd/main.go`, then `make -C operator manifests` if it carries `+kubebuilder:rbac` markers |
| Domain logic                 | `operator/internal/<domain>/`                            | nothing                                                                                                   |
| Shared node primitive        | `atlas-lib/<concern>/` (public) or `atlas-lib/internal/` | nothing                                                                                                   |
| CSI logic                    | `csi-driver/pkg/util/` or `csi-driver/pkg/<server>/`     | nothing                                                                                                   |
| Chart template               | `helm-charts/charts/simplyblock-operator/templates/`     | never hand-write into `crds/` or `templates/roles/`, which are synced                                     |
| Design document or test plan | `operator/docs/designs/`, `operator/docs/tests/`         | see the `design-doc` skill                                                                                |

Adding an RBAC marker or a CRD field without regenerating is the most common way
a new file breaks CI. See the `build-system` skill for the drift gates.

### A new CRD kind: what the generators do not do

`make -C operator manifests generate` writes the CRD into `config/crd/bases` and
the deepcopy methods, and that is all. The rest is hand-wired, and a new kind
that skips it is silently absent from every install path:

1. **`config/crd/kustomization.yaml`:** add the new `bases/…yaml` to
   `resources:`. This list is maintained by hand (17 entries for 17 CRDs today).
   A CRD missing from it never reaches `dist/install.yaml` or the Helm chart, and
   no gate catches that, because the generator's own output is in sync.
2. **`cmd/main.go`:** register the reconciler, and the webhook if there is one.
3. **The markers on the type:** `+kubebuilder:object:root=true`, the status
   subresource, `+kubebuilder:printcolumn` for `kubectl get`, `+kubebuilder:resource:shortName=…`,
   and the validation and immutability markers. These are the API's contract, and the
   CRD is only their transcript.
4. **Then sync:** `make -C operator build-installer` and `make helm-sync`, and
   commit what they change.

Optional, and only where they earn their keep: a per-kind
`<kind>_{admin,editor,viewer}_role.yaml` in `config/rbac` (4 of 17 kinds have
them, and `helm-sync` copies them into the chart automatically once they exist),
and a `config/samples/storage_v1alpha1_<kind>.yaml` plus its entry in the samples
kustomization (7 of 17 kinds have one).

A new kind is also the moment for a design document and its test plan. See the
`design-doc` and `test-scenarios` skills.

## Naming

- **Go:** lowercase, `_` between words, matching the type or concern
  (`replicationslot_controller.go`, `volumemigration_helpers.go`).
- **Go tests:** `*_unit_test.go` for tests with a fake client and mock HTTP (20
  of them in `operator/internal/controller`), `*_controller_test.go` for the
  envtest suites (6). Pick the suffix that matches what the test actually needs,
  the test plans cite these names.
- **Python:** `snake_case.py`.
- **Markdown:** `design-<slug>.md`, `test-plan-<slug>.md`: never with an issue
  number in the name (see the `design-doc` skill).
- Identifiers inside the file are American English and carry the brand as one
  word, and the `house-style` identifier gate checks that.

## What the linters require of a new Go file

`operator/.golangci.yml` and `csi-driver/.golangci.yml` enable `errcheck`,
`gocyclo`, `goconst`, `dupl`, `lll`, `prealloc`, `unparam`, `unused`,
`nakedret`, `misspell`, `copyloopvar`, `staticcheck`, `govet`, and `revive`, with
`gofmt` and `goimports` as formatters. Three of those shape a file as it is
written rather than after:

- **`revive: comment-spacings`:** `//text` fails, `// text` passes. It applies
  to the opening comment of rule 2 as much as to any other.
- **`revive: import-shadowing`:** a variable may not be named after an imported
  package (`nvme`, `utils`, `class`).
- **`lll`:** line length, excluded only under `api/*`. `dupl` is excluded under
  `internal/*`, so duplication in an API type or a CSI package is a hard failure
  where the same code in a controller is not.

`atlas-lib` has no `.golangci.yml` and runs the tool's defaults.

## Tests, scripts, and throwaway files

- **A new Go file's tests go in its sibling:** `<name>_test.go`, or the
  `*_unit_test.go` / `*_controller_test.go` suffix in
  `operator/internal/controller` (see Naming). The test plans under
  `operator/docs/tests` cite these names, so a renamed test file breaks a
  document. What to test is the `test-scenarios` skill's question, not this one.
- **A new shell script** opens with `#!/usr/bin/env bash`, then the header block
  from `references/openings.md`, then `set -euo pipefail`
  (`scripts/tools.sh` and `helm-charts/scripts/sync-from-operator.sh` are the
  models). Set the executable bit. `make -C csi-driver shellcheck` runs `bash -n`
  and `shellcheck -x` over `scripts`, `deploy`, and `e2e`.
- **Shared functionality lives in `atlas-lib`, in both directions.** Before
  writing a helper, read `atlas-lib/README.md`, which carries the worked flows with
  their idiomatic call sequence and a note on which are already wired, and check
  the package index (`go doc github.com/simplyblock/atlas`,
  `cd atlas-lib && go list ./...`). NVMe
  discovery, NVMe-oF connections, NQN handling, lvol identity, the control-plane
  client, error classification, Kubernetes object matching, locks, and state
  machines exist. And when the new functionality is shared, or could be, it goes
  there rather than into one consumer with a copy waiting to happen in the other.
  The test is what the code is about, not who needs it today: a node-level or
  control-plane-level primitive belongs in `atlas-lib`, Kubernetes-shaped logic
  (reconcilers, CR status, admission) belongs in the consumer. Public package
  under `atlas-lib/<concern>/`, internal under `atlas-lib/internal/`, and never a
  new Go module, because both consumers already carry
  `replace github.com/simplyblock/atlas => ../atlas-lib`. A new public package
  also belongs in the package list in `atlas-lib/doc.go`, and a new flow in
  `atlas-lib/README.md`, since those two are what the next person reads before
  deciding to write their own.
- **A throwaway manifest is not a repository file.** Scratch clusters, test pods,
  and fio jobs belong in the session scratchpad. A manifest that is worth keeping
  needs a home and an opening comment saying what it is for. Build output belongs
  in `.gitignore`, never in a commit.

## Before handing a new file back

1. No license header, no copyright line, no SPDX identifier.
2. An opening comment that says what is in the file and why it is here.
3. One trailing newline, no trailing whitespace.
4. `.claude/skills/house-style/scripts/quality-gate.sh --changed` is clean for it.
5. `make -C <component> lint` passes on it.
6. Whatever the file makes stale is regenerated and committed with it. For a new
   CRD kind, the hand-wired list above as well.
7. Nothing scaffolded came along for the ride: no Apache header, no `TODO(user)`
   comment, no `example` sample left as generated.
