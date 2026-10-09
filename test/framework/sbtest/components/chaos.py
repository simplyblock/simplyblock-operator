"""Restarting the pods a pNFS run depends on, while it runs, to judge how it recovers.

A run that only ever measures a healthy cluster says nothing about the moments that break
volumes: the metadata server's guest booting cold, a node plugin that comes back with no
memory of the mounts it staged, a controller restarted mid-reconcile. `chaos.restart`
deletes those pods during the timed run and records each one as evidence (`restarts.json`),
so the detectors can tell a pause the restart explains from one it does not, and
`chaos.recovery` can fail a pod that never came back. The victim's log is followed from
just before its deletion until its containers exit (`restart-<n>-<target>-<pod>.txt`),
since kubelet removes it with the pod and logs.collect only finds what is left at the end.

Restarts are scheduled, not sprinkled. `guaranteed` of them fall at seeded random times
inside the run, so every run tests recovery at least that often. Beyond them each tick
restarts something with a low `chance`, which over many runs reaches the orderings a fixed
schedule never does. A restart waits for the one in flight unless `overlap_chance` comes
up, because two at once is a case worth having but not the common one. Nothing restarts in
the last `quiet_tail_s` of the run, so every recovery is observed before fio ends. The seed
is logged, and the same seed with the same window replays the same schedule.
"""

from __future__ import annotations

import contextlib
import json
import random
import subprocess
import threading
import time
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from ..core import Component, RunContext, component
from . import kube

#: What each target restarts, and how its replacement is recognized: the pod-name prefix
#: in the CSI namespace, and whether the replacement keeps the name (a StatefulSet's) or
#: gets a new one (a DaemonSet's, on the same node).
_TARGETS: dict[str, tuple[str, bool]] = {
    "mds": ("simplyblock-pnfs-mds-", True),
    "csi-node": ("simplyblock-csi-node-", False),
    "csi-controller": ("simplyblock-csi-controller-", True),
}


class RestartPlan:
    """When to restart what. Pure: it never touches a cluster, so it is testable alone.

    Times are seconds on any monotonic scale shared by `start`, `end`, and the `now` passed
    to `due`. `due` is called once per tick, and `chance` is per call.
    """

    def __init__(self, *, seed: int, weights: dict[str, float], start: float, end: float,
                 guaranteed: int, chance: float, overlap_chance: float) -> None:
        self._rng = random.Random(seed)
        self._targets = [t for t, w in weights.items() if w > 0]
        self._weights = [weights[t] for t in self._targets]
        self._end = end
        self._chance = chance
        self._overlap = overlap_chance
        span = max(0.0, end - start)
        self._pending = sorted(
            (start + self._rng.random() * span, self._pick()) for _ in range(guaranteed)
        ) if self._targets else []

    def _pick(self) -> str:
        return self._rng.choices(self._targets, weights=self._weights)[0]

    def due(self, now: float, in_flight: int) -> list[str]:
        """The targets to restart at `now`, given how many restarts are still in flight."""
        if not self._targets or now > self._end:
            return []
        out: list[str] = []
        while self._pending and self._pending[0][0] <= now:
            # A guaranteed restart waits out one in flight rather than being dropped, unless
            # the overlap chance comes up. At the end of the window it goes regardless.
            busy = in_flight + len(out) > 0
            if busy and now < self._end and self._rng.random() >= self._overlap:
                break
            out.append(self._pending.pop(0)[1])
        if (self._chance and self._rng.random() < self._chance
                and (in_flight + len(out) == 0 or self._rng.random() < self._overlap)):
            out.append(self._pick())
        return out


@dataclass
class _Record:
    target: str
    pod: str
    node: str
    deleted: datetime
    ready: datetime | None = None
    replacement: str = ""
    log: str = ""


