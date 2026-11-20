"""Reading an NFS client's own per-operation counters out of /proc/self/mountstats.

The counters are the one witness to whether pNFS did what it is for. A pNFS mount that fell
back to plain NFS still mounts, still reads and writes, and still passes fio's verification:
the data is right, it just went through the metadata server. Only the counts tell the two
apart. LAYOUTGET says the client was handed layouts, and READ and WRITE count the data that
went through the server instead of to the shared device.

`nfs.mountstats` reads those counters during the run as well, per fio instance, into
nfs-timeline.csv. End totals say that data went through the server. The timeline says
when, which is what ties it to a restart, a recall, or nothing at all.
"""

from __future__ import annotations

import threading
from typing import Any

from ..core import Component, NfsSample, RunContext, component, now_utc
from . import kube


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



def mount_sample(stats: str, mountpoint: str) -> dict[str, int] | None:
    """LAYOUTGET, READ, and WRITE of the mount at mountpoint, and how often its transport
    has connected, or None when there is no such mount. A reconnect is how a server restart
    shows on the client."""
    ops = mount_ops(stats, mountpoint)
    if ops is None:
        return None
    out = {op: ops.get(op, 0) for op in ("LAYOUTGET", "READ", "WRITE")}
    out["connects"] = _connects(stats, mountpoint)
    return out


def _connects(stats: str, mountpoint: str) -> int:
    """The connect count of the mount's transport: `xprt: tcp <srcport> <binds> <connects>`."""
    inside = False
    for line in stats.splitlines():
        if line.startswith("device "):
            inside = f" mounted on {mountpoint} " in line
            continue
        key, _, value = line.strip().partition(":")
        if inside and key == "xprt":
            fields = value.split()
            if len(fields) > 3 and fields[0] != "udp" and fields[3].isdigit():
                return int(fields[3])
    return 0


_TIMELINE_HEADER = "ts,instance,pod,container,layoutget,read,write,connects\n"


def write_timeline(path: str, samples: list[NfsSample]) -> None:
    with open(path, "w") as fh:
        fh.write(_TIMELINE_HEADER)
        for x in sorted(samples, key=lambda x: (x.ts, x.instance)):
            fh.write(f"{x.ts.strftime('%Y-%m-%dT%H:%M:%SZ')},{x.instance},{x.pod},"
                     f"{x.container},{x.layoutget},{x.read},{x.write},{x.connects}\n")


@component
class MountstatsSampler(Component):
    """Sample each pNFS fio pod's NFS client counters on an interval.

    One read per pod: its containers share the mount, so the counts are the pod's, and
    each instance in the pod gets the same sample under its own name, which is how the
    detectors address an instance. The instances come from the pNFS workload
    (`ctx.shared["pnfs.instances"]`), which publishes them once its pods run.
    """

    name = "nfs.mountstats"
    summary = "sample each pNFS fio pod's NFS client counters on an interval"
    namespace_options = {"namespace": "test"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {"namespace": None, "mountpoint": "/data", "interval_s": 10.0}

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._samples: list[NfsSample] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def _sample(self, ctx: RunContext) -> None:
        by_pod: dict[str, list[dict[str, str]]] = {}
        for inst in ctx.shared.get("pnfs.instances") or []:
            by_pod.setdefault(inst["pod"], []).append(inst)
        for pod, instances in sorted(by_pod.items()):
            stats = kube.exec_sh(self.opt("namespace"), pod, "cat /proc/self/mountstats",
                                 container=instances[0]["container"], timeout=30)
            counts = mount_sample(stats, str(self.opt("mountpoint")))
            if counts is None:
                continue
            ts = now_utc()
            batch = [NfsSample(ts=ts, instance=i["evidence"], pod=pod, container=i["container"],
                               layoutget=counts["LAYOUTGET"], read=counts["READ"],
                               write=counts["WRITE"], connects=counts["connects"])
                     for i in instances]
            with self._lock:
                self._samples.extend(batch)

    def start(self, ctx: RunContext) -> None:
        if not ctx.shared.get("pnfs.instances"):
            ctx.log.warn(f"{self.name}: no pNFS fio instances published; nothing to sample")
            return
        interval = float(self.opt("interval_s"))

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                self._sample(ctx)
                self._stop.wait(interval)

        self._thread = threading.Thread(target=loop, name="nfs-mountstats", daemon=True)
        self._thread.start()
        ctx.log.info(f"{self.name}: sampling every {interval}s")

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=30)
            self._thread = None
            # One last reading after the workload stopped, so the final interval counts.
            self._sample(ctx)

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            samples = list(self._samples)
        if samples:
            write_timeline(ctx.path("nfs-timeline.csv"), samples)
        ctx.log.info(f"{self.name}: {len(samples)} sample(s)")
