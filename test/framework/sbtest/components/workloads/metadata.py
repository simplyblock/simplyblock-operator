"""Namespace operations against the pNFS metadata server while fio drives data, checked from
another client.

fio exercises the data path: once a client holds a layout, its writes go to the namespace
directly, and the metadata server sees little of them. What only the metadata server serves
is the namespace: creates, renames, unlinks, directories, and attributes. This component
keeps that traffic going for the whole run, so a metadata server restart, a node plugin
restart, and churn all happen under it, and checks that what one client did is what another
client sees.

Each worker pod makes namespace operations in a directory of its own on one of the run's
shared pNFS volumes (`metadata_agent.py worker`), at a low, steady, seeded rate, and keeps a
manifest of what that directory should hold. Each worker has a verifier pod on another node
mounting the same volume. A check pauses the worker, which then writes a fresh manifest and
holds still, lists the directory from the verifier (`metadata_agent.py list`), and compares
the two. Checks run every `verify_interval_s` and once more after the worker stops, a
`quiet_tail_s` before the run ends. The op logs become `metadata-<worker>.log` and the
checks `metadata.json`, which `pnfs.metadata` judges.

A Component rather than a FioWorkload: there is no fio in it, and the volumes are
workload.pnfs's, which publishes them once its pods run.
"""

from __future__ import annotations

import json
import random
import threading
import time
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from ...core import Component, RunContext, component
from .. import kube
from . import metadata_agent
from .fio import FIO_IMAGE

MOUNT = "/data"
LOGDIR = "/logs"
METADATA_LABEL = "sbtest-metadata"


@dataclass(frozen=True)
class Assignment:
    """Where one worker runs, on which claim, and on which node its verifier lists it."""

    index: int
    node: str
    claim: str
    verifier_node: str


def assign(nodes: list[str], claims: list[str], workers: int) -> list[Assignment]:
    """Workers spread over the nodes and claims in order, each verified from the next node.
    `workers` 0 means one per node. With a single node the verifier shares it, and its
    checks say they were not cross-node."""
    if not nodes or not claims:
        return []
    n = workers or len(nodes)
    return [Assignment(index=i, node=nodes[i % len(nodes)], claim=claims[i % len(claims)],
                       verifier_node=nodes[(i + 1) % len(nodes)]) for i in range(n)]


@dataclass(frozen=True)
class Diff:
    """What another client lists that the manifest does not say, and the reverse."""

    missing: tuple[str, ...] = ()
    extra: tuple[str, ...] = ()
    mismatched: tuple[str, ...] = ()

    @property
    def clean(self) -> bool:
        return not (self.missing or self.extra or self.mismatched)


def compare(manifest: dict[str, Any], listing: dict[str, Any]) -> Diff:
    """The manifest against a listing. A path whose last operation failed, and anything
    under it, is left out: it may or may not have changed, and only the error says so."""
    uncertain = list(manifest.get("uncertain") or [])

    def judged(p: str) -> bool:
        return not any(p == u or p.startswith(u + "/") for u in uncertain)

    mfiles = {p: v for p, v in (manifest.get("files") or {}).items() if judged(p)}
    lfiles = {p: v for p, v in (listing.get("files") or {}).items() if judged(p)}
    mdirs = {d for d in manifest.get("dirs") or [] if judged(d)}
    ldirs = {d for d in listing.get("dirs") or [] if judged(d)}
    changed = [p for p in set(mfiles) & set(lfiles)
               if (mfiles[p].get("size"), mfiles[p].get("md5"))
               != (lfiles[p].get("size"), lfiles[p].get("md5"))]
    return Diff(missing=tuple(sorted((set(mfiles) - set(lfiles)) | (mdirs - ldirs))),
                extra=tuple(sorted((set(lfiles) - set(mfiles)) | (ldirs - mdirs))),
                mismatched=tuple(sorted(changed)))


def agent_source() -> str:
    with open(metadata_agent.__file__) as fh:
        return fh.read()


@dataclass
class _Worker:
    a: Assignment
    pod: str
    verifier: str
    root: str


