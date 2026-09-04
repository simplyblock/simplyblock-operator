---
name: build-system
description: Drive this repository's make build system: force a regeneration or sync of the kubebuilder manifests (CRDs, RBAC, webhooks), the consolidated installer, the Helm chart, the OpenShift/OLM bundle, the operator and CSI binaries, and the atlas generated client. Use when asked to build, rebuild, regenerate, sync, or force any of those, when a CI drift check fails, or when a generated artifact looks stale.
---

# The make build system

A monorepo of four components, each with its own Makefile, orchestrated from the
root:

| Component   | Directory      | Own Makefile | Role                                                                       |
|-------------|----------------|--------------|----------------------------------------------------------------------------|
| atlas-lib   | `atlas-lib/`   | yes          | Shared Go library, generated control-plane API client                      |
| csi-driver  | `csi-driver/`  | yes          | The `spdkcsi` CSI driver binary and image                                  |
| operator    | `operator/`    | yes          | The kubebuilder operator: manifests, manager binary, installer, OLM bundle |
| helm-charts | `helm-charts/` | **no**       | Chart sources, fed by a sync script and driven from the root Makefile      |

`make help` at the root lists the orchestration targets, and `make -C operator help`
lists the operator's own, grouped by category. The root Makefile is
`.NOTPARALLEL`: it runs component targets serially and lets each component
manage its own parallelism.

Reference material:

- `references/targets.md`: every target, what it runs, what it depends on, and
  the variables that change its behavior.
- `references/artifacts.md`: every generated artifact, with its source, the command
  that regenerates it, the CI gate that catches it when stale, and how to force
  it when make thinks it is current.

## The first question: is the artifact generated or built?

|               | Generated (committed)                                                                                                                                                                                                                 | Built (ignored)                                                                            |
|---------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------|
| Examples      | `operator/config/crd/bases/*.yaml`, `operator/config/rbac/role.yaml`, `zz_generated.deepcopy.go`, `operator/dist/install.yaml`, `helm-charts/charts/simplyblock-operator/{crds,templates/roles}`, `atlas-lib/internal/cpapi/*.gen.go` | `operator/build/manager`, `csi-driver/build/spdkcsi`, container images, `operator/bundle/` |
| Lives in git  | **yes,** a stale copy is a review-visible defect                                                                                                                                                                                      | no (`.gitignore`: `build/`, `.bin/`)                                                       |
| CI enforces   | yes, `make <target>` then `git diff --exit-code`                                                                                                                                                                                      | only that it compiles                                                                      |
| After running | **commit the diff**                                                                                                                                                                                                                   | nothing to commit                                                                          |

Generated-and-committed artifacts are the ones worth forcing. The rule the CI
gates encode: *running the generator must produce no diff.*

## Forcing the four things that get asked for

### Kubebuilder manifests: CRDs, RBAC, webhooks

```bash
make -C operator manifests generate      # or: make operator-manifests
```

`manifests` runs `controller-gen` for `rbac:roleName=manager-role`,
`crd:allowDangerousTypes=true`, and `webhook` over `./...`, writing CRDs to
`config/crd/bases`. `generate` writes the `zz_generated.deepcopy.go` files.
Both are phony, so **they always re-run** and there is nothing to force. What
appears stale is almost always one of:

- a marker that was edited without re-running the generator (just run it),
- a type that is not reachable from `./...` (check the package is imported),
- an enum or default expressed in a comment rather than a `+kubebuilder:` marker.

Verify the way CI does:

```bash
make -C operator manifests generate && git diff --exit-code
```

### The Helm chart

```bash
make helm-sync                # from the repo root only
```

`helm-sync` depends on `operator-manifests`, so the chart is always fed freshly
generated sources, then `helm-charts/scripts/sync-from-operator.sh` copies and
rewrites them:

- CRDs → `charts/simplyblock-operator/crds/` (and stale ones are deleted),
- `config/rbac/*_role.yaml`, `*_role_binding.yaml` → `templates/roles/`, with
  `managed-by: kustomize` rewritten to `Helm` and a name typo repaired,
- `config/rbac/role.yaml` → `templates/roles/manager_role.yaml`, renamed to
  `simplyblock-operator-manager-role`,
- `kustomize build config/webhook` → `templates/simplyblock-operator-webhook.yaml`,
  wrapped in `{{- if .Values.operator.enabled }}`, names and namespace rewritten
  to the Helm equivalents of the `kustomize` `namePrefix`.

The webhook is built with `kustomize` rather than copied, so kustomize-only
patches survive, notably the `matchConditions` that scope the pinned-volume
validator, which controller-gen markers cannot express. The script prefers
`.bin/kustomize` and falls back to whatever `kustomize` is on `PATH`.

Verify the way CI does:

```bash
make helm-sync && git diff --exit-code -- helm-charts
```

Chart **packaging** is not part of `make`: the release workflow runs
`helm package` and `helm repo index` when
`helm-charts/charts/simplyblock-operator/Chart.yaml` changes. Bumping
`version:` in `Chart.yaml` is what triggers a chart release.

### The installer and the OpenShift/OLM bundle

```bash
make -C operator build-installer         # dist/install.yaml, committed
make -C operator bundle                  # bundle/, needs a pushed image
```

`build-installer` regenerates the manifests, then `kustomize build config/default`
into `dist/install.yaml`.

