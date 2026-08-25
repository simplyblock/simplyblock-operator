# The measured backlog

A snapshot, not a specification. It exists so that a cleanup can start from a
ranked list instead of from wherever the last file was open, and so that the
numbers in `SKILL.md` and `passes.md` can be checked rather than believed.

**Measured 2026-08-25.** Re-measure before acting on any row:

```bash
.claude/skills/code-cleanup/scripts/measure.sh --paths <row's scope> --detail
.claude/skills/code-cleanup/scripts/find-twins.sh
```

## Where the mechanism is

| Scope                          | Files | Lines  | Functions | Longest | ≥60 | ≥100 | ≥5 tabs | Files ≥600 |
|--------------------------------|-------|--------|-----------|---------|-----|------|---------|------------|
| `atlas-lib`                    | 61    | 8,895  | 333       | 65      | 2   | 0    | 2       | 0          |
| `operator/internal/controller` | 30    | 18,041 | 424       | 183     | 75  | 22   | 32      | 14         |

`atlas-lib` is the standard this repository already meets: nothing over 100
lines, nothing deeply nested, no oversized file. The controller tree is where a
cleanup earns the most, and the gap between the two columns is the argument.

## Ranked candidates

Ordered by mechanism removed per unit of risk, which is not the same as by size.

| #  | Candidate                                                                                                                                                                           | Evidence                                                                    | Pass | Risk                                                        |
|----|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------|------|-------------------------------------------------------------|
| 1  | Three names for one 16-line body in `simplyblockstoragenodeset_controller.go:353,375,397`                                                                                           | identical normalized bodies                                                 | 4    | Low — watch mappers, covered by the controller's unit tests |
| 2  | `apiClient()` copied verbatim in five controllers                                                                                                                                   | identical normalized bodies                                                 | 4, 8 | Low, but see the `webapi` note below                        |
| 3  | The repeated controller helper set: `handleDeletion` ×5, `patchStatus` ×4, `succeedOps` / `failOps` ×3, `ensureFinalizer` / `releaseLock` / `reconcileStart` / `reconcileDelete` ×2 | name and shape repetition across controllers                                | 4, 8 | Medium — a shared base changes many call sites at once      |
| 4  | Two `atlas-lib` helpers copied into `csi-driver/pkg/util/nvmerepair.go:356,373` because the originals are unexported                                                                | exact body match against `nvmeof/inspect.go:498` and `nvmeof/repair.go:600` | 3    | Low — export and adopt; `extract-to-atlas-lib`              |
| 5  | 12 hand-rolled phase switches across 7 controller files, against 0 `statemachine` adoptions                                                                                         | `switch` on `Phase` / `SubPhase`                                            | 3, 8 | High — a state machine conversion touches control flow      |
| 6  | `nqn` and `lvol` have no importers while both consumers spell out NQNs and split handles by hand                                                                                    | `nqn.Host`, `lvol.VolumeHandle.Split` exist and are unused                  | 3    | Low — mechanical, and both have tests                       |
| 7  | `operator/cmd/main.go:101`, 413 lines of wiring                                                                                                                                     | longest function in the repository                                          | 6    | Low — flat sequence, groups are obvious                     |
| 8  | `BuildStorageNodeSetDaemonSet`, 311 lines at `operator/internal/utils/storage_nodeset_ds.go:48`                                                                                     | second longest                                                              | 6    | Low, and largely a flat literal — split only reused parts   |
| 9  | `storagenodeops_controller.go`, 1,696 lines: reconciler plus hand-rolled machine plus topology                                                                                      | largest hand-written file                                                   | 7    | Medium — a file split, then possibly candidate 5            |
| 10 | `operator/internal/utils/objects.go`, 750 lines named after nothing                                                                                                                 | package-as-bag                                                              | 7    | Medium — moves ripple through imports                       |
| 11 | `operator/cmd/simplyblock-rebalancer/main.go` shells out to `sudo nvme connect`, `disconnect`, and `list`                                                                           | a third nvme-cli call site beside the CSI initiator and `atlas-lib/nvmeof`  | 3    | Medium — node-level behavior, hard to cover off a cluster   |
| 12 | 52 `nolint` directives across the three modules                                                                                                                                     | each is a suppressed finding                                                | —    | Varies — read each one; some are load-bearing               |

**The `webapi` note.** `operator/internal/webapi` (2,065 lines, 245 references)
is being retired in favor of `atlas-lib/controlplane`. Candidate 2 unifies five
copies of a helper that constructs a client which is going away, so unify it in
the shape that survives the migration, or leave it and let the migration delete
all five. Do not deepen the investment either way.

## Not a cleanup: the linter gap

Two configuration facts explain why several rows above went unnoticed for so
long. Both are one-line changes with a large first-run finding count, and both
belong in a commit of their own rather than inside a cleanup.

- `operator/.golangci.yml` and `csi-driver/.golangci.yml` (identical) exclude
  `dupl` and `lll` for `path: internal/*`. In the operator that is 24,926 of
  29,572 hand-written lines, including every controller. Duplication detection
  does not run where duplication accumulates.
- Neither config enables `funlen`, `gocognit`, or `nestif`, and `gocyclo` runs at
  its default minimum of 30. Nothing in CI has an opinion about a 183-line
  function.

A linter that enforces the rule on every commit is worth more than a skill that
has to be invoked, so fixing these is the highest-leverage change on this page.
It is also the one that produces the largest backlog on its first run, which is
the reason to do it deliberately and not as a side effect.
