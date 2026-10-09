"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric and its I/O counters, `events` pulls the control plane's own event
log, `workloads` holds what drives fio against volumes: a common base and one module per
kind of run, `chaos` restarts the pods a run depends on while it runs, `versions` records
what was deployed, and `nfs` samples the NFS client's counters over the run. `kube` and
`sbctl` hold no components: they are the pieces the components share.
"""

from . import chaos, events, logs, migration, nfs, nvme, versions, workloads  # noqa: F401

__all__ = ["chaos", "events", "logs", "migration", "nfs", "nvme", "versions",
           "workloads"]
