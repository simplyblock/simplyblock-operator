---
name: extract-to-atlas-lib
description: Move a shared primitive into atlas-lib and adopt it in both consumers: deciding whether it belongs there at all, whether it extends an existing package or needs a new one, whether it is public or internal, then performing the move, deleting both copies, and updating the atlas-lib README index and Today pointers the move invalidates. Use when the same helper exists in both the operator and the CSI driver, when a node-level or control-plane primitive is about to be written in a consumer, when an atlas-lib helper was copied because the original is unexported, and as the delegate of the code-cleanup skill's deduplication and modernization passes.
---

# Extracting into atlas-lib

`atlas-lib` exists because the operator and the CSI driver kept solving the same
problems separately, and **a second implementation of a primitive is worse than
a slightly awkward dependency on the first**: the two copies drift, and a fix
lands in one of them. This skill is how a primitive gets there and how both
copies actually disappear.

It is invoked on its own, or by `code-cleanup` passes 3 and 4 with a candidate
already identified.

## 0. Does it belong there at all?

**The test is what the code is about, not who needs it today.**

| Belongs in `atlas-lib`                                                            | Belongs in the consumer                                                  |
|-----------------------------------------------------------------------------------|--------------------------------------------------------------------------|
| A node-level storage primitive: NVMe, NVMe-oF, sysfs, device mapping              | A reconciler, a phase, a CR status write, a webhook admission check      |
| A control-plane concept: the client, its request and response types               | An RBAC or manifest concern, anything shaped by a `+kubebuilder:` marker |
| An identity or naming rule both components must agree on, character for character | A CSI RPC handler, a gRPC status mapping at the service boundary         |
| Correlation between an lvol and a Kubernetes object                               | Anything whose signature mentions a CRD type from `operator/api`         |
| A cross-cutting utility both already use: pointers, sentinels, URL checks         | Something one consumer needs and the other plausibly never will          |

Two rules that admit no exceptions:

- **Never a new Go module.** Both consumers already carry
  `replace github.com/simplyblock/atlas => ../atlas-lib`. A third module would
  need that wiring everywhere it is consumed.
- **A CRD type never crosses the boundary.** If the primitive needs one, the
  primitive is not the shared thing. The plain data it operates on is. Take the
  fields, not the type.

**The Kubernetes-shaped trap.** Duplication across controllers is real
duplication and still does not go here. `handleDeletion`, `patchStatus`,
`succeedOps`, `failOps`, `ensureFinalizer`, and `releaseLock` are copied across
the operator's controllers. They are about reconciliation, so they belong in a
shared base inside the operator, and `reconciler-patterns` owns their shape.
`atlas-lib/kube` is the exception that proves the rule: it holds lvol-to-PV
correlation, not controller machinery.

## 1. Find the twin, and read what already exists

```bash
.claude/skills/code-cleanup/scripts/find-twins.sh --cross
.claude/skills/code-cleanup/scripts/find-twins.sh --handrolled
```

Then the two authoritative lookups, in this order, and neither of them is memory:

```bash
go doc github.com/simplyblock/atlas             # overview and package list
(cd atlas-lib && go list ./...)                 # every package, internal included
go doc github.com/simplyblock/atlas/nvmeof      # one package in detail
```

`atlas-lib/README.md` answers the question behind most extractions: how a thing
is already done here. Its `Layout` block indexes every package file by file, its
`Use cases` show the idiomatic call sequence for each flow the two consumers
perform, and each `_Today:_` note points at the live call site, so it is visible
which patterns are wired and which are available but unadopted.

**Four outcomes, and only one of them is an extraction:**

| What the search finds                  | What to do                                                                |
|----------------------------------------|---------------------------------------------------------------------------|
| The primitive exists and is exported   | Not an extraction. Adopt it and delete the copy — `code-cleanup` pass 3   |
| It exists but is unexported            | Export it in place, with its documentation; then adopt. No new code       |
| It exists with different edge behavior | Read both. Reconcile deliberately, and if one is wrong, `regression-test` |
| Nothing like it exists                 | Extract, from here on                                                     |

The second row is common and cheap. `csi-driver/pkg/util/nvmerepair.go:356` is a
copy of the unexported `atlas-lib/nvmeof/inspect.go:498`, and `nvmerepair.go:373`
is a copy of the unexported `atlas-lib/nvmeof/repair.go:600`. Nothing needed
designing in either case. The primitive was simply out of reach.

