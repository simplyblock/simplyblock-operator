# Makefile for the atlas shared Go library.

# Pinned to match the repo-global lint workflow (.github/workflows/repo_lint.yaml).
GOLANGCI_LINT_VERSION ?= v2.11.4

.DEFAULT_GOAL := all
.PHONY: all build test lint vet fmt

all: build test ## Build and test the library.

build: ## Build all packages.
	go build ./...

test: vet ## Run go vet and the unit tests (race + coverage).
	go test -race -coverprofile=coverage.out ./...

lint: ## Run golangci-lint (pinned version, matches CI).
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vet: ## Run go vet.
	go vet ./...

fmt: ## Format the code.
	go fmt ./...
