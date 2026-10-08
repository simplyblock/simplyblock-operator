"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric and its I/O counters, `events` pulls the control plane's own event
log, and `workload` and `pnfs` drive fio against volumes. `fio`, `nfs`, `kube` and `sbctl`
hold no components: they are the pieces those components share.
"""

from . import events, logs, migration, nvme, pnfs, workload  # noqa: F401

__all__ = ["events", "logs", "migration", "nvme", "pnfs", "workload"]
