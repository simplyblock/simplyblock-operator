"""The pNFS workload: shared and private pNFS volumes, a fio instance in every container.

pNFS exists for ReadWriteMany: several pods on several nodes reading and writing one
filesystem while their data goes straight to the shared NVMe-oF namespace. So this component
provisions some volumes that several pods share and some that one pod owns, and runs fio in
every container of every pod, so a shared volume has several writers and readers at once.

**Every fio instance owns its data file.** fio's md5 verification assumes nothing else
writes the blocks it checks, so two instances on one file report each other's writes as
corruption. Each instance therefore writes and verifies a file named after its run, pod and
container, and the instances on a shared volume meet only in the filesystem: concurrent
allocation, layouts, and commits through one metadata server, which is what multi-writer
pNFS has to get right.

**Pods sharing a volume are spread across nodes.** Two pods on one node share that node's
NFS client and its single connection to the namespace, so the multi-writer case worth
running is the multi-node one. The spread is preferred rather than required, so a cluster
with fewer nodes than sharing pods still runs, and the volume map says where each pod landed.

**Cross-client reads are opt-in** (`cross_readers`). Each instance above verifies what its
own client wrote, which a client cache can satisfy without the data ever crossing to another
node. With cross_readers set, every shared volume also gets a writer pod that writes a new
file per round and publishes a marker after closing it, and that many reader pods, never on
the writer's node, that verify each published round with fio. `fio.cross-read` judges them.

What it leaves for the detectors: the per-instance fio evidence every fio workload leaves, the
NFS mount's own per-operation counts (`pnfs.layout` reads them), and `pnfs.json`, the volume
to consuming-node map `pnfs.device-io` checks the client-side NVMe counters against.
"""

from __future__ import annotations

import json
from typing import Any

from ...core import RunContext, component
from .. import kube, nfs
from . import fio
from .base import FioWorkload

PARAM_FSTYPE = "csi.storage.k8s.io/fstype"
FSTYPE_PNFS = "pnfs"
MOUNT = "/data"
# Filesystem metadata and fio's own layout need room beside the data files.
HEADROOM_GB = 2


