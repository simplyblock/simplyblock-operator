# Root Makefile orchestrating build & test across the monorepo components:
#   atlas-lib (shared library), csi-driver, operator
#
# Each target delegates to the relevant component's own Makefile.

ATLAS_DIR       := atlas-lib
CSI_DIR         := csi-driver
OPERATOR_DIR    := operator
HELM_DIR        := helm-charts
INTEGRATION_DIR := test/integration

# Local, untracked settings: the paths that are one machine's business rather
# than this repository's. SBCLI_DIR below is the one that needs it today.
-include local.mk

# The control-plane spec, and where it is exported from.
#
# SBCLI_DIR has no default, because a checkout of another repository is not
# something this one can know the location of. It is resolved, in order, from
# the command line or the environment, then from the repo.sbcli setting that
# scripts/local-config.sh records in local.mk, and then from .sbcli inside this
# repository, which is where CI checks sbcli out and which .gitignore covers.
# Failing all three, the openapi targets say what to set.
SBCLI_DIR    ?= $(firstword $(strip $(REPO_SBCLI)) $(wildcard .sbcli))
OPENAPI_SPEC := shared/openapi.json

# The export imports the control plane's own app, so it runs against sbcli's
# requirement set rather than this machine's Python. The environment lives with
# the other pinned tools in .bin, which is ignored, and is built on first use:
# the alternative is a target that only ever works in CI.

# The interpreter the environment is built with, for a host whose python3 is too
# old for sbcli's requirements. Set it with
# `scripts/local-config.sh venv interpreter set python3.13`.
VENV_INTERPRETER   ?= $(firstword $(strip $(VENV_INTERPRETER)) python3)
OPENAPI_VENV       := .bin/openapi-venv
OPENAPI_PY         := $(OPENAPI_VENV)/bin/python
OPENAPI_STAMP      := $(OPENAPI_VENV)/.requirements-installed
# Through wildcard, so an absent sbcli checkout reaches the recipe's own message
# instead of make's "no rule to make target."
SBCLI_REQUIREMENTS := $(wildcard $(SBCLI_DIR)/requirements.txt)

# Run orchestrated targets serially; each component's Makefile manages its own
# internal parallelism.
.NOTPARALLEL:

.DEFAULT_GOAL := help

.PHONY: all build test lint fmt vet help \
        atlas atlas-build atlas-test atlas-lint atlas-fmt atlas-vet \
        csi csi-build csi-test csi-lint csi-fmt csi-vet \
        operator operator-manifests operator-build-installer operator-build operator-test operator-lint operator-fmt operator-vet \
        helm-sync \
        configure openapi-sync openapi-diff openapi-ref openapi-checkout

# ─── Aggregate ──────────────────────────────────────────────────────────────
all: build test ## Build and test every component.

build: atlas-build csi-build operator-build-installer operator-build helm-sync ## Build every component.

test: atlas-test csi-test operator-test ## Test every component.

lint: atlas-lint csi-lint operator-lint ## Lint every component.

fmt: atlas-fmt csi-fmt operator-fmt ## Format every component.

vet: atlas-vet csi-vet operator-vet ## Vet every component.

# ─── atlas ────────────────────────────────────────────────────────────────
atlas: atlas-build atlas-test ## Build and test atlas.

atlas-build: ## Build atlas.
	$(MAKE) -C $(ATLAS_DIR) build

atlas-test: ## Test atlas.
	$(MAKE) -C $(ATLAS_DIR) test

atlas-lint: ## Lint atlas.
	$(MAKE) -C $(ATLAS_DIR) lint

atlas-fmt: ## Format atlas.
	$(MAKE) -C $(ATLAS_DIR) fmt

atlas-vet: ## Vet atlas.
	$(MAKE) -C $(ATLAS_DIR) vet

# ─── csi ──────────────────────────────────────────────────────────────────
csi: csi-build csi-test ## Build and test csi.

csi-build: ## Build csi (spdkcsi binary).
	$(MAKE) -C $(CSI_DIR) spdkcsi

csi-test: ## Test csi.
	$(MAKE) -C $(CSI_DIR) test

csi-lint: ## Lint csi.
	$(MAKE) -C $(CSI_DIR) lint

csi-fmt: ## Format csi.
	$(MAKE) -C $(CSI_DIR) fmt

csi-vet: ## Vet csi.
	$(MAKE) -C $(CSI_DIR) vet

# ─── operator ───────────────────────────────────────────────────────────────
operator: operator-manifests operator-build-installer operator-build operator-test ## Manifests, installer, build and test operator.

operator-manifests: ## Generate operator manifests (CRDs, RBAC, webhooks).
	$(MAKE) -C $(OPERATOR_DIR) manifests

operator-build-installer: ## Generate the operator dist/install.yaml.
	$(MAKE) -C $(OPERATOR_DIR) build-installer

operator-build: ## Build the operator manager binary.
	$(MAKE) -C $(OPERATOR_DIR) build

.PHONY: operator-build-upgrade
operator-build-upgrade: ## Build the API upgrade tool, which is not in the operator image.
	$(MAKE) -C $(OPERATOR_DIR) build-upgrade

