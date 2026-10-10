"""Core tests: config resolution, lifecycle ordering, findings/report, archive round-trip.

The lifecycle tests matter more than they look. A component that starts pods must be torn
down even when the run explodes, and a collector that fails must not take the run's
judgement with it — both are properties of the runner, not of any component, so this is the
only place they can be pinned.
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import sys
import tempfile
import time
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any, cast
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components import kube  # noqa: E402
from sbtest.components.chaos import RestartPlan  # noqa: E402
from sbtest.components.workloads.churn import ChurnPlan  # noqa: E402
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


@component
class _Looks(Component):
    """Records the options it was set up with, for the namespace-binding tests."""

    name = "test.looks"
    namespace_options = {"csi_namespace": "operator", "snode_namespace": "cluster",  # noqa: RUF012
                         "namespace": "test"}
    seen: dict[str, Any] = {}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {"csi_namespace": None, "snode_namespace": None, "namespace": None}

    def setup(self, ctx: RunContext) -> None:
        _Looks.seen = dict(self.options)


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

    def _seen(self, **opts: object) -> dict[str, object]:
        _Looks.seen = {}
        with tempfile.TemporaryDirectory() as d:
            cfg = load(None, list(known_components()), list(known_detectors()))
            apply_cli_toggles(cfg.components, ["test.looks"], [])
            cfg.components.enabled["test.looks"] = dict(opts)
            cfg.detectors.enabled = {}
            ctx = RunContext(run_id="t", outdir=d, log=Logger(None), operator_namespace="sb-op",
                             cluster_namespace="sb-cluster-a", test_namespace="sb-test")
            Runner(cfg, ctx).build().setup()
        return _Looks.seen

    def test_each_namespace_option_gets_its_run_namespace_before_setup(self):
        """Where the operator, the cluster, and the test's own pods live are properties of
        the run, not of each component."""
        self.assertEqual(self._seen(), {"csi_namespace": "sb-op",
                                        "snode_namespace": "sb-cluster-a",
                                        "namespace": "sb-test"})

    def test_an_explicit_namespace_option_wins(self):
        self.assertEqual(self._seen(csi_namespace="csi-only")["csi_namespace"], "csi-only")

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

    def test_a_followed_log_is_placed_in_time_through_its_pod_prefix(self):
        # chaos.restart follows a victim with `kubectl logs --prefix --timestamps`, which
        # puts the pod and container in front of the stamp. Unparsed, the whole restart log
        # counted as carrying no timestamp, and nothing in it could be placed in time.
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "restart-1-mds-mds-0.txt"), "w") as fh:
                fh.write("[pod/mds-0/mds-runner] 2026-10-09T06:07:43.860630989Z booting\n"
                         "[pod/mds-0/mds-runner] 2026-10-09T06:08:11.000000000Z ready\n")
            span = {s.name: s for s in ArchiveEvidence(d).log_spans()}["restart-1-mds-mds-0"]
            self.assertEqual(span.first, datetime(2026, 10, 9, 6, 7, 43, tzinfo=UTC))
            self.assertEqual(span.last, datetime(2026, 10, 9, 6, 8, 11, tzinfo=UTC))

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

    def test_a_pnfs_run_knows_its_cluster_from_its_volumes(self):
        # No migration, so no NQN is recorded. The pNFS volume map carries the cluster out of
        # each volume's handle, and without it nvme.dirty-start and nvme.foreign-cluster skip.
        cluster = "06075ebb-8c40-4857-a5f1-40b13bca10a7"
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            with open(os.path.join(d, "pnfs.json"), "w") as fh:
                json.dump({"volumes": [{"claim": "c", "lvol": "l", "cluster": cluster}]}, fh)
            self.assertEqual(ArchiveEvidence(d).cluster_uuid(), cluster)

    def test_reads_the_reservation_snapshots(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            with open(os.path.join(d, "reservations-post.json"), "w") as fh:
                json.dump({"namespaces": [
                    {"node": "w1", "device": "nvme3n1", "uuid": "lv1", "rtype": 4,
                     "generation": 4, "registrants": [
                         {"hostid": "8d2a", "rkey": 72057594037927936, "holder": True},
                         {"hostid": "913d", "rkey": 7694384339219550510,
                          "holder": False}]}]}, fh)
            ev = ArchiveEvidence(d)
            post, pre = ev.reservations_post(), ev.reservations_pre()
        self.assertEqual(pre, [])
        self.assertEqual(len(post), 1)
        self.assertEqual((post[0].uuid, post[0].rtype), ("lv1", 4))
        self.assertEqual([(r.hostid, r.holder) for r in post[0].registrants],
                         [("8d2a", True), ("913d", False)])
        self.assertEqual(post[0].registrants[1].rkey, 7694384339219550510)

    def test_reads_what_was_deployed(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            with open(os.path.join(d, "versions.json"), "w") as fh:
                json.dump({"server": "v1.34.1",
                           "images": [{"namespace": "simplyblock", "pod": "p", "container": "c",
                                       "image": "repo/x:1", "image_id": "repo/x@sha256:ab"}],
                           "nodes": [{"node": "w1", "kernel": "6.18.5", "os_image": "Talos",
                                      "runtime": "containerd://2", "kubelet": "v1.34.1"}]}, fh)
            got = ArchiveEvidence(d).versions()
        assert got is not None
        self.assertEqual(got.server, "v1.34.1")
        self.assertEqual([(i.pod, i.image_id) for i in got.images], [("p", "repo/x@sha256:ab")])
        self.assertEqual([(n.node, n.kernel) for n in got.nodes], [("w1", "6.18.5")])

    def test_an_archive_without_versions_has_none(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            self.assertIsNone(ArchiveEvidence(d).versions())

    def test_reads_the_restarts_a_run_made(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            with open(os.path.join(d, "restarts.json"), "w") as fh:
                json.dump({"restarts": [
                    {"target": "mds", "pod": "mds-0", "node": "w3",
                     "deleted": "2026-08-19T22:00:10Z", "ready": "2026-08-19T22:00:40Z",
                     "replacement": "mds-0"},
                    {"target": "csi-node", "pod": "csi-a", "node": "w1",
                     "deleted": "2026-08-19T22:01:00Z", "ready": None}]}, fh)
            got = ArchiveEvidence(d).restarts()
        self.assertEqual([r.target for r in got], ["mds", "csi-node"])
        ready = got[0].ready
        assert ready is not None
        self.assertEqual((ready - got[0].deleted).total_seconds(), 30)
        self.assertIsNone(got[1].ready)
        # A run from before the pod IPs were recorded reads as unknown, not as an error.
        self.assertEqual((got[0].ip, got[0].replacement_ip), ("", ""))

    def test_reads_the_pod_ips_a_restart_recorded(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "restarts.json"), "w") as fh:
                json.dump({"restarts": [
                    {"target": "mds", "pod": "mds-0", "node": "w3",
                     "deleted": "2026-08-19T22:00:10Z", "ready": "2026-08-19T22:00:40Z",
                     "replacement": "mds-0", "ip": "10.244.3.118",
                     "replacement_ip": "10.244.3.123"}]}, fh)
            got = ArchiveEvidence(d).restarts()
        self.assertEqual((got[0].ip, got[0].replacement_ip), ("10.244.3.118", "10.244.3.123"))

    def test_reads_the_conntrack_samples_a_run_took(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "conntrack.csv"), "w") as fh:
                fh.write("ts,node,state,orig_src,orig_sport,orig_dst,orig_dport,reply_src\n"
                         "2026-10-09T06:08:20Z,w1,ESTABLISHED,10.10.10.11,835,10.108.41.72,"
                         "2049,10.244.3.118\n"
                         "2026-10-09T06:08:10Z,w2,NONE,,0,,0,\n")
            got = ArchiveEvidence(d).conntrack()
        self.assertEqual([(s.node, s.state) for s in got], [("w2", "NONE"), ("w1", "ESTABLISHED")])
        self.assertEqual((got[1].orig_sport, got[1].orig_dport, got[1].reply_src),
                         (835, 2049, "10.244.3.118"))

    def test_reads_the_churn_a_run_made(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "state.json"), "w") as fh:
                json.dump({"run_id": "r", "migrations": []}, fh)
            with open(os.path.join(d, "churn.json"), "w") as fh:
                json.dump({"seed": 3, "pods": [
                    {"pod": "r-churn-2", "claim": "r-pnfs-shared-0", "own_volume": False,
                     "created": "2026-08-19T22:01:00Z", "node": "w2"},
                    {"pod": "r-churn-1", "claim": "r-churn-1", "own_volume": True,
                     "created": "2026-08-19T22:00:00Z", "io_started": "2026-08-19T22:00:05Z",
                     "deleted": "2026-08-19T22:01:05Z", "rc": 0, "pv": "pvc-1",
                     "pvc_deleted": "2026-08-19T22:01:06Z", "pv_gone": True,
                     "export_gone": False, "gone_s": 30.5}]}, fh)
            got = ArchiveEvidence(d).churn()
        self.assertEqual([c.pod for c in got], ["r-churn-1", "r-churn-2"])
        first = got[0]
        self.assertTrue(first.own_volume)
        self.assertEqual((first.rc, first.pv, first.pv_gone, first.export_gone, first.gone_s),
                         (0, "pvc-1", True, False, 30.5))
        self.assertIsNone(got[1].io_started)
        self.assertIsNone(got[1].pv_gone)

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


class FioJobTiming(unittest.TestCase):
    def test_a_jobs_start_and_runtime_come_from_its_result(self):
        """When an instance ran is what attributes a quiet device to it having finished
        rather than to a stall."""
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "r-fio-0-c0"))
            with open(os.path.join(d, "r-fio-0-c0", "result.json"), "w") as fh:
                json.dump({"jobs": [{"job_start": 1791458653564, "job_runtime": 600001,
                                     "error": 0}]}, fh)
            job = ArchiveEvidence(d).fio_jobs()[0]
        self.assertEqual(job.start, datetime(2026, 10, 8, 11, 24, 13, 564000, tzinfo=UTC))
        self.assertAlmostEqual(job.runtime_s, 600.001)


class LoadDrivers(unittest.TestCase):
    """A run with no component creating load only observes, and says so. Which components
    create load is what `required` already marks, not a naming convention: a workload is
    the run as much as a migration driver is."""

    def test_a_workload_counts_as_load(self):
        from sbtest.cli import load_drivers
        self.assertEqual(load_drivers(["workload.pnfs", "nvme.iostat", "host.dmesg"]),
                         ["workload.pnfs"])

    def test_the_migration_run_counts_both_of_its_drivers(self):
        from sbtest.cli import load_drivers
        self.assertEqual(sorted(load_drivers(["workload.fio", "migration.driver", "ana.sample"])),
                         ["migration.driver", "workload.fio"])

    def test_collectors_alone_are_no_load(self):
        from sbtest.cli import load_drivers
        self.assertEqual(load_drivers(["logs.collect", "nvme.iostat"]), [])


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


class LogNamespaces(unittest.TestCase):
    """A storage cluster's pods (SPDK, the node agents) live in its cluster namespace, and
    the operator's, the control plane's, and the CSI driver's in the operator namespace.
    Targets that named a namespace of their own found nothing on a cluster deployed
    differently, and said nothing, so pNFS runs collected neither the SPDK nor the
    node-agent log."""

    PODS = {"sb-cluster": [kube.Pod(name="snode-spdk-pod-4420-06075e", namespace="sb-cluster",
                                    node="vm02", containers=("spdk-container",))],
            "sb-op": [kube.Pod(name="simplyblock-csi-node-abc", namespace="sb-op",
                               node="vm02", containers=("csi-node",))]}

    def _list(self, ns: str, *a: object, **k: object) -> list[kube.Pod]:
        return list(self.PODS.get(ns, []))

    def _ctx(self, d: str) -> RunContext:
        return RunContext(run_id="r", outdir=d, log=Logger(None), operator_namespace="sb-op",
                          cluster_namespace="sb-cluster", test_namespace="sb-test")

    def test_the_default_targets_name_a_plane_and_no_namespace(self):
        from sbtest.components import logs as logs_mod
        for t in logs_mod.LogCollect().opt("targets"):
            self.assertNotIn("namespace", t, t)
            self.assertIn(t.get("plane"), {"operator", "cluster"}, t)

    def test_collect_reads_each_target_in_its_planes_namespace(self):
        from sbtest.components import logs as logs_mod

        class Probe(logs_mod.LogCollect):
            def _start_grabbers(self, ctx, nodes, ttl_s):
                return {n: f"own-{n}" for n in nodes}

        c = Probe(targets=[
            {"pods": ["snode-spdk"], "containers": ["spdk-container"], "plane": "cluster",
             "name_from": "snode-port"},
            {"pods": ["simplyblock-csi-node"], "containers": ["csi-node"], "plane": "operator",
             "name_from": "pod-node", "name": "csi-node"}])
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", self._list), \
                mock.patch.object(kube, "run_bytes", lambda *a, **k: b"log line\n"):
            ctx = self._ctx(d)
            c.bind_namespaces(ctx)
            c.collect(ctx)
            self.assertTrue(os.path.exists(os.path.join(d, "spdk-4420.txt")))
            self.assertTrue(os.path.exists(os.path.join(d, "csi-node-vm02.txt")))

    def test_stream_follows_the_spdk_pods_in_the_cluster_namespace(self):
        from sbtest.components import logs as logs_mod

        class Probe(logs_mod.LogStream):
            def _start_grabbers(self, ctx, nodes, ttl_s):
                return {n: f"stream-{n}" for n in nodes}

        s = Probe()
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", self._list):
            ctx = self._ctx(d)
            s.bind_namespaces(ctx)
            s.setup(ctx)
        self.assertEqual([p.name for p in s._pods], ["snode-spdk-pod-4420-06075e"])


class NamespaceFlags(unittest.TestCase):
    """A flag beats the suite's run block, which beats the default. Today, the operator and
    the cluster both run in simplyblock. The planned layout moves the operator and control
    plane to simplyblock-system and keeps the initial cluster in simplyblock, so the two
    defaults are independent."""

    def args(self, **kw: object) -> argparse.Namespace:
        base: dict[str, object] = {"operator_namespace": None, "cluster_namespace": None,
                                   "test_namespace": None}
        base.update(kw)
        return argparse.Namespace(**base)

    def test_defaults(self):
        from sbtest import cli
        cfg = load(None, list(known_components()), list(known_detectors()))
        self.assertEqual(cli.namespaces(self.args(), cfg),
                         ("simplyblock", "simplyblock", "default"))

    def test_moving_the_operator_leaves_the_cluster_in_simplyblock(self):
        from sbtest import cli
        cfg = load(None, list(known_components()), list(known_detectors()))
        cfg.run["operator_namespace"] = "simplyblock-system"
        self.assertEqual(cli.namespaces(self.args(), cfg),
                         ("simplyblock-system", "simplyblock", "default"))

    def test_an_unset_run_context_puts_the_cluster_in_simplyblock(self):
        ctx = RunContext(run_id="t", outdir="/tmp", log=Logger(None),
                         operator_namespace="simplyblock-system")
        self.assertEqual(ctx.namespace("cluster"), "simplyblock")

    def test_a_flag_beats_the_suite(self):
        from sbtest import cli
        cfg = load(None, list(known_components()), list(known_detectors()))
        cfg.run.update({"operator_namespace": "a", "cluster_namespace": "b",
                        "test_namespace": "c"})
        self.assertEqual(cli.namespaces(self.args(cluster_namespace="flag"), cfg),
                         ("a", "flag", "c"))


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


class RestartDeleteFailure(unittest.TestCase):
    """A restart whose delete failed did not happen. Recorded as one, chaos.recovery
    reported the untouched pod as never coming back (review on #698)."""

    def test_a_failed_delete_is_not_a_restart(self):
        import subprocess

        from sbtest.components import chaos

        def run(args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            rc = 1 if "delete" in args else 0
            return subprocess.CompletedProcess(args, rc, "", "forbidden" if rc else "")

        r = chaos.Restarter(ready_timeout_s=1)
        victim = kube.Pod(name="mds-0", namespace="simplyblock", node="w3", containers=(),
                          phase="Running", ip="10.244.3.118")
        with tempfile.TemporaryDirectory() as d, mock.patch.object(kube, "run", run), \
                mock.patch.object(chaos, "_uid", return_value="u1"), \
                mock.patch.object(r, "_victim", return_value=victim), \
                mock.patch.object(r, "_follow", return_value=None):
            ctx = RunContext(run_id="r1", outdir=d, log=Logger(os.path.join(d, "run.log")))
            r.bind_namespaces(ctx)
            r._restart(ctx, "mds")
            r.collect(ctx)
            ctx.log.close()
            with open(os.path.join(d, "restarts.json")) as fh:
                saved = json.load(fh)
        self.assertEqual(saved["restarts"], [])
        self.assertEqual([f["pod"] for f in saved["failed"]], ["mds-0"])
        self.assertIn("forbidden", saved["failed"][0]["error"])


class ChurnLeave(unittest.TestCase):
    """A churn pod has left only when its delete succeeded. Counted as gone after a failed
    delete, it freed a slot under the concurrency cap while still running, and a claim
    whose delete failed started the cleanup clock (review on #698)."""

    def leave(self, pod_rc: int, pvc_rc: int):
        import subprocess
        from datetime import UTC, datetime

        from sbtest.components.workloads import churn, fio

        def run(args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            rc = pod_rc if "pod" in args else pvc_rc if "pvc" in args else 0
            return subprocess.CompletedProcess(args, rc, "", "forbidden" if rc else "")

        w = churn.ChurnWorkload()
        inst = fio.FioInstance(pod="r1-fio-churn-1", container="fio-0", filename="/data/f",
                               logdir="/logs", evidence="r1-fio-churn-1")
        record = churn._Record(pod=inst.pod, claim="r1-churn-1", own_volume=True,
                               created=datetime.now(UTC))
        with tempfile.TemporaryDirectory() as d, mock.patch.object(kube, "run", run), \
                mock.patch.object(churn, "_volume_of", return_value=("pv-1", "lvol-1")):
            ctx = RunContext(run_id="r1", outdir=d, log=Logger(os.path.join(d, "run.log")))
            w._leave(ctx, "default", inst, record)
            ctx.log.close()
        return record

    def test_a_failed_pod_delete_has_not_left(self):
        record = self.leave(pod_rc=1, pvc_rc=0)
        self.assertIsNone(record.deleted)
        self.assertIsNone(record.pvc_deleted)
        self.assertIn("forbidden", record.error)

    def test_a_failed_claim_delete_starts_no_cleanup_clock(self):
        record = self.leave(pod_rc=0, pvc_rc=1)
        self.assertIsNotNone(record.deleted)
        self.assertIsNone(record.pvc_deleted)
        self.assertIn("forbidden", record.error)

    def test_both_deleted_is_a_clean_leave(self):
        record = self.leave(pod_rc=0, pvc_rc=0)
        self.assertIsNotNone(record.deleted)
        self.assertIsNotNone(record.pvc_deleted)
        self.assertEqual(record.error, "")


class RestartSchedule(unittest.TestCase):
    """When chaos.restart restarts what: at least `guaranteed` times inside the window,
    otherwise by a low chance per tick, never after the window, reproducibly per seed."""

    WEIGHTS = {"mds": 2.0, "csi-node": 1.0, "csi-controller": 0.5}

    def plan(self, **kw: object) -> RestartPlan:
        args = {"seed": 7, "weights": self.WEIGHTS, "start": 0.0, "end": 300.0,
                "guaranteed": 2, "chance": 0.0, "overlap_chance": 0.0}
        args.update(kw)
        return RestartPlan(**args)  # type: ignore[arg-type]

    def fire(self, plan: RestartPlan, until: float = 400.0, step: float = 5.0,
             in_flight: int = 0) -> list[tuple[float, str]]:
        out, t = [], 0.0
        while t <= until:
            out += [(t, target) for target in plan.due(t, in_flight)]
            t += step
        return out

    def test_the_guaranteed_restarts_all_happen_inside_the_window(self):
        fired = self.fire(self.plan(guaranteed=3))
        self.assertEqual(len(fired), 3)
        self.assertTrue(all(0.0 <= t <= 300.0 for t, _ in fired))

    def test_nothing_fires_after_the_window_whatever_the_chance(self):
        fired = self.fire(self.plan(guaranteed=0, chance=1.0), until=600.0)
        self.assertTrue(fired)
        self.assertTrue(all(t <= 300.0 for t, _ in fired))

    # pnfs-1791616895 and pnfs-1791619063 drew a csi-node for their one guaranteed restart,
    # so neither tested the metadata server's recovery it was meant to.
    def test_guaranteed_targets_name_what_the_guaranteed_restarts_restart(self):
        for seed in range(20):
            fired = self.fire(self.plan(seed=seed, guaranteed=3,
                                        guaranteed_targets=["mds", "csi-node"]))
            self.assertEqual(sorted(t for _, t in fired), ["csi-node", "mds", "mds"], seed)

    def test_without_guaranteed_targets_the_weights_decide(self):
        targets = {t for seed in range(30)
                   for _, t in self.fire(self.plan(seed=seed, guaranteed=1))}
        self.assertGreater(len(targets), 1)

    def test_the_same_seed_gives_the_same_schedule(self):
        a = self.fire(self.plan(guaranteed=2, chance=0.1))
        b = self.fire(self.plan(guaranteed=2, chance=0.1))
        self.assertEqual(a, b)

    def test_a_target_weighted_zero_is_never_chosen(self):
        fired = self.fire(self.plan(weights={"mds": 1.0, "csi-node": 0.0}, guaranteed=0,
                                    chance=1.0))
        self.assertEqual({target for _, target in fired}, {"mds"})

    def test_a_restart_waits_for_the_one_in_flight_unless_overlap_comes_up(self):
        plan = self.plan(guaranteed=1)
        self.assertEqual(self.fire(plan, until=290.0, in_flight=1), [])
        # Once nothing is in flight, the deferred restart goes, still inside the window.
        fired = self.fire(plan, until=300.0)
        self.assertEqual(len(fired), 1)


class ChurnSchedule(unittest.TestCase):
    """When a churn pod arrives, how long it lives, and whether it brings its own volume:
    a steady flow inside the window, capped, deferred rather than dropped at the cap, and
    the same for the same seed."""

    def plan(self, **kw: object) -> ChurnPlan:
        args: dict[str, object] = {"seed": 11, "start": 0.0, "end": 600.0,
                                   "mean_interval_s": 20.0, "min_life_s": 30.0,
                                   "max_life_s": 90.0, "reuse_ratio": 0.5,
                                   "max_concurrent": 100}
        args.update(kw)
        return ChurnPlan(**args)  # type: ignore[arg-type]

    def flow(self, plan: ChurnPlan, until: float = 900.0, active: int = 0,
            step: float = 1.0) -> list[tuple[float, bool, float]]:
        out, t = [], 0.0
        while t <= until:
            out += [(t, a.reuse, a.lifetime_s) for a in plan.due(t, active)]
            t += step
        return out

    def test_arrivals_keep_coming_inside_the_window_and_stop_after_it(self):
        got = self.flow(self.plan())
        self.assertGreater(len(got), 10)   # about 30 expected at one per 20s over 600s
        self.assertTrue(all(t <= 600.0 for t, _, _ in got))

    def test_lifetimes_stay_within_their_bounds(self):
        got = self.flow(self.plan())
        self.assertTrue(all(30.0 <= life <= 90.0 for _, _, life in got))

    def test_the_reuse_ratio_decides_who_brings_a_volume(self):
        self.assertTrue(all(reuse for _, reuse, _ in self.flow(self.plan(reuse_ratio=1.0))))
        self.assertFalse(any(reuse for _, reuse, _ in self.flow(self.plan(reuse_ratio=0.0))))

    def test_the_same_seed_gives_the_same_flow(self):
        self.assertEqual(self.flow(self.plan()), self.flow(self.plan()))

    def test_at_the_cap_an_arrival_waits_for_room_instead_of_being_dropped(self):
        plan = self.plan(max_concurrent=2)
        self.assertEqual(self.flow(plan, until=100.0, active=2), [])
        # Room again: the arrival that was held goes first, inside the window.
        later = self.flow(plan, until=101.0)
        self.assertTrue(later)
        self.assertLessEqual(later[0][0], 101.0)


def _cp(stdout: str = "", rc: int = 0) -> Any:
    return argparse.Namespace(stdout=stdout, stderr="", returncode=rc)


class PodAddresses(unittest.TestCase):
    """The pod IP a restart replaced is what tells a stale conntrack entry from a live one."""

    def test_list_pods_carries_each_pods_ip(self):
        items = {"items": [{"metadata": {"name": "mds-0"},
                            "spec": {"nodeName": "w3", "containers": [{"name": "c"}]},
                            "status": {"phase": "Running", "podIP": "10.244.3.118"}}]}
        with mock.patch.object(kube, "run", lambda *a, **k: _cp(json.dumps(items))):
            self.assertEqual([p.ip for p in kube.list_pods("ns")], ["10.244.3.118"])

    def test_a_restart_records_the_old_and_the_new_pod_ip(self):
        from sbtest.components import chaos

        victim = kube.Pod(name="simplyblock-pnfs-mds-x-0", namespace="op", node="w3",
                          containers=("mds-runner",), phase="Running", ip="10.244.3.118")
        replacement = {"items": [{
            "metadata": {"name": victim.name, "uid": "new"}, "spec": {"nodeName": "w3"},
            "status": {"podIP": "10.244.3.123",
                       "conditions": [{"type": "Ready", "status": "True"}]}}]}

        def run(args: list[str], **_: object) -> Any:
            return _cp(json.dumps(replacement)) if "get" in args else _cp()

        r = chaos.Restarter(namespace="op")
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.object(kube, "list_pods", lambda *a, **k: [victim]), \
                mock.patch.object(kube, "run", run), \
                mock.patch.object(chaos, "_uid", lambda *a: "old"), \
                mock.patch.object(chaos.Restarter, "_follow", lambda *a: None):
            ctx = RunContext(run_id="t", outdir=d, log=Logger(None))
            r._restart(ctx, "mds")
            r.collect(ctx)
            with open(os.path.join(d, "restarts.json")) as fh:
                rec = json.load(fh)["restarts"][0]
        self.assertEqual((rec["ip"], rec["replacement_ip"]), ("10.244.3.118", "10.244.3.123"))


class ConntrackSampling(unittest.TestCase):
    """What the conntrack sampler reads off a node, and what it records."""

    TOOLS = ("tcp      6 431996 ESTABLISHED src=10.10.10.11 dst=10.108.41.72 sport=835 "
             "dport=2049 src=10.244.3.118 dst=10.10.10.11 sport=2049 dport=835 [ASSURED] "
             "mark=0 use=1\n")
    PROC = ("ipv4     2 tcp      6 116 SYN_SENT src=10.10.10.12 dst=10.103.85.218 sport=962 "
            "dport=2049 [UNREPLIED] src=10.244.3.118 dst=10.10.10.12 sport=2049 dport=962 "
            "mark=0 zone=0 use=2\n")
    T = datetime(2026, 10, 9, 6, 8, 20, tzinfo=UTC)

    def test_parses_both_tuples_of_a_conntrack_tools_line(self):
        from sbtest.components.conntrack import parse_conntrack
        [s] = parse_conntrack(self.TOOLS, self.T, "w1")
        self.assertEqual((s.state, s.orig_src, s.orig_sport, s.orig_dst, s.orig_dport,
                          s.reply_src),
                         ("ESTABLISHED", "10.10.10.11", 835, "10.108.41.72", 2049,
                          "10.244.3.118"))

    def test_parses_a_proc_nf_conntrack_line(self):
        from sbtest.components.conntrack import parse_conntrack
        [s] = parse_conntrack(self.PROC, self.T, "w2")
        self.assertEqual((s.state, s.orig_sport, s.reply_src), ("SYN_SENT", 962, "10.244.3.118"))

    def test_keeps_only_flows_to_the_nfs_port(self):
        from sbtest.components.conntrack import parse_conntrack
        other = self.TOOLS.replace("dport=2049 src", "dport=443 src")
        text = other + "conntrack v1.4.8 (conntrack-tools): 1 flow entries have been shown.\n"
        self.assertEqual(parse_conntrack(text, self.T, "w1"), [])

    def _sampler(self, outputs: dict[str, str]) -> Any:
        from sbtest.components.conntrack import ConntrackSampler
        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op")
        s._helpers = {node: f"r-ct-{node}" for node in outputs}
        by_pod = {f"r-ct-{node}": out for node, out in outputs.items()}
        # The sampler execs through kube.run, and the pod follows "exec" in its arguments.
        return s, lambda args, **k: _cp(by_pod[args[args.index("exec") + 1]])

    # pnfs-1791616895 warned for every node at the first sample, only because the helpers
    # were still installing conntrack-tools, and never said the reads worked afterward.
    def test_a_node_that_becomes_readable_again_is_said_once(self):
        from sbtest.components.conntrack import UNAVAILABLE
        outputs = {"w1": UNAVAILABLE + "\n"}
        s, run = self._sampler(outputs)
        with tempfile.TemporaryDirectory() as d:
            log = os.path.join(d, "run.log")
            ctx = RunContext(run_id="r", outdir=d, log=Logger(log))
            with mock.patch.object(kube, "run", run):
                s._sample(ctx)
                outputs["w1"] = self.TOOLS
                s2, run2 = self._sampler(outputs)
            with mock.patch.object(kube, "run", run2):
                s._sample(ctx)
                s._sample(ctx)
            ctx.log.close()
            with open(log) as fh:
                text = fh.read()
        self.assertEqual(text.count("readable again"), 1, text)

    def test_sampling_waits_for_the_helpers_to_be_ready(self):
        from sbtest.components.conntrack import ConntrackSampler
        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op")
        s._helpers = {"w1": "r-ct-w1", "w2": "r-ct-w2"}
        tries: dict[str, int] = {}

        def run(args: list[str], **_: object) -> Any:
            pod = args[args.index("exec") + 1]
            tries[pod] = tries.get(pod, 0) + 1
            return _cp(rc=0 if tries[pod] >= 3 else 1)
        with mock.patch.object(kube, "run", run), \
                mock.patch("sbtest.components.conntrack.time.sleep", lambda s: None):
            ready = s._wait_for_helpers(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(ready, ["w1", "w2"])
        self.assertEqual(tries, {"r-ct-w1": 3, "r-ct-w2": 3})

    def test_helpers_that_never_get_ready_do_not_hold_the_run(self):
        from sbtest.components.conntrack import ConntrackSampler
        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op", helper_ready_s=0)
        s._helpers = {"w1": "r-ct-w1"}
        with mock.patch.object(kube, "run", lambda *a, **k: _cp(rc=1)), \
                mock.patch("sbtest.components.conntrack.time.sleep", lambda s: None):
            ready = s._wait_for_helpers(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(ready, [])

    def test_a_node_with_no_nfs_flows_is_still_recorded_as_sampled(self):
        # Otherwise "nothing pinned" and "never looked" read the same.
        s, exec_sh = self._sampler({"w1": self.TOOLS, "w2": ""})
        with mock.patch.object(kube, "run", exec_sh):
            s._sample(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(sorted((x.node, x.state) for x in s._samples),
                         [("w1", "ESTABLISHED"), ("w2", "NONE")])

    def test_a_node_it_cannot_read_records_nothing(self):
        from sbtest.components.conntrack import UNAVAILABLE
        s, exec_sh = self._sampler({"w1": UNAVAILABLE + "\n"})
        with mock.patch.object(kube, "run", exec_sh):
            s._sample(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(s._samples, [])

    def test_the_samples_written_are_the_ones_the_archive_reads(self):
        s, exec_sh = self._sampler({"w1": self.TOOLS, "w2": ""})
        with tempfile.TemporaryDirectory() as d, mock.patch.object(kube, "run", exec_sh):
            ctx = RunContext(run_id="r", outdir=d, log=Logger(None))
            s._sample(ctx)
            s.collect(ctx)
            got = ArchiveEvidence(d).conntrack()
        self.assertEqual(sorted((x.node, x.state, x.reply_src) for x in got),
                         [("w1", "ESTABLISHED", "10.244.3.118"), ("w2", "NONE", "")])

    # pnfs-1791554843: the helpers' apply failed, the sampler kept the helpers it had
    # named, every exec into the missing pods came back empty, and 295 rows said "NONE"
    # (no flows) for what was never read.
    def test_an_exec_that_fails_records_nothing(self):
        from sbtest.components.conntrack import ConntrackSampler
        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op")
        s._helpers = {"w1": "r-ct-w1"}
        with mock.patch.object(kube, "run", lambda *a, **k: _cp("", rc=1)):
            s._sample(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(s._samples, [])

    def test_a_failed_apply_leaves_no_helpers_to_sample(self):
        from sbtest.components.conntrack import ConntrackSampler
        pods = [kube.Pod(name="simplyblock-csi-node-w1", namespace="sb-op", node="w1",
                         containers=("csi-node",), phase="Running")]

        def run(args: list[str], stdin: str | None = None, **_: object) -> Any:
            if stdin:
                raise kube.KubectlError("kubectl apply: admission webhook denied the request")
            return _cp()

        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op")
        with mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                mock.patch.object(kube, "run", run), self.assertRaises(kube.KubectlError):
            s.setup(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        self.assertEqual(s._helpers, {})

    def test_a_helper_goes_on_every_node_running_a_node_plugin(self):
        from sbtest.components.conntrack import ConntrackSampler
        pods = [kube.Pod(name=f"simplyblock-csi-node-{n}", namespace="sb-op", node=f"{n}.lab",
                         containers=("csi-node",), phase="Running") for n in ("w1", "w2")]
        applied: list[str] = []

        def run(args: list[str], stdin: str | None = None, **_: object) -> Any:
            if stdin:
                applied.append(stdin)
            return _cp()

        s = ConntrackSampler(namespace="sb-test", csi_namespace="sb-op")
        with mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                mock.patch.object(kube, "run", run):
            s.setup(RunContext(run_id="r", outdir="/tmp", log=Logger(None)))
        # One apply per pod: neither a v1 List nor a stream of documents (see
        # KubectlApplyEach).
        docs = [json.loads(d) for d in applied]
        self.assertTrue(all(d["kind"] == "Pod" for d in docs))
        self.assertEqual(sorted(d["spec"]["nodeName"] for d in docs), ["w1.lab", "w2.lab"])
        self.assertTrue(all(d["spec"]["hostNetwork"] for d in docs))
        self.assertEqual(sorted(s._helpers), ["w1", "w2"])


class KubectlApplyEach(unittest.TestCase):
    """Objects are applied one kubectl call each. A v1 List fails client-side validation on
    a cluster whose metrics API publishes OpenAPI names with slashes in them
    (pnfs-1791554843, pnfs-1791556484), and a stream of JSON documents between YAML
    separators panicked kubectl 1.33's decoder (pnfs-1791557763: slice bounds out of range
    [-5:] in StreamReader.Consume). One object per call has no wrapper and no stream."""

    def test_each_object_is_applied_alone(self):
        stdins: list[str] = []

        def run(args: list[str], stdin: str | None = None, **_: object) -> Any:
            stdins.append(stdin or "")
            return _cp()
        docs = [{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": n}} for n in ("a", "b")]
        with mock.patch.object(kube, "run", run):
            kube.apply_each("ns", docs)
        self.assertEqual([json.loads(s)["metadata"]["name"] for s in stdins], ["a", "b"])


class ParallelCollection(unittest.TestCase):
    """Collection did one kubectl call at a time: 40-47 s for 16 fio instances and 34-37 s
    for 35 container logs in pnfs-1791616895 and pnfs-1791575321."""

    @staticmethod
    def concurrency() -> tuple[Any, list[int]]:
        import threading
        lock, now, peak = threading.Lock(), [0], [0]

        def enter() -> None:
            with lock:
                now[0] += 1
                peak[0] = max(peak[0], now[0])
            time.sleep(0.05)
            with lock:
                now[0] -= 1
        return enter, peak

    def test_fio_instances_are_collected_in_parallel_and_in_order(self):
        from sbtest.components.workloads import fio, pnfs_rwx
        enter, peak = self.concurrency()
        done: list[str] = []

        def collect(ctx: Any, ns: str, inst: Any, migs: list) -> str:
            enter()
            done.append(inst.evidence)
            return str(inst.evidence)
        w = pnfs_rwx.PnfsRwxWorkload(namespace="default")
        w._instances = [fio.FioInstance(pod=f"p{i}", container="c", filename="/f", logdir="/l",
                                        evidence=f"r-fio-{i}-c0") for i in range(8)]
        w._pods = [f"p{i}" for i in range(8)]
        with mock.patch.object(fio, "collect_instance", collect), \
                mock.patch.object(w, "after_collect", lambda ctx: None):
            w.collect(RunContext(run_id="r", outdir=tempfile.mkdtemp(), log=Logger(None)))
        self.assertGreater(peak[0], 1)
        self.assertEqual(sorted(done), sorted(i.evidence for i in w._instances))

    def test_container_logs_are_fetched_in_parallel_into_the_same_artifacts(self):
        from sbtest.components import logs as logs_mod
        enter, peak = self.concurrency()
        pods = [kube.Pod(name=f"simplyblock-csi-node-{n}", namespace="sb", node=n,
                         containers=("csi-node",), phase="Running") for n in ("w1", "w2", "w3")]

        def run_bytes(args: list[str], **_: object) -> bytes:
            enter()
            return f"log of {args[args.index('exec') + 1]}\n".encode()
        c = logs_mod.LogCollect(namespace="sb", targets=[
            {"pods": ["simplyblock-csi-node"], "containers": ["csi-node"], "plane": "operator",
             "name_from": "pod-node", "name": "csi-node"}])
        with tempfile.TemporaryDirectory() as d:
            ctx = RunContext(run_id="r", outdir=d, log=Logger(None))
            ctx.shared["logs.grabbers"] = {n: f"grab-{n}" for n in ("w1", "w2", "w3")}
            c.bind_namespaces(ctx)
            with mock.patch.object(kube, "list_pods", lambda *a, **k: pods), \
                    mock.patch.object(kube, "run_bytes", run_bytes):
                c.collect(ctx)
            got = {}
            for f in sorted(os.listdir(d)):
                if f.startswith("csi-node-"):
                    with open(os.path.join(d, f)) as fh:
                        got[f] = fh.read()
        self.assertGreater(peak[0], 1)
        self.assertEqual(got, {f"csi-node-{n}.txt": f"log of grab-{n}\n" for n in ("w1", "w2", "w3")})
