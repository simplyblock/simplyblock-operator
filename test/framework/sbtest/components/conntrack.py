"""Sampling each node's connection tracking table for its NFS flows, over the run.

A Service routes a new connection to a pod by rewriting its destination, and the node's
connection tracking table remembers that rewrite for the flow's address and port pair. The
Linux NFS client reconnects from the same source port it used before, so after the metadata
server's pod is replaced the reconnect can match the entry left by the old connection and
be sent to the deleted pod's address, whatever the Service says now. In pnfs-1791525621
clients went unanswered for the whole minute a healthy replacement was serving while fresh
mounts reached it at once, which this would explain.

`nfs.conntrack` records the evidence that decides it: every NFS flow on every node running
a node plugin, with the address it was translated to, every `interval_s`, in conntrack.csv.
`pnfs.conntrack-pinned` reads it against the restarts. It lives apart from the NFS client
counters (`nfs`) because it reads the node, not a client, and needs its own host-network
helper pod per node to do so.
"""

from __future__ import annotations

import concurrent.futures
import json
import re
import threading
from datetime import datetime
from typing import Any

from ..core import Component, ConntrackSample, RunContext, component, now_utc
from . import kube

#: The NFS port. Only flows to it are kept: the table holds every connection on the node.
NFS_PORT = 2049

#: What the helper prints when neither conntrack(8) nor the procfs table can be read.
UNAVAILABLE = "SBTEST-CONNTRACK-UNAVAILABLE"

_CSI_NODE_PREFIX = "simplyblock-csi-node-"

# conntrack(8) first, then the kernel's own table where the kernel exposes it. Both print one
# flow per line with the original tuple first and the reply tuple second.
_READ = (f"if out=$(conntrack -L -p tcp --dport {NFS_PORT} 2>/dev/null); then "
         f"printf '%s\\n' \"$out\"; "
         f"elif [ -r /proc/net/nf_conntrack ]; then "
         f"grep ' dport={NFS_PORT} ' /proc/net/nf_conntrack || true; "
         f"else echo {UNAVAILABLE}; fi")

_HELPER = ("apk add --no-cache conntrack-tools >/dev/null 2>&1 || "
           "echo 'apk add conntrack-tools failed' >&2; sleep {ttl}")

_TCP_STATES = frozenset({
    "SYN_SENT", "SYN_RECV", "ESTABLISHED", "FIN_WAIT", "CLOSE_WAIT", "LAST_ACK", "TIME_WAIT",
    "CLOSE", "SYN_SENT2"})
_FIELD = re.compile(r"(src|dst|sport|dport)=(\S+)")

_HEADER = "ts,node,state,orig_src,orig_sport,orig_dst,orig_dport,reply_src\n"


def parse_conntrack(text: str, ts: datetime, node: str) -> list[ConntrackSample]:
    """The NFS flows in a conntrack(8) or /proc/net/nf_conntrack listing."""
    out = []
    for line in text.splitlines():
        fields = _FIELD.findall(line)
        state = next((w for w in line.split() if w in _TCP_STATES), "")
        if len(fields) < 5 or not state:
            continue
        orig: dict[str, str] = {}
        for key, value in fields[:4]:
            orig[key] = value
        reply_src = next((v for k, v in fields[4:] if k == "src"), "")
        try:
            sport, dport = int(orig.get("sport", "")), int(orig.get("dport", ""))
        except ValueError:
            continue
        if dport != NFS_PORT:
            continue
        out.append(ConntrackSample(ts=ts, node=node, state=state, orig_src=orig.get("src", ""),
                                   orig_sport=sport, orig_dst=orig.get("dst", ""),
                                   orig_dport=dport, reply_src=reply_src))
    return out


def write_samples(path: str, samples: list[ConntrackSample]) -> None:
    with open(path, "w") as fh:
        fh.write(_HEADER)
        for x in sorted(samples, key=lambda x: (x.ts, x.node)):
            fh.write(f"{x.ts.strftime('%Y-%m-%dT%H:%M:%SZ')},{x.node},{x.state},{x.orig_src},"
                     f"{x.orig_sport},{x.orig_dst},{x.orig_dport},{x.reply_src}\n")


