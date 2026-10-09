"""Unit tests for the parts of chaos.fence that need no cluster.

Covered here: when the scenario starts, which nodes and claim it uses, the partition rules,
how a direct write path is recognized, the probe writer's log, the archive record, and a
teardown that removes exactly the rules the scenario inserted. The steps against a cluster
run only on a live run.
"""

from __future__ import annotations

import json
import os
import random
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components import fence, kube  # noqa: E402
from sbtest.core import Logger, RunContext  # noqa: E402


class FenceStart(unittest.TestCase):
    """Once per run, inside the timed run, with room left for the whole scenario."""

    def start(self, seed: int = 7, duration: float = 1800.0) -> float | None:
        return fence.fence_start(seed=seed, t0=0.0, duration=duration, min_delay_s=60.0,
                                 quiet_tail_s=120.0, budget_s=420.0)

    def test_it_starts_after_the_delay_and_ends_before_the_quiet_tail(self):
        for seed in range(50):
            t = self.start(seed)
            assert t is not None
            self.assertGreaterEqual(t, 60.0)
            self.assertLessEqual(t + 420.0, 1800.0 - 120.0)

    def test_the_same_seed_gives_the_same_start(self):
        self.assertEqual(self.start(3), self.start(3))
        self.assertNotEqual(self.start(3), self.start(4))

    def test_a_run_too_short_for_the_scenario_has_no_start(self):
        self.assertIsNone(self.start(duration=500.0))


class FencePick(unittest.TestCase):
    """The victim loses the metadata server for minutes, and every pNFS mount on it with it,
    so a node where no workload pod mounts a volume is preferred."""

    NODES = ["w1", "w2", "w3", "w4"]

    def test_an_idle_victim_and_a_client_recaller(self):
        got = fence.pick(random.Random(1), nodes=self.NODES, client_nodes=["w1", "w2"],
                         claims=["c0"], allow_busy_victim=False)
        assert got is not None
        victim, recaller, claim = got
        self.assertIn(victim, {"w3", "w4"})
        self.assertIn(recaller, set(self.NODES) - {victim})
        self.assertEqual(claim, "c0")

    def test_the_same_seed_picks_the_same(self):
        args = {"nodes": self.NODES, "client_nodes": ["w1"], "claims": ["c0", "c1"],
                "allow_busy_victim": False}
        self.assertEqual(fence.pick(random.Random(5), **args),  # type: ignore[arg-type]
                         fence.pick(random.Random(5), **args))  # type: ignore[arg-type]

    def test_no_idle_node_means_no_fence_unless_a_busy_victim_is_allowed(self):
        nodes = ["w1", "w2"]
        self.assertIsNone(fence.pick(random.Random(1), nodes=nodes, client_nodes=nodes,
                                     claims=["c0"], allow_busy_victim=False))
        got = fence.pick(random.Random(1), nodes=nodes, client_nodes=nodes, claims=["c0"],
                         allow_busy_victim=True)
        assert got is not None
        self.assertNotEqual(got[0], got[1])

    def test_nothing_to_fence_without_a_claim_or_a_second_node(self):
        self.assertIsNone(fence.pick(random.Random(1), nodes=self.NODES, client_nodes=["w1"],
                                     claims=[], allow_busy_victim=True))
        self.assertIsNone(fence.pick(random.Random(1), nodes=["w1"], client_nodes=[],
                                     claims=["c0"], allow_busy_victim=True))


class PartitionRules(unittest.TestCase):
    """Port 2049 in both directions for each metadata server address, and nothing else. The
    NVMe-oF paths stay up, so after the fence only the reservation stops the victim's
    writes."""

    def test_both_directions_for_every_address_each_tagged(self):
        rules = fence.partition_rules("sbtest-fence-r1", ["10.244.3.7", "10.96.4.4"])
        self.assertEqual(len(rules), 4)
        for r in rules:
            self.assertIn("2049", r)
            self.assertEqual(r[-2:], ["-j", "DROP"])
            self.assertIn("sbtest-fence-r1", r)
        self.assertEqual(sorted(r[0] for r in rules), ["INPUT", "INPUT", "OUTPUT", "OUTPUT"])
        out = [r for r in rules if r[0] == "OUTPUT"]
        self.assertTrue(all("--dport" in r and "-d" in r for r in out))
        inp = [r for r in rules if r[0] == "INPUT"]
        self.assertTrue(all("--sport" in r and "-s" in r for r in inp))

    def test_an_address_listed_twice_gets_one_pair(self):
        self.assertEqual(len(fence.partition_rules("t", ["10.0.0.1", "10.0.0.1"])), 2)


