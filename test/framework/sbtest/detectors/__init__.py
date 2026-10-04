"""Bundled detectors. Importing this module registers them all.

Each module groups checks by the evidence they read, not by the subsystem they blame:
`ana` reads host path samples, `fio` reads the workload's own output, `nvme` reads a fabric
snapshot, `logs` reads collected container logs, `migration` reads the timeline, `kernel` reads
dmesg — the only source for what the *host* did about a fabric event — `control` reads the
control plane's own event log, `security` scans whatever was collected, and `meta` judges the
evidence itself rather than the system.
"""

from . import (  # noqa: F401
    ana,
    control,
    fio,
    kernel,
    logs,
    meta,
    migration,
    nvme,
    security,
)

__all__ = ["ana", "control", "fio", "kernel", "logs", "meta", "migration", "nvme", "security"]
