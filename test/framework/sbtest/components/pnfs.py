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

What it leaves for the detectors: the per-instance fio evidence every fio workload leaves, the
NFS mount's own per-operation counts (`pnfs.layout` reads them), and `pnfs.json`, the volume
to consuming-node map `pnfs.device-io` checks the client-side NVMe counters against.
"""

from __future__ import annotations

import json
from typing import Any

from ..core import Component, RunContext, component
from . import fio, kube, nfs

PARAM_FSTYPE = "csi.storage.k8s.io/fstype"
FSTYPE_PNFS = "pnfs"
MOUNT = "/data"
# Filesystem metadata and fio's own layout need room beside the data files.
HEADROOM_GB = 2


@component
class PnfsWorkload(Component):
    """Provision shared and private pNFS volumes and run verified fio in every container.

    `required`, because a run whose workload never came up has nothing to judge.
    """

    name = "workload.pnfs"
    summary = "pNFS volumes shared by several pods, a verified fio instance per container"
    required = True

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
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
            **fio.FIO_DEFAULTS,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._pods: list[str] = []
        self._instances: list[fio.FioInstance] = []
        self._claim_of: dict[str, str] = {}     # pod -> claim
        self._shared: set[str] = set()          # claims several pods mount
        self._lvol_of: dict[str, str] = {}      # claim -> lvol uuid
        self._created_sc = ""

    # ── setup ───────────────────────────────────────────────────────────────────────

    def setup(self, ctx: RunContext) -> None:
        sc = self._storageclass(ctx)
        docs = self._documents(ctx, sc)
        kube.run(["-n", self.opt("namespace"), "apply", "-f", "-"],
                 stdin="\n---\n".join(json.dumps(d) for d in docs))
        ctx.log.info(f"{self.name}: {len(self._claim_of)} pod(s) on "
                     f"{len(set(self._claim_of.values()))} pNFS volume(s), "
                     f"{len(self._instances)} fio instance(s)")
        fio.wait_running(ctx, self.name, self.opt("namespace"), self._pods,
                         float(self.opt("ready_timeout_s")))
        self._lvol_of = self._resolve_lvols()
        nodes = self._nodes()
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
            sc = self._get_sc(named)
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
        src = self._get_sc(source)
        params = dict(src.get("parameters", {}), **{PARAM_FSTYPE: FSTYPE_PNFS})
        name = f"sbtest-{ctx.run_id}-pnfs"
        kube.run(["delete", "sc", name, "--ignore-not-found"], check=False)
        kube.run(["apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "storage.k8s.io/v1",
            "kind": "StorageClass",
            "metadata": {"name": name, "labels": {"sbtest-run": "true"}},
            "provisioner": src.get("provisioner", "csi.simplyblock.io"),
            "parameters": params,
            "reclaimPolicy": src.get("reclaimPolicy", "Delete"),
            # Immediate: a shared claim is mounted by pods on several nodes, so there is no
            # single first consumer whose topology the volume should follow.
            "volumeBindingMode": "Immediate",
            "allowVolumeExpansion": src.get("allowVolumeExpansion", True),
        }))
        self._created_sc = name
        ctx.log.info(f"{self.name}: StorageClass {name}, cloned from {source} as pNFS")
        return name

    @staticmethod
    def _get_sc(name: str) -> dict:
        cp = kube.run(["get", "sc", name, "-o", "json"], check=False)
        if cp.returncode != 0 or not cp.stdout:
            raise RuntimeError(f"StorageClass {name} not found")
        return dict(json.loads(cp.stdout))

    def _documents(self, ctx: RunContext, sc: str) -> list[dict]:
        """The claims and pods, with every fio instance and its own file planned."""
        shared, per_shared = int(self.opt("shared_volumes")), int(self.opt("pods_per_shared"))
        solo, containers = int(self.opt("solo_pods")), int(self.opt("containers_per_pod"))
        if shared * per_shared + solo == 0 or containers < 1:
            raise RuntimeError(f"{self.name}: no pods or no containers, so the run would have "
                               "no I/O and could not detect anything")

        file_gb, volume_gb = int(self.opt("file_size_gb")), int(self.opt("volume_size_gb"))
        busiest = max(per_shared if shared else 0, 1 if solo else 0) * containers
        if busiest * file_gb + HEADROOM_GB > volume_gb:
            raise RuntimeError(
                f"{self.name}: a volume carries {busiest} fio file(s) of {file_gb}G, which "
                f"with {HEADROOM_GB}G of headroom does not fit {volume_gb}G; raise "
                "volume_size_gb or lower file_size_gb")

        plan: list[tuple[str, bool]] = []    # (claim, shared) per pod
        for v in range(shared):
            plan += [(f"{ctx.run_id}-pnfs-shared-{v}", True)] * per_shared
        plan += [(f"{ctx.run_id}-pnfs-solo-{i}", False) for i in range(solo)]

        docs: list[dict] = []
        for claim in dict.fromkeys(c for c, _ in plan):
            docs.append(self._claim(ctx, claim, sc, claim in {c for c, s in plan if s}))
        for i, (claim, is_shared) in enumerate(plan):
            pod = f"{ctx.run_id}-pnfs-{i}"
            self._pods.append(pod)
            self._claim_of[pod] = claim
            if is_shared:
                self._shared.add(claim)
            docs.append(self._pod(ctx, pod, i, claim, is_shared, containers, file_gb))
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
            spec_containers.append({
                "name": inst.container, "image": str(self.opt("image")),
                "imagePullPolicy": "IfNotPresent",
                "command": ["sh", "-c", fio.container_script(args, inst.logdir)],
                "volumeMounts": [{"name": "data", "mountPath": MOUNT},
                                 {"name": "logs", "mountPath": "/logs"}],
                "resources": {"requests": {"cpu": "250m", "memory": "256Mi"}},
            })
        spec: dict[str, Any] = {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 5,
            "containers": spec_containers,
            "volumes": [
                {"name": "data", "persistentVolumeClaim": {"claimName": claim}},
                {"name": "logs", "emptyDir": {}},
            ],
        }
        if shared and self.opt("spread"):
            spec["affinity"] = {"podAntiAffinity": {
                "preferredDuringSchedulingIgnoredDuringExecution": [{
                    "weight": 100,
                    "podAffinityTerm": {
                        "labelSelector": {"matchLabels": {"pnfs-shared": claim}},
                        "topologyKey": "kubernetes.io/hostname"}}]}}
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": pod, "labels": labels}, "spec": spec}

    def _resolve_lvols(self) -> dict[str, str]:
        """claim -> lvol UUID, from the PV's CSI handle <cluster>:<pool>:<volume>."""
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
                out[claim] = parts[2]
        return out

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
            pods = sorted(p for p, c in self._claim_of.items() if c == claim)
            volumes.append({
                "claim": claim, "lvol": self._lvol_of.get(claim, ""),
                "shared": claim in self._shared,
                "pods": pods,
                "nodes": sorted({nodes[p] for p in pods if nodes.get(p)}),
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

    # ── collection ──────────────────────────────────────────────────────────────────

    def collect(self, ctx: RunContext) -> None:
        ns = self.opt("namespace")
        migs = ctx.shared.get("migrations") or []
        for inst in self._instances:
            fio.collect_instance(ctx, ns, inst, migs)
        # One NFS mount per pod: every instance in the pod wrote through it, so every
        # instance's evidence carries the mount's counts.
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
        ctx.log.info(f"{self.name}: collected {len(self._instances)} fio instance(s) "
                     f"from {len(self._pods)} pod(s)")

    def teardown(self, ctx: RunContext) -> None:
        if ctx.shared.get("keep"):
            ctx.log.info(f"{self.name}: keep set; leaving pods, claims and classes in place")
            return
        ns = self.opt("namespace")
        kube.run(["-n", ns, "delete", "pod", "-l", f"sbtest={ctx.run_id}",
                  "--ignore-not-found", "--grace-period=5"], check=False, timeout=300)
        kube.run(["-n", ns, "delete", "pvc", "-l", f"sbtest={ctx.run_id}",
                  "--ignore-not-found"], check=False, timeout=300)
        if self._created_sc:
            kube.run(["delete", "sc", self._created_sc, "--ignore-not-found"], check=False)
        ctx.log.info(f"{self.name}: removed pods, claims and the run's StorageClass")
