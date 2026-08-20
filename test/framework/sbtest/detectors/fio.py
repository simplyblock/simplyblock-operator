"""fio-side detectors: I/O errors, silent corruption, and sustained outages.

The checksum detector is the one that decides whether a run lost data, so its attribution
matters as much as its detection — see VERIFY_LAG_S.
"""

from __future__ import annotations

import re
from collections.abc import Iterable
from datetime import UTC, datetime, timedelta

from ..core import Detector, Evidence, Finding, SkipDetector, attribute, critical, detector, warning

#: How long after a migration ends one of its lost writes may still surface.
#:
#: fio notices a lost write when it next *reads* that block, not when the write was lost,
#: so detection trails the loss by however long the read pattern takes to come round —
#: measured at 3-34s across runs. Attribution has to allow for that: without it, a
#: migration's own losses get filed under "no migration was running", which is exactly how
#: a Completed-but-corrupting migration stayed invisible for a whole analysis pass.
VERIFY_LAG_S = 45.0

#: fio's verify failure lines. "bad magic header" is the signature seen in every event so
#: far: a block that never carried an fio header at all, i.e. a *first* write that was lost
#: rather than a stale or torn overwrite.
_VERIFY_RE = re.compile(
    r"verify:? (?:bad magic header|header|crc|md5|pattern)|"
    r"verify_(?:header|md5|crc)|"
    r"got (?:crc|md5) .*expected",
    re.IGNORECASE)

_OFFSET_RE = re.compile(r"offset[= ](\d+)")
#: CRI log prefix: 2026-08-19T22:23:18.994807954Z stderr F <msg>
_CRI_TS_RE = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})")


def _cri_ts(line: str) -> datetime | None:
    m = _CRI_TS_RE.match(line)
    if not m:
        return None
    return datetime.strptime(m.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)


@detector
class JobError(Detector):
    """An fio job that ended with an error.

    Reported per pod with the errno, because the errno is the diagnosis and they mean very
    different things: 84 is a failed *verification* (the data was wrong), 121 is a path that
    went away under the I/O (the data never arrived).

    The numbers below are **Linux** errno values, written out rather than looked up. The
    analysis usually runs somewhere other than the host that produced the run, and the errno
    table is platform-specific — 84 is EILSEQ on Linux and EOVERFLOW on macOS, so resolving it
    locally silently mislabels the most important code in the set.
    """

    name = "fio.job-error"
    summary = "an fio job ended with a non-zero errno"

    ERRNO_HINT = {
        84: "EILSEQ — fio's verify failed: a block read back did not match what was written. "
            "This is silent data corruption, not an I/O failure; see fio.checksum for the "
            "blocks and the migration they belong to",
        121: "EREMOTEIO — every path to the namespace was inaccessible under the I/O; "
             "correlate with ana.cutover-pause / ana.freeze-count on the same window",
        5: "EIO — the target failed the command outright rather than losing the path",
        28: "ENOSPC — capacity, not connectivity",
        110: "ETIMEDOUT — the command was accepted and never completed",
        108: "ESHUTDOWN — the target went away mid-flight",
    }

    def defaults(self) -> dict:
        return {"ignore_errnos": []}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        jobs = ev.fio_jobs()
        if not jobs:
            raise SkipDetector("no fio job results")
        ignore = {int(x) for x in self.opt("ignore_errnos")}
        for j in jobs:
            if not j.error or j.error in ignore:
                continue
            yield critical(
                self.name,
                title=f"fio job ended in error {j.error}",
                subject=j.pod,
                detail=self.ERRNO_HINT.get(j.error, ""),
                evidence={"errno": j.error, "total_iops": j.total_iops},
            )


@detector
class Checksum(Detector):
    """fio read back a block that does not match what it wrote — silent corruption.

    The read *succeeded*, so nothing outside fio's own verification notices: no I/O error,
    no kernel message, no control-plane event. This is the most serious thing a run can
    find, and it is why the attribution window is generous rather than exact — a lost write
    that cannot be tied to a migration is much harder to act on than one that can.
    """

    name = "fio.checksum"
    summary = "fio read back data it never wrote (silent corruption), attributed to a migration"

    def defaults(self) -> dict:
        return {"verify_lag_s": VERIFY_LAG_S}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        pods = ev.pods()
        if not pods:
            raise SkipDetector("no fio pods")
        lag = timedelta(seconds=float(self.opt("verify_lag_s")))
        migs = ev.migrations()
        seen_any_log = False
        # subject -> list of (pod, ts, line)
        per_subject: dict[str, list[tuple[str, datetime | None, str]]] = {}

        for pod in pods:
            for line in ev.fio_log(pod):
                seen_any_log = True
                if not _VERIFY_RE.search(line):
                    continue
                ts = _cri_ts(line)
                m = attribute(migs, ts, lag) if ts else None
                per_subject.setdefault(m.name if m else "outside-any-migration", []).append(
                    (pod, ts, line.strip()))

        if not seen_any_log:
            raise SkipDetector("no fio pod logs available")

        for subject, hits in sorted(per_subject.items()):
            pods_hit: dict[str, int] = {}
            offsets = []
            for pod, _ts, line in hits:
                pods_hit[pod] = pods_hit.get(pod, 0) + 1
                mo = _OFFSET_RE.search(line)
                if mo:
                    offsets.append(int(mo.group(1)))
            mig = next((m for m in migs if m.name == subject), None)
            lagged = ""
            if mig and mig.end:
                late = [t for _p, t, _l in hits if t and t > mig.end]
                if late:
                    worst = max((t - mig.end).total_seconds() for t in late)
                    lagged = (f"; {len(late)} of them detected up to {worst:.0f}s after the "
                              f"migration ended, inside fio's verify backlog")
            yield critical(
                self.name,
                title=f"{len(hits)} block(s) read back with the wrong contents",
                subject=subject,
                detail=("pods: " + ", ".join(f"{p}={n}" for p, n in sorted(pods_hit.items()))
                        + (f"; phase={mig.phase}" if mig else "") + lagged),
                evidence={"blocks": len(hits), "pods": pods_hit,
                          "offsets": sorted(offsets)[:32],
                          "phase": mig.phase if mig else "",
                          "verify_lag_s": float(self.opt("verify_lag_s"))},
                artifacts=[f"{p}/fio.log" for p in sorted(pods_hit)],
                note="The reads succeeded, so this is silent: the volume served data that "
                     "was never written at those offsets. A Completed migration can do "
                     "this — phase is not a filter.",
            )


