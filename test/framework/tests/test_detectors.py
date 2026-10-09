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
    BlockSample,
    ChurnPod,
    ConntrackSample,
    ControlEvent,
    Fence,
    FenceWrite,
    Finding,
    FioJob,
    IopsSample,
    LogSpan,
    Migration,
    NamespaceReservation,
    NfsSample,
    NvmeController,
    PnfsVolume,
    Registrant,
    Report,
    Restart,
    Severity,
    SkipDetector,
    Versions,
    VolumeOp,
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
        nfs: dict[str, dict[str, int]] | None = None,
        blocks: list[BlockSample] | None = None,
        pnfs: list[PnfsVolume] | None = None,
        restarts: list[Restart] | None = None,
        fence: Fence | None = None,
        churn: list[ChurnPod] | None = None,
        reservations_pre: list[NamespaceReservation] | None = None,
        reservations_post: list[NamespaceReservation] | None = None,
        versions: Versions | None = None,
        timeline: list[NfsSample] | None = None,
        conntrack: list[ConntrackSample] | None = None,
        volume_ops: list[VolumeOp] | None = None,
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
        self._nfs = nfs or {}
        self._blocks = blocks or []
        self._pnfs = pnfs or []
        self._restarts = restarts or []
        self._fence = fence
        self._churn = churn or []
        self._resv_pre = reservations_pre or []
        self._resv_post = reservations_post or []
        self._versions = versions
        self._timeline = timeline or []
        self._conntrack = conntrack or []
        self._volume_ops = volume_ops or []

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
        return sorted(set(self._fio_logs) | {j.pod for j in self._jobs} | set(self._series)
                      | set(self._nfs))

    def nfs_ops(self, pod: str) -> dict[str, int]:
        return dict(self._nfs.get(pod, {}))

    def block_samples(self) -> list[BlockSample]:
        return list(self._blocks)

    def pnfs_volumes(self) -> list[PnfsVolume]:
        return list(self._pnfs)

    def restarts(self) -> list[Restart]:
        return list(self._restarts)

    def fence(self) -> Fence | None:
        return self._fence

    def churn(self) -> list[ChurnPod]:
        return list(self._churn)

    def reservations_pre(self) -> list[NamespaceReservation]:
        return list(self._resv_pre)

    def reservations_post(self) -> list[NamespaceReservation]:
        return list(self._resv_post)

    def versions(self) -> Versions | None:
        return self._versions

    def nfs_timeline(self) -> list[NfsSample]:
        return list(self._timeline)

    def conntrack(self) -> list[ConntrackSample]:
        return list(self._conntrack)

    def volume_ops(self) -> list[VolumeOp]:
        return list(self._volume_ops)

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


class FioThroughputOutlier(unittest.TestCase):
    """A churn pod lives for minutes, most of them spent laying out its file, so its average
    says nothing about the volume and would pull the median down for everyone else."""

    def jobs(self) -> list[FioJob]:
        steady = [FioJob(pod=f"r-fio-{i}-c0", total_iops=1000.0 + i) for i in range(5)]
        return steady + [FioJob(pod="r-fio-churn-3-c0", total_iops=50.0)]

    def test_churn_instances_are_left_out_by_default(self):
        found = list(build_detector("fio.throughput-outlier").detect(FakeEvidence(jobs=self.jobs())))
        self.assertEqual(found, [])

    def test_a_steady_pod_far_below_the_median_still_warns(self):
        jobs = self.jobs() + [FioJob(pod="r-fio-9-c0", total_iops=100.0)]
        found = list(build_detector("fio.throughput-outlier").detect(FakeEvidence(jobs=jobs)))
        self.assertEqual([f.subject for f in found], ["r-fio-9-c0"])


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

    # An error inside a restart the run caused is still the application seeing it, so it
    # stays critical. What changes is where to look: pnfs-1791525621's EIO and EACCES all
    # ended within 35 s of an MDS deletion, and the finding has to say so.
    def test_an_error_during_a_restart_names_the_restart(self):
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(100), ready=ts(120))
        job = FioJob(pod="fio-churn-5", error=5, start=ts(0), runtime_s=135)
        found = list(build_detector("fio.job-error").detect(
            FakeEvidence(jobs=[job], restarts=[mds])))
        self.assertEqual(found[0].severity, Severity.CRITICAL)
        self.assertIn("mds restart", found[0].note)
        self.assertEqual(found[0].evidence["restart"], "mds/mds-0")

    def test_an_error_long_after_a_restart_does_not_name_it(self):
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(100), ready=ts(120))
        job = FioJob(pod="fio-0", error=5, start=ts(0), runtime_s=1000)
        found = list(build_detector("fio.job-error").detect(
            FakeEvidence(jobs=[job], restarts=[mds])))
        self.assertNotIn("restart", found[0].evidence)


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


