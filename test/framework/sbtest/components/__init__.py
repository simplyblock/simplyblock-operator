"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric and its I/O counters, `events` pulls the control plane's own event
log, and `workloads` holds what drives fio against volumes: a common base and one module per
kind of run. `nfs`, `kube` and `sbctl` hold no components: they are the pieces the
components share.
"""

from . import events, logs, migration, nvme, workloads  # noqa: F401

__all__ = ["events", "logs", "migration", "nvme", "workloads"]
