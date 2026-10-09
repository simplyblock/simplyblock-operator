"""Bundled detectors. Importing this module registers them all.

Each module groups checks by the evidence they read, not by the subsystem they blame:
`ana` reads host path samples, `fio` reads the workload's own output, `nvme` reads a fabric
snapshot, `logs` reads collected container logs, `migration` reads the timeline, `kernel` reads
dmesg — the only source for what the *host* did about a fabric event — `control` reads the
control plane's own event log, `pnfs` reads the NFS client's counters and the client nodes'
NVMe counters, `reservations` reads the namespaces' NVMe reservations, `conntrack` reads the nodes'
connection tracking samples, `metadata` reads the namespace operations and cross-node checks
of workload.pnfs-metadata, `security` scans
whatever was collected, and `meta` judges the evidence itself rather than the system.
"""

from . import (  # noqa: F401
    ana,
    chaos,
    churn,
    conntrack,
    control,
    fence,
    fio,
    kernel,
    logs,
    meta,
    metadata,
    migration,
    nvme,
    pnfs,
    reservations,
    security,
    volume_ops,
)

__all__ = ["ana", "chaos", "churn", "conntrack", "control", "fence", "fio", "kernel", "logs", "meta",
           "metadata", "migration", "nvme", "pnfs", "reservations", "security",
           "volume_ops"]
