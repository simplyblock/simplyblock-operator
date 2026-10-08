"""Host NVMe observation: ANA and I/O sampling, and an end-of-run fabric snapshot.

Both read the host's sysfs through a pod that already has it — the CSI node plugin — rather
than starting anything, because they need to run on every consuming node and the plugin is
already there on all of them.
"""

from __future__ import annotations

import threading
from datetime import datetime
from typing import Any

from ..core import (
    AnaSample,
    BlockSample,
    Component,
    NvmeController,
    RunContext,
    component,
    now_utc,
)
from . import kube

#: Read every lvol controller's state, its address and its per-namespace ANA states.
#: One line per controller: name|state|address|ctrl_loss_tmo|nsid=ana,nsid=ana,...
_SNAPSHOT_SH = r'''
for c in /sys/class/nvme/nvme*; do
  [ -f "$c/subsysnqn" ] || continue
  nqn=$(cat "$c/subsysnqn" 2>/dev/null)
  case "$nqn" in *lvol:*) ;; *) continue;; esac
  ns=""
  for n in "$c"/nvme*n*; do
    [ -d "$n" ] || continue
    id=$(cat "$n/nsid" 2>/dev/null) || continue
    a=$(cat "$n/ana_state" 2>/dev/null)
    ns="$ns$id=$a,"
  done
  printf '%s|%s|%s|%s|%s|%s\n' \
    "$(basename "$c")" "$(cat "$c/state" 2>/dev/null)" \
    "$(cat "$c/address" 2>/dev/null | tr -d '\n')" \
    "$(cat "$c/ctrl_loss_tmo" 2>/dev/null)" "$nqn" "$ns"
done
'''


def _parse_snapshot(node: str, out: str) -> list[NvmeController]:
    ctrls = []
    for line in out.splitlines():
        parts = line.split("|")
        if len(parts) < 6:
            continue
        name, state, address, clt, nqn, ns = parts[:6]
        traddr = trsvcid = ""
        for kv in address.split(","):
            if kv.startswith("traddr="):
                traddr = kv[7:]
            elif kv.startswith("trsvcid="):
                trsvcid = kv[8:]
        namespaces = {}
        for item in ns.split(","):
            if "=" in item:
                k, _, v = item.partition("=")
                try:
                    namespaces[int(k)] = v
                except ValueError:
                    continue
        try:
            loss = int(clt) if clt.strip() else None
        except ValueError:
            loss = None
        ctrls.append(NvmeController(
            node=node, name=name, nqn=nqn, address=f"{traddr}:{trsvcid}",
            state=state, namespaces=namespaces, ctrl_loss_tmo=loss))
    return ctrls


class _CsiNodeBase(Component):
    """Shared lookup of the CSI node plugin, the window onto each host's sysfs.

    A Component for the same reason as _GrabberBase: it uses `opt` and `name`.
    """

    def _csi_node_pods(self, ctx: RunContext) -> dict[str, str]:
        """node -> CSI node-plugin pod, the window onto each host's sysfs."""
        out = {}
        for p in kube.list_pods(self.opt("csi_namespace"), [self.opt("csi_pod_prefix")]):
            if p.node:
                out[p.node] = p.name
        if not out:
            ctx.log.warn(f"{self.name}: no CSI node pods matching "
                         f"{self.opt('csi_pod_prefix')!r} in {self.opt('csi_namespace')}; "
                         "host NVMe state cannot be read")
        return out


@component
class FabricSnapshot(_CsiNodeBase):
    """Record every lvol NVMe controller on every node, at setup and at the end.

    Two snapshots on purpose. The one at setup answers "did the last run leave a mess?" —
    which is a real question, because a leaked controller breaks the *next* run rather than
    the one that created it. The one at the end answers "did this run leave a mess?".
    """

    name = "nvme.snapshot"
    summary = "snapshot host NVMe controllers before and after the run (leak detection)"

    def defaults(self) -> dict[str, Any]:
        return {"csi_namespace": "simplyblock", "csi_pod_prefix": "simplyblock-csi-node",
                "container": "csi-node", "at_setup": True, "at_collect": True}

    def _snapshot(self, ctx: RunContext, label: str) -> list[NvmeController]:
        ctrls: list[NvmeController] = []
        for node, pod in sorted(self._csi_node_pods(ctx).items()):
            out = kube.exec_sh(self.opt("csi_namespace"), pod, _SNAPSHOT_SH,
                               container=self.opt("container"), timeout=120)
            ctrls.extend(_parse_snapshot(kube.short(node), out))
        ctx.save_json(f"nvme-controllers-{label}.json",
                      [{"node": c.node, "name": c.name, "nqn": c.nqn, "address": c.address,
                        "state": c.state, "namespaces": {str(k): v for k, v in c.namespaces.items()},
                        "ctrl_loss_tmo": c.ctrl_loss_tmo} for c in ctrls])
        stuck = [c for c in ctrls if c.state == "connecting"]
        empty = [c for c in ctrls if c.state == "live" and c.serves_nothing]
        ctx.log.info(f"{self.name} ({label}): {len(ctrls)} lvol controller(s), "
                     f"{len(stuck)} connecting, {len(empty)} live-with-no-namespace")
        return ctrls

    def setup(self, ctx: RunContext) -> None:
        if not self.opt("at_setup"):
            return
        pre = self._snapshot(ctx, "pre")
        ctx.shared["nvme.controllers.pre"] = pre

    def collect(self, ctx: RunContext) -> None:
        if not self.opt("at_collect"):
            return
        post = self._snapshot(ctx, "post")
        ctx.shared["nvme.controllers"] = post
        # The detectors read the canonical name; keep it pointing at the post-run state.
        ctx.save_json("nvme-controllers.json",
                      [{"node": c.node, "name": c.name, "nqn": c.nqn, "address": c.address,
                        "state": c.state, "namespaces": {str(k): v for k, v in c.namespaces.items()},
                        "ctrl_loss_tmo": c.ctrl_loss_tmo} for c in post])


