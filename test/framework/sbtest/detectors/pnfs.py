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

import json
import re
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
                yield from self._judge(subject, vol.lvol, node, got,
                                       _server_ops(ev, vol, node))

    def _judge(self, subject: str, lvol: str, node: str, got: list[BlockSample],
               server: dict[str, int] | None) -> Iterable[Finding]:
        counted = "" if server is None else \
            f"; NFS WRITE {server['WRITE']}, NFS READ {server['READ']}"
        server_writes = None if server is None else server["WRITE"]
        hung = server_writes == 0
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
                       f"{(last.ts - first.ts).total_seconds():.0f}s{counted}",
                evidence={"lvol": lvol, "node": node, "write_sectors": wrote,
                          "read_sectors": read, "server_ops": server},
                note=_flat_note(flat, server))
            return

        trailing = _trailing_stall(got)
        if trailing > float(self.opt("max_stall_s")):
            where = ("on the namespace and at the metadata server" if hung
                     else "to the namespace")
            yield critical(
                self.name, title=f"writes {where} stopped and never resumed "
                                 f"({trailing:.0f}s before the run ended)",
                subject=subject,
                detail=f"{node}: no sector written from {last.ts - timedelta(seconds=trailing):%H:%M:%S} "
                       f"to {last.ts:%H:%M:%S} while the run was going{counted}",
                evidence={"lvol": lvol, "node": node, "stopped_s": trailing,
                          "server_ops": server},
                note=_HUNG_NOTE if hung else
                "the client lost the direct path and did not get it back, so its I/O "
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


def _server_ops(ev: Evidence, vol: PnfsVolume, node: str) -> dict[str, int] | None:
    """The NFS READs and WRITEs the volume's mounts on node sent to the metadata server, or
    None when an instance has no mount statistics and the counts would be partial.

    Every instance of a pod carries a copy of the pod's one mount's counters, so each mount
    is counted once: by the pod the volume map names, or, in runs from before it named
    one, by the copy itself, since two different mounts never agree on every counter.
    """
    mounts: dict[str, dict[str, int]] = {}
    for inst, where in vol.instances.items():
        if where != node:
            continue
        ops = ev.nfs_ops(inst)
        if not ops:
            return None
        mounts[vol.mounts.get(inst) or json.dumps(ops, sort_keys=True)] = ops
    if not mounts:
        return None
    return {k: sum(m.get(k, 0) for m in mounts.values()) for k in ("READ", "WRITE")}


def _flat_note(flat: list[str], server: dict[str, int] | None) -> str:
    """Where the I/O a namespace never saw went, by the mount's own counters."""
    if "writes" in flat:
        if server is not None and server["WRITE"] == 0:
            return _HUNG_NOTE
        return ("the pod's I/O went through the metadata server instead of to its "
                "NVMe-oF namespace")
    if server is None:
        return "the pod's reads did not reach its NVMe-oF namespace"
    if server["READ"]:
        return ("the pod's reads went through the metadata server instead of to its "
                "NVMe-oF namespace")
    return ("no read reached the namespace or the metadata server: the workload read "
            "nothing, or its reads hung")


#: The note for writes that stopped on the namespace without moving to the server.
_HUNG_NOTE = ("the mount sent no WRITE to the metadata server either, so the writes did "
              "not fall back to the server: they stopped, which is a hung client or fio, "
              "not a lost direct path")


#: A path the metadata server's guest connected: its kernel's line for a new controller.
_GUEST_CTRL = re.compile(r'new ctrl: NQN "[^"]*:lvol:(?P<lvol>[0-9a-zA-Z-]+)", addr (?P<ip>[0-9.]+):')

#: A connect the guest's kernel gave up on. It names no address: which path failed is what
#: the comparison with the clients' paths finds.
_GUEST_CONNECT_FAILED = re.compile(r"failed to connect socket: -(?P<errno>\d+)")

#: A path a client holds, by the volume in its NQN.
_LVOL_NQN = re.compile(r":lvol:(?P<lvol>[0-9a-zA-Z-]+)$")


@detector
class Multipath(Detector):
    """Whether the metadata server's guest reached every path a volume has.

    The guest attaches each volume it exports over NVMe/TCP, as a client does, and needs
    the same paths: a volume left on one path stops when that path does. Two comparisons
    find a missing path, and neither needs the volume's clients to outlive it:

    * A volume the guest attached on fewer paths than its clients hold to it, or, for a
      volume no client holds any more, than the most any of the guest's volumes has in
      the guest or on a client. Volumes the guest does not export set no count.
    * An address the clients connect to, for any volume, that the guest never connected
      to, for any volume. It names the path the short volumes are missing, and is
      reported only beside them: a volume the guest does not export, such as its own
      state disk, can be served from an address no export uses.

    The case that produces it is a metadata server on a node that runs a storage node: the
    node's NVMe/TCP listeners are bound to its data interface, and neither the pod network
    nor a macvlan delivers a connection to its own node on that interface.
    """

    name = "pnfs.multipath"
    summary = "a volume the metadata server's guest attached on fewer paths than it has"

    def defaults(self) -> dict[str, Any]:
        return {"container_log": "mds-runner"}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        guest: dict[str, set[str]] = {}
        failed: dict[str, int] = {}
        any_line = False
        for line in ev.container_log(str(self.opt("container_log"))):
            any_line = True
            if m := _GUEST_CTRL.search(line):
                guest.setdefault(m["lvol"], set()).add(m["ip"])
            elif m := _GUEST_CONNECT_FAILED.search(line):
                failed[m["errno"]] = failed.get(m["errno"], 0) + 1
        if not any_line:
            raise SkipDetector("no metadata server log collected")
        if not guest:
            raise SkipDetector("the metadata server's guest attached no volume")

        clients: dict[str, set[str]] = {}
        for c in ev.nvme_controllers():
            if m := _LVOL_NQN.search(c.nqn):
                clients.setdefault(m["lvol"], set()).add(c.address.rsplit(":", 1)[0])

        exported = {lvol: clients[lvol] for lvol in guest if lvol in clients}
        paths = max(len(p) for p in [*guest.values(), *exported.values()])
        short = sorted(lvol for lvol, reached in guest.items()
                       if len(reached) < (len(exported[lvol]) if lvol in exported else paths))
        reached_any = set().union(*guest.values())
        unreached = sorted(set().union(*clients.values()) - reached_any) if clients and short else []

        for ip in unreached:
            yield critical(
                self.name, f"the metadata server's guest never reached {ip}", subject=ip,
                detail=f"{len(short)} volume(s) of {len(guest)} ran on fewer than {paths} path(s) "
                       f"in the guest: {', '.join(short[:10])}{' ...' if len(short) > 10 else ''}",
                note="The clients reach this address and the guest never did. A metadata "
                     "server on the node that owns the address cannot reach it through the "
                     "pod network or a macvlan.")
        if short and not unreached:
            yield critical(
                self.name, f"{len(short)} volume(s) ran on fewer paths in the guest than volumes have",
                subject="paths",
                detail=f"fewer than {paths} path(s): {', '.join(short[:10])}"
                       f"{' ...' if len(short) > 10 else ''}",
                note="No client path names the missing address, so it is known only by the "
                     "count.")
        if failed:
            total = sum(failed.values())
            yield warning(
                self.name, f"the guest failed {total} NVMe/TCP connect(s)", subject="connect",
                detail=", ".join(f"-{e}: {n}" for e, n in sorted(failed.items())),
                note="110 is a timeout and 113 an unreachable host. Every new export waits for "
                     "these before it is served.")
        yield info(self.name, f"{len(guest)} volume(s) attached in the guest, {paths} path(s) each "
                   "expected", subject="coverage",
                   detail=f"{len(guest) - len(short)} with every path, {len(short)} short")


@detector
class MdsPlacementCoverage(Detector):
    """Whether the metadata server ran on a storage node at least once.

    Its guest reaches the storage nodes on other nodes as a client does, and the one on its
    own node by another way or not at all. A run in which it never shared a node with a
    storage node never exercised that, and passing it says nothing about it. On the Talos
    lab the scheduler preferred the control-plane node, which runs no storage node, in every
    run on disk.
    """

    name = "pnfs.mds-placement"
    summary = "a run in which the metadata server never ran on a storage node"

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        placement = ev.mds_placement()
        if placement is None:
            raise SkipDetector("the run did not record where the metadata server ran "
                               "(enable the pnfs.mds-placement component)")
        if not placement.instances:
            raise SkipDetector("no metadata server instance was seen running")
        on_storage = [i for i in placement.instances if i.node in placement.storage_nodes]
        nodes = sorted({i.node for i in placement.instances})
        if not on_storage:
            yield critical(
                self.name, "the metadata server never ran on a storage node", subject="placement",
                detail=f"{len(placement.instances)} instance(s) on {', '.join(nodes)}; storage nodes: "
                       f"{', '.join(placement.storage_nodes) or 'none recorded'}",
                note="Its guest's path to its own node's storage was never exercised. Set "
                     "pin_to_storage_node on pnfs.mds-placement to run that case on purpose.")
        yield info(self.name, f"{len(on_storage)} of {len(placement.instances)} metadata server "
                   f"instance(s) ran on a storage node", subject="placement",
                   detail=f"nodes: {', '.join(nodes)}; pinned: {placement.pinned}")
