"""Evidence — the only thing a detector is allowed to read.

Detectors do not touch the cluster and do not know how anything was collected. They are
pure functions from Evidence to findings, and that is the whole point: the same detector
runs against a live run's in-memory state and against a four-hour archive on disk, so a
check can be fixed and re-tried against the run that motivated it instead of only against
the next one. Reproducing a verdict offline was the single biggest gap in the harness this
framework grew out of.

Everything here is lazy. A run's SPDK logs are tens of megabytes per node and most
detectors never open them, so `container_log` yields lines and `ana_samples` is fetched per
migration rather than eagerly for all of them.
"""

from __future__ import annotations

from collections.abc import Iterable, Iterator
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Protocol, runtime_checkable

# ── the value types detectors reason about ──────────────────────────────────────────


@dataclass(frozen=True)
class AnaSample:
    """One controller's state on one consuming host at one instant."""

    ts: datetime
    node: str
    address: str  # "<ip>:<port>" — an ip alone names a node, only ip:port names a path
    state: str  # controller state: live / connecting / resetting / ...
    ana: dict[int, str] = field(default_factory=dict)  # nsid -> ANA state
    phase: str = ""  # the migration's phase at that instant, when known
    role: str = ""  # source / target / other, when the collector knew it

    ACCESSIBLE = ("optimized", "non-optimized", "nonoptimized", "non_optimized")

    def accessible_nsids(self) -> set[int]:
        return {n for n, a in self.ana.items() if a in self.ACCESSIBLE}

    @property
    def ip(self) -> str:
        return self.address.rsplit(":", 1)[0]

    @property
    def port(self) -> str:
        _, _, p = self.address.rpartition(":")
        return p


@dataclass
class Migration:
    """One migration attempt, as the timeline saw it."""

    name: str
    start: datetime
    end: datetime | None = None
    phase: str = ""  # Completed / Failed / TIMEOUT / ...
    source: str = ""  # storage-node uuid
    target: str = ""
    pv: str = ""
    pod: str = ""
    members: list[str] = field(default_factory=list)  # PVs moving together
    error: str = ""
    # Host-observed cutover instants per node, when the collector derived them.
    cutover: dict[str, datetime] = field(default_factory=dict)

    @property
    def batch(self) -> bool:
        return len(self.members) > 1

    def covers(self, ts: datetime, lag: timedelta = timedelta(0)) -> bool:
        """Whether ts falls in this migration's window, optionally extended by `lag`.

        The lag exists for symptoms that are *detected* later than they happen — an fio
        verify failure surfaces when fio next reads the block, seconds to tens of seconds
        after the write was lost. Without it a migration's own losses get filed under "no
        migration was running", which is exactly how a completed-but-corrupting migration
        stayed invisible.
        """
        if ts < self.start:
            return False
        end = self.end or self.start
        return ts <= end + lag


@dataclass
class FioJob:
    """One fio job's outcome for one pod."""

    pod: str
    error: int = 0  # errno fio ended with, 0 = clean
    total_iops: float = 0.0
    read_iops: float = 0.0
    write_iops: float = 0.0


@dataclass(frozen=True)
class IopsSample:
    """One second of one pod's I/O, from the fio time series."""

    offset_s: int  # seconds since fio started
    wall: datetime | None
    total_iops: float


@dataclass(frozen=True)
class ControlEvent:
    """One control-plane event — a status change, an object creation, a task update.

    The control plane's own account of what it thought was happening, which is the other half
    of every host-side symptom: "the path went away" and "the control plane decided the node
    was down" are the same incident told from two ends.
    """

    ts: datetime
    level: str            # Info / Warning / Error
    kind: str             # STATUS_CHANGE / OBJ_CREATED / ...
    message: str
    subject: str = ""     # node / volume / task id, when the event names one


@dataclass(frozen=True)
class LogSpan:
    """What time range a collected log actually covers.

    Exists because the answer is routinely "less than the run". A log-based verdict over a log
    that only covers the last forty minutes of a four-hour run is not a verdict, and nothing
    else in the evidence makes that visible.
    """

    name: str
    first: datetime | None
    last: datetime | None
    lines: int = 0


@dataclass(frozen=True)
class NvmeController:
    """One NVMe controller on one host, as sysfs reports it.

    The shape that matters for leak detection: a controller can be `live` and serve no
    namespace at all, which looks connected from every angle a connect checks.
    """

    node: str
    name: str  # "nvme7"
    nqn: str
    address: str  # "<ip>:<port>"
    state: str  # live / connecting / ...
    namespaces: dict[int, str] = field(default_factory=dict)  # nsid -> ana state
    ctrl_loss_tmo: int | None = None

    @property
    def serves_nothing(self) -> bool:
        return not self.namespaces


# ── the contract ────────────────────────────────────────────────────────────────────


