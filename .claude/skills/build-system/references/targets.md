# Target Reference

Every target, what it actually runs, and the variables that change it. Read the
Makefile when in doubt, because this is a map, not a substitute.

## Root Makefile

Delegates to the components with `$(MAKE) -C`. `.NOTPARALLEL` is set, so the
prerequisites run serially. `make` with no target prints the help.

| Target                                 | Runs                                                                                  |
|----------------------------------------|---------------------------------------------------------------------------------------|
| `all`                                  | `build test`                                                                          |
| `build`                                | `atlas-build csi-build operator-build-installer operator-build helm-sync`             |
| `test`                                 | `atlas-test csi-test operator-test`                                                   |
| `lint` / `fmt` / `vet`                 | the matching target in atlas-lib, csi-driver, operator                                |
| `atlas`, `csi`, `operator`             | build plus test for that component (`operator` also runs manifests and the installer) |
| `atlas-build\|test\|lint\|fmt\|vet`    | `make -C atlas-lib <t>`                                                               |
| `csi-build`                            | `make -C csi-driver spdkcsi` (note the target rename)                                 |
| `csi-test\|lint\|fmt\|vet`             | `make -C csi-driver <t>`                                                              |
| `operator-manifests`                   | `make -C operator manifests`                                                          |
| `operator-build-installer`             | `make -C operator build-installer`                                                    |
| `operator-build\|test\|lint\|fmt\|vet` | `make -C operator <t>`                                                                |
| `helm-sync`                            | `operator-manifests`, then `bash helm-charts/scripts/sync-from-operator.sh operator`  |
| `configure`                            | `bash scripts/local-config.sh detect $(CONFIGURE_ARGS)`, which records what it finds  |
| `openapi-diff`                         | export the spec to a temporary file, then summarize it against the committed one      |
| `openapi-sync`                         | export over `shared/openapi.json`, then `make -C atlas-lib generate` and the suite's  |
| `openapi-ref`                          | print which sbcli branch and commit the export would read                             |

`helm-sync` exists **only** at the root, and there is no `helm-charts/Makefile`.

The `openapi-*` targets export the control-plane spec from a local sbcli
checkout. `SBCLI_DIR` locates it and **has no default**, because where another
repository is checked out is not something this one can know: it is read from
the environment or from an untracked `local.mk` (`-include`d at the top of the
root Makefile), and failing that from `.sbcli` inside the repository, which is
where CI checks sbcli out and which `.gitignore` covers. Configured nowhere, the
targets print the ways to set it and stop.

They build a virtual environment under `.bin/openapi-venv` on first use, from
`$(SBCLI_DIR)/requirements.txt`, because the export imports the control plane's
own FastAPI app; it is rebuilt when that requirement set moves, and
`VENV_INTERPRETER` overrides the interpreter it is built with. Nothing connects
to a database at import time.

**The settings that belong to one machine are named by a section and a name,**
and `scripts/local-config.sh` is the only thing that reads or writes them:

```
scripts/local-config.sh repo sbcli set ../sbcli   # record a setting
scripts/local-config.sh repo sbcli get            # print it, or nothing
scripts/local-config.sh repo sbcli unset          # forget it
scripts/local-config.sh list [<section>]          # print every setting
scripts/local-config.sh detect [--clone]          # find sbcli and record it
```

Two are read today: `repo.sbcli`, the path to an sbcli checkout, and
`venv.interpreter`, what the openapi environment is built with. They are stored
in the untracked `local.mk` as one make variable each (`repo.sbcli` becomes
`REPO_SBCLI`), which is where the Makefile reads them from and is otherwise this
script's business. Writing one rewrites its own assignment and keeps every other
line, so settings do not overwrite each other.

