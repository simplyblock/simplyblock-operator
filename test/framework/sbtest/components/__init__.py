"""Bundled components. Importing this module registers them all.

Grouped by what they touch: `logs` collects container logs (live or post-run), `nvme`
observes the host fabric, `events` pulls the control plane's own event log.
"""

from . import events, logs, migration, nvme, workload  # noqa: F401

__all__ = ["events", "logs", "migration", "nvme", "workload"]