@detector
class Outage(Detector):
    """A sustained window where a pod did no I/O at all.

    Distinct from a checksum failure and from a job error: the I/O neither failed nor
    returned wrong data, it stopped. Short dips are normal during a cutover, so only runs
    at or above `min_seconds` count.

    Reported per pod rather than per window. A pod that stalls repeatedly stalls *hundreds* of
    times in a soak — one run produced 781 qualifying windows at a 10s threshold — and a
    report with 781 entries for one symptom is a report nobody reads to the end. The worst
    windows are named, the rest are counted, and the total downtime is what the finding leads
    with, because "18% of the run did no I/O" is the fact that decides anything.
    """

    name = "fio.outage"
    summary = "a pod's I/O stopped for longer than a cutover should cost"

    def defaults(self) -> dict:
        return {"min_seconds": 30, "iops_floor": 0.0, "name_worst": 6}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        pods = ev.pods()
        if not pods:
            raise SkipDetector("no fio pods")
        floor = float(self.opt("iops_floor"))
        need = int(self.opt("min_seconds"))
        show = int(self.opt("name_worst"))
        migs = ev.migrations()
        saw_series = False

        for pod in pods:
            series = ev.fio_timeseries(pod)
            if not series:
                continue
            saw_series = True
            windows: list[tuple[int, int, int, str]] = []   # dur, from, to, migration
            run_start: int | None = None
            prev_off: int | None = None
            # None is a sentinel that closes a run still open at the end of the series.
            for s in [*series, None]:
                down = s is not None and s.total_iops <= floor
                if down:
                    assert s is not None  # implied by `down`; stated for the type checker
                    if run_start is None:
                        run_start = s.offset_s
                elif run_start is not None:
                    end = s.offset_s if s is not None else (prev_off or run_start)
                    dur = end - run_start
                    if dur >= need:
                        wall = next((x.wall for x in series if x.offset_s == run_start), None)
                        mig = attribute(migs, wall) if wall else None
                        windows.append((dur, run_start, end, mig.name if mig else ""))
                    run_start = None
                if s is not None:
                    prev_off = s.offset_s

            if not windows:
                continue
            windows.sort(key=lambda w: -w[0])
            downtime = sum(w[0] for w in windows)
            span = series[-1].offset_s - series[0].offset_s or 1
            named = ", ".join(
                f"{d}s at +{a}s" + (f" ({m})" if m else " (no migration)")
                for d, a, _, m in windows[:show])
            during = sorted({w[3] for w in windows if w[3]})
            yield critical(
                self.name,
                title=(f"I/O stopped {len(windows)}x for {downtime}s total "
                       f"({downtime / span:.0%} of the run), worst {windows[0][0]}s"),
                subject=pod,
                detail=named + (f", and {len(windows) - show} more" if len(windows) > show
                                else ""),
                evidence={"windows": len(windows), "downtime_s": downtime,
                          "worst_s": windows[0][0],
                          "fraction_of_run": round(downtime / span, 4),
                          "migrations": during,
                          "worst_windows": [{"seconds": d, "from_offset_s": a,
                                             "to_offset_s": b, "migration": m}
                                            for d, a, b, m in windows[:show]]},
                note=("Windows are attributed to the migration in flight when there was one; "
                      f"{len(during)} migration(s) are implicated here."
                      if during else
                      "None of these windows fall inside a migration, so the cause is "
                      "elsewhere — check the fabric and the node's own health."),
            )
        if not saw_series:
            raise SkipDetector("no fio time series available")


@detector
class Throughput(Detector):
    """A pod whose average IOPS is far below its peers.

    A weak signal on its own, which is why it is a WARNING: a pod on a shared subsystem that
    is being migrated repeatedly will legitimately lag. It earns its place next to the other
    detectors — a pod that is both slow and the subject of an ANA finding is a different
    story from one that is merely slow.
    """

    name = "fio.throughput-outlier"
    summary = "a pod whose average IOPS is far below the run's median"

    def defaults(self) -> dict:
        return {"min_fraction_of_median": 0.5, "min_pods": 4}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        jobs = [j for j in ev.fio_jobs() if j.total_iops > 0]
        if len(jobs) < int(self.opt("min_pods")):
            raise SkipDetector(f"needs at least {self.opt('min_pods')} pods with I/O, "
                               f"got {len(jobs)}")
        vals = sorted(j.total_iops for j in jobs)
        median = vals[len(vals) // 2]
        frac = float(self.opt("min_fraction_of_median"))
        for j in jobs:
            if j.total_iops < median * frac:
                yield warning(
                    self.name,
                    title=f"{j.total_iops:.0f} IOPS against a median of {median:.0f}",
                    subject=j.pod,
                    evidence={"total_iops": j.total_iops, "median_iops": median,
                              "fraction": round(j.total_iops / median, 3)},
                )
