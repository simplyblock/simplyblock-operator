# Generated Artifacts

One row per artifact: where it comes from, what regenerates it, and which CI job
fails when it is stale. Everything above the horizontal rule is committed, so a
stale copy is a defect a reviewer can see.

## Committed artifacts

| Artifact                                                                              | Generated from                                               | Regenerate with                    | CI gate                                                        |
|---------------------------------------------------------------------------------------|--------------------------------------------------------------|------------------------------------|----------------------------------------------------------------|
| `operator/config/crd/bases/*.yaml`                                                    | `+kubebuilder:` markers on the API types                     | `make -C operator manifests`       | `Operator: Manifests` → `git diff --exit-code`                 |
| `operator/config/rbac/role.yaml` and the other `*_role*.yaml`                         | `+kubebuilder:rbac:` markers                                 | `make -C operator manifests`       | same                                                           |
| `operator/config/webhook/manifests.yaml`                                              | `+kubebuilder:webhook:` markers                              | `make -C operator manifests`       | same                                                           |
| `operator/api/**/zz_generated.deepcopy.go`                                            | the API structs                                              | `make -C operator generate`        | same                                                           |
| `operator/dist/install.yaml`                                                          | `kustomize build config/default`                             | `make -C operator build-installer` | `Operator: Manifests` → `git diff`, then `kubeconform -strict` |
| `helm-charts/charts/simplyblock-operator/crds/*.yaml`                                 | `operator/config/crd/bases`                                  | `make helm-sync`                   | `Operator: Manifests` → `helm-sync` job                        |
| `helm-charts/charts/simplyblock-operator/templates/roles/*.yaml`                      | `operator/config/rbac`                                       | `make helm-sync`                   | same                                                           |
| `helm-charts/charts/simplyblock-operator/templates/simplyblock-operator-webhook.yaml` | `kustomize build operator/config/webhook`                    | `make helm-sync`                   | same                                                           |
| `atlas-lib/internal/cpapi/cpapi.gen.go`                                               | `shared/openapi.json` + `oapi-codegen.yaml` + `overlay.yaml` | `make -C atlas-lib generate`       | compiles in `Atlas: Test`                                      |
| `atlas-lib/internal/cpapi/validation.gen.go`                                          | `validation.yaml` + the generated client                     | `make -C atlas-lib generate`       | same                                                           |

---

## Build output, never committed

| Artifact                                                  | Produced by                                                | Ignored via                                |
|-----------------------------------------------------------|------------------------------------------------------------|--------------------------------------------|
| `operator/build/manager`                                  | `make -C operator build`                                   | `.gitignore: build/`                       |
| `csi-driver/build/spdkcsi`                                | `make -C csi-driver spdkcsi`                               | same                                       |
| `operator/cover.out`, `atlas-lib/coverage.out`            | the `test` targets                                         |                                            |
| `operator/bundle/`                                        | `make -C operator bundle`                                  | packaged as a release asset                |
| `operator/Dockerfile.cross`                               | `docker-buildx` (removed again at the end)                 |                                            |
| `.bin/*`                                                  | `scripts/tools.sh`                                         | `.gitignore: .bin/`                        |
| `helm-charts/charts/<version>/*.tgz`, `charts/index.yaml` | `helm package` + `helm repo index` in the release workflow | committed by the release flow, not by hand |

## Which symptom points at which artifact

| Symptom                                                              | Stale artifact                                                                                | Command                                                         |
|----------------------------------------------------------------------|-----------------------------------------------------------------------------------------------|-----------------------------------------------------------------|
| A CR rejects a field that exists in the Go type                      | CRD schema                                                                                    | `make -C operator manifests`                                    |
| The operator logs a `forbidden` error on a verb it needs             | `role.yaml` (and the chart's `manager_role.yaml`)                                             | `make -C operator manifests && make helm-sync`                  |
| A new CRD is missing after a Helm install                            | chart `crds/`                                                                                 | `make helm-sync`                                                |
| The webhook does not fire, or fires on the wrong object              | chart webhook template (the `matchConditions` patch lives in `kustomize`, not in the markers) | `make helm-sync`                                                |
| `kubectl apply -f dist/install.yaml` lacks a resource                | the installer                                                                                 | `make -C operator build-installer`                              |
| A deepcopy compile error after adding a field                        | `zz_generated.deepcopy.go`                                                                    | `make -C operator generate`                                     |
| An OpenShift install shows the wrong image or misses `relatedImages` | the OLM bundle                                                                                | `make -C operator bundle` (image must be pushed)                |
| A control-plane call sends the wrong field name                      | the atlas client                                                                              | update `shared/openapi.json`, then `make -C atlas-lib generate` |
| A Helm release does not appear in the repo index                     | `Chart.yaml` `version:` was not bumped                                                        | bump it, since the release workflow triggers on that file       |

## Staleness rules that actually bite

- **Phony targets cannot be stale.** `manifests`, `generate`, `build-installer`,
  and `helm-sync` re-run unconditionally. When their output still looks wrong,
  the input is wrong: a missing marker, a type outside `./...`, or an edit to a
  chart file that the sync script overwrites on the next run. Never hand-edit a
  file under `helm-charts/charts/simplyblock-operator/crds/` or
  `templates/roles/`.
- **File targets can be stale.** Only `atlas-lib`'s two `.gen.go` files. Force
  them by deleting them or by touching a prerequisite
  (`touch shared/openapi.json`).
- **`.bin` tools are cached by version.** A pinned version already present is
  never re-downloaded. Delete `.bin/<tool>*` to force it.
- **A generator writing no diff is the success condition**, not a sign that it
  did not run. Confirm with `git diff --exit-code` rather than by watching the
  output.

## The CI contract, in one block

What every drift job does, and what to run locally before pushing:

```bash
# Operator: Manifests → manifests job
make -C operator manifests generate && git diff --exit-code
make -C operator build-installer   && git diff --exit-code
kubeconform -strict -summary \
  -skip CustomResourceDefinition,Certificate,ClusterIssuer \
  operator/dist/install.yaml

# Operator: Manifests → helm-sync job
make helm-sync && git diff --exit-code -- helm-charts
```

`git diff --exit-code` means "no diff, or fail." Both jobs run on every pull
request that touches `operator/**`.

## Release flows, for orientation

- **Operator image and bundle** (`Operator: Release`, manual or on a tag):
  `make docker-buildx` and `make docker-buildx-simplyblock-rebalancer` with
  `VERSION` and `IMG_TAG` from the tag, then `operator-sdk` v1.42.2 is installed,
  then `make bundle`, and `bundle/manifests` plus `bundle/metadata` are zipped
  onto the GitHub release. The bundle step needs the images pushed first, which
  is why it runs after buildx in the same job.
- **Helm chart** (`Helm: Release`, on a change to the chart's `Chart.yaml`):
  `helm package` into `helm-charts/charts/<version>/`, then
  `helm repo index --url https://install.simplyblock.io/helm`.
- **Chart repository merge** (`Helm: Merge Charts`): the operator and CSI chart
  artifacts are merged into one index by
  `helm-charts/scripts/merge_helm_repos.py`.

The chart's `version:` is the release trigger, `appVersion:` is `latest`, and the
component image tags live in `values.yaml`, so a new operator image needs both
the chart `values.yaml` tag and the chart `version:` bumped.