class SecretExposureArtifacts(unittest.TestCase):
    def test_the_artifacts_are_the_files_a_reader_can_open(self):
        """Regression: 2026-08-26-secret-artifacts-miss-the-extension (PR #445 review).

        The finding listed log *names* ("operator") where the run directory holds
        "operator.txt", so the one field that says where to look did not name a file. Every
        other detector reporting a container log names it with its extension.
        """
        ev = FakeEvidence(logs={
            "operator": ["password=hunter2hunter2"],
            "webappapi": ["nothing to see"]})
        found = list(build_detector("security.secret-exposure").detect(ev))
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].artifacts, ["operator.txt"])


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

    # SPDK prints status 01/82 under the I/O command's name whatever the command was. On a
    # Fabric Connect, which always runs on queue 0, it means the subsystem is gone, not that
    # a write landed on a frozen range (pnfs-1791525621: 28 such lines, no write among them).
    _REFUSED_CONNECT = [
        "[2026-10-09 06:12:01.180571] ctrlr.c:1041:nvmf_ctrlr_cmd_connect: *NOTICE*: Invalid "
        "subsystem...1...target nqn.2023-02.io.simplyblock:c:lvol:194db54b host nqn.h\n",
        "[2026-10-09 06:12:01.180653] nvme_qpair.c: 291:nvme_admin_qpair_print_command_s: "
        "*NOTICE*: FABRIC CONNECT qid:0 cid:49152 SGL DATA BLOCK OFFSET 0x0 len:0x400\n",
        "[2026-10-09 06:12:01.180672] nvme_qpair.c: 547:spdk_nvme_print_completion_s: "
        "*NOTICE*: WRITE TO RO RANGE (01/82) qid:0 cid:49152 cdw0:10100 sqhd:0000 p:0 m:0 "
        "dnr:0\n",
    ]

    def _subjects(self, lines: list[str]) -> list[str]:
        ev = FakeEvidence(logs={"spdk-4420": lines})
        return [f.subject for f in build_detector("logs.pattern").detect(ev)]

    def test_a_refused_connect_is_not_a_write_to_a_readonly_range(self):
        self.assertNotIn("nvme.write-to-readonly", self._subjects(self._REFUSED_CONNECT))

    def test_a_write_on_an_io_queue_is_still_a_write_to_a_readonly_range(self):
        line = ("[2026-10-09 06:12:01.1] nvme_qpair.c: 547:spdk_nvme_print_completion_s: "
                "*NOTICE*: WRITE TO RO RANGE (01/82) qid:3 cid:12 cdw0:0 sqhd:0000 p:0 m:0 "
                "dnr:0\n")
        self.assertIn("nvme.write-to-readonly", self._subjects([line]))

    def test_a_host_connecting_to_a_removed_subsystem_is_reported(self):
        self.assertIn("nvme.connect-to-removed-subsystem", self._subjects(self._REFUSED_CONNECT))


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



def expanded(**kw: object) -> VolumeOp:
    """An expansion of r-shared-0 requested at 100s, on the claim at 110s, and seen by its
    client at 130s, unless kw says otherwise."""
    base: dict[str, object] = {"op": "expand", "claim": "r-shared-0", "requested": ts(100),
                               "timeout_s": 300.0, "target_bytes": 21, "capacity_at": ts(110),
                               "client_pod": "r-pnfs-0", "client_before_b": 20,
                               "client_after_b": 21, "client_seen_at": ts(130)}
    base.update(kw)
    return VolumeOp(**base)  # type: ignore[arg-type]


def snapped(**kw: object) -> VolumeOp:
    """A snapshot of r-shared-0 requested at 100s, ready at 120s, restored and read back at
    200s, and deleted, unless kw says otherwise."""
    base: dict[str, object] = {"op": "snapshot", "claim": "r-shared-0", "requested": ts(100),
                               "timeout_s": 300.0, "snapshot": "r-volops-1",
                               "marker_md5": "aa", "ready_at": ts(120),
                               "restore_claim": "r-volops-1-restore", "restored_at": ts(200),
                               "restore_md5": "aa", "snapshot_deleted": ts(210),
                               "restore_deleted": ts(205)}
    base.update(kw)
    return VolumeOp(**base)  # type: ignore[arg-type]


