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

| Rule                          | Level | Why it is a defect                                                                                                       |
|-------------------------------|-------|--------------------------------------------------------------------------------------------------------------------------|
| `no-status-subresource`       | error | A status write would need spec RBAC and would bump `generation`                                                          |
| `no-list-type`                | error | Without a `<Kind>List` the client cannot list the resource                                                               |
| `unenforced-immutability`     | error | A doc comment saying immutable, with none of the three markers that reach the schema. See below                          |
| `untyped-phase`               | error | `Phase string` lets an impossible value compile. A named type makes it a compile error                                   |
| `conditions-without-listtype` | error | Without `listType=map` and `listMapKey=type`, server-side apply replaces the whole list instead of merging               |
| `enumless-closed-set`         | error | A named string type with constants and no `Enum` marker: the API server accepts anything                                 |
| `ops-action-not-enum`         | error | An unknown action becomes a `Failed` phase discovered later instead of an admission rejection                            |
| `enum-value-not-pascal-case`  | error | An enum value this group defines that is not PascalCase, so one object mixes `Pending` with `rolling-restart`. See below |
| `toggle-not-enable-disable`   | error | A boolean toggle named anything but `enableXyz` or `disableXyz`, so the zero value is not the default. See below         |
| `no-observed-generation`      | error | A stale status cannot be told from a current one, and a spec edit cannot be waited on. `design-crd-model.md` §7.9        |
| `no-shortname`                | warn  | A design that names a short name and a type that does not declare it is a test scenario that cannot pass                 |
| `thin-printcolumns`           | warn  | Fewer than three columns, so `kubectl get` does not answer "what is happening"                                           |
| `no-age-column`               | warn  | No `creationTimestamp` column                                                                                            |
| `no-phase-no-conditions`      | warn  | The status reports no progress at all                                                                                    |
| `unspecified-spec-field`      | warn  | No `+optional`, no `validation:Required`, and no `omitempty`, so whether it is required is decided by accident           |
| `ops-without-action`          | warn  | A kind named `…Ops` with no action verb                                                                                  |
| `ops-spec-mutable`            | warn  | A request that can be rewritten while it runs is a different request                                                     |

## Boolean toggles: `enableXyz` or `disableXyz`, nothing else

A property that turns something on or off is `enableXyz` when the thing is off by
default and `disableXyz` when it is on. `skipXyz`, `withXyz`, `noXyz`,
`xyzEnabled`, and a bare `enabled` are all rejected. `design-crd-model.md` §7.5 is
the rule and §9.6 is the twelve fields that violate it.

**The choice between the two forms is not taste, it is the default.** A Go `bool`
zero-values to `false`, so only the negative spelling makes an unset field mean
the default when the default is on. `enableXyz *bool` with `nil` read as `true`
moves the default out of the type and into the controller.

The prefix is also the Kubernetes-native side: core `PodSpec` carries
`enableServiceLinks`, and the kubelet configuration carries eight `enableXyz`
fields and no suffixed ones. Suffixed `-Enabled` appears twice in the whole of
`k8s.io/api`, both in the deprecated ScaleIO volume source.

**What the rule is not about.** A boolean naming a fact (`ubuntuHost`,
`openShiftCluster`) is not enabling a capability, and status booleans are
observations. Neither are an `Ops` spec's booleans: an entity's spec says whether a
capability is on, while an `Ops` spec says what one request should do, so `force`,
`deleteSource`, and `refreshSNodeAPI` qualify an operation rather than switch
anything on. The checker therefore skips `*Status` structs and every `*Spec` of an
`Ops` kind, and otherwise fires only on an invalid name shape or a doc comment
saying the field enables, disables, or skips something. A toggle with no doc comment
at all is missed: `snapshotBackups` and `localTesting` are two, found by reading.

## Enum values: PascalCase, unless the word is not ours

Every value of a `+kubebuilder:validation:Enum` this group defines is PascalCase.
`Activate`, `RollingRestart`, `HostMaintenance`, `ControlPlane`. Not `activate`,
not `rolling-restart`, not `shutdown_called`.

Three things follow from it, and the third is the one people find surprising:

- **It matches core Kubernetes**, which spells every enum it owns this way:
  `Pending`, `Running`, `IfNotPresent`, `ClusterIP`, `Retain`, `OnFailure`.
- **It matches the phases this group already has.** `StorageClusterOpsPhase` is
  `Pending;Running;Succeeded;Failed` and its action is `activate;expand;shutdown`,
  so one object reports two casings and nothing explains which a reader should
  expect where.
