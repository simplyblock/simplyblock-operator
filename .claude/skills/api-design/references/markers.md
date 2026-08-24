# The marker set

What each marker is for, and what this repository currently does with it. Counts
are over `operator/api/v1alpha1`.

## Type level

| Marker | Count | Use |
|---|---|---|
| `+kubebuilder:object:root=true` | 34 | On every root type and its `List` companion |
| `+kubebuilder:subresource:status` | 17 | On every type. Without it, a status write needs spec write permission and bumps `generation` |
| `+kubebuilder:resource:scope=…,shortName=…` | 0 short names | Scope is Namespaced unless the object genuinely has no namespace. **Three designs name a short name that no type declares** (`scops`, `relpair`, `repl`) |
| `+kubebuilder:printcolumn` | 83 | Two or three per type, answering "what is happening" — for an Ops kind: target, action, phase, sub-phase, message, age |
| `+kubebuilder:validation:XValidation` | 10, in 4 types | Cross-field and immutability rules that field markers cannot express |

## Field level

| Marker | Count | Use |
|---|---|---|
| `+optional` | 173 | Every field that is not required. A pointer plus `omitempty` when "unset" differs from "zero" |
| `+kubebuilder:validation:Required` | 17 | The fields without which the object is meaningless |
| `+kubebuilder:validation:Enum=a;b;c` | 18 | Every closed set, and every action verb |
| `+kubebuilder:default=…` | 17 | A default the API server applies. It is part of the API: tightening one later is a breaking change |
| `+kubebuilder:validation:Minimum` / `Maximum` | 7 | Numeric bounds. A vCPU or size floor belongs here, not in a reconciler's error path |

## Immutability

Two shapes, both CEL, one on the field and one on the type:

```go
// Field: immutable from creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetCluster is immutable"

// Type: immutable once set, so it may be filled in later but never changed.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.fabricType) || self.fabricType == oldSelf.fabricType",message="fabricType is immutable once set"
```

Use the first for anything that identifies what the object is about — a target
reference, an action verb, a pool or cluster binding. Use the second for
configuration that a controller or a user fills in once, where an empty value is
a legitimate starting state (`fabricType`, `stripe`, a KMS binding).

`backuprestore_types.go`, `replicationpair_types.go`, `storagecluster_types.go`,
and `storagenodeset_types.go` are the four types that enforce anything today.

## Cross-field rules worth writing

CEL runs at admission, which is the cheapest place to say no:

```go
// A parameter that only applies to one action.
// +kubebuilder:validation:XValidation:rule="self.action != 'migrate' || has(self.targetWorkerNode)",message="targetWorkerNode is required for action=migrate"

// Two fields that must agree.
// +kubebuilder:validation:XValidation:rule="!has(self.stripeCount) || self.stripeCount <= self.memberLimit",message="stripeCount cannot exceed memberLimit"
```

Every rule that lands here is a runtime error path the reconciler no longer needs
and a test that becomes an admission scenario instead of a reconcile scenario.

## What generation does with all of this

`make -C operator manifests` turns these markers into the CRD schema, and
`make helm-sync` copies the result into the chart. Neither adds the new CRD to
`config/crd/kustomization.yaml` — that list is hand-maintained, and a CRD missing
from it never reaches the installer or the chart. See the `build-system` and
`new-files` skills.

The generated schema is the transcript, not the source. Never edit
`config/crd/bases/*.yaml` or the chart's `crds/` copies: fix the marker and
regenerate.
