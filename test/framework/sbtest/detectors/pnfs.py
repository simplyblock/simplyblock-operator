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

A restart of the metadata server (`chaos.restart`) pauses every client until the guest has
booted and its grace period has ended, and a pause that begins at such a restart and stays
within `restart_pause_s` is that restart working, reported as information. A restart of
anything else is no excuse: the node plugin and the controller are not on the data path.
`pnfs.recovery` measures that pause per client, from the restart to the first write that
reached the client's namespace again.
"""

from __future__ import annotations

from collections.abc import Iterable
from datetime import datetime, timedelta
from typing import Any

from ..core import (
    BlockSample,
    Detector,
    Evidence,
    Finding,
    FioJob,
    NfsSample,
    PnfsVolume,
    Restart,
    SkipDetector,
    critical,
    detector,
    info,
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
        timeline: dict[str, list[NfsSample]] = {}
        for x in ev.nfs_timeline():
            timeline.setdefault(x.instance, []).append(x)
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
                detail = (f"LAYOUTGET {layouts}, READ {ops.get('READ', 0)}, "
                          f"WRITE {ops.get('WRITE', 0)}")
                evidence: dict[str, Any] = {"ops": ops, "server_io_ops": server_io}
                window = _server_io_window(timeline.get(pod, []))
                if window:
                    detail += f"; through the server from {window[0]:%H:%M:%S} to " \
                              f"{window[1]:%H:%M:%S}"
                    evidence["server_io_from"] = window[0].isoformat()
                    evidence["server_io_until"] = window[1].isoformat()
                yield warning(
                    self.name, title=f"{server_io} data operation(s) through the metadata "
                                     "server beside layouts",
                    subject=pod, detail=detail, evidence=evidence,
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
            # The longest pause a metadata server restart explains: the guest's boot, and
            # its grace period, which lasts until the slowest client has reclaimed.
            "restart_pause_s": 180,
            # The restarts that pause clients at all.
            "pausing_targets": ["mds"],
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
        jobs = {j.pod: j for j in ev.fio_jobs()}
        pausing = set(self.opt("pausing_targets") or [])
        self._restarts = [r for r in ev.restarts() if r.target in pausing]

        for vol in volumes:
            if not vol.lvol:
                continue
            for node in vol.nodes:
                subject = f"{vol.claim}@{node}"
                got = sorted(series.get((node, vol.lvol), []), key=lambda b: b.ts)
                got = _while_running(got, vol, node, jobs)
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

        trailing = _trailing_stall(got)
        if trailing > float(self.opt("max_stall_s")):
            yield critical(
                self.name, title=f"writes to the namespace stopped and never resumed "
                                 f"({trailing:.0f}s before the run ended)",
                subject=subject,
                detail=f"{node}: no sector written from {last.ts - timedelta(seconds=trailing):%H:%M:%S} "
                       f"to {last.ts:%H:%M:%S} while the run was going",
                evidence={"lvol": lvol, "node": node, "stopped_s": trailing},
                note="the client lost the direct path and did not get it back, so its I/O "
                     "went through the metadata server from then on; after an MDS restart "
                     "this is a device lookup made from the pod's mount namespace")
            return

        budget = float(self.opt("restart_pause_s"))
        unexplained, explained = 0.0, []
        for start, seconds in _stalls(got):
            if seconds <= float(self.opt("max_stall_s")):
                continue
            restart = _pausing_restart(self._restarts, start, seconds, budget)
            if restart:
                explained.append((seconds, restart))
            else:
                unexplained = max(unexplained, seconds)
        if unexplained:
            yield warning(
                self.name, title=f"writes to the namespace stalled for {unexplained:.0f}s",
                subject=subject,
                detail=f"{node}: no sector written for {unexplained:.0f}s while the run was "
                       "going",
                evidence={"lvol": lvol, "node": node, "stall_s": unexplained},
                note="the client stopped writing to its namespace for a while; if the "
                     "NFS counters grew meanwhile, its I/O went through the server")
        for seconds, restart in explained:
            yield info(
                self.name, title=f"writes paused for {seconds:.0f}s across a {restart.target} "
                                 "restart",
                subject=subject,
                detail=f"{node}: paused from {restart.deleted:%H:%M:%S}, within the "
                       f"{budget:.0f}s a restart may take",
                evidence={"lvol": lvol, "node": node, "stall_s": seconds,
                          "restart": restart.pod})


@detector
class Recovery(Detector):
    """How long after a metadata server restart each client wrote to its own namespace
    again: the time to the direct path, which is what a restart costs a client.

    A client's writes pause from about when the guest goes down until its grace period
    has ended and the client holds a layout again. That pause is found as the stall in the
    client's namespace counters that begins at the restart, and the time is counted to the
    first sample that saw a write again. A client whose writes never paused is reported
    with zero, and one whose writes never came back is a warning. `pnfs.device-io` judges
    the pause itself, and this reports what it measured.
    """

    name = "pnfs.recovery"
    summary = "the time from a metadata server restart to each client's direct path"

    def defaults(self) -> dict[str, Any]:
        # The device-io detector's restart_pause_s: the same restart, measured from the
        # restart rather than from the last write before it.
        return {"budget_s": 180, "targets": ["mds"]}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        targets = set(self.opt("targets") or [])
        restarts = [r for r in ev.restarts() if r.target in targets]
        if not restarts:
            raise SkipDetector("no metadata server restart in this run")
        volumes, samples = ev.pnfs_volumes(), ev.block_samples()
        if not volumes or not samples:
            raise SkipDetector("no pNFS volumes or NVMe I/O samples to measure recovery on")
        series: dict[tuple[str, str], list[BlockSample]] = {}
        for x in samples:
            series.setdefault((x.node, x.uuid), []).append(x)
        budget = float(self.opt("budget_s"))
        for r in restarts:
            for vol in volumes:
                for node in vol.nodes:
                    got = sorted(series.get((node, vol.lvol), []), key=lambda b: b.ts)
                    if len(got) < 2:
                        continue
                    yield self._judge(r, f"{vol.claim}@{node}", node, got, budget)

    def _judge(self, r: Restart, subject: str, node: str, got: list[BlockSample],
               budget: float) -> Finding:
        evidence: dict[str, Any] = {"restart": r.pod, "node": node}
        resumed = _resumed_after(got, r)
        if resumed is None:
            return warning(
                self.name, title=f"not back on the direct path after the {r.target} restart",
                subject=subject,
                detail=f"{node}: no write reached the namespace after the pause that began "
                       f"at {r.deleted:%H:%M:%S}",
                evidence=evidence,
                note="the client's I/O went through the metadata server, or nowhere, for "
                     "the rest of the run")
        took = max(0.0, (resumed - r.deleted).total_seconds())
        evidence["direct_after_s"] = round(took)
        make = warning if took > budget else info
        return make(
            self.name, title=f"back on the direct path {took:.0f}s after the {r.target} "
                             "restart",
            subject=subject,
            detail=f"{node}: {r.target} {r.pod} deleted {r.deleted:%H:%M:%S}, writes to the "
                   f"namespace again at {resumed:%H:%M:%S} (budget {budget:.0f}s)",
            evidence=evidence)


def _resumed_after(got: list[BlockSample], r: Restart) -> datetime | None:
    """When the client wrote to its namespace again after the restart: the first sample
    after the pause that began at it, the restart itself when no pause began there, and
    None when the pause lasted to the end of the series."""
    margin = timedelta(seconds=30)
    for start, seconds in _stalls(got):
        if not r.deleted - margin <= start <= r.deleted + margin:
            continue
        flat_until = start + timedelta(seconds=seconds)
        return next((b.ts for b in got if b.ts > flat_until), None)
    return r.deleted


def _server_io_window(samples: list[NfsSample]) -> tuple[datetime, datetime] | None:
    """From the last sample before data started going through the server to the last one
    that saw it grow, or None when the timeline never saw it grow."""
    ordered = sorted(samples, key=lambda x: x.ts)
    first = last = None
    for prev, cur in zip(ordered, ordered[1:], strict=False):
        if cur.read + cur.write > prev.read + prev.write:
            first = first or prev.ts
            last = cur.ts
    return (first, last) if first and last else None


def _stalls(got: list[BlockSample]) -> list[tuple[datetime, float]]:
    """Every span over which the written-sector counter did not move, as (start, seconds):
    from the last sample that saw it move to the last that still did not. A span still
    open at the end of the series is included."""
    out: list[tuple[datetime, float]] = []
    since = got[0]
    for prev, cur in zip(got, got[1:], strict=False):
        if cur.write_sectors > prev.write_sectors:
            if prev is not since:
                out.append((since.ts, (prev.ts - since.ts).total_seconds()))
            since = cur
    if since is not got[-1]:
        out.append((since.ts, (got[-1].ts - since.ts).total_seconds()))
    return out


def _pausing_restart(restarts: list[Restart], start: datetime, seconds: float,
                     budget: float) -> Restart | None:
    """The restart that explains a pause, when one began near the pause's start and the
    pause stayed within the budget. The margin covers the sampling interval, since a pause
    is dated from the last sample that still saw a write."""
    if seconds > budget:
        return None
    margin = timedelta(seconds=30)
    for r in restarts:
        if r.deleted - margin <= start <= r.deleted + margin:
            return r
    return None


def _trailing_stall(got: list[BlockSample]) -> float:
    """How long the written-sector counter had stood still when the series ended."""
    last = got[-1]
    for prev, cur in zip(reversed(got[:-1]), reversed(got), strict=False):
        if cur.write_sectors > prev.write_sectors:
            return (last.ts - cur.ts).total_seconds()
    return (last.ts - got[0].ts).total_seconds()


def _while_running(got: list[BlockSample], vol: PnfsVolume, node: str,
                   jobs: dict[str, FioJob]) -> list[BlockSample]:
    """The samples taken while the volume's fio instances on node were running.

    Every instance counts its runtime from its own start, and those starts spread over the
    time layout took, so a node whose instances began early goes quiet before the run ends.
    That is fio being done, not a stall. Judged from the first instance's start to the last
    one's end; the whole series when the run recorded no timing, as older runs did not.
    """
    windows = []
    for evidence, where in vol.instances.items():
        job = jobs.get(evidence)
        if where == node and job and job.start and job.runtime_s > 0:
            windows.append((job.start, job.start + timedelta(seconds=job.runtime_s)))
    if not windows:
        return got
    start, end = min(w[0] for w in windows), max(w[1] for w in windows)
    inside = [s for s in got if start <= s.ts <= end]
    return inside if len(inside) >= 2 else got

