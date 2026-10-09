"""A constant flow of short-lived pods joining and leaving pNFS volumes during the run.

`workload.pnfs` runs the same pods on the same volumes for the whole run, which never
exercises what a real ReadWriteMany user does all day: a pod arrives, stages a pNFS volume on
its node, takes layouts, writes, and leaves, and another one does the same somewhere else.
This component keeps that happening. Every churn pod either joins one of the volumes the
long-lived pods share, which is a new NFS client and a new layout on a volume in use, or
brings a pNFS volume of its own, which is a whole export created and torn down while the rest
of the run is under load.

Each pod runs one md5-verified fio on a file of its own for its lifetime, and its evidence is
collected before the pod is deleted, in the layout the fio detectors read, so they judge it
like any other instance. What only churn can show goes into `churn.json`: when each pod
arrived, reached I/O, finished, and left, and for an own volume whether its PersistentVolume
and NFSExport were gone afterward. `pnfs.churn` judges that.

The flow is scheduled, not sprinkled. Arrivals follow a seeded Poisson process at
`mean_interval_s`, each with a lifetime drawn from `min_life_s` to `max_life_s`, and an
arrival that finds `max_concurrent` pods already running waits for room rather than being
dropped, so the flow stays constant. Nothing arrives in the last `quiet_tail_s`, so every
pod has left before the run ends. The seed is logged, and the same seed replays the flow.

A Component rather than a FioWorkload: the base provisions once and waits once, and a churn
pod's whole life, from claim to deletion, happens inside the run.
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
from . import fio

MOUNT = "/data"
CHURN_LABEL = "sbtest-churn"


@dataclass(frozen=True)
class Arrival:
    """One churn pod to start: whether it joins a shared volume, and how long it lives."""

    reuse: bool
    lifetime_s: float


class ChurnPlan:
    """When churn pods arrive. Pure: it never touches a cluster, so it is testable alone.

    Times are seconds on any monotonic scale shared by `start`, `end`, and the `now` passed
    to `due`.
    """

    def __init__(self, *, seed: int, start: float, end: float, mean_interval_s: float,
                 min_life_s: float, max_life_s: float, reuse_ratio: float,
                 max_concurrent: int) -> None:
        self._rng = random.Random(seed)
        self._end = end
        self._mean = max(mean_interval_s, 0.001)
        self._life = (min_life_s, max(min_life_s, max_life_s))
        self._reuse = reuse_ratio
        self._cap = max_concurrent
        self._next = start + self._rng.expovariate(1.0 / self._mean)

    def due(self, now: float, active: int) -> list[Arrival]:
        """The pods to start at `now`, given how many churn pods are still running."""
        out: list[Arrival] = []
        while self._next <= now and self._next <= self._end:
            if active + len(out) >= self._cap:
                # Held rather than dropped: the arrival goes as soon as there is room.
                break
            out.append(Arrival(reuse=self._rng.random() < self._reuse,
                               lifetime_s=self._rng.uniform(*self._life)))
            self._next += self._rng.expovariate(1.0 / self._mean)
        return out


@dataclass
class _Record:
    pod: str
    claim: str
    own_volume: bool
    created: datetime
    node: str = ""
    io_started: datetime | None = None
    finished: datetime | None = None
    deleted: datetime | None = None
    rc: int | None = None
    pvc_deleted: datetime | None = None
    pv: str = ""
    lvol: str = ""
    pv_gone: bool | None = None
    export_gone: bool | None = None
    gone_s: float | None = None
    error: str = ""


@component
class ChurnWorkload(Component):
    """Short-lived pods joining and leaving pNFS volumes, a verified fio in each."""

    name = "workload.pnfs-churn"
    summary = "short-lived pods that join and leave pNFS volumes, some with their own volume"
    namespace_options = {"namespace": "test"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,         # the run's test namespace when unset
            "mean_interval_s": 30.0,   # between arrivals, on average
            "min_life_s": 60.0,
            "max_life_s": 180.0,
            "reuse_ratio": 0.5,        # share of pods that join a shared volume
            "max_concurrent": 4,
            "min_delay_s": 10.0,       # after the timed run starts
            "quiet_tail_s": 240.0,     # before it ends: the longest life plus cleanup
            "volume_size_gb": 5,       # an own volume
            "file_size_gb": 1,
            "ready_timeout_s": 300.0,  # to Running, which includes staging the volume
            "io_timeout_s": 180.0,     # from Running to fio's timed run
            "gone_timeout_s": 180.0,   # for an own volume's PV and NFSExport, at collect
            "seed": None,              # random, and logged, when unset
            **{k: v for k, v in fio.FIO_DEFAULTS.items() if k != "runtime_s"},
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._records: list[_Record] = []
        self._workers: list[threading.Thread] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._loop: threading.Thread | None = None
        self._seed = 0

    # ── the flow ─────────────────────────────────────────────────────────────────────

    def start(self, ctx: RunContext) -> None:
        duration = float(ctx.shared.get("run.duration_s") or 0)
        sc = str(ctx.shared.get("pnfs.storageclass") or "")
        if duration <= 0 or not sc:
            ctx.log.warn(f"{self.name}: needs the run's duration and workload.pnfs's "
                         "StorageClass; no churn this run")
            return
        seed = self.opt("seed")
        self._seed = int(seed) if seed is not None else random.SystemRandom().randrange(2**31)
        t0 = time.monotonic()
        start = t0 + float(self.opt("min_delay_s"))
        end = t0 + duration - float(self.opt("quiet_tail_s"))
        if end <= start:
            ctx.log.warn(f"{self.name}: a {duration:.0f}s run leaves no window for churn")
            return
        plan = ChurnPlan(seed=self._seed, start=start, end=end,
                         mean_interval_s=float(self.opt("mean_interval_s")),
                         min_life_s=float(self.opt("min_life_s")),
                         max_life_s=float(self.opt("max_life_s")),
                         reuse_ratio=float(self.opt("reuse_ratio")),
                         max_concurrent=int(self.opt("max_concurrent")))
        ctx.log.info(f"{self.name}: seed {self._seed}, a pod every "
                     f"{self.opt('mean_interval_s')}s on average for {end - t0:.0f}s, at most "
                     f"{self.opt('max_concurrent')} at once")
        rng = random.Random(self._seed)

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                for arrival in plan.due(time.monotonic(), self._active()):
                    worker = threading.Thread(target=self._live, args=(ctx, sc, arrival, rng),
                                              name="churn-pod", daemon=True)
                    with self._lock:
                        self._workers.append(worker)
                    worker.start()
                self._stop.wait(1.0)

        self._loop = threading.Thread(target=loop, name="churn", daemon=True)
        self._loop.start()

    def _active(self) -> int:
        with self._lock:
            return sum(1 for r in self._records if r.deleted is None)

    def _live(self, ctx: RunContext, sc: str, arrival: Arrival, rng: random.Random) -> None:
        """One churn pod's whole life: claim, pod, I/O, evidence, deletion."""
        ns = str(self.opt("namespace"))
        with self._lock:
            n = len(self._records) + 1
            shared = list(ctx.shared.get("pnfs.claims") or [])
            reuse = arrival.reuse and bool(shared)
            name = f"{ctx.run_id}-churn-{n}"
            claim = rng.choice(shared) if reuse else name
            record = _Record(pod=name, claim=claim, own_volume=not reuse,
                             created=datetime.now(UTC))
            self._records.append(record)
        inst = fio.FioInstance(pod=name, container="fio-0",
                               filename=f"{MOUNT}/{ctx.run_id}-churn-{n}.fio",
                               logdir="/logs/c0", evidence=f"{ctx.run_id}-fio-churn-{n}")
        try:
            if record.own_volume:
                kube.run(["-n", ns, "apply", "-f", "-"],
                         stdin=json.dumps(self._claim(ctx, claim, sc)))
            kube.run(["-n", ns, "apply", "-f", "-"],
                     stdin=json.dumps(self._pod(ctx, inst, claim, arrival.lifetime_s)))
            ctx.log.info(f"{self.name}: {name} joins {claim} "
                         f"({'its own' if record.own_volume else 'shared'}) for "
                         f"{arrival.lifetime_s:.0f}s")
            self._run(ctx, ns, inst, record, arrival.lifetime_s)
        except Exception as e:  # noqa: BLE001
            record.error = record.error or str(e)
            ctx.log.warn(f"{self.name}: {name}: {e}")
        finally:
            self._leave(ctx, ns, inst, record)

    def _run(self, ctx: RunContext, ns: str, inst: fio.FioInstance, record: _Record,
             lifetime_s: float) -> None:
        deadline = time.monotonic() + float(self.opt("ready_timeout_s"))
        while _phase(ns, inst.pod) != "Running":
            if time.monotonic() > deadline:
                record.error = (f"not Running within {self.opt('ready_timeout_s')}s "
                                f"({_phase(ns, inst.pod) or 'absent'})")
                return
            time.sleep(3)
        record.node = kube.short(_node(ns, inst.pod))
        deadline = time.monotonic() + float(self.opt("io_timeout_s"))
        while not fio.in_timed_run(kube.run(["-n", ns, "logs", inst.pod, "-c", inst.container,
                                             "--tail=8"], check=False, timeout=30).stdout or ""):
            if time.monotonic() > deadline:
                record.error = f"fio not in its timed run within {self.opt('io_timeout_s')}s"
                return
            time.sleep(3)
        record.io_started = datetime.now(UTC)
        deadline = time.monotonic() + lifetime_s + 120
        while time.monotonic() < deadline:
            rc = kube.exec_sh(ns, inst.pod, f"cat {inst.logdir}/fio.rc 2>/dev/null",
                              container=inst.container, timeout=30).strip()
            if rc:
                record.finished = datetime.now(UTC)
                record.rc = int(rc) if rc.lstrip("-").isdigit() else None
                break
            time.sleep(3)
        else:
            record.error = "fio still running past its lifetime"
        # Collected before the pod goes: the evidence is on its emptyDir.
        fio.collect_instance(ctx, ns, inst, [])

    def _leave(self, ctx: RunContext, ns: str, inst: fio.FioInstance, record: _Record) -> None:
        """Delete the pod, and an own volume's claim after it, recording when."""
        kube.run(["-n", ns, "delete", "pod", inst.pod, "--ignore-not-found",
                  "--grace-period=5", "--timeout=120s"], check=False, timeout=150)
        record.deleted = datetime.now(UTC)
        if not record.own_volume:
            return
        record.pv, record.lvol = _volume_of(ns, record.claim)
        kube.run(["-n", ns, "delete", "pvc", record.claim, "--ignore-not-found",
                  "--wait=false"], check=False, timeout=60)
        record.pvc_deleted = datetime.now(UTC)
        ctx.log.info(f"{self.name}: {inst.pod} left, deleting its volume {record.claim}")

    # ── what churn creates ───────────────────────────────────────────────────────────

    def _labels(self, ctx: RunContext) -> dict[str, str]:
        return {"sbtest": ctx.run_id, "sbtest-run": "true", CHURN_LABEL: ctx.run_id}

    def _claim(self, ctx: RunContext, claim: str, sc: str) -> dict:
        return {
            "apiVersion": "v1", "kind": "PersistentVolumeClaim",
            "metadata": {"name": claim, "labels": {**self._labels(ctx),
                                                    "volume-kind": "pnfs-churn"}},
            "spec": {"accessModes": ["ReadWriteMany"], "storageClassName": sc,
                     "resources": {"requests": {
                         "storage": f"{self.opt('volume_size_gb')}Gi"}}},
        }

    def _pod(self, ctx: RunContext, inst: fio.FioInstance, claim: str,
             lifetime_s: float) -> dict:
        opts = {**self.options, "runtime_s": int(lifetime_s)}
        args = fio.fio_args(opts, filename=inst.filename,
                            size_gb=int(self.opt("file_size_gb")), logdir=inst.logdir)
        return {
            "apiVersion": "v1", "kind": "Pod",
            # No app=fio: workload.pnfs finds its own pods by that label.
            "metadata": {"name": inst.pod, "labels": self._labels(ctx)},
            "spec": {
                "restartPolicy": "Never",
                "terminationGracePeriodSeconds": 5,
                "containers": [{
                    "name": inst.container, "image": str(self.opt("image")),
                    "imagePullPolicy": "IfNotPresent",
                    "command": ["sh", "-c", fio.container_script(args, inst.logdir)],
                    "volumeMounts": [{"name": "data", "mountPath": MOUNT},
                                     {"name": "logs", "mountPath": "/logs"}],
                    "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}},
                }],
                "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": claim}},
                            {"name": "logs", "emptyDir": {}}],
            },
        }

    # ── the end of the run ───────────────────────────────────────────────────────────

    def stop(self, ctx: RunContext) -> None:
        # Only arrivals stop. A pod still alive finishes its life, and collect waits for it.
        self._stop.set()
        if self._loop:
            self._loop.join(timeout=15)
            self._loop = None

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            workers = list(self._workers)
        budget = (float(self.opt("ready_timeout_s")) + float(self.opt("io_timeout_s"))
                  + float(self.opt("max_life_s")) + 300)
        deadline = time.monotonic() + budget
        for worker in workers:
            worker.join(timeout=max(0.0, deadline - time.monotonic()))
        with self._lock:
            records = list(self._records)
        self._check_gone(ctx, [r for r in records if r.own_volume and r.pvc_deleted])
        ctx.save_json("churn.json", {"seed": self._seed, "pods": [{
            "pod": r.pod, "claim": r.claim, "own_volume": r.own_volume, "node": r.node,
            "created": _iso(r.created), "io_started": _iso(r.io_started),
            "finished": _iso(r.finished), "deleted": _iso(r.deleted), "rc": r.rc,
            "pvc_deleted": _iso(r.pvc_deleted), "pv": r.pv, "lvol": r.lvol,
            "pv_gone": r.pv_gone, "export_gone": r.export_gone, "gone_s": r.gone_s,
            "error": r.error} for r in records]})
        own = sum(1 for r in records if r.own_volume)
        ctx.log.info(f"{self.name}: {len(records)} pod(s) joined and left, {own} with a "
                     f"volume of their own (seed {self._seed})")

    def _check_gone(self, ctx: RunContext, records: list[_Record]) -> None:
        """Whether each deleted own volume's PV and NFSExport are gone, waiting a while."""
        ns = str(self.opt("namespace"))
        deadline = time.monotonic() + float(self.opt("gone_timeout_s"))
        pending = list(records)
        while pending:
            for r in list(pending):
                r.pv_gone = not r.pv or not _exists(None, "pv", r.pv)
                r.export_gone = not r.lvol or not _exists(ns, "nfsexport", f"nfsexp-{r.lvol}")
                if r.pv_gone and r.export_gone and r.pvc_deleted:
                    r.gone_s = (datetime.now(UTC) - r.pvc_deleted).total_seconds()
                    pending.remove(r)
            if not pending or time.monotonic() >= deadline:
                return
            time.sleep(5)

    def teardown(self, ctx: RunContext) -> None:
        # Whatever churn created and did not delete, a failed run included.
        ns, selector = str(self.opt("namespace")), f"{CHURN_LABEL}={ctx.run_id}"
        self._stop.set()
        kube.run(["-n", ns, "delete", "pod", "-l", selector, "--ignore-not-found",
                  "--grace-period=5"], check=False, timeout=300)
        kube.run(["-n", ns, "delete", "pvc", "-l", selector, "--ignore-not-found"],
                 check=False, timeout=300)