class PnfsVolumeOps(unittest.TestCase):
    """An expansion or a snapshot of a pNFS volume under load has to complete, reach the
    clients, and leave fio running."""

    VOL = PnfsVolume(claim="r-shared-0", lvol="lv1", shared=True, nodes=["w1"])

    def found(self, ops: list[VolumeOp], pnfs: list[PnfsVolume] | None = None,
              blocks: list[BlockSample] | None = None) -> list[Finding]:
        ev = FakeEvidence(volume_ops=ops, pnfs=pnfs, blocks=blocks)
        return list(build_detector("pnfs.volume-ops").detect(ev))

    def severities(self, ops: list[VolumeOp], pnfs: list[PnfsVolume] | None = None,
                   blocks: list[BlockSample] | None = None) -> list[Severity]:
        return [f.severity for f in self.found(ops, pnfs, blocks) if f.severity != Severity.INFO]

    def test_completed_operations_are_information(self):
        self.assertEqual(self.severities([expanded(), snapped()]), [])
        self.assertTrue(self.found([expanded(), snapped()]))

    def test_an_expansion_the_claim_never_showed_is_critical(self):
        self.assertEqual(self.severities([expanded(capacity_at=None, client_seen_at=None)]),
                         [Severity.CRITICAL])

    def test_an_expansion_no_client_saw_is_critical(self):
        found = [f for f in self.found([expanded(client_seen_at=None)])
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertIn("client", found[0].title)

    def test_a_snapshot_that_never_became_ready_is_critical(self):
        self.assertEqual(self.severities([snapped(ready_at=None, restored_at=None,
                                                  restore_md5="")]), [Severity.CRITICAL])

    def test_a_restore_that_read_other_data_is_critical(self):
        found = [f for f in self.found([snapped(restore_md5="bb")])
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["restore_md5"], "bb")

    def test_a_restore_that_never_read_back_is_critical(self):
        self.assertEqual(self.severities([snapped(restored_at=None, restore_md5="")]),
                         [Severity.CRITICAL])

    def test_an_operation_that_failed_is_critical(self):
        self.assertEqual(self.severities([expanded(error="patch refused", capacity_at=None,
                                                   client_seen_at=None)]),
                         [Severity.CRITICAL])

    def test_a_snapshot_left_behind_is_a_warning(self):
        self.assertEqual(self.severities([snapped(snapshot_deleted=None)]), [Severity.WARNING])

    # Review on #704: a failed deletion also set the error, and the warning was given only
    # without one, so no real leftover was ever reported.
    def test_a_snapshot_whose_deletion_failed_is_still_left_behind(self):
        op = snapped(snapshot_deleted=None, error="deleting volumesnapshot r-volops-1: timed out")
        self.assertIn(Severity.WARNING, self.severities([op]))

    def test_a_snapshot_never_created_is_not_left_behind(self):
        op = snapped(snapshot="", ready_at=None, restored_at=None, restore_md5="",
                     restore_claim="", snapshot_deleted=None, error="creating the snapshot refused")
        self.assertNotIn(Severity.WARNING, self.severities([op]))

    # Review on #704: an expansion with no client to observe it passed on the claim's half.
    def test_an_expansion_no_client_observed_is_critical(self):
        found = [f for f in self.found([expanded(client_pod="", client_before_b=0,
                                                 client_after_b=0, client_seen_at=None)])
                 if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertIn("client", found[0].title)

    def test_an_operation_not_attempted_is_information(self):
        self.assertEqual(self.severities([snapped(skipped="no VolumeSnapshotClass",
                                                  ready_at=None, restored_at=None)]), [])

    def test_fio_stalling_across_an_operation_is_a_warning(self):
        # Writes stop after 100s and the counter stands still through 190s: a 90s stall,
        # measured as pnfs.device-io measures it, across the 100-130s expansion.
        blocks = [blk("w1", t, 1, w) for t, w in
                  ((60, 1), (80, 2), (100, 3), (130, 3), (160, 3), (190, 3), (220, 4))]
        found = [f for f in self.found([expanded()], pnfs=[self.VOL], blocks=blocks)
                 if f.severity == Severity.WARNING]
        self.assertEqual(len(found), 1)
        self.assertIn("stall", found[0].title)

    def test_a_stall_outside_the_operation_is_not_its_warning(self):
        blocks = [blk("w1", t, 1, w) for t, w in
                  ((300, 1), (320, 1), (400, 1), (420, 2))]
        self.assertEqual(self.severities([expanded()], pnfs=[self.VOL], blocks=blocks), [])

    def test_a_run_without_volume_operations_is_skipped(self):
        with self.assertRaises(SkipDetector):
            self.found([])

if __name__ == "__main__":
    unittest.main(verbosity=2)


def dmesg(*msgs: str, day: int = 20, start: int = 0) -> list[str]:
    """dmesg -T lines. Local time, which is why these detectors do not attribute to migrations."""
    return [f"[Thu Aug {day} 05:{46 + (start + i) // 60:02d}:{(start + i) % 60:02d} 2026] {m}\n"
            for i, m in enumerate(msgs)]


def iso_dmesg(*lines: tuple[str, str]) -> list[str]:
    """dmesg --time-format=iso lines, each given as (HH:MM:SS on Aug 20, message)."""
    return [f"2026-08-20T{t},000000+00:00 {m}\n" for t, m in lines]


class KernelClockOffset(unittest.TestCase):
    """The kernel's timestamps drift from wall time, and the collector's marker corrects them.

    dmesg renders a line's time from the boot time and the kernel's own clock, which is not
    NTP-disciplined: lab-talos nodes up for 43 to 129 days measured 61s to 182s behind. Placed
    against the run window uncorrected, the first minutes of a run read as before it, and
    what the run broke is reported as inherited.
    """

    RUN = (datetime(2026, 8, 20, 5, 50, 0, tzinfo=UTC), datetime(2026, 8, 20, 6, 0, 0, tzinfo=UTC))
    FAILING = "block nvme0n1: no available path - failing I/O"
    #: Written at wall 05:55:00, rendered at 05:53:00: the kernel is two minutes behind.
    MARKER = ("05:53:00", "sbtest-clock-probe wall=2026-08-20T05:55:00+00:00")

    def failing(self, log: list[str]) -> Finding:
        ev = FakeEvidence(window=self.RUN, logs={"dmesg-vm03": log})
        found = [f for f in build_detector("kernel.path-loss").detect(ev)
                 if f.evidence.get("failing_io")]
        self.assertEqual(len(found), 1)
        return found[0]

    def test_an_event_rendered_before_the_run_but_inside_it_by_wall_time_is_the_runs(self):
        found = self.failing(iso_dmesg(("05:48:30", self.FAILING), self.MARKER))
        self.assertIs(found.attribution, Attribution.RUN)

    def test_an_event_before_the_run_by_wall_time_stays_inherited(self):
        found = self.failing(iso_dmesg(("05:45:00", self.FAILING), self.MARKER))
        self.assertIs(found.attribution, Attribution.PRE_EXISTING)

    def test_without_a_marker_the_rendered_time_is_taken_as_it_is(self):
        found = self.failing(iso_dmesg(("05:48:30", self.FAILING)))
        self.assertIs(found.attribution, Attribution.PRE_EXISTING)


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

    # A pod the run restarted has nothing to say before its replacement existed, and the
    # follow of the victim begins at its deletion (pnfs-1791525621: mds-runner and
    # restart-2-mds-... were reported as missing the first 8-9 minutes).
    def test_a_log_that_begins_with_a_restart_the_run_caused_is_not_short(self):
        deleted = self.START + timedelta(minutes=60)
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=deleted,
                      ready=deleted + timedelta(seconds=20))
        ev = FakeEvidence(window=(self.START, self.END), restarts=[mds], spans=[
            LogSpan("mds-runner", deleted + timedelta(seconds=3), self.END, 100),
            LogSpan("restart-1-mds-mds-0", deleted, deleted + timedelta(seconds=5), 10),
            LogSpan("spdk-4424", self.START + timedelta(minutes=90), self.END, 100)])
        found = list(build_detector("evidence.log-coverage").detect(ev))
        self.assertEqual(sorted(found[0].evidence["logs"]), ["spdk-4424"])

    def test_skips_without_a_run_window(self):
        ev = FakeEvidence(spans=[LogSpan("x", self.START, self.END, 1)])
        with self.assertRaises(SkipDetector):
            list(build_detector("evidence.log-coverage").detect(ev))

    def test_a_restart_excuses_only_the_restarted_pods_logs(self):
        """An SPDK log that rotated through the first half of the run is still short when its
        first surviving line falls inside an MDS restart (review on #698)."""
        deleted = self.START + timedelta(minutes=60)
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=deleted,
                      ready=deleted + timedelta(seconds=20))
        node = Restart(target="csi-node", pod="csi-node-abc", node="w1",
                       deleted=deleted, ready=deleted + timedelta(seconds=20))
        first = deleted + timedelta(seconds=5)
        ev = FakeEvidence(window=(self.START, self.END), restarts=[mds, node], spans=[
            LogSpan("spdk-4424", first, self.END, 100),
            LogSpan("csi-node-w2", first, self.END, 100),
            LogSpan("csi-node-w1", first, self.END, 100),
            LogSpan("mds-runner", first, self.END, 100)])
        found = list(build_detector("evidence.log-coverage").detect(ev))
        self.assertEqual(sorted(found[0].evidence["logs"]), ["csi-node-w2", "spdk-4424"])

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