@runtime_checkable
class Evidence(Protocol):
    """What a detector may ask for. Every accessor may legitimately return nothing.

    A detector that needs evidence a run does not have must *say so* (see
    `Detector.detect` and `Report.skip`) rather than return no findings — silence is how a
    check that cannot run gets mistaken for a check that passed.
    """

    run_id: str
    outdir: str

    def migrations(self) -> list[Migration]: ...

    def ana_samples(self, migration: str) -> list[AnaSample]: ...

    def fio_jobs(self) -> list[FioJob]: ...

    def fio_timeseries(self, pod: str) -> list[IopsSample]: ...

    def fio_log(self, pod: str) -> Iterator[str]: ...

    def container_logs(self) -> list[str]:
        """Names of the container logs available, e.g. "spdk-4420", "operator"."""
        ...

    def container_log(self, name: str) -> Iterator[str]: ...

    def nvme_controllers(self) -> list[NvmeController]: ...

    def pods(self) -> list[str]: ...

    def run_window(self) -> tuple[datetime | None, datetime | None]:
        """When the run started and ended, or (None, None) if not known.

        Needed by any detector reading evidence that outlives the run — dmesg is a ring
        buffer covering hours, so without a window a detector counts the previous runs'
        damage as this one's. A detector that gets (None, None) must mark what it finds
        Attribution.UNKNOWN rather than assume.
        """
        ...

    def control_events(self) -> list[ControlEvent]: ...

    def log_spans(self) -> list[LogSpan]:
        """The time range each collected log covers. Empty when not determinable."""
        ...

    def cluster_uuid(self) -> str:
        """The cluster this run ran against, or "" when unknown.

        Present for one reason: an NQN names its cluster, so a controller whose NQN names a
        *different* cluster is leaked beyond any doubt — no threshold, no topology, no
        "might be a transient reconnect". See detectors/kernel.py::ForeignCluster.
        """
        ...


# ── helpers shared by detectors ─────────────────────────────────────────────────────


def freeze_windows(samples: Iterable[AnaSample],
                   expected_nsids: set[int] | None = None) -> list[tuple[datetime, float]]:
    """The windows in which some namespace had no accessible path on a node.

    Returns (start, seconds) per window, taken from the node that saw the most of them.
    Per node rather than merged across nodes: every consuming host sees the same freeze, so
    the count is how many times the volume froze, not how many hosts noticed.

    Zero-length windows are dropped. A window one sample wide began and ended between two
    samples, so counting it would make the result depend on the sampling interval rather
    than on what the volume did.

    This is the primitive behind the freeze-count detector, which is the sharpest predictor
    of silent write loss found so far — see detectors/ana.py.
    """
    by_node: dict[str, dict[datetime, set[int]]] = {}
    for s in samples:
        if not s.ana:
            continue
        per_ts = by_node.setdefault(s.node, {})
        per_ts.setdefault(s.ts, set()).update(s.accessible_nsids())

    best: list[tuple[datetime, float]] = []
    for per_ts in by_node.values():
        times = sorted(per_ts)
        if not times:
            continue
        want = expected_nsids or {n for acc in per_ts.values() for n in acc}
        if not want:
            continue
        windows: list[tuple[datetime, float]] = []
        start: datetime | None = None
        for t in times:
            if want - per_ts[t]:
                if start is None:
                    start = t
            elif start is not None:
                windows.append((start, (t - start).total_seconds()))
                start = None
        if start is not None:
            windows.append((start, (times[-1] - start).total_seconds()))
        windows = [w for w in windows if w[1] > 0]
        if len(windows) > len(best):
            best = windows
    return best


def attribute(migrations: list[Migration], ts: datetime,
              lag: timedelta = timedelta(0)) -> Migration | None:
    """The migration a symptom at `ts` belongs to, allowing for detection lag."""
    for m in migrations:
        if m.covers(ts, lag):
            return m
    return None


def attribute_window(migrations: list[Migration], start: datetime, end: datetime,
                     lag: timedelta = timedelta(0)) -> Migration | None:
    """The migration a symptom that *lasted* belongs to: the one it shares most seconds with.

    Not the same question as `attribute`, and answering it with `attribute(start)` is what
    made outages disappear. A gap does not have to begin inside a migration's window to
    belong to it — the host goes dry a few seconds before the operator records the migration
    as started — so testing only the first second files those gaps under "no migration was
    running", which reads as "the cluster is unwell" rather than "the cutover cost this".

    Overlap is measured, not merely tested, because a long window can touch two migrations;
    the one holding most of it is the one worth naming. Zero counts as an overlap: a
    zero-length window inside a migration, and a window that only touches one, are both
    inside rather than outside.
    """
    t0, t1 = start.timestamp(), end.timestamp()
    best: Migration | None = None
    best_overlap: float | None = None
    for m in migrations:
        m_end = (m.end or m.start) + lag
        overlap = min(t1, m_end.timestamp()) - max(t0, m.start.timestamp())
        if overlap < 0:
            continue
        if best_overlap is None or overlap > best_overlap:
            best, best_overlap = m, overlap
    return best
