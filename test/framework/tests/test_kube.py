"""Unit tests for the kubectl wrapper's reads against a faked kubectl.

Covered here: listing pods rides out an API server that is briefly unreachable, and still
fails at once on an error that waiting does not fix.
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

from sbtest.components import kube  # noqa: E402

PODS = json.dumps({"items": [{"metadata": {"name": "simplyblock-pnfs-mds-0"},
                              "spec": {"nodeName": "worker-4", "containers": [{"name": "mds-runner"}]},
                              "status": {"phase": "Running", "podIP": "10.129.2.30"}}]})

UNREACHABLE = "Unable to connect to the server: dial tcp 192.168.10.248:6443: i/o timeout"


def answers(*replies: tuple[int, str, str]) -> Callable[..., subprocess.CompletedProcess[str]]:
    """A kubectl that gives these (returncode, stdout, stderr) replies in order."""
    calls = iter(replies)

    def run(args: list[str], **_: object) -> subprocess.CompletedProcess[str]:
        rc, out, err = next(calls)
        return subprocess.CompletedProcess(["kubectl", *args], rc, out, err)
    return run


class ListPods(unittest.TestCase):
    # Regression: pnfs-1791985761. One kubectl get pods during a few seconds of API server
    # outage failed logs.collect as a whole, and the metadata server's log, which only it
    # collects, was lost.
    def test_an_unreachable_api_server_is_retried(self):
        fake = answers((1, "", UNREACHABLE), (1, "", UNREACHABLE), (0, PODS, ""))
        with mock.patch.object(kube.subprocess, "run", side_effect=fake), \
                mock.patch.object(kube.time, "sleep"):
            pods = kube.list_pods("simplyblock", ["pnfs-mds"])
        self.assertEqual([(p.name, p.node) for p in pods], [("simplyblock-pnfs-mds-0", "worker-4")])

    def test_an_error_waiting_does_not_fix_fails_at_once(self):
        fake = answers((1, "", 'Error from server (Forbidden): pods is forbidden'), (0, PODS, ""))
        with mock.patch.object(kube.subprocess, "run", side_effect=fake), \
                mock.patch.object(kube.time, "sleep"), self.assertRaises(kube.KubectlError):
            kube.list_pods("simplyblock")

    def test_an_outage_that_outlasts_the_retries_fails(self):
        fake = answers(*[(1, "", UNREACHABLE)] * 10)
        with mock.patch.object(kube.subprocess, "run", side_effect=fake), \
                mock.patch.object(kube.time, "sleep"), self.assertRaises(kube.KubectlError):
            kube.list_pods("simplyblock")


if __name__ == "__main__":
    unittest.main()


class FullNodeName(unittest.TestCase):
    """A pod's nodeName is the node's full name, though the runs carry the short one."""

    NODES = ["vm01.simplyblock3.localdomain", "vm02.simplyblock3.localdomain"]

    def test_a_short_name_resolves_to_the_nodes_full_name(self):
        self.assertEqual("vm02.simplyblock3.localdomain", kube.full_node_name("vm02", self.NODES))

    def test_a_full_name_is_kept(self):
        self.assertEqual("vm01.simplyblock3.localdomain",
                         kube.full_node_name("vm01.simplyblock3.localdomain", self.NODES))

    def test_a_node_the_cluster_does_not_have_is_returned_as_given(self):
        self.assertEqual("vm09", kube.full_node_name("vm09", self.NODES))

    def test_a_short_name_two_nodes_share_is_not_guessed(self):
        both = ["vm01.a.example", "vm01.b.example"]
        self.assertEqual("vm01", kube.full_node_name("vm01", both))

    def test_the_cluster_nodes_are_read_without_their_prefix(self):
        cp = subprocess.CompletedProcess([], 0, stdout="node/vm01.x\nnode/vm02.x\n", stderr="")
        with mock.patch.object(kube, "run", return_value=cp):
            self.assertEqual(["vm01.x", "vm02.x"], kube.node_names())