class PnfsLayout(unittest.TestCase):
    """pNFS that silently became plain NFS passes every fio check: the data is right, it
    just went through the metadata server. The layout counters are the only witness."""

    def _found(self, nfs: dict[str, dict[str, int]]) -> list:
        return list(build_detector("pnfs.layout").detect(FakeEvidence(nfs=nfs)))

    def test_a_mount_that_fetched_no_layout_fails_the_run(self):
        found = self._found({"r-fio-0-c0": {"LAYOUTGET": 0, "WRITE": 4096, "READ": 900}})
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "r-fio-0-c0")])

    def test_data_through_the_server_beside_layouts_is_a_warning(self):
        found = self._found({"r-fio-0-c0": {"LAYOUTGET": 4, "WRITE": 12, "READ": 0}})
        self.assertEqual([f.severity for f in found], [Severity.WARNING])
        self.assertEqual(found[0].evidence["server_io_ops"], 12)

    def test_layouts_and_no_server_io_is_clean(self):
        self.assertEqual(self._found({"r-fio-0-c0": {"LAYOUTGET": 2, "WRITE": 0, "READ": 0}}),
                         [])

    def test_a_run_without_nfs_mounts_is_skipped_not_clean(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("pnfs.layout").detect(FakeEvidence()))


def blk(node: str, off: int, rd: int, wr: int, uuid: str = "lv1") -> BlockSample:
    return BlockSample(ts=ts(off), node=node, device="nvme0n1", uuid=uuid,
                       read_ios=rd, read_sectors=rd * 8, write_ios=wr, write_sectors=wr * 8)


class PnfsDeviceIO(unittest.TestCase):
    """With pNFS doing its job, the client writes to its own NVMe-oF namespace. A client
    whose namespace stays flat while fio runs sent its data through the metadata server."""

    VOL = PnfsVolume(claim="c1", lvol="lv1", shared=True, nodes=["w1", "w2"])

    def _found(self, blocks: list[BlockSample], jobs: list[FioJob] | None = None,
               vol: PnfsVolume | None = None, **opts: object) -> list:
        ev = FakeEvidence(blocks=blocks, pnfs=[vol or self.VOL], jobs=jobs)
        return list(build_detector("pnfs.device-io", **opts).detect(ev))

    def test_growing_reads_and_writes_on_every_client_node_is_clean(self):
        blocks = [blk(n, off, off * 10, off * 5) for n in ("w1", "w2") for off in (0, 10, 20)]
        self.assertEqual(self._found(blocks), [])

    def test_a_client_whose_namespace_stayed_flat_fails(self):
        blocks = ([blk("w1", off, off * 10, off * 5) for off in (0, 10, 20)]
                  + [blk("w2", off, 7, 3) for off in (0, 10, 20)])
        found = self._found(blocks)
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "c1@w2")])

    def test_a_client_without_the_namespace_attached_fails(self):
        found = self._found([blk("w1", off, off * 10, off * 5) for off in (0, 10, 20)])
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "c1@w2")])
        self.assertIn("not attached", found[0].title)

    def test_writes_that_stalled_mid_run_are_a_warning(self):
        offs = (0, 10, 20, 100, 110)
        blocks = ([blk("w1", o, o * 10, o * 5) for o in offs]
                  + [blk("w2", o, o * 10, 50 if o <= 100 else o * 5) for o in offs])
        found = self._found(blocks, max_stall_s=60)
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.WARNING, "c1@w2")])

    def test_writes_that_stopped_and_never_resumed_fail(self):
        """The shape an MDS restart left: the client's writes stopped mid-run and its I/O
        went through the server until fio ended. A pause that ends is a warning, one that
        lasts to the end of the run is the direct path lost."""
        offs = (0, 10, 20, 100, 200)
        blocks = ([blk("w1", o, o * 10, o * 5) for o in offs]
                  + [blk("w2", o, min(o, 20) * 10, min(o, 20) * 5) for o in offs])
        found = self._found(blocks, max_stall_s=60)
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "c1@w2")])
        self.assertIn("never resumed", found[0].title)

    # w2 stops writing at 20s and resumes at 110s: a 90s pause.
    PAUSE = (0, 10, 20, 100, 110, 200)

    def _paused(self) -> list[BlockSample]:
        return ([blk("w1", o, o * 10, o * 5) for o in self.PAUSE]
                + [blk("w2", o, o * 10, 100 if 20 <= o <= 100 else o * 5) for o in self.PAUSE])

    def test_a_pause_across_an_mds_restart_within_budget_is_expected(self):
        """Clients wait out the guest's boot and grace period, so writes pause. That is the
        restart working, not a stall."""
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(15), ready=ts(40))
        ev = FakeEvidence(blocks=self._paused(), pnfs=[self.VOL], restarts=[mds])
        found = list(build_detector("pnfs.device-io", max_stall_s=60).detect(ev))
        self.assertEqual([f.severity for f in found if f.severity != Severity.INFO], [])

    def test_a_pause_longer_than_the_restart_budget_still_warns(self):
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(15), ready=ts(40))
        ev = FakeEvidence(blocks=self._paused(), pnfs=[self.VOL], restarts=[mds])
        found = list(build_detector("pnfs.device-io", max_stall_s=60,
                                    restart_pause_s=60).detect(ev))
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.WARNING, "c1@w2")])

    def test_a_pause_across_a_csi_node_restart_still_warns(self):
        """The node plugin is not on the data path, so restarting it must not pause I/O."""
        node = Restart(target="csi-node", pod="csi-node-x", node="w2", deleted=ts(15),
                       ready=ts(40))
        ev = FakeEvidence(blocks=self._paused(), pnfs=[self.VOL], restarts=[node])
        found = list(build_detector("pnfs.device-io", max_stall_s=60).detect(ev))
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.WARNING, "c1@w2")])

    def test_writes_that_never_resumed_after_a_restart_still_fail(self):
        mds = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(15), ready=ts(40))
        blocks = ([blk("w1", o, o * 10, o * 5) for o in self.PAUSE]
                  + [blk("w2", o, min(o, 20) * 10, min(o, 20) * 5) for o in self.PAUSE])
        ev = FakeEvidence(blocks=blocks, pnfs=[self.VOL], restarts=[mds])
        found = list(build_detector("pnfs.device-io", max_stall_s=60).detect(ev))
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "c1@w2")])

    def test_reads_are_not_required_when_the_run_did_not_read(self):
        blocks = [blk(n, off, 0, off * 5) for n in ("w1", "w2") for off in (0, 10, 20)]
        self.assertEqual(self._found(blocks, require_reads=False), [])

    def test_a_run_without_samples_is_skipped_not_clean(self):
        with self.assertRaises(SkipDetector):
            self._found([])

    def test_a_quiet_device_after_its_instances_finished_is_not_a_stall(self):
        """Each fio counts its runtime from its own start, so on a node whose instances began
        early the device goes quiet before the run ends. That is fio being done."""
        vol = PnfsVolume(claim="c1", lvol="lv1", shared=True, nodes=["w1"],
                         instances={"r-fio-0-c0": "w1"})
        offs = (0, 10, 20, 30, 110)
        blocks = [blk("w1", o, o * 10 + 1, min(o, 30) * 5 + 1) for o in offs]
        jobs = [FioJob(pod="r-fio-0-c0", start=ts(0), runtime_s=30)]
        self.assertEqual(self._found(blocks, jobs=jobs, vol=vol, max_stall_s=60), [])

    def test_a_stall_while_its_instances_ran_is_still_one(self):
        vol = PnfsVolume(claim="c1", lvol="lv1", shared=True, nodes=["w1"],
                         instances={"r-fio-0-c0": "w1"})
        offs = (0, 10, 100, 110, 200)
        blocks = [blk("w1", o, o * 10 + 1, (5 if o <= 100 else o) * 5) for o in offs]
        jobs = [FioJob(pod="r-fio-0-c0", start=ts(0), runtime_s=200)]
        found = self._found(blocks, jobs=jobs, vol=vol, max_stall_s=60)
        self.assertEqual([f.severity for f in found], [Severity.WARNING])