@component
class Restarter(Component):
    """Delete the MDS pod, a pNFS client's node plugin, or the controller, during the run."""

    name = "chaos.restart"
    summary = "restart the MDS, csi-node, or csi-controller pods during the run"

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "simplyblock",
            # Relative weights. The metadata server matters most under load, the node plugin
            # next. The controller is off the I/O path and proves little until the run
            # provisions volumes mid-flight.
            "weights": {"mds": 3.0, "csi-node": 2.0, "csi-controller": 1.0},
            "guaranteed": 1,
            "chance": 0.02,          # per tick
            "overlap_chance": 0.1,
            "tick_s": 10.0,
            "min_delay_s": 30.0,     # after the timed run starts
            "quiet_tail_s": 120.0,   # before it ends
            "ready_timeout_s": 600.0,
            "grace_period_s": None,  # kubectl's default when unset
            "seed": None,            # random, and logged, when unset
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._records: list[_Record] = []
        self._workers: list[threading.Thread] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._loop: threading.Thread | None = None
        self._seed = 0

    def start(self, ctx: RunContext) -> None:
        duration = float(ctx.shared.get("run.duration_s") or 0)
        if duration <= 0:
            ctx.log.warn(f"{self.name}: the run has no duration, so nothing is restarted")
            return
        seed = self.opt("seed")
        self._seed = int(seed) if seed is not None else random.SystemRandom().randrange(2**31)
        t0 = time.monotonic()
        start = t0 + float(self.opt("min_delay_s"))
        end = t0 + duration - float(self.opt("quiet_tail_s"))
        if end <= start:
            ctx.log.warn(f"{self.name}: a {duration:.0f}s run leaves no window for restarts")
            return
        plan = RestartPlan(seed=self._seed, weights=dict(self.opt("weights")), start=start,
                           end=end, guaranteed=int(self.opt("guaranteed")),
                           chance=float(self.opt("chance")),
                           overlap_chance=float(self.opt("overlap_chance")))
        ctx.log.info(f"{self.name}: seed {self._seed}, {self.opt('guaranteed')} guaranteed "
                     f"restart(s) between {start - t0:.0f}s and {end - t0:.0f}s, chance "
                     f"{self.opt('chance')} per {self.opt('tick_s')}s tick")

        def loop() -> None:
            tick = float(self.opt("tick_s"))
            while not self._stop.is_set() and not ctx.stopping.is_set():
                for target in plan.due(time.monotonic(), self._in_flight()):
                    worker = threading.Thread(target=self._restart, args=(ctx, target),
                                              name=f"chaos-{target}", daemon=True)
                    with self._lock:
                        self._workers.append(worker)
                    worker.start()
                self._stop.wait(tick)

        self._loop = threading.Thread(target=loop, name="chaos-restart", daemon=True)
        self._loop.start()

    def _in_flight(self) -> int:
        with self._lock:
            return sum(1 for r in self._records if r.ready is None)

    def _victim(self, ctx: RunContext, target: str) -> kube.Pod | None:
        """The pod to delete. A node plugin is chosen among the nodes that mount a pNFS
        volume, when the workload said which: those are the ones a restart can hurt."""
        prefix, _ = _TARGETS[target]
        pods = [p for p in kube.list_pods(self.opt("namespace"), [prefix])
                if p.name.startswith(prefix) and p.phase == "Running"]
        if target == "csi-node":
            clients = set(ctx.shared.get("pnfs.client_nodes") or [])
            pods = [p for p in pods if kube.short(p.node) in clients] or pods
        if not pods:
            return None
        with self._lock:
            n = len(self._records)
        return random.Random(self._seed + n).choice(sorted(pods, key=lambda p: p.name))

    def _restart(self, ctx: RunContext, target: str) -> None:
        victim = self._victim(ctx, target)
        if victim is None:
            ctx.log.warn(f"{self.name}: no running {target} pod to restart")
            return
        uid = _uid(self.opt("namespace"), victim.name)
        record = _Record(target=target, pod=victim.name, node=kube.short(victim.node),
                         deleted=datetime.now(UTC))
        with self._lock:
            self._records.append(record)
            record.log = f"restart-{len(self._records)}-{target}-{victim.name}.txt"
        follower = self._follow(ctx, victim.name, record.log)
        args = ["-n", self.opt("namespace"), "delete", "pod", victim.name, "--wait=false"]
        if self.opt("grace_period_s") is not None:
            args.append(f"--grace-period={int(self.opt('grace_period_s'))}")
        cp = kube.run(args, check=False)
        if cp.returncode != 0:
            ctx.log.warn(f"{self.name}: deleting {victim.name} failed: {cp.stderr.strip()}")
        ctx.log.info(f"{self.name}: restarting {target} {victim.name} on {record.node}")
        deadline = time.monotonic() + float(self.opt("ready_timeout_s"))
        while time.monotonic() < deadline:
            replacement = self._replacement(target, victim, uid)
            if replacement:
                ready = datetime.now(UTC)
                with self._lock:
                    record.ready, record.replacement = ready, replacement
                ctx.log.info(f"{self.name}: {target} back as {replacement} after "
                             f"{(ready - record.deleted).total_seconds():.0f}s")
                _finish(follower)
                return
            time.sleep(2)
        _finish(follower)
        ctx.log.warn(f"{self.name}: {target} {victim.name} had no Ready replacement within "
                     f"{self.opt('ready_timeout_s')}s")

    def _follow(self, ctx: RunContext, pod: str,
                name: str) -> tuple[subprocess.Popen[bytes], Any] | None:
        """Follow the victim's log into the run directory until its containers exit, which
        is after the deletion: the shutdown is part of what is worth reading."""
        try:
            fh = open(ctx.path(name), "wb")  # noqa: SIM115
            proc = subprocess.Popen(  # noqa: S603
                ["kubectl", "-n", self.opt("namespace"), "logs", "-f", pod, "--all-containers",
                 "--timestamps", "--prefix"], stdout=fh, stderr=subprocess.DEVNULL)
        except Exception as e:  # noqa: BLE001
            ctx.log.warn(f"{self.name}: cannot follow {pod}'s log: {e}")
            return None
        return proc, fh

    def _replacement(self, target: str, victim: kube.Pod, old_uid: str) -> str:
        """The Ready pod that replaced the victim, or "" while there is none."""
        prefix, same_name = _TARGETS[target]
        cp = kube.run(["-n", self.opt("namespace"), "get", "pods", "-o", "json"], check=False)
        if cp.returncode != 0:
            return ""
        for it in json.loads(cp.stdout or "{}").get("items", []):
            meta, spec = it.get("metadata", {}), it.get("spec", {})
            name = str(meta.get("name", ""))
            if not name.startswith(prefix) or meta.get("uid") == old_uid:
                continue
            if same_name and name != victim.name:
                continue
            if not same_name and spec.get("nodeName") != victim.node:
                continue
            if any(c.get("type") == "Ready" and c.get("status") == "True"
                   for c in it.get("status", {}).get("conditions", [])):
                return name
        return ""

    def stop(self, ctx: RunContext) -> None:
        # Only the scheduler stops. A restart still waiting for its replacement keeps
        # waiting: ending the run is no reason to stop observing whether the cluster came
        # back, and collect waits for it.
        self._stop.set()
        if self._loop:
            self._loop.join(timeout=15)
            self._loop = None

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            workers = list(self._workers)
        for worker in workers:
            worker.join(timeout=float(self.opt("ready_timeout_s")))
        with self._lock:
            records = list(self._records)
        ctx.save_json("restarts.json", {"seed": self._seed, "restarts": [{
            "target": r.target, "pod": r.pod, "node": r.node,
            "deleted": _iso(r.deleted), "ready": _iso(r.ready) if r.ready else None,
            "replacement": r.replacement, "log": r.log} for r in records]})
        back = sum(1 for r in records if r.ready)
        ctx.log.info(f"{self.name}: {len(records)} restart(s), {back} came back "
                     f"(seed {self._seed})")


def _finish(follower: tuple[subprocess.Popen[bytes], Any] | None) -> None:
    """End a log follower. It ends by itself once the old pod's containers have exited, which
    they have by the time the replacement is Ready. One that has not is cut off."""
    if follower is None:
        return
    proc, fh = follower
    try:
        proc.wait(timeout=30)
    except subprocess.TimeoutExpired:
        proc.kill()
    with contextlib.suppress(Exception):
        fh.close()


def _iso(t: datetime) -> str:
    return t.isoformat().replace("+00:00", "Z")


def _uid(namespace: str, pod: str) -> str:
    cp = kube.run(["-n", namespace, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}"],
                  check=False)
    return cp.stdout.strip()
