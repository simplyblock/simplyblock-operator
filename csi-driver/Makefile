# Copyright (c) Arm Limited and Contributors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# build output dir
BUILD_DIR := ./build
# go source, scripts
SOURCE_DIRS := cmd pkg
SCRIPT_DIRS := scripts deploy e2e
# goarch for cross building
ifeq ($(origin GOARCH), undefined)
  GOARCH := $(shell go env GOARCH)
endif
# csi image info (spdkcsi/spdkcsi:canary)
ifeq ($(origin CSI_IMAGE_REGISTRY), undefined)
  CSI_IMAGE_REGISTRY := simplyblock
endif
ifeq ($(origin CSI_IMAGE_TAG), undefined)
  CSI_IMAGE_TAG := latest
endif
CSI_IMAGE := $(CSI_IMAGE_REGISTRY)/spdkcsi:$(CSI_IMAGE_TAG)

# default target
all: spdkcsi lint test

# build binary
.PHONY: spdkcsi
spdkcsi:
	@echo === building spdkcsi binary
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=0 GOARCH=$(GOARCH) GOOS=linux go build -buildvcs=false -o $(BUILD_DIR)/spdkcsi ./cmd/

# static code check, text lint
# lint: golangci yamllint shellcheck mdl codespell
lint: golangci

# golangci-lint (and any other dev tools) are downloaded, checksum-verified, and
# pinned into the repo-root .bin by the shared downloader.
include ../scripts/tools.mk

.PHONY: golangci
golangci: golangci-lint
	@echo === running golangci-lint
	@"$(GOLANGCI_LINT)" run ./...

.PHONY: fmt
fmt:
	@echo === running go fmt
	@go fmt ./...

.PHONY: vet
vet:
	@echo === running go vet
	@go vet ./...

.PHONY: yamllint
yamllint:
	@echo === running yamllint
	@if hash yamllint 2> /dev/null; then                     \
	     yamllint -s -c scripts/yamllint.yml $(SCRIPT_DIRS); \
	     bash scripts/verify-helm-yamllint.sh;               \
	 else                                                    \
	     echo yamllint not installed, skip test;             \
	 fi

.PHONY: shellcheck
shellcheck:
	@echo === running shellcheck
	@find $(SCRIPT_DIRS) -name "*.sh" -type f | xargs bash -n
	@if hash shellcheck 2> /dev/null; then                               \
	     find $(SCRIPT_DIRS) -name "*.sh" -type f | xargs shellcheck -x; \
	 else                                                                \
	     echo shellcheck not installed, skip test;                       \
	 fi

.PHONY: mdl
mdl:
	@echo === running mdl
	@if hash mdl 2> /dev/null; then                                      \
		mdl --git-recurse --style scripts/ci/mdl_rules.rb .;             \
	else                                                                 \
	    echo Markdown linter not found, please install;                  \
	    false                                          ;                 \
	fi

.PHONY: codespell
codespell:
	@echo === running codespell
	@if hash codespell 2> /dev/null; then                                \
		git ls-files -z | xargs -0 codespell;                            \
	else                                                                 \
	    echo codespell linter not found, please install;                 \
	    false                                          ;                 \
	fi

# tests
test: mod-check unit-test

.PHONY: mod-check
mod-check:
	@echo === running go mod verify
	@go mod verify

.PHONY: unit-test
unit-test:
	@echo === running unit test
	@go test -v -race -cover $(if $(SKIP_TESTS),-skip $(SKIP_TESTS)) $(foreach d,$(SOURCE_DIRS),./$(d)/...)

# e2e test
.PHONY: e2e-test
# Pass extra arguments to e2e tests.
# Use E2E_PROCS=N to run specs in parallel (requires the ginkgo CLI).
# Use E2E_TEST_ARGS to pass additional ginkgo flags.
# Examples:
#   make e2e-test E2E_PROCS=4
#   make e2e-test E2E_PROCS=4 E2E_TEST_ARGS='--skip="SPDKCSI-MULTICLUSTER|XFS"'
#   make e2e-test E2E_TEST_ARGS='--focus="SPDKCSI-NVMEOF"'
E2E_PROCS=
E2E_TEST_ARGS=
e2e-test:
	@echo === running e2e test
	@if [ -n "$(E2E_PROCS)" ]; then \
		go run github.com/onsi/ginkgo/v2/ginkgo --procs=$(E2E_PROCS) --race --timeout=30m $(E2E_TEST_ARGS) ./e2e/; \
	else \
		go test -race -timeout 30m ./e2e $(E2E_TEST_ARGS); \
	fi

# helm test
.PHONY: helm-test
helm-test:
	@echo === running helm test
	@./scripts/install-helm.sh up
	@./scripts/install-helm.sh install-spdkcsi
	@./scripts/install-helm.sh cleanup-spdkcsi
	@./scripts/install-helm.sh clean

# docker image
# The Dockerfile is a multi-stage build that compiles spdkcsi from source, so the
# build context is the repo root (it needs both csi-driver/ and the sibling
# atlas-lib/ module) -- not $(BUILD_DIR), and no pre-built binary is required.
image:
	@echo === running docker build
	@if [ -n "$(HTTP_PROXY)" ]; then \
		proxy_opt="--build-arg http_proxy=$(HTTP_PROXY) --build-arg https_proxy=$(HTTP_PROXY)"; \
	fi; \
	sudo docker build --no-cache -t $(CSI_IMAGE)-amd64 $$proxy_opt \
	-f deploy/image/Dockerfile ..; \
	sudo apt-get update; \
	sudo apt-get install -y qemu qemu-user-static ;\
	export DOCKER_DEFAULT_PLATFORM=linux/arm64 ;\
	sudo docker buildx build --platform linux/arm64 -t $(CSI_IMAGE)-arm64 $$proxy_opt \
	-f deploy/image/Dockerfile ..; \

.PHONY: clean
clean:
	rm -f $(BUILD_DIR)/spdkcsi
	go clean -testcache