`detect` is what `make configure` runs. It looks in `.sbcli`, beside the
repository, and under `~/git`, `~/src`, `~/dev`, and `~/projects`, reports the
branch the checkout it picked is on, and takes `--clone` to fetch one into
`.sbcli` when there is none. Run from a linked worktree it searches beside the
main checkout as well, since a worktree lives inside the repository and its own
neighbors are the other worktrees. Pass that flag through make as `CONFIGURE_ARGS`.
Recording an sbcli path directly checks that it is a checkout, so a directory
that merely has the right name is refused rather than recorded.

Changing `venv.interpreter` after the environment exists does not rebuild it,
since the stamp tracks the requirement set rather than the interpreter. Remove
`.bin/openapi-venv` to switch interpreters.

**Which ref the checkout is on is the whole meaning of the export,** so both
targets print it first. A sibling checkout sits on whatever branch it was last
left on, and exporting from a release branch removes the endpoints that exist
only on `main` — `openapi-diff` writes nothing and is the way to see that before
`openapi-sync` commits to it. CI does the same export nightly from `main` and
proposes the drift as a pull request (`.github/workflows/repo_openapi_sync.yaml`).

## operator/Makefile

Kubebuilder scaffold, extended. Includes `../scripts/tools.mk`. `SHELL` is bash
with `-o pipefail` and `.SHELLFLAGS = -ec`, so a failing pipe stage fails the
recipe.

### Development

| Target                            | Runs                                                                                                                | Notes                                                     |
|-----------------------------------|---------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------|
| `manifests`                       | `controller-gen rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..."`                      | CRDs to `config/crd/bases`, phony, always re-runs         |
| `generate`                        | `controller-gen object:headerFile=hack/boilerplate.go.txt paths="./..."`                                            | the `zz_generated.deepcopy.go` files                      |
| `fmt` / `vet`                     | `go fmt ./...` / `go vet ./...`                                                                                     |                                                           |
| `test`                            | `manifests generate fmt vet setup-envtest`, then `go test $(go list ./... \| grep -v /e2e) -coverprofile cover.out` | writes `cover.out`, **regenerating and formatting first** |
| `setup-test-e2e`                  | creates the Kind cluster `$(KIND_CLUSTER)` if absent                                                                | needs `kind` on `PATH`                                    |
| `test-e2e`                        | `go test -tags=e2e ./test/e2e/`, then `cleanup-test-e2e`                                                            |                                                           |
| `lint`, `lint-fix`, `lint-config` | pinned `golangci-lint`                                                                                              |                                                           |

`ENVTEST_VERSION` and `ENVTEST_K8S_VERSION` are derived from `go.mod`
(controller-runtime's release branch, `k8s.io/api`'s minor) rather than pinned.
The envtest assets land in `.bin` (`LOCALBIN := $(BIN_DIR)`).

### Build

| Target                                | Runs                                                                                                             | Notes                                                                            |
|---------------------------------------|------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| `build`                               | `manifests generate fmt vet`, then `go build -o build/manager cmd/main.go`                                       | host platform                                                                    |
| `run`                                 | same prerequisites, then `go run ./cmd/main.go`                                                                  | against the current kubeconfig                                                   |
| `docker-build` / `docker-push`        | `$(CONTAINER_TOOL) build -f Dockerfile ..` / push `$(IMG)`                                                       | build context is the **repo root**, both modules are needed                      |
| `docker-buildx` / `docker-buildx-ecr` | multi-arch build and push of `$(IMG)` / `$(ECR_IMG)`                                                             | rewrites the first `FROM` into `Dockerfile.cross`, creates and removes a builder |
| `docker-*-simplyblock-rebalancer`     | the same four shapes for `Dockerfile.simplyblock-rebalancer`                                                     |                                                                                  |
| `build-installer`                     | `manifests generate kustomize`, `kustomize edit set image`, `kustomize build config/default > dist/install.yaml` | **mutates `config/manager/kustomization.yaml`**                                  |

### Deployment

