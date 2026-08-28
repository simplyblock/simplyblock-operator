# Consistency: what is checked, and what is outstanding

`scripts/check-crds.py` reads the conventions this skill states out of
`operator/api/v1alpha1` and reports where the types disagree with them. It exists
because the conventions were checkable and were not being checked: the adoption
numbers in `markers.md` were stale within weeks of being written, and a claim in
prose is not something a reviewer can verify across 17 types by eye.

```bash
.claude/skills/api-design/scripts/check-crds.py --changed   # gate an API change
.claude/skills/api-design/scripts/check-crds.py --kind StorageNodeOps
.claude/skills/api-design/scripts/check-crds.py             # the whole backlog
.claude/skills/api-design/scripts/check-crds.py --adoption  # the marker counts
```

**Use `--changed` when handing an API change back.** The full run reports the
repository's accumulated backlog, and a run whose output is mostly other people's
findings is a run that gets skimmed.

## The rules

| Rule                          | Level | Why it is a defect                                                                                             |
|-------------------------------|-------|----------------------------------------------------------------------------------------------------------------|
| `no-status-subresource`       | error | A status write would need spec RBAC and would bump `generation`                                                |
| `no-list-type`                | error | Without a `<Kind>List` the client cannot list the resource                                                     |
| `unenforced-immutability`     | error | A doc comment saying immutable, with none of the three markers that reach the schema. See below                |
| `untyped-phase`               | error | `Phase string` lets an impossible value compile. A named type makes it a compile error                         |
| `conditions-without-listtype` | error | Without `listType=map` and `listMapKey=type`, server-side apply replaces the whole list instead of merging     |
| `enumless-closed-set`         | error | A named string type with constants and no `Enum` marker: the API server accepts anything                       |
| `ops-action-not-enum`         | error | An unknown action becomes a `Failed` phase discovered later instead of an admission rejection                  |
| `no-shortname`                | warn  | A design that names a short name and a type that does not declare it is a test scenario that cannot pass       |
| `thin-printcolumns`           | warn  | Fewer than three columns, so `kubectl get` does not answer "what is happening"                                 |
| `no-age-column`               | warn  | No `creationTimestamp` column                                                                                  |
| `no-observed-generation`      | warn  | A stale status cannot be told from a current one. See `reconciler-patterns`                                    |
| `no-phase-no-conditions`      | warn  | The status reports no progress at all                                                                          |
| `unspecified-spec-field`      | warn  | No `+optional`, no `validation:Required`, and no `omitempty`, so whether it is required is decided by accident |
| `ops-without-action`          | warn  | A kind named `…Ops` with no action verb                                                                        |
| `ops-spec-mutable`            | warn  | A request that can be rewritten while it runs is a different request                                           |

## The three spellings of immutability that work, and the one that does not

This is the trap the checker exists for, and it is easy to get backward.

| Spelling                                                         | Reaches the schema                                                              | Use for                                                    |
|------------------------------------------------------------------|---------------------------------------------------------------------------------|------------------------------------------------------------|
| `// +k8s:immutable`                                              | **yes:** controller-gen emits `x-kubernetes-validations: rule: self == oldSelf` | always immutable, from creation                            |
| `// +kubebuilder:validation:XValidation:rule="self == oldSelf"`  | yes, field level                                                                | the same thing, written out, when the message matters      |
| type-level `XValidation` naming the field with `!has(oldSelf.x)` | yes                                                                             | immutable **once set**, so it can be filled in later       |
| `// Immutable.` in a doc comment                                 | **no**                                                                          | nothing. It documents an intention and enforces none of it |

29 fields use `+k8s:immutable` and the generated CRDs carry 30 `self == oldSelf`
rules, so the marker is live and is the shortest correct spelling. Reach for the
type-level CEL form only when an empty value is a legitimate starting state.

## The backlog, measured 2026-08-25

21 errors and 43 warnings over 17 kinds. Re-measure rather than trusting these:

| Finding                       | Count | Note                                                                                                       |
|-------------------------------|-------|------------------------------------------------------------------------------------------------------------|
| `no-observed-generation`      | 17    | Every kind. No type has ever carried it                                                                    |
| `unspecified-spec-field`      | 13    | Mostly `StorageClusterSpec`, where `operator-sdk:csv` markers stand in for `+optional`                     |
| `untyped-phase`               | 7     | Only `StorageClusterOps`, `StorageNodeOps`, and `VolumeMigration` type their phase                         |
| `no-shortname`                | 7     | `BackupPolicy`, `ControlPlane`, `StorageBackup`, `StorageCluster`, `StorageNodeSet`, `StoragePool`, `Task` |
| `unenforced-immutability`     | 6     | `ReplicationOps` (3) and `ReplicationSlot` (3), prose only, no marker                                      |
| `enumless-closed-set`         | 5     | Including `ReplicationOpsPhase`, which has four constants and no `Enum`                                    |
| `no-phase-no-conditions`      | 4     |                                                                                                            |
| `conditions-without-listtype` | 3     | All three condition-using types: `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`                 |
| `ops-spec-mutable`            | 1     | `ReplicationOps`. The other two Ops kinds use `+k8s:immutable`                                             |
| `thin-printcolumns`           | 1     | `Task` has no printcolumns at all                                                                          |

**The two competing status idioms are the finding behind several rows.** Ten
kinds report progress with `status.phase` and a `message`. Three, all in the
replication family, use `status.conditions` instead. None use both, and none
carry `observedGeneration`. The rule this skill states, a typed phase always and
conditions in addition when something waits on the object, is not what any
existing type does, so a new type follows the rule and an existing one is a
`code-cleanup` candidate rather than something to fix in passing.

Nothing on this page is a bug. Every row is a convention the repository states
and does not yet meet everywhere, which is why the checker separates the two
levels: an error is a defect in the type being changed, and a warning is usually
this backlog showing through.