@component
class AnaSampler(_CsiNodeBase):
    """Sample per-namespace ANA state on every consuming node, on an interval.

    The evidence behind every ANA detector, and the reason the sampling interval is a
    first-class option: the freeze *count* is robust to it, but a freeze shorter than one
    interval can be missed entirely, so a suite hunting short pauses should lower it.

    Samples are written per migration into ana/<migration>.csv in the same layout the
    archive reader expects, so a run's ANA evidence is replayable.
    """

    name = "ana.sample"
    summary = "sample host ANA state per namespace on an interval, per migration"

    def defaults(self) -> dict[str, Any]:
        return {"csi_namespace": "simplyblock", "csi_pod_prefix": "simplyblock-csi-node",
                "container": "csi-node", "interval_s": 2.0, "nodes": None}

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._current: str | None = None
        self._phase = ""
        self._samples: dict[str, list[AnaSample]] = {}
        self._pods: dict[str, str] = {}

    def setup(self, ctx: RunContext) -> None:
        self._pods = self._csi_node_pods(ctx)
        ctx.shared["ana.samples"] = self._samples
        # Published so a driver can attribute samples to the migration in flight. Samples
        # that belong to no migration are worth nothing — freeze_windows is per migration —
        # so without a driver calling begin/end this component collects nothing, and the ANA
        # detectors correctly report themselves skipped rather than clean.
        ctx.shared["ana.sampler"] = self

    # -- the scenario drives these two ----------------------------------------------
    def begin(self, migration: str) -> None:
        """Start attributing samples to `migration`. Called by whatever drives migrations."""
        self._current = migration
        self._phase = ""
        self._samples.setdefault(migration, [])

    def set_phase(self, phase: str) -> None:
        """Stamp subsequent samples with the migration's current phase.

        This is what turns a wall-clock ANA transition into a readable one: "all paths went
        inaccessible during Cutover" is a finding, "all paths went inaccessible at 09:31:02"
        is a timestamp. Optional — the driver calls it if it has a phase to report, and a
        sampler that is never told stamps the empty string.
        """
        self._phase = phase

    def end(self, ctx: RunContext, migration: str) -> str | None:
        """Stop attributing and write the CSV for `migration`."""
        self._current = None
        self._phase = ""
        samples = self._samples.get(migration) or []
        if not samples:
            return None
        path = ctx.path(f"ana/{migration}.csv")
        with open(path, "w") as fh:
            fh.write("ts,node,phase,address,role,ctrl_state,nsid,ana_state\n")
            for s in sorted(samples, key=lambda x: (x.ts, x.node, x.address)):
                stamp = s.ts.strftime("%Y-%m-%dT%H:%M:%SZ")
                if not s.ana:
                    # A controller with no namespace at all is itself the finding, so it
                    # gets a row rather than being skipped.
                    fh.write(f"{stamp},{s.node},{s.phase},{s.address},{s.role},{s.state},,\n")
                for nsid, ana in sorted(s.ana.items()):
                    fh.write(f"{stamp},{s.node},{s.phase},{s.address},{s.role},"
                             f"{s.state},{nsid},{ana}\n")
        return path

    def start(self, ctx: RunContext) -> None:
        if not self._pods:
            return
        interval = float(self.opt("interval_s"))
        if interval <= 0:
            ctx.log.info(f"{self.name}: disabled (interval_s <= 0)")
            return

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                mig = self._current
                if mig:
                    for node, pod in self._pods.items():
                        try:
                            out = kube.exec_sh(self.opt("csi_namespace"), pod, _SNAPSHOT_SH,
                                               container=self.opt("container"), timeout=30)
                        except Exception:  # noqa: BLE001
                            continue
                        ts = now_utc()
                        for c in _parse_snapshot(kube.short(node), out):
                            self._samples.setdefault(mig, []).append(AnaSample(
                                ts=ts, node=c.node, address=c.address, state=c.state,
                                ana=dict(c.namespaces), phase=self._phase))
                self._stop.wait(interval)

        self._thread = threading.Thread(target=loop, name="ana-sampler", daemon=True)
        self._thread.start()
        ctx.log.info(f"{self.name}: sampling every {interval}s on "
                     f"{len(self._pods)} node(s)")

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=15)
            self._thread = None


