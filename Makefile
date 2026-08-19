# Root Makefile orchestrating build & test across the monorepo components:
#   atlas-lib (shared library), csi-driver, operator
#
# Each target delegates to the relevant component's own Makefile.

ATLAS_DIR    := atlas-lib
CSI_DIR      := csi-driver
OPERATOR_DIR := operator
HELM_DIR     := helm-charts
INTEGRATION_DIR := test/integration

# Run orchestrated targets serially; each component's Makefile manages its own
# internal parallelism.
.NOTPARALLEL:

.DEFAULT_GOAL := help

.PHONY: all build test lint fmt vet help \
        atlas atlas-build atlas-test atlas-lint atlas-fmt atlas-vet \
        csi csi-build csi-test csi-lint csi-fmt csi-vet \
        operator operator-manifests operator-build-installer operator-build operator-test operator-lint operator-fmt operator-vet \
        integration integration-build integration-test integration-lint integration-vet integration-clean \
        helm-sync

# ─── Aggregate ──────────────────────────────────────────────────────────────
all: build test ## Build and test every component.

build: atlas-build csi-build operator-build-installer operator-build helm-sync ## Build every component.

# Deliberately excludes integration-test, which boots virtual machines and takes
# minutes; run it explicitly with `make integration-test`.
test: atlas-test csi-test operator-test ## Test every component (unit/e2e, no VMs).

lint: atlas-lint csi-lint operator-lint integration-lint ## Lint every component.

fmt: atlas-fmt csi-fmt operator-fmt ## Format every component.

vet: atlas-vet csi-vet operator-vet integration-vet ## Vet every component.

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

# ─── help ─────────────────────────────────────────────────────────────────
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ─── integration ──────────────────────────────────────────────────────────
# Boots a Talos cluster in QEMU; needs root and hardware virtualisation.
integration: integration-test ## Run the integration suite (boots VMs).

integration-build: ## Build the integration harness.
	$(MAKE) -C $(INTEGRATION_DIR) build

integration-test: ## Boot a cluster and run the integration suite.
	$(MAKE) -C $(INTEGRATION_DIR) test

integration-lint: ## Lint the integration harness.
	$(MAKE) -C $(INTEGRATION_DIR) lint

integration-vet: ## Vet the integration harness.
	$(MAKE) -C $(INTEGRATION_DIR) vet

integration-clean: ## Destroy a cluster left behind by an interrupted run.
	$(MAKE) -C $(INTEGRATION_DIR) clean-clusters
