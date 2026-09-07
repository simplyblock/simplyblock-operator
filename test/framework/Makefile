# sbtest — component/detector framework for large-scale test runs.
#
# Every target that needs the virtualenv depends on it, so there is no separate setup step
# to remember: `make test` on a clean checkout creates .venv, installs, and runs.
#
#   make test                                   unit tests (no cluster)
#   make analyze RUN=<dir> [SUITE=<name>]       judge a finished run directory
#   make collect OUT=<dir> [SUITE=] [DURATION=] collect from a live cluster, then judge
#   make run SUITE=<name> [DURATION=] [KEEP=1] drive a full test run, then judge it
#   make detectors | make components            what is available, with options
#   make gate                                   quality gate: ruff + mypy + tests
#   make lint | make types | make fmt           the gate's parts, individually
#   make clean | make distclean
#
# SUITE takes a bundled suite name (see `make suites`) or a path to your own file.

SHELL := /bin/bash
.DEFAULT_GOAL := help

VENV       := .venv
PY         := $(VENV)/bin/python
PIP        := $(VENV)/bin/pip
SBTEST     := $(VENV)/bin/sbtest
RUFF       := $(VENV)/bin/ruff
MYPY       := $(VENV)/bin/mypy
STAMP      := $(VENV)/.install-stamp
PYTHON     ?= python3

# ── parameters ────────────────────────────────────────────────────────────────
# SUITE is optional everywhere: with no suite, detectors all run with their defaults and no
# components do, which is exactly what judging an archive wants.
SUITE      ?=
RUN        ?=
OUT        ?=
DURATION   ?= 0
JSON_NAME  ?= findings.json
# KEEP=1 leaves the volumes, pods and CRs behind. Most post-mortems need this, so it is worth
# knowing the flag exists before the run rather than after.
KEEP       ?=
EXTRA      ?=

SUITE_ARG  := $(if $(SUITE),--suite $(SUITE),)

# ── venv ──────────────────────────────────────────────────────────────────────

$(VENV):
	@echo "==> creating $(VENV)"
	@$(PYTHON) -m venv $(VENV)

# Re-installs when pyproject changes; the stamp keeps every other target from paying for it.
$(STAMP): pyproject.toml | $(VENV)
	@echo "==> installing sbtest (editable) + dev extras"
	@$(PIP) install -q --upgrade pip
	@$(PIP) install -q -e '.[dev]'
	@touch $@

.PHONY: venv
venv: $(STAMP) ## Create the virtualenv and install sbtest.
	@$(PY) -c "import sbtest; print(f'sbtest {sbtest.__version__} ready in $(VENV)')"

# ── tests and lint ────────────────────────────────────────────────────────────

.PHONY: test
test: $(STAMP) ## Run the unit tests (no cluster needed).
	@$(PY) -m unittest discover -s tests -p 'test_*.py' -v

.PHONY: lint
lint: $(STAMP) ## Lint with ruff.
	@$(RUFF) check sbtest tests

.PHONY: types
types: $(STAMP) ## Type-check with mypy.
	@$(MYPY)

.PHONY: fmt
fmt: $(STAMP) ## Apply ruff's safe fixes.
	@$(RUFF) check --fix sbtest tests

# The gate runs every check even when an earlier one fails, then reports once. Stopping at
# the first failure means three round trips to find out you had three problems; a detector
# framework whose own checks are annoying to run is a framework nobody runs.
.PHONY: gate
gate: $(STAMP) ## Quality gate: ruff + mypy + tests. Non-zero if any part fails.
	@rc=0; \
	printf '\n==> ruff\n';  $(RUFF) check sbtest tests || rc=1; \
	printf '\n==> mypy\n';  $(MYPY) || rc=1; \
	printf '\n==> tests\n'; $(PY) -m unittest discover -s tests -p 'test_*.py' || rc=1; \
	printf '\n'; \
	if [ $$rc -eq 0 ]; then echo "GATE: PASS"; else echo "GATE: FAIL"; fi; \
	exit $$rc

.PHONY: check
check: gate ## Alias for `gate`.

# ── using it ──────────────────────────────────────────────────────────────────

.PHONY: detectors
detectors: $(STAMP) ## List the available detectors and their options.
	@$(SBTEST) detectors

.PHONY: components
components: $(STAMP) ## List the available components and their options.
	@$(SBTEST) components

.PHONY: suites
suites: $(STAMP) ## List the bundled suites.
	@for f in sbtest/suites/*.yaml; do \
	  name=$$(basename $$f .yaml); \
	  desc=$$($(PY) -c "import yaml; print(' '.join(yaml.safe_load(open('$$f')).get('description','').split()))"); \
	  printf '  %-18s %s\n' "$$name" "$$desc"; \
	done

.PHONY: analyze
analyze: $(STAMP) ## Judge a finished run directory. RUN=<dir> [SUITE=<name>]
ifndef RUN
	$(error RUN is required, e.g. make analyze RUN=../../operator/fio-mig-1787171993 SUITE=corruption-hunt)
endif
	@$(SBTEST) analyze "$(RUN)" $(SUITE_ARG) --json-name "$(JSON_NAME)" --freeze-table $(EXTRA)

.PHONY: collect
collect: $(STAMP) ## Collect from a live cluster, then judge. OUT=<dir> [SUITE=] [DURATION=]
ifndef OUT
	$(error OUT is required, e.g. make collect OUT=./runs/soak SUITE=migration-soak DURATION=600)
endif
	@$(SBTEST) collect "$(OUT)" $(SUITE_ARG) --duration "$(DURATION)" --judge \
	    --json-name "$(JSON_NAME)" $(EXTRA)

.PHONY: run
run: $(STAMP) ## Drive a full test run against a cluster, then judge it. SUITE=<name> [DURATION=] [KEEP=1] [OUT=<dir>]
ifndef SUITE
	$(error SUITE is required, e.g. make run SUITE=migration-full DURATION=7200 KEEP=1)
endif
	@$(SBTEST) run $(SUITE_ARG) --duration "$(DURATION)" \
	    $(if $(OUT),--outdir "$(OUT)",) $(if $(KEEP),--keep,) \
	    --json-name "$(JSON_NAME)" $(EXTRA)

# Judging every archived run at once: the cheapest way to see whether a new or changed
# detector fires where it should and stays quiet where it should not.
.PHONY: analyze-all
analyze-all: $(STAMP) ## Judge every operator/fio-mig-* run directory. [SUITE=<name>]
	@shopt -s nullglob; \
	found=0; \
	for d in ../../operator/fio-mig-*/; do \
	  found=1; \
	  echo "════════ $$d"; \
	  $(SBTEST) analyze "$$d" $(SUITE_ARG) --json-name "$(JSON_NAME)" 2>&1 \
	    | grep -E 'RESULT|skipped \(' || true; \
	done; \
	[ $$found -eq 1 ] || echo "no operator/fio-mig-* run directories found"

# ── housekeeping ──────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove caches and build leftovers (keeps the venv).
	@rm -rf .ruff_cache .mypy_cache **/__pycache__ */__pycache__ */*/__pycache__ *.egg-info src/*.egg-info
	@echo "cleaned"

.PHONY: distclean
distclean: clean ## Also remove the virtualenv.
	@rm -rf $(VENV)
	@echo "removed $(VENV)"

.PHONY: help
help: ## Show this help.
	@echo "sbtest — component/detector framework for large-scale test runs"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Parameters: RUN, OUT, SUITE, DURATION, JSON_NAME, EXTRA"
	@echo "Example:    make analyze RUN=../../operator/fio-mig-1787171993 SUITE=corruption-hunt"