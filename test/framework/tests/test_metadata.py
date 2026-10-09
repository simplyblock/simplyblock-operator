"""Unit tests for workload.pnfs-metadata's parts that need no cluster.

The agent is a standalone script the component ships into its pods, so it is run here as it
runs there, against a temporary directory: the worker for a couple of seconds, and the lister
over what it left. The manifest-against-listing comparison and the archive reader are pure.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from datetime import UTC, datetime

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components.workloads import metadata, metadata_agent  # noqa: E402

AGENT = metadata_agent.__file__


def run_worker(root: str, logdir: str, *, seconds: float = 2.0, seed: int = 7,
               extra: list[str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, AGENT, "worker", "--root", root, "--logdir", logdir,
         "--seed", str(seed), "--rate", "200", "--duration", str(seconds),
         "--manifest-interval", "0.5", *(extra or [])],
        capture_output=True, text=True, timeout=60, check=False)


def listing(root: str) -> dict:
    cp = subprocess.run([sys.executable, AGENT, "list", "--root", root],
                        capture_output=True, text=True, timeout=60, check=True)
    got: dict = json.loads(cp.stdout)
    return got


def manifest(logdir: str) -> dict:
    with open(os.path.join(logdir, "manifest.json")) as fh:
        got: dict = json.load(fh)
    return got


class Worker(unittest.TestCase):
    """The worker's own account of what it did must match what is on disk afterward."""

    def test_it_logs_every_op_and_leaves_a_manifest_the_disk_matches(self):
        with tempfile.TemporaryDirectory() as d:
            root, logdir = os.path.join(d, "data"), os.path.join(d, "logs")
            cp = run_worker(root, logdir)
            self.assertEqual(cp.returncode, 0, cp.stderr)
            with open(os.path.join(logdir, "ops.log")) as fh:
                ops = [json.loads(line) for line in fh if line.strip()]
            self.assertGreater(len(ops), 50)
            self.assertTrue({"create", "rename", "unlink", "mkdir", "stat"} <= {o["op"] for o in ops})
            for o in ops:
                self.assertEqual(set(o), {"t", "op", "path", "ok", "err", "ms"})
            self.assertTrue(os.path.exists(os.path.join(logdir, "stopped")))
            diff = metadata.compare(manifest(logdir), listing(root))
            self.assertTrue(diff.clean, diff)

    def test_the_same_seed_replays_the_same_ops(self):
        with tempfile.TemporaryDirectory() as d:
            seqs = []
            for run in ("a", "b"):
                logdir = os.path.join(d, run, "logs")
                run_worker(os.path.join(d, run, "data"), logdir, seconds=1.0,
                           extra=["--max-ops", "100"])
                with open(os.path.join(logdir, "ops.log")) as fh:
                    seqs.append([(o["op"], o["path"]) for o in map(json.loads, fh)])
            self.assertEqual(seqs[0], seqs[1])

    def test_a_pause_request_is_answered_with_a_fresh_manifest(self):
        """The verifier compares against a manifest the worker wrote while holding still,
        so nothing it lists can have changed since."""
        with tempfile.TemporaryDirectory() as d:
            root, logdir = os.path.join(d, "data"), os.path.join(d, "logs")
            proc = subprocess.Popen(
                [sys.executable, AGENT, "worker", "--root", root, "--logdir", logdir,
                 "--seed", "3", "--rate", "200", "--duration", "30",
                 "--manifest-interval", "60"])
            try:
                time.sleep(1.0)
                open(os.path.join(logdir, "pause"), "w").close()
                deadline = time.monotonic() + 10
                while not os.path.exists(os.path.join(logdir, "paused")):
                    self.assertLess(time.monotonic(), deadline, "never paused")
                    time.sleep(0.05)
                diff = metadata.compare(manifest(logdir), listing(root))
                self.assertTrue(diff.clean, diff)
                os.remove(os.path.join(logdir, "pause"))
                deadline = time.monotonic() + 10
                while os.path.exists(os.path.join(logdir, "paused")):
                    self.assertLess(time.monotonic(), deadline, "never resumed")
                    time.sleep(0.05)
            finally:
                open(os.path.join(logdir, "stop"), "w").close()
                proc.wait(timeout=30)


