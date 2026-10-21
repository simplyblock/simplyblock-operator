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
# passes via $(ENVTEST_VERSION), along with the Kubernetes version of the
# envtest assets in $(ENVTEST_K8S_VERSION).

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

# ── envtest assets ───────────────────────────────────────────────────────────
# setup-envtest installs the Kubernetes binaries (etcd, kube-apiserver, kubectl)
# read-only, the files and their version directory alike, only to keep them
# from being edited by accident. It has no option to skip that, and a read-only
# directory is one rm -rf and `git worktree remove` cannot delete, which leaves
# every worktree that ran the envtest suites half-removed. setup-envtest never
# checks the permissions when it reuses an installed version, so the target
# gives the owner write access back after every install.

# ENVTEST_ASSETS is the installed asset directory, for KUBEBUILDER_ASSETS. It
# only looks a version up (-i) and never downloads one, so setup-envtest stays
# the one place assets are installed, and with it the chmod. Recursive, so it is
# resolved when a recipe uses it, after setup-envtest has run.
ENVTEST_ASSETS = $(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_DIR)" -i -p path)

.PHONY: setup-envtest
setup-envtest: envtest ## Install envtest's Kubernetes binaries ($(ENVTEST_K8S_VERSION) from the caller) into .bin.
	@echo "Setting up envtest binaries for Kubernetes $(ENVTEST_K8S_VERSION)..."
	@assets=$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_DIR)" -p path) || { \
		echo "Error: failed to set up envtest binaries for Kubernetes $(ENVTEST_K8S_VERSION)." >&2; \
		exit 1; \
	}; \
	chmod -R u+w "$$assets"
