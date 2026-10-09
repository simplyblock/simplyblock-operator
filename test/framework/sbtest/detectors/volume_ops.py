"""Whether each operation chaos.volume-ops ran on a live pNFS volume completed, reached the
clients, and left fio running.

An expansion is two halves: the claim grows, and the operator re-assembles the export, which
grows XFS in the metadata server's guest. A client sees the new size only after the second,
so a claim that grew while `df` on a client never moved is the export half failing, and it is
critical. So are a snapshot that never became ready, a restore that never read back, and a
restored marker whose checksum differs from the one written before the snapshot, which is a
snapshot missing data the client had synced.

A completed operation can still have hurt the run: the client's namespace standing still
while it ran. That is measured from the same block samples and with the same rule as
`pnfs.device-io` (`_stalls`), and reported here as a warning tied to the operation, since
the stall's cause is what the operator of the run needs, and `pnfs.device-io` reports the
stall without one.
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
    PnfsVolume,
    SkipDetector,
    VolumeOp,
    critical,
    detector,
    info,
    warning,
)
from .pnfs import _stalls


@detector
class VolumeOps(Detector):
    name = "pnfs.volume-ops"
    summary = "an expansion or snapshot of a live pNFS volume that did not complete or reach clients"

    def defaults(self) -> dict[str, Any]:
        return {
            # pnfs.device-io's threshold: the longest the namespace may stand still.
            "max_stall_s": 60,
        }

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        ops = ev.volume_ops()
        if not ops:
            raise SkipDetector("the run performed no volume operations; enable "
                               "chaos.volume-ops to judge them")
        volumes = {v.claim: v for v in ev.pnfs_volumes()}
        series: dict[tuple[str, str], list[BlockSample]] = {}
        for s in ev.block_samples():
            series.setdefault((s.node, s.uuid), []).append(s)

        for op in ops:
            subject = f"{op.op}/{op.claim}" if op.claim else op.op
            if op.skipped:
                yield info(self.name, title=f"{op.op} not attempted", subject=subject,
                           detail=op.skipped)
                continue
            problems = list(self._problems(op))
            if op.error:
                problems.insert(0, (f"the {op.op} failed", op.error))
            for title, detail in problems:
                yield critical(self.name, title=title, subject=subject, detail=detail,
                               evidence=_evidence(op))
            if op.op == "snapshot" and op.snapshot and op.snapshot_deleted is None and not op.error:
                yield warning(self.name, title="the snapshot was not deleted afterward",
                              subject=subject, detail=f"VolumeSnapshot {op.snapshot}",
                              evidence=_evidence(op),
                              note="the operation left its snapshot behind on the cluster")
            if problems:
                continue
            yield from self._stalled(op, subject, volumes.get(op.claim), series)
            yield info(self.name, title=f"{op.op} completed", subject=subject,
                       detail=_timeline(op), evidence=_evidence(op))

    @staticmethod
    def _problems(op: VolumeOp) -> Iterable[tuple[str, str]]:
        limit = f"within {op.timeout_s:.0f}s" if op.timeout_s else "before the run ended"
        if op.op == "expand" and not op.error:
            if op.capacity_at is None:
                yield ("the claim never reached its new size",
                       f"{op.claim} asked for {op.target_bytes} bytes at {_t(op.requested)}, "
                       f"and its status did not show it {limit}")
            elif op.client_pod and op.client_seen_at is None:
                yield ("a client never saw the expansion",
                       f"{op.client_pod} still reported {op.client_before_b} bytes {limit} of "
                       f"the request, though the claim showed the new size at "
                       f"{_t(op.capacity_at)}: the export was not grown on the metadata server")
        if op.op == "snapshot" and not op.error:
            if op.ready_at is None:
                yield ("the snapshot never became ready",
                       f"{op.snapshot} of {op.claim}, requested {_t(op.requested)}, was not "
                       f"ready to use {limit}")
            elif op.restore_claim and op.restored_at is None:
                yield ("the restored snapshot was never read back",
                       f"{op.restore_claim}, restored from {op.snapshot}, had no pod reading "
                       "it before the restore timed out")
            elif op.restore_claim and op.restore_md5 != op.marker_md5:
                yield ("the restored snapshot holds other data than was written",
                       f"the marker read back from {op.restore_claim} has checksum "
                       f"{op.restore_md5 or 'none'}, and {op.marker_md5} was written and "
                       "synced before the snapshot")

    def _stalled(self, op: VolumeOp, subject: str, vol: PnfsVolume | None,
                 series: dict[tuple[str, str], list[BlockSample]]) -> Iterable[Finding]:
        end = _done(op)
        if vol is None or not vol.lvol or end is None:
            return
        limit = float(self.opt("max_stall_s"))
        for node in vol.nodes:
            got = sorted(series.get((node, vol.lvol), []), key=lambda b: b.ts)
            if len(got) < 2:
                continue
            for start, seconds in _stalls(got):
                stop = start + timedelta(seconds=seconds)
                if seconds > limit and start <= end and stop >= op.requested:
                    yield warning(
                        self.name, title=f"writes stalled {seconds:.0f}s across the {op.op}",
                        subject=subject,
                        detail=f"{node}: no sector written to {vol.lvol} from {_t(start)} to "
                               f"{_t(stop)}, while the {op.op} ran from {_t(op.requested)} "
                               f"to {_t(end)}",
                        evidence={**_evidence(op), "node": node, "stall_s": round(seconds)},
                        note="the operation completed, but the client's direct path stood "
                             "still while it ran; see pnfs.device-io for the same stall")
                    break


def _done(op: VolumeOp) -> datetime | None:
    """When the operation's last step completed."""
    if op.op == "expand":
        return op.client_seen_at or op.capacity_at
    return op.restored_at or op.ready_at


def _timeline(op: VolumeOp) -> str:
    if op.op == "expand":
        return (f"requested {_t(op.requested)}, claim at {op.target_bytes} bytes "
                f"{_t(op.capacity_at)}, {op.client_pod or 'no client'} saw "
                f"{op.client_after_b} bytes {_t(op.client_seen_at) or 'never'}")
    return (f"marker written and snapshot requested {_t(op.requested)}, ready "
            f"{_t(op.ready_at)}, restored and read back {_t(op.restored_at) or 'not restored'}, "
            f"snapshot deleted {_t(op.snapshot_deleted) or 'never'}")


def _evidence(op: VolumeOp) -> dict[str, Any]:
    out: dict[str, Any] = {"op": op.op, "claim": op.claim, "requested": _t(op.requested)}
    if op.op == "expand":
        out.update(target_bytes=op.target_bytes, capacity_at=_t(op.capacity_at),
                   client_pod=op.client_pod, client_seen_at=_t(op.client_seen_at))
    else:
        out.update(snapshot=op.snapshot, ready_at=_t(op.ready_at),
                   restore_claim=op.restore_claim, marker_md5=op.marker_md5,
                   restore_md5=op.restore_md5)
    return out


def _t(t: datetime | None) -> str:
    return f"{t:%H:%M:%S}" if t else ""