class Direct(unittest.TestCase):
    """Writes on a layout go to the block device, so the mount's NFS WRITE count stays flat
    while they succeed."""

    def test_layout_and_no_nfs_writes_is_direct(self):
        self.assertTrue(fence.is_direct({"LAYOUTGET": 1, "WRITE": 4},
                                        {"LAYOUTGET": 1, "WRITE": 4}, ok_writes=10))

    def test_nfs_writes_while_the_probe_writes_is_not_direct(self):
        self.assertFalse(fence.is_direct({"LAYOUTGET": 1, "WRITE": 4},
                                         {"LAYOUTGET": 1, "WRITE": 14}, ok_writes=10))

    def test_no_layout_or_no_writes_is_not_direct(self):
        self.assertFalse(fence.is_direct({"WRITE": 0}, {"WRITE": 0}, ok_writes=10))
        self.assertFalse(fence.is_direct({"LAYOUTGET": 1, "WRITE": 0},
                                         {"LAYOUTGET": 1, "WRITE": 0}, ok_writes=0))
        self.assertFalse(fence.is_direct(None, {"LAYOUTGET": 1}, ok_writes=10))


class WriterLog(unittest.TestCase):
    def test_each_write_is_read_with_its_result(self):
        text = ("1791500000000 ok\n"
                "1791500001500 fail 124\n"
                "garbage\n"
                "1791500003000 fail 1 dd: error writing: Input/output error\n")
        got = fence.parse_writer_log(text)
        self.assertEqual([w.ok for w in got], [True, False, False])
        self.assertEqual(got[1].ts, datetime.fromtimestamp(1791500001.5, UTC))
        self.assertEqual(got[1].detail, "124")
        self.assertIn("Input/output error", got[2].detail)


class FenceArchive(unittest.TestCase):
    def test_reads_the_fence_record(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "fence.json"), "w") as fh:
                json.dump({"seed": 4, "victim_node": "w3", "recaller_node": "w1",
                           "claim": "c0", "rules": ["OUTPUT -d 10.0.0.9 -j DROP"],
                           "partitioned": "2026-10-09T10:00:00Z",
                           "healed": "2026-10-09T10:06:00Z",
                           "truncate_issued": "2026-10-09T10:00:30Z",
                           "truncate_returned": None, "truncate_timeout_s": 240,
                           "writes": [{"ts": "2026-10-09T10:00:10Z", "ok": True},
                                      {"ts": "2026-10-09T10:01:00Z", "ok": False,
                                       "detail": "124"}]}, fh)
            got = ArchiveEvidence(d).fence()
        assert got is not None
        self.assertEqual((got.victim_node, got.recaller_node, got.claim), ("w3", "w1", "c0"))
        self.assertIsNone(got.truncate_returned)
        self.assertEqual(got.truncate_timeout_s, 240.0)
        self.assertEqual([w.ok for w in got.writes], [True, False])
        self.assertEqual(got.writes[1].detail, "124")
        self.assertEqual(got.rules, ("OUTPUT -d 10.0.0.9 -j DROP",))

    def test_an_archive_without_a_fence_has_none(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(ArchiveEvidence(d).fence())


class FenceTeardown(unittest.TestCase):
    """The rules live on the host, not in a pod, so deleting the pods is not enough.
    Teardown deletes exactly the rules it recorded, through the helper pod, before it
    deletes the pods."""

    def run_teardown(self, rc: int = 0) -> tuple[list[list[str]], list[list[str]], str]:
        calls: list[list[str]] = []

        def run(args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            calls.append(list(args))
            return subprocess.CompletedProcess(args, rc if "exec" in args else 0, "", "")

        f = fence.Fencer()
        rules = fence.partition_rules("sbtest-fence-r1", ["10.244.3.7"])
        f._rules = [list(r) for r in rules]
        f._net_pod = "r1-fence-net"
        f._pods = ["r1-fence-writer", "r1-fence-recaller", "r1-fence-net"]
        with tempfile.TemporaryDirectory() as d, mock.patch.object(kube, "run", run):
            log = os.path.join(d, "run.log")
            ctx = RunContext(run_id="r1", outdir=d, log=Logger(log))
            f.bind_namespaces(ctx)
            f.teardown(ctx)
            f.teardown(ctx)
            ctx.log.close()
            with open(log) as fh:
                text = fh.read()
        return calls, rules, text

    def test_it_deletes_exactly_the_recorded_rules_then_the_pods(self):
        calls, rules, _ = self.run_teardown()
        execs = [c[c.index("--") + 1:] for c in calls if "exec" in c]
        self.assertEqual(sorted(e for e in execs if "iptables" in e),
                         sorted(["iptables", "-D", *r] for r in rules))
        last_exec = max(i for i, c in enumerate(calls) if "exec" in c)
        deletes = [i for i, c in enumerate(calls) if "delete" in c]
        self.assertTrue(deletes)
        self.assertLess(last_exec, min(deletes))
        deleted = {n for i in deletes for n in calls[i]}
        self.assertTrue({"r1-fence-writer", "r1-fence-recaller", "r1-fence-net"} <= deleted)

    def test_a_rule_it_cannot_remove_is_logged_with_the_rule(self):
        _, rules, text = self.run_teardown(rc=1)
        self.assertIn("ERROR", text)
        self.assertIn(" ".join(rules[0]), text)


if __name__ == "__main__":
    unittest.main()
