"""Capturing each client node's NFS traffic, headers only, for the whole run.

The metadata workload records what a call returned and the restart log records when the
metadata server went down, but neither says what was on the wire in between. In
pnfs-1791711824 a mkdir issued after the drain had started failed with EEXIST once the
server was back: the server had created the directory, and nothing in the run could say
whether the request reached it before the drain, or was replayed after the restart by a
retry of the whole compound. A capture answers that directly: each request's XID, how
often it was sent, and every reply's status.

`nfs.capture` runs tcpdump on every node running a node plugin, which is every node a pNFS
claim is mounted on, keeping the first 512 bytes of each packet on the NFS port: the RPC
header and the compound's operations, not the file data. pNFS data goes to the NVMe/TCP
target, so it does not show up here. It lives apart from `nfs.conntrack` because it holds
its own capture for the run and collects files rather than samples.
"""

from __future__ import annotations

from typing import Any

from ..core import Component, RunContext, component
from . import kube

#: The NFS port; the capture keeps nothing else.
NFS_PORT = 2049

#: Where the helper writes its capture files inside its own container.
_DIR = "/cap"

_CSI_NODE_PREFIX = "simplyblock-csi-node-"

_HELPER = ("apk add --no-cache tcpdump >/dev/null 2>&1 || echo 'apk add tcpdump failed' >&2; "
           f"mkdir -p {_DIR}; "
           "tcpdump -i any -nn -s {snaplen} -C {file_mb} -W {files} "
           f"-w {_DIR}/nfs.pcap 'tcp port {NFS_PORT}' & "
           "sleep {ttl}")

# SIGINT makes tcpdump flush and close its current file; the listing follows once it has.
_STOP = (f"pkill -INT tcpdump 2>/dev/null; i=0; "
         f"while pgrep tcpdump >/dev/null 2>&1 && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done; "
         f"ls -1 {_DIR}")


@component
class NfsCapture(Component):
    """Capture every client node's NFS packet headers over the run and collect the files.

    One privileged host-network helper per node running a node plugin. tcpdump rotates
    through `files` files of `file_mb` each, so a long run keeps its last
    `files * file_mb` MB per node rather than filling the node.
    """

    name = "nfs.capture"
    summary = "capture each client node's NFS packet headers over the run"
    namespace_options = {"namespace": "test", "csi_namespace": "operator"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,       # the helper pods, in the test namespace when unset
            "csi_namespace": None,   # the node plugins, in the operator's when unset
            "image": "alpine:3.20",
            "snaplen": 512,          # RPC header and compound operations, not file data
            "file_mb": 100,
            "files": 10,
            "ttl_s": 86400,          # the helpers end by themselves after this
            "timeout_s": 120,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        #: Short node name to helper pod.
        self._helpers: dict[str, str] = {}

    def setup(self, ctx: RunContext) -> None:
        pods = kube.list_pods(str(self.opt("csi_namespace")), [_CSI_NODE_PREFIX])
        nodes = sorted({p.node for p in pods
                        if p.name.startswith(_CSI_NODE_PREFIX) and p.phase == "Running"
                        and p.node})
        if not nodes:
            ctx.log.warn(f"{self.name}: no node plugin running; nothing to capture")
            return
        command = _HELPER.format(snaplen=int(self.opt("snaplen")),
                                 file_mb=int(self.opt("file_mb")),
                                 files=int(self.opt("files")), ttl=int(self.opt("ttl_s")))
        docs = []
        helpers: dict[str, str] = {}
        for node in nodes:
            name = f"{ctx.run_id}-cap-{kube.short(node)}"
            helpers[kube.short(node)] = name
            docs.append({
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "labels": {"sbtest": ctx.run_id,
                                                      "sbtest-run": "true",
                                                      "app": "sbtest-nfs-capture"}},
                "spec": {
                    "nodeName": node, "restartPolicy": "Never", "hostNetwork": True,
                    "terminationGracePeriodSeconds": 1,
                    "tolerations": [{"operator": "Exists"}],
                    "containers": [{
                        "name": "capture", "image": str(self.opt("image")),
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["sh", "-c", command],
                        # Capturing on the host's interfaces needs CAP_NET_RAW there.
                        "securityContext": {"privileged": True}}],
                }})
        kube.apply_each(str(self.opt("namespace")), docs)
        self._helpers = helpers
        ctx.log.info(f"{self.name}: capturing NFS headers on {len(docs)} node(s)")

    def collect(self, ctx: RunContext) -> None:
        ns, timeout = str(self.opt("namespace")), int(self.opt("timeout_s"))
        saved = 0
        for node, pod in sorted(self._helpers.items()):
            cp = kube.run(["-n", ns, "exec", pod, "--", "sh", "-c", _STOP],
                          timeout=timeout, check=False)
            if cp.returncode != 0:
                ctx.log.warn(f"{self.name}: cannot stop {node}'s capture; nothing collected "
                             f"from it")
                continue
            files = sorted(f for f in cp.stdout.split() if f.startswith("nfs.pcap"))
            for f in files:
                # Read through exec rather than kubectl cp, which truncates large files.
                try:
                    data = kube.run_bytes(["-n", ns, "exec", pod, "--", "cat", f"{_DIR}/{f}"],
                                          timeout=timeout)
                except Exception as e:  # noqa: BLE001
                    ctx.log.warn(f"{self.name}: cannot read {node}'s {f}: {e}")
                    continue
                with open(ctx.path(f"nfs-capture-{node}-{f.removeprefix('nfs.pcap')}.pcap"),
                          "wb") as fh:
                    fh.write(data)
                saved += 1
        ctx.log.info(f"{self.name}: {saved} capture file(s)")

    def teardown(self, ctx: RunContext) -> None:
        if self._helpers:
            kube.run(["-n", str(self.opt("namespace")), "delete", "pod", "--wait=false",
                      "--ignore-not-found", *sorted(self._helpers.values())], check=False)