operator-test: ## Test operator.
	$(MAKE) -C $(OPERATOR_DIR) test

operator-lint: ## Lint operator.
	$(MAKE) -C $(OPERATOR_DIR) lint

operator-fmt: ## Format operator.
	$(MAKE) -C $(OPERATOR_DIR) fmt

operator-vet: ## Vet operator.
	$(MAKE) -C $(OPERATOR_DIR) vet

# ─── helm ─────────────────────────────────────────────────────────────────
# Sync the operator's generated CRDs and RBAC roles into the Helm chart. Depends
# on operator-manifests so the copied sources are freshly generated (not stale).
helm-sync: operator-manifests ## Sync operator CRDs and RBAC into the Helm chart.
	bash $(HELM_DIR)/scripts/sync-from-operator.sh $(OPERATOR_DIR)

# ─── openapi ──────────────────────────────────────────────────────────────
# shared/openapi.json is the contract atlas-lib's generated client and the
# integration suite's control-plane simulator are built from, and it is exported
# from the control plane rather than written here. That makes it go stale
# silently: nothing in this repository fails when sbcli grows a field, and the
# generated client simply cannot see it.
#
# CI exports it nightly and proposes the drift as a pull request (see
# .github/workflows/repo_openapi_sync.yaml). These two targets are the same
# export by hand, for when the question is whether the export is behind today.
#
# Importing the control plane's app needs sbcli's own requirements installed
# (pip install -r $(SBCLI_DIR)/requirements.txt), and nothing connects to a
# database or to FoundationDB at import time.

# Which sbcli the spec is being read from, stated every time. A sibling checkout
# sits on whatever branch it was last left on, and an export from a release
# branch drops the endpoints that only exist on main: the committed spec is the
# contract, so the ref it came from is not a detail.
# ─── configuration ────────────────────────────────────────────────────────
# Where the checkouts this repository cannot contain actually are. Detecting
# them beats explaining local.mk to everybody once, and recording the answer
# beats detecting it on every invocation.
configure: ## Find the sbcli checkout the openapi targets need and record it.
	@bash scripts/local-config.sh detect $(CONFIGURE_ARGS)

# Says what to do rather than failing on a path nobody set.
openapi-checkout:
	@test -n "$(SBCLI_DIR)" && test -d "$(SBCLI_DIR)" || { \
	  echo "no sbcli checkout: SBCLI_DIR is unset and .sbcli does not exist. Either"; \
	  echo "  let it be found: make configure"; \
	  echo "  or have it cloned: make configure CONFIGURE_ARGS=--clone"; \
	  echo "  or name it: scripts/local-config.sh repo sbcli set <dir>"; \
	  echo "  or pass it once: make $(MAKECMDGOALS) SBCLI_DIR=<dir>"; \
	  exit 1; }

openapi-ref: openapi-checkout
	@printf 'sbcli: %s at %s (%s)\n' \
	  "$$(git -C $(SBCLI_DIR) rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown branch')" \
	  "$$(git -C $(SBCLI_DIR) rev-parse --short HEAD 2>/dev/null || echo 'unknown commit')" \
	  "$(SBCLI_DIR)"

# Rebuilt when sbcli's requirements move, so an export never runs against a
# stale environment.
$(OPENAPI_STAMP): $(SBCLI_REQUIREMENTS)
	@test -n "$(SBCLI_REQUIREMENTS)" || { \
	  echo "no sbcli checkout at $(SBCLI_DIR): clone it there, or pass SBCLI_DIR=<path>"; \
	  exit 1; }
	$(VENV_INTERPRETER) -m venv $(OPENAPI_VENV)
	$(OPENAPI_VENV)/bin/pip install --quiet --requirement $(SBCLI_REQUIREMENTS)
	@touch $@

openapi-diff: openapi-checkout $(OPENAPI_STAMP) ## Show what re-exporting the spec would change, writing nothing.
	@$(MAKE) --no-print-directory openapi-ref
	@new=$$(mktemp); trap 'rm -f "$$new"' EXIT; \
	  $(OPENAPI_PY) shared/export-openapi.py export --sbcli $(SBCLI_DIR) --out $$new >/dev/null && \
	  $(OPENAPI_PY) shared/export-openapi.py summarize --old $(OPENAPI_SPEC) --new $$new

# Regenerating is part of the sync rather than a step to remember: a spec that
# moved and a client that did not is what the drift gates fail on.
openapi-sync: openapi-checkout $(OPENAPI_STAMP) ## Re-export the spec from SBCLI_DIR, then regenerate what reads it.
	@$(MAKE) --no-print-directory openapi-ref
	@old=$$(mktemp); cp $(OPENAPI_SPEC) $$old; trap 'rm -f "$$old"' EXIT; \
	  $(OPENAPI_PY) shared/export-openapi.py export --sbcli $(SBCLI_DIR) --out $(OPENAPI_SPEC) && \
	  $(OPENAPI_PY) shared/export-openapi.py summarize --old $$old --new $(OPENAPI_SPEC)
	$(MAKE) -C $(ATLAS_DIR) generate
	$(MAKE) -C $(INTEGRATION_DIR) generate

# ─── help ─────────────────────────────────────────────────────────────────
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
