"""Unit tests for the metadata server placement record: choosing the storage nodes to pin
it to, and the record's round trip through the run directory."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from types import SimpleNamespace
from typing import Any
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.adapters import ArchiveEvidence  # noqa: E402
from sbtest.components import mds_placement  # noqa: E402
from sbtest.core import MdsInstance, MdsPlacement  # noqa: E402

LABEL = "io.simplyblock.storagenodeset"


class StorageSelector(unittest.TestCase):
    def test_the_node_set_most_storage_nodes_carry_is_chosen(self):
        nodes = {"master-1": {}, "worker-1": {LABEL: "set-a"}, "worker-2": {LABEL: "set-a"},
                 "worker-3": {LABEL: "set-b"}}
        self.assertEqual(mds_placement.storage_selector(nodes, LABEL), {LABEL: "set-a"})
        self.assertEqual(mds_placement.storage_nodes(nodes, LABEL), ["worker-1", "worker-2", "worker-3"])

    def test_a_cluster_without_storage_nodes_has_no_selector(self):
        self.assertIsNone(mds_placement.storage_selector({"master-1": {}}, LABEL))


class Record(unittest.TestCase):
    def test_the_record_round_trips_through_the_run_directory(self):
        t0 = datetime(2026, 10, 14, 13, 0, tzinfo=UTC)
        record = MdsPlacement(pinned=True, storage_nodes=["worker-1"],
                              instances=[MdsInstance(pod="mds-0", node="worker-1", ip="10.0.0.1",
                                                     first=t0, last=t0)])
        with tempfile.TemporaryDirectory() as d:
            mds_placement.write_record(os.path.join(d, mds_placement.RECORD), record)
            self.assertEqual(ArchiveEvidence(d).mds_placement(), record)


def merge_patch(target: Any, patch: Any) -> Any:
    """RFC 7386, which is what kubectl patch --type=merge applies."""
    if not isinstance(patch, dict):
        return patch
    out = dict(target) if isinstance(target, dict) else {}
    for key, value in patch.items():
        if value is None:
            out.pop(key, None)
        else:
            out[key] = merge_patch(out.get(key), value)
    return out


class FakeCluster:
    """One SimplyblockDriver and two nodes, behind kube.read and kube.run."""

    def __init__(self, mds: dict[str, Any]) -> None:
        self.driver = {"metadata": {"name": "simplyblock"}, "spec": {"pnfs": {"mds": mds}}}
        self.nodes = {"items": [{"metadata": {"name": "worker-1", "labels": {LABEL: "set-a"}}},
                                {"metadata": {"name": "worker-2", "labels": {}}}]}

    def read(self, args: list[str], timeout: int = 60) -> subprocess.CompletedProcess[str]:
        body = self.nodes if args[:2] == ["get", "nodes"] else self.driver
        return subprocess.CompletedProcess(args, 0, json.dumps(body), "")

    def run(self, args: list[str], **_: Any) -> subprocess.CompletedProcess[str]:
        self.driver = merge_patch(self.driver, json.loads(args[args.index("-p") + 1]))
        return subprocess.CompletedProcess(args, 0, "", "")

    def mds(self) -> dict[str, Any]:
        return self.driver["spec"]["pnfs"]["mds"]


class Pinning(unittest.TestCase):
    def pin_and_restore(self, mds: dict[str, Any]) -> dict[str, Any]:
        cluster = FakeCluster(mds)
        ctx = SimpleNamespace(namespace=lambda plane: "simplyblock",
                              log=SimpleNamespace(info=lambda m: None, warn=lambda m: None))
        recorder = mds_placement.MdsPlacementRecorder(pin_to_storage_node=True, driver="simplyblock")
        with mock.patch.object(mds_placement.kube, "read", cluster.read), \
                mock.patch.object(mds_placement.kube, "run", cluster.run), \
                mock.patch.object(mds_placement.kube, "list_pods", lambda ns, sel: []):
            recorder.setup(ctx)
            self.assertEqual(cluster.mds()["nodeSelector"].get(LABEL), "set-a", "setup did not pin")
            recorder.teardown(ctx)
        return cluster.mds()

    def test_teardown_restores_a_selector_of_other_labels(self):
        self.assertEqual(self.pin_and_restore({"nodeSelector": {"zone": "a"}}),
                         {"nodeSelector": {"zone": "a"}})

    def test_teardown_restores_an_empty_selector(self):
        self.assertEqual(self.pin_and_restore({"nodeSelector": {}}), {"nodeSelector": {}})

    def test_teardown_removes_a_selector_that_was_not_there(self):
        self.assertEqual(self.pin_and_restore({}), {})


if __name__ == "__main__":
    unittest.main()
