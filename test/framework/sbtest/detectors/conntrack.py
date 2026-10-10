"""Whether NFS flows kept going to the metadata server's old pod after it was replaced.

The node translates a connection to an export's Service address into the MDS pod's address
once, and its connection tracking table keeps that for the flow. The Linux NFS client
reconnects from the source port it used before, so its reconnect can match the old entry
and go to the deleted pod while the replacement is Ready and serving others. Read from
`nfs.conntrack`'s samples against `chaos.restart`'s record of the old and new pod address.
It lives apart from `pnfs` because it judges the node's packet translation, not the NFS
client's counters or the block device.
"""

from __future__ import annotations

from collections.abc import Iterable
from datetime import datetime, timedelta

from ..core import (
    ConntrackSample,
    Detector,
    Evidence,
    Finding,
    Restart,
    SkipDetector,
    critical,
    detector,
    info,
)

#: Entry states that no longer carry traffic. They linger for a while after a close and say
#: nothing about where the next packet goes.
_CLOSED = frozenset({"TIME_WAIT", "CLOSE", "NONE"})


@detector
class ConntrackPinned(Detector):
    name = "pnfs.conntrack-pinned"
    summary = "an NFS flow still translated to the replaced MDS pod after its replacement was Ready"

    def defaults(self) -> dict:
        # Time after Ready for a client to finish a reconnect that was already under way.
        return {"grace_s": 15.0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        samples = ev.conntrack()
        if not samples:
            raise SkipDetector("no conntrack samples; enable nfs.conntrack")
        mds = [r for r in ev.restarts() if r.target == "mds"]
        judged = [r for r in mds if r.ip and r.replacement_ip and r.ready
                  and r.ip != r.replacement_ip]
        if not judged:
            raise SkipDetector("no MDS restart recorded both the old and the new pod address")
        grace = timedelta(seconds=float(self.opt("grace_s")))
        found = False
        for r in judged:
            assert r.ready is not None
            start = r.ready + grace
            later = [x.deleted for x in mds if x.deleted > r.deleted]
            end = min(later) if later else None
            window = [s for s in samples if s.ts >= start and (end is None or s.ts < end)]
            if not window:
                continue
            found = True
            pinned = _pinned(window, r.ip)
            if not pinned:
                yield info(
                    self.name,
                    title=f"conntrack pinning ruled out for the MDS restart at "
                          f"{r.deleted:%H:%M:%S}",
                    subject=f"mds/{r.pod}",
                    detail=f"{len(window)} sample row(s) on "
                           f"{len({s.node for s in window})} node(s) after "
                           f"{start:%H:%M:%S}, none translated to the old pod {r.ip}",
                    evidence={"restart": r.pod, "old_ip": r.ip, "new_ip": r.replacement_ip,
                              "rows": len(window)})
                continue
            for (node, flow), seen in sorted(pinned.items()):
                yield _finding(self.name, r, node, flow, seen)
        if not found:
            raise SkipDetector("the conntrack samples end before any MDS replacement was Ready")


def _pinned(window: list[ConntrackSample], old_ip: str) -> dict[tuple[str, str],
                                                                list[ConntrackSample]]:
    out: dict[tuple[str, str], list[ConntrackSample]] = {}
    for s in window:
        if s.state in _CLOSED or s.reply_src != old_ip:
            continue
        # Addressed to the pod itself, so nothing translated it: the record the old pod's
        # node keeps of a connection that arrived there. It outlives the pod and routes
        # nothing, while a pinned flow is one sent to the Service and translated to the pod.
        if s.orig_dst == old_ip:
            continue
        flow = f"{s.orig_src}:{s.orig_sport} -> {s.orig_dst}"
        out.setdefault((s.node, flow), []).append(s)
    return out


def _finding(name: str, r: Restart, node: str, flow: str,
             seen: list[ConntrackSample]) -> Finding:
    ready: datetime = r.ready or r.deleted
    first, last = seen[0].ts, seen[-1].ts
    return critical(
        name,
        title=f"NFS flow on {node} still went to the replaced MDS pod "
              f"{(last - ready).total_seconds():.0f}s after its replacement was Ready",
        subject=f"{node}/{flow}",
        detail=f"{flow} translated to {r.ip} (the pod deleted at {r.deleted:%H:%M:%S}) from "
               f"{first:%H:%M:%S} to {last:%H:%M:%S}; the replacement {r.replacement_ip} was "
               f"Ready at {ready:%H:%M:%S}. States: "
               f"{', '.join(sorted({s.state for s in seen}))}",
        evidence={"node": node, "flow": flow, "old_ip": r.ip, "new_ip": r.replacement_ip,
                  "first_seen": first.isoformat(), "last_seen": last.isoformat(),
                  "pinned_after_ready_s": round((last - ready).total_seconds())},
        note="The NFS client reconnects from its previous source port, so the reconnect "
             "matched the connection tracking entry the old connection left, and the node "
             "kept sending it to the deleted pod's address. Fresh mounts use a new port and "
             "reach the replacement. Deleting the export's entries on each node when the "
             "MDS address changes would end it.")
