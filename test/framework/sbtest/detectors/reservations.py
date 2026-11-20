"""Whether a pNFS namespace still carries registrations a metadata server restart left behind.

nfsd hands each client a reservation key whose upper 32 bits are nfsd's boot time, and the
client registers it on the export's namespace. On NVMe a client's registration outlives the
server that issued it: releasing the device sends a Replace with a zero key rather than an
Unregister. After a metadata server restart, the registration from the earlier boot is still
there and blocks the client from registering its new key. The client then marks the device
unavailable and sends its I/O through the metadata server, and nothing logs it. The guest's
nfsd fences such keys when it reserves the device, so a stale key at the end of a run means
that fencing did not happen.

A key is stale when its boot time is older than the newest boot among the namespace's
client keys, or older than an MDS restart the run recorded (`chaos.restart`): after a
restart no client may have managed to register at all, and then every key is from the
earlier boot. The metadata server's own key carries no boot time and is never judged.
"""

from __future__ import annotations

from collections.abc import Iterable
from datetime import UTC, datetime

from ..core import (
    Attribution,
    Detector,
    Evidence,
    Finding,
    NamespaceReservation,
    Registrant,
    Restart,
    SkipDetector,
    critical,
    detector,
    info,
    warning,
)

#: The key nfsd registers and reserves with itself (NFSD_MDS_PR_KEY).
MDS_KEY = 0x0100000000000000


def _boot(key: int) -> int:
    return key >> 32


def _clients(ns: NamespaceReservation) -> list[Registrant]:
    return [r for r in ns.registrants if r.rkey not in (0, MDS_KEY)]


def _stale(ns: NamespaceReservation, restarted: int = 0) -> list[Registrant]:
    """The client registrations on `ns` from an earlier boot than the newest, or than the
    last MDS restart (epoch seconds, 0 for none)."""
    clients = _clients(ns)
    if not clients:
        return []
    floor = max(max(_boot(r.rkey) for r in clients), restarted)
    return [r for r in clients if _boot(r.rkey) < floor]


def _one_per_namespace(snapshot: list[NamespaceReservation]) -> dict[str, NamespaceReservation]:
    """Every node attached to a namespace reports the target's same registrants, so one
    report per namespace is judged."""
    out: dict[str, NamespaceReservation] = {}
    for ns in sorted(snapshot, key=lambda n: (n.uuid, n.node)):
        out.setdefault(ns.uuid, ns)
    return out


def _last_mds_restart(restarts: list[Restart]) -> int:
    times = [int(r.deleted.timestamp()) for r in restarts if r.target == "mds"]
    return max(times) if times else 0


@detector
class StaleReservations(Detector):
    name = "nvme.stale-reservations"
    summary = "a pNFS namespace still carrying a registration from an earlier MDS boot"

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        post = ev.reservations_post()
        if not post:
            raise SkipDetector("no reservation snapshot after the run; enable nvme.reservations")
        lvols = {v.lvol for v in ev.pnfs_volumes() if v.lvol}
        restarted = _last_mds_restart(ev.restarts())
        pre_stale = {(uuid, r.hostid, r.rkey)
                     for uuid, ns in _one_per_namespace(ev.reservations_pre()).items()
                     for r in _stale(ns)}

        for uuid, ns in _one_per_namespace(post).items():
            if lvols and uuid not in lvols:
                continue
            clients = _clients(ns)
            holders = [r.hostid for r in ns.registrants if r.holder]
            stale = _stale(ns, restarted)
            if stale:
                inherited = all((uuid, r.hostid, r.rkey) in pre_stale for r in stale)
                yield critical(
                    self.name,
                    title=f"{len(stale)} registration(s) from an earlier metadata server boot",
                    subject=uuid,
                    detail="; ".join(f"{r.hostid}: key 0x{r.rkey:016x} from the boot at "
                                     f"{_when(_boot(r.rkey))}" for r in stale),
                    evidence={"stale": sorted(r.hostid for r in stale),
                              "keys": [f"0x{r.rkey:016x}" for r in stale],
                              "device": ns.device, "node": ns.node},
                    attribution=Attribution.PRE_EXISTING if inherited else Attribution.RUN,
                    note="each stale registration blocks its host from registering the key the "
                         "current boot hands out, so that client's I/O goes through the "
                         "metadata server; nfsd fences them when it reserves the device")
            if clients and not holders:
                yield warning(
                    self.name, title="clients registered on a namespace nobody reserves",
                    subject=uuid,
                    detail=f"{len(clients)} client registration(s), reservation type "
                           f"{ns.rtype}",
                    evidence={"clients": sorted(r.hostid for r in clients),
                              "device": ns.device, "node": ns.node},
                    note="the metadata server takes the reservation on its first device "
                         "lookup, so a registered client without a holder means the server "
                         "never did, or lost it")
            yield info(
                self.name, title=f"{len(clients)} client registration(s)",
                subject=uuid,
                detail=f"held by {', '.join(holders) or 'nobody'} (type {ns.rtype}, "
                       f"generation {ns.generation}), seen from {ns.node} as {ns.device}",
                evidence={"clients": sorted(r.hostid for r in clients), "holders": holders})


def _when(epoch: int) -> str:
    return datetime.fromtimestamp(epoch, tz=UTC).strftime("%Y-%m-%d %H:%M:%S UTC")
