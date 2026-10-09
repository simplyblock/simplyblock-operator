"""Unit tests for the parts of chaos.volume-ops that need no cluster.

Covered here: when the operations run, the expand and snapshot steps against a faked
kubectl, what a failed step records, and the archive record. The steps against a cluster run
only on a live run.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from typing import Any
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components import kube, volume_ops  # noqa: E402
from sbtest.core import Logger, RunContext  # noqa: E402

GI = 1024 ** 3


class VolumeOpsSchedule(unittest.TestCase):
    """Every requested operation runs once inside the window, in time order, and the same
    seed gives the same times."""

    def plan(self, **kw: Any) -> volume_ops.VolumeOpsPlan:
        args: dict[str, Any] = {"seed": 7, "start": 100.0, "end": 400.0,
                                "counts": {"expand": 2, "snapshot": 1}}
        args.update(kw)
        return volume_ops.VolumeOpsPlan(**args)

    def test_each_operation_runs_once_inside_the_window(self):
        p = self.plan()
        fired = [op for t in range(0, 500, 5) for op in p.due(float(t))]
        self.assertEqual(sorted(fired), ["expand", "expand", "snapshot"])
        self.assertTrue(all(100.0 <= t <= 400.0 for t, _ in self.plan().schedule))

    def test_nothing_is_due_before_its_time(self):
        self.assertEqual(self.plan().due(99.0), [])

    def test_the_same_seed_gives_the_same_schedule(self):
        self.assertEqual(self.plan().schedule, self.plan().schedule)
        self.assertNotEqual(self.plan().schedule, self.plan(seed=8).schedule)

    def test_a_window_with_no_room_schedules_nothing(self):
        self.assertEqual(self.plan(end=100.0).schedule, [])


class FakeCluster:
    """kubectl for one pNFS claim, a fio pod mounting it, and whatever the test changes."""

    def __init__(self) -> None:
        self.capacity = "20Gi"
        self.df = [20 * GI, 21 * GI]   # what df reports, call by call, the last repeating
        self.patch_rc = 0
        self.snapshot_classes = [{"metadata": {"name": "sb-snap"}, "driver": "csi.simplyblock.io"}]
        self.snapshot_ready = True
        self.marker = "0123456789abcdef0123456789abcdef"
        self.restored = self.marker
        self.delete_rc = 0
        self.calls: list[str] = []

    def run(self, args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
        line = " ".join(args)
        self.calls.append(line)
        out, rc = "", 0
        if "patch pvc" in line:
            rc = self.patch_rc
            if rc == 0:
                self.capacity = "21Gi"
        elif "get pvc" in line:
            out = json.dumps({"spec": {"resources": {"requests": {"storage": "20Gi"}}},
                              "status": {"capacity": {"storage": self.capacity}}})
        elif "get pods" in line:
            out = json.dumps({"items": [{
                "metadata": {"name": "r1-pnfs-0"}, "status": {"phase": "Running"},
                "spec": {"containers": [{"name": "fio-0"}], "volumes": [
                    {"name": "data", "persistentVolumeClaim": {"claimName": "r1-shared-0"}}]}}]})
        elif "get volumesnapshotclass" in line:
            out = json.dumps({"items": self.snapshot_classes})
        elif "get volumesnapshot" in line:
            out = "true" if self.snapshot_ready else "false"
        elif "get sc" in line:
            out = json.dumps({"provisioner": "csi.simplyblock.io",
                              "parameters": {"csi.storage.k8s.io/fstype": "pnfs", "pool": "p"}})
        elif "get pod" in line:
            out = "Running"
        elif "delete" in line:
            rc = self.delete_rc
        return subprocess.CompletedProcess(args, rc, out, "forbidden" if rc else "")

    def exec_sh(self, _ns: str, _pod: str, script: str, **_: object) -> str:
        self.calls.append("exec " + script)
        if "df " in script:
            return str(self.df.pop(0) if len(self.df) > 1 else self.df[0])
        if "head -c" in script:
            return self.marker
        if "md5sum" in script:
            return self.restored
        return ""


def run_op(fake: FakeCluster, op: str, **opts: Any) -> dict:
    c = volume_ops.VolumeOps(poll_s=0, expand_timeout_s=1, snapshot_timeout_s=1,
                             restore_timeout_s=1, **opts)
    with tempfile.TemporaryDirectory() as d, mock.patch.object(kube, "run", fake.run), \
            mock.patch.object(kube, "exec_sh", fake.exec_sh):
        ctx = RunContext(run_id="r1", outdir=d, log=Logger(os.path.join(d, "run.log")))
        c.bind_namespaces(ctx)
        ctx.shared["pnfs.claims"] = ["r1-shared-0"]
        ctx.shared["pnfs.storageclass"] = "sbtest-r1-pnfs"
        c._run_op(ctx, op)
        c.collect(ctx)
        ctx.log.close()
        with open(os.path.join(d, "volume-ops.json")) as fh:
            saved = json.load(fh)
    return dict(saved["ops"][0])


class Expand(unittest.TestCase):
    def test_an_expansion_records_the_claim_and_the_client_seeing_it(self):
        rec = run_op(FakeCluster(), "expand", grow_gb=1)
        self.assertEqual(rec["claim"], "r1-shared-0")
        self.assertEqual(rec["target_bytes"], 21 * GI)
        self.assertIsNotNone(rec["capacity_at"])
        self.assertIsNotNone(rec["client_seen_at"])
        self.assertEqual(rec["client_pod"], "r1-pnfs-0")
        self.assertEqual((rec["client_before_b"], rec["client_after_b"]), (20 * GI, 21 * GI))
        self.assertEqual(rec["error"], "")

    def test_a_client_that_never_sees_the_size_is_recorded_as_such(self):
        fake = FakeCluster()
        fake.df = [20 * GI]
        rec = run_op(fake, "expand", grow_gb=1)
        self.assertIsNotNone(rec["capacity_at"])
        self.assertIsNone(rec["client_seen_at"])

    def test_a_refused_patch_is_an_error_and_nothing_after_it(self):
        fake = FakeCluster()
        fake.patch_rc = 1
        rec = run_op(fake, "expand", grow_gb=1)
        self.assertIn("forbidden", rec["error"])
        self.assertIsNone(rec["capacity_at"])


class Snapshot(unittest.TestCase):
    def test_a_snapshot_is_taken_restored_read_back_and_removed(self):
        rec = run_op(FakeCluster(), "snapshot")
        self.assertIsNotNone(rec["ready_at"])
        self.assertEqual(rec["marker_md5"], rec["restore_md5"])
        self.assertIsNotNone(rec["restored_at"])
        self.assertIsNotNone(rec["snapshot_deleted"])
        self.assertIsNotNone(rec["restore_deleted"])
        self.assertEqual(rec["error"], "")

    def test_without_a_snapshot_class_for_the_driver_nothing_is_attempted(self):
        fake = FakeCluster()
        fake.snapshot_classes = [{"metadata": {"name": "other"}, "driver": "other.csi"}]
        rec = run_op(fake, "snapshot")
        self.assertIn("VolumeSnapshotClass", rec["skipped"])
        self.assertFalse(any("volumesnapshot" in c and "apply" in c for c in fake.calls))

    def test_a_snapshot_that_never_becomes_ready_is_not_restored(self):
        fake = FakeCluster()
        fake.snapshot_ready = False
        rec = run_op(fake, "snapshot")
        self.assertIsNone(rec["ready_at"])
        self.assertIsNone(rec["restored_at"])

    def test_a_restore_reading_other_data_is_recorded_with_both_checksums(self):
        fake = FakeCluster()
        fake.restored = "ffffffffffffffffffffffffffffffff"
        rec = run_op(fake, "snapshot")
        self.assertNotEqual(rec["marker_md5"], rec["restore_md5"])

    def test_a_failed_delete_is_not_recorded_as_a_deletion(self):
        fake = FakeCluster()
        fake.delete_rc = 1
        rec = run_op(fake, "snapshot")
        self.assertIsNone(rec["snapshot_deleted"])
        self.assertIn("forbidden", rec["error"])


class VolumeOpsArchive(unittest.TestCase):
    def test_reads_the_operations_and_tolerates_their_absence(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(ArchiveEvidence(d).volume_ops(), [])
            with open(os.path.join(d, "volume-ops.json"), "w") as fh:
                json.dump({"seed": 1, "ops": [
                    {"op": "snapshot", "claim": "c", "requested": "2026-10-09T06:10:00Z",
                     "ready_at": "2026-10-09T06:10:20Z", "marker_md5": "a",
                     "restore_md5": "a", "skipped": "", "error": ""},
                    {"op": "expand", "claim": "c", "requested": "2026-10-09T06:05:00Z",
                     "target_bytes": 5, "capacity_at": None, "error": "x"}]}, fh)
            ops = ArchiveEvidence(d).volume_ops()
        self.assertEqual([o.op for o in ops], ["expand", "snapshot"])
        self.assertEqual(ops[1].ready_at, datetime(2026, 10, 9, 6, 10, 20, tzinfo=UTC))
        self.assertIsNone(ops[0].capacity_at)
        self.assertEqual(ops[0].target_bytes, 5)


if __name__ == "__main__":
    unittest.main()
