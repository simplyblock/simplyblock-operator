"""Core tests: config resolution, lifecycle ordering, findings/report, archive round-trip.

The lifecycle tests matter more than they look. A component that starts pods must be torn
down even when the run explodes, and a collector that fails must not take the run's
judgement with it — both are properties of the runner, not of any component, so this is the
only place they can be pinned.
"""

from __future__ import annotations

import csv
import json
import os
import sys
import tempfile
import unittest
from datetime import UTC, datetime, timedelta
from typing import cast
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components import kube  # noqa: E402
from sbtest.core import (  # noqa: E402
    Component,
    Detector,
    Logger,
    Report,
    RunContext,
    Runner,
    Severity,
    SkipDetector,
    apply_cli_toggles,
    component,
    critical,
    detector,
    known_components,
    known_detectors,
    load,
    now_utc,
)
from sbtest.core.config import _resolve  # noqa: E402


class ConfigResolution(unittest.TestCase):
    KNOWN_C = ["logs.stream", "logs.collect"]
    KNOWN_D = ["ana.freeze-count", "fio.checksum"]

    def test_absent_component_is_off_and_absent_detector_is_on(self):
        """Collecting is a cost you opt into; judging should not need remembering."""
        cfg = load(None, self.KNOWN_C, self.KNOWN_D)
        self.assertEqual(cfg.components.enabled, {})
        self.assertEqual(sorted(cfg.components.disabled), sorted(self.KNOWN_C))
        self.assertEqual(sorted(cfg.detectors.enabled), sorted(self.KNOWN_D))

    def test_bool_and_mapping_forms(self):
        sel = _resolve({"logs.stream": True, "logs.collect": {"ttl_s": 99}},
                       self.KNOWN_C, False)
        self.assertEqual(sel.enabled["logs.stream"], {})
        self.assertEqual(sel.enabled["logs.collect"], {"ttl_s": 99})

    def test_enabled_false_inside_a_mapping_disables(self):
        sel = _resolve({"logs.stream": {"enabled": False, "ttl_s": 5}}, self.KNOWN_C, True)
        self.assertIn("logs.stream", sel.disabled)
        self.assertNotIn("logs.stream", sel.enabled)

    def test_unknown_name_fails_loudly(self):
        with self.assertRaises(ValueError) as cm:
            _resolve({"logs.stremm": True}, self.KNOWN_C, False)
        self.assertIn("logs.stremm", str(cm.exception))

    def test_unknown_option_fails_at_build_not_at_use(self):
        with self.assertRaises(KeyError):
            sbtest.build_detector("nope.not-a-detector")
        with self.assertRaises(ValueError):
            sbtest.build_detector("ana.freeze-count", wrong_option=1)

    def test_cli_disable_beats_enable(self):
        sel = _resolve({}, self.KNOWN_C, True)
        apply_cli_toggles(sel, ["logs.stream"], ["logs.stream"])
        self.assertIn("logs.stream", sel.disabled)
        self.assertNotIn("logs.stream", sel.enabled)

    def test_yaml_suite_round_trip(self):
        """Comments and all — the reason suites are YAML is that a threshold wants a sentence
        saying why it is that number, and the parser must not care that one is there."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "suite.yaml")
            with open(p, "w") as fh:
                fh.write("components:\n"
                         "  logs.stream:\n"
                         "    ttl_s: 7      # six hours was not enough on vm04\n"
                         "detectors:\n"
                         "  fio.checksum: false\n")
            cfg = load(p, self.KNOWN_C, self.KNOWN_D)
        self.assertEqual(cfg.components.enabled["logs.stream"], {"ttl_s": 7})
        self.assertIn("fio.checksum", cfg.detectors.disabled)
        self.assertIn("ana.freeze-count", cfg.detectors.enabled)

    def test_json_suite_still_loads(self):
        """Kept working for anything generating suites programmatically."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "suite.json")
            with open(p, "w") as fh:
                json.dump({"components": {"logs.stream": {"ttl_s": 7}},
                           "detectors": {"fio.checksum": False}}, fh)
            cfg = load(p, self.KNOWN_C, self.KNOWN_D)
        self.assertEqual(cfg.components.enabled["logs.stream"], {"ttl_s": 7})
        self.assertIn("fio.checksum", cfg.detectors.disabled)

    def test_malformed_yaml_names_the_file(self):
        """A parse error must say which suite and where, not surface as a bare YAMLError."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "broken.yaml")
            with open(p, "w") as fh:
                fh.write("components:\n  logs.stream: {ttl_s: 7\n")
            with self.assertRaises(ValueError) as cm:
                load(p, self.KNOWN_C, self.KNOWN_D)
        self.assertIn("broken.yaml", str(cm.exception))

    def test_a_suite_that_is_not_a_mapping_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "list.yaml")
            with open(p, "w") as fh:
                fh.write("- logs.stream\n- logs.collect\n")
            with self.assertRaises(ValueError) as cm:
                load(p, self.KNOWN_C, self.KNOWN_D)
        self.assertIn("mapping", str(cm.exception))

    def test_every_bundled_suite_loads_against_the_real_registry(self):
        """The suites ship with the package, so a typo in one is a broken release. Loading
        resolves names against the registry, so this also pins that no suite references a
        detector that has since been renamed."""
        names = ["analyze-only", "corruption-hunt", "migration-soak", "migration-full"]
        for name in names:
            p = sbtest.suite_path(name)
            self.assertIsNotNone(p, f"bundled suite {name} not found")
            assert p is not None
            self.assertTrue(p.endswith(".yaml"), f"{name} should be YAML, got {p}")
            cfg = load(p, list(known_components()), list(known_detectors()))
            self.assertTrue(cfg.raw.get("description"), f"{name} has no description")
        # migration-full is the one that must actually drive a run.
        full = load(sbtest.suite_path("migration-full"),
                    list(known_components()), list(known_detectors()))
        self.assertIn("workload.fio", full.components.enabled)
        self.assertIn("migration.driver", full.components.enabled)


class Lifecycle(unittest.TestCase):
    def ctx(self, d):
        return RunContext(run_id="t", outdir=d, log=Logger(None))

    def test_hooks_run_in_order(self):
        calls = []

        @component
        class Ordered(Component):
            name = "test.ordered"
            def setup(self, ctx): calls.append("setup")
            def start(self, ctx): calls.append("start")
            def tick(self, ctx): calls.append("tick")
            def stop(self, ctx): calls.append("stop")
            def collect(self, ctx): calls.append("collect")
            def teardown(self, ctx): calls.append("teardown")

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            apply_cli_toggles(cfg.components, ["test.ordered"], [])
            cfg.detectors.enabled = {}
            r = Runner(cfg, self.ctx(d)).build()
            for phase in ("setup", "start", "tick", "stop", "collect", "teardown"):
                getattr(r, phase)()
        self.assertEqual(calls, ["setup", "start", "tick", "stop", "collect", "teardown"])

    def test_teardown_runs_even_when_setup_failed(self):
        """A component that allocates in setup must still get its teardown."""
        torn = []

        @component
        class Exploding(Component):
            name = "test.exploding"
            required = True  # this component *is* the run; failing it must abort
            def setup(self, ctx): raise RuntimeError("boom")
            def teardown(self, ctx): torn.append(self.name)

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            apply_cli_toggles(cfg.components, ["test.exploding"], [])
            cfg.detectors.enabled = {}
            r = Runner(cfg, self.ctx(d)).build()
            with self.assertRaises(RuntimeError):
                r.setup()
            r.teardown()
        self.assertEqual(torn, ["test.exploding"])

    def test_a_failing_collector_does_not_stop_the_others(self):
        done = []

        @component
        class BadCollect(Component):
            name = "test.badcollect"
            def collect(self, ctx): raise RuntimeError("nope")

        @component
        class GoodCollect(Component):
            name = "test.goodcollect"
            def collect(self, ctx): done.append("good")

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            apply_cli_toggles(cfg.components, ["test.badcollect", "test.goodcollect"], [])
            cfg.detectors.enabled = {}
            r = Runner(cfg, self.ctx(d)).build()
            r.collect()
        self.assertEqual(done, ["good"])
        # and the failure is recorded rather than swallowed
        self.assertTrue(any(f.detector == "component/test.badcollect" for f in r.report.findings))


class Judging(unittest.TestCase):
    def test_a_raising_detector_is_reported_not_treated_as_clean(self):
        @detector
        class Boom(Detector):
            name = "test.boom"
            def detect(self, ev): raise ValueError("bug in me")

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            cfg.components.enabled = {}
            cfg.detectors.enabled = {"test.boom": {}}
            r = Runner(cfg, RunContext(run_id="t", outdir=d, log=Logger(None))).build()
            # Evidence is never touched: the detector raises first. That is the point.
            rep = r.judge(cast("sbtest.core.Evidence", object()))
        self.assertIn("test.boom", rep.skipped)
        self.assertTrue(any("raised" in f.title for f in rep.findings))

    def test_skip_is_distinguishable_from_clean(self):
        @detector
        class Skipper(Detector):
            name = "test.skipper"
            def detect(self, ev): raise SkipDetector("no evidence here")

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            cfg.components.enabled = {}
            cfg.detectors.enabled = {"test.skipper": {}}
            r = Runner(cfg, RunContext(run_id="t", outdir=d, log=Logger(None))).build()
            rep = r.judge(cast("sbtest.core.Evidence", object()))
        self.assertEqual(rep.findings, [])
        self.assertEqual(rep.skipped["test.skipper"], "no evidence here")
        self.assertEqual(rep.verdict, "PASS")  # nothing found, but the gap is on record


class Findings(unittest.TestCase):
    def test_critical_fails_the_verdict(self):
        r = Report(run_id="x")
        self.assertEqual(r.verdict, "PASS")
        r.add(critical("d", "bad thing", subject="s"))
        self.assertEqual(r.verdict, "FAIL")

    def test_by_subject_groups_and_orders_worst_first(self):
        r = Report()
        r.add(sbtest.info("a", "note", subject="mig-1"),
              critical("b", "boom", subject="mig-1"))
        self.assertEqual([f.severity for f in r.by_subject()["mig-1"]],
                         [Severity.CRITICAL, Severity.INFO])

    def test_json_is_serialisable(self):
        r = Report(run_id="x")
        r.add(critical("d", "t", subject="s", evidence={"n": 1}))
        parsed = json.loads(r.to_json())
        self.assertEqual(parsed["verdict"], "FAIL")
        self.assertEqual(parsed["findings"][0]["severity"], "CRITICAL")


class Archive(unittest.TestCase):
    """The archive reader is the seam that makes a check testable offline, so its
    tolerance for missing files is a property worth pinning."""

    def _write_run(self, d: str) -> None:
        t0 = datetime(2026, 8, 19, 22, 0, 0, tzinfo=UTC)
        state = {"run_id": "fiomig-test", "pods": ["fiomig-test-fio-0"], "migrations": [
            {"name": "mig-1", "start": t0.isoformat().replace("+00:00", "Z"),
             "end": (t0 + timedelta(seconds=30)).isoformat().replace("+00:00", "Z"),
             "phase": "Completed", "source": "srcuuid", "target": "tgtuuid",
             "pv": "pvc-a", "group_pvs": ["pvc-a", "pvc-b"]}]}
        with open(os.path.join(d, "state.json"), "w") as fh:
            json.dump(state, fh)
        os.makedirs(os.path.join(d, "ana"), exist_ok=True)
        with open(os.path.join(d, "ana", "mig-1.csv"), "w", newline="") as fh:
            w = csv.writer(fh)
            w.writerow(["ts", "node", "phase", "address", "role", "ctrl_state", "nsid", "ana_state"])
            for off, st in ((0, "optimized"), (2, "inaccessible"), (4, "optimized")):
                stamp = (t0 + timedelta(seconds=off)).strftime("%Y-%m-%dT%H:%M:%SZ")
                for nsid in (1, 2):
                    w.writerow([stamp, "vm03", "Running", "10.0.0.1:4420", "source",
                                "live", nsid, st])
        pod = os.path.join(d, "fiomig-test-fio-0")
        os.makedirs(pod, exist_ok=True)
        with open(os.path.join(pod, "result.json"), "w") as fh:
            json.dump({"jobs": [{"error": 121, "read": {"iops": 100.0},
                                 "write": {"iops": 50.0}}]}, fh)
        with open(os.path.join(pod, "fio.log"), "w") as fh:
            fh.write("2026-08-19T22:00:05.000000000Z stderr F all fine\n")
        with open(os.path.join(pod, "timeseries.csv"), "w") as fh:
            fh.write("t,wall,total_iops\n0,,100\n1,,0\n")
        with open(os.path.join(d, "spdk-4420.txt"), "w") as fh:
            fh.write("boring line\n")

    def test_reads_a_run_directory(self):
        with tempfile.TemporaryDirectory() as d:
            self._write_run(d)
            ev = ArchiveEvidence(d)
            self.assertEqual(ev.run_id, "fiomig-test")
            migs = ev.migrations()
            self.assertEqual(len(migs), 1)
            self.assertTrue(migs[0].batch)  # two group_pvs
            self.assertEqual(len(ev.ana_samples("mig-1")), 3)  # one per (ts, node, address)
            self.assertEqual(ev.fio_jobs()[0].error, 121)
            self.assertAlmostEqual(ev.fio_jobs()[0].total_iops, 150.0)
            self.assertEqual(len(ev.fio_timeseries("fiomig-test-fio-0")), 2)
            self.assertIn("spdk-4420", ev.container_logs())

    def test_missing_files_yield_empty_not_an_exception(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            ev = ArchiveEvidence(d)
            self.assertEqual(ev.migrations(), [])
            self.assertEqual(ev.ana_samples("nope"), [])
            self.assertEqual(ev.fio_jobs(), [])
            self.assertEqual(list(ev.fio_log("nope")), [])
            self.assertEqual(ev.nvme_controllers(), [])

    def test_falls_back_to_test_log_when_state_is_absent(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "test.log"), "w") as fh:
                fh.write(
                    "2026-08-19T22:00:00Z [EVENT   ] MIGRATION START  mig-9 kind=namespaced "
                    "pod=p pv=pvc-x source=s target=t\n"
                    "2026-08-19T22:01:00Z [EVENT   ] MIGRATION STOP   mig-9 phase=TIMEOUT "
                    "error='timed out'\n")
            ev = ArchiveEvidence(d)
            migs = ev.migrations()
            self.assertEqual(len(migs), 1)
            self.assertEqual(migs[0].phase, "TIMEOUT")
            self.assertEqual(migs[0].error, "timed out")

    def test_absent_directory_is_an_error(self):
        with self.assertRaises(FileNotFoundError):
            ArchiveEvidence("/definitely/not/here")


class Registry(unittest.TestCase):
    @staticmethod
    def _bundled(reg):
        # Other tests register throwaway plugins into the same global registry.
        return {k: v for k, v in reg.items() if not k.startswith("test.")}

    def test_every_bundled_detector_has_a_name_and_a_summary(self):
        for name, cls in self._bundled(known_detectors()).items():
            self.assertTrue(name, cls)
            self.assertTrue(cls.summary, f"{name} needs a summary")

    def test_every_bundled_component_has_a_name_and_a_summary(self):
        for name, cls in self._bundled(known_components()).items():
            self.assertTrue(name, cls)
            self.assertTrue(cls.summary, f"{name} needs a summary")

    def test_detector_defaults_are_the_option_allow_list(self):
        for name, cls in self._bundled(known_detectors()).items():
            d = cls()
            self.assertEqual(set(d.options), set(d.defaults()),
                             f"{name}: options must start from defaults")

    def test_duplicate_registration_is_rejected(self):
        with self.assertRaises(ValueError):
            @detector
            class Dup(Detector):
                name = "ana.freeze-count"


if __name__ == "__main__":
    unittest.main(verbosity=2)


class RunWindowRecording(unittest.TestCase):
    """A live run must record its own window.

    Found by running the collector against a real cluster: with no window, nothing in the
    artifact directory says when the run was, so every detector reading a ring buffer marks
    what it finds UNKNOWN — and UNKNOWN counts. The collector failed on twenty-nine filesystem
    shutdowns that had happened hours before it started.
    """

    def test_run_json_is_written_and_read_back(self):
        with tempfile.TemporaryDirectory() as d:
            ctx = RunContext(run_id="t", outdir=d, log=Logger(None))
            ctx.mark_window()
            ctx.mark_window(end=now_utc())
            with open(os.path.join(d, "run.json")) as fh:
                rec = json.load(fh)
            self.assertEqual(rec["run_id"], "t")
            self.assertTrue(rec["start"] and rec["end"])
            start, end = ArchiveEvidence(d).run_window()
            self.assertIsNotNone(start)
            self.assertIsNotNone(end)

    def test_a_later_mark_does_not_erase_the_recorded_end(self):
        """Regression: 2026-08-26-mark-window-erases-end (PR #445 review).

        mark_window persisted its `end` *argument* rather than the end it had stored, so any
        later call without one rewrote run.json with end: null. Every detector that bounds
        what it may blame on the run reads that window; with no end, evidence from after the
        run counts as the run's.
        """
        with tempfile.TemporaryDirectory() as d:
            ctx = RunContext(run_id="t", outdir=d, log=Logger(None))
            ctx.mark_window()
            ctx.mark_window(end=now_utc())
            ctx.mark_window()          # a second collect phase, a component, a retry
            with open(os.path.join(d, "run.json")) as fh:
                rec = json.load(fh)
            self.assertIsNotNone(rec["end"])
            self.assertIsNotNone(ArchiveEvidence(d).run_window()[1])

    def test_run_json_wins_over_inference_from_test_log(self):
        """The run's own record beats whatever happened to get logged."""
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "test.log"), "w") as fh:
                fh.write("1999-01-01T00:00:00Z [INFO] ancient\n")
            with open(os.path.join(d, "run.json"), "w") as fh:
                json.dump({"run_id": "t", "start": "2026-08-20T11:40:09Z",
                           "end": "2026-08-20T11:42:20Z"}, fh)
            start, _end = ArchiveEvidence(d).run_window()
            assert start is not None
            self.assertEqual(start.year, 2026)

    def test_a_recorded_window_demotes_older_damage(self):
        """The end-to-end point: with a window, inherited damage stops failing the run."""
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "run.json"), "w") as fh:
                json.dump({"run_id": "t", "start": "2026-08-20T11:40:09Z",
                           "end": "2026-08-20T11:42:20Z"}, fh)
            # A filesystem shutdown from hours before the run began.
            with open(os.path.join(d, "dmesg-vm03.txt"), "w") as fh:
                fh.write("[Thu Aug 20 05:46:57 2026] XFS (nvme0n1): "
                         "Filesystem has been shut down due to log error (0x2).\n")
            ev = ArchiveEvidence(d)
            found = list(sbtest.build_detector("kernel.filesystem-shutdown").detect(ev))
            self.assertEqual(len(found), 1)
            self.assertIs(found[0].attribution, sbtest.Attribution.PRE_EXISTING)
            rep = Report()
            rep.add(*found)
            self.assertFalse(rep.failed)          # must not fail the run
            self.assertEqual(rep.verdict, "PASS")  # a WARNING, so not even inconclusive


