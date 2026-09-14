# The fleet component's build. It produces both of a fleet's binaries: the
# manager that runs on the hub, and the agent that runs in each member. They
# share one module because they share a wire contract, so a single definition of
# it cannot skew between them.
#
# It is deliberately smaller than the operator's build: this component installs
# one API group on a hub and has no aggregated API server and no bundle, so the
# targets here are the generate-build-test set and nothing more.
#
# Tool binaries and their pinned versions come from ../scripts/tools.mk, which
# every component in this repository shares.

VERSION ?= 0.1.0
IMG_BASE ?= quay.io/simplyblock-io/simplyblock-fleet
IMG_TAG  ?= $(VERSION)
IMG      ?= $(IMG_BASE):$(IMG_TAG)

CONTAINER_TOOL ?= docker

# bash, so that a failing command in a pipeline fails the recipe.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

include ../scripts/tools.mk

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# No headerFile is passed to controller-gen: files in this component carry no
# license header, generated ones included.
.PHONY: manifests
manifests: controller-gen addon-manifests ## Generate the CustomResourceDefinition objects and the add-on template.
	"$(CONTROLLER_GEN)" rbac:roleName=fleet-manager-role crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases

# The add-on template carries the admission guard, and internal/guard is where
# that guard is defined. Rendering it rather than hand-maintaining a second copy
# is what keeps the rule the tests exercise and the rule a member installs one
# definition.
.PHONY: addon-manifests
addon-manifests: ## Render config/addon/addontemplate.yaml from internal/guard.
	go run ./hack/gen-addon

.PHONY: generate
generate: controller-gen ## Generate the DeepCopy method implementations.
	"$(CONTROLLER_GEN)" object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_DIR)" -p path)" go test ./... -coverprofile cover.out

# gomodver resolves the effective version of a go.mod dependency, honoring a
# replace directive. Both envtest versions are derived rather than pinned, so a
# dependency bump moves them together.
define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

# ENVTEST_VERSION is the controller-runtime release branch matching go.mod.
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

# ENVTEST_K8S_VERSION is the Kubernetes version of the envtest assets, from
# go.mod. The admission guard is a ValidatingAdmissionPolicy, which needs 1.30 or
# later, so an asset set older than that fails the guard's suite rather than
# skipping it.
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: setup-envtest
setup-envtest: envtest ## Download the Kubernetes binaries the envtest suites run against.
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_DIR)" -p path >/dev/null || { \
		echo "Failed to set up envtest binaries for Kubernetes $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify the golangci-lint configuration.
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the fleet binaries.
	mkdir -p build
	go build -o build/fleet-manager ./cmd/fleet-manager

.PHONY: run
run: manifests generate fmt vet ## Run the fleet manager from this host.
	go run ./cmd/fleet-manager

# The build context is the repository root, because the module resolves atlas-lib
# and the operator through local replace directives and Docker cannot reach
# outside its context.
.PHONY: docker-build
docker-build: ## Build the fleet image, which carries both binaries.
	$(CONTAINER_TOOL) build -f Dockerfile -t $(IMG) ..

.PHONY: docker-push
docker-push: ## Push the fleet image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install the fleet CRDs into the cluster $(KUBECTL) points at.
	$(KUBECTL) apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Remove the fleet CRDs from the cluster $(KUBECTL) points at.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f config/crd/bases

# System tools expected on PATH rather than downloaded.
KUBECTL ?= kubectl
ignore-not-found ?= false
