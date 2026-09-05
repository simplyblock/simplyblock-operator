# Integration suite: boots a Talos cluster in QEMU and runs the tests against it.
#
# `make test` needs root, because talosctl's QEMU provisioner builds the guest
# network (a CNI bridge on Linux, vmnet on macOS). The harness elevates talosctl
# itself rather than the test process, so the sudo prompt lands in the middle of a
# run — where `go test` has captured stdout and it is invisible. `preflight`
# refreshes the credential cache first so that does not happen.

SHELL := /bin/bash
GO ?= go
TIMEOUT ?= 60m
# -count=1 defeats the test cache. Go caches a passing package result and replays
# its whole transcript, so a run that booted nothing prints the same green output
# as one that did — with only "(cached)" to tell them apart. For a suite whose
# entire purpose is to exercise a real kernel, that is worse than no result.
TEST_FLAGS ?= -v -count=1

# Unset by default: the suite derives a per-process cluster name so two runs on
# one machine cannot fight over the same network. Set it to pin a name when a run
# has to be identifiable from outside.
CLUSTER_NAME ?=
CLUSTER_ENV := $(if $(CLUSTER_NAME),SB_CLUSTER_NAME=$(CLUSTER_NAME),)

.DEFAULT_GOAL := help
.PHONY: help build vet fmt lint test test-defects test-short preflight clean-clusters \
	generate generate-check

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

generate: ## Regenerate the control-plane simulator from the OpenAPI spec.
	$(GO) generate ./...

# The generated files are committed, so a spec change nobody regenerated would
# leave the simulator answering the previous revision's endpoints. This makes
# that a failure rather than a surprise.
generate-check: generate ## Fail if the committed generated files are stale.
	@git diff --exit-code -- . \
	  || { echo "generated files are stale: run 'make generate' and commit the result"; exit 1; }

preflight: ## Check the tools and privileges a run needs.
	@fail=0; \
	for t in talosctl qemu-img kubectl; do \
	  command -v $$t >/dev/null || { echo "missing: $$t"; fail=1; }; \
	done; \
	case "$$(uname -s)/$$(uname -m)" in \
	  Darwin/arm64) \
	    echo "warning: cluster creation has been seen to fail on macOS at the dhcpd step,"; \
	    echo "  with: cannot bind to port 67: address already in use."; \
	    echo "  The machines are created before that step, and macOS starts its own bootpd"; \
	    echo "  on *:67 within seconds of one attaching to vmnet, so talosctl's dhcpd finds"; \
	    echo "  the port taken. Observed on darwin/arm64 with talosctl 1.13.8 and QEMU 10.1.3,"; \
	    echo "  on a host with no vmnet interface and port 67 free beforehand."; \
	    echo "  Whether anything makes it win that race is not established. If a run here"; \
	    echo "  fails this way, Linux with /dev/kvm is the path that is known to work."; \
	    ;; \
	  Darwin/*) echo "Talos' QEMU provisioner supports only arm on macOS"; fail=1 ;; \
	  Linux/*) test -e /dev/kvm || { echo "missing: /dev/kvm (QEMU would fall back to emulation, far too slow)"; fail=1; } ;; \
	  *) echo "unsupported host $$(uname -s)/$$(uname -m); on Windows use WSL2" ; fail=1 ;; \
	esac; \
	test $$fail -eq 0 || exit 1; \
	echo "talosctl: $$(talosctl version --client --short 2>/dev/null || talosctl version --client 2>/dev/null | head -2 | tail -1)"; \
	if [ "$$(id -u)" -ne 0 ]; then \
	  echo "refreshing sudo credentials (the provisioner needs root)"; \
	  sudo -v; \
	fi

test: preflight ## Boot a cluster and run the suite.
	SB_INTEGRATION=1 $(CLUSTER_ENV) \
	  $(GO) test $(TEST_FLAGS) -timeout $(TIMEOUT) ./...

test-defects: preflight ## Force the NVMe-oF defects and check atlas names them.
	SB_INTEGRATION=1 $(CLUSTER_ENV) \
	  $(GO) test $(TEST_FLAGS) -timeout 40m -run TestDefect ./suites/

test-short: preflight ## Run only the smoke test.
	SB_INTEGRATION=1 $(CLUSTER_ENV) \
	  $(GO) test $(TEST_FLAGS) -timeout 15m -run TestSmoke ./suites/

# A run killed part-way leaves the cluster behind holding memory and a network,
# and its root-owned helper processes reparented to init — sudo's children outlive
# the sudo the test killed. Cluster state is kept in talosctl's default location
# precisely so this can find them by name afterwards.
#
# A create that died before writing state.yaml is the harder case: talosctl
# cannot destroy what it cannot read, and the directory still holds the nodes'
# sparse disk images, so it is removed directly once destroy has had its turn.
#
# Worse still is a create whose own failure path removed the state directory
# while its children kept running. Nothing then names the cluster, the virtual
# machines stay up holding their memory and the host bridge, and macOS restarts
# bootpd for as long as one of them holds a vmnet interface. So the sweep matches
# the processes rather than the state: every talosctl helper, qemu-launch among
# them, and the virtual machines themselves, recognized by the cluster directory
# on their command line so that a developer's unrelated machines are left alone.
clean-clusters: ## Destroy clusters and helpers left by interrupted runs.
	@found=0; \
	for d in $$HOME/.talos/clusters/sbi-* $$HOME/.talos/clusters/sb-integration*; do \
	  [ -d "$$d" ] || continue; \
	  found=1; n=$$(basename $$d); \
	  echo "destroying $$n"; \
	  sudo -E talosctl cluster destroy --name "$$n" || true; \
	  [ -d "$$d" ] && { echo "  removing leftover state $$d"; sudo rm -rf "$$d"; } || true; \
	done; \
	pids=$$(pgrep -f "talosctl (cluster create|loadbalancer-launch|dhcpd-launch|qemu-launch|.*sbi-|.*sb-integration)" 2>/dev/null || true); \
	vms=$$(pgrep -f "qemu-system-[a-z0-9_]+ .*\.talos/clusters/(sbi-|sb-integration)" 2>/dev/null || true); \
	all=$$(echo $$pids $$vms); \
	if [ -n "$$all" ]; then \
	  found=1; \
	  echo "killing orphaned helpers: $$all"; \
	  sudo kill $$all || true; \
	  sleep 2; \
	  still=$$(ps -o pid= -p $$all 2>/dev/null || true); \
	  if [ -n "$$still" ]; then echo "  still up, forcing: $$still"; sudo kill -9 $$still || true; fi; \
	fi; \
	test $$found -eq 1 || echo "nothing left to clean"