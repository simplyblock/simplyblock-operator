"""Detector unit tests.

Detectors are pure functions from Evidence to findings, which is what makes this file
possible: every check below is a handful of synthetic samples rather than a cluster. The
cases are the ones the real runs taught — a healthy single freeze, a retried cutover, a
completed-but-corrupting migration, a verify failure that surfaces after its migration
ended.
"""

from __future__ import annotations

import json
import os
import sys
import unittest
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from sbtest.core import (  # noqa: E402
    AnaSample,
    Attribution,
    ControlEvent,
    FioJob,
    IopsSample,
    LogSpan,
    Migration,
    NvmeController,
    Report,
    Severity,
    SkipDetector,
    attribute_window,
    build_detector,
    freeze_windows,
)

T0 = datetime(2026, 8, 19, 22, 0, 0, tzinfo=UTC)


def ts(sec: int) -> datetime:
    return T0 + timedelta(seconds=sec)


class FakeEvidence:
    """Evidence from literals. Only what a test sets is present; the rest is empty.

    Typed explicitly rather than **kwargs so that mypy checks it against the Evidence
    protocol — a test double that has drifted from the real contract is worse than no double,
    because every detector test keeps passing while the detector itself has stopped matching.
    """

    def __init__(
        self,
        run_id: str = "test-run",
        outdir: str = "/nonexistent",
        migrations: list[Migration] | None = None,
        ana: dict[str, list[AnaSample]] | None = None,
        jobs: list[FioJob] | None = None,
        series: dict[str, list[IopsSample]] | None = None,
        fio_logs: dict[str, list[str]] | None = None,
        logs: dict[str, list[str]] | None = None,
        controllers: list[NvmeController] | None = None,
        cluster: str = "",
        window: tuple[datetime | None, datetime | None] = (None, None),
        events: list[ControlEvent] | None = None,
        spans: list[LogSpan] | None = None,
    ) -> None:
        self.run_id = run_id
        self.outdir = outdir
        self._migrations = migrations or []
        self._ana = ana or {}
        self._jobs = jobs or []
        self._series = series or {}
        self._fio_logs = fio_logs or {}
        self._logs = logs or {}
        self._ctrls = controllers or []
        self._cluster = cluster
        self._window = window
        self._events = events or []
        self._spans = spans

    def migrations(self) -> list[Migration]:
        return list(self._migrations)

    def ana_samples(self, migration: str) -> list[AnaSample]:
        return list(self._ana.get(migration, []))

    def fio_jobs(self) -> list[FioJob]:
        return list(self._jobs)

    def fio_timeseries(self, pod: str) -> list[IopsSample]:
        return list(self._series.get(pod, []))

    def fio_log(self, pod: str) -> Iterator[str]:
        return iter(self._fio_logs.get(pod, []))

    def container_logs(self) -> list[str]:
        return sorted(self._logs)

    def container_log(self, name: str) -> Iterator[str]:
        return iter(self._logs.get(name, []))

    def nvme_controllers(self) -> list[NvmeController]:
        return list(self._ctrls)

    def pods(self) -> list[str]:
        return sorted(set(self._fio_logs) | {j.pod for j in self._jobs} | set(self._series))

    def cluster_uuid(self) -> str:
        return self._cluster

    def run_window(self) -> tuple[datetime | None, datetime | None]:
        return self._window

    def control_events(self) -> list[ControlEvent]:
        return list(self._events)

    def log_spans(self) -> list[LogSpan]:
        if self._spans is not None:
            return list(self._spans)
        return [LogSpan(name=n, first=None, last=None, lines=len(self._logs[n]))
                for n in sorted(self._logs)]


def ana_series(node: str, address: str, spec: list[tuple[int, str]], nsids=(1, 2),
               state="live", role="") -> list[AnaSample]:
    """Samples for one path: spec is [(offset_seconds, ana_state), ...]."""
    return [AnaSample(ts=ts(off), node=node, address=address, state=state,
                      ana=dict.fromkeys(nsids, st), role=role)
            for off, st in spec]


class FreezeWindows(unittest.TestCase):
    def test_single_healthy_freeze(self):
        s = ana_series("vm03", "10.0.0.113:4430",
                       [(0, "optimized"), (2, "inaccessible"), (4, "optimized"),
                        (6, "optimized")])
        w = freeze_windows(s)
        self.assertEqual(len(w), 1)
        self.assertAlmostEqual(w[0][1], 2.0)

    def test_zero_length_window_is_not_a_freeze(self):
        # One sample inaccessible then immediately accessible again at the same instant is
        # a sampling artefact, not a window.
        s = [AnaSample(ts=ts(0), node="v", address="a", state="live", ana={1: "optimized"}),
             AnaSample(ts=ts(0), node="v", address="b", state="live", ana={1: "inaccessible"})]
        self.assertEqual(freeze_windows(s), [])

    def test_counts_per_node_not_summed(self):
        # Two nodes see the same freeze; the count is one, not two.
        s = ana_series("vm02", "a", [(0, "optimized"), (2, "inaccessible"), (4, "optimized")])
        s += ana_series("vm03", "b", [(0, "optimized"), (2, "inaccessible"), (4, "optimized")])
        self.assertEqual(len(freeze_windows(s)), 1)

    def test_trailing_freeze_is_counted(self):
        s = ana_series("vm03", "a", [(0, "optimized"), (2, "inaccessible"), (6, "inaccessible")])
        w = freeze_windows(s)
        self.assertEqual(len(w), 1)
        self.assertAlmostEqual(w[0][1], 4.0)


