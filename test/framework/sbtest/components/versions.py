"""What was deployed when the run started, so a result can be tied to it.

A verdict is only comparable with another when both say what they ran against. Without it,
the question "which guest image did that run have" is answered by reconstructing a build
timeline from CI logs, which is how it was answered the first time. `run.versions` records,
at setup, the image and the resolved digest of every container in the operator and cluster
namespaces, every node's kernel, OS image, runtime, and kubelet, and the API server's
version, into versions.json. `evidence.versions` reports the summary.
"""

from __future__ import annotations

import json
from typing import Any

from ..core import Component, RunContext, component
from . import kube


def parse_versions(pods: list[dict], nodes: dict, server: dict) -> dict[str, Any]:
    """versions.json from what kubectl prints for the pods of each namespace, the nodes,
    and the server version. A namespace listed twice, which the operator and the cluster
    namespaces are when they are the same, is recorded once."""
    images: list[dict[str, str]] = []
    seen: set[tuple[str, str, str]] = set()
    for listing in pods:
        for it in listing.get("items", []):
            meta = it.get("metadata", {})
            for c in it.get("status", {}).get("containerStatuses", []) or []:
                key = (str(meta.get("namespace", "")), str(meta.get("name", "")),
                       str(c.get("name", "")))
                if key in seen:
                    continue
                seen.add(key)
                images.append({"namespace": key[0], "pod": key[1], "container": key[2],
                               "image": str(c.get("image", "")),
                               "image_id": str(c.get("imageID", ""))})
    node_rows = []
    for it in nodes.get("items", []):
        info = it.get("status", {}).get("nodeInfo", {})
        node_rows.append({"node": str(it.get("metadata", {}).get("name", "")),
                          "kernel": str(info.get("kernelVersion", "")),
                          "os_image": str(info.get("osImage", "")),
                          "runtime": str(info.get("containerRuntimeVersion", "")),
                          "kubelet": str(info.get("kubeletVersion", ""))})
    return {"server": str(server.get("serverVersion", {}).get("gitVersion", "")),
            "images": images, "nodes": node_rows}


@component
class RunVersions(Component):
    """Record the images, digests, node kernels, and server version the run started on."""

    name = "run.versions"
    summary = "record deployed images and digests, node kernels, and the server version"
    namespace_options = {"operator_namespace": "operator",  # noqa: RUF012
                         "cluster_namespace": "cluster"}

    def defaults(self) -> dict[str, Any]:
        return {"operator_namespace": None, "cluster_namespace": None}

    def setup(self, ctx: RunContext) -> None:
        pods = []
        for ns in dict.fromkeys([self.opt("operator_namespace"), self.opt("cluster_namespace")]):
            pods.append(_json(["-n", str(ns), "get", "pods", "-o", "json"]))
        doc = parse_versions(pods, _json(["get", "nodes", "-o", "json"]),
                             _json(["version", "-o", "json"]))
        ctx.save_json("versions.json", doc)
        kernels = sorted({n["kernel"] for n in doc["nodes"]})
        ctx.log.info(f"{self.name}: {len(doc['images'])} container(s) on "
                     f"{len(doc['nodes'])} node(s), kernel(s) {', '.join(kernels) or '?'}, "
                     f"server {doc['server'] or '?'}")


def _json(args: list[str]) -> dict:
    cp = kube.run(args, check=False, timeout=60)
    try:
        out = json.loads(cp.stdout or "{}")
    except json.JSONDecodeError:
        return {}
    return out if isinstance(out, dict) else {}
