# The Control Center's everyday operations: install the toolchain, compile the
# JSX into dist/, serve the console against the mock backend or a real cluster,
# test the mock, and build the images. It lives here rather than in the root
# Makefile because the console is a Node and Go project of its own, built
# outside the Go module graph the root targets drive.
#
# Every recipe is a single command with no shell constructs, so the targets
# also run under GNU make on Windows.

# The console the mock backend serves: the fleet dashboard needs several
# contexts, and a fixed seed makes every run show the same world.
MOCK_LISTEN   ?= :8081
MOCK_SEED     ?= 42
MOCK_CONTEXTS ?= dev=small-healthy,uat=medium-degraded,prd=large-scale
CRD_DIR       ?= ../../operator/config/crd/bases

# The console against a real cluster. CLUSTER_CONTEXTS maps console context
# names to kubeconfig contexts (dev=admin@dev,prd=admin@prd); empty runs one
# context on kubectl's current one.
CLUSTER_LISTEN    ?= 127.0.0.1:8080
CLUSTER_CONTEXTS  ?=
CLUSTER_NAMESPACE ?= simplyblock

IMAGE      ?= quay.io/simplyblock-io/control-center:dev
MOCK_IMAGE ?= quay.io/simplyblock-io/control-center-mock:dev

.DEFAULT_GOAL := help

.PHONY: help install build serve-mock serve-cluster test test-ui test-mock image image-demo image-mock

help: ## List the targets.
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# The targets that need node_modules install it only when it is missing or
# older than the lockfile; `make install` reinstalls it unconditionally.
node_modules/.package-lock.json: package.json package-lock.json
	npm ci --no-audit --no-fund

install: ## Install the Babel toolchain, React, and the typefaces from the lockfile.
	npm ci --no-audit --no-fund

build: node_modules/.package-lock.json ## Compile the JSX sources into dist/app.js and dist/mock.js.
	node deploy/build/bundle.mjs

serve-mock: build ## Serve the console on the generated mock world (MOCK_LISTEN, MOCK_SEED, MOCK_CONTEXTS).
	go -C mock run . --serve-ui .. --seed $(MOCK_SEED) --listen $(MOCK_LISTEN) --crd-dir $(CRD_DIR) --contexts $(MOCK_CONTEXTS)

serve-cluster: build ## Serve the console against a real cluster through kubectl proxy (CLUSTER_CONTEXTS, CLUSTER_NAMESPACE).
	node dev/serve-cluster.mjs --listen=$(CLUSTER_LISTEN) --namespace=$(CLUSTER_NAMESPACE) --contexts=$(CLUSTER_CONTEXTS)

test: test-ui test-mock ## Run every test.

test-ui: build ## Test the console in the system Chrome (playwright-core, in-browser fixtures).
	npm test

test-mock: ## Test the mock backend.
	go -C mock test ./...

image: ## Build the console image without the fixture backend.
	docker build -t $(IMAGE) -f deploy/Dockerfile .

image-demo: ## Build the console image with the fixture backend kept (demo only).
	docker build --build-arg KEEP_MOCKS=1 -t $(IMAGE) -f deploy/Dockerfile .

image-mock: ## Build the mock backend image.
	docker build -t $(MOCK_IMAGE) mock
