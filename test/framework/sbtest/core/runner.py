"""The runner — drives component lifecycles, then runs detectors over the evidence.

Two entry points, and the important thing is that they share the second half:

    Runner.execute()   full run: components through their lifecycle, then judge.
    Runner.judge(ev)   judge alone, over evidence from anywhere — including an archive.

Sharing the judging half is what makes a check fixable against the run that motivated it.
"""

from __future__ import annotations

import time
import traceback
from typing import Any

from .config import Config
from .context import RunContext, now_utc
from .evidence import Evidence
from .findings import Report, Severity, warning
from .plugin import Component, Detector, SkipDetector, build_component, build_detector


class Runner:
    def __init__(self, cfg: Config, ctx: RunContext) -> None:
        self.cfg = cfg
        self.ctx = ctx
        self.components: list[Component] = []
        self.detectors: list[Detector] = []
        self.report = Report(run_id=ctx.run_id)
        #: Components whose setup was entered, so teardown is owed to them.
        self._entered: list[Component] = []

    # ── wiring ─────────────────────────────────────────────────────────────────────

    def build(self) -> Runner:
        for name, opts in self.cfg.components.enabled.items():
            try:
                self.components.append(build_component(name, **opts))
            except Exception as e:  # noqa: BLE001
                raise RuntimeError(f"cannot build component {name!r}: {e}") from e
        for name, opts in self.cfg.detectors.enabled.items():
            try:
                self.detectors.append(build_detector(name, **opts))
            except Exception as e:  # noqa: BLE001
                raise RuntimeError(f"cannot build detector {name!r}: {e}") from e

        if self.cfg.components.disabled:
            self.ctx.log.info("components disabled: " + ", ".join(sorted(self.cfg.components.disabled)))
        if self.cfg.detectors.disabled:
            self.ctx.log.info("detectors disabled: " + ", ".join(sorted(self.cfg.detectors.disabled)))
        self.ctx.log.info(f"components enabled ({len(self.components)}): "
                          + (", ".join(c.name for c in self.components) or "-"))
        self.ctx.log.info(f"detectors enabled ({len(self.detectors)}): "
                          + (", ".join(d.name for d in self.detectors) or "-"))
        return self

    # ── lifecycle ──────────────────────────────────────────────────────────────────

    def _phase(self, hook: str, comps: list[Component], fatal: bool = False) -> None:
        """Call one hook on each component, recording rather than raising on failure.

        A collector that fails must not take the run with it — the run's purpose is the
        workload and the judgement, and partial evidence still judges; the failure becomes a
        WARNING finding so the gap is on the record rather than invisible.

        `fatal` is passed for the setup of a component that declares itself `required`,
        which is how a workload or a migration driver says that continuing without it would
        produce a green result for a test that never ran.
        """
        for c in comps:
            try:
                getattr(c, hook)(self.ctx)
            except Exception as e:  # noqa: BLE001
                self.ctx.log.error(f"component {c.name}.{hook} failed: {e}")
                self.ctx.log.debug(traceback.format_exc())
                self.report.add(warning(
                    detector=f"component/{c.name}",
                    title=f"{hook} failed: {e}",
                    subject=c.name,
                    note="Evidence this component would have produced may be missing; any "
                         "detector that depends on it will report itself skipped.",
                ))
                if fatal:
                    raise

    def setup(self) -> None:
        # Before any component runs: the window has to include setup, because a component
        # that breaks something breaks it during setup as much as during the run.
        self.ctx.mark_window()
        for c in self.components:
            # Appended before the hook runs, so a component that failed half-way through
            # allocating still gets the teardown it is owed.
            self._entered.append(c)
            self._phase("setup", [c], fatal=c.required)

    def start(self) -> None:
        self._phase("start", self.components)

    def tick(self) -> None:
        self._phase("tick", self.components)

    def run_for(self, seconds: float, tick_every: float = 5.0) -> None:
        """Tick components for `seconds`. The scenario's own work happens in a component."""
        deadline = time.time() + seconds
        while time.time() < deadline and not self.ctx.stopping.is_set():
            self.tick()
            time.sleep(min(tick_every, max(0.0, deadline - time.time())))

    def stop(self) -> None:
        self.ctx.stopping.set()
        self._phase("stop", list(reversed(self.components)))

    def collect(self) -> None:
        self._phase("collect", self.components)
        # Closed after collection, so the window covers everything the artifacts contain.
        self.ctx.mark_window(end=now_utc())

    def teardown(self) -> None:
        self._phase("teardown", list(reversed(self._entered)))

    # ── judging ────────────────────────────────────────────────────────────────────

    def judge(self, ev: Evidence) -> Report:
        """Run every enabled detector over `ev` and fold the results into the report."""
        for d in self.detectors:
            try:
                found = list(d.detect(ev))
            except SkipDetector as e:
                self.report.skip(d.name, str(e) or "evidence not available")
                continue
            except Exception as e:  # noqa: BLE001
                # A detector that throws is a bug in the detector, not a clean run. Say so
                # loudly rather than letting it look like "nothing found".
                self.ctx.log.error(f"detector {d.name} raised: {e}")
                self.ctx.log.debug(traceback.format_exc())
                self.report.skip(d.name, f"raised {type(e).__name__}: {e}")
                self.report.add(warning(
                    detector=d.name, title=f"detector raised {type(e).__name__}: {e}",
                    note="This is a detector bug: the run was not judged on this dimension."))
                continue
            self.report.add(*found)
        return self.report

    def execute(self, evidence_for: Any, duration_s: float = 0.0) -> Report:
        """Full run: lifecycle, then judge. `evidence_for` builds Evidence from the ctx."""
        try:
            self.setup()
            self.start()
            if duration_s:
                self.run_for(duration_s)
            self.stop()
            self.collect()
        finally:
            self.teardown()
        return self.judge(evidence_for(self.ctx))

    # ── output ─────────────────────────────────────────────────────────────────────

    def emit(self, json_name: str = "findings.json") -> Report:
        r = self.report
        log = self.ctx.log
        line = "=" * 78
        log.info(line)
        log.info("FINDINGS")
        log.info(line)

        if not r.findings:
            log.info("no findings")
        for sev in (Severity.CRITICAL, Severity.WARNING, Severity.INFO):
            group = [f for f in r.of_severity(sev) if f.counts_against_the_run]
            if not group:
                continue
            log.info(f"{sev} ({len(group)}):")
            for f in group:
                emit = log.crit if sev is Severity.CRITICAL else (
                    log.warn if sev is Severity.WARNING else log.info)
                emit(f"    {f.subject or '-'}: {f.title}")
                if f.detail:
                    for chunk in f.detail.splitlines():
                        emit(f"        {chunk}")
                if f.note:
                    emit(f"        note: {f.note}")
                for a in f.artifacts:
                    emit(f"        evidence: {a}")

        inherited = [f for f in r.findings if not f.counts_against_the_run]
        if inherited:
            log.info(line)
            log.info(f"PRE-EXISTING ({len(inherited)}) — already true before this run began, "
                     "so not counted against it:")
            for f in inherited:
                emit = log.crit if f.severity is Severity.CRITICAL else log.warn
                emit(f"    [{f.severity}] {f.subject or '-'}: {f.title}")
                if f.detail:
                    for chunk in f.detail.splitlines():
                        emit(f"        {chunk}")
                if f.note:
                    emit(f"        note: {f.note}")

        if r.skipped:
            log.info(f"skipped ({len(r.skipped)}) — not judged on these dimensions:")
            for name, why in sorted(r.skipped.items()):
                log.warn(f"    {name}: {why}")

        path = self.ctx.path(json_name)
        with open(path, "w") as fh:
            fh.write(r.to_json())
        log.info(line)
        summary = (f"{len(r.of_severity(Severity.CRITICAL, counting_only=True))} critical, "
                   f"{len(r.of_severity(Severity.WARNING, counting_only=True))} warning")
        if inherited:
            summary += f", {len(inherited)} pre-existing"
        log.info(f"RESULT: {r.verdict}   ({summary}) -> {path}")
        if r.verdict == "INCONCLUSIVE":
            log.crit("The environment was already damaged before this run started, so neither "
                     "a pass nor a failure can be believed. Clean it up and run again.")
        log.info(line)
        return r


def findings_by_subject_table(report: Report) -> list[str]:
    """Findings grouped by subject — the join that makes correlated defects visible.

    A subject carrying two independent findings is the strongest signal these runs produce:
    "re-froze the volume" plus "lost writes" on the same migration is what turned a
    correlation into a root cause.
    """
    lines = []
    for subject, group in sorted(report.by_subject().items()):
        if len(group) < 2:
            continue
        lines.append(f"{subject}: " + "; ".join(f"{f.detector}({f.severity})" for f in group))
    return lines