#: Every NVMe head device's namespace UUID and its sysfs `stat` line, one per device:
#: dev|uuid|stat. The per-path nvmeXcYnZ devices are listed too and dropped by the parser,
#: which is where the rule lives that is worth a test.
_IOSTAT_SH = r'''
for b in /sys/block/nvme*n*; do
  [ -f "$b/stat" ] || continue
  printf '%s|%s|%s\n' "$(basename "$b")" "$(cat "$b/uuid" 2>/dev/null)" "$(cat "$b/stat")"
done
'''


def parse_iostat(node: str, ts: datetime, out: str) -> list[BlockSample]:
    """Readings from one node's _IOSTAT_SH output.

    Head devices only. nvmeXcYnZ is one path of a multipath namespace and nvmeXnY carries
    the namespace's total, so counting both would double every byte. A device without a
    namespace UUID is not a simplyblock volume and is dropped too.
    """
    samples = []
    for line in out.splitlines():
        parts = line.split("|")
        if len(parts) < 3:
            continue
        dev, uuid, stat = parts[0].strip(), parts[1].strip(), parts[2].split()
        if not uuid or "c" in dev[len("nvme"):] or len(stat) < 7:
            continue
        try:
            # sysfs block stat: reads completed, reads merged, sectors read, ms reading,
            # writes completed, writes merged, sectors written, ...
            samples.append(BlockSample(
                ts=ts, node=node, device=dev, uuid=uuid,
                read_ios=int(stat[0]), read_sectors=int(stat[2]),
                write_ios=int(stat[4]), write_sectors=int(stat[6])))
        except ValueError:
            continue
    return samples


def write_iostat(path: str, samples: list[BlockSample]) -> None:
    with open(path, "w") as fh:
        fh.write("ts,node,device,uuid,read_ios,read_sectors,write_ios,write_sectors\n")
        for b in sorted(samples, key=lambda x: (x.ts, x.node, x.device)):
            fh.write(f"{b.ts.strftime('%Y-%m-%dT%H:%M:%SZ')},{b.node},{b.device},{b.uuid},"
                     f"{b.read_ios},{b.read_sectors},{b.write_ios},{b.write_sectors}\n")


@component
class IostatSampler(_CsiNodeBase):
    """Sample every node's NVMe namespace I/O counters on an interval.

    What this shows that nothing above the block layer can: whether a node's I/O actually
    reached its NVMe-oF namespace. For pNFS that is the property under test. A client with a
    layout writes to its own namespace, and one without sends the same data through the
    metadata server; the files read back correctly either way. Sampled rather than read once
    at the end, so a stall in the middle of a run shows as one.

    Every head device with a namespace UUID on every node is sampled, whatever volume it
    belongs to. Which ones matter is the detectors' business, and a volume the run did not
    expect to be attached somewhere is evidence too.
    """

    name = "nvme.iostat"
    summary = "sample NVMe namespace I/O counters per node on an interval"

    def defaults(self) -> dict[str, Any]:
        return {"csi_namespace": "simplyblock", "csi_pod_prefix": "simplyblock-csi-node",
                "container": "csi-node", "interval_s": 5.0}

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._samples: list[BlockSample] = []
        self._lock = threading.Lock()
        self._pods: dict[str, str] = {}

    def setup(self, ctx: RunContext) -> None:
        self._pods = self._csi_node_pods(ctx)

    def _sample(self) -> None:
        for node, pod in self._pods.items():
            try:
                out = kube.exec_sh(self.opt("csi_namespace"), pod, _IOSTAT_SH,
                                   container=self.opt("container"), timeout=30)
            except Exception:  # noqa: BLE001
                continue
            batch = parse_iostat(kube.short(node), now_utc(), out)
            with self._lock:
                self._samples.extend(batch)

    def start(self, ctx: RunContext) -> None:
        if not self._pods:
            return
        interval = float(self.opt("interval_s"))
        if interval <= 0:
            ctx.log.info(f"{self.name}: disabled (interval_s <= 0)")
            return

        def loop() -> None:
            while not self._stop.is_set() and not ctx.stopping.is_set():
                self._sample()
                self._stop.wait(interval)

        self._thread = threading.Thread(target=loop, name="iostat-sampler", daemon=True)
        self._thread.start()
        ctx.log.info(f"{self.name}: sampling every {interval}s on {len(self._pods)} node(s)")

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=15)
            self._thread = None
        # One last reading after the workload stopped, so the final interval is measured.
        if self._pods:
            self._sample()

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            samples = list(self._samples)
        if samples:
            write_iostat(ctx.path("iostat.csv"), samples)
        ctx.log.info(f"{self.name}: {len(samples)} reading(s) from {len(self._pods)} node(s)")
