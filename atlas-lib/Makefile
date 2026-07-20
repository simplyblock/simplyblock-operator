# Makefile for the atlas shared Go library.

.DEFAULT_GOAL := all
.PHONY: all build test lint vet fmt generate

# golangci-lint is downloaded, checksum-verified and pinned into the repo-root
# .bin by the shared downloader (version matches the repo-global lint workflow).
include ../scripts/tools.mk

# The control-plane API client is generated (oapi-codegen) and gitignored, so
# every compiling target regenerates it first when missing or when the spec
# changes. The generator version is pinned in go.mod (internal/cpapi/tools.go).
CPAPI_GEN := internal/cpapi/cpapi.gen.go

all: build test ## Build and test the library.

generate: $(CPAPI_GEN) ## Generate the control-plane API client from its OpenAPI spec.

$(CPAPI_GEN): internal/cpapi/openapi.json internal/cpapi/oapi-codegen.yaml internal/cpapi/gen.go
	go generate ./internal/cpapi/...

build: $(CPAPI_GEN) ## Build all packages.
	go build ./...

test: vet ## Run go vet and the unit tests (race + coverage).
	go test -race -coverprofile=coverage.out ./...

lint: golangci-lint $(CPAPI_GEN) ## Run golangci-lint (pinned version, matches CI).
	"$(GOLANGCI_LINT)" run ./...

vet: $(CPAPI_GEN) ## Run go vet.
	go vet ./...

fmt: ## Format the code.
	go fmt ./...
