"""Workloads: the components that create the load a run judges.

Every workload is a `base.FioWorkload`: it decides what to provision and how to lay pods out,
and the base applies it, waits for it, collects each fio instance's evidence and cleans up.
`fio` holds the fio building blocks they share. Importing this package registers them all.
"""

from . import pnfs_rwx, volumemigration  # noqa: F401

__all__ = ["pnfs_rwx", "volumemigration"]
