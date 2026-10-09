"""Workloads: the components that create the load a run judges.

Every workload is a `base.FioWorkload`: it decides what to provision and how to lay pods out,
and the base applies it, waits for it, collects each fio instance's evidence and cleans up.
`fio` holds the fio building blocks they share. `churn` is the one exception to the base: it
keeps short-lived pods joining and leaving pNFS volumes during the run, so it owns each pod's
whole life itself. `metadata` is the other: it runs no fio, only namespace operations on the
shared pNFS volumes, checked from another node, through `metadata_agent`, the program its
pods run. Importing this package registers them all.
"""

from . import churn, metadata, pnfs_rwx, volumemigration  # noqa: F401

__all__ = ["churn", "metadata", "pnfs_rwx", "volumemigration"]
