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


#: What host.dmesg writes into a node's kernel log, followed by `wall=<ISO time>`, just before
#: reading it back. dmesg renders a line's time from the boot time and the kernel's own clock,
#: which is not NTP-disciplined and drifts: nodes up for weeks run minutes behind. The marker
#: carries the wall time it was written at and comes back with the time dmesg renders for it,
#: and the difference is what every other line of that log is corrected by. It stays in the
#: collected file, so an archived run carries its own correction.
KERNEL_CLOCK_PROBE = "sbtest-clock-probe"


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
    start: datetime | None = None   # when fio's timed run began, from job_start
    runtime_s: float = 0.0          # how long it ran, from job_runtime


@dataclass(frozen=True)
class BlockSample:
    """One reading of one NVMe namespace's I/O counters on one node, from sysfs `stat`.

    Counters are cumulative since the device appeared, so a reading means nothing alone and
    everything against the one before it.
    """

    ts: datetime
    node: str
    device: str        # the head device, nvmeXnY
    uuid: str          # the namespace UUID, which for a simplyblock volume is the lvol's
    read_ios: int
    read_sectors: int
    write_ios: int
    write_sectors: int


@dataclass(frozen=True)
class PnfsVolume:
    """A pNFS volume a run provisioned, and the client nodes that should write to it."""

    claim: str
    lvol: str
    shared: bool
    nodes: list[str] = field(default_factory=list)
    # fio instance (its evidence directory) -> the node it ran on
    instances: dict[str, str] = field(default_factory=dict)


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


@dataclass(frozen=True)
class FenceWrite:
    """One write of the fence scenario's probe writer on the partitioned node."""

    ts: datetime
    ok: bool
    detail: str = ""


@dataclass(frozen=True)
class Fence:
    """What the fence scenario did and saw (chaos.fence).

    The victim node is cut off from the metadata server while its NVMe-oF paths stay up,
    and the recaller changes the size of the file the victim's probe writer holds a layout
    on. nfsd recalls the victim's layout, cannot reach it, and fences it by preempting its
    reservation key, after which no write of the victim's may land.
    """

    victim_node: str
    recaller_node: str
    claim: str
    victim_pod: str = ""
    recaller_pod: str = ""
    file: str = ""
    rules: tuple[str, ...] = ()
    partitioned: datetime | None = None
    healed: datetime | None = None
    truncate_issued: datetime | None = None
    #: None when the truncate did not return within truncate_timeout_s.
    truncate_returned: datetime | None = None
    truncate_timeout_s: float = 0.0
    #: The truncate's exit status and first error line, empty when it succeeded.
    truncate_error: str = ""
    writes: tuple[FenceWrite, ...] = ()
    #: Why the scenario could not run to the end, empty when it did.
    error: str = ""


@dataclass(frozen=True)
class Restart:
    """One pod a run restarted on purpose, and when its replacement came back."""

    target: str
    pod: str
    node: str
    deleted: datetime
    ready: datetime | None = None
    replacement: str = ""
    #: The victim's log, followed from its deletion until its containers exited.
    log: str = ""
    #: The victim's pod IP and its replacement's, "" when the run did not record them. A
    #: Service routes to the pod IP, so a flow still translated to the old one after the
    #: replacement is Ready never reaches it.
    ip: str = ""
    replacement_ip: str = ""


@dataclass(frozen=True)
class ChurnPod:
    """One short-lived pod that joined a pNFS volume, ran fio, and left.

    `own_volume` says whether it brought a volume of its own, which it deleted when it
    left, or used one the run's long-lived pods share. For an own volume, `pv_gone` and
    `export_gone` say whether the PersistentVolume and the NFSExport behind it were gone by
    the end of the run, None when that was never checked, and `gone_s` how long after the
    claim's deletion both were.
    """

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
    pv_gone: bool | None = None
    export_gone: bool | None = None
    gone_s: float | None = None
    error: str = ""


@dataclass(frozen=True)
class Registrant:
    """One host registered on a namespace's NVMe reservation, with the key it holds."""

    hostid: str
    rkey: int
    holder: bool = False