| Target                         | Runs                                                                                                                                                          | Notes                                                         |
|--------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------|
| `bundle`                       | `operator-sdk generate kustomize manifests`, digest lookups on quay.io, `kustomize build config/manifests \| operator-sdk generate bundle`, then `yq` patches | needs pushed images, `operator-sdk` and `kustomize` on `PATH` |
| `bundle-build` / `bundle-push` | `docker build -f bundle.Dockerfile -t $(BUNDLE_IMG)` / push                                                                                                   |                                                               |
| `install` / `uninstall`        | `kustomize build config/crd` applied or deleted                                                                                                               | skips cleanly when the build is empty                         |
| `deploy` / `undeploy`          | `kustomize edit set image`, `kustomize build config/default` applied or deleted                                                                               | `deploy` **mutates `config/manager/kustomization.yaml`**      |

`uninstall` and `undeploy` accept `ignore-not-found=true`.

### Variables

| Variable                                                     | Default                                                      | Effect                                                    |
|--------------------------------------------------------------|--------------------------------------------------------------|-----------------------------------------------------------|
| `VERSION`                                                    | `0.1.0`                                                      | the bundle version, and the default `IMG_TAG`             |
| `IMG_BASE` / `IMG_TAG` / `IMG`                               | `quay.io/simplyblock-io/simplyblock-operator` / `$(VERSION)` | the operator image                                        |
| `ECR_IMG_BASE` / `ECR_IMG`                                   | `public.ecr.aws/simply-block/simplyblock-operator`           | the ECR mirror                                            |
| `REBALANCER_IMG_BASE` / `REBALANCER_IMG`, `ECR_REBALANCER_*` | quay / ECR `simplyblock-rebalancer`                          | the rebalancer image                                      |
| `BUNDLE_IMG`                                                 | `…/simplyblock-operator-bundle:$(VERSION)`                   | the OLM bundle image                                      |
| `OPENSHIFT_VERSION`                                          | `v4.19`                                                      | `com.redhat.openshift.versions` in the bundle annotations |
| `CLUSTER_IMAGE_BASE` / `CLUSTER_IMAGE_TAG`                   | `quay.io/simplyblock-io/simplyblock` / `26.2.6-PRE`          | pinned by digest into `relatedImages`                     |
| `SPDK_IMAGE_BASE` / `SPDK_IMAGE_TAG`                         | `quay.io/simplyblock-io/ultra` / `R26.2-PRE-latest`          | same                                                      |
| `CONTAINER_TOOL`                                             | `docker`                                                     | set to `podman` to switch                                 |
| `PLATFORMS`                                                  | `linux/arm64,linux/amd64`                                    | buildx targets                                            |
| `KUBECTL`, `KIND`                                            | `kubectl`, `kind`                                            | expected on `PATH`, never downloaded                      |
| `KIND_CLUSTER`                                               | `simplyblock-operator-test-e2e`                              | the e2e Kind cluster                                      |

## csi-driver/Makefile

Includes `../scripts/tools.mk`. Default goal `all` is `spdkcsi lint test`.

| Target                                       | Runs                                                                                         | Notes                                                                                         |
|----------------------------------------------|----------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------|
| `spdkcsi`                                    | `CGO_ENABLED=0 GOARCH=$(GOARCH) GOOS=linux go build -buildvcs=false -o build/spdkcsi ./cmd/` | **always Linux**, whatever the host                                                           |
| `lint` → `golangci`                          | pinned `golangci-lint run ./...`                                                             |                                                                                               |
| `fmt`, `vet`                                 | `go fmt` / `go vet`                                                                          |                                                                                               |
| `yamllint`, `shellcheck`, `mdl`, `codespell` | skip with a message when the tool is absent (`mdl` and `codespell` fail instead)             | not part of `lint`                                                                            |
| `test`                                       | `mod-check unit-test`                                                                        | `go mod verify`, then `go test -race -cover` over `cmd` and `pkg`. `SKIP_TESTS=<regex>` skips |
| `e2e-test`                                   | `go test -race -timeout 30m ./e2e`, or ginkgo with `E2E_PROCS=N`                             | `E2E_TEST_ARGS` passes ginkgo flags                                                           |
| `helm-test`                                  | `scripts/install-helm.sh` up, install, cleanup, clean                                        |                                                                                               |
| `image`                                      | `sudo docker build -f deploy/image/Dockerfile ..` plus an `arm64` buildx pass                | uses `sudo` and `apt-get`. **Linux only**                                                     |
| `clean`                                      | removes the binary, `go clean -testcache`                                                    |                                                                                               |