class AnaFreezeCount(unittest.TestCase):
    def detector(self, **opts):
        return build_detector("ana.freeze-count", **opts)

    def test_one_freeze_is_clean(self):
        m = Migration(name="mig-1", start=ts(0), end=ts(10), phase="Completed")
        ev = FakeEvidence(migrations=[m], ana={"mig-1": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (4, "optimized")])})
        self.assertEqual(list(self.detector().detect(ev)), [])

    def test_two_freezes_is_critical(self):
        """The mig-20 shape: froze, reverted, froze again — and it Completed."""
        m = Migration(name="mig-20", start=ts(0), end=ts(30), phase="Completed")
        ev = FakeEvidence(migrations=[m], ana={"mig-20": ana_series(
            "vm03", "a",
            [(0, "optimized"), (2, "inaccessible"), (8, "optimized"),
             (12, "inaccessible"), (18, "optimized")])})
        found = list(self.detector().detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertEqual(found[0].subject, "mig-20")
        self.assertEqual(found[0].evidence["freezes"], 2)
        # Phase must not exempt it: a Completed migration can still have lost writes.
        self.assertEqual(found[0].evidence["phase"], "Completed")

    def test_threshold_is_configurable(self):
        m = Migration(name="mig-20", start=ts(0), end=ts(30))
        ev = FakeEvidence(migrations=[m], ana={"mig-20": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (8, "optimized"),
                          (12, "inaccessible"), (18, "optimized")])})
        self.assertEqual(list(self.detector(max_freezes=2).detect(ev)), [])

    def test_skips_when_no_samples(self):
        ev = FakeEvidence(migrations=[Migration(name="m", start=ts(0))])
        with self.assertRaises(SkipDetector):
            list(self.detector().detect(ev))

    def test_skips_when_no_migrations(self):
        with self.assertRaises(SkipDetector):
            list(self.detector().detect(FakeEvidence()))

    def test_rejects_unknown_option(self):
        with self.assertRaises(ValueError):
            build_detector("ana.freeze-count", maxfreezes=2)


class AnaCutoverPause(unittest.TestCase):
    def test_pause_within_budget_is_clean(self):
        m = Migration(name="m", start=ts(0), end=ts(20))
        ev = FakeEvidence(migrations=[m], ana={"m": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (5, "optimized")])})
        self.assertEqual(list(build_detector("ana.cutover-pause").detect(ev)), [])

    def test_overlong_pause_is_critical(self):
        m = Migration(name="m", start=ts(0), end=ts(30))
        ev = FakeEvidence(migrations=[m], ana={"m": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (14, "optimized")])})
        found = list(build_detector("ana.cutover-pause").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["worst_pause_s"], 12.0)

    def test_single_long_pause_is_caught_where_freeze_count_is_not(self):
        """Why both detectors exist: one window, too long. Count says fine, pause does not."""
        m = Migration(name="m", start=ts(0), end=ts(30))
        ev = FakeEvidence(migrations=[m], ana={"m": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (20, "optimized")])})
        self.assertEqual(list(build_detector("ana.freeze-count").detect(ev)), [])
        self.assertEqual(len(list(build_detector("ana.cutover-pause").detect(ev))), 1)

    def test_two_short_freezes_are_caught_where_pause_is_not(self):
        """And the converse: two design-length windows. Pause says fine, count does not."""
        m = Migration(name="m", start=ts(0), end=ts(30))
        ev = FakeEvidence(migrations=[m], ana={"m": ana_series(
            "vm03", "a", [(0, "optimized"), (2, "inaccessible"), (5, "optimized"),
                          (10, "inaccessible"), (13, "optimized")])})
        self.assertEqual(list(build_detector("ana.cutover-pause").detect(ev)), [])
        self.assertEqual(len(list(build_detector("ana.freeze-count").detect(ev))), 1)


class AnaSplitBrain(unittest.TestCase):
    def test_both_sides_optimized_is_critical(self):
        m = Migration(name="m", start=ts(0), end=ts(20))
        s = ana_series("vm03", "10.0.0.112:4426", [(0, "optimized")], role="source")
        s += ana_series("vm03", "10.0.0.114:4428", [(0, "optimized")], role="target")
        found = list(build_detector("ana.split-brain").detect(
            FakeEvidence(migrations=[m], ana={"m": s})))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)

    def test_target_non_optimized_is_normal_ha_standby(self):
        m = Migration(name="m", start=ts(0), end=ts(20))
        s = ana_series("vm03", "10.0.0.112:4426", [(0, "optimized")], role="source")
        s += ana_series("vm03", "10.0.0.114:4428", [(0, "non-optimized")], role="target")
        self.assertEqual(list(build_detector("ana.split-brain").detect(
            FakeEvidence(migrations=[m], ana={"m": s}))), [])

    def test_skips_without_roles(self):
        m = Migration(name="m", start=ts(0), end=ts(20))
        s = ana_series("vm03", "a", [(0, "optimized")])
        with self.assertRaises(SkipDetector):
            list(build_detector("ana.split-brain").detect(
                FakeEvidence(migrations=[m], ana={"m": s})))


