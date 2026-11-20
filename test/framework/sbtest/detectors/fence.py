"""Whether a pNFS client that nfsd fenced stopped writing.

`chaos.fence` cuts one node off from the metadata server while its NVMe-oF paths stay up,
and has a client on another node change the size of a file the cut-off node writes with a
layout. nfsd cannot recall that layout, so after two lease periods it fences the node by
preempting its reservation key, and only then lets the truncate finish. From then on the
reservation is the only thing between the cut-off node and the shared namespace.

Critical: a write by the cut-off node that succeeded after the truncate returned and
before the partition healed, since that is two writers on one filesystem. Also critical: a
truncate that never returned within its bound, because the metadata server then holds every
client of that file. A warning when none of the cut-off node's writes failed during the
partition: its writes did not depend on the layout, or the partition did not take, so the
run proved nothing about fencing. The timeline is reported as information.
"""

from __future__ import annotations

from collections.abc import Iterable
from datetime import datetime

from ..core import (
    Detector,
    Evidence,
    Fence,
    Finding,
    SkipDetector,
    critical,
    detector,
    info,
    warning,
)


@detector
class FenceDetector(Detector):
    name = "pnfs.fence"
    summary = "a fenced pNFS client whose writes still landed"

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        f = ev.fence()
        if f is None:
            raise SkipDetector("the run fenced no client; enable chaos.fence to judge fencing")
        subject = f"{f.victim_node}/{f.claim}" if f.victim_node else f.claim
        if f.error:
            yield warning(self.name, title="the fence scenario did not run to the end",
                          subject=subject, detail=f.error,
                          note="findings below cover only the steps that ran")
        if f.partitioned is None:
            return
        healed = f.healed
        during = [w for w in f.writes
                  if w.ts >= f.partitioned and (healed is None or w.ts < healed)]
        failed = [w for w in during if not w.ok]
        if f.truncate_issued is not None and f.truncate_returned is None:
            yield critical(
                self.name, title="the truncate never returned, so the client was never fenced",
                subject=subject,
                detail=f"truncate from {f.recaller_node} issued {_t(f.truncate_issued)} had not "
                       f"returned after {f.truncate_timeout_s:.0f}s"
                       + (f" ({f.truncate_error})" if f.truncate_error else ""),
                evidence={"truncate_timeout_s": f.truncate_timeout_s,
                          "recaller_node": f.recaller_node},
                note="nfsd fences after two lease periods; a recall that outlasts that holds "
                     "every client of the file")
        if f.truncate_returned is not None and f.truncate_error:
            # It returned, but failed: nothing was recalled, so nothing was fenced.
            yield warning(
                self.name, title="the truncate failed, so fencing went untested",
                subject=subject,
                detail=f"truncate from {f.recaller_node} returned {_t(f.truncate_returned)}: "
                       f"{f.truncate_error}",
                evidence={"truncate_error": f.truncate_error,
                          "recaller_node": f.recaller_node},
                note="a recall needs a size change that succeeds; writes after it prove "
                     "nothing about the reservation")
        elif f.truncate_returned is not None:
            after = [w for w in during if w.ok and w.ts > f.truncate_returned]
            if after:
                yield critical(
                    self.name, title=f"{len(after)} write(s) landed after the client was fenced",
                    subject=subject,
                    detail=f"{f.victim_node} wrote {f.file} at {_t(after[0].ts)} and later, "
                           f"after the truncate returned at {_t(f.truncate_returned)} and "
                           f"before the partition healed"
                           + (f" at {_t(healed)}" if healed else ""),
                    evidence={"writes_after_fence": len(after),
                              "first": _t(after[0].ts), "last": _t(after[-1].ts),
                              "truncate_returned": _t(f.truncate_returned)},
                    note="the reservation did not stop the fenced node: two writers on one "
                         "filesystem")
        if not failed:
            yield warning(
                self.name, title="no write failed during the partition, so fencing went untested",
                subject=subject,
                detail=f"{len(during)} write(s) by {f.victim_node} between {_t(f.partitioned)} "
                       f"and {_t(healed) if healed else 'the end'}, none failed",
                evidence={"writes_during": len(during)},
                note="the writes did not depend on the layout, or the partition did not take")
        yield info(
            self.name, title="fence timeline", subject=subject,
            detail=_timeline(f, len(during), len(failed)),
            evidence={"partitioned": _t(f.partitioned), "healed": _t(healed),
                      "truncate_issued": _t(f.truncate_issued),
                      "truncate_returned": _t(f.truncate_returned),
                      "writes_during": len(during), "failed_during": len(failed),
                      "first_failure": _t(failed[0].ts) if failed else "",
                      "rules": list(f.rules)})


def _timeline(f: Fence, during: int, failed: int) -> str:
    took = ""
    if f.truncate_issued and f.truncate_returned:
        took = f" after {(f.truncate_returned - f.truncate_issued).total_seconds():.0f}s"
    return (f"{f.victim_node} cut off {_t(f.partitioned)}, truncate from {f.recaller_node} "
            f"issued {_t(f.truncate_issued)}, returned {_t(f.truncate_returned) or 'never'}"
            f"{took}, healed {_t(f.healed) or 'never'}; {failed} of {during} write(s) during "
            "the partition failed")


def _t(t: datetime | None) -> str:
    return f"{t:%H:%M:%S.%f}"[:-3] if t else ""
