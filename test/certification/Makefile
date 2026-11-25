# The certification runs, one target per run (README.md). Every target runs
# against the cluster KUBECONFIG points at and writes under level1/results/.
#
#   make level1                   Level 1, every test pattern
#   make level1-block-multinode   Level 1, ReadWriteMany and ReadOnlyMany on raw block
#   make level1-dry-run           list the Level 1 tests of the main profile, run none

.PHONY: help level1 level1-block-multinode level1-dry-run

help:
	@sed -n 's/^#   //p' $(firstword $(MAKEFILE_LIST))

level1:
	./level1/run.sh main

level1-block-multinode:
	./level1/run.sh block-multinode

level1-dry-run:
	./level1/run.sh main -- --dry-run