## 2. Choose the home

`references/placement.md` carries what each existing package owns, which is the
first question: **extend before adding.** A new package is justified when the
concern is genuinely not one of the existing ones, and its package comment has to
be able to say why the seam is there, in one sentence, without an "and also."

| Decision           | The rule                                                                                                                              |
|--------------------|---------------------------------------------------------------------------------------------------------------------------------------|
| Public or internal | `atlas-lib/<concern>/` when a consumer imports it; `atlas-lib/internal/` when only the library does                                   |
| Extend or add      | Extend the package that owns the concern. A new one needs a one-sentence reason for its own existence                                 |
| Layering           | A package may import one below it — `lvol` imports `nvme`, `nvmeof` imports `nvme` — and never the reverse                            |
| Naming             | The package reads like the problem (`nvme`, `nvmeof`, `lvol`, `nqn`), no `pkg/` prefix, and never a name like `util` or `common`      |
| Seams              | Every public API is an interface or takes a config, so a consumer test needs no kernel, `/sys`, `nvme-cli`, cluster, or control plane |

## 3. Move it

Order matters: the library lands first and complete, then each consumer adopts,
then the copies go. A move that adopts before the primitive is tested leaves two
implementations and no test for either.

1. **Write it in `atlas-lib`,** with its doc comment saying why the seam is
   there and what invariant it holds. No license header (see `new-files`).
2. **Write its tests there,** against the seam, with no kernel, cluster, or
   control plane. Existing fixture patterns: a sysfs tree under `testdata/sys`,
   a fake clientset, a hand-written `nvmeof.Connector`.
3. **Adopt in consumer A.** Its tests stay green without being rewritten to
   match the new implementation. If they need rewriting, behavior moved.
4. **Adopt in consumer B.**
5. **Delete both copies.** Not deprecated, not wrapped, not left behind a
   one-line delegation. **Neither consumer keeps a wrapper**, because a wrapper
   is the third implementation and it is the one that drifts next.
6. `go mod tidy` in each module that changed. The `replace` directive is already
   in place in both.

**Prove the deletion, in the report:**

```bash
grep -rn '<old symbol>' operator csi-driver --include='*.go'   # expect nothing
make test                                                       # all three modules
```

If a copy has to survive temporarily (a call site that cannot move in this
change), that is a **transition**, not a completed extraction. Name the owner and
the condition for removal, and do not claim the deduplication until the last copy
is gone. `code-cleanup`'s mechanism gate enforces this distinction.

## 4. Update what the move invalidated

A move silently falsifies the documentation that made it findable, which is what
makes the next extraction harder than this one.

- **`atlas-lib/README.md` `Layout`:** the file-by-file index. A new file, a new
  package, or a moved one changes it.
- **`atlas-lib/README.md` `Use cases`:** if the primitive is part of a flow the
  consumers perform, its idiomatic call sequence belongs in the matching
  subsection: *Control plane*, *Kubernetes correlation*, *Node & fabric*, or
  *Cross-cutting*.
- **The `_Today:_` note:** this is the one that is always forgotten. It names
  the live call site for a pattern, and an extraction is exactly the event that
  changes it. A note reading "still calls the control plane through its own
  client" has to stop saying that once it no longer does.
- **`atlas-lib/doc.go`:** the package index in the library overview.
- **The package doc comment:** for a new package, why this seam exists.
- **`.claude/skills/code-cleanup/references/worklist.md`:** if the extraction
  closed a ranked row.

Then `.claude/skills/house-style/scripts/quality-gate.sh --changed` over the
prose that changed.

## 5. The gates

- `make test`: all three modules, green.
- `make lint`: no new `nolint`.
- The copies are gone, proved by `grep`, and the proof is in the report.
- Both consumers' tests pass **unchanged in intent**. A test that had to be
  rewritten to match the new implementation is the signal that behavior moved,
  which makes this no longer an extraction.
- `find-twins.sh --cross` no longer pairs the bodies that started this.
- The README says what is true now.
- Nothing under `operator/api/**` was touched. If the extraction seemed to need
  it, revisit section 0. The answer is in the plain data, not the CRD type.
