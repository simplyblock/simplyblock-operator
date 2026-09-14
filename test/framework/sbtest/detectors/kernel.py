"""Kernel-side detectors, from dmesg.

dmesg is the only source that says what the *host* did about a fabric event, and it turns
out to carry things nothing else does.

The headline is a severity ladder. When every path to a namespace goes away, the kernel does
not simply fail I/O — it queues, waits, and only fails once a timeout expires:

    all paths inaccessible
      -> "block nvmeXnY: no usable path - requeuing I/O"    queued; the application waits
      -> "nvme nvmeN: failfast expired"                     fast_io_fail_tmo elapsed
      -> "block nvmeXnY: no available path - failing I/O"    errors reach the application
      -> "XFS (nvmeXnY): log I/O error -5"
      -> "XFS (nvmeXnY): Filesystem has been shut down"      the volume needs repair

Every stage was observed across the archived runs, and the ladder is monotone in severity:
one run reached only the first rung (163 requeues, no failures, no filesystem damage) while
a later one reached the last (221 requeues, 30 failures, 20 filesystem shutdowns). That makes
the rung reached a much better statement of blast radius than any count of ANA samples, and
it identifies the knob that decides it: **fast_io_fail_tmo**. A pause shorter than it is
absorbed; a pause longer than it becomes application-visible I/O errors and then filesystem
damage.

The second thing dmesg alone shows is that leaked controllers **outlive their cluster**. In
one run, 90% of the kernel's NVMe traffic was controllers retrying subsystems belonging to
two clusters that had already been destroyed and reinstalled. An NQN names its cluster, so
that is decidable without any threshold — see ForeignCluster.

**Everything here is bounded to the run's window**, and that is not a detail. dmesg is a ring
buffer covering hours, so it routinely holds the previous runs' damage and a cluster's worth
of leftover debris. Counting that against this run means a clean run inherits its
predecessor's mess — and, worse, a genuinely broken run hides inside inherited noise. Events
dated before the run are reported separately as Attribution.PRE_EXISTING, which makes the
verdict INCONCLUSIVE rather than FAIL: the action is to clean the environment and run again.

Timestamps are read from either `--time-format=iso` (which carries an offset, so the
comparison against a UTC run window is sound) or `-T` (local time, no offset — assumed to be
the run's own timezone, and flagged as such). Events with no usable timestamp are
Attribution.UNKNOWN and still count, because "I cannot date this" must not become "not our
problem". Individual migrations are never blamed: the window is the run, not the migration.
"""

from __future__ import annotations

import fnmatch
import re
from collections.abc import Callable, Iterable, Iterator
from datetime import UTC, datetime

from ..core import (
    Attribution,
    Detector,
    Evidence,
    Finding,
    Severity,
    SkipDetector,
    critical,
    detector,
    info,
    warning,
)

#: `[Thu Aug 20 05:46:57 2026] ...` — dmesg -T, host-local, no offset.
_TS_CTIME = re.compile(r"^\[(\w{3} \w{3}\s+\d+ \d{2}:\d{2}:\d{2} \d{4})\]\s*(.*)$")
#: `2026-08-20T05:46:57,123456+00:00 ...` — dmesg --time-format=iso, with an offset.
_TS_ISO = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})[,.]\d+([+-]\d{2}:?\d{2})?\s*(.*)$")

#: Which logs these detectors read. dmesg-<node>.txt by convention.
DEFAULT_LOG_GLOBS = ["dmesg-*"]


def _parse_line(raw: str) -> tuple[datetime | None, str]:
    """(timestamp, message) from a dmesg line in either format."""
    line = raw.rstrip("\n")
    m = _TS_ISO.match(line)
    if m:
        stamp, offset, msg = m.group(1), m.group(2), m.group(3)
        try:
            return datetime.fromisoformat(stamp + (offset or "+00:00")), msg
        except ValueError:
            return None, msg
    m = _TS_CTIME.match(line)
    if m:
        try:
            # No offset in this format. Treated as UTC, which is what these hosts run and
            # what makes the window comparison meaningful; a host in another zone shifts
            # events across the boundary, which is why the ISO form is collected now.
            return datetime.strptime(m.group(1), "%a %b %d %H:%M:%S %Y").replace(
                tzinfo=UTC), m.group(2)
        except ValueError:
            return None, m.group(2)
    return None, line.strip()


