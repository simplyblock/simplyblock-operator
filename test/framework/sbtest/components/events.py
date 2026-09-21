"""Control-plane event log collection.

Small, cheap, and disproportionately useful: it is the only source that says what the
control plane *thought* it was doing, which is what turns a host-side symptom into a
diagnosis.
"""

from __future__ import annotations

import json
from typing import Any

from ..core import Component, RunContext, component
from . import kube


@component
class ClusterEvents(Component):
    """Dump the simplyblock cluster event log via sbctl inside a webappapi pod."""

    name = "cluster.events"
    summary = "sbctl cluster get-logs -> cluster-events.json"

    def defaults(self) -> dict[str, Any]:
        return {"namespace": "simplyblock", "pod_prefix": "simplyblock-webappapi",
                "limit": 50000, "cluster_uuid": None}

    def collect(self, ctx: RunContext) -> None:
        pods = kube.list_pods(self.opt("namespace"), [self.opt("pod_prefix")])
        if not pods:
            ctx.log.warn(f"{self.name}: no {self.opt('pod_prefix')} pod; skipping")
            return
        pod = pods[0].name
        cluster = self.opt("cluster_uuid") or ctx.shared.get("cluster.uuid")
        if not cluster:
            out = kube.exec_sh(self.opt("namespace"), pod,
                               "sbctl cluster list --json 2>/dev/null || true", timeout=60)
            try:
                items = json.loads(out)
                cluster = (items[0].get("UUID") or items[0].get("uuid")) if items else None
            except (json.JSONDecodeError, AttributeError, IndexError):
                cluster = None
        if not cluster:
            ctx.log.warn(f"{self.name}: cannot resolve the cluster uuid; skipping")
            return

        out = kube.exec_sh(
            self.opt("namespace"), pod,
            f"sbctl cluster get-logs {cluster} --json --limit={int(self.opt('limit'))}",
            timeout=180)
        if not out.strip():
            ctx.log.warn(f"{self.name}: sbctl returned nothing")
            return
        path = ctx.path("cluster-events.json")
        with open(path, "w") as fh:
            fh.write(out)
        try:
            ctx.log.info(f"{self.name}: {len(json.loads(out))} entries -> {path}")
        except json.JSONDecodeError:
            ctx.log.info(f"{self.name}: -> {path} (not valid JSON)")
