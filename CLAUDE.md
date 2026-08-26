# simplyblock-operator

A monorepo of four components. `atlas-lib` is the shared library, and the two
consumers depend on it through a `replace` directive rather than a published
version:

| Component   | Directory      | Role                                                                       |
|-------------|----------------|----------------------------------------------------------------------------|
| atlas-lib   | `atlas-lib/`   | Shared library: node-level storage primitives and the control-plane client |
| operator    | `operator/`    | The kubebuilder operator: CRDs, reconcilers, webhooks                      |
| csi-driver  | `csi-driver/`  | The `spdkcsi` CSI driver                                                   |
| helm-charts | `helm-charts/` | Chart sources; `charts/simplyblock-operator` is the development chart      |

## Shared code lives in atlas-lib

**Before writing a helper, look in `atlas-lib`.** It exists because the operator
and the CSI driver kept solving the same problems separately, and a second
implementation of a primitive is worse than a slightly awkward dependency on the
first: the two copies drift, and a fix lands in one of them.

Two places answer whether it is there already, and neither is memory:

- **`atlas-lib/README.md`:** the worked flows the operator and the CSI driver
  actually perform, each with its idiomatic call sequence, a file-level index of
  every package, and a *Today* note pointing at the live call site where a
  pattern is already wired. Start here — how a thing is done in this repository
  is the question behind most new helpers.
- **The package index**, read from the code:

  ```bash
  go doc github.com/simplyblock/atlas          # the overview and its package list
  (cd atlas-lib && go list ./...)              # every package, including internal
  go doc github.com/simplyblock/atlas/nvmeof   # one package in detail
  ```

NVMe discovery, NVMe-oF connection handling, NQN construction, lvol identity and
device mapping, the control-plane client, error classification, Kubernetes object
correlation, locks, state machines, and URL validation are all there already.

**When new functionality is shared, or could be, it belongs in `atlas-lib` too** —
not in one consumer with a copy waiting to happen in the other. The test is what
the code is about, not who happens to need it today: a node-level or
control-plane-level primitive belongs in `atlas-lib`, while Kubernetes-shaped
logic (reconcilers, CR status, webhook admission) belongs in the consumer.
Public package under `atlas-lib/<concern>/` when a consumer imports it, under
`atlas-lib/internal/` when it does not. Never a new Go module: both consumers
already carry `replace github.com/simplyblock/atlas => ../atlas-lib`, and a third
module would need that wiring everywhere.

## Tests are written first and proven red before the fix

For every bug fix and every behavior change:

1. **Write the test first**, describing the behavior that is wanted.
2. **Run it and show it fail** — the actual failure output, against the unchanged code.
   A test that was never red proves nothing: it may be asserting what the code already did,
   or asserting nothing at all.
3. **Then** write the fix, and run the test again to show it green.
4. Run the full suite / quality gate before reporting the work done.

The red run is not a formality — it is the only evidence that the test covers the defect.
If a test cannot be made red first (it covers something already correct, or the failure only
reproduces on a cluster), say so explicitly rather than skipping the step silently.

## House rules that hold in every edit

- **No license headers in new files:** no Apache block, no copyright line, no
  SPDX identifier. Every file opens with a comment saying what is in it and why
  it lives there.
- **American English, the Oxford comma, and `simplyblock` lowercase:** in prose,
  in comments, and in identifiers. `.claude/skills/house-style/scripts/quality-gate.sh --changed`
  checks a change before it is handed back.
- **Regenerate what a change invalidates.** Editing a `+kubebuilder:` marker or an
  API type means `make -C operator manifests generate`, and `make helm-sync` when
  CRDs or RBAC moved. CI fails on the diff otherwise.

## Skills

Load the one that matches the task; each carries the detail this file leaves out.

| Skill                  | For                                                                             |
|------------------------|---------------------------------------------------------------------------------|
| `brainstorming`        | Exploring an idea before anything is built; ends in a design or a no            |
| `new-files`            | Creating a file: separation of concerns, the opening comment, where it goes     |
| `house-style`          | Writing or fixing prose, comments, and names; resolving a gate finding          |
| `build-system`         | Building, regenerating, syncing the chart, the OLM bundle, drift failures       |
| `design-doc`           | A design document under `operator/docs/designs` and its test plan               |
| `test-scenarios`       | Enumerating test scenarios: paired positive and negative, across topologies     |
| `regression-test`      | Fixing a bug: the failing test comes first, red for the bug's reason            |
| `reconciler-patterns`  | Writing or reviewing a controller: phases, never blocking, generations, locks   |
| `api-design`           | Adding or changing a CRD, and auditing the API for consistency                  |
| `code-cleanup`         | Cleaning up, refactoring, modernizing, or restructuring code that already works |
| `extract-to-atlas-lib` | Moving a shared primitive into `atlas-lib` and deleting both copies             |
| `work-plan`            | Splitting a settled design into issue-ready, dependency-ordered work items      |
| `rbac-hardening`       | Granting or auditing Kubernetes permissions and workload privilege              |