class ChaosRecovery(unittest.TestCase):
    """Every restart the run made must have come back: a pod that never returned is a
    cluster left broken by the test, and nothing after it is evidence of anything."""

    def test_a_restart_whose_replacement_never_turned_ready_is_critical(self):
        r = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(10), ready=None)
        found = list(build_detector("chaos.recovery").detect(FakeEvidence(restarts=[r])))
        self.assertEqual([(f.severity, f.subject) for f in found],
                         [(Severity.CRITICAL, "mds/mds-0")])

    def test_restarts_that_recovered_are_reported_as_information(self):
        restarts = [Restart(target="mds", pod="mds-0", node="w3", deleted=ts(10),
                            ready=ts(40)),
                    Restart(target="csi-node", pod="csi-node-a", node="w1", deleted=ts(90),
                            ready=ts(100))]
        found = list(build_detector("chaos.recovery").detect(FakeEvidence(restarts=restarts)))
        self.assertEqual({f.severity for f in found}, {Severity.INFO})

    def test_a_run_without_restarts_is_skipped(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("chaos.recovery").detect(FakeEvidence()))


def churned(n: int, own: bool = True, **kw: object) -> ChurnPod:
    """A churn pod that came, did I/O, and left cleanly, unless kw says otherwise."""
    base: dict[str, object] = {
        "pod": f"r-churn-{n}", "claim": f"r-churn-{n}" if own else "r-pnfs-shared-0",
        "own_volume": own, "created": ts(n * 10), "node": "w1", "io_started": ts(n * 10 + 5),
        "finished": ts(n * 10 + 60), "deleted": ts(n * 10 + 65), "rc": 0}
    if own:
        base.update({"pvc_deleted": ts(n * 10 + 66), "pv": f"pvc-{n}", "pv_gone": True,
                     "export_gone": True, "gone_s": 20.0})
    base.update(kw)
    return ChurnPod(**base)  # type: ignore[arg-type]


class PnfsChurn(unittest.TestCase):
    """Pods join pNFS volumes, run fio, and leave, some with a volume of their own. Each one
    has to reach I/O, and an own volume has to be gone, export and all, once it is deleted."""

    def found(self, pods: list[ChurnPod], **opts: object) -> list:
        return list(build_detector("pnfs.churn", **opts).detect(FakeEvidence(churn=pods)))

    def test_a_clean_flow_is_reported_as_information_only(self):
        found = self.found([churned(1), churned(2, own=False), churned(3)])
        self.assertTrue(found)
        self.assertEqual({f.severity for f in found}, {Severity.INFO})

    def test_a_pod_that_never_reached_io_is_critical(self):
        found = self.found([churned(1, io_started=None, error="timed run not reached")])
        self.assertEqual([(f.severity, f.subject) for f in found if f.severity != Severity.INFO],
                         [(Severity.CRITICAL, "r-churn-1")])

    def test_an_own_volume_whose_export_outlived_it_is_critical(self):
        found = self.found([churned(1, export_gone=False)])
        crit = [f for f in found if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 1)
        self.assertIn("NFSExport", crit[0].title)

    def test_an_own_volume_whose_pv_outlived_it_is_critical(self):
        found = self.found([churned(1, pv_gone=False)])
        crit = [f for f in found if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 1)
        self.assertIn("PersistentVolume", crit[0].title)

    def test_a_slow_cleanup_is_a_warning(self):
        found = self.found([churned(1, gone_s=400.0)], delete_budget_s=120)
        self.assertEqual([f.severity for f in found if f.severity != Severity.INFO],
                         [Severity.WARNING])

    def test_a_reused_volume_is_not_judged_on_its_cleanup(self):
        """A volume the run's long-lived pods share stays when a churn pod leaves it."""
        found = self.found([churned(1, own=False)])
        self.assertEqual({f.severity for f in found}, {Severity.INFO})

    def test_a_run_without_churn_is_skipped(self):
        with self.assertRaises(SkipDetector):
            self.found([])


