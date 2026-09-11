# Reusable Makefile fragment: downloads pinned dev tools into the repo-root
# .bin directory (shared by every component). Include it from a component
# Makefile with:
#
#   include ../scripts/tools.mk
#
# It exposes tool path variables ($(GOLANGCI_LINT), $(KUSTOMIZE), ...) and one
# install target per tool that lazily fetches the pinned version through
# scripts/tools.sh. A component only lists the tools it actually uses as
# prerequisites; the other targets stay dormant.
#
# There are deliberately no version pins here: the pinned version of each tool
# lives in scripts/tools.manifest (single source of truth). The install targets
# omit the version so tools.sh resolves it from the manifest. The one exception
# is setup-envtest, whose version the operator derives from its go.mod and
# passes via $(ENVTEST_VERSION).

# Resolve the repo root from this fragment's own path so BIN_DIR / TOOLS_SH are
# absolute no matter which component directory `make` runs in.
TOOLS_MK  := $(lastword $(MAKEFILE_LIST))
REPO_ROOT := $(abspath $(dir $(TOOLS_MK))..)
BIN_DIR   := $(REPO_ROOT)/.bin
TOOLS_SH  := $(REPO_ROOT)/scripts/tools.sh

# ── Tool binaries (stable symlinks maintained by scripts/tools.sh) ───────────
GOLANGCI_LINT  ?= $(BIN_DIR)/golangci-lint
# The same linter with this repository's own rule compiled in, built by
# `golangci-lint custom` from .custom-gcl.yml. The downloaded binary does not
# know the onelinefunc linter and rejects a config that enables it, so this is
# what `make lint` runs.
CUSTOM_GCL     ?= $(BIN_DIR)/custom-gcl
KUSTOMIZE      ?= $(BIN_DIR)/kustomize
CONTROLLER_GEN ?= $(BIN_DIR)/controller-gen
ENVTEST        ?= $(BIN_DIR)/setup-envtest
OPENAPI_GEN    ?= $(BIN_DIR)/openapi-gen
YQ             ?= $(BIN_DIR)/yq
BUF            ?= $(BIN_DIR)/buf

# ── Install targets ──────────────────────────────────────────────────────────
# Each is phony and defers to tools.sh, which is idempotent: it re-installs only
# when the pinned version is missing from .bin (a cheap stat otherwise), so
# bumping a version in the manifest transparently triggers a reinstall.

.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install golangci-lint

# Rebuilt whenever the plugin's sources or .custom-gcl.yml change, and not
# otherwise: the build clones golangci-lint and compiles it, which takes a
# minute or two.
$(CUSTOM_GCL): $(REPO_ROOT)/.custom-gcl.yml $(wildcard $(REPO_ROOT)/hack/golangci-onelinefunc/*.go) $(REPO_ROOT)/hack/golangci-onelinefunc/go.mod | golangci-lint
	@echo ">> building custom-gcl (golangci-lint + hack/golangci-onelinefunc)"
	@cd "$(REPO_ROOT)" && "$(GOLANGCI_LINT)" custom

.PHONY: custom-gcl
custom-gcl: $(CUSTOM_GCL) ## Build golangci-lint with this repository's linters compiled in.

.PHONY: kustomize
kustomize: ## Install kustomize (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install kustomize

.PHONY: controller-gen
controller-gen: ## Install controller-gen (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install controller-gen

.PHONY: openapi-gen
openapi-gen: ## Install openapi-gen (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install openapi-gen

.PHONY: yq
yq: ## Install yq (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install yq

.PHONY: envtest
envtest: ## Install setup-envtest into .bin ($(ENVTEST_VERSION) from the caller).
	@"$(TOOLS_SH)" install setup-envtest $(ENVTEST_VERSION)

# buf shells out to the two protoc-gen-* plugins and finds them on PATH, so all
# three install together -- buf alone cannot generate anything.
.PHONY: buf
buf: ## Install buf and the protoc-gen-go plugins (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install buf
	@"$(TOOLS_SH)" install protoc-gen-go
	@"$(TOOLS_SH)" install protoc-gen-go-grpc
