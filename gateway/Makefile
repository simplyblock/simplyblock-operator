# The gateway component's build: a single static binary embedding versitygw
# (github.com/versity/versitygw/embedgw) over its own posix backend. No
# manifests/generate step like operator or fleet: this component carries no
# CRDs of its own.
#
# Tool binaries and their pinned versions come from ../scripts/tools.mk, which
# every component in this repository shares.

BUILD_DIR := ./build

IMG_REGISTRY ?= docker.io/simplyblock
IMG_TAG      ?= latest
IMG          ?= $(IMG_REGISTRY)/gateway:$(IMG_TAG)

CONTAINER_TOOL ?= docker

.PHONY: all
all: build

include ../scripts/tools.mk

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT)" run ./...

.PHONY: test
test: ## Run the test suite.
	go test ./... -race

##@ Build

.PHONY: build
build: fmt vet ## Build the gateway binary to ./build/gateway.
	@mkdir -p $(BUILD_DIR)
	go build -buildvcs=false -o $(BUILD_DIR)/gateway ./cmd/gateway

.PHONY: docker-build
docker-build: ## Build the gateway image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the gateway image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: clean
clean: ## Remove build output.
	rm -rf $(BUILD_DIR)

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
