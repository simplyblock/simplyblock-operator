"""Whether the pNFS namespace held up under metadata traffic: consistent, available, and fast.

workload.pnfs-metadata keeps namespace operations going against the metadata server and
checks each worker's directory from another node. `pnfs.metadata` judges three things:

* **Consistency.** Another client listing a path the manifest does not have, missing one it
  has, or reading different content is critical, restart or not: the namespace has diverged
  between clients of one server.
* **Errors.** An operation that failed outside every restart window is critical. One inside
  a restart's window is a warning naming the restart, because the application still saw it,
  and how often a restart costs one is worth tracking.
* **Stalls.** No operation completing for longer than `max_stall_s` is a warning, naming the
  restart that overlaps it when there is one.

A summary of the rate and the latency by operation is reported as information.
"""

from __future__ import annotations

from collections import Counter
from collections.abc import Iterable
from datetime import datetime, timedelta

from ..core import (
    Detector,
    Evidence,
    Finding,
    MetadataOp,
    Restart,
    SkipDetector,
    critical,
    detector,
    info,
    warning,
)


@detector
class PnfsMetadata(Detector):
    name = "pnfs.metadata"
    summary = "pNFS namespace operations that failed, stalled, or diverged between clients"

    def defaults(self) -> dict:
        return {
            # How long after a restart an error or stall is still placed in it. A pNFS
            # client was measured taking up to 97 s back after a metadata server restart.
            "restart_window_s": 180.0,
            "max_stall_s": 60.0,
        }

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        ops, checks = ev.metadata_ops(), ev.metadata_checks()
        if not ops and not checks:
            raise SkipDetector("no metadata workload ran; enable workload.pnfs-metadata")
        restarts = ev.restarts()
        window = timedelta(seconds=float(self.opt("restart_window_s")))

        for c in checks:
            if c.error:
                yield warning(self.name, title=f"{c.worker} could not be checked",
                              subject=c.worker, detail=c.error,
                              note="its namespace was not compared at this point")
            elif not c.clean:
                yield critical(
                    self.name,
                    title=f"{c.verifier_node} sees {c.worker}'s directory differently",
                    subject=c.worker,
                    detail=(f"at {c.ts:%H:%M:%S}: {len(c.missing)} missing, {len(c.extra)} "
                            f"extra, {len(c.mismatched)} with different content"
                            + ("" if c.cross_node else " (checked from the same node)")),
                    evidence={"missing": list(c.missing[:20]), "extra": list(c.extra[:20]),
                              "mismatched": list(c.mismatched[:20]),
                              "worker_node": c.worker_node, "verifier_node": c.verifier_node},
                    note="the worker held still with a fresh manifest while the other client "
                         "listed; a difference is the namespace diverging between clients")

        by_worker: dict[str, list[MetadataOp]] = {}
        for o in ops:
            by_worker.setdefault(o.worker, []).append(o)
        for worker, wops in sorted(by_worker.items()):
            yield from self._errors(worker, [o for o in wops if not o.ok], restarts, window)
            yield from self._stalls(worker, wops, restarts, window)
        if ops:
            yield self._summary(ops)

    def _errors(self, worker: str, failed: list[MetadataOp], restarts: list[Restart],
                window: timedelta) -> Iterable[Finding]:
        outside = [o for o in failed if _restart_at(o.ts, restarts, window) is None]
        if outside:
            yield critical(
                self.name, title=f"{len(outside)} namespace operation(s) failed outside any "
                                 "restart", subject=worker,
                detail=_first(outside),
                evidence={"errors": dict(Counter(o.error for o in outside)),
                          "ops": dict(Counter(o.op for o in outside))},
                note="nothing the run did explains these")
        inside: dict[str, list[MetadataOp]] = {}
        for o in failed:
            r = _restart_at(o.ts, restarts, window)
            if r is not None:
                inside.setdefault(f"{r.target}/{r.pod}", []).append(o)
        for restart, rops in inside.items():
            yield warning(
                self.name, title=f"{len(rops)} namespace operation(s) failed during the "
                                 f"{restart} restart", subject=worker,
                detail=_first(rops),
                evidence={"restart": restart, "errors": dict(Counter(o.error for o in rops))},
                note="the application saw these errors; a restart that costs none is the goal")

    def _stalls(self, worker: str, wops: list[MetadataOp], restarts: list[Restart],
                window: timedelta) -> Iterable[Finding]:
        limit = float(self.opt("max_stall_s"))
        ends = sorted(o.ended for o in wops)
        for prev, nxt in zip(ends, ends[1:], strict=False):
            gap = (nxt - prev).total_seconds()
            if gap <= limit:
                continue
            r = next((r for r in restarts
                      if r.deleted <= nxt and prev <= r.deleted + window), None)
            evidence: dict = {"stall_s": round(gap, 1), "from": f"{prev:%H:%M:%S}",
                              "to": f"{nxt:%H:%M:%S}"}
            if r is not None:
                evidence["restart"] = f"{r.target}/{r.pod}"
            yield warning(
                self.name, title=f"no namespace operation completed for {gap:.0f}s",
                subject=worker,
                detail=f"{prev:%H:%M:%S} to {nxt:%H:%M:%S}"
                       + (f", during the {r.target} restart of {r.pod} at {r.deleted:%H:%M:%S}"
                          if r else ", with no restart to explain it"),
                evidence=evidence,
                note="every client of the metadata server waited this long on the namespace")

    def _summary(self, ops: list[MetadataOp]) -> Finding:
        span = (max(o.ended for o in ops) - min(o.ts for o in ops)).total_seconds() or 1.0
        latency: dict[str, dict[str, float]] = {}
        for op in sorted({o.op for o in ops}):
            ms = sorted(o.ms for o in ops if o.op == op and o.ok)
            if ms:
                latency[op] = {"p50": _pct(ms, 0.50), "p99": _pct(ms, 0.99), "n": len(ms)}
        failed = sum(1 for o in ops if not o.ok)
        return info(
            self.name, title=f"{len(ops)} namespace operation(s), {len(ops) / span:.1f}/s",
            subject="metadata",
            detail=", ".join(f"{op} p50 {v['p50']:.1f} ms p99 {v['p99']:.1f} ms"
                             for op, v in latency.items()) + f"; {failed} failed",
            evidence={"ops": len(ops), "failed": failed, "per_s": round(len(ops) / span, 2),
                      "latency_ms": latency})


def _restart_at(t: datetime, restarts: list[Restart], window: timedelta) -> Restart | None:
    """The latest restart whose window holds t."""
    during = [r for r in restarts if r.deleted <= t <= r.deleted + window]
    return max(during, key=lambda r: r.deleted) if during else None


def _first(ops: list[MetadataOp]) -> str:
    o = min(ops, key=lambda o: o.ts)
    return f"first: {o.op} {o.path} at {o.ts:%H:%M:%S} failed with {o.error or 'an error'}"


def _pct(sorted_ms: list[float], q: float) -> float:
    return round(sorted_ms[min(len(sorted_ms) - 1, int(q * len(sorted_ms)))], 3)
