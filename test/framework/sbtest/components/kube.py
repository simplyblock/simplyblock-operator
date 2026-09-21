"""Minimal kubectl plumbing shared by the cluster-touching components.

Deliberately subprocess-over-kubectl rather than a client library: it keeps the framework
stdlib-only, it works with whatever kubeconfig and context the operator already uses, and
every call it makes is one a human can paste into a terminal when a run misbehaves — which
is most of the debugging value.
"""

from __future__ import annotations

import json
import subprocess
from dataclasses import dataclass


class KubectlError(RuntimeError):
    pass


def run(args: list[str], timeout: int = 60, check: bool = True,
        stdin: str | None = None) -> subprocess.CompletedProcess[str]:
    cp = subprocess.run(["kubectl", *args], input=stdin, capture_output=True,
                        text=True, timeout=timeout, check=False)
    if check and cp.returncode != 0:
        raise KubectlError(f"kubectl {' '.join(args)}: {cp.stderr.strip() or cp.returncode}")
    return cp


def run_bytes(args: list[str], timeout: int = 300) -> bytes:
    cp = subprocess.run(["kubectl", *args], capture_output=True, timeout=timeout, check=False)
    return cp.stdout


@dataclass(frozen=True)
class Pod:
    name: str
    namespace: str
    node: str
    containers: tuple[str, ...]
    phase: str = ""


def list_pods(namespace: str, name_contains: list[str] | None = None) -> list[Pod]:
    cp = run(["-n", namespace, "get", "pods", "-o", "json"])
    out = []
    for it in json.loads(cp.stdout).get("items", []):
        name = it["metadata"]["name"]
        if name_contains and not any(s in name for s in name_contains):
            continue
        out.append(Pod(
            name=name, namespace=namespace,
            node=it.get("spec", {}).get("nodeName", "") or "",
            containers=tuple(c["name"] for c in it.get("spec", {}).get("containers", [])),
            phase=it.get("status", {}).get("phase", "") or ""))
    return out


def exec_sh(namespace: str, pod: str, script: str, container: str | None = None,
            timeout: int = 300) -> str:
    args = ["-n", namespace, "exec", pod]
    if container:
        args += ["-c", container]
    args += ["--", "sh", "-c", script]
    return run(args, timeout=timeout, check=False).stdout


def short(node: str) -> str:
    return node.split(".")[0]
