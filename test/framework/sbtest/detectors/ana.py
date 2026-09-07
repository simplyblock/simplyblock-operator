"""ANA / host-path detectors.

These are the checks that came out of the migration corruption work, and the first one is
the reason this framework has a detector abstraction at all: it is a one-number check that
predicted silent data loss exactly, and it was not expressible in the harness without
editing the harness.
"""

from __future__ import annotations

from collections.abc import Iterable

from ..core import (
    AnaSample,
    Detector,
    Evidence,
    Finding,
    SkipDetector,
    critical,
    detector,
    freeze_windows,
    info,
    warning,
)

#: How long the control plane deliberately holds every path inaccessible while it moves a
#: volume. Measured, not guessed: it is a `time.sleep(2)` in the batch migration barrier.
CUTOVER_PAUSE_DESIGN_S = 2.0


@detector
class FreezeCount(Detector):
    """More than one cutover freeze in a migration.

    On the run this came from (fiomig-1787171993, 46 migrations) this was **exact**: the
    four migrations that froze the volume more than once are precisely the four that
    silently lost writes, and none of the other 42 lost anything.

    The mechanism is why it works. A migration takes the pause once; a second window means
    the control plane released the source back into the read/write path and retried the
    transfer, and the retry replays a non-idempotent step against a source that has been
    serving writes in between. The freeze count is the retry count.

    Prefer this over the pause *duration*, which the same run showed to be both less
    sensitive and less specific: two 3s freezes look like one healthy pause on any
    longest-window measure, and one migration's single 5-6s pause lost nothing.
    """

    name = "ana.freeze-count"
    summary = ("a migration that froze the volume more than once — retried cutover, and an "
               "exact predictor of silent write loss so far")

    def defaults(self) -> dict:
        return {
            "max_freezes": 1,
            # A freeze shorter than this is treated as sampling noise rather than a window.
            # 0 keeps every non-zero window, which is what the measured result used.
            "min_window_s": 0.0,
        }

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        judged = 0
        for m in migs:
            samples = ev.ana_samples(m.name)
            if not samples:
                continue
            judged += 1
            windows = [w for w in freeze_windows(samples)
                       if w[1] >= float(self.opt("min_window_s"))]
            if len(windows) <= int(self.opt("max_freezes")):
                continue
            spans = ", ".join(f"{s.strftime('%H:%M:%S')}+{d:.0f}s" for s, d in windows)
            yield critical(
                self.name,
                title=f"volume froze {len(windows)} times during one migration",
                subject=m.name,
                detail=f"windows: {spans}",
                evidence={"freezes": len(windows), "max_freezes": int(self.opt("max_freezes")),
                          "windows_s": [round(d, 1) for _, d in windows],
                          "phase": m.phase, "members": len(m.members)},
                note="A migration takes the cutover pause once; the rest are retries of a "
                     "step that did not take. Every migration observed to freeze more than "
                     "once has also silently lost writes — treat as corruption until the "
                     "checksums say otherwise, including when the phase is Completed.",
            )
        if not judged:
            raise SkipDetector("no ANA samples for any migration")


@detector
class CutoverPause(Detector):
    """A cutover pause that ran longer than the design window allows.

    A bounded pause is expected — the control plane drives every path inaccessible for about
    CUTOVER_PAUSE_DESIGN_S while it moves the volume — so this bounds that window rather
    than forbidding it. The default allows the design window plus one sampling interval,
    which is the widest a well-behaved pause can be *measured* as.

    Keep it alongside ana.freeze-count rather than instead of it: this catches a single
    window that overran, which the count cannot see.
    """

    name = "ana.cutover-pause"
    summary = "an all-paths-inaccessible window longer than the cutover pause allows"

    def defaults(self) -> dict:
        return {"max_pause_s": CUTOVER_PAUSE_DESIGN_S + 3.0,
                "design_pause_s": CUTOVER_PAUSE_DESIGN_S}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        limit = float(self.opt("max_pause_s"))
        judged = 0
        for m in migs:
            samples = ev.ana_samples(m.name)
            if not samples:
                continue
            judged += 1
            windows = freeze_windows(samples)
            if not windows:
                continue
            worst = max(d for _, d in windows)
            if worst <= limit:
                continue
            yield critical(
                self.name,
                title=f"every path to some namespace was inaccessible for {worst:.0f}s",
                subject=m.name,
                detail=f"limit {limit:.0f}s; the cutover pause is meant to last about "
                       f"{float(self.opt('design_pause_s')):.0f}s",
                evidence={"worst_pause_s": round(worst, 1), "max_pause_s": limit,
                          "freezes": len(windows), "phase": m.phase},
                note="An application that sees an I/O error during a migration sees it "
                     "inside this window, so its length bounds the blast radius.",
            )
        if not judged:
            raise SkipDetector("no ANA samples for any migration")