def _iso(t: datetime | None) -> str | None:
    return t.isoformat().replace("+00:00", "Z") if t else None


def _phase(ns: str, pod: str) -> str:
    cp = kube.run(["-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.phase}"], check=False)
    return cp.stdout.strip() if cp.returncode == 0 else ""


def _node(ns: str, pod: str) -> str:
    cp = kube.run(["-n", ns, "get", "pod", pod, "-o", "jsonpath={.spec.nodeName}"],
                  check=False)
    return cp.stdout.strip()


def _volume_of(ns: str, claim: str) -> tuple[str, str]:
    """A claim's PersistentVolume and the lvol behind it, from the PV's CSI handle."""
    cp = kube.run(["-n", ns, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}"],
                  check=False)
    pv = cp.stdout.strip() if cp.returncode == 0 else ""
    if not pv:
        return "", ""
    cp = kube.run(["get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeHandle}"], check=False)
    parts = cp.stdout.strip().split(":")
    return pv, parts[2] if len(parts) == 3 else ""


def _exists(ns: str | None, kind: str, name: str) -> bool:
    args = ["get", kind, name, "-o", "name"]
    if ns:
        args = ["-n", ns, *args]
    cp = kube.run(args, check=False)
    if cp.returncode == 0:
        return bool(cp.stdout.strip())
    # Only NotFound means gone. Any other error is not evidence of absence.
    return "NotFound" not in cp.stderr and "not found" not in cp.stderr