@component
class ConntrackSampler(Component):
    """Read every node's NFS flows from its connection tracking table on an interval.

    One privileged host-network helper per node running a node plugin, which is every node
    a pNFS claim can be mounted on, churn pods included. A round reads all nodes in
    parallel, each bounded by `timeout_s`, so a stuck node does not delay the others.
    """

    name = "nfs.conntrack"
    summary = "sample each node's NFS flows in its connection tracking table on an interval"
    namespace_options = {"namespace": "test", "csi_namespace": "operator"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,       # the helper pods, in the test namespace when unset
            "csi_namespace": None,   # the node plugins, in the operator's when unset
            "interval_s": 10.0,
            "timeout_s": 20,
            "ttl_s": 86400,          # the helpers end by themselves after this
            "image": "alpine:3.20",
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        #: Short node name to helper pod.
        self._helpers: dict[str, str] = {}
        self._samples: list[ConntrackSample] = []
        self._unreadable: set[str] = set()
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def setup(self, ctx: RunContext) -> None:
        pods = kube.list_pods(str(self.opt("csi_namespace")), [_CSI_NODE_PREFIX])
        nodes = sorted({p.node for p in pods
                        if p.name.startswith(_CSI_NODE_PREFIX) and p.phase == "Running"
                        and p.node})
        if not nodes:
            ctx.log.warn(f"{self.name}: no node plugin running; nothing to sample")
            return
        docs = []
        helpers: dict[str, str] = {}
        for node in nodes:
            name = f"{ctx.run_id}-ct-{kube.short(node)}"
            helpers[kube.short(node)] = name
            docs.append({
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "labels": {"sbtest": ctx.run_id,
                                                      "sbtest-run": "true",
                                                      "app": "sbtest-conntrack"}},
                "spec": {
                    "nodeName": node, "restartPolicy": "Never", "hostNetwork": True,
                    "terminationGracePeriodSeconds": 1,
                    "tolerations": [{"operator": "Exists"}],
                    "containers": [{
                        "name": "conntrack", "image": str(self.opt("image")),
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["sh", "-c", _HELPER.format(ttl=int(self.opt("ttl_s")))],
                        # Reading the host's table needs CAP_NET_ADMIN in its namespace.
                        "securityContext": {"privileged": True}}],
                }})
        kube.run(["-n", str(self.opt("namespace")), "apply", "-f", "-"],
                 stdin=json.dumps({"apiVersion": "v1", "kind": "List", "items": docs}))
        # Only once they exist: a helper that was never created reads as a node with no
        # flows, which is the one thing this component must not claim without looking.
        self._helpers = helpers
        ctx.log.info(f"{self.name}: a helper on each of {len(docs)} node(s)")

    def _read(self, node: str, pod: str) -> str:
        """The helper's output, or UNAVAILABLE when the exec itself failed: an exec into a
        pod that is not running prints nothing, and nothing parses as no flows."""
        cp = kube.run(["-n", str(self.opt("namespace")), "exec", pod, "--", "sh", "-c", _READ],
                      timeout=int(self.opt("timeout_s")), check=False)
        if cp.returncode != 0:
            return f"{UNAVAILABLE} exec into {pod} exited {cp.returncode}"
        return cp.stdout

    def _sample(self, ctx: RunContext) -> None:
        if not self._helpers:
            return
        with concurrent.futures.ThreadPoolExecutor(max_workers=len(self._helpers)) as pool:
            reads = {node: pool.submit(self._read, node, pod)
                     for node, pod in sorted(self._helpers.items())}
        ts = now_utc()
        batch: list[ConntrackSample] = []
        for node, fut in reads.items():
            try:
                text = fut.result()
            except Exception as e:  # noqa: BLE001
                text = f"{UNAVAILABLE} {e}"
            if UNAVAILABLE in text:
                if node not in self._unreadable:
                    self._unreadable.add(node)
                    ctx.log.warn(f"{self.name}: cannot read {node}'s connection tracking "
                                 "table; its flows are not recorded")
                continue
            flows = parse_conntrack(text, ts, node)
            batch.extend(flows or [ConntrackSample(ts=ts, node=node, state="NONE")])
        with self._lock:
            self._samples.extend(batch)

    def start(self, ctx: RunContext) -> None:
        if not self._helpers:
            return
        interval = float(self.opt("interval_s"))

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                self._sample(ctx)
                self._stop.wait(interval)

        self._thread = threading.Thread(target=loop, name="nfs-conntrack", daemon=True)
        self._thread.start()
        ctx.log.info(f"{self.name}: sampling every {interval}s")

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=float(self.opt("timeout_s")) + 10)
            self._thread = None

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            samples = list(self._samples)
        if samples:
            write_samples(ctx.path("conntrack.csv"), samples)
        ctx.log.info(f"{self.name}: {len(samples)} sample row(s)")

    def teardown(self, ctx: RunContext) -> None:
        if self._helpers:
            kube.run(["-n", str(self.opt("namespace")), "delete", "pod", "--wait=false",
                      "--ignore-not-found", *sorted(self._helpers.values())], check=False)