@detector
class SplitBrain(Detector):
    """Source and target both serving at the same instant.

    Two simultaneously optimized paths to two copies means a read can land on either, and
    the one that is behind returns data that was never written there. This is the one ANA
    defect that is silent corruption *by construction* rather than by race, so it is worth
    checking even though it has not yet been observed.
    """

    name = "ana.split-brain"
    summary = "source and target paths both optimized at the same instant (two writers)"

    def defaults(self) -> dict:
        return {"optimized_states": ["optimized"]}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        opt = set(self.opt("optimized_states"))
        judged = 0
        for m in migs:
            samples = ev.ana_samples(m.name)
            if not samples or not any(s.role for s in samples):
                continue
            judged += 1
            per_ts: dict = {}
            for s in samples:
                if any(a in opt for a in s.ana.values()):
                    per_ts.setdefault(s.ts, {}).setdefault(s.role or "?", set()).add(s.address)
            for ts, roles in sorted(per_ts.items()):
                if "source" in roles and "target" in roles:
                    yield critical(
                        self.name,
                        title="source and target both optimized at the same instant",
                        subject=m.name,
                        detail=f"at {ts:%H:%M:%S}: source={sorted(roles['source'])} "
                               f"target={sorted(roles['target'])}",
                        evidence={"ts": str(ts), "source": sorted(roles["source"]),
                                  "target": sorted(roles["target"])},
                        note="Reads can be served by either copy; the one that is behind "
                             "returns data that was never written at that offset.",
                    )
                    break  # one finding per migration is enough to act on
        if not judged:
            raise SkipDetector("no role-labelled ANA samples (needs source/target roles)")


@detector
class UnservedAfterCutover(Detector):
    """A completed migration whose target does not serve every namespace.

    The half-moved case seen from the host: the migration reports success, a live controller
    exists at the target, and one of the subsystem's namespaces has no path over it. On a
    shared subsystem that is one volume left stranded while its siblings moved.
    """

    name = "ana.unserved-after-cutover"
    summary = "after a Completed migration, a live target controller serves only some namespaces"

    def defaults(self) -> dict:
        return {"completed_phases": ["Completed"]}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = [m for m in ev.migrations() if m.phase in set(self.opt("completed_phases"))]
        if not migs:
            raise SkipDetector("no completed migrations in this run")
        judged = 0
        for m in migs:
            samples = ev.ana_samples(m.name)
            targets = [s for s in samples if s.role == "target"]
            if not targets:
                continue
            judged += 1
            last_ts = max(s.ts for s in samples)
            final = [s for s in targets if s.ts == last_ts]
            live = [s for s in final if s.state == "live"]
            served = {n for s in live for n in s.accessible_nsids()}
            expected = {n for s in samples for n in s.ana}
            missing = expected - served
            if live and expected and missing:
                yield critical(
                    self.name,
                    title=f"{len(missing)} namespace(s) unserved on the target after cutover",
                    subject=m.name,
                    detail=f"target serves {sorted(served) or '-'} of {sorted(expected)}; "
                           f"missing {sorted(missing)}",
                    evidence={"served": sorted(served), "expected": sorted(expected),
                              "missing": sorted(missing)},
                )
            elif not live:
                yield warning(
                    self.name,
                    title="no live target controller after a completed migration",
                    subject=m.name,
                    detail=f"controllers at the target: "
                           f"{sorted({(s.address, s.state) for s in final})}",
                    note="Non-live controllers at the target address are normal while the "
                         "old instance is torn down; a persisting one is not.",
                )
        if not judged:
            raise SkipDetector("no role-labelled ANA samples for completed migrations")


@detector
class PathChurn(Detector):
    """An unusual number of distinct path addresses for one subsystem on one host.

    A leak indicator that does not need a live cluster: a subsystem accumulating listeners
    it never sheds shows up in the ANA samples as a growing address set. Informational by
    default, because the healthy count is topology-dependent (primary plus HA replicas per
    side) and a threshold that fits one cluster will not fit another.
    """

    name = "ana.path-churn"
    summary = "more distinct path addresses per host than the topology should produce"

    def defaults(self) -> dict:
        return {"max_addresses": 6, "severity": "info"}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")
        limit = int(self.opt("max_addresses"))
        make = critical if self.opt("severity") == "critical" else (
            warning if self.opt("severity") == "warning" else info)
        judged = 0
        for m in migs:
            samples = ev.ana_samples(m.name)
            if not samples:
                continue
            judged += 1
            per_node: dict[str, set[str]] = {}
            for s in samples:
                per_node.setdefault(s.node, set()).add(s.address)
            for node, addrs in sorted(per_node.items()):
                if len(addrs) > limit:
                    yield make(
                        self.name,
                        title=f"{len(addrs)} distinct path addresses on {node}",
                        subject=m.name,
                        detail=", ".join(sorted(addrs)),
                        evidence={"node": node, "addresses": sorted(addrs), "limit": limit},
                        note="An ip names a node; only ip:port names a path. Extra "
                             "addresses on one node are usually leaked listeners from "
                             "earlier migrations.",
                    )
        if not judged:
            raise SkipDetector("no ANA samples for any migration")


def freeze_summary(ev: Evidence) -> list[tuple[str, int, float]]:
    """(migration, freezes, worst window) for every migration with samples.

    Exposed because it is the table that makes the freeze-count result legible at a glance,
    and callers other than the detector want it — the CLI prints it and the README quotes it.
    """
    out = []
    for m in ev.migrations():
        samples: list[AnaSample] = ev.ana_samples(m.name)
        if not samples:
            continue
        w = freeze_windows(samples)
        out.append((m.name, len(w), max((d for _, d in w), default=0.0)))
    return out