- **The Go constant and the wire value become the same word.**
  `StorageNodeOpsActionHostMaintenance = "HostMaintenance"` reads straight off
  `action: HostMaintenance`. With a kebab-case value the two spellings diverge and
  every reader has to check the constant to be sure which hyphenation the API
  wants.

**A value that names something outside this group keeps that thing's spelling.**
`ext4` and `xfs` are the kernel's names for filesystems, and renaming them to
`Ext4` would be inventing a spelling nothing else uses. The same holds for a wire
protocol and for a value mirrored verbatim from the control plane's own
vocabulary. The test is whether this group invented the word, and the checker
carries the list rather than trying to recognize a foreign word by shape.

**A backend status reflected into `status` is not an enum of ours at all.**
`StorageNode.status.status` holds `online`, `in_creation`, and `in_restart`
because those are the control plane's strings, and it carries no `Enum` marker
for that reason. Reflecting a value and defining one are different things, and
only the second is covered by this rule.

## The spellings of immutability that work, and the one that does not

| Spelling                                                         | Reaches the schema | Semantics                                                  |
|------------------------------------------------------------------|--------------------|------------------------------------------------------------|
| `// +k8s:immutable` on an optional field                         | **yes**, two rules | immutable **once set**: fillable later, then frozen        |
| `// +k8s:immutable` on a `Required` field                        | **yes**, one rule  | immutable from creation, since it can never be absent      |
| `// +kubebuilder:validation:XValidation:rule="self == oldSelf"`  | yes, field level   | guards the value only, and not removal. See below          |
| type-level `XValidation` naming the field with `!has(oldSelf.x)` | yes                | the long spelling of once-set, and also value-only         |
| `// Immutable.` in a doc comment                                 | **no**             | nothing. It documents an intention and enforces none of it |

`+k8s:immutable` emits two rules. In controller-gen v0.21.0, which is the version
the repo generates with (`operator/dist/install.yaml:14`):

- `pkg/crd/markers/validation.go:603` adds a field-level
  `self == oldSelf` / `field is immutable`.
- `pkg/crd/schema.go:567-579` adds, **only when the field is not in `required`**, a
  parent-level `!has(oldSelf.X) || has(self.X)` /
  `field X is immutable once set`.

The parent rule covers the case the field rule cannot: a field-level transition
rule is evaluated only where the field is present, so a cleared field would
otherwise be re-settable to any value. A first assignment matches neither rule,
which makes the marker once-set on an optional field and from-creation on a
required one. The marker's own documentation shows both `+required` and `+optional`
uses.

The hand-written CEL forms express the same intent at greater length and guard only
the value. Write CEL when the rejection message has to say something the generated
one cannot.

## The backlog, measured 2026-08-28

59 errors and 26 warnings over 17 kinds. Re-measure rather than trusting these:

| Finding                       | Count | Note                                                                                                                                                                  |
|-------------------------------|-------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `no-observed-generation`      | 17    | Every kind. No type has ever carried it, and it is the group's largest single gap                                                                                     |
| `unspecified-spec-field`      | 13    | Mostly `StorageClusterSpec`, where `operator-sdk:csv` markers stand in for `+optional`                                                                                |
| `toggle-not-enable-disable`   | 11    | `design-crd-model.md` §9.6 is the list and what each rename costs                                                                                                     |
| `enum-value-not-pascal-case`  | 10    | Six are the replication kinds, which are out of scope. The other four are the two action verbs, `MetricsBackend`, and the drain phase that dies with `StorageNodeSet` |
| `untyped-phase`               | 7     | Only `StorageClusterOps`, `StorageNodeOps`, and `VolumeMigration` type their phase                                                                                    |
| `no-shortname`                | 7     | `BackupPolicy`, `ControlPlane`, `StorageBackup`, `StorageCluster`, `StorageNodeSet`, `StoragePool`, `Task`                                                            |
| `unenforced-immutability`     | 6     | `ReplicationOps` (3) and `ReplicationSlot` (3), prose only, no marker                                                                                                 |
| `enumless-closed-set`         | 5     | Including `ReplicationOpsPhase`, which has four constants and no `Enum`                                                                                               |
| `no-phase-no-conditions`      | 4     |                                                                                                                                                                       |
| `conditions-without-listtype` | 3     | All three condition-using types: `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`                                                                            |
| `ops-spec-mutable`            | 1     | `ReplicationOps`. The other two Ops kinds use `+k8s:immutable`                                                                                                        |
| `thin-printcolumns`           | 1     | `Task` has no printcolumns at all                                                                                                                                     |

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