MDS_KEY = 0x0100000000000000
OLD_BOOT = 0x6AC79C58      # 2026-10-08T13:36:24Z, the previous metadata server
NEW_BOOT = 0x6AC79CF4      # 2026-10-08T13:39:00Z, after its restart


def resv(uuid: str, *clients: tuple[str, int], holder: bool = True,
         node: str = "w1") -> NamespaceReservation:
    """A namespace's reservation state: the MDS's key (the holder, when `holder`) and one
    registration per (hostid, boot) client."""
    regs = [Registrant(hostid="mds-host", rkey=MDS_KEY, holder=holder)]
    regs += [Registrant(hostid=h, rkey=(boot << 32) | (i + 1), holder=False)
             for i, (h, boot) in enumerate(clients)]
    return NamespaceReservation(node=node, device="nvme3n1", uuid=uuid,
                                rtype=4 if holder else 0, generation=4,
                                registrants=tuple(regs))


class StaleReservations(unittest.TestCase):
    """A client registers the key nfsd gives it, and the key carries nfsd's boot time. A
    registration from an earlier boot outlives that server on NVMe and blocks the client's
    new key, and the client's I/O then goes through the metadata server without a word."""

    VOL = PnfsVolume(claim="c1", lvol="lv1", shared=True, nodes=["w1", "w2"])

    def found(self, **kw: object) -> list[Finding]:
        kw.setdefault("pnfs", [self.VOL])
        ev = FakeEvidence(**kw)  # type: ignore[arg-type]
        return list(build_detector("nvme.stale-reservations").detect(ev))

    def test_a_key_from_an_earlier_boot_beside_current_ones_is_critical(self):
        post = [resv("lv1", ("host-a", NEW_BOOT), ("host-b", OLD_BOOT))]
        crit = [f for f in self.found(reservations_post=post) if f.severity == Severity.CRITICAL]
        self.assertEqual([f.subject for f in crit], ["lv1"])
        self.assertEqual(crit[0].evidence["stale"], ["host-b"])
        self.assertIs(crit[0].attribution, Attribution.RUN)

    def test_keys_older_than_a_recorded_mds_restart_are_stale_even_when_all_are_old(self):
        """The shape seen on lab-talos: after the restart no client could register its new
        key, so every key on the namespace was from the previous boot."""
        post = [resv("lv1", ("host-a", OLD_BOOT), ("host-b", OLD_BOOT))]
        mds = Restart(target="mds", pod="mds-0", node="w3",
                      deleted=datetime(2026, 10, 8, 13, 38, 50, tzinfo=UTC),
                      ready=datetime(2026, 10, 8, 13, 39, 5, tzinfo=UTC))
        crit = [f for f in self.found(reservations_post=post, restarts=[mds])
                if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 1)
        self.assertEqual(sorted(crit[0].evidence["stale"]), ["host-a", "host-b"])

    def test_current_keys_under_the_mds_reservation_are_clean(self):
        post = [resv("lv1", ("host-a", NEW_BOOT), ("host-b", NEW_BOOT))]
        self.assertEqual({f.severity for f in self.found(reservations_post=post)},
                         {Severity.INFO})

    def test_the_mds_key_alone_is_never_stale(self):
        post = [resv("lv1")]
        self.assertEqual({f.severity for f in self.found(reservations_post=post)},
                         {Severity.INFO})

    def test_clients_registered_with_no_holder_is_a_warning(self):
        post = [resv("lv1", ("host-a", NEW_BOOT), holder=False)]
        self.assertEqual([f.severity for f in self.found(reservations_post=post)
                          if f.severity != Severity.INFO], [Severity.WARNING])

    def test_a_key_already_stale_before_the_run_is_pre_existing(self):
        pre = [resv("lv1", ("host-a", NEW_BOOT), ("host-b", OLD_BOOT))]
        post = [resv("lv1", ("host-a", NEW_BOOT), ("host-b", OLD_BOOT))]
        crit = [f for f in self.found(reservations_pre=pre, reservations_post=post)
                if f.severity != Severity.INFO]
        self.assertEqual(len(crit), 1)
        self.assertIs(crit[0].attribution, Attribution.PRE_EXISTING)

    def test_a_namespace_seen_from_several_nodes_is_judged_once(self):
        post = [resv("lv1", ("host-a", NEW_BOOT), ("host-b", OLD_BOOT), node=n)
                for n in ("w1", "w2")]
        crit = [f for f in self.found(reservations_post=post) if f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 1)

    def test_namespaces_of_other_volumes_are_not_judged(self):
        post = [resv("not-pnfs", ("host-a", NEW_BOOT), ("host-b", OLD_BOOT))]
        self.assertEqual(self.found(reservations_post=post), [])

    def test_a_run_without_a_post_snapshot_is_skipped(self):
        with self.assertRaises(SkipDetector):
            self.found()


