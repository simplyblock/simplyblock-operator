# Makefile for the atlas shared Go library.

.DEFAULT_GOAL := all
.PHONY: all build test lint vet fmt

# golangci-lint is downloaded, checksum-verified and pinned into the repo-root
# .bin by the shared downloader (version matches the repo-global lint workflow).
include ../scripts/tools.mk

all: build test ## Build and test the library.

build: ## Build all packages.
	go build ./...

test: vet ## Run go vet and the unit tests (race + coverage).
	go test -race -coverprofile=coverage.out ./...

lint: golangci-lint ## Run golangci-lint (pinned version, matches CI).
	"$(GOLANGCI_LINT)" run ./...

vet: ## Run go vet.
	go vet ./...

fmt: ## Format the code.
	go fmt ./...
