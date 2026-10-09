"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric and its I/O counters, `reservations` snapshots each namespace's
NVMe reservation, `events` pulls the control plane's own event log, `workloads` holds what
drives fio against volumes: a common base and one module per kind of run, and `chaos`
restarts the pods a run depends on while it runs. `nfs`, `kube` and `sbctl` hold no
components: they are the pieces the components share.
"""

from . import chaos, events, logs, migration, nvme, reservations, workloads  # noqa: F401

__all__ = ["chaos", "events", "logs", "migration", "nvme", "reservations", "workloads"]