Variables: `GOARCH` (host default), `CSI_IMAGE_REGISTRY` (`simplyblock`),
`CSI_IMAGE_TAG` (`latest`), `HTTP_PROXY`.

## atlas-lib/Makefile

Includes `../scripts/tools.mk`. Default goal `all` is `build test`. The only
component with **file targets**, so make's staleness rules apply.

| Target                                        | Runs                                                                                                         |
|-----------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| `generate`                                    | every generated file below, when out of date                                                                 |
| `internal/cpapi/cpapi.gen.go`                 | `go generate ./internal/cpapi/...`, depending on `../shared/openapi.json`, `oapi-codegen.yaml`, and `gen.go` |
| `internal/cpapi/validation.gen.go`            | `cd internal/cpapi && go run ./gen`, depending on the client, `validation.yaml`, `gen/main.go`               |
| `link/linkv1/link.pb.go`                      | `buf generate` in that directory, depending on `link.proto`, `buf.gen.yaml`, `buf.yaml`                      |
| `storage/storagerpc/storagev1/nvme.pb.go`     | `buf generate` in that directory, depending on `nvme.proto`, `buf.gen.yaml`, `buf.yaml`                      |
| `*_grpc.pb.go`                                | nothing of its own: each depends on its `.pb.go`, whose one recipe writes both                                |
| `build`                                       | the generated files, then `go build ./...`                                                                   |
| `test`                                        | `vet`, then `go test -race -coverprofile=coverage.out ./...`                                                 |
| `lint`                                        | pinned `golangci-lint run ./...`                                                                             |
| `lint-proto`                                  | `buf lint` in each protocol directory. **Not run by `lint`, and not run in CI**                              |
| `vet`, `fmt`                                  | `go vet` / `go fmt`                                                                                          |

The `oapi-codegen` version is pinned in `go.mod` through
`internal/cpapi/tools.go`, not in the tool manifest. The protobuf toolchain is
the other way round: `buf`, `protoc-gen-go`, and `protoc-gen-go-grpc` are pinned
in `scripts/tools.manifest` precisely so they stay out of `go.mod`. `buf` is an
order-only prerequisite of the `.pb.go` rules, so it installs on demand and its
timestamp never makes the outputs look stale. The recipes prepend `$(BIN_DIR)`
to `PATH` because `buf` finds the two generators there rather than being told
where they are.

## scripts/tools.mk and scripts/tools.sh

`tools.mk` resolves the repo root from its own path, so it works from any
component directory. It exposes `$(GOLANGCI_LINT)`, `$(KUSTOMIZE)`,
`$(CONTROLLER_GEN)`, `$(ENVTEST)`, and `$(YQ)`, all under `$(BIN_DIR)` = `.bin`,
and one phony install target per tool that defers to `tools.sh`. No versions live
in the fragment: `scripts/tools.manifest` is the single source of truth, with
`scripts/tools.lock` holding the checksums. `setup-envtest` is the exception:
its version comes from the caller (`$(ENVTEST_VERSION)`, derived from `go.mod`).

```bash
scripts/tools.sh install <tool> [version]   # idempotent
scripts/tools.sh version <tool>             # the manifest pin
scripts/tools.sh lock    <tool> [version]   # record checksums for all platforms
```

Binaries are stored version-suffixed with a stable symlink, so switching pins is
atomic. Environment: `TOOLS_BIN_DIR`, `TOOLS_LOCK_FILE`, `TOOLS_MANIFEST_FILE`,
`TOOLS_STRICT=1` (fail rather than warn on a missing hash).
