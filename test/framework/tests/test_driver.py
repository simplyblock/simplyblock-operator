"""Driver and workload tests.

These cover the decisions the driver makes on its own — which node to migrate to, which
volumes move together, what a non-terminal poll means — because those are the parts that
silently produce a *valid-looking* run when they are wrong. A driver that always picks the
same target still completes migrations and still reports PASS; the run just never exercised
the case it claimed to.

Everything here fakes kubectl at the module boundary. That is the whole point of routing
cluster access through one `kube.run`: the logic above it stays testable without a cluster.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime, timedelta

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import sbtest  # noqa: E402,F401
from sbtest.components import kube, migration, workload  # noqa: E402
from sbtest.core import Logger, Migration, RunContext  # noqa: E402


def _cp(stdout: str = "", rc: int = 0) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(args=["kubectl"], returncode=rc, stdout=stdout,
                                       stderr="")


def _nodes_json(*specs: tuple[str, str, str]) -> str:
    """specs: (uuid, k8s host, status)."""
    return json.dumps({"items": [
        {"spec": {"workerNode": host}, "status": {"uuid": uuid, "status": status}}
        for uuid, host, status in specs]})


class _FakeKube:
    """Records every kubectl invocation and answers from a scripted table."""

    def __init__(self, answers: dict[str, str] | None = None) -> None:
        self.calls: list[list[str]] = []
        self.stdins: list[str | None] = []
        self.answers = answers or {}

    def run(self, args: list[str], timeout: int = 60, check: bool = True,
            stdin: str | None = None) -> subprocess.CompletedProcess[str]:
        self.calls.append(list(args))
        self.stdins.append(stdin)
        for key, out in self.answers.items():
            if key in " ".join(args):
                return _cp(out)
        return _cp("")


class _Ctx:
    """A RunContext with a real temp dir, for components that write artifacts."""

    def __enter__(self) -> RunContext:
        self._d = tempfile.TemporaryDirectory()
        self.ctx = RunContext(run_id="run1", outdir=self._d.__enter__(), log=Logger(None))
        return self.ctx

    def __exit__(self, *exc: object) -> None:
        self._d.__exit__(*exc)  # type: ignore[arg-type]


class TargetPolicy(unittest.TestCase):
    """The policy decides whether the target also hosts a consumer — the harder case."""

    def _driver(self, ctx: RunContext, policy: str,
                consumers: dict[str, list[str]]) -> migration.MigrationDriver:
        d = migration.MigrationDriver(target_policy=policy)
        d._nodes = ["uuid-a", "uuid-b", "uuid-c"]
        d._node_host = {"uuid-a": "vm02", "uuid-b": "vm03", "uuid-c": "vm04"}
        # pv -> pod -> node, which is how the driver learns where consumers run.
        ctx.shared["workload.pod_of"] = {"pv1": "fio-0"}
        ctx.shared["workload.node_of"] = {"fio-0": next(iter(consumers), "")}
        return d

    def test_consumer_policy_picks_a_node_running_a_consumer(self):
        with _Ctx() as ctx:
            d = self._driver(ctx, "consumer", {"vm03": ["fio-0"]})
            target, policy, named = d._pick_target(ctx, ["pv1"], idx=1, source="uuid-a")
            self.assertEqual(target, "uuid-b")     # vm03, where fio-0 runs
            self.assertEqual(policy, "consumer")
            self.assertEqual(named, ["fio-0"])

    def test_no_consumer_policy_avoids_it(self):
        with _Ctx() as ctx:
            d = self._driver(ctx, "no-consumer", {"vm03": ["fio-0"]})
            target, policy, named = d._pick_target(ctx, ["pv1"], idx=1, source="uuid-a")
            self.assertEqual(target, "uuid-c")     # vm04: the only other non-consumer
            self.assertEqual(policy, "no-consumer")
            self.assertEqual(named, [])

    def test_alternate_starts_with_the_harder_case(self):
        """Odd migrations get `consumer`, so a run cut short still exercised it."""
        with _Ctx() as ctx:
            d = self._driver(ctx, "alternate", {"vm03": ["fio-0"]})
            _, first, _ = d._pick_target(ctx, ["pv1"], idx=1, source="uuid-a")
            _, second, _ = d._pick_target(ctx, ["pv1"], idx=2, source="uuid-a")
            self.assertEqual(first, "consumer")
            self.assertEqual(second, "no-consumer")

    def test_unmet_policy_falls_back_and_says_so(self):
        """A migration under the other condition is still evidence; skipping it is not.

        The recorded policy has to show it was unmet, or the run's own record claims a case
        it never exercised.
        """
        with _Ctx() as ctx:
            d = self._driver(ctx, "consumer", {})   # no consumer anywhere
            target, policy, _ = d._pick_target(ctx, ["pv1"], idx=1, source="uuid-a")
            self.assertIn(target, ("uuid-b", "uuid-c"))
            self.assertEqual(policy, "consumer(unmet)")

    def test_the_source_is_never_the_target(self):
        with _Ctx() as ctx:
            d = self._driver(ctx, "random", {})
            for _ in range(20):
                target, _, _ = d._pick_target(ctx, ["pv1"], idx=1, source="uuid-a")
                self.assertNotEqual(target, "uuid-a")

    def test_consumers_are_counted_across_the_whole_subsystem(self):
        """Every pod holding any namespace of the subsystem has its paths moved."""
        with _Ctx() as ctx:
            d = migration.MigrationDriver(target_policy="consumer")
            d._nodes = ["uuid-a", "uuid-b"]
            d._node_host = {"uuid-a": "vm02", "uuid-b": "vm03"}
            ctx.shared["workload.pod_of"] = {"pv1": "fio-0", "pv2": "fio-1"}
            ctx.shared["workload.node_of"] = {"fio-0": "vm02", "fio-1": "vm03"}
            _, _, named = d._pick_target(ctx, ["pv1", "pv2"], idx=1, source="uuid-a")
            self.assertEqual(named, ["fio-1"])   # the sibling on the target counts


class Grouping(unittest.TestCase):
    """A migration moves the whole subsystem, so the group is what must be tracked."""

    def test_group_is_the_shared_subsystem(self):
        d = migration.MigrationDriver()
        d._nqn_of = {"pv1": "nqn.a", "pv2": "nqn.a", "pv3": "nqn.b"}
        d._groups = d._regroup()
        self.assertEqual(d._group_of("pv1"), ["pv1", "pv2"])
        self.assertEqual(d._group_of("pv3"), ["pv3"])

    def test_an_unknown_subsystem_migrates_alone(self):
        d = migration.MigrationDriver()
        self.assertEqual(d._group_of("pv9"), ["pv9"])

    def test_a_changed_subsystem_regroups(self):
        """A previous migration can repack the subsystems; a stale group samples the wrong
        nodes and verifies the wrong volumes."""
        class FakeSb:
            def subsystem_of(self, lvol: str) -> tuple[str, int]:
                return "nqn.new", 1

        with _Ctx() as ctx:
            d = migration.MigrationDriver()
            d._sb = FakeSb()  # type: ignore[assignment]
            d._volume_of = {"pv1": "lvol-1"}
            d._nqn_of = {"pv1": "nqn.old", "pv2": "nqn.old"}
            d._groups = d._regroup()
            self.assertEqual(d._group_of("pv1"), ["pv1", "pv2"])
            d._reread_subsystem(ctx, "pv1")
            self.assertEqual(d._nqn_of["pv1"], "nqn.new")
            self.assertEqual(d._group_of("pv1"), ["pv1"])   # no longer with pv2


class PollLoop(unittest.TestCase):
    def test_terminal_phase_ends_the_wait(self):
        fake = _FakeKube({"get volumemigration": json.dumps(
            {"status": {"phase": "Completed", "sourceNodeUUID": "uuid-real"}})})
        with _Ctx() as ctx, _patch(kube, "run", fake.run):
            d = migration.MigrationDriver(poll_s=0.01)
            rec = Migration(name="m1", start=datetime.now(UTC), pv="pv1", source="uuid-guess")
            d._await_terminal(ctx, rec, "m1", sampler=None)
        self.assertEqual(rec.phase, "Completed")
        self.assertIsNotNone(rec.end)
        # The operator's resolved source overrides the driver's guess.
        self.assertEqual(rec.source, "uuid-real")

    def test_never_reaching_a_terminal_phase_is_a_timeout_not_a_failure(self):
        """A rejected migration and one that never finished are different defects, and only
        one of them has an error to read."""
        fake = _FakeKube({"get volumemigration": json.dumps(
            {"status": {"phase": "Migrating"}})})
        with _Ctx() as ctx, _patch(kube, "run", fake.run):
            d = migration.MigrationDriver(poll_s=0.01, timeout_s=0.05)
            rec = Migration(name="m1", start=datetime.now(UTC), pv="pv1")
            d._await_terminal(ctx, rec, "m1", sampler=None)
        self.assertEqual(rec.phase, "TIMEOUT")
        self.assertEqual(rec.error, "")

    def test_phase_changes_reach_the_sampler_and_the_timeline(self):
        class Sampler:
            def __init__(self) -> None:
                self.phases: list[str] = []

            def set_phase(self, p: str) -> None:
                self.phases.append(p)

        seq = [json.dumps({"status": {"phase": p}})
               for p in ("Preparing", "Preparing", "Cutover", "Completed")]

        def run(args: list[str], timeout: int = 60, check: bool = True,
                stdin: str | None = None) -> subprocess.CompletedProcess[str]:
            return _cp(seq.pop(0) if seq else "")

        s = Sampler()
        with _Ctx() as ctx, _patch(kube, "run", run):
            d = migration.MigrationDriver(poll_s=0.01)
            rec = Migration(name="m1", start=datetime.now(UTC), pv="pv1")
            d._await_terminal(ctx, rec, "m1", sampler=s)
        # Deduplicated: only transitions, not every poll.
        self.assertEqual(s.phases, ["Preparing", "Cutover", "Completed"])
        self.assertEqual([e.data["phase"] for e in ctx.timeline.of_kind("migration.phase")],
                         ["Preparing", "Cutover", "Completed"])


class Manifest(unittest.TestCase):
    def test_the_cr_carries_the_run_label_so_teardown_can_find_it(self):
        d = migration.MigrationDriver(namespace="default")
        m = json.loads(d._manifest("run1-mig-3", "pvc-abc", "uuid-b"))
        self.assertEqual(m["kind"], "VolumeMigration")
        self.assertEqual(m["spec"], {"pvName": "pvc-abc", "targetNodeUUID": "uuid-b"})
        self.assertEqual(m["metadata"]["labels"]["sbtest-run"], "true")

    def test_online_nodes_only(self):
        """Migrating to an offline node is a rejected request, not a test."""
        fake = _FakeKube({"get storagenodes": _nodes_json(
            ("uuid-a", "vm02", "online"), ("uuid-b", "vm03", "offline"),
            ("uuid-c", "vm04", "online"))})
        with _patch(kube, "run", fake.run):
            uuids, hosts = migration.MigrationDriver()._storage_nodes()
        self.assertEqual(uuids, ["uuid-a", "uuid-c"])
        self.assertEqual(hosts["uuid-c"], "vm04")


class SetupGuards(unittest.TestCase):
    def test_one_node_cannot_be_migrated_between(self):
        fake = _FakeKube({"get storagenodes": _nodes_json(("uuid-a", "vm02", "online"))})
        with _Ctx() as ctx, _patch(kube, "run", fake.run), \
                self.assertRaises(RuntimeError) as e:
            migration.MigrationDriver().setup(ctx)
        self.assertIn("at least two", str(e.exception))

    def test_no_volumes_is_an_explicit_error_not_an_empty_run(self):
        """Silently migrating nothing would report PASS for a test that never ran."""
        fake = _FakeKube({"get storagenodes": _nodes_json(
            ("uuid-a", "vm02", "online"), ("uuid-b", "vm03", "online"))})
        with _Ctx() as ctx, _patch(kube, "run", fake.run), \
                self.assertRaises(RuntimeError) as e:
            migration.MigrationDriver().setup(ctx)
        self.assertIn("no volumes to migrate", str(e.exception))


class Persistence(unittest.TestCase):
    def test_migrations_round_trip_through_the_file(self):
        """What the driver writes is what the analyser reads — the seam that lets a run be
        re-judged later against a detector that did not exist when it ran."""
        with _Ctx() as ctx:
            d = migration.MigrationDriver()
            start = datetime.now(UTC).replace(microsecond=0)
            d._records = [Migration(name="run1-mig-1", start=start,
                                    end=start + timedelta(seconds=42), phase="Completed",
                                    source="uuid-a", target="uuid-b", pv="pv1", pod="fio-0",
                                    members=["pv1", "pv2"]),
                          Migration(name="run1-mig-2", start=start + timedelta(minutes=1),
                                    phase="TIMEOUT", pv="pv3")]
            d._nqn_of = {"pv1": "nqn.a"}
            d.collect(ctx)
            back = migration.migrations_from_file(os.path.join(ctx.outdir,
                                                               "migrations.json"))
        self.assertEqual([m.name for m in back], ["run1-mig-1", "run1-mig-2"])
        self.assertEqual(back[0].members, ["pv1", "pv2"])
        self.assertTrue(back[0].batch)
        self.assertEqual(back[0].phase, "Completed")
        self.assertIsNotNone(back[0].end)
        assert back[0].end is not None
        self.assertEqual((back[0].end - back[0].start).total_seconds(), 42)
        self.assertIsNone(back[1].end)

    def test_a_record_without_a_start_is_dropped_rather_than_guessed(self):
        with _Ctx() as ctx:
            p = os.path.join(ctx.outdir, "migrations.json")
            with open(p, "w") as fh:
                json.dump([{"name": "broken", "phase": "Completed"}], fh)
            self.assertEqual(migration.migrations_from_file(p), [])


class WorkloadFio(unittest.TestCase):
    def test_verification_is_off_when_it_cannot_be_trusted(self):
        """numjobs>1 cannot serialize overlapping writes, so verify would report corruption
        that never happened. A throughput run must not look like an integrity run."""
        with _Ctx() as ctx:
            single = workload.FioWorkload(numjobs=1, iodepth=8)._fio_script(ctx)
            multi = workload.FioWorkload(numjobs=4, iodepth=8)._fio_script(ctx)
        self.assertIn("--verify=md5", single)
        self.assertIn("--serialize_overlap=1", single)
        self.assertNotIn("--verify=md5", multi)

    def test_verify_is_not_fatal_by_default_so_every_bad_block_is_counted(self):
        with _Ctx() as ctx:
            s = workload.FioWorkload(numjobs=1)._fio_script(ctx)
        self.assertIn("--verify_fatal=0", s)

    def test_the_file_stays_inside_the_volume(self):
        with _Ctx() as ctx:
            s = workload.FioWorkload(volume_size_gb=10, file_size_gb=50)._fio_script(ctx)
        self.assertIn("--size=8G", s)   # 10 - 2 of filesystem headroom

    def test_logs_live_off_the_volume_under_test(self):
        """Collecting the evidence must not depend on the health of what it is about."""
        with _Ctx() as ctx:
            s = workload.FioWorkload()._fio_script(ctx)
        self.assertIn("--output=/logs/result.json", s)
        self.assertIn("--filename=/data/fiotest", s)

    def test_timeseries_is_written_where_the_analyser_reads_it(self):
        raw = "\n".join([
            "1000, 500, 0, 4096",     # 1s: 500 read
            "1000, 100, 1, 4096",     # 1s: 100 write
            "2000, 400, 0, 4096",
        ])
        start = datetime(2026, 8, 20, 9, 0, 0, tzinfo=UTC)
        migs = [Migration(name="mig-1", start=start + timedelta(seconds=1),
                          end=start + timedelta(seconds=1), pv="pv1")]
        with _Ctx() as ctx, _patch(kube, "exec_sh", lambda *a, **k: raw):
            ctx.mark_window(start=start)
            w = workload.FioWorkload()
            d = ctx.dir("fio-0")
            w._write_timeseries(ctx, "default", "fio-0", d, migs)
            with open(os.path.join(d, "timeseries.csv")) as fh:
                rows = list(__import__("csv").DictReader(fh))
        self.assertEqual(rows[0]["second"], "1")
        self.assertEqual(float(rows[0]["total_iops"]), 600.0)
        self.assertEqual(rows[0]["wall_clock"], "2026-08-20T09:00:01Z")
        # The migration column is the point: it makes a dip attributable.
        self.assertEqual(rows[0]["active_migration"], "mig-1")
        self.assertEqual(rows[1]["active_migration"], "")

    def test_the_analyser_reads_back_what_the_workload_wrote(self):
        """Guards the column-name seam. Reading only fio's own names silently placed every
        sample at offset 0 — the series stayed the right length with the whole run collapsed
        onto one instant, so nothing looked like a parse failure."""
        from sbtest.adapters import ArchiveEvidence
        raw = "1000, 500, 0, 4096\n2000, 400, 0, 4096"
        with _Ctx() as ctx, _patch(kube, "exec_sh", lambda *a, **k: raw):
            ctx.mark_window(start=datetime(2026, 8, 20, 9, 0, 0, tzinfo=UTC))
            workload.FioWorkload()._write_timeseries(
                ctx, "default", "run1-fio-0", ctx.dir("run1-fio-0"), [])
            series = ArchiveEvidence(ctx.outdir).fio_timeseries("run1-fio-0")
        self.assertEqual([s.offset_s for s in series], [1, 2])
        self.assertEqual([s.total_iops for s in series], [500.0, 400.0])
        self.assertIsNotNone(series[0].wall)

    def test_the_clock_comes_from_fio_not_from_the_run(self):
        """fio counts from its own launch, which is minutes after the run's start: the PVCs,
        the pods and the fio processes all have to exist first. Basing the wall clock on the
        run shifts every sample by that gap and hands an outage to the wrong migration."""
        raw = "1000, 500, 0, 4096"
        run_start = datetime(2026, 8, 20, 9, 0, 0, tzinfo=UTC)
        fio_start = run_start + timedelta(seconds=180)
        # The migration ran while fio was running, i.e. nowhere near the run's own start.
        migs = [Migration(name="mig-1", start=fio_start, end=fio_start + timedelta(seconds=30))]
        with _Ctx() as ctx, _patch(kube, "exec_sh", lambda *a, **k: raw):
            ctx.mark_window(start=run_start)
            d = ctx.dir("fio-0")
            with open(os.path.join(d, "result.json"), "w") as fh:
                json.dump({"jobs": [{"job_start": int(fio_start.timestamp() * 1000)}]}, fh)
            workload.FioWorkload()._write_timeseries(ctx, "default", "fio-0", d, migs)
            with open(os.path.join(d, "timeseries.csv")) as fh:
                rows = list(__import__("csv").DictReader(fh))
        self.assertEqual(rows[0]["wall_clock"], "2026-08-20T09:03:01Z")
        self.assertEqual(rows[0]["active_migration"], "mig-1")

    def test_a_result_without_job_start_falls_back_to_the_run(self):
        """A wrong base still beats an empty wall_clock column — but it is reported."""
        raw = "1000, 500, 0, 4096"
        with _Ctx() as ctx, _patch(kube, "exec_sh", lambda *a, **k: raw):
            ctx.mark_window(start=datetime(2026, 8, 20, 9, 0, 0, tzinfo=UTC))
            d = ctx.dir("fio-0")
            with open(os.path.join(d, "result.json"), "w") as fh:
                json.dump({"jobs": [{}]}, fh)
            workload.FioWorkload()._write_timeseries(ctx, "default", "fio-0", d, [])
            with open(os.path.join(d, "timeseries.csv")) as fh:
                rows = list(__import__("csv").DictReader(fh))
        self.assertEqual(rows[0]["wall_clock"], "2026-08-20T09:00:01Z")

    def test_the_analyser_re_derives_the_base_from_fio(self):
        """Replay has to correct archives written before the base was fixed: the wall_clock
        column is only as right as whatever wrote it, and job_start is right by
        construction."""
        from sbtest.adapters import ArchiveEvidence
        fio_start = datetime(2026, 8, 20, 9, 3, 0, tzinfo=UTC)
        with _Ctx() as ctx:
            d = ctx.dir("run1-fio-0")
            with open(os.path.join(d, "result.json"), "w") as fh:
                json.dump({"jobs": [{"job_start": int(fio_start.timestamp() * 1000)}]}, fh)
            with open(os.path.join(d, "timeseries.csv"), "w") as fh:
                fh.write("second,wall_clock,total_iops\n"
                         "1,2026-08-20T09:00:01Z,500.0\n")   # the pre-fix, run-based clock
            series = ArchiveEvidence(ctx.outdir).fio_timeseries("run1-fio-0")
        self.assertEqual(series[0].wall, fio_start + timedelta(seconds=1))

    def test_the_wall_clock_column_is_used_when_fio_says_nothing(self):
        from sbtest.adapters import ArchiveEvidence
        with _Ctx() as ctx:
            d = ctx.dir("run1-fio-0")
            with open(os.path.join(d, "timeseries.csv"), "w") as fh:
                fh.write("second,wall_clock,total_iops\n1,2026-08-20T09:00:01Z,500.0\n")
            series = ArchiveEvidence(ctx.outdir).fio_timeseries("run1-fio-0")
        self.assertEqual(series[0].wall, datetime(2026, 8, 20, 9, 0, 1, tzinfo=UTC))

    def test_a_workload_with_no_pods_is_refused(self):
        with _Ctx() as ctx, self.assertRaises(RuntimeError) as e:
            workload.FioWorkload(pods=0, ns_pods=0)._create(ctx)
        self.assertIn("no I/O", str(e.exception))


class _patch:
    """Minimal attribute patcher — the stdlib one needs a dotted target string."""

    def __init__(self, obj: object, attr: str, value: object) -> None:
        self.obj, self.attr, self.value = obj, attr, value

    def __enter__(self) -> object:
        self.old = getattr(self.obj, self.attr)
        setattr(self.obj, self.attr, self.value)
        return self.value

    def __exit__(self, *exc: object) -> None:
        setattr(self.obj, self.attr, self.old)


if __name__ == "__main__":
    unittest.main()
