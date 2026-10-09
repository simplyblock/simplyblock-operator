"""Whether pods could join and leave pNFS volumes all run long, and leave nothing behind.

`workload.pnfs-churn` keeps short-lived pods arriving during the run: some mount a volume the
run's long-lived pods share, some bring a pNFS volume of their own and delete it when they
leave. Their fio output is judged by the fio detectors like every other instance's. What is
judged here is the coming and going itself:

* **A pod that never reached I/O.** It never got its fio into the timed run, so it never
  used its volume, and whatever stopped it (staging, the export, the layout) is a defect.
* **An own volume left behind.** A PersistentVolume or NFSExport that outlived its claim
  leaves an export and its backing namespace behind on every deletion, which a cluster under
  constant churn accumulates.
* **A slow cleanup.** One that took longer than `delete_budget_s` finished, but slowly
  enough that churn outpaces it, so it is a warning.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from ..core import (
    ChurnPod,
    Detector,
    Evidence,
    Finding,
    SkipDetector,
    critical,
    detector,
    info,
    warning,
)


@detector
class Churn(Detector):
    name = "pnfs.churn"
    summary = "a pod that joined a pNFS volume and never did I/O, or an own volume left behind"

    def defaults(self) -> dict[str, Any]:
        # How long an own volume's PersistentVolume and NFSExport may take to go once its
        # claim is deleted: the export's teardown in the guest, then the volume's deletion.
        return {"delete_budget_s": 120}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        pods = ev.churn()
        if not pods:
            raise SkipDetector("no pod joined or left a pNFS volume; enable workload.pnfs-churn")
        for p in pods:
            if p.io_started is None:
                yield critical(
                    self.name, title="a churn pod never reached I/O on its pNFS volume",
                    subject=p.pod,
                    detail=f"{p.pod} on {p.node or 'no node'}, claim {p.claim} "
                           f"({'own' if p.own_volume else 'shared'}): "
                           f"{p.error or 'fio never entered its timed run'}",
                    evidence={"pod": p.pod, "claim": p.claim, "own_volume": p.own_volume},
                    note="the pod could not use the volume it joined, so staging, the export, "
                         "or the mount failed for it")
            if p.own_volume:
                yield from self._cleanup(p)
        yield self._summary(pods)

    def _cleanup(self, p: ChurnPod) -> Iterable[Finding]:
        for gone, what in ((p.pv_gone, "PersistentVolume"), (p.export_gone, "NFSExport")):
            if gone is False:
                yield critical(
                    self.name, title=f"an own volume's {what} outlived its claim",
                    subject=p.claim,
                    detail=f"claim {p.claim} (PV {p.pv or 'unknown'}) deleted "
                           f"{p.pvc_deleted:%H:%M:%S}, and its {what} was still there at the "
                           "end of the run" if p.pvc_deleted else
                           f"claim {p.claim}: its {what} was still there at the end of the run",
                    evidence={"claim": p.claim, "pv": p.pv, "what": what},
                    note="every deletion leaves this behind, which constant churn accumulates")
        budget = float(self.opt("delete_budget_s"))
        if p.gone_s is not None and p.gone_s > budget:
            yield warning(
                self.name, title=f"an own volume took {p.gone_s:.0f}s to go after its claim",
                subject=p.claim,
                detail=f"claim {p.claim}: PersistentVolume and NFSExport gone {p.gone_s:.0f}s "
                       f"after the claim's deletion, over the {budget:.0f}s budget",
                evidence={"claim": p.claim, "gone_s": p.gone_s})

    @staticmethod
    def _summary(pods: list[ChurnPod]) -> Finding:
        own = [p for p in pods if p.own_volume]
        left = sum(1 for p in pods if p.deleted is not None)
        removed = sum(1 for p in own if p.pv_gone and p.export_gone)
        return info(
            "pnfs.churn", title=f"{len(pods)} pod(s) joined pNFS volumes, {left} left",
            subject="churn",
            detail=f"{len(own)} brought a volume of their own, and {removed} of those "
                   f"volumes are gone. {len(pods) - len(own)} joined a shared volume.",
            evidence={"joined": len(pods), "left": left, "own_volumes": len(own),
                      "own_volumes_gone": removed})
