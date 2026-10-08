"""Whether pNFS sent the data where it is supposed to go.

A pNFS client given a layout reads and writes the shared NVMe-oF namespace directly, and the
metadata server only hands out layouts and records commits. A client that gets no layout
falls back to plain NFS, and everything still works: the mount mounts, fio's verification
passes, the data is right. It just all went through the metadata server, which is the one
thing pNFS is meant to avoid. No fio check can tell the two apart, so these two read the
evidence that can, from both ends of the data path:

* `pnfs.layout` reads the NFS client's own counters: whether it was handed layouts at all,
  and whether data still went through the server beside them.
* `pnfs.device-io` reads the client node's NVMe counters: whether the namespace under the
  volume is attached on each consuming node, and whether reads and writes on it kept
  growing for the length of the run.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from ..core import (
    BlockSample,
    Detector,
    Evidence,
    Finding,
    SkipDetector,
    critical,
    detector,
    warning,
)


@detector
class Layout(Detector):
    name = "pnfs.layout"
    summary = "a pNFS mount that got no layouts, or sent data through the metadata server"

    def defaults(self) -> dict[str, Any]:
        # READ and WRITE through the server beside layouts. Zero, because with a layout
        # and direct I/O a client has no reason to send data through the server at all,
        # and the green run on a correct kernel measured exactly zero.
        return {"max_server_io_ops": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        measured = {p: ops for p in ev.pods() if (ops := ev.nfs_ops(p))}
        if not measured:
            raise SkipDetector("no NFS mount statistics; this run used no pNFS volume")
        for pod, ops in sorted(measured.items()):
            layouts = ops.get("LAYOUTGET", 0)
            server_io = ops.get("READ", 0) + ops.get("WRITE", 0)
            if layouts == 0:
                yield critical(
                    self.name, title="no layout fetched: pNFS ran as plain NFS",
                    subject=pod,
                    detail=f"LAYOUTGET 0, READ {ops.get('READ', 0)}, "
                           f"WRITE {ops.get('WRITE', 0)}",
                    evidence={"ops": ops},
                    note="every byte went through the metadata server; check that its "
                         "kernel issues SCSI layouts and the export carries pnfs")
            elif server_io > int(self.opt("max_server_io_ops")):
                yield warning(
                    self.name, title=f"{server_io} data operation(s) through the metadata "
                                     "server beside layouts",
                    subject=pod,
                    detail=f"LAYOUTGET {layouts}, READ {ops.get('READ', 0)}, "
                           f"WRITE {ops.get('WRITE', 0)}",
                    evidence={"ops": ops, "server_io_ops": server_io},
                    note="layouts were issued but some I/O fell back to the server, e.g. "
                         "after a recall or for a range the layout did not cover")


@detector
class DeviceIO(Detector):
    name = "pnfs.device-io"
    summary = "a pNFS client node whose NVMe-oF namespace did not see the data"

    def defaults(self) -> dict[str, Any]:
        return {
            # The longest the namespace's counters may stand still while the run is going.
            # Longer than a layout recall and re-fetch, far shorter than any real outage.
            "max_stall_s": 60,
            # Reads are required to grow too, which needs a workload that reads. A write-only
            # run turns this off rather than failing on it.
            "require_reads": True,
        }

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        volumes = ev.pnfs_volumes()
        samples = ev.block_samples()
        if not volumes:
            raise SkipDetector("no pNFS volumes recorded for this run")
        if not samples:
            raise SkipDetector("no NVMe I/O samples; enable the nvme.iostat component")

        series: dict[tuple[str, str], list[BlockSample]] = {}
        for s in samples:
            series.setdefault((s.node, s.uuid), []).append(s)

        for vol in volumes:
            if not vol.lvol:
                continue
            for node in vol.nodes:
                subject = f"{vol.claim}@{node}"
                got = sorted(series.get((node, vol.lvol), []), key=lambda b: b.ts)
                if len(got) < 2:
                    yield critical(
                        self.name, title="namespace not attached on a client node",
                        subject=subject,
                        detail=f"lvol {vol.lvol} has no NVMe device on {node}, which "
                               "mounts the volume",
                        evidence={"lvol": vol.lvol, "node": node, "samples": len(got)},
                        note="without the namespace the client cannot use a layout, so "
                             "all of its I/O went through the metadata server")
                    continue
                yield from self._judge(subject, vol.lvol, node, got)

    def _judge(self, subject: str, lvol: str, node: str,
               got: list[BlockSample]) -> Iterable[Finding]:
        first, last = got[0], got[-1]
        wrote = last.write_sectors - first.write_sectors
        read = last.read_sectors - first.read_sectors
        flat = [k for k, v in (("writes", wrote), ("reads", read)) if v <= 0
                and (k == "writes" or self.opt("require_reads"))]
        if flat:
            yield critical(
                self.name, title=f"no {' or '.join(flat)} reached the namespace on the client",
                subject=subject,
                detail=f"{node}: {wrote} sector(s) written, {read} read over "
                       f"{(last.ts - first.ts).total_seconds():.0f}s",
                evidence={"lvol": lvol, "node": node, "write_sectors": wrote,
                          "read_sectors": read},
                note="the pod's I/O went through the metadata server instead of to its "
                     "NVMe-oF namespace")
            return

        stall = _longest_stall(got)
        if stall > float(self.opt("max_stall_s")):
            yield warning(
                self.name, title=f"writes to the namespace stalled for {stall:.0f}s",
                subject=subject,
                detail=f"{node}: no sector written for {stall:.0f}s while the run was going",
                evidence={"lvol": lvol, "node": node, "stall_s": stall},
                note="the client stopped writing to its namespace for a while; if the "
                     "NFS counters grew meanwhile, its I/O went through the server")


def _longest_stall(got: list[BlockSample]) -> float:
    """The longest span over which the written-sector counter did not move."""
    longest = 0.0
    since = got[0]
    for prev, cur in zip(got, got[1:], strict=False):
        if cur.write_sectors > prev.write_sectors:
            since = cur
            continue
        longest = max(longest, (cur.ts - since.ts).total_seconds())
    return longest
