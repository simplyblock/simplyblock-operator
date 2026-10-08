"""Reading an NFS client's own per-operation counters out of /proc/self/mountstats.

The counters are the one witness to whether pNFS did what it is for. A pNFS mount that fell
back to plain NFS still mounts, still reads and writes, and still passes fio's verification:
the data is right, it just went through the metadata server. Only the counts tell the two
apart. LAYOUTGET says the client was handed layouts, and READ and WRITE count the data that
went through the server instead of to the shared device.
"""

from __future__ import annotations


def mount_ops(stats: str, mountpoint: str) -> dict[str, int] | None:
    """The operation counts of the NFS mount at mountpoint, or None when there is none.

    mountstats lists every mount, each under a `device ... mounted on ... with fstype ...`
    header, and a count belongs to the header above it: reading the counts without the
    header adds up every NFS mount on the node. None rather than an empty dict, so a mount
    that is not there cannot be read as one that did nothing.
    """
    ops: dict[str, int] = {}
    inside = per_op = found = False
    for line in stats.splitlines():
        if line.startswith("device "):
            inside = f" mounted on {mountpoint} " in line
            found = found or inside
            per_op = False
            continue
        if not inside:
            continue
        stripped = line.strip()
        if stripped == "per-op statistics":
            per_op = True
            continue
        if not per_op:
            continue
        name, sep, rest = stripped.partition(":")
        fields = rest.split()
        if not sep or not fields:
            continue
        try:
            ops[name] = int(fields[0])
        except ValueError:
            continue
    return ops if found else None
