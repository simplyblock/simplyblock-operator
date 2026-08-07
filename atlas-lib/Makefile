# Makefile for the atlas shared Go library.

.DEFAULT_GOAL := all
.PHONY: all build test lint lint-proto vet fmt generate

# golangci-lint is downloaded, checksum-verified and pinned into the repo-root
# .bin by the shared downloader (version matches the repo-global lint workflow).
include ../scripts/tools.mk

# The control-plane API client is generated (oapi-codegen), as is the response
# validation that goes with it, so every compiling target regenerates them first
# when missing or when the spec, the client or the rules change. The generator
# version is pinned in go.mod (internal/cpapi/tools.go).
CPAPI_GEN := internal/cpapi/cpapi.gen.go internal/cpapi/validation.gen.go

# The protocols are generated from their .proto by buf. Unlike the cpapi
# generator this one is not a go.mod dependency but a pinned binary in .bin, so
# the rules take `buf` as an order-only prerequisite: it installs the toolchain
# when absent without a phony target forcing a regeneration on every make run.
PROTO_DIRS := link/linkv1 storage/storagerpc/storagev1
PROTO_GEN := link/linkv1/link.pb.go link/linkv1/link_grpc.pb.go \
	storage/storagerpc/storagev1/nvme.pb.go storage/storagerpc/storagev1/nvme_grpc.pb.go

GENERATED := $(CPAPI_GEN) $(PROTO_GEN)

all: build test ## Build and test the library.

generate: $(GENERATED) ## Generate the control-plane API client, its response validation, and the link protocol.

internal/cpapi/cpapi.gen.go: ../shared/openapi.json internal/cpapi/oapi-codegen.yaml \
		internal/cpapi/overlay.yaml internal/cpapi/gen.go
	go generate ./internal/cpapi/...

# The rules are checked against the spec and resolved against the client, so
# this reruns when either of those changes. Both are `go generate` directives;
# only the second one is needed here, and the client's recipe above covers it.
internal/cpapi/validation.gen.go: internal/cpapi/cpapi.gen.go internal/cpapi/validation.yaml \
		internal/cpapi/gen/main.go
	cd internal/cpapi && go run ./gen

# Both files of a pair come out of one buf invocation; the grpc half is
# expressed as depending on the other so a single recipe runs, portably across
# make versions (grouped targets would need GNU make 4.3+).
link/linkv1/link.pb.go: link/linkv1/link.proto link/linkv1/buf.gen.yaml link/linkv1/buf.yaml | buf
	cd link/linkv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

link/linkv1/link_grpc.pb.go: link/linkv1/link.pb.go

storage/storagerpc/storagev1/nvme.pb.go: storage/storagerpc/storagev1/nvme.proto storage/storagerpc/storagev1/buf.gen.yaml storage/storagerpc/storagev1/buf.yaml | buf
	cd storage/storagerpc/storagev1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

storage/storagerpc/storagev1/nvme_grpc.pb.go: storage/storagerpc/storagev1/nvme.pb.go

build: $(GENERATED) ## Build all packages.
	go build ./...

test: vet ## Run go vet and the unit tests (race + coverage).
	go test -race -coverprofile=coverage.out ./...

lint: golangci-lint $(GENERATED) ## Run golangci-lint (pinned version, matches CI).
	"$(GOLANGCI_LINT)" run ./...

lint-proto: | buf ## Lint the protocol definitions.
	@for dir in $(PROTO_DIRS); do \
		echo "buf lint $$dir"; \
		(cd $$dir && "$(BUF)" lint) || exit 1; \
	done

vet: $(GENERATED) ## Run go vet.
	go vet ./...

fmt: ## Format the code.
	go fmt ./...