class FioChecksum(unittest.TestCase):
    LINE = ("2026-08-19T22:00:{sec:02d}.123456789Z stderr F verify: bad magic header 0, "
            "wanted acca at file /data/fiotest offset 43999232, length 4096")

    def test_attributes_a_lagged_detection_to_its_migration(self):
        """The mig-20 case: the loss surfaces after the migration ended, inside the backlog.

        Without the lag this lands in "outside-any-migration", which is how a
        Completed-but-corrupting migration hid.
        """
        m = Migration(name="mig-20", start=ts(0), end=ts(10), phase="Completed")
        ev = FakeEvidence(migrations=[m],
                          fio_logs={"fio-16": [self.LINE.format(sec=28)]})
        found = list(build_detector("fio.checksum").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].subject, "mig-20")
        self.assertIn("verify backlog", found[0].detail)

    def test_beyond_the_lag_is_not_attributed(self):
        m = Migration(name="mig-20", start=ts(0), end=ts(10), phase="Completed")
        ev = FakeEvidence(migrations=[m],
                          fio_logs={"fio-16": [self.LINE.format(sec=59)]})
        found = list(build_detector("fio.checksum", verify_lag_s=5).detect(ev))
        self.assertEqual(found[0].subject, "outside-any-migration")

    def test_groups_blocks_per_migration_and_counts_pods(self):
        m = Migration(name="mig-29", start=ts(0), end=ts(10), phase="TIMEOUT")
        ev = FakeEvidence(migrations=[m], fio_logs={
            "fio-6": [self.LINE.format(sec=5), self.LINE.format(sec=6)],
            "fio-7": [self.LINE.format(sec=7)]})
        found = list(build_detector("fio.checksum").detect(ev))
        self.assertEqual(found[0].evidence["blocks"], 3)
        self.assertEqual(found[0].evidence["pods"], {"fio-6": 2, "fio-7": 1})

    def test_clean_log_yields_nothing(self):
        ev = FakeEvidence(fio_logs={"fio-1": ["all good\n"]})
        self.assertEqual(list(build_detector("fio.checksum").detect(ev)), [])

    def test_skips_without_logs(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("fio.checksum").detect(FakeEvidence(jobs=[FioJob(pod="p")])))


class FioJobError(unittest.TestCase):
    def test_eremoteio_carries_the_hint_that_points_at_ana(self):
        ev = FakeEvidence(jobs=[FioJob(pod="fio-0", error=121)])
        found = list(build_detector("fio.job-error").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertIn("EREMOTEIO", found[0].detail)

    def test_eilseq_is_named_as_corruption_not_an_io_failure(self):
        """Linux errno 84. Worth pinning: it is EOVERFLOW on macOS, so a local lookup would
        mislabel the one code that means the data was wrong."""
        ev = FakeEvidence(jobs=[FioJob(pod="fio-13", error=84)])
        found = list(build_detector("fio.job-error").detect(ev))
        self.assertIn("EILSEQ", found[0].detail)
        self.assertIn("corruption", found[0].detail)

    def test_clean_jobs_yield_nothing(self):
        ev = FakeEvidence(jobs=[FioJob(pod="fio-0", error=0)])
        self.assertEqual(list(build_detector("fio.job-error").detect(ev)), [])

    def test_ignore_list(self):
        ev = FakeEvidence(jobs=[FioJob(pod="fio-0", error=121)])
        self.assertEqual(list(build_detector("fio.job-error", ignore_errnos=[121]).detect(ev)), [])


class FioOutage(unittest.TestCase):
    def series(self, pattern: str) -> list[IopsSample]:
        # "1" = doing I/O, "0" = stopped; one character per second.
        return [IopsSample(offset_s=i, wall=ts(i), total_iops=100.0 if c == "1" else 0.0)
                for i, c in enumerate(pattern)]

    def test_short_dip_is_not_an_outage(self):
        ev = FakeEvidence(series={"p": self.series("1" * 10 + "0" * 5 + "1" * 10)})
        self.assertEqual(list(build_detector("fio.outage", min_seconds=30).detect(ev)), [])

    def test_sustained_stop_is_critical(self):
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40 + "1" * 5)})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["worst_s"], 40)
        self.assertEqual(found[0].evidence["downtime_s"], 40)

    def test_trailing_outage_is_reported(self):
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40)})
        self.assertEqual(len(list(build_detector("fio.outage", min_seconds=30).detect(ev))), 1)

    def test_many_windows_collapse_into_one_finding_per_pod(self):
        """A repeatedly stalling pod stalls hundreds of times in a soak — one archived run
        produced 781 qualifying windows. Per-window findings make the report unreadable, so
        the count and the total are the finding and the worst windows are named."""
        # Trailing "1" so all 20 windows are closed by a resumption: a window still open at
        # the end of the series is measured to the last sample, which is one second short.
        ev = FakeEvidence(series={"p": self.series(("1" * 5 + "0" * 31) * 20 + "1")})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["windows"], 20)
        self.assertEqual(found[0].evidence["downtime_s"], 20 * 31)
        self.assertIn("20x", found[0].title)

    def test_windows_outside_any_migration_say_so(self):
        """Attribution is the difference between "migration cost this" and "the cluster is
        unwell", and the note must not imply the former when it was neither."""
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40)})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(found[0].evidence["migrations"], [])
        self.assertIn("elsewhere", found[0].note)

    def test_a_gap_that_recovered_is_a_freeze_not_a_loss(self):
        """A cutover is a freeze by design: every write was eventually taken. It still fails
        the run at this length, but calling it loss says data went missing when none did."""
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40 + "1" * 5)})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["kind"], "freeze")
        self.assertIn("froze", found[0].title)
        self.assertIn("no I/O was lost", found[0].note)
        self.assertTrue(found[0].evidence["worst_windows"][0]["recovered"])

    def test_a_gap_still_open_when_fio_stopped_is_a_loss(self):
        """Nothing observed the volume come back, so this is I/O it was supposed to accept
        and never did — the finding a reader must not have to dig for."""
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40)})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(found[0].evidence["kind"], "loss")
        self.assertIn("LOSS", found[0].title)
        self.assertFalse(found[0].evidence["worst_windows"][0]["recovered"])

    def test_a_pod_with_both_reports_the_loss_first(self):
        """Both fail the run; the order they are emitted in is the order they are read in,
        and a gap that never closed outranks one that did."""
        ev = FakeEvidence(series={"p": self.series("1" * 5 + "0" * 40 + "1" * 5 + "0" * 40)})
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual([f.evidence["kind"] for f in found], ["loss", "freeze"])
        self.assertEqual([f.evidence["windows"] for f in found], [1, 1])

    def test_a_gap_beginning_before_the_migration_still_belongs_to_it(self):
        """The host goes dry before the operator records the migration as started. Testing
        only the window's first second files those gaps under no migration at all."""
        mig = Migration(name="mig-1", start=ts(20), end=ts(60))
        ev = FakeEvidence(series={"p": self.series("1" * 10 + "0" * 40 + "1")},
                          migrations=[mig])
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(found[0].evidence["migrations"], ["mig-1"])

    def test_a_window_spanning_two_migrations_names_the_one_holding_most_of_it(self):
        ev = FakeEvidence(
            series={"p": self.series("1" * 10 + "0" * 40 + "1")},
            migrations=[Migration(name="mig-a", start=ts(5), end=ts(20)),
                        Migration(name="mig-b", start=ts(30), end=ts(70))])
        found = list(build_detector("fio.outage", min_seconds=30).detect(ev))
        self.assertEqual(found[0].evidence["migrations"], ["mig-b"])


