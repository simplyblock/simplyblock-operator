# The support tool's build: one binary that serves the page and the API on the
# support agent's own machine. Nothing here reaches a cluster or an install
# path, so the targets are build, run, and the checks.
#
# Tool binaries and their pinned versions come from ../scripts/tools.mk, which
# every component in this repository shares.

# bash, so that a failing command in a pipeline fails the recipe.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LISTEN ?= 127.0.0.1:8099

.PHONY: all
all: build

include ../scripts/tools.mk

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build bin/support-tool.
	go build -o bin/support-tool .

.PHONY: run
run: ## Serve the tool on $(LISTEN).
	go run . -listen $(LISTEN)

.PHONY: test
test: ## Run the tests.
	go test -race ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT)" run

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...
