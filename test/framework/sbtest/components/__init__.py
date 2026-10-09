"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric and its I/O counters, `reservations` snapshots each namespace's
NVMe reservation, `events` pulls the control plane's own event log, `workloads` holds what
drives fio against volumes: a common base and one module per kind of run, `chaos` restarts
the pods a run depends on while it runs, `conntrack` samples each node's NFS flows in
its connection tracking table, `volume_ops` expands and snapshots live pNFS volumes, `versions` records what was
deployed, and `nfs`
samples the NFS client's counters over the run. `kube` and `sbctl` hold no components: they
are the pieces the components share.
"""

from . import (  # noqa: F401
    chaos,
    conntrack,
    events,
    fence,
    logs,
    migration,
    nfs,
    nvme,
    reservations,
    versions,
    volume_ops,
    workloads,
)

__all__ = ["chaos", "conntrack", "events", "fence", "logs", "migration", "nfs", "nvme", "reservations",
           "versions", "volume_ops", "workloads"]