`bundle` is the OpenShift path and is the one target that reaches outside the
repository. Before running it, know these four things:

1. **The image must already be pushed.** The recipe `curl`s quay.io for the
   digest of the operator, cluster, SPDK, and rebalancer images and fails with
   "could not fetch digest, is the image pushed?" when one is missing. It pins
   digests, not tags, for airgap `relatedImages`.
2. **`operator-sdk` must be on `PATH`:** it is not one of the pinned `.bin`
   tools. CI installs v1.42.2 with a checksum check.
3. **`kustomize` must be on `PATH` too.** The recipe calls bare `kustomize`
   (unlike the rest of the Makefile, which uses `$(KUSTOMIZE)` from `.bin`). See
   the traps below.
4. **`OPENSHIFT_VERSION`** (default `v4.19`) is written into
   `bundle/metadata/annotations.yaml` as `com.redhat.openshift.versions`.

`bundle-build` and `bundle-push` wrap it into `$(BUNDLE_IMG)`.

### The binaries

```bash
make -C operator build                   # build/manager (host platform)
make -C csi-driver spdkcsi               # build/spdkcsi (always GOOS=linux)
make -C atlas-lib build
make build                               # every component + helm-sync
```

`make -C operator build` and `test` both depend on `manifests generate fmt vet`,
so **a build can change tracked files**. The CSI binary is always cross-compiled
for `linux` with `CGO_ENABLED=0`. On macOS the resulting binary does not run
locally, and that is intended.

`atlas-lib` is the one component with real file targets rather than phony ones:
`internal/cpapi/cpapi.gen.go` and `validation.gen.go` rebuild only when
`../shared/openapi.json`, the codegen config, or the generator
source is newer. To force them:

```bash
rm atlas-lib/internal/cpapi/*.gen.go && make -C atlas-lib generate
```

## Traps

- **`build-installer`, `deploy`, and `bundle` mutate a tracked file.** Each runs
  `kustomize edit set image controller=...`, which rewrites
  `operator/config/manager/kustomization.yaml`. Running one with a custom `IMG`
  or `IMG_TAG` leaves that pin in the working tree. Check
  `git diff operator/config/manager/kustomization.yaml` afterward and revert it
  unless the change is intended.
- **`make bundle` escapes the version pinning.** The recipe calls bare
  `kustomize`, not `$(KUSTOMIZE)`, so it uses whatever is on `PATH`, such as a Homebrew
  `kustomize` on a developer machine, which is very likely a different version
  than the `.bin` pin. Prefix the run with `PATH="$PWD/.bin:$PATH"` to get the
  pinned one. The release workflow puts `operator/bin` on `PATH` for this, but
  the pinned tools install into the repo-root `.bin`, so that line does nothing
  and CI silently depends on the runner image's `kustomize`. Treat it as a latent
  bug, not as a working example.
- **`make -C operator test` regenerates and formats.** It is not a read-only
  operation, so run it before inspecting `git status`, not after.
- **Chart files are outputs, not sources.** Anything under
  `helm-charts/charts/simplyblock-operator/{crds,templates/roles}` and the
  webhook template is overwritten by the next `make helm-sync`. Fix the wording
  or the schema in the operator's markers and types, then sync. The
  `house-style` gates exclude exactly those three paths for that reason, while
  checking the rest of the development chart, which is hand-written source.
- **`.bin` holds tools no target references.** `buf`, `protoc-gen-go`, and
  `protoc-gen-go-grpc` are left over from prototype work and are not in
  `scripts/tools.manifest`. Do not assume a binary in `.bin` is part of the
  build.
- **Two `bin` conventions.** The kubebuilder scaffold's `operator/bin` is not
  used. Every pinned tool lives in the repo-root `.bin` via
  `scripts/tools.mk`. A recipe referring to `$(LOCALBIN)` means `.bin` as well.
- **Deleting `.bin` is safe but not free.** `scripts/tools.sh install` re-downloads
  and re-verifies against `scripts/tools.lock`, which needs network access.

## Forcing a tool reinstall

Tool versions are pinned in `scripts/tools.manifest` with checksums in
`scripts/tools.lock`, installed version-suffixed into `.bin` with a stable
symlink. `tools.sh install` is idempotent: it stats the pinned version and
returns. To force one:

```bash
rm -f .bin/kustomize .bin/kustomize-*        # then any target that needs it
scripts/tools.sh install kustomize           # or directly
scripts/tools.sh version kustomize           # what the manifest pins
```

Bumping a version in the manifest triggers a reinstall on its own, because the
new version is missing from `.bin`.

## The workflow to follow

1. **Identify which artifact is stale**, using the table in
   `references/artifacts.md`. A symptom in a cluster (a missing CRD field, an
   RBAC denial, a webhook not firing) usually points at exactly one.
2. **Run the generator from the directory the target expects.** `helm-sync` only
   exists at the root, while `manifests`, `build-installer`, and `bundle` are operator
   targets (reachable from the root as `operator-*`).
3. **Verify with `git diff --exit-code`** the way CI does, per artifact.
4. **Commit the regenerated files** with the change that caused them. A
   generated artifact in a separate commit is what makes drift hard to review.
5. **Check for side effects:** `git status` for a mutated
   `config/manager/kustomization.yaml`, and for `cover.out` or `dist/` changes
   nobody asked for.
