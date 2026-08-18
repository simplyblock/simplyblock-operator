# Integration suite: boots a Talos cluster in QEMU and runs the tests against it.
#
# `make test` needs root, because talosctl's QEMU provisioner builds the guest
# network (a CNI bridge on Linux, vmnet on macOS). The harness elevates talosctl
# itself rather than the test process, so the sudo prompt lands in the middle of a
# run — where `go test` has captured stdout and it is invisible. `preflight`
# refreshes the credential cache first so that does not happen.

SHELL := /bin/bash
GO ?= go
TIMEOUT ?= 40m
TEST_FLAGS ?= -v

# Overridable so concurrent runs on one machine do not collide on the name.
CLUSTER_NAME ?= sb-integration

.DEFAULT_GOAL := help
.PHONY: help build vet fmt lint test test-short preflight clean-clusters

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Compile the harness.
	$(GO) build ./...

vet: ## Vet the harness.
	$(GO) vet ./...

fmt: ## Format the harness.
	$(GO) fmt ./...

lint: ## Lint the harness.
	golangci-lint run ./...

preflight: ## Check the tools and privileges a run needs.
	@fail=0; \
	for t in talosctl qemu-img kubectl; do \
	  command -v $$t >/dev/null || { echo "missing: $$t"; fail=1; }; \
	done; \
	case "$$(uname -s)/$$(uname -m)" in \
	  Darwin/arm64) ;; \
	  Darwin/*) echo "Talos' QEMU provisioner supports only arm on macOS"; fail=1 ;; \
	  Linux/*) test -e /dev/kvm || { echo "missing: /dev/kvm (QEMU would fall back to emulation, far too slow)"; fail=1; } ;; \
	  *) echo "unsupported host $$(uname -s)/$$(uname -m); on Windows use WSL2" ; fail=1 ;; \
	esac; \
	test $$fail -eq 0 || exit 1; \
	echo "talosctl: $$(talosctl version --client --short 2>/dev/null || talosctl version --client 2>/dev/null | head -2 | tail -1)"; \
	echo "refreshing sudo credentials (the provisioner needs root)"; \
	sudo -v

test: preflight ## Boot a cluster and run the suite.
	SB_INTEGRATION=1 SB_CLUSTER_NAME=$(CLUSTER_NAME) \
	  $(GO) test $(TEST_FLAGS) -timeout $(TIMEOUT) ./...

test-short: preflight ## Run only the smoke test.
	SB_INTEGRATION=1 SB_CLUSTER_NAME=$(CLUSTER_NAME) \
	  $(GO) test $(TEST_FLAGS) -timeout 15m -run TestSmoke ./suites/

# A run killed part-way leaves the cluster behind, holding memory and a network.
clean-clusters: ## Destroy a cluster left behind by an interrupted run.
	-sudo -E talosctl cluster destroy --name $(CLUSTER_NAME)