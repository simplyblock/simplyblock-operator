# The certification runs, one target per run (README.md). Every target runs
# against the cluster KUBECONFIG points at and writes under level1/results/.
#
#   make level1                   Level 1, every test pattern
#   make level1-block-multinode   Level 1, ReadWriteMany and ReadOnlyMany on raw block
#   make level1-dry-run           list the Level 1 tests of the main profile, run none
#   make level2                   Level 2, the KubeVirt storage checkup
#   make level3-preflight         Level 3, the harness's checks of cluster and plan
#   make level3-load-80           Level 3 run 1, latency under external load (EXTERNAL_LOAD=...)
#   make level3-no-load-80        Level 3 run 2, the VM criteria
#   make level3-package           Level 3, the archive to submit

.PHONY: help level1 level1-block-multinode level1-dry-run level2 level3-preflight level3-load-80 level3-no-load-80 level3-package

help:
	@sed -n 's/^#   //p' $(firstword $(MAKEFILE_LIST))

level1:
	./level1/run.sh main

level1-block-multinode:
	./level1/run.sh block-multinode

level1-dry-run:
	./level1/run.sh main -- --dry-run

level2:
	./level2/run.sh

level3-preflight:
	./level3/run.sh preflight

level3-load-80:
	./level3/run.sh load-80

level3-no-load-80:
	./level3/run.sh no-load-80

level3-package:
	./level3/run.sh package