class AttributeWindow(unittest.TestCase):
    """The primitive behind outage attribution: a symptom that lasted, not one that fired."""

    def named(self, migs: list[Migration], start: datetime, end: datetime) -> str:
        m = attribute_window(migs, start, end)
        return m.name if m else ""

    def test_a_window_wholly_inside_a_migration(self):
        migs = [Migration(name="m", start=ts(0), end=ts(100))]
        self.assertEqual(self.named(migs, ts(10), ts(20)), "m")

    def test_a_window_that_misses_every_migration(self):
        migs = [Migration(name="m", start=ts(0), end=ts(10))]
        self.assertIsNone(attribute_window(migs, ts(20), ts(30)))

    def test_touching_counts_as_overlapping(self):
        """Zero is an overlap: a gap that begins the second a migration ends is the
        migration's, and the sampling interval must not decide that."""
        migs = [Migration(name="m", start=ts(0), end=ts(10))]
        self.assertEqual(self.named(migs, ts(10), ts(50)), "m")

    def test_the_largest_overlap_wins_not_the_first(self):
        migs = [Migration(name="early", start=ts(0), end=ts(15)),
                Migration(name="late", start=ts(20), end=ts(60))]
        self.assertEqual(self.named(migs, ts(10), ts(50)), "late")

    def test_a_running_migration_is_a_point_in_time(self):
        """`end` is None while it is in flight — the same convention `covers` uses."""
        migs = [Migration(name="m", start=ts(30), end=None)]
        self.assertEqual(self.named(migs, ts(10), ts(50)), "m")


class NvmeStaleControllers(unittest.TestCase):
    def ctrl(self, name, state, ns, node="vm03", nqn="nqn:lvol:x", addr="10.0.0.1:4420", clt=60):
        return NvmeController(node=node, name=name, nqn=nqn, address=addr, state=state,
                              namespaces=ns, ctrl_loss_tmo=clt)

    def test_live_with_no_namespace_is_critical(self):
        """The state that blocked every later migration of a subsystem."""
        ev = FakeEvidence(controllers=[
            self.ctrl("nvme12", "live", {}),
            self.ctrl("nvme13", "live", {1: "optimized"}, addr="10.0.0.2:4420")])
        found = [f for f in build_detector("nvme.stale-controllers").detect(ev)
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["count"], 1)

    def test_connecting_is_only_a_warning(self):
        # A snapshot cannot tell a leak from a normal HA reconnect.
        ev = FakeEvidence(controllers=[self.ctrl("nvme6", "connecting", {})])
        sevs = {f.severity for f in build_detector("nvme.stale-controllers").detect(ev)}
        self.assertNotIn(Severity.CRITICAL, sevs)
        self.assertIn(Severity.WARNING, sevs)

    def test_healthy_fabric_is_clean(self):
        ev = FakeEvidence(controllers=[
            self.ctrl("nvme1", "live", {1: "optimized"}, addr="10.0.0.1:4420"),
            self.ctrl("nvme2", "live", {1: "non-optimized"}, addr="10.0.0.2:4420")])
        self.assertEqual(list(build_detector("nvme.stale-controllers").detect(ev)), [])

    def test_skips_without_a_snapshot(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("nvme.stale-controllers").detect(FakeEvidence()))

    def test_loss_timeout_flags_a_value_that_outlives_the_run(self):
        ev = FakeEvidence(controllers=[self.ctrl("nvme1", "live", {1: "optimized"}, clt=3600)])
        found = list(build_detector("nvme.loss-timeout").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["values"], [3600])


