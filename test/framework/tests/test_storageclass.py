"""Unit tests for how a workload finds the StorageClass it clones, against a faked kubectl.

The class name embeds the cluster's name, which changes with every install, so a suite
leaves it unset and the workload picks the cluster's own class. These tests cover that
choice and the errors when there is no single class to pick.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import unittest
from collections.abc import Callable
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import sbtest  # noqa: E402,F401  (registers the bundled plugins)
from sbtest.components import kube  # noqa: E402
from sbtest.components.workloads.base import FioWorkload  # noqa: E402


def _sc(name: str, provisioner: str = "csi.simplyblock.io", fstype: str = "",
        managed_by: str = "") -> dict:
    params = {"csi.storage.k8s.io/fstype": fstype} if fstype else {}
    labels = {"storage.simplyblock.io/managed-by": managed_by} if managed_by else {}
    return {"metadata": {"name": name, "labels": labels}, "provisioner": provisioner,
            "parameters": params}


def _fake(classes: list[dict]) -> Callable[..., subprocess.CompletedProcess[str]]:
    def run(args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
        assert args[:2] == ["get", "sc"], args
        if len(args) > 2 and not args[2].startswith("-"):
            found = [c for c in classes if c["metadata"]["name"] == args[2]]
            if not found:
                return subprocess.CompletedProcess(args, 1, "", "NotFound")
            return subprocess.CompletedProcess(args, 0, json.dumps(found[0]), "")
        return subprocess.CompletedProcess(args, 0, json.dumps({"items": classes}), "")
    return run


class SourceStorageClass(unittest.TestCase):
    POOL = "simplyblock-simplyblock-discovered-discovery-block-bb816d8c-cluster"

    def resolve(self, classes: list[dict], named: str = "") -> str:
        with mock.patch.object(kube, "run", _fake(classes)):
            return FioWorkload.source_storageclass(named)

    def test_unset_picks_the_only_simplyblock_class(self) -> None:
        classes = [_sc("local-path", provisioner="rancher.io/local-path"), _sc(self.POOL),
                   _sc("sbtest-pnfs-1-pnfs", fstype="pnfs"), _sc("sbtest-mig-1-single")]
        self.assertEqual(self.resolve(classes), self.POOL)

    def test_unset_skips_pnfs_classes(self) -> None:
        classes = [_sc(self.POOL), _sc("shared-files", fstype="pnfs")]
        self.assertEqual(self.resolve(classes), self.POOL)

    def test_unset_skips_the_metadata_server_state_class(self) -> None:
        classes = [_sc("simplyblock-b59f5ffa-pnfs-mds-state", managed_by="pnfs-mds"),
                   _sc(self.POOL, fstype="xfs", managed_by="storagecluster")]
        self.assertEqual(self.resolve(classes), self.POOL)

    def test_unset_with_several_candidates_names_them(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "pool-a, pool-b"):
            self.resolve([_sc("pool-a"), _sc("pool-b")])

    def test_unset_with_no_candidate_fails(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "no simplyblock StorageClass"):
            self.resolve([_sc("local-path", provisioner="rancher.io/local-path")])

    def test_named_class_is_used(self) -> None:
        self.assertEqual(self.resolve([_sc("pool-a"), _sc("pool-b")], "pool-b"), "pool-b")

    def test_named_class_missing_names_the_candidates(self) -> None:
        with self.assertRaisesRegex(RuntimeError, f"gone not found.*{self.POOL}"):
            self.resolve([_sc(self.POOL)], "gone")


if __name__ == "__main__":
    unittest.main()