@component
class PnfsRwxWorkload(FioWorkload):
    """Shared and private pNFS volumes, a verified fio instance in every container."""

    name = "workload.pnfs"
    summary = "pNFS volumes shared by several pods, a verified fio instance per container"

    def workload_defaults(self) -> dict[str, Any]:
        return {
            # A pNFS StorageClass to use as it is. Empty clones `source_storageclass` with
            # fstype pnfs instead, which inherits the pool's real parameters.
            "storageclass": "",
            "source_storageclass": "",
            "shared_volumes": 1,      # RWX volumes several pods mount
            "pods_per_shared": 3,
            "solo_pods": 1,           # pods with a volume of their own
            "containers_per_pod": 2,  # fio instances per pod, each with its own file
            "spread": True,           # prefer different nodes for pods sharing a volume
            "volume_size_gb": 20,
            "file_size_gb": 1,        # per fio instance
            "direct": True,
            "ready_timeout_s": 600,
            # Reader pods per shared volume that verify, from other nodes, the rounds a
            # writer pod of that volume publishes. 0 adds neither.
            "cross_readers": 0,
            # Each round's file. Up to ROUND_KEEP + 1 + cross_readers of them on the volume.
            "round_size_mb": 256,
            "round_s": 30,          # a writer starts a round at most this often
            # How long a reader waits for the next round before logging a timeout. Below
            # stop_timeout_s, so a reader has exited before stop() gives up on it.
            "round_wait_s": 120,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._claim_of: dict[str, str] = {}     # pod -> claim
        self._shared: set[str] = set()          # claims several pods mount
        self._lvol_of: dict[str, str] = {}      # claim -> lvol UUID
        self._cluster_of: dict[str, str] = {}   # claim -> cluster UUID
        self._round_names: set[str] = set()     # the cross-read writers and readers

    # ── the layout ──────────────────────────────────────────────────────────────────

    def documents(self, ctx: RunContext) -> list[dict]:
        sc = self._storageclass(ctx)
        # For workload.pnfs-churn, whose own volumes come from the same class.
        ctx.shared["pnfs.storageclass"] = sc
        docs = self._documents(ctx, sc)
        ctx.log.info(f"{self.name}: {len(self._claim_of)} pod(s) on "
                     f"{len(set(self._claim_of.values()))} pNFS volume(s), "
                     f"{len(self._instances)} fio instance(s)")
        return docs

    def after_running(self, ctx: RunContext) -> None:
        self._cluster_of, self._lvol_of = self._resolve_lvols()
        nodes = self._nodes()
        # Which nodes mount a pNFS volume, for chaos.restart to pick a node plugin from.
        ctx.shared["pnfs.client_nodes"] = sorted(set(nodes.values()))
        # The long-lived claims, which workload.pnfs-churn's pods join and leave.
        ctx.shared["pnfs.claims"] = sorted(set(self._claim_of.values()))
        # Only the claims several pods mount, for chaos.volume-ops, which exercises those.
        ctx.shared["pnfs.shared_claims"] = sorted(self._shared)
        # Which fio instance runs in which pod and container, for nfs.mountstats.
        ctx.shared["pnfs.instances"] = [
            {"evidence": i.evidence, "pod": i.pod, "container": i.container}
            for i in self._instances]
        self._write_volume_map(ctx, nodes)
        self._report_spread(ctx, nodes)

    def _storageclass(self, ctx: RunContext) -> str:
        """The pNFS class the claims use: the named one, checked, or a clone of the source.

        A named class that is not pNFS is refused rather than used. A block class hands
        every pod its own device, the shared claims never bind ReadWriteMany, and a run
        with only private volumes would pass without pNFS ever being involved.
        """
        named = str(self.opt("storageclass") or "")
        if named:
            sc = self.get_storageclass(named)
            if sc is None:
                raise RuntimeError(f"{self.name}: StorageClass {named} not found")
            fstype = sc.get("parameters", {}).get(PARAM_FSTYPE, "")
            if fstype != FSTYPE_PNFS:
                raise RuntimeError(
                    f"{self.name}: StorageClass {named} has fstype {fstype or 'unset'!r}, "
                    f"not {FSTYPE_PNFS!r}; name a pNFS class, or set source_storageclass "
                    "to have one cloned")
            return named

        source = str(self.opt("source_storageclass") or "")
        if not source:
            raise RuntimeError(f"{self.name}: set storageclass (a pNFS class) or "
                               "source_storageclass (a pool class to clone as pNFS)")
        src = self.get_storageclass(source)
        if src is None:
            raise RuntimeError(f"{self.name}: source StorageClass {source} not found")
        name = f"sbtest-{ctx.run_id}-pnfs"
        # Immediate: a shared claim is mounted by pods on several nodes, so there is no
        # single first consumer whose topology the volume should follow.
        self.apply_storageclass(src, name,
                                dict(src.get("parameters", {}), **{PARAM_FSTYPE: FSTYPE_PNFS}),
                                binding_mode="Immediate")
        ctx.log.info(f"{self.name}: StorageClass {name}, cloned from {source} as pNFS")
        return name

    def _documents(self, ctx: RunContext, sc: str) -> list[dict]:
        """The claims and pods, with every fio instance and its own file planned."""
        shared, per_shared = int(self.opt("shared_volumes")), int(self.opt("pods_per_shared"))
        solo, containers = int(self.opt("solo_pods")), int(self.opt("containers_per_pod"))
        if shared * per_shared + solo == 0 or containers < 1:
            raise RuntimeError(f"{self.name}: no pods or no containers, so the run would have "
                               "no I/O and could not detect anything")

        readers = int(self.opt("cross_readers")) if shared else 0
        if readers and int(self.opt("round_wait_s")) >= int(self.opt("stop_timeout_s")):
            raise RuntimeError(
                f"{self.name}: round_wait_s {self.opt('round_wait_s')} must be below "
                f"stop_timeout_s {self.opt('stop_timeout_s')}, or a reader still waiting "
                "for a round when stop() gives up never writes its exit code")

        file_gb, volume_gb = int(self.opt("file_size_gb")), int(self.opt("volume_size_gb"))
        busiest = max(per_shared if shared else 0, 1 if solo else 0) * containers
        if busiest * file_gb + HEADROOM_GB > volume_gb:
            raise RuntimeError(
                f"{self.name}: a volume carries {busiest} fio file(s) of {file_gb}G, which "
                f"with {HEADROOM_GB}G of headroom does not fit {volume_gb}G; raise "
                "volume_size_gb or lower file_size_gb")
        # The writer removes a round only after publishing the one ROUND_KEEP after it, so
        # the round being written and the ROUND_KEEP before it exist at once. A reader still
        # reading a removed round keeps it open, and the server frees it only at the close.
        round_files = fio.ROUND_KEEP + 1 + readers
        rounds_gb = round_files * int(self.opt("round_size_mb")) / 1024 if readers else 0
        if readers and per_shared * containers * file_gb + rounds_gb + HEADROOM_GB > volume_gb:
            raise RuntimeError(
                f"{self.name}: a shared volume carries {per_shared * containers} fio file(s) "
                f"of {file_gb}G and up to {round_files} round file(s) of "
                f"{self.opt('round_size_mb')}M, which with {HEADROOM_GB}G of headroom does "
                f"not fit {volume_gb}G; raise volume_size_gb or lower round_size_mb")

        plan: list[tuple[str, bool]] = []    # (claim, shared) per pod
        for v in range(shared):
            plan += [(f"{ctx.run_id}-pnfs-shared-{v}", True)] * per_shared
        plan += [(f"{ctx.run_id}-pnfs-solo-{i}", False) for i in range(solo)]

        docs: list[dict] = []
        shared_claims = {c for c, is_shared in plan if is_shared}
        for claim in dict.fromkeys(c for c, _ in plan):
            docs.append(self._claim(ctx, claim, sc, claim in shared_claims))
        for i, (claim, is_shared) in enumerate(plan):
            pod = f"{ctx.run_id}-pnfs-{i}"
            self._pods.append(pod)
            self._claim_of[pod] = claim
            if is_shared:
                self._shared.add(claim)
            docs.append(self._pod(ctx, pod, i, claim, is_shared, containers, file_gb))
        for v in range(shared if readers else 0):
            docs += self._round_pods(ctx, v, f"{ctx.run_id}-pnfs-shared-{v}", readers)
        return docs

    def _claim(self, ctx: RunContext, claim: str, sc: str, shared: bool) -> dict:
        return {
            "apiVersion": "v1", "kind": "PersistentVolumeClaim",
            "metadata": {"name": claim, "labels": {
                "sbtest": ctx.run_id, "sbtest-run": "true",
                "volume-kind": "pnfs-shared" if shared else "pnfs-solo"}},
            "spec": {"accessModes": ["ReadWriteMany"], "storageClassName": sc,
                     "resources": {"requests": {
                         "storage": f"{self.opt('volume_size_gb')}Gi"}}},
        }

    def _pod(self, ctx: RunContext, pod: str, index: int, claim: str, shared: bool,
             containers: int, file_gb: int) -> dict:
        labels = {"sbtest": ctx.run_id, "sbtest-run": "true", "app": "fio"}
        if shared:
            labels["pnfs-shared"] = claim
        spec_containers = []
        for k in range(containers):
            inst = fio.FioInstance(
                pod=pod, container=f"fio-{k}",
                # Run, pod and container in the name, so no two instances share a file even
                # across pods mounting one volume, nor with what an earlier run left there.
                filename=f"{MOUNT}/{ctx.run_id}-{index}-c{k}.fio",
                logdir=f"/logs/c{k}",
                evidence=f"{ctx.run_id}-fio-{index}-c{k}")
            self._instances.append(inst)
            args = fio.fio_args(self.options, filename=inst.filename, size_gb=file_gb,
                                logdir=inst.logdir, direct=bool(self.opt("direct")))
            spec_containers.append(
                self.fio_container(inst, fio.container_script(args, inst.logdir), MOUNT))
        spec: dict[str, Any] = {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 5,
            "containers": spec_containers,
            "volumes": self.pod_volumes(claim),
        }
        if shared and self.opt("spread"):
            spec["affinity"] = {"podAntiAffinity": self._spread(claim)}
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": pod, "labels": labels}, "spec": spec}

    @staticmethod
    def _spread(claim: str) -> dict[str, Any]:
        return {"preferredDuringSchedulingIgnoredDuringExecution": [{
            "weight": 100,
            "podAffinityTerm": {
                "labelSelector": {"matchLabels": {"pnfs-shared": claim}},
                "topologyKey": "kubernetes.io/hostname"}}]}

    def _round_pods(self, ctx: RunContext, v: int, claim: str, readers: int) -> list[dict]:
        """One shared volume's round writer and its readers, each a pod of one container.

        The readers' anti-affinity to the writer is required, not preferred: a reader on the
        writer's node reads through the writer's own NFS client and proves nothing about
        another one. A cluster with one schedulable node therefore leaves the readers
        Pending, and setup says so.
        """
        rounds = fio.Rounds(base=f"{MOUNT}/{ctx.run_id}-xw-{v}",
                            size_mb=int(self.opt("round_size_mb")),
                            round_s=int(self.opt("round_s")),
                            wait_s=int(self.opt("round_wait_s")),
                            runtime_s=int(self.opt("runtime_s")),
                            # As long as setup() can take to reach the timed run, and a
                            # minute for the exec that releases the pod.
                            start_wait_s=int(self.opt("ready_timeout_s"))
                            + int(self.opt("io_timeout_s")) + 60)
        direct = bool(self.opt("direct"))
        plan = [(f"{ctx.run_id}-pnfs-xw-{v}", "xwrite", "/logs/xw",
                 f"{ctx.run_id}-fio-xw-{v}", "pnfs-xwriter",
                 fio.round_writer_script(self.options, rounds, "/logs/xw", direct))]
        plan += [(f"{ctx.run_id}-pnfs-xr-{v}-{r}", "xread", "/logs/xr",
                  f"{ctx.run_id}-fio-xr-{v}-{r}", "pnfs-xreader",
                  fio.round_reader_script(self.options, rounds, "/logs/xr", direct))
                 for r in range(readers)]
        docs = []
        for pod, container, logdir, evidence, role, script in plan:
            inst = fio.FioInstance(pod=pod, container=container, filename=rounds.base,
                                   logdir=logdir, evidence=evidence)
            self._instances.append(inst)
            self._pods.append(pod)
            self._claim_of[pod] = claim
            self._round_names.add(pod)
            anti: dict[str, Any] = self._spread(claim) if self.opt("spread") else {}
            if role == "pnfs-xreader":
                anti["requiredDuringSchedulingIgnoredDuringExecution"] = [{
                    "labelSelector": {"matchLabels": {"pnfs-xwriter": claim}},
                    "topologyKey": "kubernetes.io/hostname"}]
            spec: dict[str, Any] = {
                "restartPolicy": "Never",
                "terminationGracePeriodSeconds": 5,
                "containers": [self.fio_container(inst, script, MOUNT)],
                "volumes": self.pod_volumes(claim),
            }
            if anti:
                spec["affinity"] = {"podAntiAffinity": anti}
            docs.append({"apiVersion": "v1", "kind": "Pod", "metadata": {
                "name": pod, "labels": {"sbtest": ctx.run_id, "sbtest-run": "true",
                                        "app": "fio", "pnfs-shared": claim, role: claim}},
                         "spec": spec})
        return docs

    def timed_instances(self) -> list[fio.FioInstance]:
        # A round writer writes sequentially and a reader runs fio only once a round is
        # published, so neither prints the randrw timed-run status the wait looks for.
        return [i for i in self._instances if i.pod not in self._round_names]

    # ── what the cluster decided ────────────────────────────────────────────────────

    def _resolve_lvols(self) -> tuple[dict[str, str], dict[str, str]]:
        """claim -> cluster UUID and claim -> lvol UUID, from the PV's CSI handle
        <cluster>:<pool>:<volume>."""
        clusters: dict[str, str] = {}
        out: dict[str, str] = {}
        ns = self.opt("namespace")
        for claim in sorted(set(self._claim_of.values())):
            cp = kube.run(["-n", ns, "get", "pvc", claim, "-o", "json"], check=False)
            pv = json.loads(cp.stdout or "{}").get("spec", {}).get("volumeName", "")
            if not pv:
                continue
            cp = kube.run(["get", "pv", pv, "-o", "json"], check=False)
            handle = json.loads(cp.stdout or "{}").get("spec", {}).get("csi", {}).get(
                "volumeHandle", "")
            parts = handle.split(":")
            if len(parts) == 3 and parts[2]:
                clusters[claim], out[claim] = parts[0], parts[2]
        return clusters, out

    def _nodes(self) -> dict[str, str]:
        """pod -> node, as the scheduler placed them."""
        cp = kube.run(["-n", self.opt("namespace"), "get", "pods", "-l",
                       "app=fio", "-o", "json"], check=False)
        out: dict[str, str] = {}
        for it in json.loads(cp.stdout or "{}").get("items", []):
            name = it["metadata"]["name"]
            if name in self._claim_of:
                out[name] = kube.short(it.get("spec", {}).get("nodeName", "") or "")
        return out

    def _write_volume_map(self, ctx: RunContext, nodes: dict[str, str]) -> None:
        volumes = []
        for claim in sorted(set(self._claim_of.values())):
            # The round pods stay out of pods, nodes, and instances, which pnfs.device-io
            # reads: a reader-only node writes nothing to the namespace, and a writer idles
            # between rounds, so either reads as a client that bypassed it.
            pods = sorted(p for p, c in self._claim_of.items()
                          if c == claim and p not in self._round_names)
            rounds = [i for i in self._instances
                      if i.pod in self._round_names and self._claim_of[i.pod] == claim]
            volumes.append({
                "claim": claim, "lvol": self._lvol_of.get(claim, ""),
                # The run's cluster, which nothing else in a run without migrations records.
                "cluster": self._cluster_of.get(claim, ""),
                "shared": claim in self._shared,
                "pods": pods,
                "nodes": sorted({nodes[p] for p in pods if nodes.get(p)}),
                # Which instance ran where, so a node is judged while its own fio ran.
                "instances": {i.evidence: nodes[i.pod] for i in self._instances
                              if i.pod in pods and nodes.get(i.pod)},
                **({"cross_read": {i.evidence: nodes.get(i.pod, "") for i in rounds}}
                   if rounds else {}),
            })
        ctx.save_json("pnfs.json", {"volumes": volumes})

    def _report_spread(self, ctx: RunContext, nodes: dict[str, str]) -> None:
        for claim in sorted(self._shared):
            pods = [p for p, c in self._claim_of.items() if c == claim]
            on = sorted({nodes.get(p, "?") for p in pods})
            ctx.log.info(f"{self.name}: {claim} (lvol {self._lvol_of.get(claim, '?')}): "
                         f"{len(pods)} pod(s) on {', '.join(on)}")
            if len(on) < 2 <= len(pods):
                ctx.log.warn(f"{self.name}: every pod sharing {claim} landed on {on[0]}, so "
                             "this volume exercises one NFS client, not several")

    # ── collection: the base collects every instance, this adds the mount ─────────

    def after_collect(self, ctx: RunContext) -> None:
        # One NFS mount per pod: every instance in the pod wrote through it, so every
        # instance's evidence carries the mount's counts.
        ns = self.opt("namespace")
        for pod in self._pods:
            mine = [i for i in self._instances if i.pod == pod]
            stats = kube.exec_sh(ns, pod, "cat /proc/self/mountstats",
                                 container=mine[0].container, timeout=60)
            ops = nfs.mount_ops(stats, MOUNT)
            if ops is None:
                ctx.log.warn(f"{self.name}: {pod}: no NFS mount on {MOUNT} in mountstats")
                continue
            for inst in mine:
                with open(ctx.path(inst.evidence, "nfs-ops.json"), "w") as fh:
                    json.dump(ops, fh, indent=2, sort_keys=True)