class LogPattern(unittest.TestCase):
    def test_catalogue_catches_the_undrained_transfer(self):
        ev = FakeEvidence(logs={"spdk-4422": [
            "transfer task failed: ----- but still have outstanding io 1\n"] * 3})
        found = [f for f in build_detector("logs.pattern").detect(ev)
                 if f.subject == "spdk.undrained-transfer"]
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertEqual(found[0].evidence["total"], 3)

    def test_min_count_suppresses_a_line_that_is_normal_in_small_numbers(self):
        ev = FakeEvidence(logs={"spdk-4422": ["does not allow host X to connect at this address\n"]})
        found = [f for f in build_detector("logs.pattern").detect(ev)
                 if f.subject == "nvme.host-not-allowed"]
        self.assertEqual(found, [])  # catalogue min_count is 50

    def test_user_defined_pattern_replaces_the_catalogue(self):
        ev = FakeEvidence(logs={"mylog": ["something odd happened\n"]})
        d = build_detector("logs.pattern", patterns=[
            {"id": "my.check", "regex": "something odd", "logs": ["mylog"],
             "severity": "critical"}])
        found = list(d.detect(ev))
        self.assertEqual([f.subject for f in found], ["my.check"])

    def test_log_glob_scopes_the_pattern(self):
        ev = FakeEvidence(logs={"spdk-4420": ["boom\n"], "operator": ["boom\n"]})
        d = build_detector("logs.pattern", patterns=[
            {"id": "only.spdk", "regex": "boom", "logs": ["spdk-*"], "severity": "warning"}])
        found = list(d.detect(ev))
        self.assertEqual(found[0].evidence["per_log"], {"spdk-4420": 1})

    def test_rejects_a_malformed_pattern(self):
        ev = FakeEvidence(logs={"x": ["y"]})
        d = build_detector("logs.pattern", patterns=[{"id": "a", "rgex": "y"}])
        with self.assertRaises(ValueError):
            list(d.detect(ev))

    def test_skips_without_logs(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("logs.pattern").detect(FakeEvidence()))


class MigrationOutcomes(unittest.TestCase):
    def test_low_completion_is_critical(self):
        migs = ([Migration(name=f"m{i}", start=ts(i), phase="TIMEOUT") for i in range(25)]
                + [Migration(name=f"c{i}", start=ts(100 + i), phase="Completed") for i in range(13)])
        found = list(build_detector("migration.outcomes").detect(FakeEvidence(migrations=migs)))
        crit = [f for f in found if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 2)  # completion rate and timeout rate
        self.assertTrue(any("timed out" in f.title for f in crit))

    def test_healthy_run_reports_info_only(self):
        migs = [Migration(name=f"c{i}", start=ts(i), phase="Completed") for i in range(10)]
        found = list(build_detector("migration.outcomes").detect(FakeEvidence(migrations=migs)))
        self.assertTrue(all(f.severity == Severity.INFO for f in found))

    def test_errors_group_by_shape(self):
        migs = [Migration(name=f"m{i}", start=ts(i), phase="Failed",
                          error=f"NVMe path validation failed on node vm0{i}; cancelled")
                for i in range(3)]
        found = list(build_detector("migration.errors").detect(FakeEvidence(migrations=migs)))
        self.assertEqual(len(found), 1)  # three messages, one shape
        self.assertEqual(found[0].evidence["count"], 3)


if __name__ == "__main__":
    unittest.main(verbosity=2)


def dmesg(*msgs: str, day: int = 20, start: int = 0) -> list[str]:
    """dmesg -T lines. Local time, which is why these detectors do not attribute to migrations."""
    return [f"[Thu Aug {day} 05:{46 + (start + i) // 60:02d}:{(start + i) % 60:02d} 2026] {m}\n"
            for i, m in enumerate(msgs)]


class KernelPathLoss(unittest.TestCase):
    """The ladder: requeue (absorbed) -> failfast -> failing I/O (application-visible)."""

    def test_requeue_only_is_a_warning_not_a_failure(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            "block nvme0n1: no usable path - requeuing I/O",
            "block nvme0n1: no usable path - requeuing I/O")})
        found = list(build_detector("kernel.path-loss").detect(ev))
        self.assertEqual([f.severity for f in found], [Severity.WARNING])
        self.assertEqual(found[0].evidence["requeues"], 2)

    def test_failing_io_is_critical(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            "block nvme0n1: no usable path - requeuing I/O",
            "nvme nvme10: failfast expired",
            "block nvme0n1: no available path - failing I/O")})
        found = list(build_detector("kernel.path-loss").detect(ev))
        crit = [f for f in found if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 1)
        self.assertEqual(crit[0].evidence["failing_io"], 1)

    def test_failfast_is_reported_separately_as_the_boundary(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg("nvme nvme10: failfast expired")})
        found = list(build_detector("kernel.path-loss").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertIn("fast_io_fail_tmo", found[0].title)

    def test_clean_dmesg_yields_nothing(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg("nvme nvme1: creating 3 I/O queues.")})
        self.assertEqual(list(build_detector("kernel.path-loss").detect(ev)), [])

    def test_skips_without_dmesg(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("kernel.path-loss").detect(FakeEvidence()))


