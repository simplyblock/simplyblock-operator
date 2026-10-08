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
import time
import unittest
from collections.abc import Callable
from datetime import UTC, datetime, timedelta

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import sbtest  # noqa: E402,F401
from sbtest.components import kube, migration, nfs, nvme  # noqa: E402
from sbtest.components.workloads import fio, pnfs_rwx, volumemigration  # noqa: E402
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
            single = volumemigration.VolumeMigrationWorkload(numjobs=1, iodepth=8)._fio_script(ctx)
            multi = volumemigration.VolumeMigrationWorkload(numjobs=4, iodepth=8)._fio_script(ctx)
        self.assertIn("--verify=md5", single)
        self.assertIn("--serialize_overlap=1", single)
        self.assertNotIn("--verify=md5", multi)

    def test_verify_is_not_fatal_by_default_so_every_bad_block_is_counted(self):
        with _Ctx() as ctx:
            s = volumemigration.VolumeMigrationWorkload(numjobs=1)._fio_script(ctx)
        self.assertIn("--verify_fatal=0", s)

    def test_the_file_stays_inside_the_volume(self):
        with _Ctx() as ctx:
            s = volumemigration.VolumeMigrationWorkload(volume_size_gb=10, file_size_gb=50)._fio_script(ctx)
        self.assertIn("--size=8G", s)   # 10 - 2 of filesystem headroom

    def test_logs_live_off_the_volume_under_test(self):
        """Collecting the evidence must not depend on the health of what it is about."""
        with _Ctx() as ctx:
            s = volumemigration.VolumeMigrationWorkload()._fio_script(ctx)
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
            w = volumemigration.VolumeMigrationWorkload()
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
            volumemigration.VolumeMigrationWorkload()._write_timeseries(
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
            volumemigration.VolumeMigrationWorkload()._write_timeseries(ctx, "default", "fio-0", d, migs)
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
            volumemigration.VolumeMigrationWorkload()._write_timeseries(ctx, "default", "fio-0", d, [])
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
            volumemigration.VolumeMigrationWorkload(pods=0, ns_pods=0)._documents(ctx)
        self.assertIn("no I/O", str(e.exception))


MOUNTSTATS = """device rootfs mounted on / with fstype rootfs
device 10.5.0.9:/other mounted on /other with fstype nfs4 statvers=1.1
\topts:\trw,vers=4.1
\tper-op statistics
\t       WRITE: 900 900 0 1 2 3 4 5 0
\t   LAYOUTGET: 0 0 0 0 0 0 0 0 0
device 10.5.0.2:/ mounted on /data with fstype nfs4 statvers=1.1
\topts:\trw,vers=4.1
\tper-op statistics
\t        READ: 2 2 0 1 2 3 4 5 0
\t       WRITE: 0 0 0 0 0 0 0 0 0
\t   LAYOUTGET: 3 3 0 600 400 1 2 3 0
\tLAYOUTCOMMIT: 1 1 0 300 200 1 1 2 0
"""


class NfsMountstats(unittest.TestCase):
    def test_counts_come_from_the_named_mount_only(self):
        """A node can carry several NFS mounts; counts belong to the header above them."""
        ops = nfs.mount_ops(MOUNTSTATS, "/data")
        self.assertEqual(ops, {"READ": 2, "WRITE": 0, "LAYOUTGET": 3, "LAYOUTCOMMIT": 1})

    def test_a_mountpoint_that_prefixes_another_does_not_match(self):
        self.assertIsNone(nfs.mount_ops(MOUNTSTATS, "/dat"))

    def test_a_missing_mount_is_none_not_zero_counts(self):
        """Zero counts would read as "pNFS did no I/O" rather than "not measured"."""
        self.assertIsNone(nfs.mount_ops(MOUNTSTATS, "/absent"))


class NvmeIostat(unittest.TestCase):
    """The client side of a pNFS write: the NVMe-oF namespace on the client worker itself."""

    def test_head_devices_are_read_and_per_path_devices_skipped(self):
        """nvmeXcYnZ is one path of a multipath namespace; the head nvmeXnY carries the
        namespace's total, so counting both would double every byte."""
        out = ("nvme0n1|0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90|"
               "  100 0 800 5 40 0 320 9 0 0 0 0 0 0 0 0 0\n"
               "nvme0c0n1|0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90|"
               "  100 0 800 5 40 0 320 9 0 0 0 0 0 0 0 0 0\n"
               "nvme1n1||  1 0 8 0 1 0 8 0 0 0 0 0 0 0 0 0 0\n")
        ts = datetime(2026, 10, 8, 9, 0, 0, tzinfo=UTC)
        got = nvme.parse_iostat("worker-1", ts, out)
        self.assertEqual(len(got), 1)
        s = got[0]
        self.assertEqual((s.node, s.device, s.uuid), ("worker-1", "nvme0n1",
                                                       "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"))
        self.assertEqual((s.read_ios, s.read_sectors, s.write_ios, s.write_sectors),
                         (100, 800, 40, 320))

    def test_samples_round_trip_through_the_archive(self):
        from sbtest.adapters import ArchiveEvidence
        ts = datetime(2026, 10, 8, 9, 0, 0, tzinfo=UTC)
        samples = nvme.parse_iostat("worker-1", ts, "nvme0n1|u1|1 0 8 0 2 0 16 0 0 0 0\n")
        with _Ctx() as ctx:
            nvme.write_iostat(ctx.path("iostat.csv"), samples)
            back = ArchiveEvidence(ctx.outdir).block_samples()
        self.assertEqual(back, samples)


class WorkloadPnfs(unittest.TestCase):
    """pNFS volumes, some shared by several pods and some private, every container running
    its own fio. Multi-reader and multi-writer on one filesystem, without the writers
    corrupting each other's verification."""

    def _plan(self, **opts: object) -> tuple[pnfs_rwx.PnfsRwxWorkload, list[dict]]:
        w = pnfs_rwx.PnfsRwxWorkload(**opts)
        with _Ctx() as ctx:
            docs = w._documents(ctx, "sc-pnfs")
        return w, docs

    @staticmethod
    def _of(docs: list[dict], kind: str) -> list[dict]:
        return [d for d in docs if d["kind"] == kind]

    def test_every_fio_instance_writes_and_verifies_its_own_file(self):
        """fio's md5 verify trusts that nothing else writes its blocks. Two instances on one
        file would report each other's writes as corruption, so every instance, across
        every pod sharing a volume, gets a file of its own and verifies only that."""
        w, docs = self._plan(shared_volumes=2, pods_per_shared=3, solo_pods=2,
                             containers_per_pod=2)
        self.assertEqual(len(w._instances), (2 * 3 + 2) * 2)
        files = [i.filename for i in w._instances]
        self.assertEqual(len(set(files)), len(files), files)
        for pod in self._of(docs, "Pod"):
            for c in pod["spec"]["containers"]:
                script = c["command"][-1]
                mine = [i for i in w._instances
                        if i.pod == pod["metadata"]["name"] and i.container == c["name"]]
                self.assertEqual(len(mine), 1)
                self.assertIn(f"--filename={mine[0].filename}", script)
                self.assertEqual(script.count("--filename="), 1)
                self.assertIn("--verify=md5", script)
                self.assertIn("--serialize_overlap=1", script)

    def test_pods_of_a_shared_volume_mount_one_rwx_claim(self):
        w, docs = self._plan(shared_volumes=2, pods_per_shared=3, solo_pods=2,
                             containers_per_pod=1)
        claims = self._of(docs, "PersistentVolumeClaim")
        self.assertEqual(len(claims), 2 + 2)
        for c in claims:
            self.assertEqual(c["spec"]["accessModes"], ["ReadWriteMany"])
            self.assertEqual(c["spec"]["storageClassName"], "sc-pnfs")
        users: dict[str, set[str]] = {}
        for pod in self._of(docs, "Pod"):
            for v in pod["spec"]["volumes"]:
                if "persistentVolumeClaim" in v:
                    users.setdefault(v["persistentVolumeClaim"]["claimName"], set()).add(
                        pod["metadata"]["name"])
        self.assertEqual(sorted(len(p) for p in users.values()), [1, 1, 3, 3])

    def test_pods_sharing_a_volume_are_spread_across_nodes(self):
        """Two writers on one node share one NFS client, so the multi-writer case is the
        multi-node one."""
        _, docs = self._plan(shared_volumes=1, pods_per_shared=2, solo_pods=1)
        shared = [p for p in self._of(docs, "Pod") if "pnfs-shared" in p["metadata"]["labels"]]
        self.assertEqual(len(shared), 2)
        for p in shared:
            terms = p["spec"]["affinity"]["podAntiAffinity"][
                "preferredDuringSchedulingIgnoredDuringExecution"]
            self.assertEqual(terms[0]["podAffinityTerm"]["topologyKey"], "kubernetes.io/hostname")
            self.assertEqual(terms[0]["podAffinityTerm"]["labelSelector"]["matchLabels"],
                             {"pnfs-shared": p["metadata"]["labels"]["pnfs-shared"]})
        _, docs = self._plan(shared_volumes=1, pods_per_shared=2, spread=False)
        for p in self._of(docs, "Pod"):
            self.assertNotIn("podAntiAffinity", p["spec"].get("affinity", {}))

    def test_the_files_must_fit_the_volume(self):
        with _Ctx() as ctx, self.assertRaises(RuntimeError) as e:
            pnfs_rwx.PnfsRwxWorkload(shared_volumes=1, pods_per_shared=3, containers_per_pod=2,
                              file_size_gb=4, volume_size_gb=20)._documents(ctx, "sc")
        self.assertIn("6 fio file(s) of 4G", str(e.exception))

    def test_a_workload_with_no_pods_is_refused(self):
        with _Ctx() as ctx, self.assertRaises(RuntimeError) as e:
            pnfs_rwx.PnfsRwxWorkload(shared_volumes=0, solo_pods=0)._documents(ctx, "sc")
        self.assertIn("no I/O", str(e.exception))

    def test_every_instance_has_evidence_the_analyser_finds(self):
        from sbtest.adapters import ArchiveEvidence
        w, _ = self._plan(shared_volumes=1, pods_per_shared=2, solo_pods=1,
                          containers_per_pod=2)
        names = [i.evidence for i in w._instances]
        self.assertEqual(len(set(names)), len(names))
        with _Ctx() as ctx:
            for n in names:
                ctx.dir(n)
            self.assertEqual(sorted(ArchiveEvidence(ctx.outdir).pods()), sorted(names))

    def test_a_named_class_that_is_not_pnfs_is_refused(self):
        """A block class would hand every pod its own ext4 or xfs device; the shared claims
        would never bind ReadWriteMany, and a solo-only run would pass without pNFS."""
        fake = _FakeKube({"get sc block": json.dumps({
            "provisioner": "csi.simplyblock.io",
            "parameters": {"csi.storage.k8s.io/fstype": "xfs"}})})
        with _Ctx() as ctx, _patch(kube, "run", fake.run), \
                self.assertRaises(RuntimeError) as e:
            pnfs_rwx.PnfsRwxWorkload(storageclass="block")._storageclass(ctx)
        self.assertIn("fstype", str(e.exception))

    def test_a_source_class_is_cloned_as_pnfs(self):
        src = {"provisioner": "csi.simplyblock.io", "reclaimPolicy": "Delete",
               "parameters": {"cluster_id": "c1", "pool_name": "p1",
                              "csi.storage.k8s.io/fstype": "xfs"}}
        fake = _FakeKube({"get sc pool-class": json.dumps(src)})
        with _Ctx() as ctx, _patch(kube, "run", fake.run):
            name = pnfs_rwx.PnfsRwxWorkload(source_storageclass="pool-class")._storageclass(ctx)
        applied = [json.loads(s) for s in fake.stdins if s]
        self.assertEqual(len(applied), 1)
        self.assertEqual(applied[0]["metadata"]["name"], name)
        self.assertEqual(applied[0]["parameters"]["csi.storage.k8s.io/fstype"], "pnfs")
        self.assertEqual(applied[0]["parameters"]["pool_name"], "p1")
        self.assertEqual(applied[0]["volumeBindingMode"], "Immediate")

    def test_the_volume_map_names_every_consumer_node(self):
        """The device check needs to know, per volume, which client nodes should be writing
        to its namespace; the scheduler decides the nodes, so they are read back."""
        from sbtest.adapters import ArchiveEvidence
        w, _ = self._plan(shared_volumes=1, pods_per_shared=2, solo_pods=1,
                          containers_per_pod=1)
        claims = sorted(set(w._claim_of.values()))
        w._lvol_of = {c: f"lvol-{c}" for c in claims}
        nodes = {p: f"node-{i}" for i, p in enumerate(sorted(w._claim_of))}
        with _Ctx() as ctx:
            w._write_volume_map(ctx, nodes)
            vols = ArchiveEvidence(ctx.outdir).pnfs_volumes()
        self.assertEqual(len(vols), 2)
        shared = next(v for v in vols if v.shared)
        self.assertEqual(len(shared.nodes), 2)
        self.assertEqual(shared.lvol, f"lvol-{shared.claim}")

    def test_collect_records_the_mounts_nfs_ops_for_every_instance(self):
        """One mount per pod, so every instance in the pod carries that mount's counts."""
        from sbtest.adapters import ArchiveEvidence

        def exec_sh(ns: str, pod: str, script: str, container: str | None = None,
                    timeout: int = 300) -> str:
            return MOUNTSTATS if "mountstats" in script else ""

        w, _ = self._plan(shared_volumes=0, solo_pods=1, containers_per_pod=2)
        with _Ctx() as ctx, _patch(kube, "exec_sh", exec_sh), \
                _patch(kube, "run", _FakeKube().run):
            w.collect(ctx)
            ev = ArchiveEvidence(ctx.outdir)
            for inst in w._instances:
                self.assertEqual(ev.nfs_ops(inst.evidence)["LAYOUTGET"], 3)


class WorkloadStop(unittest.TestCase):
    """fio writes its summary, its exit code and its time series when it exits, and verifies
    what is outstanding when its runtime ends. So a run waits for fio to finish on its own,
    as operator/test/fio_migration_test.py did, and interrupts only an instance still going
    long after its runtime, so a stuck fio cannot hang the run."""

    def _workload(self, **opts: object) -> pnfs_rwx.PnfsRwxWorkload:
        w = pnfs_rwx.PnfsRwxWorkload(shared_volumes=0, solo_pods=1, containers_per_pod=2,
                                     **opts)
        with _Ctx() as ctx:
            w._documents(ctx, "sc")
        return w

    @staticmethod
    def _exec(calls: list[tuple[str, str | None, str]],
              rc: str) -> Callable[..., str]:
        def exec_sh(ns: str, pod: str, script: str, container: str | None = None,
                    timeout: int = 300) -> str:
            calls.append((pod, container, script))
            return rc if "fio.rc" in script else ""
        return exec_sh

    def test_fio_that_finishes_on_its_own_is_not_interrupted(self):
        calls: list[tuple[str, str | None, str]] = []
        w = self._workload(runtime_s=0, stop_timeout_s=5)
        w._io_started = time.time()
        with _Ctx() as ctx, _patch(kube, "exec_sh", self._exec(calls, "0")):
            w.stop(ctx)
        self.assertFalse([s for _p, _c, s in calls if "pkill" in s],
                         "a fio that exited on its own was interrupted")
        for inst in w._instances:
            self.assertTrue(any(p == inst.pod and c == inst.container and
                                f"{inst.logdir}/fio.rc" in s for p, c, s in calls),
                            f"{inst.container} was not waited for")

    def test_only_fio_still_running_past_its_runtime_and_grace_is_interrupted(self):
        calls: list[tuple[str, str | None, str]] = []
        w = self._workload(runtime_s=0, stop_timeout_s=0)
        w._io_started = time.time() - 10
        with _Ctx() as ctx, _patch(kube, "exec_sh", self._exec(calls, "")), \
                _patch(pnfs_rwx.FioWorkload, "INTERRUPT_GRACE_S", 0):
            w.stop(ctx)   # returns rather than waiting forever
        for inst in w._instances:
            self.assertTrue(any(p == inst.pod and c == inst.container and "pkill -INT" in s
                                for p, c, s in calls), f"{inst.container} was not interrupted")


class FioRuntime(unittest.TestCase):
    """A run waits for fio to finish its runtime, so fio's runtime is the run's length. The
    CLI's duration is therefore what sets it, and a suite overrides it only on purpose."""

    def _resolved(self, duration: float | None, **opts: object) -> object:
        w = pnfs_rwx.PnfsRwxWorkload(**opts)
        with _Ctx() as ctx:
            if duration is not None:
                ctx.shared["run.duration_s"] = duration
            w._resolve_runtime(ctx)
        return w.opt("runtime_s")

    def test_the_run_duration_sets_fios_runtime(self):
        self.assertEqual(self._resolved(1800.0), 1800)

    def test_a_suite_that_sets_it_wins(self):
        self.assertEqual(self._resolved(1800.0, runtime_s=900), 900)

    def test_neither_falls_back_to_an_hour(self):
        self.assertEqual(self._resolved(None), 3600)


class WaitIOFlowing(unittest.TestCase):
    """fio's runtime counts the timed run, not the file layout before it, so the run's clock
    starts when every instance is in the timed run, as operator/test/fio_migration_test.py
    did. Counting from pods Running would end the wait early by however long layout took."""

    def test_the_timed_run_is_told_apart_from_layout(self):
        self.assertFalse(fio.in_timed_run("[pod] installing fio\nfiotest: Laying out IO file (1 file / 1024MiB)\n"))
        self.assertFalse(fio.in_timed_run("Jobs: 1 (f=1): [f(1)][100.0%][eta 00m:00s]\n"))
        self.assertTrue(fio.in_timed_run(
            "Jobs: 1 (f=1): [m(1)][0.2%][r=508KiB/s,w=196KiB/s][r=127,w=49 IOPS][eta 34m:57s]\n"))

    def test_every_instance_is_waited_for_in_its_own_container(self):
        asked: list[tuple[str, str]] = []

        def run(args: list[str], timeout: int = 60, check: bool = True,
                stdin: str | None = None) -> subprocess.CompletedProcess[str]:
            if "logs" in args:
                pod, container = args[args.index("logs") + 1], args[args.index("-c") + 1]
                asked.append((pod, container))
                return _cp("Jobs: 1 (f=1): [m(1)][1.0%][r=1,w=1 IOPS][eta 30m:00s]\n")
            return _cp("")

        w = pnfs_rwx.PnfsRwxWorkload(shared_volumes=1, pods_per_shared=2, solo_pods=0,
                                     containers_per_pod=2)
        with _Ctx() as ctx:
            w._documents(ctx, "sc")
            with _patch(kube, "run", run):
                pending = fio.wait_io_flowing(ctx, "w", "default", w._instances, 5)
        self.assertEqual(pending, [])
        self.assertEqual(sorted(asked), sorted((i.pod, i.container) for i in w._instances))

    def test_an_instance_still_laying_out_is_named_rather_than_waited_for_forever(self):
        def run(args: list[str], timeout: int = 60, check: bool = True,
                stdin: str | None = None) -> subprocess.CompletedProcess[str]:
            return _cp("fiotest: Laying out IO file (1 file / 1024MiB)\n")

        w = pnfs_rwx.PnfsRwxWorkload(shared_volumes=0, solo_pods=1, containers_per_pod=1)
        with _Ctx() as ctx:
            w._documents(ctx, "sc")
            with _patch(kube, "run", run):
                pending = fio.wait_io_flowing(ctx, "w", "default", w._instances, 0)
        self.assertEqual(pending, w._instances)


class HostDmesg(unittest.TestCase):
    def test_no_matching_pod_is_said_rather_than_collecting_nothing(self):
        from sbtest.components.logs import Dmesg
        with tempfile.TemporaryDirectory() as d:
            ctx = RunContext(run_id="run1", outdir=d, log=Logger(os.path.join(d, "test.log")))
            with _patch(kube, "list_pods", lambda ns, matching: []):
                Dmesg(namespace="simplyblock", pods_matching=["absent"]).collect(ctx)
            with open(os.path.join(d, "test.log")) as fh:
                log = fh.read()
        self.assertIn("no pods", log)
        self.assertIn("absent", log)


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
