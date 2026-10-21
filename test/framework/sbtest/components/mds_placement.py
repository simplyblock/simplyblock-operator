"""Where the pNFS metadata server runs during a run, and putting it on a storage node.

The metadata server's guest attaches every volume it exports over NVMe/TCP, and on a node
that runs a storage node it cannot reach that node's own listeners the way it reaches the
others. Whether a run exercised that case depends on where the scheduler put the pod: on
the Talos lab it chose the control-plane node, which runs no storage node, in every run on
disk, and the case never ran. Nothing in the evidence said so.

`pnfs.mds-placement` records every metadata server instance and the node it ran on, and
the nodes that run storage nodes, in mds-placement.json, for the detector of the same name
to judge. With `pin_to_storage_node` it points the SimplyblockDriver's
spec.pnfs.mds.nodeSelector at the storage node set for the run, so the case runs on every
cluster, and restores the selector the driver had at teardown. Changing the selector
restarts the metadata server, which is why the pinning happens in setup, before any
workload mounts.
"""

from __future__ import annotations

import json
import threading
import time
from collections import Counter
from collections.abc import Mapping
from typing import Any

from ..core import Component, MdsInstance, MdsPlacement, RunContext, component, now_utc
from . import kube

#: The record's file name in the run directory.
RECORD = "mds-placement.json"

#: The metadata server's pods, by name.
MDS_POD = "simplyblock-pnfs-mds-"


def storage_nodes(node_labels: dict[str, dict[str, str]], label: str) -> list[str]:
    """The nodes carrying the storage node set label, by name."""
    return sorted(n for n, labels in node_labels.items() if labels.get(label))


def storage_selector(node_labels: dict[str, dict[str, str]], label: str) -> dict[str, str] | None:
    """A node selector for the storage node set most storage nodes belong to, or None when
    no node runs a storage node."""
    sets = Counter(v for labels in node_labels.values() if (v := labels.get(label)))
    if not sets:
        return None
    value = sorted(sets.items(), key=lambda kv: (-kv[1], kv[0]))[0][0]
    return {label: value}


def write_record(path: str, record: MdsPlacement) -> None:
    with open(path, "w") as fh:
        json.dump({
            "pinned": record.pinned,
            "storage_nodes": record.storage_nodes,
            "instances": [{"pod": i.pod, "node": i.node, "ip": i.ip,
                           "first": i.first.isoformat(), "last": i.last.isoformat()}
                          for i in record.instances],
        }, fh, indent=2)