class KernelFilesystemShutdown(unittest.TestCase):
    def test_shutdown_is_critical_and_names_the_devices(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            "XFS (nvme0n1): log I/O error -5",
            "XFS (nvme0n1): Filesystem has been shut down due to log error (0x2).",
            "XFS (nvme1n6): Filesystem has been shut down due to log error (0x2).")})
        found = list(build_detector("kernel.filesystem-shutdown").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertEqual(found[0].evidence["devices"], ["nvme0n1", "nvme1n6"])

    def test_io_errors_without_a_shutdown_are_only_a_warning(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg("XFS (nvme0n1): metadata I/O error")})
        found = list(build_detector("kernel.filesystem-shutdown").detect(ev))
        self.assertEqual([f.severity for f in found], [Severity.WARNING])

    def test_healthy_mount_messages_are_not_findings(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            "XFS (nvme0n1): Ending clean mount",
            "XFS (nvme0n1): Unmounting Filesystem abc")})
        self.assertEqual(list(build_detector("kernel.filesystem-shutdown").detect(ev)), [])


class NvmeForeignCluster(unittest.TestCase):
    LIVE = "d26b8f37-2b45-47c0-9d20-983e6c5ee3fe"
    DEAD = "5fd9ad70-3cd1-4fc8-b8e6-e085081601f6"

    def test_a_dead_cluster_being_retried_is_reported_as_hygiene(self):
        """The sharpest form of 'controllers never disappear': they outlive the cluster.

        Reported, but never as this run's failure — a controller for a destroyed cluster
        cannot affect a migration of the live cluster's subsystems. See NvmeDirtyStart for
        the leak that does invalidate a run.
        """
        ev = FakeEvidence(cluster=self.LIVE, logs={"dmesg-vm03": dmesg(
            f'nvme nvme6: Connect Invalid Data Parameter, subsysnqn "nqn.2023-02.io.simplyblock:{self.DEAD}:lvol:x"',
            f'nvme nvme6: Connect Invalid Data Parameter, subsysnqn "nqn.2023-02.io.simplyblock:{self.DEAD}:lvol:x"',
            f'nvme nvme7: connected to nqn.2023-02.io.simplyblock:{self.LIVE}:lvol:y')})
        found = list(build_detector("nvme.foreign-cluster").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.WARNING)
        self.assertIs(found[0].attribution, Attribution.PRE_EXISTING)
        self.assertFalse(found[0].counts_against_the_run)
        self.assertEqual(found[0].evidence["foreign"], {self.DEAD: 2})
        self.assertEqual(found[0].evidence["live_cluster"], self.LIVE)

    def test_only_the_live_cluster_is_clean(self):
        ev = FakeEvidence(cluster=self.LIVE, logs={"dmesg-vm03": dmesg(
            f'nvme nvme7: connected to nqn.2023-02.io.simplyblock:{self.LIVE}:lvol:y')})
        self.assertEqual(list(build_detector("nvme.foreign-cluster").detect(ev)), [])

    def test_skips_when_the_live_cluster_is_unknown(self):
        """Without it a foreign NQN cannot be told from the live one, so do not guess."""
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            f'nvme nvme6: nqn.2023-02.io.simplyblock:{self.DEAD}:lvol:x')})
        with self.assertRaises(SkipDetector):
            list(build_detector("nvme.foreign-cluster").detect(ev))


class NvmeControllerChurn(unittest.TestCase):
    def test_created_but_never_removed_is_reported(self):
        msgs = [f"nvme nvme{i}: new ctrl: NQN \"nqn.x\"" for i in range(8)]
        msgs.append("nvme nvme0: Removing ctrl: NQN \"nqn.x\"")
        found = list(build_detector("nvme.controller-churn").detect(
            FakeEvidence(logs={"dmesg-vm03": dmesg(*msgs)})))
        churn = [f for f in found if "never removed" in f.title]
        self.assertEqual(len(churn), 1)
        self.assertEqual(churn[0].evidence["net"], 7)

    def test_balanced_churn_is_only_info(self):
        msgs = ["nvme nvme0: new ctrl: NQN \"nqn.x\"", "nvme nvme0: Removing ctrl: NQN \"nqn.x\""]
        found = list(build_detector("nvme.controller-churn").detect(
            FakeEvidence(logs={"dmesg-vm03": dmesg(*msgs)})))
        self.assertTrue(all(f.severity == Severity.INFO for f in found))

    def test_a_controller_retrying_forever_is_reported(self):
        found = list(build_detector("nvme.controller-churn").detect(
            FakeEvidence(logs={"dmesg-vm03": dmesg(
                "nvme nvme9: Failed reconnect attempt 5",
                "nvme nvme9: Failed reconnect attempt 834")})))
        stuck = [f for f in found if "without ever succeeding" in f.title]
        self.assertEqual(len(stuck), 1)
        self.assertEqual(stuck[0].evidence["controllers"], {"nvme9": 834})


class KernelFabricErrors(unittest.TestCase):
    def test_groups_by_kind_and_respects_per_kind_floors(self):
        ev = FakeEvidence(logs={"dmesg-vm03": dmesg(
            "nvme nvme1: starting error recovery",
            "nvme nvme1: Property Set error: 880, offset 0x14",
            "nvme nvme2: rescanning namespaces.")})
        found = list(build_detector("kernel.fabric-errors").detect(ev))
        self.assertEqual(len(found), 1)
        counts = found[0].evidence["counts"]
        # a single rescan is normal and must not be reported; the two errors must be
        self.assertNotIn("namespace rescan", counts)
        self.assertIn("error recovery started", counts)
        self.assertIn("property set failed (controller config)", counts)