class EvidenceVersions(unittest.TestCase):
    """A result is only comparable with another when both say what was deployed."""

    def _versions(self) -> Versions:
        from sbtest.core import DeployedImage, NodeVersion
        def img(pod: str, container: str, image: str, digest: str) -> DeployedImage:
            return DeployedImage(namespace="simplyblock", pod=pod, container=container,
                                 image=image, image_id=f"{image.split(':')[0]}@sha256:{digest}")
        return Versions(
            server="v1.34.1",
            images=(
                img("simplyblock-operator-6d9f", "manager", "repo/operator:main", "a" * 64),
                img("simplyblock-csi-node-x1", "csi-node", "repo/spdkcsi:feat", "b" * 64),
                img("simplyblock-csi-node-x2", "csi-node", "repo/spdkcsi:feat", "b" * 64),
                img("simplyblock-pnfs-mds-06075ebb-0", "mds-runner", "repo/spdkcsi:pnfs-mds",
                    "c" * 64),
                img("snode-spdk-pod-4420-06075e", "spdk-container", "repo/spdk:main", "d" * 64),
            ),
            nodes=(NodeVersion(node="w1", kernel="6.18.5-talos", os_image="Talos (v1.12.7)",
                               runtime="containerd://2.1", kubelet="v1.34.1"),
                   NodeVersion(node="w2", kernel="6.18.5-talos", os_image="Talos (v1.12.7)",
                               runtime="containerd://2.1", kubelet="v1.34.1")))

    def test_one_finding_names_the_images_and_kernels_that_were_deployed(self):
        found = list(build_detector("evidence.versions").detect(
            FakeEvidence(versions=self._versions())))
        self.assertEqual([f.severity for f in found], [Severity.INFO])
        ev = found[0].evidence
        self.assertEqual(ev["operator"], ["repo/operator:main@sha256:aaaaaaaaaaaa"])
        self.assertEqual(ev["csi"], ["repo/spdkcsi:feat@sha256:bbbbbbbbbbbb"])
        self.assertEqual(ev["mds"], ["repo/spdkcsi:pnfs-mds@sha256:cccccccccccc"])
        self.assertEqual(ev["spdk"], ["repo/spdk:main@sha256:dddddddddddd"])
        self.assertEqual(ev["kernels"], {"6.18.5-talos": 2})
        self.assertEqual(ev["server"], "v1.34.1")

    def test_a_run_without_versions_is_skipped(self):
        with self.assertRaises(SkipDetector):
            list(build_detector("evidence.versions").detect(FakeEvidence()))


def nfs(instance: str, sec: int, read: int, write: int, layouts: int = 4) -> NfsSample:
    return NfsSample(ts=ts(sec), instance=instance, pod="p0", container="c0",
                     layoutget=layouts, read=read, write=write)


class PnfsLayoutTimeline(unittest.TestCase):
    """End totals say that data went through the metadata server. The timeline says when,
    which is what ties it to a restart, a recall, or nothing at all."""

    def test_the_window_of_server_io_is_reported(self):
        offs = (0, 50, 100, 110, 120, 160, 200)
        io = (0, 0, 0, 10, 20, 42, 42)
        timeline = [nfs("r-fio-0-c0", o, v, 0) for o, v in zip(offs, io, strict=True)]
        ev = FakeEvidence(nfs={"r-fio-0-c0": {"LAYOUTGET": 4, "READ": 42, "WRITE": 0}},
                          timeline=timeline)
        found = list(build_detector("pnfs.layout").detect(ev))
        self.assertEqual([f.severity for f in found], [Severity.WARNING])
        self.assertEqual(found[0].evidence["server_io_from"], ts(100).isoformat())
        self.assertEqual(found[0].evidence["server_io_until"], ts(160).isoformat())
        self.assertIn("from 22:01:40 to 22:02:40", found[0].detail)

    def test_without_a_timeline_the_totals_are_reported_as_before(self):
        ev = FakeEvidence(nfs={"r-fio-0-c0": {"LAYOUTGET": 4, "READ": 42, "WRITE": 0}})
        found = list(build_detector("pnfs.layout").detect(ev))
        self.assertNotIn("server_io_from", found[0].evidence)


class PnfsRecovery(unittest.TestCase):
    """How long after a metadata server restart each client was writing to its own
    namespace again: the number a restart costs, per client."""

    VOL = PnfsVolume(claim="c1", lvol="lv1", shared=True, nodes=["w1", "w2"])
    MDS = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(15), ready=ts(40))
    OFFS = (0, 10, 20, 100, 110, 200)

    def _blocks(self, w2_resumes: bool = True) -> list[BlockSample]:
        def w2(o: int) -> int:
            if 20 <= o <= 100 or (not w2_resumes and o > 100):
                return 100
            return o * 5
        return ([blk("w1", o, o * 10, o * 5) for o in self.OFFS]
                + [blk("w2", o, o * 10, w2(o)) for o in self.OFFS])

    def _found(self, blocks: list[BlockSample], **opts: object) -> dict[str, Finding]:
        ev = FakeEvidence(blocks=blocks, pnfs=[self.VOL], restarts=[self.MDS])
        return {f.subject: f for f in build_detector("pnfs.recovery", **opts).detect(ev)}

    def test_a_client_back_within_budget_is_information(self):
        found = self._found(self._blocks())
        self.assertEqual(found["c1@w2"].severity, Severity.INFO)
        self.assertEqual(found["c1@w2"].evidence["direct_after_s"], 95)

    def test_a_client_whose_writes_did_not_pause_is_information(self):
        found = self._found(self._blocks())
        self.assertEqual(found["c1@w1"].severity, Severity.INFO)
        self.assertEqual(found["c1@w1"].evidence["direct_after_s"], 0)

    def test_a_client_back_after_the_budget_warns(self):
        found = self._found(self._blocks(), budget_s=60)
        self.assertEqual(found["c1@w2"].severity, Severity.WARNING)

    def test_a_client_that_never_came_back_warns(self):
        found = self._found(self._blocks(w2_resumes=False))
        self.assertEqual(found["c1@w2"].severity, Severity.WARNING)
        self.assertIn("not back", found["c1@w2"].title)

    def test_a_run_without_an_mds_restart_is_skipped(self):
        ev = FakeEvidence(blocks=self._blocks(), pnfs=[self.VOL])
        with self.assertRaises(SkipDetector):
            list(build_detector("pnfs.recovery").detect(ev))


def fenced(writes: list[tuple[int, bool]], **kw: object) -> Fence:
    """A fence run partitioned at 100s, the truncate issued at 120s and returned at 200s,
    healed at 300s, unless kw says otherwise. writes are (second, ok)."""
    base: dict[str, object] = {
        "victim_node": "w1", "recaller_node": "w2", "claim": "r-pnfs-shared-0",
        "victim_pod": "r-fence-writer", "recaller_pod": "r-fence-recaller",
        "file": "/data/r-fence.probe",
        "rules": ("OUTPUT -d 10.0.0.9 -p tcp --dport 2049 -j DROP",),
        "partitioned": ts(100), "healed": ts(300), "truncate_issued": ts(120),
        "truncate_returned": ts(200), "truncate_timeout_s": 240.0,
        "writes": tuple(FenceWrite(ts=ts(t), ok=ok) for t, ok in writes)}
    base.update(kw)
    return Fence(**base)  # type: ignore[arg-type]