@component
class MetadataWorkload(Component):
    """Namespace operations on the shared pNFS volumes, checked from another node."""

    name = "workload.pnfs-metadata"
    summary = "namespace operations on the shared pNFS volumes, verified from another node"
    namespace_options = {"namespace": "test"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,           # the run's test namespace when unset
            "workers": 0,                # 0: one per pNFS client node
            "rate": 2.0,                 # operations per second per worker
            "mix": dict(metadata_agent.DEFAULT_MIX),
            "verify_interval_s": 300.0,  # 0 checks only once, after the workers stop
            "quiet_tail_s": 120.0,       # the workers stop this long before the run ends
            "manifest_interval_s": 30.0,
            "pause_timeout_s": 120.0,    # for a worker to hold still for a check
            "settle_s": 2.0,             # between the pause and the listing
            "ready_timeout_s": 300.0,
            "image": FIO_IMAGE,
            "seed": None,                # random, and logged, when unset
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._workers: list[_Worker] = []
        self._checks: list[dict[str, Any]] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._loop: threading.Thread | None = None
        self._seed = 0

    # ── the run ──────────────────────────────────────────────────────────────────────

    def start(self, ctx: RunContext) -> None:
        duration = float(ctx.shared.get("run.duration_s") or 0)
        nodes = sorted(ctx.shared.get("pnfs.client_nodes") or [])
        claims = sorted(ctx.shared.get("pnfs.claims") or [])
        span = duration - float(self.opt("quiet_tail_s"))
        plan = assign(nodes, claims, int(self.opt("workers")))
        if span <= 0 or not plan:
            ctx.log.warn(f"{self.name}: needs the run's duration and workload.pnfs's claims and "
                         "nodes; no metadata workload this run")
            return
        seed = self.opt("seed")
        self._seed = int(seed) if seed is not None else random.SystemRandom().randrange(2**31)
        ns = str(self.opt("namespace"))
        for a in plan:
            w = _Worker(a=a, pod=f"{ctx.run_id}-meta-{a.index}",
                        verifier=f"{ctx.run_id}-metaverify-{a.index}",
                        root=f"{MOUNT}/{ctx.run_id}-meta/{ctx.run_id}-meta-{a.index}")
            if a.verifier_node == a.node:
                ctx.log.warn(f"{self.name}: {w.pod} is verified from its own node {a.node}, "
                             "so its checks are not cross-node")
            kube.run(["-n", ns, "apply", "-f", "-"],
                     stdin=json.dumps(self._worker_pod(ctx, w, span)))
            kube.run(["-n", ns, "apply", "-f", "-"], stdin=json.dumps(self._verifier_pod(ctx, w)))
            self._workers.append(w)
        ctx.log.info(f"{self.name}: seed {self._seed}, {len(plan)} worker(s) at "
                     f"{self.opt('rate')} op/s for {span:.0f}s")
        interval = float(self.opt("verify_interval_s"))
        if interval > 0:
            def loop() -> None:
                while not self._stop.wait(interval) and not ctx.stopping.is_set():
                    for w in list(self._workers):
                        self._check(ctx, w)

            self._loop = threading.Thread(target=loop, name="metadata-verify", daemon=True)
            self._loop.start()

    def _check(self, ctx: RunContext, w: _Worker) -> None:
        """Pause the worker, list its directory from the verifier, and compare."""
        ns = str(self.opt("namespace"))
        record: dict[str, Any] = {"ts": _iso(datetime.now(UTC)), "worker": w.pod,
                                  "worker_node": kube.short(w.a.node),
                                  "verifier_node": kube.short(w.a.verifier_node),
                                  "missing": [], "extra": [], "mismatched": [], "error": ""}
        paused = False
        try:
            kube.exec_sh(ns, w.pod, f"touch {LOGDIR}/pause", timeout=30)
            paused = True
            deadline = time.monotonic() + float(self.opt("pause_timeout_s"))
            while kube.exec_sh(ns, w.pod, f"ls {LOGDIR}/paused {LOGDIR}/stopped 2>/dev/null",
                               timeout=30).strip() == "":
                if time.monotonic() > deadline:
                    raise RuntimeError(f"the worker did not pause within "
                                       f"{self.opt('pause_timeout_s')}s")
                time.sleep(1)
            manifest = json.loads(kube.exec_sh(ns, w.pod, f"cat {LOGDIR}/manifest.json",
                                               timeout=60) or "{}")
            time.sleep(float(self.opt("settle_s")))
            cp = kube.run(["-n", ns, "exec", w.verifier, "--", "python3", "-c", agent_source(),
                           "list", "--root", w.root], check=False, timeout=300)
            if cp.returncode != 0:
                raise RuntimeError(f"listing from {w.verifier} failed: "
                                   f"{cp.stderr.strip() or cp.returncode}")
            diff = compare(manifest, json.loads(cp.stdout))
            record.update(missing=list(diff.missing), extra=list(diff.extra),
                          mismatched=list(diff.mismatched))
        except Exception as e:  # noqa: BLE001
            record["error"] = str(e)
        finally:
            if paused:
                kube.exec_sh(ns, w.pod, f"rm -f {LOGDIR}/pause", timeout=30)
        with self._lock:
            self._checks.append(record)
        state = record["error"] or ("clean" if not (record["missing"] or record["extra"]
                                                     or record["mismatched"]) else "DIFFERS")
        ctx.log.info(f"{self.name}: {w.pod} checked from {w.verifier}: {state}")

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._loop:
            self._loop.join(timeout=float(self.opt("pause_timeout_s")) + 60)
            self._loop = None

    def collect(self, ctx: RunContext) -> None:
        ns = str(self.opt("namespace"))
        for w in self._workers:
            # The worker stops by itself a quiet tail before the end. The stop file covers a
            # run cut short.
            kube.exec_sh(ns, w.pod, f"touch {LOGDIR}/stop", timeout=30)
            self._check(ctx, w)
            log = kube.run(["-n", ns, "exec", w.pod, "--", "cat", f"{LOGDIR}/ops.log"],
                           check=False, timeout=300).stdout
            with open(ctx.path(f"metadata-{w.pod}.log"), "w") as fh:
                fh.write(log or "")
        with self._lock:
            checks = list(self._checks)
        ctx.save_json("metadata.json", {"seed": self._seed, "workers": [{
            "worker": w.pod, "node": kube.short(w.a.node), "claim": w.a.claim,
            "verifier": w.verifier, "verifier_node": kube.short(w.a.verifier_node),
            "root": w.root} for w in self._workers], "checks": checks})
        bad = sum(1 for c in checks if c["missing"] or c["extra"] or c["mismatched"])
        ctx.log.info(f"{self.name}: {len(checks)} check(s), {bad} with a difference "
                     f"(seed {self._seed})")

    def teardown(self, ctx: RunContext) -> None:
        self._stop.set()
        kube.run(["-n", str(self.opt("namespace")), "delete", "pod", "-l",
                  f"{METADATA_LABEL}={ctx.run_id}", "--ignore-not-found", "--grace-period=5"],
                 check=False, timeout=300)

    # ── the pods ─────────────────────────────────────────────────────────────────────

    def _labels(self, ctx: RunContext) -> dict[str, str]:
        return {"sbtest": ctx.run_id, "sbtest-run": "true", METADATA_LABEL: ctx.run_id}

    def _pod(self, ctx: RunContext, name: str, node: str, claim: str, script: str,
             env: list[dict[str, str]]) -> dict[str, Any]:
        return {
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": name, "labels": self._labels(ctx)},
            "spec": {
                "nodeName": node, "restartPolicy": "Never", "terminationGracePeriodSeconds": 5,
                "containers": [{
                    "name": "meta", "image": str(self.opt("image")),
                    "imagePullPolicy": "IfNotPresent", "command": ["sh", "-c", script],
                    "env": env,
                    "volumeMounts": [{"name": "data", "mountPath": MOUNT},
                                     {"name": "logs", "mountPath": LOGDIR}],
                    "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}},
                }],
                "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": claim}},
                            {"name": "logs", "emptyDir": {}}],
            },
        }

    def _worker_pod(self, ctx: RunContext, w: _Worker, span_s: float) -> dict[str, Any]:
        args = (f"worker --root {w.root} --logdir {LOGDIR} --seed {self._seed + w.a.index} "
                f"--rate {float(self.opt('rate'))} --duration {span_s:.0f} "
                f"--manifest-interval {float(self.opt('manifest_interval_s'))} "
                f"--mix '{json.dumps(self.opt('mix'))}'")
        script = (_INSTALL + f'python3 -c "$META_AGENT" {args}\n'
                  f'echo "$?" > {LOGDIR}/agent.rc\nsleep 100000\n')
        return self._pod(ctx, w.pod, w.a.node, w.a.claim, script,
                         [{"name": "META_AGENT", "value": agent_source()}])

    def _verifier_pod(self, ctx: RunContext, w: _Worker) -> dict[str, Any]:
        return self._pod(ctx, w.verifier, w.a.verifier_node, w.a.claim,
                         _INSTALL + "sleep 100000\n", [])


_INSTALL = ("apk add --no-cache python3 >/dev/null 2>&1 || "
            '{ echo "[pod] apk add python3 FAILED"; exit 90; }\n')


def _iso(t: datetime) -> str:
    return t.isoformat().replace("+00:00", "Z")

