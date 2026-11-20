"""The regression corpus: archived pNFS runs, judged again on every change to the detectors.

Each fixture under tests/fixtures/runs is a real run on the lab cluster, trimmed by
tests/fixtures/trim_run.py to what the detectors read. The verdict each one reached is the
evidence a bug was found or fixed, so a detector change that flips one has to be
deliberate: it either catches something the run hid, which is worth saying in the change,
or it has stopped catching something it caught.

The detectors run with the pnfs-fio suite's settings, read from the suite file, so the
corpus judges what a run judges today. Assertions are by detector, severity, and count,
not by wording, so rephrasing a finding does not break the corpus.
"""

from __future__ import annotations

import os
import sys
import tempfile
import unittest
from collections import Counter

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.core import (  # noqa: E402
    Logger,
    Report,
    RunContext,
    Runner,
    Severity,
    known_components,
    known_detectors,
    load,
    suite_path,
)

FIXTURES = os.path.join(os.path.dirname(__file__), "fixtures", "runs")


def judge(run: str) -> Report:
    """Judge one fixture with the pnfs-fio suite's detectors, as `sbtest analyze` would."""
    cfg = load(suite_path("pnfs-fio"), list(known_components()), list(known_detectors()))
    cfg.components.enabled = {}
    # Other test modules register throwaway detectors (test.*), and a detector is on unless
    # a suite turns it off, so a whole-suite run would judge the corpus with them too.
    cfg.detectors.enabled = {name: opts for name, opts in cfg.detectors.enabled.items()
                             if not name.startswith("test.")}
    with tempfile.TemporaryDirectory() as d:
        ctx = RunContext(run_id=run, outdir=d, log=Logger(None))
        runner = Runner(cfg, ctx).build()
        return runner.judge(ArchiveEvidence(os.path.join(FIXTURES, run)))


def counts(report: Report, *severities: Severity) -> Counter[tuple[str, Severity]]:
    return Counter((f.detector, f.severity) for f in report.findings
                   if f.severity in severities)


class Corpus(unittest.TestCase):
    """What every fixture has to satisfy whatever its verdict."""

    RUNS = sorted(os.listdir(FIXTURES))

    def test_the_corpus_is_there(self):
        self.assertEqual(self.RUNS, ["pnfs-1791461034", "pnfs-1791468760", "pnfs-1791484322",
                                     "pnfs-1791488311"])

    def test_no_detector_raises_on_any_fixture(self):
        for run in self.RUNS:
            with self.subTest(run=run):
                raised = [f for f in judge(run).findings if "raised" in f.title]
                self.assertEqual(raised, [])

    def test_no_fixture_carries_anything_secret_shaped(self):
        for run in self.RUNS:
            with self.subTest(run=run):
                found = [f for f in judge(run).findings
                         if f.detector == "security.secret-exposure"]
                self.assertEqual(found, [])


class MdsRestartWithoutReprobe(unittest.TestCase):
    """pnfs-1791461034: the MDS restarted mid-run, and every client's device lookup after
    it came from the pod's mount namespace and failed, so no client went back to its
    namespace and all of their I/O went through the MDS until fio ended."""

    def setUp(self):
        self.report = judge("pnfs-1791461034")

    def test_it_fails(self):
        self.assertTrue(self.report.failed)

    def test_every_client_namespace_stopped_and_never_resumed(self):
        crit = [f for f in self.report.findings if f.detector == "pnfs.device-io"
                and f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 8)
        self.assertTrue(all("never resumed" in f.title for f in crit), [f.title for f in crit])

    def test_every_instance_sent_data_through_the_mds(self):
        self.assertEqual(counts(self.report, Severity.WARNING)[("pnfs.layout", Severity.WARNING)],
                         16)

    def test_no_fio_instance_failed_outright(self):
        self.assertEqual(counts(self.report, Severity.CRITICAL)[("fio.job-error",
                                                                 Severity.CRITICAL)], 0)


class MdsRestartLosingOpens(unittest.TestCase):
    """pnfs-1791468760: the restarted MDS could not fence the previous boot's reservation
    keys (SPDK refuses Preempt and Abort), so the clients never got their direct path back,
    and one client reclaimed its opens on an export not yet re-exported, lost them, and fio
    failed with EBADF."""

    def setUp(self):
        self.report = judge("pnfs-1791468760")

    def test_it_fails(self):
        self.assertTrue(self.report.failed)

    def test_both_instances_of_the_pod_that_lost_its_opens_failed(self):
        failed = [f for f in self.report.findings if f.detector == "fio.job-error"
                  and f.severity == Severity.CRITICAL]
        self.assertEqual(len(failed), 2)
        self.assertTrue(all("fio-7" in f.subject for f in failed), [f.subject for f in failed])

    def test_the_client_namespaces_never_resumed(self):
        crit = [f for f in self.report.findings if f.detector == "pnfs.device-io"
                and f.severity == Severity.CRITICAL]
        self.assertEqual(len(crit), 7)
        self.assertTrue(all("never resumed" in f.title for f in crit), [f.title for f in crit])

    def test_data_went_through_the_mds(self):
        self.assertEqual(counts(self.report, Severity.WARNING)[("pnfs.layout", Severity.WARNING)],
                         16)


class MdsRestartRecovered(unittest.TestCase):
    """pnfs-1791484322: the MDS restarted mid-run with all three guest nfsd patches and the
    CSI node's re-probe in place, and every client went back to its namespace."""

    def test_it_passes_with_nothing_to_report(self):
        report = judge("pnfs-1791484322")
        self.assertFalse(report.failed)
        self.assertEqual(counts(report, Severity.CRITICAL, Severity.WARNING), Counter())


class MdsRestartRecoveredWithPauses(unittest.TestCase):
    """pnfs-1791488311: a recovered MDS restart whose clients paused for 61 to 69 seconds
    until the slowest one reclaimed and grace ended.

    The four stall warnings are expected. pnfs.device-io excuses a pause that begins at an
    MDS restart only when the run recorded the restart (restarts.json, written by
    chaos.restart), and this run predates it: its MDS was deleted by hand. A run with the
    restart recorded reports the same pauses as information.
    """

    def test_it_passes_with_the_pauses_reported_as_stalls(self):
        report = judge("pnfs-1791488311")
        self.assertFalse(report.failed)
        self.assertEqual(counts(report, Severity.CRITICAL, Severity.WARNING),
                         Counter({("pnfs.device-io", Severity.WARNING): 4}))


if __name__ == "__main__":
    unittest.main()
