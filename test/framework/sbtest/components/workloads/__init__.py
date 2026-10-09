"""Workloads: the components that create the load a run judges.

Every workload is a `base.FioWorkload`: it decides what to provision and how to lay pods out,
and the base applies it, waits for it, collects each fio instance's evidence and cleans up.
`fio` holds the fio building blocks they share. `churn` is the one exception to the base: it
keeps short-lived pods joining and leaving pNFS volumes during the run, so it owns each pod's
whole life itself. Importing this package registers them all.
"""

from . import churn, pnfs_rwx, volumemigration  # noqa: F401

__all__ = ["churn", "pnfs_rwx", "volumemigration"]
