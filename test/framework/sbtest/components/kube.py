"""Minimal kubectl plumbing shared by the cluster-touching components.

Deliberately subprocess-over-kubectl rather than a client library: it keeps the framework
stdlib-only, it works with whatever kubeconfig and context the operator already uses, and
every call it makes is one a human can paste into a terminal when a run misbehaves — which
is most of the debugging value.
"""

from __future__ import annotations

import json
import subprocess
import time
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


def apply_each(namespace: str, docs: list[dict], timeout: int = 60) -> None:
    """Apply each object with its own kubectl call.

    Not as a v1 List: kubectl validates a List against every model the cluster publishes,
    and the operator's metrics API publishes names with slashes in them, whose references
    kubectl cannot resolve. Not as a stream of documents either: kubectl 1.33 panicked
    decoding JSON documents between YAML separators (slice bounds out of range in
    StreamReader.Consume). A single object needs neither.
    """
    for doc in docs:
        run(["-n", namespace, "apply", "-f", "-"], stdin=json.dumps(doc), timeout=timeout)


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
    ip: str = ""


#: What kubectl says when the API server did not answer at all, as opposed to answering
#: with an error. Only these are worth waiting out.
UNREACHABLE = ("Unable to connect to the server", "i/o timeout", "connection refused",
               "TLS handshake timeout", "the server is currently unable to handle the request")

#: How often, and how far apart, a read is retried on an API server that did not answer.
#: About half a minute in all: a blip outlasts it rarely, a down server always.
READ_ATTEMPTS = 6
READ_BACKOFF_S = 5.0


def read(args: list[str], timeout: int = 60) -> subprocess.CompletedProcess[str]:
    """A kubectl read that rides out an API server that briefly does not answer.

    A component that lists or gets once at a fixed moment, such as the collection at the
    end of a run, otherwise loses its whole step to a few seconds of API server outage.
    Every other failure is raised at once, because waiting does not fix it.
    """
    for attempt in range(READ_ATTEMPTS):
        cp = run(args, timeout=timeout, check=False)
        if cp.returncode == 0:
            return cp
        if not any(m in cp.stderr for m in UNREACHABLE) or attempt == READ_ATTEMPTS - 1:
            break
        time.sleep(READ_BACKOFF_S)
    raise KubectlError(f"kubectl {' '.join(args)}: {cp.stderr.strip() or cp.returncode}")


def list_pods(namespace: str, name_contains: list[str] | None = None) -> list[Pod]:
    cp = read(["-n", namespace, "get", "pods", "-o", "json"])
    out = []
    for it in json.loads(cp.stdout).get("items", []):
        name = it["metadata"]["name"]
        if name_contains and not any(s in name for s in name_contains):
            continue
        out.append(Pod(
            name=name, namespace=namespace,
            node=it.get("spec", {}).get("nodeName", "") or "",
            containers=tuple(c["name"] for c in it.get("spec", {}).get("containers", [])),
            phase=it.get("status", {}).get("phase", "") or "",
            ip=it.get("status", {}).get("podIP", "") or ""))
    return out


def exec_sh(namespace: str, pod: str, script: str, container: str | None = None,
            timeout: int = 300) -> str:
    return exec_result(namespace, pod, script, container=container, timeout=timeout).stdout


def exec_result(namespace: str, pod: str, script: str, container: str | None = None,
                timeout: int = 300) -> subprocess.CompletedProcess[str]:
    """exec_sh with the exit code and stderr kept, for a caller that has to say why a command
    failed rather than only that its output was empty."""
    args = ["-n", namespace, "exec", pod]
    if container:
        args += ["-c", container]
    args += ["--", "sh", "-c", script]
    return run(args, timeout=timeout, check=False)


def pod_mounting(namespace: str, claim: str) -> tuple[str, str] | None:
    """A running fio pod mounting claim, and its first container."""
    cp = run(["-n", namespace, "get", "pods", "-l", "app=fio", "-o", "json"], check=False)
    if cp.returncode != 0:
        return None
    for p in json.loads(cp.stdout or "{}").get("items", []):
        if p.get("status", {}).get("phase") != "Running":
            continue
        claims = {(v.get("persistentVolumeClaim") or {}).get("claimName")
                  for v in p.get("spec", {}).get("volumes", [])}
        containers = p.get("spec", {}).get("containers", [])
        if claim in claims and containers:
            return str(p["metadata"]["name"]), str(containers[0]["name"])
    return None


def short(node: str) -> str:
    return node.split(".")[0]
