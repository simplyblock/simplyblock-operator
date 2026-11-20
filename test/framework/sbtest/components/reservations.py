"""Snapshotting every namespace's NVMe reservation, before the run and after it.

A pNFS metadata server holds a reservation on each export's namespace, and every client
registers the key nfsd hands it, whose upper 32 bits are nfsd's boot time. When the metadata
server restarts, the clients' old registrations stay on the namespace and block the keys the
new boot hands out, and the clients' I/O then goes through the metadata server with nothing
logged anywhere. The only witness is the target's own registrant list, which
`nvme resv-report` reads, so `nvme.reservations` records it on every node at setup and at
collect (`reservations-pre.json`, `reservations-post.json`) for `nvme.stale-reservations`.

It reads through the CSI node plugin, as the other NVMe components do, because that pod has
the host's /dev and nvme-cli.
"""

from __future__ import annotations

import json
import re
from typing import Any

from ..core import NamespaceReservation, Registrant, RunContext, component
from . import kube
from .nvme import _CsiNodeBase

#: Every device with a namespace UUID, each under a `### <device>|<uuid>` marker and
#: followed by its `nvme resv-report` JSON. A device whose report fails prints only its
#: marker, and the parser leaves it out. The parser also drops the per-path nvmeXcYnZ
#: devices, since the reservation is the namespace's and its head reports it once.
_RESV_SH = r'''
for b in /sys/block/nvme*n*
do
  test -f "$b/uuid" || continue
  n=$(basename "$b")
  echo "### $n|$(cat "$b/uuid")"
  nvme resv-report "/dev/$n" -e 1 -o json 2>/dev/null
done
echo "### end"
'''

#: A multipath namespace's per-path device, as opposed to its head.
_PER_PATH = re.compile(r"^nvme\d+c\d+n\d+$")


def parse_report(node: str, out: str) -> list[NamespaceReservation]:
    """The namespaces in one node's _RESV_SH output, in the order printed."""
    out_list: list[NamespaceReservation] = []
    for block in out.split("### ")[1:]:
        header, _, body = block.partition("\n")
        device, sep, uuid = header.strip().partition("|")
        if not sep or not body.strip() or _PER_PATH.match(device):
            continue
        try:
            raw = json.loads(body)
        except json.JSONDecodeError:
            continue
        regs = tuple(
            Registrant(hostid=str(r.get("hostid", "")), rkey=int(r.get("rkey", 0)),
                       holder=bool(r.get("rcsts", 0) & 1))
            for r in raw.get("regctlext") or [] if isinstance(r, dict))
        out_list.append(NamespaceReservation(
            node=node, device=device, uuid=uuid.strip(), rtype=int(raw.get("rtype", 0)),
            generation=int(raw.get("gen", 0)), registrants=regs))
    return out_list


def write_snapshot(ctx: RunContext, label: str, snapshot: list[NamespaceReservation]) -> None:
    """Save a snapshot as `reservations-<label>.json`, in the layout ArchiveEvidence reads."""
    ctx.save_json(f"reservations-{label}.json", {"namespaces": [{
        "node": n.node, "device": n.device, "uuid": n.uuid, "rtype": n.rtype,
        "generation": n.generation,
        "registrants": [{"hostid": r.hostid, "rkey": r.rkey, "holder": r.holder}
                        for r in n.registrants]} for n in snapshot]})


def _pnfs_lvols(ctx: RunContext) -> set[str]:
    """The lvols the run's pNFS workload provisioned, from its volume map, or none when it
    wrote none (yet): at setup the volumes do not exist, and a run may have no pNFS."""
    try:
        with open(ctx.path("pnfs.json")) as fh:
            raw = json.load(fh)
    except (OSError, json.JSONDecodeError):
        return set()
    return {str(v.get("lvol")) for v in raw.get("volumes", [])
            if isinstance(v, dict) and v.get("lvol")}


@component
class ReservationSnapshot(_CsiNodeBase):
    """Record every namespace's NVMe reservation on every node, at setup and at the end."""

    name = "nvme.reservations"
    summary = "snapshot each namespace's NVMe reservation before and after the run"

    def defaults(self) -> dict[str, Any]:
        return {"csi_namespace": None, "csi_pod_prefix": "simplyblock-csi-node",
                "container": "csi-node", "at_setup": True, "at_collect": True}

    def _snapshot(self, ctx: RunContext, label: str) -> None:
        lvols = _pnfs_lvols(ctx)
        found: list[NamespaceReservation] = []
        for node, pod in sorted(self._csi_node_pods(ctx).items()):
            out = kube.exec_sh(self.opt("csi_namespace"), pod, _RESV_SH,
                               container=self.opt("container"), timeout=120)
            found.extend(parse_report(kube.short(node), out))
        if lvols:
            found = [n for n in found if n.uuid in lvols]
        write_snapshot(ctx, label, found)
        held = sum(1 for n in found if any(r.holder for r in n.registrants))
        ctx.log.info(f"{self.name} ({label}): {len(found)} namespace report(s), {held} under "
                     "a reservation" + (" (pNFS volumes only)" if lvols else ""))

    def setup(self, ctx: RunContext) -> None:
        if self.opt("at_setup"):
            self._snapshot(ctx, "pre")

    def collect(self, ctx: RunContext) -> None:
        if self.opt("at_collect"):
            self._snapshot(ctx, "post")
