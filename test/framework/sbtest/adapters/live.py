"""LiveEvidence — evidence assembled from a run that just happened.

A thin thing on purpose. Components write their artifacts into the run directory in the
same layout an archive uses, and record what they observed on the timeline; so a live run's
evidence is the archive reader pointed at the directory being written, overlaid with the
timeline for the parts that are not files yet.

Keeping the two paths this close is deliberate: if live evidence and archived evidence
diverge, a check that passes during a run can fail on replay, and then nobody trusts either.
"""

from __future__ import annotations

from ..core import AnaSample, Migration, NvmeController, RunContext
from .archive import ArchiveEvidence


class LiveEvidence(ArchiveEvidence):
    def __init__(self, ctx: RunContext) -> None:
        super().__init__(ctx.outdir)
        self.ctx = ctx
        self.run_id = ctx.run_id
        self._live_ana: dict[str, list[AnaSample]] = ctx.shared.get("ana.samples", {})
        self._live_migs: list[Migration] = ctx.shared.get("migrations", [])
        self._live_ctrls: list[NvmeController] = ctx.shared.get("nvme.controllers", [])

    def migrations(self) -> list[Migration]:
        return self._live_migs or super().migrations()

    def ana_samples(self, migration: str) -> list[AnaSample]:
        return self._live_ana.get(migration) or super().ana_samples(migration)

    def nvme_controllers(self) -> list[NvmeController]:
        return self._live_ctrls or super().nvme_controllers()

    def cluster_uuid(self) -> str:
        return str(self.ctx.shared.get("cluster.uuid") or super().cluster_uuid())