@component
class MdsPlacementRecorder(Component):
    """Record where the metadata server ran, and optionally pin it to the storage nodes."""

    name = "pnfs.mds-placement"
    summary = "record the metadata server's node per instance, and optionally pin it to storage nodes"

    def defaults(self) -> dict[str, Any]:
        return {
            "plane": "operator",
            # The SimplyblockDriver whose spec.pnfs.mds is changed. Empty takes the only one
            # in the operator's namespace.
            "driver": "",
            # Put the metadata server on a storage node for the run, so its path to its own
            # node's storage is exercised whatever the scheduler would prefer.
            "pin_to_storage_node": False,
            # The label every node running a storage node carries, valued with its set.
            "storage_label": "io.simplyblock.storagenodeset",
            "interval_s": 10,
            # How long setup waits for a pinned metadata server to run on a storage node.
            "pin_timeout_s": 600,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._instances: dict[tuple[str, str], list[Any]] = {}  # (pod, address) -> [node, first, last]
        self._storage: list[str] = []
        self._pinned = False
        self._previous: dict[str, str] | None = None
        self._applied: dict[str, str] = {}
        self._driver = ""

    # -- the cluster ----------------------------------------------------------------
    def _node_labels(self) -> dict[str, dict[str, str]]:
        cp = kube.read(["get", "nodes", "-o", "json"])
        return {n["metadata"]["name"]: dict(n["metadata"].get("labels") or {})
                for n in json.loads(cp.stdout).get("items", [])}

    def _driver_name(self, ns: str) -> str:
        if self.opt("driver"):
            return str(self.opt("driver"))
        cp = kube.read(["-n", ns, "get", "simplyblockdrivers", "-o", "json"])
        names = [str(d["metadata"]["name"]) for d in json.loads(cp.stdout).get("items", [])]
        if len(names) != 1:
            raise RuntimeError(f"{self.name}: {len(names)} SimplyblockDriver(s) in {ns}; "
                               "name the one to pin with the driver option")
        return names[0]

    def _selector(self, ns: str) -> dict[str, str] | None:
        cp = kube.read(["-n", ns, "get", "simplyblockdriver", self._driver, "-o", "json"])
        mds = (json.loads(cp.stdout).get("spec", {}).get("pnfs") or {}).get("mds") or {}
        return dict(mds.get("nodeSelector") or {}) if "nodeSelector" in mds else None

    def _set_selector(self, ns: str, selector: Mapping[str, str | None] | None) -> None:
        patch = {"spec": {"pnfs": {"mds": {"nodeSelector": selector}}}}
        kube.run(["-n", ns, "patch", "simplyblockdriver", self._driver, "--type=merge",
                  "-p", json.dumps(patch)])

    def _sample(self, ctx: RunContext) -> list[kube.Pod]:
        pods = [p for p in kube.list_pods(ctx.namespace(str(self.opt("plane"))), [MDS_POD])
                if p.node and p.phase == "Running"]
        ts = now_utc()
        with self._lock:
            for p in pods:
                entry = self._instances.setdefault((p.name, p.ip), [p.node, ts, ts])
                entry[0], entry[2] = p.node, ts
        return pods

    # -- lifecycle ------------------------------------------------------------------
    def setup(self, ctx: RunContext) -> None:
        ns = ctx.namespace(str(self.opt("plane")))
        labels = self._node_labels()
        self._storage = storage_nodes(labels, str(self.opt("storage_label")))
        if not self.opt("pin_to_storage_node"):
            return
        selector = storage_selector(labels, str(self.opt("storage_label")))
        if selector is None:
            ctx.log.warn(f"{self.name}: no node carries {self.opt('storage_label')}; the "
                         "metadata server is not pinned")
            return
        self._driver = self._driver_name(ns)
        self._previous = self._selector(ns)
        self._applied = {**(self._previous or {}), **selector}
        self._set_selector(ns, self._applied)
        self._pinned = True
        ctx.log.info(f"{self.name}: metadata server pinned to {selector} on driver {self._driver}")
        deadline = time.monotonic() + float(self.opt("pin_timeout_s"))
        while time.monotonic() < deadline:
            pods = self._sample(ctx)
            if not pods or all(p.node in self._storage for p in pods):
                return
            time.sleep(5)
        ctx.log.warn(f"{self.name}: the metadata server still runs off the storage nodes after "
                     f"{self.opt('pin_timeout_s')}s")

    def start(self, ctx: RunContext) -> None:
        def loop() -> None:
            while not self._stop.wait(float(self.opt("interval_s"))):
                try:
                    self._sample(ctx)
                except Exception as e:  # noqa: BLE001
                    ctx.log.warn(f"{self.name}: sampling failed: {e}")
        self._sample(ctx)
        self._thread = threading.Thread(target=loop, name="mds-placement", daemon=True)
        self._thread.start()

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=30)

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            instances = [MdsInstance(pod=pod, node=node, ip=ip, first=first, last=last)
                         for (pod, ip), (node, first, last) in self._instances.items()]
        instances.sort(key=lambda i: i.first)
        write_record(ctx.path(RECORD), MdsPlacement(pinned=self._pinned, storage_nodes=self._storage,
                                                     instances=instances))
        nodes = sorted({i.node for i in instances})
        ctx.log.info(f"{self.name}: {len(instances)} metadata server instance(s) on {nodes}")

    def teardown(self, ctx: RunContext) -> None:
        if not self._pinned:
            return
        ns = ctx.namespace(str(self.opt("plane")))
        # A merge patch merges a map into the one there, so a key the pin added is
        # removed by naming it with null. A selector that was not there at all goes
        # with a null for the whole field.
        restore: dict[str, str | None] | None = None
        if self._previous is not None:
            restore = {**{k: None for k in self._applied if k not in self._previous}, **self._previous}
        try:
            self._set_selector(ns, restore)
            ctx.log.info(f"{self.name}: driver {self._driver} node selector restored")
        except Exception as e:  # noqa: BLE001
            ctx.log.warn(f"{self.name}: restoring the node selector on {self._driver} failed: {e}")
