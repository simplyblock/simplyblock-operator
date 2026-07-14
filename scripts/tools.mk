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
KUSTOMIZE      ?= $(BIN_DIR)/kustomize
CONTROLLER_GEN ?= $(BIN_DIR)/controller-gen
ENVTEST        ?= $(BIN_DIR)/setup-envtest
YQ             ?= $(BIN_DIR)/yq

# ── Install targets ──────────────────────────────────────────────────────────
# Each is phony and defers to tools.sh, which is idempotent: it re-installs only
# when the pinned version is missing from .bin (a cheap stat otherwise), so
# bumping a version in the manifest transparently triggers a reinstall.

.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install golangci-lint

.PHONY: kustomize
kustomize: ## Install kustomize (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install kustomize

.PHONY: controller-gen
controller-gen: ## Install controller-gen (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install controller-gen

.PHONY: yq
yq: ## Install yq (manifest-pinned) into .bin.
	@"$(TOOLS_SH)" install yq

.PHONY: envtest
envtest: ## Install setup-envtest into .bin ($(ENVTEST_VERSION) from the caller).
	@"$(TOOLS_SH)" install setup-envtest $(ENVTEST_VERSION)