class PnfsFence(unittest.TestCase):
    """A client cut off from the metadata server keeps its NVMe-oF paths, so once nfsd has
    fenced it only the reservation stops its writes. A write that lands after the fence
    means two writers on one filesystem."""

    def found(self, fence: Fence | None) -> list[Finding]:
        return list(build_detector("pnfs.fence").detect(FakeEvidence(fence=fence)))

    def severities(self, fence: Fence) -> list[Severity]:
        return [f.severity for f in self.found(fence) if f.severity != Severity.INFO]

    def test_writes_that_stop_once_fenced_are_information(self):
        writes = [(50, True), (110, True), (150, False), (250, False), (310, True)]
        self.assertEqual(self.severities(fenced(writes)), [])
        self.assertTrue(self.found(fenced(writes)))

    def test_a_write_that_lands_after_the_fence_is_critical(self):
        writes = [(50, True), (150, False), (250, True), (260, True), (310, True)]
        found = [f for f in self.found(fenced(writes)) if f.severity == Severity.CRITICAL]
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].evidence["writes_after_fence"], 2)

    def test_a_write_after_the_heal_is_not_split_brain(self):
        writes = [(50, True), (150, False), (300, True), (310, True)]
        self.assertEqual(self.severities(fenced(writes)), [])

    def test_a_truncate_that_never_returned_is_critical(self):
        writes = [(50, True), (150, False)]
        self.assertEqual(self.severities(fenced(writes, truncate_returned=None)),
                         [Severity.CRITICAL])

    def test_a_failed_truncate_proves_no_fence(self):
        """An exit status other than timeout's still records when the truncate returned,
        but nothing was recalled, so later writes are not split brain (review on #698)."""
        writes = [(50, True), (150, False), (250, True), (260, True)]
        found = self.found(fenced(writes, truncate_error="exit 1: Permission denied"))
        self.assertNotIn(Severity.CRITICAL, [f.severity for f in found])
        self.assertTrue(any("truncate failed" in f.title for f in found))

    def test_a_victim_whose_writes_never_failed_proved_nothing(self):
        """Its writes went through the metadata server, or the partition did not take, so
        no layout was at stake."""
        writes = [(50, True), (110, True), (150, True), (190, True)]
        self.assertEqual(self.severities(fenced(writes, healed=ts(200))), [Severity.WARNING])

    def test_a_scenario_that_could_not_run_is_a_warning(self):
        self.assertEqual(self.severities(fenced([], partitioned=None, truncate_issued=None,
                                                truncate_returned=None, healed=None,
                                                error="no idle node to fence")),
                         [Severity.WARNING])

    def test_a_run_without_the_scenario_is_skipped(self):
        with self.assertRaises(SkipDetector):
            self.found(None)


class ConntrackPinned(unittest.TestCase):
    """A client flow to the export still translated to the deleted MDS pod's address after
    the replacement is Ready: the reconnect reused its source port and so its conntrack
    entry (pnfs-1791525621, finding 2)."""

    OLD, NEW = "10.244.3.118", "10.244.3.123"
    MDS = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(100), ready=ts(130),
                  ip=OLD, replacement_ip=NEW)

    @staticmethod
    def flow(sec: int, reply: str, node: str = "w1", sport: int = 835,
             state: str = "ESTABLISHED") -> ConntrackSample:
        return ConntrackSample(ts=ts(sec), node=node, state=state, orig_src="10.10.10.11",
                               orig_sport=sport, orig_dst="10.108.41.72", orig_dport=2049,
                               reply_src=reply)

    def found(self, samples: list[ConntrackSample],
              restarts: list[Restart] | None = None) -> list[Finding]:
        ev = FakeEvidence(conntrack=samples,
                          restarts=[self.MDS] if restarts is None else restarts)
        return list(build_detector("pnfs.conntrack-pinned").detect(ev))

    def test_a_flow_still_on_the_old_pod_after_ready_is_pinned(self):
        found = self.found([self.flow(90, self.OLD), self.flow(160, self.OLD),
                            self.flow(220, self.OLD)])
        self.assertEqual([f.severity for f in found], [Severity.CRITICAL])
        ev = found[0].evidence
        self.assertEqual((ev["node"], ev["flow"]), ("w1", "10.10.10.11:835 -> 10.108.41.72"))
        self.assertEqual(ev["pinned_after_ready_s"], 90)

    def test_the_same_flow_on_the_new_pod_is_not_pinned(self):
        found = self.found([self.flow(90, self.OLD), self.flow(160, self.NEW)])
        self.assertEqual([f.severity for f in found], [Severity.INFO])
        self.assertIn("ruled out", found[0].title)

    def test_the_old_pod_inside_the_grace_after_ready_is_not_pinned(self):
        # Ready at 130: the client may still be finishing its reconnect at 140.
        found = self.found([self.flow(140, self.OLD), self.flow(160, self.NEW)])
        self.assertEqual([f.severity for f in found], [Severity.INFO])

    def test_a_closed_entry_that_lingers_is_not_pinned(self):
        found = self.found([self.flow(160, self.OLD, state="TIME_WAIT"),
                            self.flow(160, "", node="w2", state="NONE")])
        self.assertEqual([f.severity for f in found], [Severity.INFO])

    def test_a_restart_the_samples_never_reach_after_is_not_judged(self):
        with self.assertRaises(SkipDetector):
            self.found([self.flow(90, self.OLD)])

    def test_a_restart_without_recorded_addresses_is_skipped(self):
        bare = Restart(target="mds", pod="mds-0", node="w3", deleted=ts(100), ready=ts(130))
        with self.assertRaises(SkipDetector):
            self.found([self.flow(160, self.OLD)], restarts=[bare])

    def test_a_run_without_samples_is_skipped(self):
        with self.assertRaises(SkipDetector):
            self.found([])

