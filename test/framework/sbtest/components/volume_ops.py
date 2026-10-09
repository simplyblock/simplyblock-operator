"""Expanding and snapshotting live pNFS volumes during the run, to judge that each completes
and reaches the clients while fio keeps running.

A pNFS volume's handle is its backing lvol's, so the volume operations a user runs against a
claim address the lvol and work without knowing an export is in front of it. Expansion has a
second half that only the export host can do: the operator re-assembles the export, which
grows XFS in the metadata server's guest, and every client's node plugin re-takes its layout
(csi-driver/internal/csi/node/expand.go). `chaos.volume-ops` exercises the two operations a
pNFS claim supports, under the run's load, on the claims several pods share
(`pnfs.shared_claims`):

- An **expand** grows a shared claim by `grow_gb`, and records when the claim's status
  reached the new size and when `df` in a fio pod mounting it saw it. With no fio pod to
  ask, the expansion fails: its client half would go unobserved.
- A **snapshot** has a fio pod write a marker file of known checksum to a shared claim, takes
  a VolumeSnapshot, and records when it was ready. With `restore` on, the snapshot is restored
  into a block volume formatted XFS, the filesystem the export carries. A pod reads the marker
  back, and the restore and the snapshot are deleted again.

A clone or restore into a pNFS claim is not attempted: the driver refuses it
(csi-driver/internal/csi/controller/pnfsunsupported.go, test plan U-33). Neither is a
snapshot when no VolumeSnapshotClass names the driver, which is recorded as skipped.

The operations run one at a time, each at a seeded time inside the run, and each has to
finish while fio runs. Every operation has a worst-case budget (its timeouts, its
deletions, and `op_overhead_s`), and the schedule fits them one after another before the
last `quiet_tail_s`. An operation that does not fit, or that would still be running when fio
ends, is recorded as skipped rather than run against an idle volume. A deletion is recorded
only once the object is gone, and one that failed or did not finish is a cleanup error, apart
from the operation's own. Everything lands in `volume-ops.json`, which `pnfs.volume-ops`
judges.
"""

from __future__ import annotations

import json
import random
import re
import threading
import time
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from ..core import Component, RunContext, component
from . import kube

LABEL = "sbtest-volops"
GI = 1024 ** 3
_UNITS = {"": 1, "k": 1000, "M": 1000 ** 2, "G": 1000 ** 3, "T": 1000 ** 4,
          "Ki": 1024, "Mi": 1024 ** 2, "Gi": GI, "Ti": 1024 ** 4}


class VolumeOpsPlan:
    """When each operation runs. Pure: it never touches a cluster, so it is testable alone.

    Times are seconds on any monotonic scale shared by `start`, `end`, and the `now` passed
    to `due`. Each operation counted in `counts` runs once. `budgets` is how long an
    operation can take at worst, and the operations are placed one after another so each
    has its whole budget before the next starts and before `end`. The time left over is
    spread at random between them. An operation that does not fit is in `dropped` instead.
    """

    def __init__(self, *, seed: int, start: float, end: float, counts: dict[str, int],
                 budgets: dict[str, float] | None = None) -> None:
        rng = random.Random(seed)
        budgets = budgets or {}
        wanted = [op for op in sorted(counts) for _ in range(int(counts[op]))]
        self.schedule: list[tuple[float, str]] = []
        self.dropped: list[str] = []
        self._next = 0
        if end <= start:
            self.dropped = wanted
            return
        rng.shuffle(wanted)
        kept: list[str] = []
        total = 0.0
        for op in wanted:
            need = float(budgets.get(op, 0.0))
            if total + need <= end - start:
                kept.append(op)
                total += need
            else:
                self.dropped.append(op)
        # Sorted cut points in the time left over: each operation starts after the one before it
        # has had its full budget, and the last one ends by `end`.
        cuts = sorted(rng.uniform(0.0, end - start - total) for _ in kept)
        used = 0.0
        for cut, op in zip(cuts, kept, strict=True):
            self.schedule.append((start + cut + used, op))
            used += float(budgets.get(op, 0.0))

    def due(self, now: float) -> list[str]:
        """The operations whose time has come, each returned once."""
        out = []
        while self._next < len(self.schedule) and self.schedule[self._next][0] <= now:
            out.append(self.schedule[self._next][1])
            self._next += 1
        return out