def _severity(sev: Severity) -> Callable[..., Finding]:
    """The finding constructor for a severity, so a detector can vary it by attribution.

    Which it must: the same observation is a defect when the run caused it and a hygiene note
    when the run inherited it, and that difference is severity, not wording.
    """
    return {Severity.CRITICAL: critical, Severity.WARNING: warning}.get(sev, info)


class _Window:
    """Places a dmesg event inside or before the run, and counts each bucket.

    The whole point of this class is that "before the run" is tracked separately rather than
    filtered away: inherited damage is worth reporting loudly, just not as this run's fault.
    """

    def __init__(self, ev: Evidence) -> None:
        self.start, self.end = ev.run_window()

    def attribute(self, ts: datetime | None) -> Attribution:
        if ts is None or self.start is None:
            return Attribution.UNKNOWN
        return Attribution.PRE_EXISTING if ts < self.start else Attribution.RUN

    @property
    def described(self) -> str:
        if not self.start:
            return "the run window is unknown, so nothing could be dated"
        return f"run window {self.start:%b %d %H:%M:%S}..{self.end:%H:%M:%S} UTC" if self.end \
            else f"run started {self.start:%b %d %H:%M:%S} UTC"


def _dmesg_lines(ev: Evidence, globs: list[str]) -> Iterator[tuple[str, datetime | None, str]]:
    """(log name, timestamp or None, message) for every dmesg line available."""
    names = [n for n in ev.container_logs() if any(fnmatch.fnmatch(n, g) for g in globs)]
    for name in names:
        for raw in ev.container_log(name):
            ts, msg = _parse_line(raw)
            yield name, ts, msg


def _span(times: list[datetime]) -> str:
    if not times:
        return ""
    lo, hi = min(times), max(times)
    return (f"{lo:%b %d %H:%M:%S}" if lo == hi
            else f"{lo:%b %d %H:%M:%S}..{hi:%H:%M:%S} host-local")


