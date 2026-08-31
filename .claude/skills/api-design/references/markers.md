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

| Marker                                        | Use                                                                                                                          |
|-----------------------------------------------|------------------------------------------------------------------------------------------------------------------------------|
| `+optional`                                   | Every field that is not required. A pointer plus `omitempty` when "unset" differs from "zero"                                |
| `+kubebuilder:validation:Required`            | The fields without which the object is meaningless                                                                           |
| `+kubebuilder:validation:Enum=A;B;C`          | Every closed set, and every action verb. Values are PascalCase unless they name something outside this group (`ext4`, `xfs`) |
| `+kubebuilder:default=…`                      | A default the API server applies. It is part of the API: tightening one later is a breaking change                           |
| `+kubebuilder:validation:Minimum` / `Maximum` | Numeric bounds. A vCPU or size floor belongs here, not in a reconciler's error path                                          |
| `+k8s:immutable`                              | Always-immutable fields. controller-gen turns it into `self == oldSelf`, as below                                            |
| `+listType=map` with `+listMapKey=type`       | On a `[]metav1.Condition`, so server-side apply merges by condition type instead of replacing the list                       |

`omitempty` in the JSON tag already makes controller-gen treat a field as
optional, so `+optional` is for saying so out loud. A field with neither, and no
`Required`, gets whichever answer the tag happens to imply, and the checker
reports that as `unspecified-spec-field`.

## Immutability

**Use the marker, and let the field's optionality pick the meaning.**
`// +k8s:immutable` on the field is the whole job for both meanings. It is not
spelled `+kubebuilder:`, which is why it is repeatedly assumed to be inert
documentation. It is not.

It emits two rules, and which of them apply depends on `required`:

```go
// Immutable from creation, because a required field can never be absent.
// +k8s:immutable
// +kubebuilder:validation:Required
ClusterRef string `json:"clusterRef"`

// Immutable once set: fillable later, then neither changeable nor removable.
// +k8s:immutable
// +optional
FabricType string `json:"fabricType,omitempty"`
```

The field-level `self == oldSelf` rejects a change of value. The parent-level
`!has(oldSelf.X) || has(self.X)`, added only for a field outside `required`,
rejects removal, and exists because the field rule cannot fire when the field is
gone. Neither blocks a first assignment, which is what once-set means.

So do **not** hand-write `!has(oldSelf.x) || self.x == oldSelf.x` for the once-set
case: it is longer, and it guards the value without guarding removal. Write CEL
only when the rejection message has to say something the generated one cannot.

A doc comment reading `// Immutable.` is the spelling that enforces nothing.
`references/consistency.md` tabulates them all with the generator source lines,
and `check-crds.py` reports every field that claims immutability in prose without
backing it.

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
