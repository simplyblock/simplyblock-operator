# The marker set

What each marker is for. **The counts come from the code, not from this page:**

```bash
.claude/skills/api-design/scripts/check-crds.py --adoption
```

The numbers that used to be written out here were wrong within weeks. Anything
below that looks like a count is a rule of thumb, and the script is the fact.

## Type level

| Marker                                      | Use                                                                                                                   |
|---------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `+kubebuilder:object:root=true`             | On every root type and its `List` companion                                                                           |
| `+kubebuilder:subresource:status`           | On every type. Without it, a status write needs spec write permission and bumps `generation`                          |
| `+kubebuilder:resource:scope=…,shortName=…` | Scope is Namespaced unless the object genuinely has no namespace. Declare the short name the design names             |
| `+kubebuilder:printcolumn`                  | Two or three per type, answering "what is happening." For an Ops kind: target, action, phase, sub-phase, message, age |
| `+kubebuilder:validation:XValidation`       | Cross-field and immutability rules that field markers cannot express                                                  |

## Field level

| Marker                                        | Use                                                                                                    |
|-----------------------------------------------|--------------------------------------------------------------------------------------------------------|
| `+optional`                                   | Every field that is not required. A pointer plus `omitempty` when "unset" differs from "zero"          |
| `+kubebuilder:validation:Required`            | The fields without which the object is meaningless                                                     |
| `+kubebuilder:validation:Enum=a;b;c`          | Every closed set, and every action verb                                                                |
| `+kubebuilder:default=…`                      | A default the API server applies. It is part of the API: tightening one later is a breaking change     |
| `+kubebuilder:validation:Minimum` / `Maximum` | Numeric bounds. A vCPU or size floor belongs here, not in a reconciler's error path                    |
| `+k8s:immutable`                              | Always-immutable fields. controller-gen turns it into `self == oldSelf`, as below                      |
| `+listType=map` with `+listMapKey=type`       | On a `[]metav1.Condition`, so server-side apply merges by condition type instead of replacing the list |

`omitempty` in the JSON tag already makes controller-gen treat a field as
optional, so `+optional` is for saying so out loud. A field with neither, and no
`Required`, gets whichever answer the tag happens to imply, and the checker
reports that as `unspecified-spec-field`.

## Immutability

**Start with the marker.** `// +k8s:immutable` on the field is the whole job for
anything that is immutable from creation: controller-gen emits
`x-kubernetes-validations: rule: self == oldSelf` into the schema. It is not
spelled `+kubebuilder:`, which is why it is repeatedly assumed to be inert
documentation. It is not.

Reach for CEL when `self == oldSelf` is not what is meant. Two shapes, both CEL,
one on the field and one on the type:

```go
// Field: immutable from creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetCluster is immutable"

// Type: immutable once set, so it may be filled in later but never changed.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.fabricType) || self.fabricType == oldSelf.fabricType",message="fabricType is immutable once set"
```

Use the first for anything that identifies what the object is about: a target
reference, an action verb, or a pool or cluster binding. Use the second for
configuration that a controller or a user fills in once, where an empty value is
a legitimate starting state (`fabricType`, `stripe`, a KMS binding).

A doc comment reading `// Immutable.` is the fourth spelling and the only one
that enforces nothing. `references/consistency.md` tabulates all four, and
`check-crds.py` reports every field that claims immutability in prose without
backing it with one of the first three.

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
`config/crd/kustomization.yaml`, which is hand-maintained, and a CRD missing
from it never reaches the installer or the chart. See the `build-system` and
`new-files` skills.

The generated schema is the transcript, not the source. Never edit
`config/crd/bases/*.yaml` or the chart's `crds/` copies: fix the marker and
regenerate.