@dataclass
class _Op:
    op: str
    claim: str
    requested: datetime
    timeout_s: float = 0.0
    skipped: str = ""
    error: str = ""
    target_bytes: int = 0
    capacity_at: datetime | None = None
    client_pod: str = ""
    client_before_b: int = 0
    client_after_b: int = 0
    client_seen_at: datetime | None = None
    snapshot: str = ""
    marker_md5: str = ""
    ready_at: datetime | None = None
    restore_claim: str = ""
    restored_at: datetime | None = None
    restore_md5: str = ""
    restore_deleted: datetime | None = None
    snapshot_deleted: datetime | None = None
    cleanup_error: str = ""

    def fail(self, message: str) -> None:
        self.error = f"{self.error}; {message}" if self.error else message

    def cleanup_failed(self, message: str) -> None:
        self.cleanup_error = f"{self.cleanup_error}; {message}" if self.cleanup_error else message


@component
class VolumeOps(Component):
    """Expand and snapshot live pNFS volumes while fio runs on them."""

    name = "chaos.volume-ops"
    summary = "expand and snapshot live pNFS volumes during the run, restoring the snapshot"
    namespace_options = {"namespace": "test"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,           # the run's test namespace when unset
            "ops": {"expand": 1, "snapshot": 1},
            "grow_gb": 1,
            "restore": True,             # restore each snapshot and read its marker back
            "driver": "csi.simplyblock.io",
            "snapshot_class": None,      # the first class naming `driver` when unset
            "mount": "/data",            # where the fio pods mount their claim
            "marker_mb": 1,
            "image": "alpine:3.20",      # the pod reading a restored marker
            "min_delay_s": 120.0,        # after the timed run starts
            "quiet_tail_s": 300.0,       # before it ends
            "expand_timeout_s": 300.0,
            "snapshot_timeout_s": 300.0,
            "restore_timeout_s": 600.0,
            "delete_timeout_s": 120.0,   # for each object an operation deletes
            "op_overhead_s": 120.0,      # an operation's exec and kubectl calls
            "join_margin_s": 60.0,       # what collect waits beyond the operations' budgets
            "poll_s": 5.0,
            "seed": None,                # random, and logged, when unset
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._ops: list[_Op] = []
        self._current: _Op | None = None
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._loop: threading.Thread | None = None
        self._seed = 0
        self._restore_sc = ""
        #: When the operations have to be done by, on time.monotonic(): fio's end less the
        #: quiet tail. None until start, which leaves operations unbounded in tests.
        self._run_end: float | None = None

    def _budget(self, op: str) -> float:
        """How long op can take at worst: its timeouts, its deletions, and its calls."""
        overhead = float(self.opt("op_overhead_s"))
        delete = float(self.opt("delete_timeout_s"))
        if op == "expand":
            return float(self.opt("expand_timeout_s")) + overhead
        if op == "snapshot":
            if self.opt("restore"):
                return (float(self.opt("snapshot_timeout_s")) + float(self.opt("restore_timeout_s"))
                        + 3 * delete + overhead)
            return float(self.opt("snapshot_timeout_s")) + delete + overhead
        return overhead

    def _deadline(self, timeout_s: float) -> float:
        """The end of a step's timeout, and never after the run's end."""
        deadline = time.monotonic() + timeout_s
        return min(deadline, self._run_end) if self._run_end is not None else deadline

    # ── the schedule ─────────────────────────────────────────────────────────────────

    def start(self, ctx: RunContext) -> None:
        duration = float(ctx.shared.get("run.duration_s") or 0)
        if duration <= 0:
            ctx.log.warn(f"{self.name}: the run has no duration, so no volume operations")
            return
        seed = self.opt("seed")
        self._seed = int(seed) if seed is not None else random.SystemRandom().randrange(2**31)
        t0 = time.monotonic()
        counts = dict(self.opt("ops") or {})
        self._run_end = t0 + duration - float(self.opt("quiet_tail_s"))
        plan = VolumeOpsPlan(seed=self._seed, start=t0 + float(self.opt("min_delay_s")),
                             end=self._run_end, counts=counts,
                             budgets={op: self._budget(op) for op in counts})
        for op in plan.dropped:
            with self._lock:
                self._ops.append(_Op(op=op, claim="", requested=datetime.now(UTC),
                                     skipped=f"its worst case of {self._budget(op):.0f}s does "
                                             "not fit in the run before fio ends"))
        if not plan.schedule:
            ctx.log.warn(f"{self.name}: a {duration:.0f}s run leaves no window for volume "
                         "operations")
            return
        ctx.log.info(f"{self.name}: seed {self._seed}, " + ", ".join(
            f"{op} at {t - t0:.0f}s" for t, op in plan.schedule)
            + (f"; dropped {', '.join(plan.dropped)}" if plan.dropped else ""))

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                for op in plan.due(time.monotonic()):
                    self._run_op(ctx, op)
                self._stop.wait(1.0)

        self._loop = threading.Thread(target=loop, name="volume-ops", daemon=True)
        self._loop.start()

    def _run_op(self, ctx: RunContext, op: str) -> None:
        with self._lock:
            n = len(self._ops) + 1
        claims = sorted(ctx.shared.get("pnfs.shared_claims") or [])
        record = _Op(op=op, claim="", requested=datetime.now(UTC))
        with self._lock:
            self._ops.append(record)
        if not claims:
            record.skipped = "workload.pnfs published no shared claims"
            return
        record.claim = random.Random(self._seed + n).choice(claims)
        if self._run_end is not None and time.monotonic() + self._budget(op) > self._run_end:
            record.skipped = (f"its worst case of {self._budget(op):.0f}s would not finish "
                              "before fio ends")
            return
        with self._lock:
            self._current = record
        try:
            if op == "expand":
                self._expand(ctx, record)
            elif op == "snapshot":
                self._snapshot(ctx, record, n)
            else:
                record.skipped = f"unknown operation {op!r}"
        except Exception as e:  # noqa: BLE001
            record.fail(str(e))
        finally:
            with self._lock:
                self._current = None
        ctx.log.info(f"{self.name}: {op} of {record.claim} "
                     + (f"skipped: {record.skipped}" if record.skipped
                        else f"failed: {record.error}" if record.error
                        else "done" if _complete(record) else "incomplete within its timeout"))

    # ── expand ───────────────────────────────────────────────────────────────────────

    def _expand(self, ctx: RunContext, record: _Op) -> None:
        ns = str(self.opt("namespace"))
        record.timeout_s = float(self.opt("expand_timeout_s"))
        client = self._client(ns, record.claim)
        if not client:
            record.fail(f"no running pod mounts {record.claim} to observe the expansion")
            return
        claim = _get_json(ns, "pvc", record.claim)
        request = _bytes(claim.get("spec", {}).get("resources", {}).get("requests", {})
                         .get("storage", ""))
        record.target_bytes = request + int(self.opt("grow_gb")) * GI
        record.client_pod = client[0]
        record.client_before_b = self._df(ns, client)
        record.requested = datetime.now(UTC)
        patch = {"spec": {"resources": {"requests": {"storage": str(record.target_bytes)}}}}
        cp = kube.run(["-n", ns, "patch", "pvc", record.claim, "--type=merge", "-p",
                       json.dumps(patch)], check=False)
        if cp.returncode != 0:
            record.fail(f"patching {record.claim}: {cp.stderr.strip() or cp.returncode}")
            return
        deadline = self._deadline(record.timeout_s)
        while record.capacity_at is None or record.client_seen_at is None:
            if record.capacity_at is None:
                status = _get_json(ns, "pvc", record.claim).get("status", {})
                if _bytes(status.get("capacity", {}).get("storage", "")) >= record.target_bytes:
                    record.capacity_at = datetime.now(UTC)
            if record.client_seen_at is None:
                size = self._df(ns, client)
                if size > record.client_before_b:
                    record.client_after_b, record.client_seen_at = size, datetime.now(UTC)
            if time.monotonic() >= deadline:
                return
            time.sleep(float(self.opt("poll_s")))

    def _client(self, ns: str, claim: str) -> tuple[str, str] | None:
        """A running fio pod mounting claim, and its first container."""
        cp = kube.run(["-n", ns, "get", "pods", "-l", "app=fio", "-o", "json"], check=False)
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

    def _df(self, ns: str, client: tuple[str, str]) -> int:
        """The size of the client's mount in bytes. Anything but a byte count fails: read as
        0, a failed read would make the next good one look like the client seeing growth."""
        # The KiB field as df prints it, multiplied here: busybox awk formats %d as a 32-bit
        # integer, which turned a 22.5 GB mount into -2147483648.
        out = kube.exec_sh(ns, client[0], f"df -kP {self.opt('mount')} | awk 'NR==2{{print $2}}'",
                           container=client[1], timeout=60).strip()
        if not out.isdigit() or int(out) <= 0:
            raise RuntimeError(f"df of {self.opt('mount')} in {client[0]} returned no size: "
                               f"{out!r}")
        return int(out) * 1024

    # ── snapshot ─────────────────────────────────────────────────────────────────────

    def _snapshot(self, ctx: RunContext, record: _Op, n: int) -> None:
        ns = str(self.opt("namespace"))
        record.timeout_s = float(self.opt("snapshot_timeout_s"))
        snapclass = self._snapshot_class()
        if not snapclass:
            record.skipped = f"no VolumeSnapshotClass names {self.opt('driver')}"
            return
        client = self._client(ns, record.claim)
        if not client:
            record.skipped = f"no running pod mounts {record.claim} to write the marker"
            return
        marker = f"{ctx.run_id}-volops-{n}.marker"
        path = f"{self.opt('mount')}/{marker}"
        try:
            record.marker_md5 = kube.exec_sh(
                ns, client[0], f"head -c {int(self.opt('marker_mb'))}M /dev/urandom > {path}.tmp "
                f"&& mv {path}.tmp {path} && sync && md5sum {path} | cut -d' ' -f1",
                container=client[1], timeout=120).strip()
            if not record.marker_md5:
                record.fail(f"writing the marker in {client[0]} returned no checksum")
                return
            name = f"{ctx.run_id}-volops-{n}"
            record.requested = datetime.now(UTC)
            cp = kube.run(["-n", ns, "apply", "-f", "-"], check=False, stdin=json.dumps({
                "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
                "metadata": {"name": name, "labels": {LABEL: ctx.run_id}},
                "spec": {"volumeSnapshotClassName": snapclass,
                         "source": {"persistentVolumeClaimName": record.claim}}}))
            if cp.returncode != 0:
                record.fail(f"creating VolumeSnapshot {name}: {cp.stderr.strip() or cp.returncode}")
                return
            # Only now: a snapshot that was never created cannot be left behind.
            record.snapshot = name
            deadline = self._deadline(record.timeout_s)
            while True:
                cp = kube.run(["-n", ns, "get", "volumesnapshot", record.snapshot, "-o",
                               "jsonpath={.status.readyToUse}"], check=False)
                if cp.stdout.strip() == "true":
                    record.ready_at = datetime.now(UTC)
                    break
                if time.monotonic() >= deadline:
                    return
                time.sleep(float(self.opt("poll_s")))
            if self.opt("restore"):
                self._restore(ctx, record, marker)
        finally:
            kube.exec_sh(ns, client[0], f"rm -f {path} {path}.tmp", container=client[1],
                         timeout=60)
            if record.snapshot and self._delete(ns, "volumesnapshot", record.snapshot, record):
                record.snapshot_deleted = datetime.now(UTC)

    def _snapshot_class(self) -> str:
        named = str(self.opt("snapshot_class") or "")
        cp = kube.run(["get", "volumesnapshotclass", "-o", "json"], check=False)
        if cp.returncode != 0:
            return ""
        for c in json.loads(cp.stdout or "{}").get("items", []):
            name = str(c.get("metadata", {}).get("name", ""))
            if c.get("driver") == self.opt("driver") and (not named or name == named):
                return name
        return ""

    def _restore(self, ctx: RunContext, record: _Op, marker: str) -> None:
        """Restore the snapshot into a block volume formatted XFS and read the marker back."""
        ns = str(self.opt("namespace"))
        sc = self._restore_class(ctx)
        claim = _get_json(ns, "pvc", record.claim)
        size = claim.get("status", {}).get("capacity", {}).get("storage") or \
            claim.get("spec", {}).get("resources", {}).get("requests", {}).get("storage", "")
        record.restore_claim = f"{record.snapshot}-restore"
        reader = f"{record.snapshot}-reader"
        labels = {LABEL: ctx.run_id}
        kube.run(["-n", ns, "apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "v1", "kind": "PersistentVolumeClaim",
            "metadata": {"name": record.restore_claim, "labels": labels},
            "spec": {"accessModes": ["ReadWriteOnce"], "storageClassName": sc,
                     "resources": {"requests": {"storage": size}},
                     "dataSource": {"apiGroup": "snapshot.storage.k8s.io",
                                    "kind": "VolumeSnapshot", "name": record.snapshot}}}))
        kube.run(["-n", ns, "apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": reader, "labels": labels},
            "spec": {"restartPolicy": "Never", "terminationGracePeriodSeconds": 5,
                     "containers": [{"name": "reader", "image": str(self.opt("image")),
                                     "command": ["sleep", "3600"],
                                     "volumeMounts": [{"name": "data", "mountPath": "/data"}]}],
                     "volumes": [{"name": "data", "persistentVolumeClaim": {
                         "claimName": record.restore_claim}}]}}))
        try:
            deadline = self._deadline(float(self.opt("restore_timeout_s")))
            while True:
                cp = kube.run(["-n", ns, "get", "pod", reader, "-o",
                               "jsonpath={.status.phase}"], check=False)
                if cp.stdout.strip() == "Running":
                    break
                if time.monotonic() >= deadline:
                    return
                time.sleep(float(self.opt("poll_s")))
            record.restore_md5 = kube.exec_sh(ns, reader, f"md5sum /data/{marker} | cut -d' ' -f1",
                                              timeout=120).strip()
            record.restored_at = datetime.now(UTC)
        finally:
            self._delete(ns, "pod", reader, record)
            if self._delete(ns, "pvc", record.restore_claim, record):
                record.restore_deleted = datetime.now(UTC)

    def _restore_class(self, ctx: RunContext) -> str:
        """A block class with the pNFS class's parameters, formatting XFS like the export."""
        if self._restore_sc:
            return self._restore_sc
        source = str(ctx.shared.get("pnfs.storageclass") or "")
        cp = kube.run(["get", "sc", source, "-o", "json"], check=False)
        if cp.returncode != 0 or not cp.stdout:
            raise RuntimeError(f"StorageClass {source or '(none)'} to restore from not found")
        src = json.loads(cp.stdout)
        name = f"sbtest-{ctx.run_id}-volops-xfs"
        kube.run(["delete", "sc", name, "--ignore-not-found"], check=False)
        kube.run(["apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
            "metadata": {"name": name, "labels": {"sbtest-run": "true", LABEL: ctx.run_id}},
            "provisioner": src.get("provisioner", self.opt("driver")),
            "parameters": dict(src.get("parameters", {}),
                               **{"csi.storage.k8s.io/fstype": "xfs"}),
            "reclaimPolicy": "Delete", "volumeBindingMode": "WaitForFirstConsumer",
            "allowVolumeExpansion": True}))
        self._restore_sc = name
        return name

    def _delete(self, ns: str, kind: str, name: str, record: _Op) -> bool:
        """Delete one object and wait until it is gone. True only then: a deletion that was
        refused or that a finalizer still holds is a cleanup error on the record."""
        timeout = float(self.opt("delete_timeout_s"))
        cp = kube.run(["-n", ns, "delete", kind, name, "--ignore-not-found", "--wait=true",
                       f"--timeout={int(timeout)}s"], check=False, timeout=int(timeout) + 30)
        if cp.returncode != 0:
            record.cleanup_failed(f"deleting {kind} {name}: {cp.stderr.strip() or cp.returncode}")
            return False
        return True

    # ── the end of the run ───────────────────────────────────────────────────────────

    def _join_budget(self) -> float:
        """How long the worker can still need: every configured operation's worst case."""
        counts = dict(self.opt("ops") or {})
        return (sum(int(n) * self._budget(op) for op, n in counts.items())
                + float(self.opt("join_margin_s")))

    def stop(self, ctx: RunContext) -> None:
        # Only the schedule stops. An operation under way finishes, and collect waits for it.
        self._stop.set()

    def collect(self, ctx: RunContext) -> None:
        if self._loop:
            self._loop.join(timeout=self._join_budget())
            if self._loop.is_alive():
                # Kept, so teardown waits for it too rather than deleting under it.
                with self._lock:
                    if self._current is not None:
                        self._current.fail("still running when the run was collected")
                ctx.log.warn(f"{self.name}: an operation was still running after "
                             f"{self._join_budget():.0f}s")
            else:
                self._loop = None
        with self._lock:
            ops = list(self._ops)
        ctx.save_json("volume-ops.json", {"seed": self._seed, "ops": [{
            "op": o.op, "claim": o.claim, "requested": _iso(o.requested),
            "timeout_s": o.timeout_s, "skipped": o.skipped, "error": o.error,
            "target_bytes": o.target_bytes, "capacity_at": _iso(o.capacity_at),
            "client_pod": o.client_pod, "client_before_b": o.client_before_b,
            "client_after_b": o.client_after_b, "client_seen_at": _iso(o.client_seen_at),
            "snapshot": o.snapshot, "marker_md5": o.marker_md5, "ready_at": _iso(o.ready_at),
            "restore_claim": o.restore_claim, "restored_at": _iso(o.restored_at),
            "restore_md5": o.restore_md5, "restore_deleted": _iso(o.restore_deleted),
            "snapshot_deleted": _iso(o.snapshot_deleted),
            "cleanup_error": o.cleanup_error} for o in ops]})
        ctx.log.info(f"{self.name}: {len(ops)} volume operation(s) (seed {self._seed})")

    def teardown(self, ctx: RunContext) -> None:
        # Whatever an operation created and did not delete, a failed run included.
        ns, selector = str(self.opt("namespace")), f"{LABEL}={ctx.run_id}"
        self._stop.set()
        if self._loop and self._loop.is_alive():
            self._loop.join(timeout=self._join_budget())
            if self._loop.is_alive():
                ctx.log.warn(f"{self.name}: deleting the operations' objects while one is "
                             "still running")
        for kind in ("pod", "pvc", "volumesnapshot"):
            kube.run(["-n", ns, "delete", kind, "-l", selector, "--ignore-not-found"],
                     check=False, timeout=300)
        kube.run(["delete", "sc", "-l", selector, "--ignore-not-found"], check=False)


def _complete(o: _Op) -> bool:
    if o.op == "expand":
        return o.capacity_at is not None and o.client_seen_at is not None
    return o.ready_at is not None and (not o.restore_claim or o.restored_at is not None)


def _get_json(ns: str, kind: str, name: str) -> dict:
    cp = kube.run(["-n", ns, "get", kind, name, "-o", "json"], check=False)
    if cp.returncode != 0 or not cp.stdout:
        return {}
    return dict(json.loads(cp.stdout))


def _bytes(quantity: object) -> int:
    """A Kubernetes quantity in bytes, or 0 for one that is absent or not understood."""
    m = re.fullmatch(r"(\d+)([A-Za-z]{0,2})", str(quantity or "").strip())
    if not m or m.group(2) not in _UNITS:
        return 0
    return int(m.group(1)) * _UNITS[m.group(2)]


def _iso(t: datetime | None) -> str | None:
    return t.isoformat().replace("+00:00", "Z") if t else None