class Attribution_(unittest.TestCase):
    """Old, unrelated damage must not be counted against a run.

    dmesg spans hours and a cluster outlives its runs, so evidence routinely contains the
    previous runs' mess. These pin the rule: only what happened inside the window counts.
    """

    RUN_START = datetime(2026, 8, 20, 6, 0, 0, tzinfo=UTC)
    RUN_END = datetime(2026, 8, 20, 7, 0, 0, tzinfo=UTC)

    def ev(self, *msgs: str, hour: int = 6) -> FakeEvidence:
        lines = [f"2026-08-20T{hour:02d}:30:0{i},000000+00:00 {m}\n" for i, m in enumerate(msgs)]
        return FakeEvidence(logs={"dmesg-vm03": lines},
                            window=(self.RUN_START, self.RUN_END))

    def test_damage_inside_the_window_is_critical_and_fails(self):
        ev = self.ev("block nvme0n1: no available path - failing I/O", hour=6)
        found = [f for f in build_detector("kernel.path-loss").detect(ev)
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertIs(found[0].attribution, Attribution.RUN)
        self.assertTrue(found[0].counts_against_the_run)

    def test_the_same_damage_before_the_window_does_not_fail(self):
        ev = self.ev("block nvme0n1: no available path - failing I/O", hour=5)
        found = list(build_detector("kernel.path-loss").detect(ev))
        self.assertTrue(found)
        self.assertTrue(all(f.attribution is Attribution.PRE_EXISTING for f in found))
        self.assertFalse(any(f.severity == Severity.CRITICAL for f in found))
        self.assertFalse(any(f.counts_against_the_run for f in found))

    def test_a_filesystem_killed_before_the_run_is_hygiene_not_failure(self):
        ev = self.ev("XFS (nvme0n1): Filesystem has been shut down due to log error (0x2).",
                     hour=5)
        found = list(build_detector("kernel.filesystem-shutdown").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertIs(found[0].attribution, Attribution.PRE_EXISTING)
        self.assertEqual(found[0].severity, Severity.WARNING)

    def test_a_filesystem_killed_during_the_run_is_critical(self):
        ev = self.ev("XFS (nvme0n1): Filesystem has been shut down due to log error (0x2).",
                     hour=6)
        found = list(build_detector("kernel.filesystem-shutdown").detect(ev))
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertIs(found[0].attribution, Attribution.RUN)

    def test_undated_events_still_count(self):
        """'I cannot date this' must not become 'not our problem'."""
        ev = FakeEvidence(logs={"dmesg-vm03": ["block nvme0n1: no available path - failing I/O\n"]},
                          window=(self.RUN_START, self.RUN_END))
        found = [f for f in build_detector("kernel.path-loss").detect(ev)
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertIs(found[0].attribution, Attribution.UNKNOWN)
        self.assertTrue(found[0].counts_against_the_run)

    def test_dead_cluster_debris_is_hygiene_never_a_verdict(self):
        """It cannot make a different cluster's migration fail, so it must not fail the run."""
        live, dead = "d26b8f37-2b45-47c0-9d20-983e6c5ee3fe", "5fd9ad70-3cd1-4fc8-b8e6-e085081601f6"
        ev = FakeEvidence(cluster=live, window=(self.RUN_START, self.RUN_END), logs={
            "dmesg-vm03": [f'nvme nvme6: subsysnqn "nqn.2023-02.io.simplyblock:{dead}:lvol:x"\n'] * 100})
        found = list(build_detector("nvme.foreign-cluster").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.WARNING)
        self.assertIs(found[0].attribution, Attribution.PRE_EXISTING)


class NvmeDirtyStart(unittest.TestCase):
    """The one pre-existing condition that does forfeit a run."""

    LIVE = "d26b8f37-2b45-47c0-9d20-983e6c5ee3fe"
    DEAD = "5fd9ad70-3cd1-4fc8-b8e6-e085081601f6"

    class WithPre(FakeEvidence):
        """FakeEvidence plus the optional pre-run snapshot nvme.dirty-start asks for."""

        def __init__(self, pre: list[NvmeController], cluster: str = "") -> None:
            super().__init__(cluster=cluster)
            self._pre = pre

        def nvme_controllers_pre(self) -> list[NvmeController]:
            return self._pre

    def ctrl(self, nqn: str, state: str, ns: dict) -> NvmeController:
        return NvmeController(node="vm03", name="nvme12", nqn=nqn, address="10.0.0.1:4420",
                              state=state, namespaces=ns, ctrl_loss_tmo=60)

    def test_live_cluster_debris_at_setup_forfeits_the_run(self):
        ev = self.WithPre([self.ctrl(f"nqn:{self.LIVE}:lvol:x", "live", {})],
                          cluster=self.LIVE)
        found = list(build_detector("nvme.dirty-start").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertIs(found[0].attribution, Attribution.PRE_EXISTING)
        # critical + pre-existing => the run is inconclusive, not failed
        rep = Report()
        rep.add(*found)
        self.assertEqual(rep.verdict, "INCONCLUSIVE")
        self.assertFalse(rep.failed)

    def test_dead_cluster_debris_at_setup_does_not_forfeit(self):
        ev = self.WithPre([self.ctrl(f"nqn:{self.DEAD}:lvol:x", "live", {})],
                          cluster=self.LIVE)
        self.assertEqual(list(build_detector("nvme.dirty-start").detect(ev)), [])

    def test_a_connecting_controller_does_not_forfeit(self):
        ev = self.WithPre([self.ctrl(f"nqn:{self.LIVE}:lvol:x", "connecting", {})],
                          cluster=self.LIVE)
        self.assertEqual(list(build_detector("nvme.dirty-start").detect(ev)), [])

    def test_skips_without_a_pre_snapshot(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("nvme.dirty-start").detect(FakeEvidence(cluster=self.LIVE)))


# ── control-plane and evidence families ─────────────────────────────────────────────

def cevent(sec: int, msg: str, subject: str = "node-a", level: str = "Info") -> ControlEvent:
    return ControlEvent(ts=ts(sec), level=level, kind="STATUS_CHANGE", message=msg,
                        subject=subject)


class ControlNodeFlap(unittest.TestCase):
    """The vela shape: a node marked down and back in seconds because the liveness check
    depended on something other than the node."""

    def test_a_short_flap_is_critical(self):
        ev = FakeEvidence(events=[
            cevent(0, "Storage node status changed from: online to: down"),
            cevent(13, "Storage node status changed from: down to: online")])
        found = list(build_detector("control.node-flap").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertEqual(found[0].evidence["flaps"][0]["seconds"], 13)

    def test_a_node_that_stays_down_is_not_a_flap(self):
        ev = FakeEvidence(events=[
            cevent(0, "Storage node status changed from: online to: down")])
        self.assertEqual(list(build_detector("control.node-flap").detect(ev)), [])

    def test_a_slow_recovery_is_not_a_flap(self):
        ev = FakeEvidence(events=[
            cevent(0, "Storage node status changed from: online to: down"),
            cevent(9000, "Storage node status changed from: down to: online")])
        self.assertEqual(list(build_detector("control.node-flap").detect(ev)), [])

    def test_skips_without_the_event_log(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("control.node-flap").detect(FakeEvidence()))


class ControlVolumeHealth(unittest.TestCase):
    def test_health_that_never_returns_is_critical(self):
        ev = FakeEvidence(events=[
            cevent(0, "LVol health check changed from: True to: False", subject="vol-1"),
            cevent(5, "LVol health check changed from: True to: False", subject="vol-2"),
            cevent(60, "LVol health check changed from: False to: True", subject="vol-2")])
        found = list(build_detector("control.volume-health").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["unhealthy"], ["vol-1"])

    def test_all_recovered_is_clean(self):
        ev = FakeEvidence(events=[
            cevent(0, "LVol health check changed from: True to: False", subject="vol-1"),
            cevent(60, "LVol health check changed from: False to: True", subject="vol-1")])
        self.assertEqual(list(build_detector("control.volume-health").detect(ev)), [])


class EvidenceCoverage(unittest.TestCase):
    """Partial evidence is not the same as clean evidence."""

    START = datetime(2026, 8, 20, 7, 0, 0, tzinfo=UTC)
    END = datetime(2026, 8, 20, 9, 0, 0, tzinfo=UTC)

    def test_a_log_that_misses_the_start_is_reported(self):
        ev = FakeEvidence(window=(self.START, self.END), spans=[
            LogSpan("spdk-4424", self.START + timedelta(minutes=114), self.END, 100),
            LogSpan("operator", self.START, self.END, 100)])
        found = list(build_detector("evidence.log-coverage").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertIn("spdk-4424", found[0].evidence["logs"])
        self.assertNotIn("operator", found[0].evidence["logs"])

    def test_full_coverage_is_clean(self):
        ev = FakeEvidence(window=(self.START, self.END), spans=[
            LogSpan("operator", self.START, self.END, 100)])
        self.assertEqual(list(build_detector("evidence.log-coverage").detect(ev)), [])

    def test_skips_without_a_run_window(self):
        ev = FakeEvidence(spans=[LogSpan("x", self.START, self.END, 1)])
        with self.assertRaises(SkipDetector):
            list(build_detector("evidence.log-coverage").detect(ev))

    def test_a_migration_no_log_covers_is_a_blind_spot(self):
        """The real case: the corrupting migration ended six seconds before a log began."""
        mig_start = self.START + timedelta(minutes=110)
        ev = FakeEvidence(
            window=(self.START, self.END),
            migrations=[Migration(name="mig-19", start=mig_start,
                                  end=mig_start + timedelta(minutes=2))],
            spans=[LogSpan("spdk-4424", mig_start + timedelta(minutes=2, seconds=6),
                           self.END, 100)])
        found = list(build_detector("evidence.blind-spot").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["blind"], {"mig-19": ["spdk-4424"]})


class SecuritySecretExposure(unittest.TestCase):
    def test_finds_a_private_key_without_quoting_it(self):
        secret = "-----BEGIN RSA PRIVATE KEY-----"
        ev = FakeEvidence(logs={"operator": [f"oops {secret} MIIEow\n"]})
        found = list(build_detector("security.secret-exposure").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        blob = json.dumps(found[0].to_dict())
        self.assertNotIn("BEGIN RSA", blob)   # the value must not travel with the finding
        self.assertIn("operator:1", found[0].detail)

    def test_finds_a_dhchap_secret(self):
        ev = FakeEvidence(logs={"operator": [
            "connect --dhchap-secret DHHC-1:00:abcdefghijklmnopqrstuvwxyz012345+/=\n"]})
        found = list(build_detector("security.secret-exposure").detect(ev))
        self.assertEqual([f.subject for f in found], ["nvme dhchap secret"])

    def test_ordinary_logs_are_clean(self):
        ev = FakeEvidence(logs={"operator": ["migration started for volume abc\n"]})
        self.assertEqual(list(build_detector("security.secret-exposure").detect(ev)), [])
