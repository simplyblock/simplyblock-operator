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
PROTO_DIRS := link/linkv1 link/sitev1 storage/storagerpc/storagev1 nfsexport/nfsexportrpc/nfsexportv1 \
	conntrack/conntrackrpc/conntrackv1 placement/placementrpc/placementv1 netcheck/netcheckrpc/netcheckv1
PROTO_GEN := link/linkv1/link.pb.go link/linkv1/link_grpc.pb.go \
	link/sitev1/relay.pb.go link/sitev1/relay_grpc.pb.go \
	link/sitev1/switchover.pb.go link/sitev1/switchover_grpc.pb.go \
	link/sitev1/site.pb.go link/sitev1/site_grpc.pb.go \
	storage/storagerpc/storagev1/nvme.pb.go storage/storagerpc/storagev1/nvme_grpc.pb.go \
	nfsexport/nfsexportrpc/nfsexportv1/export.pb.go nfsexport/nfsexportrpc/nfsexportv1/export_grpc.pb.go \
	conntrack/conntrackrpc/conntrackv1/flow.pb.go conntrack/conntrackrpc/conntrackv1/flow_grpc.pb.go \
	placement/placementrpc/placementv1/placement.pb.go placement/placementrpc/placementv1/placement_grpc.pb.go \
	netcheck/netcheckrpc/netcheckv1/check.pb.go netcheck/netcheckrpc/netcheckv1/check_grpc.pb.go

GENERATED := $(CPAPI_GEN) $(PROTO_GEN)

all: build test ## Build and test the library.

generate: $(GENERATED) ## Generate the control-plane API client, its response validation, and the link protocol.

internal/cpapi/cpapi.gen.go: ../shared/openapi.json internal/cpapi/oapi-codegen.yaml \
		internal/cpapi/gen.go
	go generate ./internal/cpapi/...

# The rules are checked against the spec and resolved against the client, so
# this reruns when either of those changes. Both are `go generate` directives;
# only the second one is needed here, and the client's recipe above covers it.
internal/cpapi/validation.gen.go: internal/cpapi/cpapi.gen.go internal/cpapi/validation.yaml \
		internal/cpapi/gen/main.go
	cd internal/cpapi && go run ./gen

# Both files of a pair come out of one buf invocation; the gRPC half is
# expressed as depending on the other so a single recipe runs, portably across
# make versions (grouped targets would need GNU make 4.3+).
link/linkv1/link.pb.go: link/linkv1/link.proto link/linkv1/buf.gen.yaml link/linkv1/buf.yaml | buf
	cd link/linkv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

link/linkv1/link_grpc.pb.go: link/linkv1/link.pb.go

link/sitev1/relay.pb.go: link/sitev1/relay.proto link/sitev1/buf.gen.yaml link/sitev1/buf.yaml | buf
	cd link/sitev1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

link/sitev1/relay_grpc.pb.go: link/sitev1/relay.pb.go

# One buf invocation generates every file of the module, so the second pair
# rides the first's recipe.
link/sitev1/switchover.pb.go: link/sitev1/switchover.proto | link/sitev1/relay.pb.go

link/sitev1/switchover_grpc.pb.go: link/sitev1/switchover.pb.go

link/sitev1/site.pb.go: link/sitev1/site.proto | link/sitev1/relay.pb.go

link/sitev1/site_grpc.pb.go: link/sitev1/site.pb.go

storage/storagerpc/storagev1/nvme.pb.go: storage/storagerpc/storagev1/nvme.proto storage/storagerpc/storagev1/buf.gen.yaml storage/storagerpc/storagev1/buf.yaml | buf
	cd storage/storagerpc/storagev1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

storage/storagerpc/storagev1/nvme_grpc.pb.go: storage/storagerpc/storagev1/nvme.pb.go

nfsexport/nfsexportrpc/nfsexportv1/export.pb.go: nfsexport/nfsexportrpc/nfsexportv1/export.proto nfsexport/nfsexportrpc/nfsexportv1/buf.gen.yaml nfsexport/nfsexportrpc/nfsexportv1/buf.yaml | buf
	cd nfsexport/nfsexportrpc/nfsexportv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

nfsexport/nfsexportrpc/nfsexportv1/export_grpc.pb.go: nfsexport/nfsexportrpc/nfsexportv1/export.pb.go

conntrack/conntrackrpc/conntrackv1/flow.pb.go: conntrack/conntrackrpc/conntrackv1/flow.proto conntrack/conntrackrpc/conntrackv1/buf.gen.yaml conntrack/conntrackrpc/conntrackv1/buf.yaml | buf
	cd conntrack/conntrackrpc/conntrackv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

conntrack/conntrackrpc/conntrackv1/flow_grpc.pb.go: conntrack/conntrackrpc/conntrackv1/flow.pb.go

placement/placementrpc/placementv1/placement.pb.go: placement/placementrpc/placementv1/placement.proto placement/placementrpc/placementv1/buf.gen.yaml placement/placementrpc/placementv1/buf.yaml | buf
	cd placement/placementrpc/placementv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

placement/placementrpc/placementv1/placement_grpc.pb.go: placement/placementrpc/placementv1/placement.pb.go

netcheck/netcheckrpc/netcheckv1/check.pb.go: netcheck/netcheckrpc/netcheckv1/check.proto netcheck/netcheckrpc/netcheckv1/buf.gen.yaml netcheck/netcheckrpc/netcheckv1/buf.yaml | buf
	cd netcheck/netcheckrpc/netcheckv1 && PATH="$(BIN_DIR):$$PATH" "$(BUF)" generate

netcheck/netcheckrpc/netcheckv1/check_grpc.pb.go: netcheck/netcheckrpc/netcheckv1/check.pb.go

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
