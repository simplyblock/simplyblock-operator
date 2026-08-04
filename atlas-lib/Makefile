# Makefile for the atlas shared Go library.

.DEFAULT_GOAL := all
.PHONY: all build test lint vet fmt generate

# golangci-lint is downloaded, checksum-verified and pinned into the repo-root
# .bin by the shared downloader (version matches the repo-global lint workflow).
include ../scripts/tools.mk

# The control-plane API client is generated (oapi-codegen), as is the response
# validation that goes with it, so every compiling target regenerates them first
# when missing or when the spec, the client or the rules change. The generator
# version is pinned in go.mod (internal/cpapi/tools.go).
CPAPI_GEN := internal/cpapi/cpapi.gen.go internal/cpapi/validation.gen.go

all: build test ## Build and test the library.

generate: $(CPAPI_GEN) ## Generate the control-plane API client and its response validation.

internal/cpapi/cpapi.gen.go: ../shared/openapi.json internal/cpapi/oapi-codegen.yaml \
		internal/cpapi/overlay.yaml internal/cpapi/gen.go
	go generate ./internal/cpapi/...

# The rules are checked against the spec and resolved against the client, so
# this reruns when either of those changes. Both are `go generate` directives;
# only the second one is needed here, and the client's recipe above covers it.
internal/cpapi/validation.gen.go: internal/cpapi/cpapi.gen.go internal/cpapi/validation.yaml \
		internal/cpapi/gen/main.go
	cd internal/cpapi && go run ./gen

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
