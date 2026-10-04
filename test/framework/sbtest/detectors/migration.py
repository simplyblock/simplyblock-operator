"""Migration-outcome detectors — the run's success rate, and how it failed.

Deliberately rate-based rather than per-migration: one failed migration in a long run is
noise, and a run where half of them time out is a different defect from a run where one did.
"""

from __future__ import annotations

from collections.abc import Iterable

from ..core import Detector, Evidence, Finding, SkipDetector, critical, detector, info, warning


@detector
class Outcomes(Detector):
    """Too few migrations completed, or too many ended one particular way.

    The phase breakdown is the finding, not just the count. A run of 46 migrations with 13
    Completed and 25 TIMEOUT says something quite specific — none of the timeouts ever
    reached a cutover — that "28% success" alone does not.
    """

    name = "migration.outcomes"
    summary = "completion rate and phase breakdown across the run"

    def defaults(self) -> dict:
        return {"min_completed_fraction": 0.8,
                "max_timeout_fraction": 0.1,
                "completed_phases": ["Completed"],
                "timeout_phases": ["TIMEOUT", "Timeout"]}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        total = len(migs)
        done = set(self.opt("completed_phases"))
        tmo = set(self.opt("timeout_phases"))

        phases: dict[str, int] = {}
        for m in migs:
            phases[m.phase or "?"] = phases.get(m.phase or "?", 0) + 1
        breakdown = ", ".join(f"{k}={v}" for k, v in sorted(phases.items(), key=lambda x: -x[1]))

        completed = sum(v for k, v in phases.items() if k in done)
        timeouts = sum(v for k, v in phases.items() if k in tmo)
        frac = completed / total
        yield info(self.name, title=f"{completed}/{total} migrations completed",
                   subject="run", detail=breakdown,
                   evidence={"total": total, "completed": completed, "phases": phases})

        if frac < float(self.opt("min_completed_fraction")):
            yield critical(
                self.name,
                title=f"only {frac:.0%} of migrations completed",
                subject="run",
                detail=breakdown,
                evidence={"completed_fraction": round(frac, 3),
                          "min_completed_fraction": float(self.opt("min_completed_fraction")),
                          "phases": phases},
            )
        if timeouts / total > float(self.opt("max_timeout_fraction")):
            yield critical(
                self.name,
                title=f"{timeouts}/{total} migrations timed out",
                subject="run",
                detail=breakdown,
                evidence={"timeouts": timeouts, "total": total,
                          "max_timeout_fraction": float(self.opt("max_timeout_fraction"))},
                note="A timeout that never reached a cutover leaves the volume on the "
                     "source with the target's objects still allocated; check whether the "
                     "cutover was attempted at all before blaming the copy.",
            )


@detector
class Errors(Detector):
    """Distinct migration error messages, grouped.

    Grouping matters more than counting here: 16 identical "NVMe path validation failed"
    errors are one defect, and the value is in seeing that they are identical.
    """

    name = "migration.errors"
    summary = "distinct migration error messages, grouped by shape"

    def defaults(self) -> dict:
        return {"max_distinct": 0}

    @staticmethod
    def _shape(err: str) -> str:
        import re
        s = re.sub(r"[0-9a-f]{8}-[0-9a-f-]{27}", "<uuid>", err)
        s = re.sub(r"\d+", "N", s)
        return s[:200]

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        groups: dict[str, list[str]] = {}
        for m in migs:
            if m.error:
                groups.setdefault(self._shape(m.error), []).append(m.name)
        if not groups:
            return
        for shape, names in sorted(groups.items(), key=lambda x: -len(x[1])):
            yield warning(
                self.name,
                title=f"{len(names)} migration(s) failed with the same error",
                subject=shape[:60],
                detail=f"{shape}\nmigrations: {', '.join(sorted(names)[:12])}"
                       + (" ..." if len(names) > 12 else ""),
                evidence={"count": len(names), "migrations": sorted(names), "shape": shape},
            )