@dataclass(frozen=True)
class NamespaceReservation:
    """A namespace's NVMe reservation as one node's `nvme resv-report` saw it.

    The state is the target's, so every node attached to the namespace reports the same
    registrants. A pNFS metadata server holds the reservation, and each client registers
    the key nfsd gave it, whose upper 32 bits are nfsd's boot time.
    """

    node: str
    device: str
    uuid: str
    rtype: int
    generation: int
    registrants: tuple[Registrant, ...] = ()


@dataclass(frozen=True)
class NfsSample:
    """One fio instance's NFS client counters at one moment. The counts are the pod's
    mount's, which every instance in the pod writes through."""

    ts: datetime
    instance: str   # the instance's evidence directory, such as "<run>-fio-0-c0"
    pod: str
    container: str
    layoutget: int
    read: int
    write: int
    connects: int = 0


@dataclass(frozen=True)
class ConntrackSample:
    """One NFS flow in a node's connection tracking table at one moment.

    The orig tuple is the client's view (its address and source port to the export's
    Service address), and reply_src is where the node translated it: the MDS pod. A row
    with state "NONE" and no tuple records that the node was read and held no NFS flow,
    which keeps a node with nothing to find apart from a node never read.
    """

    ts: datetime
    node: str
    state: str
    orig_src: str = ""
    orig_sport: int = 0
    orig_dst: str = ""
    orig_dport: int = 0
    reply_src: str = ""


@dataclass(frozen=True)
class DeployedImage:
    """One container of the deployment, and the image it actually ran."""

    namespace: str
    pod: str
    container: str
    image: str      # the reference the pod asked for, usually a tag
    image_id: str   # what the runtime resolved it to, with the digest


@dataclass(frozen=True)
class NodeVersion:
    """What one node runs underneath the pods."""

    node: str
    kernel: str
    os_image: str
    runtime: str
    kubelet: str


@dataclass(frozen=True)
class Versions:
    """What was deployed when the run started, so a result can be tied to it."""

    server: str
    images: tuple[DeployedImage, ...] = ()
    nodes: tuple[NodeVersion, ...] = ()


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

    def nfs_ops(self, pod: str) -> dict[str, int]:
        """The per-operation counts of the NFS mount a fio instance wrote through, e.g.
        {"LAYOUTGET": 3, "WRITE": 0}. Empty when the instance used no NFS mount."""
        ...

    def block_samples(self) -> list[BlockSample]:
        """NVMe namespace I/O counters per node over the run, oldest first."""
        ...

    def pnfs_volumes(self) -> list[PnfsVolume]:
        """The pNFS volumes the run provisioned, with their consuming nodes."""
        ...

    def reservations_pre(self) -> list[NamespaceReservation]:
        """Every namespace's NVMe reservation when the run started, one entry per node."""
        ...

    def reservations_post(self) -> list[NamespaceReservation]:
        """Every namespace's NVMe reservation when the run ended, one entry per node."""
        ...

    def run_window(self) -> tuple[datetime | None, datetime | None]:
        """When the run started and ended, or (None, None) if not known.

        Needed by any detector reading evidence that outlives the run — dmesg is a ring
        buffer covering hours, so without a window a detector counts the previous runs'
        damage as this one's. A detector that gets (None, None) must mark what it finds
        Attribution.UNKNOWN rather than assume.
        """
        ...

    def control_events(self) -> list[ControlEvent]: ...

    def restarts(self) -> list[Restart]:
        """The pods the run restarted on purpose, oldest first."""
        ...

    def fence(self) -> Fence | None:
        """The fence scenario's record, or None when it did not run."""
        ...

    def churn(self) -> list[ChurnPod]:
        """The short-lived pods that joined and left pNFS volumes, oldest first."""

    def versions(self) -> Versions | None:
        """What was deployed, or None when the run did not record it."""
        ...

    def nfs_timeline(self) -> list[NfsSample]:
        """Each fio instance's NFS client counters over the run, oldest first."""
        ...

    def conntrack(self) -> list[ConntrackSample]:
        """Each node's NFS flows in its connection tracking table over the run, oldest
        first."""
        ...

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
