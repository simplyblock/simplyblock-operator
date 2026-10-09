"""Whether every pod the run restarted on purpose came back.

`chaos.restart` deletes the metadata server, a node plugin, or the controller mid-run. A
replacement that never turns Ready leaves the cluster broken by the test itself, and every
finding after it describes that rather than the code, so it is critical. The restarts that
did recover are reported too, as information: a run's findings read differently when the
reader knows the MDS was restarted at 19:40.
"""

from __future__ import annotations

from collections.abc import Iterable

from ..core import Detector, Evidence, Finding, SkipDetector, critical, detector, info


@detector
class Recovery(Detector):
    name = "chaos.recovery"
    summary = "a pod the run restarted that never came back"

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        restarts = ev.restarts()
        if not restarts:
            raise SkipDetector("the run restarted nothing; enable chaos.restart to judge recovery")
        for r in restarts:
            subject = f"{r.target}/{r.pod}"
            if r.ready is None:
                yield critical(
                    self.name, title=f"{r.target} never came back after its restart",
                    subject=subject,
                    detail=f"{r.pod} on {r.node} deleted at {r.deleted:%H:%M:%S}, and no Ready "
                           "replacement followed",
                    evidence={"target": r.target, "pod": r.pod, "node": r.node},
                    artifacts=[r.log] if r.log else [],
                    note="the cluster was left without this pod by the test, so findings after "
                         f"{r.deleted:%H:%M:%S} describe that rather than the code")
                continue
            took = (r.ready - r.deleted).total_seconds()
            yield info(
                self.name, title=f"{r.target} restarted, back after {took:.0f}s",
                subject=subject,
                detail=f"{r.pod} on {r.node}: deleted {r.deleted:%H:%M:%S}, "
                       f"{r.replacement or r.pod} Ready {r.ready:%H:%M:%S}",
                evidence={"target": r.target, "pod": r.pod, "node": r.node,
                          "recovered_s": took})
