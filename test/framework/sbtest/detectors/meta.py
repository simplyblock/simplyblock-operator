"""Meta-detectors: checks on the evidence itself.

These answer "can this run be judged at all?", which is a different question from "did it
pass" and one that nothing else asks. They exist because the alternative kept happening: a
log-based verdict was drawn over a log that covered a fraction of the run, and nothing in the
report said so.

The framework already distinguishes a detector that *skipped* from one that found nothing.
This module extends that to partial evidence, which is the harder and more common case — a log
that exists, opens fine, and is missing the two hours you needed.
"""

from __future__ import annotations

from collections.abc import Iterable

from ..core import Detector, Evidence, Finding, SkipDetector, detector, info, warning


@detector
class LogCoverage(Detector):
    """A collected log that does not span the run.

    Container logs are rotated by the kubelet against a fixed byte budget, so a busy
    component's log covers the last N minutes rather than the run. That is survivable if you
    know it and misleading if you do not: every `logs.pattern` count, every kernel finding and
    every manual grep is silently scoped to whatever survived.

    Reported as a WARNING rather than a failure — incomplete evidence is not a defect in the
    system under test — but reported *loudly*, because it bounds what the rest of the report
    is allowed to claim.
    """

    name = "evidence.log-coverage"
    summary = "a collected log that does not cover the whole run, bounding what can be claimed"

    def defaults(self) -> dict:
        return {
            # Below this, a gap is startup jitter rather than rotation.
            "min_gap_s": 60.0,
            # Logs that legitimately begin mid-run: a component created by the run itself has
            # nothing to say before it existed.
            "ignore": [],
        }

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        spans = ev.log_spans()
        if not spans:
            raise SkipDetector("no collected logs to measure")
        start, end = ev.run_window()
        if not start:
            raise SkipDetector("the run window is unknown, so coverage cannot be measured")

        min_gap = float(self.opt("min_gap_s"))
        ignore = set(self.opt("ignore") or [])
        short: list[tuple[str, float, float]] = []  # name, missing-at-start, covered fraction
        empty: list[str] = []
        total = (end - start).total_seconds() if end else 0.0

        for sp in spans:
            if sp.name in ignore:
                continue
            if not sp.first or not sp.last:
                empty.append(sp.name)
                continue
            missing = (sp.first - start).total_seconds()
            covered = max(0.0, (sp.last - max(sp.first, start)).total_seconds())
            if missing > min_gap:
                short.append((sp.name, missing, covered / total if total else 0.0))

        if short:
            short.sort(key=lambda x: -x[1])
            worst = short[0]
            yield warning(
                self.name,
                title=f"{len(short)} log(s) do not cover the start of the run",
                subject="evidence",
                detail="; ".join(f"{n}: missing first {m / 60:.0f} min"
                                 f"{f', covers {c:.0%}' if c else ''}" for n, m, c in short),
                evidence={"logs": {n: {"missing_start_s": round(m),
                                       "covered_fraction": round(c, 3)}
                                   for n, m, c in short},
                          "worst": worst[0]},
                note="Log-derived findings are scoped to what survived rotation, so an absence "
                     "in these files is not evidence of absence. Raise the kubelet's "
                     "containerLogMaxSize/Files, or follow the log live (logs.stream) for the "
                     "components that outrun it.",
            )
        if empty:
            yield warning(
                self.name,
                title=f"{len(empty)} collected log(s) carry no usable timestamp",
                subject="evidence",
                detail=", ".join(sorted(empty)),
                evidence={"logs": sorted(empty)},
                note="Either the collection produced nothing or the format is unrecognised; "
                     "either way nothing in these can be placed in time.",
            )


@detector
class MigrationBlindSpot(Detector):
    """A migration that happened while a log was not covering it.

    The sharper form of the coverage problem, and the one that actually costs time: on the run
    this was written for, the migration that silently corrupted data ran 09:28:32-09:31:00 and
    one node's SPDK log began at 09:31:06 — six seconds after it finished. There is no
    post-mortem to do for that node, and the only thing worse than knowing that is not knowing
    it.

    Deliberately per (migration, log): "the run is 80% covered" is useless when the missing
    20% is the interesting part.
    """

    name = "evidence.blind-spot"
    summary = "a migration that no log covers, so it cannot be post-mortemed"

    def defaults(self) -> dict:
        return {"logs": ["spdk-*", "operator*"], "max_blind": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        import fnmatch
        migs = ev.migrations()
        spans = [s for s in ev.log_spans()
                 if any(fnmatch.fnmatch(s.name, g) for g in self.opt("logs"))]
        if not migs:
            raise SkipDetector("no migrations in this run")
        if not spans:
            raise SkipDetector("none of the requested logs were collected")

        blind: dict[str, list[str]] = {}
        for m in migs:
            end = m.end or m.start
            for sp in spans:
                if not sp.first or not sp.last:
                    continue
                # No overlap at all between the migration and what the log holds.
                if sp.first > end or sp.last < m.start:
                    blind.setdefault(m.name, []).append(sp.name)

        if len(blind) <= int(self.opt("max_blind")):
            return
        worst = sorted(blind.items(), key=lambda kv: -len(kv[1]))
        yield warning(
            self.name,
            title=f"{len(blind)} migration(s) fall outside at least one log's coverage",
            subject="evidence",
            detail="; ".join(f"{name}: no {', '.join(sorted(logs))}"
                             for name, logs in worst[:8])
                   + (" ..." if len(worst) > 8 else ""),
            evidence={"blind": {k: sorted(v) for k, v in blind.items()}},
            note="These migrations cannot be investigated from those logs whatever they turn "
                 "out to have done. If one of them also carries a CRITICAL finding, collect "
                 "again with logs.stream before spending time on the analysis.",
        )


@detector
class EvidenceInventory(Detector):
    """What evidence this run actually produced. Always INFO; never a verdict.

    Worth a finding of its own because "which of the fourteen checks could even run" is the
    first thing anyone asks of a report they did not generate, and reconstructing it from the
    skip list is guesswork.
    """

    name = "evidence.inventory"
    summary = "what evidence the run produced (informational)"

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        have = {
            "migrations": len(migs),
            "ana_sampled_migrations": sum(1 for m in migs if ev.ana_samples(m.name)),
            "fio_pods": len(ev.pods()),
            "fio_jobs": len(ev.fio_jobs()),
            "container_logs": len(ev.container_logs()),
            "control_events": len(ev.control_events()),
            "nvme_controllers": len(ev.nvme_controllers()),
        }
        start, end = ev.run_window()
        yield info(
            self.name,
            title=", ".join(f"{k}={v}" for k, v in have.items() if v),
            subject="evidence",
            detail=(f"run window {start:%H:%M:%S}..{end:%H:%M:%S} UTC"
                    if start and end else "run window unknown"),
            evidence={**have, "cluster": ev.cluster_uuid(),
                      "window_known": bool(start)},
        )