@detector
class PathLoss(Detector):
    """How far the kernel got up the path-loss ladder, per namespace device.

    Reports the worst rung reached, because the rungs mean different things:

    * **requeued only** — the pause was absorbed. The application saw latency, not errors.
      Worth knowing (it means paths went away at all) but not a failure.
    * **failfast expired** — the pause outlasted `fast_io_fail_tmo`. This is the boundary;
      past it the kernel stops protecting the application.
    * **failing I/O** — errors reached the application. Data loss is now possible and fio
      will have noticed too (correlate with fio.job-error / fio.checksum).

    This is stronger evidence than ANA sampling for the same event: it is what the kernel
    actually did, not what a sampler happened to catch, so it cannot miss a window shorter
    than the sampling interval.
    """

    name = "kernel.path-loss"
    summary = "how far the kernel got up the path-loss ladder (requeue -> failfast -> fail I/O)"

    RE_REQUEUE = re.compile(r"block (\S+): no usable path - requeuing I/O")
    RE_FAILING = re.compile(r"block (\S+): no available path - failing I/O")
    RE_FAILFAST = re.compile(r"(nvme\d+): failfast expired")

    def defaults(self) -> dict:
        return {"logs": DEFAULT_LOG_GLOBS,
                # Requeues are expected during a cutover; this bounds how many is normal.
                "max_requeues": 0,
                # Any failed I/O is a failure — the application saw an error.
                "max_failing": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        win = _Window(ev)
        # kind -> attribution -> device -> times
        seen: dict[str, dict[Attribution, dict[str, list[datetime]]]] = {}
        saw_any = False

        for _log, ts, msg in _dmesg_lines(ev, self.opt("logs")):
            saw_any = True
            for rx, kind in ((self.RE_FAILING, "failing"), (self.RE_REQUEUE, "requeue"),
                             (self.RE_FAILFAST, "failfast")):
                m = rx.search(msg)
                if not m:
                    continue
                bucket = seen.setdefault(kind, {}).setdefault(win.attribute(ts), {})
                stamps = bucket.setdefault(m.group(1), [])
                if ts:
                    stamps.append(ts)
                break
        if not saw_any:
            raise SkipDetector("no dmesg collected in this run")

        def count(kind: str, attr: Attribution) -> int:
            return sum(len(v) or 1 for v in seen.get(kind, {}).get(attr, {}).values())

        for attr in (Attribution.RUN, Attribution.UNKNOWN, Attribution.PRE_EXISTING):
            devs_fail = seen.get("failing", {}).get(attr, {})
            n_fail = count("failing", attr)
            n_req = count("requeue", attr)
            n_ff = count("failfast", attr)
            if not (devs_fail or n_req or n_ff):
                continue
            inherited = attr is Attribution.PRE_EXISTING
            times = [t for d in seen.values() for v in d.get(attr, {}).values() for t in v]

            if devs_fail and n_fail > int(self.opt("max_failing")):
                # Pre-existing damage is reported but never fails the run: I/O the kernel
                # gave up on before this run started is the previous run's story.
                make = _severity(Severity.WARNING if inherited else Severity.CRITICAL)
                yield make(
                    self.name,
                    title=(f"the kernel failed I/O on {len(devs_fail)} device(s) for want of "
                           f"a path" + (" before this run began" if inherited else "")),
                    subject="fabric",
                    detail=(f"{n_fail} occurrence(s) on {', '.join(sorted(devs_fail))}"
                            + (f"; {_span(times)}" if times else "")
                            + f"; {win.described}"),
                    evidence={"failing_io": n_fail, "devices": sorted(devs_fail),
                              "requeues": n_req, "failfast": n_ff,
                              "attribution": str(attr)},
                    artifacts=sorted(set(self.opt("logs"))),
                    attribution=attr,
                    note=("Inherited from before the run — worth cleaning up, but it says "
                          "nothing about this run's code."
                          if inherited else
                          "This is the rung past which the kernel stops protecting the "
                          "application: queued I/O is given up and the error returned. "
                          "Filesystem damage follows — see kernel.filesystem-shutdown — and "
                          "fio will have seen it too."),
                )

            if n_ff:
                yield _severity(Severity.INFO if inherited else Severity.WARNING)(
                    self.name,
                    title=(f"fast_io_fail_tmo expired {n_ff} time(s)"
                           + (" before this run began" if inherited else "")),
                    subject="fabric",
                    detail=f"{len(seen.get('failfast', {}).get(attr, {}))} controller(s)"
                           + (f"; {_span(times)}" if times else ""),
                    evidence={"failfast": n_ff, "attribution": str(attr)},
                    attribution=attr,
                    note=("" if inherited else
                          "The outage outlasted fast_io_fail_tmo, the knob that decides "
                          "whether an all-paths-inaccessible window is absorbed or becomes "
                          "application-visible. A cutover pause longer than this value "
                          "cannot be survived by queueing."),
                )

            if n_req > int(self.opt("max_requeues")):
                yield _severity(Severity.INFO if inherited else Severity.WARNING)(
                    self.name,
                    title=(f"the kernel had no usable path on "
                           f"{len(seen.get('requeue', {}).get(attr, {}))} device(s)"
                           + (" before this run began" if inherited else "")),
                    subject="fabric",
                    detail=f"{n_req} requeue(s)" + (f"; {_span(times)}" if times else ""),
                    evidence={"requeues": n_req, "attribution": str(attr)},
                    attribution=attr,
                    note=("" if inherited else
                          "I/O was queued rather than failed, so the application survived — "
                          "but every path to these namespaces was gone. This is the kernel's "
                          "own record of the window ana.freeze-count samples for, and it "
                          "cannot miss one shorter than the sampling interval."),
                )


@detector
class FilesystemShutdown(Detector):
    """A filesystem that took itself offline because its log I/O failed.

    The end of the path-loss ladder, and the most consequential thing a run can leave behind:
    the volume is unusable until it is unmounted and repaired, which no amount of retrying
    fixes. Separate from kernel.path-loss because it is the *consequence* — someone asking
    "did this run damage a filesystem" wants exactly this and nothing else.
    """

    name = "kernel.filesystem-shutdown"
    summary = "XFS/ext4 shut down or went read-only after failed log I/O"

    PATTERNS = (
        (re.compile(r"(XFS|EXT4-fs) \(([^)]+)\).*(?:has been shut down|Filesystem has been shut down)"),
         "filesystem shut down"),
        (re.compile(r"(XFS|EXT4-fs) \(([^)]+)\).*log I/O error"), "log I/O error"),
        (re.compile(r"(XFS|EXT4-fs) \(([^)]+)\).*(?:Remounting filesystem read-only|"
                    r"remounting filesystem read-only)"), "remounted read-only"),
        (re.compile(r"(XFS|EXT4-fs) \(([^)]+)\).*metadata I/O error"), "metadata I/O error"),
    )

    def defaults(self) -> dict:
        return {"logs": DEFAULT_LOG_GLOBS}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        win = _Window(ev)
        # attribution -> device -> kinds -> times
        hits: dict[Attribution, dict[str, dict[str, list[datetime]]]] = {}
        saw_any = False
        for _log, ts, msg in _dmesg_lines(ev, self.opt("logs")):
            saw_any = True
            for rx, kind in self.PATTERNS:
                m = rx.search(msg)
                if m:
                    slot = hits.setdefault(win.attribute(ts), {}).setdefault(
                        m.group(2), {}).setdefault(kind, [])
                    if ts:
                        slot.append(ts)
                    break
        if not saw_any:
            raise SkipDetector("no dmesg collected in this run")

        for attr, devices in hits.items():
            inherited = attr is Attribution.PRE_EXISTING
            shutdown = sorted(d for d, k in devices.items() if "filesystem shut down" in k)
            times = [t for k in devices.values() for v in k.values() for t in v]
            if shutdown:
                # Pre-existing damage is a hygiene warning, not this run's failure: a
                # filesystem that died before the run started is the previous run's evidence.
                # It only invalidates *this* run if the run went on to use that volume, which
                # dmesg alone cannot establish — nvme.dirty-start is the check that can.
                yield _severity(Severity.WARNING if inherited else Severity.CRITICAL)(
                    self.name,
                    title=(f"{len(shutdown)} filesystem(s) shut down after failed log I/O"
                           + (" before this run began" if inherited else "")),
                    subject="fabric",
                    detail=(", ".join(shutdown) + (f"; {_span(times)}" if times else "")
                            + f"; {win.described}"),
                    evidence={"devices": shutdown, "attribution": str(attr),
                              "kinds": {d: sorted(k) for d, k in sorted(devices.items())}},
                    attribution=attr,
                    note=("Left over from earlier activity on these hosts. Worth cleaning up "
                          "— the volume needs unmount and repair — but not evidence about "
                          "this run."
                          if inherited else
                          "The volume is unusable until it is unmounted and repaired; "
                          "retrying does not recover it. This is the end of the path-loss "
                          "ladder — see kernel.path-loss for how it got here."),
                )
            elif devices:
                yield _severity(Severity.INFO if inherited else Severity.WARNING)(
                    self.name,
                    title=(f"filesystem I/O errors on {len(devices)} device(s), no shutdown"
                           + (" (before this run)" if inherited else "")),
                    subject="fabric",
                    detail=", ".join(f"{d}: {', '.join(sorted(k))}"
                                     for d, k in sorted(devices.items())),
                    evidence={"kinds": {d: sorted(k) for d, k in sorted(devices.items())},
                              "attribution": str(attr)},
                    attribution=attr,
                )


@detector
class ForeignCluster(Detector):
    """A controller retrying a subsystem that belongs to a cluster which no longer exists.

    An NQN names its cluster, so this needs no threshold and no topology knowledge: if the
    run is against cluster A and the kernel is retrying NQNs for cluster B, those controllers
    are leaked, full stop.

    This is the sharpest form of "controllers not disappearing". Across the archived runs the
    leak survived not just the migration that made it but the cluster's *destruction and
    reinstallation*: in one run, 90% of the kernel's NVMe log traffic was retries against two
    clusters that had already been torn down. Nothing on the host ever removes them, and
    because the kernel resets its reconnect counter on a partial reconnect, they are
    effectively immortal until the node reboots.

    **Reported as hygiene, never as a run failure.** By construction these belong to a cluster
    that no longer exists, so they cannot make a migration of *this* cluster's subsystems fail
    — they are noise in the logs and wasted work on the host, which is a real thing to fix and
    a bad thing to fail a build over. The leak that does invalidate a run is a leak on the
    *live* cluster's subsystems: see nvme.dirty-start.
    """

    name = "nvme.foreign-cluster"
    summary = "controllers retrying subsystems of a cluster that is no longer the live one"

    RE_NQN_CLUSTER = re.compile(r"simplyblock:([0-9a-f]{8}-[0-9a-f-]{27}):")

    def defaults(self) -> dict:
        return {"logs": DEFAULT_LOG_GLOBS, "max_lines": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        current = ev.cluster_uuid()
        if not current:
            raise SkipDetector("the run's cluster uuid is unknown, so a foreign NQN "
                               "cannot be told from the live one")
        per_cluster: dict[str, int] = {}
        saw_any = False
        for _log, _ts, msg in _dmesg_lines(ev, self.opt("logs")):
            saw_any = True
            m = self.RE_NQN_CLUSTER.search(msg)
            if m:
                per_cluster[m.group(1)] = per_cluster.get(m.group(1), 0) + 1
        if not saw_any:
            raise SkipDetector("no dmesg collected in this run")

        foreign = {c: n for c, n in per_cluster.items() if c != current}
        if not foreign or sum(foreign.values()) <= int(self.opt("max_lines")):
            return
        total = sum(per_cluster.values()) or 1
        share = sum(foreign.values()) / total
        yield warning(
            self.name,
            title=f"{len(foreign)} dead cluster(s) still being retried by this host",
            subject="fabric",
            detail=("; ".join(f"{c[:8]}={n} lines" for c, n in
                              sorted(foreign.items(), key=lambda x: -x[1]))
                    + f"; live cluster {current[:8]}={per_cluster.get(current, 0)} "
                      f"({share:.0%} of NVMe log traffic is for dead clusters)"),
            evidence={"live_cluster": current, "foreign": foreign,
                      "foreign_share": round(share, 3)},
            attribution=Attribution.PRE_EXISTING,
            note="These controllers outlived the cluster they belong to, so nothing will "
                 "ever answer them. They cost a reconnect every ctrl_loss_tmo window and "
                 "bury the live cluster's messages in the log, but they cannot affect a "
                 "migration of this cluster's subsystems — so this is hygiene, not a "
                 "verdict. Only a disconnect or a node reboot clears them.",
        )


@detector
class ControllerChurn(Detector):
    """Controllers created versus removed — "they never disappear", counted.

    A run that connects paths and tears them down again nets out. One that leaks shows a
    positive delta, and a reconnect storm shows as a retry count out of all proportion to the
    number of controllers involved.

    Both halves are needed. The delta alone misses a leak whose controller was created before
    the dmesg ring wrapped; the retry rate alone cannot say whether the retries belong to one
    doomed controller or fifty healthy ones.
    """

    name = "nvme.controller-churn"
    summary = "controllers created vs removed, and how hard the survivors are retrying"

    RE_NEW = re.compile(r"nvme(\d+): new ctrl|nvme(\d+): creating \d+ I/O queues")
    RE_GONE = re.compile(r"nvme(\d+): Removing ctrl")
    RE_RETRY = re.compile(r"nvme(\d+): Failed reconnect attempt (\d+)")

    def defaults(self) -> dict:
        return {"logs": DEFAULT_LOG_GLOBS,
                # A positive delta this large means controllers are accumulating.
                "max_net_created": 4,
                # A single controller retrying this many times is not coming back.
                "max_retries_per_controller": 100}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        created: set[str] = set()
        removed: set[str] = set()
        worst_retry: dict[str, int] = {}
        saw_any = False
        for _log, _ts, msg in _dmesg_lines(ev, self.opt("logs")):
            saw_any = True
            m = self.RE_NEW.search(msg)
            if m:
                created.add(m.group(1) or m.group(2))
            m = self.RE_GONE.search(msg)
            if m:
                removed.add(m.group(1))
            m = self.RE_RETRY.search(msg)
            if m:
                ctrl, n = m.group(1), int(m.group(2))
                worst_retry[ctrl] = max(worst_retry.get(ctrl, 0), n)
        if not saw_any:
            raise SkipDetector("no dmesg collected in this run")

        net = len(created - removed)
        if net > int(self.opt("max_net_created")):
            yield warning(
                self.name,
                title=f"{net} controller(s) created and never removed",
                subject="fabric",
                detail=f"created={len(created)} removed={len(removed)}; "
                       f"never removed: {', '.join(sorted(created - removed)[:16])}",
                evidence={"created": len(created), "removed": len(removed), "net": net,
                          "never_removed": sorted(created - removed)},
                note="dmesg is a ring buffer, so a controller created before it wrapped will "
                     "look un-created rather than un-removed; the delta is a floor on the "
                     "leak, not the whole of it.",
            )

        stuck = {c: n for c, n in worst_retry.items()
                 if n > int(self.opt("max_retries_per_controller"))}
        if stuck:
            yield warning(
                self.name,
                title=f"{len(stuck)} controller(s) retrying without ever succeeding",
                subject="fabric",
                detail="; ".join(f"nvme{c}={n} attempts" for c, n in
                                 sorted(stuck.items(), key=lambda x: -x[1])[:12]),
                evidence={"controllers": {f"nvme{c}": n for c, n in sorted(stuck.items())},
                          "threshold": int(self.opt("max_retries_per_controller"))},
                note="The reconnect counter resets on a partial reconnect, so a high value "
                     "means the target is actively refusing this host rather than being "
                     "briefly unreachable. Check nvme.foreign-cluster: the usual cause is a "
                     "controller whose subsystem no longer exists.",
            )
        if not stuck and net <= int(self.opt("max_net_created")):
            yield info(self.name, title=f"{len(created)} created / {len(removed)} removed",
                       subject="fabric",
                       evidence={"created": len(created), "removed": len(removed)})


@detector
class FabricErrors(Detector):
    """Fabric-level errors that indicate instability short of losing a path.

    None of these is a failure on its own — they are the texture around one, and they are
    worth surfacing because they say *where* the fabric hurt: a socket that will not
    establish is a different problem from a controller that connects and then cannot be
    configured.
    """

    name = "kernel.fabric-errors"
    summary = "connect/reset/timeout errors from the NVMe fabric layer"

    PATTERNS = (
        (re.compile(r"starting error recovery"), "error recovery started"),
        (re.compile(r"Property Set error"), "property set failed (controller config)"),
        (re.compile(r"Identify Descriptors failed"), "identify descriptors failed"),
        (re.compile(r"failed to connect socket: -(\d+)"), "socket connect failed"),
        (re.compile(r"failed to connect queue: \d+ ret=(\d+)"), "queue connect failed"),
        (re.compile(r"queue \d+ socket state (\d+)"), "socket in unexpected state"),
        (re.compile(r"I/O \d+ (?:\(\S+\) )?QID \d+ timeout"), "I/O timeout"),
        (re.compile(r"Connect Invalid Data Parameter"), "connect refused: no such subsystem"),
        (re.compile(r"is not allowed, hostnqn"), "connect refused: host not in allow-list"),
        (re.compile(r"rescanning namespaces"), "namespace rescan"),
    )

    def defaults(self) -> dict:
        return {"logs": DEFAULT_LOG_GLOBS,
                #: kind -> minimum count before it is worth reporting. A rescan or a refused
                #: connect happens in ones and twos on any healthy run.
                "min_counts": {"namespace rescan": 50,
                               "connect refused: no such subsystem": 50,
                               "connect refused: host not in allow-list": 50,
                               "socket connect failed": 50,
                               "queue connect failed": 50},
                "default_min_count": 1}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        counts: dict[str, int] = {}
        saw_any = False
        for _log, _ts, msg in _dmesg_lines(ev, self.opt("logs")):
            saw_any = True
            for rx, kind in self.PATTERNS:
                if rx.search(msg):
                    counts[kind] = counts.get(kind, 0) + 1
                    break
        if not saw_any:
            raise SkipDetector("no dmesg collected in this run")

        mins = dict(self.opt("min_counts") or {})
        floor = int(self.opt("default_min_count"))
        reportable = {k: v for k, v in counts.items() if v >= mins.get(k, floor)}
        if not reportable:
            return
        yield warning(
            self.name,
            title=f"{sum(reportable.values())} fabric error(s) across {len(reportable)} kind(s)",
            subject="fabric",
            detail="; ".join(f"{k}={v}" for k, v in
                             sorted(reportable.items(), key=lambda x: -x[1])),
            evidence={"counts": reportable, "all_counts": counts},
            note="Texture rather than a verdict, and spanning whatever the ring buffer held "
                 "rather than only this run: read it next to kernel.path-loss to see whether "
                 "the fabric merely wobbled or actually lost a path.",
        )


@detector
class DirtyStart(Detector):
    """The fabric was already carrying blocking debris for the **live** cluster at setup.

    This is the one pre-existing condition that forfeits a run, and it is worth being precise
    about why. A controller that is live and serves no namespace makes VerifyMigrationPaths
    reject the migration of its subsystem — permanently, because nothing removes it. If that
    is already true when the run starts, then migrations that should have passed will fail,
    and the run's completion rate measures the mess it inherited rather than the code under
    test. A 13-of-46 result says nothing in that state.

    Everything about the qualification matters:

    * **live cluster only.** Debris for a destroyed cluster cannot block this cluster's
      subsystems; that is nvme.foreign-cluster's hygiene warning, not a forfeit.
    * **at setup, not at the end.** Debris the run *created* is the run's own finding — see
      nvme.stale-controllers — and failing it is correct.
    * **blocking shapes only.** A controller stuck in "connecting" is indistinguishable from
      a normal HA reconnect in a snapshot, so it does not qualify.

    Requires the nvme.snapshot component's pre-run snapshot; without it there is nothing to
    judge and the detector says so.
    """

    name = "nvme.dirty-start"
    summary = ("the fabric already held blocking debris for the live cluster when the run "
               "started, so the run's results cannot be trusted")

    def defaults(self) -> dict:
        return {"max_blocking": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        pre = getattr(ev, "nvme_controllers_pre", None)
        ctrls = pre() if callable(pre) else []
        if not ctrls:
            raise SkipDetector("no pre-run fabric snapshot (enable the nvme.snapshot "
                               "component to judge the state the run started from)")
        cluster = ev.cluster_uuid()
        if not cluster:
            raise SkipDetector("the run's cluster uuid is unknown, so debris for it cannot "
                               "be told from debris for a dead one")

        blocking = [c for c in ctrls
                    if cluster in c.nqn and c.state == "live" and c.serves_nothing]
        if len(blocking) <= int(self.opt("max_blocking")):
            return
        by_node: dict[str, list[str]] = {}
        for c in blocking:
            by_node.setdefault(c.node, []).append(f"{c.name}@{c.address}")
        yield critical(
            self.name,
            title=f"{len(blocking)} blocking controller(s) for the live cluster before the "
                  f"run started",
            subject="environment",
            detail="; ".join(f"{n}: {', '.join(sorted(v))}" for n, v in sorted(by_node.items())),
            evidence={"count": len(blocking), "live_cluster": cluster,
                      "per_node": {n: sorted(v) for n, v in by_node.items()}},
            attribution=Attribution.PRE_EXISTING,
            note="Each of these makes the pre-cutover path check reject its subsystem, so "
                 "migrations that should pass will fail and the completion rate measures the "
                 "inherited mess rather than the code. Clear the fabric and run again; do "
                 "not read this run's migration results.",
        )