class GrabberNaming(unittest.TestCase):
    """Two components wanting a grabber on one node must not collide.

    Found by running it: a Pod is immutable, so the second component to apply the same name
    failed on a field it may not change — and logs.collect silently produced empty files for
    every node logs.stream had already claimed.
    """

    def test_stream_and_collect_pick_different_pod_names(self):
        from sbtest.components.logs import LogCollect, LogStream
        ctx = RunContext(run_id="run1", outdir="/tmp", log=Logger(None))
        node = "vm02.example.com"
        stream = LogStream()
        collect = LogCollect()
        n1 = stream._grabber_manifest(ctx, node, f"sbtest-{stream.name.replace('.', '-')}"
                                     f"-vm02-run1", 100)
        n2 = collect._grabber_manifest(ctx, node, f"sbtest-{collect.name.replace('.', '-')}"
                                      f"-vm02-run1", 100)
        name1 = json.loads(n1)["metadata"]["name"]
        name2 = json.loads(n2)["metadata"]["name"]
        self.assertNotEqual(name1, name2)
        self.assertIn("logs-stream", name1)
        self.assertIn("logs-collect", name2)


class GrabberReuse(unittest.TestCase):
    """logs.collect must reuse logs.stream's grabbers, and must not delete them.

    Two components wanting a privileged pod on the same node is the normal case, and the
    first attempt at fixing their collision only gave them different names — which works but
    leaves two pods per node doing the same job. Reuse is the intended behaviour; the distinct
    names are the backstop for when reuse is not possible.
    """

    def _probe(self):
        """A LogCollect with the cluster faked at the component's own boundaries.

        Everything below `collect()` is stubbed and everything inside it runs, because the
        previous version of these cases re-implemented the reuse arithmetic in the test body
        and never called `collect()` at all — so it went on passing while the component
        borrowed nothing and recorded nothing to tear down.
        """
        from sbtest.components import logs as logs_mod
        started: list[list[str]] = []
        deleted: list[list[str]] = []

        class Probe(logs_mod.LogCollect):
            def _start_grabbers(self, ctx, nodes, ttl_s):
                started.append(list(nodes))
                return {n: f"own-{n}" for n in nodes}

            def _delete_grabbers(self, ctx, names):
                deleted.append(list(names))

        pods = [kube.Pod(name=f"snode-spdk-{i}", namespace="default", node=node,
                         containers=("spdk-container",))
                for i, node in enumerate(("vm02", "vm03", "vm04"))]
        c = Probe(targets=[{"pods": ["snode-spdk"], "containers": ["spdk-container"],
                            "name_from": "pod-container"}])
        return c, started, deleted, pods

    def test_collect_reuses_published_grabbers_and_starts_only_the_rest(self):
        """Regression: 2026-08-26-logcollect-ignores-published-grabbers (PR #445 review).

        logs.collect started its own privileged pod on every node regardless of what
        logs.stream had already published in ctx.shared["logs.grabbers"], so a run carried two
        privileged pods per node doing the same job.
        """
        c, started, _deleted, pods = self._probe()
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                mock.patch.object(kube, "run_bytes", lambda *a, **k: b"log line\n"):
            ctx = RunContext(run_id="r", outdir=d, log=Logger(None))
            ctx.shared["logs.grabbers"] = {"vm02": "stream-vm02", "vm03": "stream-vm03"}
            c.collect(ctx)
        self.assertEqual(started, [["vm04"]])                  # only the uncovered node
        self.assertEqual(c._grabbers["vm02"], "stream-vm02")   # borrowed, not replaced
        self.assertEqual(c._grabbers["vm04"], "own-vm04")

    def test_teardown_removes_the_grabbers_collect_started(self):
        """Regression: 2026-08-26-logcollect-grabber-leak (PR #445 review).

        collect() never recorded what it started in `_own`, so teardown deleted nothing and
        every privileged pod it created outlived the run, up to its TTL.
        """
        c, _started, deleted, pods = self._probe()
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                mock.patch.object(kube, "run_bytes", lambda *a, **k: b"log line\n"):
            ctx = RunContext(run_id="r", outdir=d, log=Logger(None))
            ctx.shared["logs.grabbers"] = {"vm02": "stream-vm02"}
            c.collect(ctx)
            c.teardown(ctx)
        # Its own three, and none of logs.stream's: deleting a borrowed pod would pull it
        # out from under the component that owns it.
        self.assertEqual(deleted, [["own-vm03", "own-vm04"]])

    def test_collect_works_standalone_when_nothing_published(self):
        """logs.stream may be disabled — collect must still start what it needs."""
        c, started, _deleted, pods = self._probe()
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                mock.patch.object(kube, "run_bytes", lambda *a, **k: b"log line\n"):
            ctx = RunContext(run_id="r", outdir=d, log=Logger(None))
            c.collect(ctx)
        self.assertEqual(started, [["vm02", "vm03", "vm04"]])
        self.assertEqual(sorted(c._grabbers), ["vm02", "vm03", "vm04"])

    def test_lifecycle_order_makes_reuse_safe(self):
        """stop precedes collect, and teardown follows it — so the borrowed pod is idle and
        still alive exactly when collection needs it."""
        order: list[str] = []

        @component
        class Streamish(Component):
            name = "test.streamish"
            def start(self, ctx): order.append("stream.start")
            def stop(self, ctx): order.append("stream.stop")
            def teardown(self, ctx): order.append("stream.teardown")

        @component
        class Collectish(Component):
            name = "test.collectish"
            def collect(self, ctx): order.append("collect.collect")

        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            apply_cli_toggles(cfg.components, ["test.streamish", "test.collectish"], [])
            cfg.detectors.enabled = {}
            r = Runner(cfg, RunContext(run_id="r", outdir=d, log=Logger(None))).build()
            for phase in ("setup", "start", "stop", "collect", "teardown"):
                getattr(r, phase)()

        self.assertLess(order.index("stream.stop"), order.index("collect.collect"))
        self.assertLess(order.index("collect.collect"), order.index("stream.teardown"))