class Compare(unittest.TestCase):
    def files(self, **kw: tuple[int, str]) -> dict:
        return {k: {"size": s, "md5": m} for k, (s, m) in kw.items()}

    def test_missing_extra_and_changed_are_each_reported(self):
        man = {"files": self.files(a=(1, "x"), b=(2, "y"), c=(3, "z")),
               "dirs": ["d1", "d2"], "uncertain": []}
        got = {"files": self.files(a=(1, "x"), c=(3, "Z"), e=(4, "w")), "dirs": ["d1", "d3"]}
        diff = metadata.compare(man, got)
        self.assertFalse(diff.clean)
        self.assertEqual(diff.missing, ("b", "d2"))
        self.assertEqual(diff.extra, ("d3", "e"))
        self.assertEqual(diff.mismatched, ("c",))

    def test_a_path_whose_last_op_failed_is_not_judged(self):
        """An op that failed during an outage may or may not have taken effect, and only
        the error says anything about it."""
        man = {"files": self.files(a=(1, "x")), "dirs": ["d"], "uncertain": ["b", "d/c", "u"]}
        got = {"files": self.files(a=(1, "x"), b=(5, "q"), **{"u/f": (1, "m")}),
               "dirs": ["d", "u"]}
        self.assertTrue(metadata.compare(man, got).clean)


class Assign(unittest.TestCase):
    """Where each worker runs, on which volume, and which node checks it."""

    def test_one_worker_per_node_each_checked_from_another_node(self):
        got = metadata.assign(["w1", "w2", "w3"], ["c0", "c1"], workers=0)
        self.assertEqual([(a.node, a.claim, a.verifier_node) for a in got],
                         [("w1", "c0", "w2"), ("w2", "c1", "w3"), ("w3", "c0", "w1")])

    def test_one_node_checks_itself_and_says_so(self):
        got = metadata.assign(["w1"], ["c0"], workers=2)
        self.assertEqual([(a.node, a.verifier_node) for a in got], [("w1", "w1"), ("w1", "w1")])

    def test_nothing_to_assign_without_nodes_or_claims(self):
        self.assertEqual(metadata.assign([], ["c0"], workers=0), [])
        self.assertEqual(metadata.assign(["w1"], [], workers=0), [])


class MetadataArchive(unittest.TestCase):
    def test_reads_the_ops_and_the_checks(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "metadata.json"), "w") as fh:
                json.dump({"seed": 1, "workers": [
                    {"worker": "r-meta-0", "node": "w1", "claim": "c0",
                     "verifier": "r-metaverify-0", "verifier_node": "w2"}],
                    "checks": [{"ts": "2026-10-09T10:00:00Z", "worker": "r-meta-0",
                                "worker_node": "w1", "verifier_node": "w2",
                                "missing": ["a"], "extra": [], "mismatched": [],
                                "error": ""}]}, fh)
            with open(os.path.join(d, "metadata-r-meta-0.log"), "w") as fh:
                fh.write(json.dumps({"t": 1791540000.5, "op": "create", "path": "a", "ok": True,
                                     "err": "", "ms": 2.5}) + "\n")
                fh.write(json.dumps({"t": 1791540001.0, "op": "unlink", "path": "a",
                                     "ok": False, "err": "EIO", "ms": 30000.0}) + "\n")
                fh.write("not json\n")
            ev = ArchiveEvidence(d)
            ops = ev.metadata_ops()
            self.assertEqual([(o.worker, o.op, o.ok, o.error) for o in ops],
                             [("r-meta-0", "create", True, ""),
                              ("r-meta-0", "unlink", False, "EIO")])
            self.assertEqual(ops[0].ts, datetime.fromtimestamp(1791540000.5, tz=UTC))
            checks = ev.metadata_checks()
            self.assertEqual(len(checks), 1)
            self.assertEqual(checks[0].missing, ("a",))
            self.assertTrue(checks[0].cross_node)

    def test_a_run_without_the_workload_has_none(self):
        with tempfile.TemporaryDirectory() as d:
            ev = ArchiveEvidence(d)
            self.assertEqual(ev.metadata_ops(), [])
            self.assertEqual(ev.metadata_checks(), [])


if __name__ == "__main__":
    unittest.main()
