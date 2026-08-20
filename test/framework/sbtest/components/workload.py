"""The fio workload: volumes with continuous, verified I/O on them.

A migration test needs I/O in flight to be a test at all — a volume nobody is writing to
migrates cleanly whatever the code does, because nothing is there to lose. So this component
provisions volumes, drives fio against them for the length of the run, and collects what fio
saw.

Two things here are less obvious than they look.

**Verification is only enabled when it can be trusted.** fio's md5 verify races itself when
two in-flight I/Os touch the same block, and reports corruption that never happened. With one
job that is solvable (`--serialize_overlap`); across processes it is not, without
`io_submit_mode=offload`. So `numjobs > 1` turns verification *off* and says so loudly,
because a run that measures throughput is not a run that would have noticed data loss — and
the report must not imply otherwise.

**Two StorageClasses, not one.** Batch migration only exists when several volumes share an
NVMe subsystem, and whether they do is the control plane's decision, not the test's. So the
namespaced volumes get their own class and the real grouping is *read back* from the backend
afterwards. If the volumes each ended up in their own subsystem, the batch half of the run is
vacuous — which is worth knowing at minute one rather than during the analysis.
"""

from __future__ import annotations

import json
import time
from typing import Any

from ..core import Component, RunContext, component
from . import kube
from .sbctl import Sbctl, SbctlError

FIO_IMAGE = "alpine:3.20"
PARAM_MAX_NS = "max_namespace_per_subsys"


@component
class FioWorkload(Component):
    """Provision volumes, run verified fio on them, collect what fio saw.

    `required`, because a run whose workload never came up is not a passing run — it is no run
    at all, and the detectors have nothing to judge.
    """

    name = "workload.fio"
    summary = "provision volumes and drive continuous verified fio I/O against them"
    required = True

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
            # The operator's own pool SC, named simplyblock-<ns>-<cluster>-<pool>. Cloned
            # rather than hand-written so the run inherits whatever the pool really uses.
            "source_storageclass": "simplyblock-default-simplyblock-cluster-pool1",
            "pods": 0,              # single-namespace volumes
            "ns_pods": 6,           # namespaced volumes (share subsystems -> batch migration)
            "ns_per_subsys": 6,
            "volume_size_gb": 10,
            "file_size_gb": 1,
            "fstype": "xfs",
            "runtime_s": 3600,
            "iodepth": 8,
            "numjobs": 1,           # >1 disables verification; see the module docstring
            "verify_fatal": False,
            "bs": "4k",
            "rwmixread": 70,
            "ioengine": "libaio",
            "ready_timeout_s": 420,
            "image": FIO_IMAGE,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._sb = Sbctl()
        self._pods: list[str] = []
        self._pvcs: list[str] = []
        self._pvc_of: dict[str, str] = {}
        self._kind_of: dict[str, str] = {}
        self._pv_of: dict[str, str] = {}       # pod -> pv
        self._pod_of: dict[str, str] = {}      # pv  -> pod
        self._volume_of: dict[str, str] = {}   # pv  -> lvol uuid
        self._nqn_of: dict[str, str] = {}      # pv  -> subsystem nqn
        self._nsid_of: dict[str, int] = {}
        self._groups: dict[str, list[str]] = {}  # nqn -> pvs, ns_id ordered
        self._node_host: dict[str, str] = {}     # storage node uuid -> k8s host
        self._sc_single = ""
        self._sc_ns = ""

    # ── setup: classes, volumes, pods, then read back what really happened ─────────

    def setup(self, ctx: RunContext) -> None:
        self._node_host = self._storage_node_hosts()
        self._ensure_storageclasses(ctx)
        self._create(ctx)
        self._wait_running(ctx)
        self._resolve_pvs(ctx)
        self._resolve_subsystems(ctx)
        self._publish(ctx)

    def _storage_node_hosts(self) -> dict[str, str]:
        cp = kube.run(["get", "storagenodes", "-A", "-o", "json"], check=False)
        try:
            items = json.loads(cp.stdout).get("items", []) if cp.stdout else []
        except json.JSONDecodeError:
            return {}
        out = {}
        for it in items:
            st = it.get("status", {})
            uuid = st.get("uuid") or ""
            if uuid:
                out[uuid] = it.get("spec", {}).get("workerNode", "") or st.get("workerNode", "")
        return out

    def _ensure_storageclasses(self, ctx: RunContext) -> None:
        """(Re)create the run's StorageClasses from the live pool SC.

        Always delete-then-create, so a class left behind by a previous run can never be
        silently reused with different parameters. And `cluster_id` is forced to the live
        cluster rather than copied: the source SC can still carry a dead one after a
        reinstall, and every volume from it would target a cluster that no longer exists.
        """
        src_name = self.opt("source_storageclass")
        cp = kube.run(["get", "sc", src_name, "-o", "json"], check=False)
        if cp.returncode != 0 or not cp.stdout:
            # The name embeds the namespace, cluster and pool, so it differs per install.
            # Naming the candidates turns a config error into a one-line fix instead of a
            # trip to kubectl.
            raise RuntimeError(
                f"source StorageClass {src_name} not found. Either the name is wrong for this "
                f"install — candidates: {', '.join(self._candidate_sources()) or 'none'} — or "
                "the operator has not created it yet, which usually means the Pool is not "
                "Active (its create is stuck retrying on the backend). Check: kubectl -n "
                f"{self.opt('namespace')} get pool -o jsonpath='{{.items[*].status}}'")
        src = json.loads(cp.stdout)

        cluster = self._sb.cluster_uuid()
        base = dict(src.get("parameters", {}))
        stale = base.get("cluster_id")
        if stale and stale != cluster:
            ctx.log.warn(f"{self.name}: source SC {src_name} carries stale cluster_id "
                         f"{stale}; overriding with the live cluster {cluster}")
        base["cluster_id"] = cluster
        base["csi.storage.k8s.io/fstype"] = str(self.opt("fstype"))
        # A single-namespace class must say so explicitly rather than inherit whatever the
        # source SC or the CSI default carries — otherwise the "single" half of the run could
        # quietly be namespaced too, and the comparison would be with itself.
        base.pop(PARAM_MAX_NS, None)

        self._sc_single = f"sbtest-{ctx.run_id}-single"
        self._apply_sc(src, self._sc_single, dict(base, **{PARAM_MAX_NS: "1"}))
        ctx.log.info(f"{self.name}: StorageClass {self._sc_single} "
                     f"(fstype={self.opt('fstype')}, {PARAM_MAX_NS}=1, cluster_id={cluster})")
        if int(self.opt("ns_pods")) > 0:
            self._sc_ns = f"sbtest-{ctx.run_id}-ns"
            self._apply_sc(src, self._sc_ns,
                           dict(base, **{PARAM_MAX_NS: str(self.opt("ns_per_subsys"))}))
            ctx.log.info(f"{self.name}: StorageClass {self._sc_ns} "
                         f"({PARAM_MAX_NS}={self.opt('ns_per_subsys')})")

    @staticmethod
    def _candidate_sources() -> list[str]:
        """simplyblock StorageClasses that are not themselves a test artifact.

        Anything carrying `sbtest-` is one of ours from an earlier run and would be a
        confusing suggestion — cloning a clone inherits its overrides.
        """
        cp = kube.run(["get", "sc", "-o", "json"], check=False)
        if cp.returncode != 0 or not cp.stdout:
            return []
        try:
            items = json.loads(cp.stdout).get("items", [])
        except json.JSONDecodeError:
            return []
        return sorted(
            it["metadata"]["name"] for it in items
            if it.get("provisioner") == "csi.simplyblock.io"
            and "sbtest-" not in it["metadata"]["name"])

    @staticmethod
    def _apply_sc(src: dict, name: str, params: dict) -> None:
        kube.run(["delete", "sc", name, "--ignore-not-found"], check=False)
        kube.run(["apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "storage.k8s.io/v1",
            "kind": "StorageClass",
            "metadata": {"name": name, "labels": {"sbtest-run": "true"}},
            "provisioner": src.get("provisioner", "csi.simplyblock.io"),
            "parameters": params,
            "reclaimPolicy": src.get("reclaimPolicy", "Delete"),
            "volumeBindingMode": src.get("volumeBindingMode", "WaitForFirstConsumer"),
            "allowVolumeExpansion": src.get("allowVolumeExpansion", True),
        }))

    def _create(self, ctx: RunContext) -> None:
        script = self._fio_script(ctx)
        affinity = self._affinity(ctx)
        docs: list[dict] = []
        # One continuous index across both kinds, so a single name filter matches every pod
        # this component created.
        plan = ([("single", self._sc_single)] * int(self.opt("pods"))
                + [("namespaced", self._sc_ns)] * int(self.opt("ns_pods")))
        if not plan:
            raise RuntimeError("workload.fio: both `pods` and `ns_pods` are 0, so the run "
                               "would have no I/O and could not detect anything")
        for i, (kind, sc) in enumerate(plan):
            pvc, pod = f"{ctx.run_id}-pvc-{i}", f"{ctx.run_id}-fio-{i}"
            self._pvcs.append(pvc)
            self._pods.append(pod)
            self._pvc_of[pod] = pvc
            self._kind_of[pod] = kind
            labels = {"sbtest": ctx.run_id, "sbtest-run": "true", "volume-kind": kind}
            docs.append({
                "apiVersion": "v1", "kind": "PersistentVolumeClaim",
                "metadata": {"name": pvc, "labels": labels},
                "spec": {"accessModes": ["ReadWriteOnce"], "storageClassName": sc,
                         "resources": {"requests": {
                             "storage": f"{self.opt('volume_size_gb')}Gi"}}},
            })
            docs.append({
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": pod, "labels": dict(labels, app="fio")},
                "spec": {
                    "restartPolicy": "Never",
                    "terminationGracePeriodSeconds": 5,
                    "affinity": affinity,
                    "containers": [{
                        "name": "fio", "image": str(self.opt("image")),
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["sh", "-c", script],
                        "volumeMounts": [{"name": "data", "mountPath": "/data"},
                                         {"name": "logs", "mountPath": "/logs"}],
                        "resources": {"requests": {"cpu": "250m", "memory": "256Mi"}},
                    }],
                    "volumes": [
                        {"name": "data", "persistentVolumeClaim": {"claimName": pvc}},
                        # fio's own logs live on an emptyDir, never on the volume under test:
                        # collecting the evidence must not depend on the health of the thing
                        # the evidence is about.
                        {"name": "logs", "emptyDir": {}},
                    ],
                },
            })
        kube.run(["-n", self.opt("namespace"), "apply", "-f", "-"],
                 stdin="\n---\n".join(json.dumps(d) for d in docs))
        ctx.log.info(f"{self.name}: created {len(plan)} PVC(s) + fio pod(s): "
                     f"{self.opt('pods')} single-namespace + {self.opt('ns_pods')} namespaced")

    def _affinity(self, ctx: RunContext) -> dict:
        """Pin fio pods to the storage worker nodes and off the control plane.

        Off the control plane in particular: a consuming pod there would exercise a path the
        product does not otherwise have, and a fio pod competing with the API server produces
        latency findings about the test rather than the system.
        """
        workers = sorted({h for h in self._node_host.values() if h})
        exprs: list[dict] = [{"key": "node-role.kubernetes.io/control-plane",
                              "operator": "DoesNotExist"}]
        if workers:
            exprs.insert(0, {"key": "kubernetes.io/hostname", "operator": "In",
                             "values": workers})
            ctx.log.info(f"{self.name}: pods restricted to {', '.join(workers)}")
        else:
            ctx.log.warn(f"{self.name}: no storage worker hostnames known; only excluding "
                         "control-plane nodes")
        return {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
            "nodeSelectorTerms": [{"matchExpressions": exprs}]}}}

    def _fio_script(self, ctx: RunContext) -> str:
        # fio always lays out a file-backed target before random I/O and cannot be told to
        # skip it for a filesystem file. A large working set buys nothing here — the point is
        # continuous I/O, not capacity — so keep the file small and the layout takes seconds.
        file_gb = max(1, min(int(self.opt("file_size_gb")),
                             int(self.opt("volume_size_gb")) - 2))
        numjobs = int(self.opt("numjobs"))
        args = [
            "fio", "--name=fiotest", "--filename=/data/fiotest", f"--size={file_gb}G",
            f"--ioengine={self.opt('ioengine')}", "--direct=1", "--rw=randrw",
            f"--rwmixread={self.opt('rwmixread')}", f"--bs={self.opt('bs')}",
            f"--iodepth={self.opt('iodepth')}", f"--numjobs={numjobs}",
            "--group_reporting", "--time_based", f"--runtime={self.opt('runtime_s')}",
            "--continue_on_error=all",   # record EIO, do not die on it
            "--percentile_list=50:95:99:99.9",
            "--write_iops_log=/logs/iops", "--write_lat_log=/logs/lat",
            "--write_bw_log=/logs/bw", "--log_avg_msec=1000",
            "--eta=always", "--eta-newline=30",
            "--output=/logs/result.json", "--output-format=json",
        ]
        if numjobs == 1:
            args += [
                # An md5 header per block, re-verified continuously during the run rather
                # than only at the end — so a lost write surfaces while the volume is still
                # migrating, close enough in time to attribute to a migration.
                "--verify=md5", "--verify_backlog=4096", "--verify_backlog_batch=4096",
                # Dump the mismatching block so its content can be read afterwards: a block
                # holding its pre-write content is a lost write, which is a different defect
                # from a block holding garbage.
                "--verify_dump=1",
                # Not fatal by default, so EVERY corrupted block gets reported instead of
                # only the first. That turns the count into a measurement — with a known
                # write rate it bounds how wide the window of lost writes was. Fatal stops at
                # the first, making the count a lower bound of 1 that says nothing about size.
                f"--verify_fatal={1 if self.opt('verify_fatal') else 0}",
            ]
            if int(self.opt("iodepth")) > 1:
                args.append("--serialize_overlap=1")
        else:
            ctx.log.warn(
                f"{self.name}: data-integrity verification DISABLED: numjobs={numjobs} (>1 "
                "cannot serialize overlapping writes without io_submit_mode=offload, so "
                "verify would report corruption that never happened). This run measures I/O "
                "only, not integrity — use iodepth for concurrency instead")
        return (
            "set -u\n"
            'echo "[pod] $(date -u +%FT%TZ) installing fio"\n'
            "apk add --no-cache fio >/dev/null 2>&1 || "
            '{ echo "[pod] apk add fio FAILED"; exit 90; }\n'
            "mkdir -p /logs\n"
            'echo "[pod] $(date -u +%FT%TZ) starting fio"\n'
            + " ".join(args) + "\n"
            "rc=$?\n"
            'echo "$rc" > /logs/fio.rc\n'
            'echo "[pod] $(date -u +%FT%TZ) fio exited rc=$rc"\n'
            # Stay alive after fio exits so the logs on the emptyDir can still be collected.
            "sleep 100000\n"
        )

    def _wait_running(self, ctx: RunContext) -> None:
        deadline = time.time() + float(self.opt("ready_timeout_s"))
        ns = self.opt("namespace")
        phases: dict[str, str] = {}
        while time.time() < deadline and not ctx.stopping.is_set():
            cp = kube.run(["-n", ns, "get", "pods", "-l", f"sbtest={ctx.run_id}",
                           "-o", "json"], check=False)
            phases = {}
            if cp.returncode == 0 and cp.stdout:
                for it in json.loads(cp.stdout).get("items", []):
                    phases[it["metadata"]["name"]] = it.get("status", {}).get("phase", "?")
            bad = [f"{p}={ph}" for p, ph in phases.items() if ph in ("Failed", "Unknown")]
            if bad:
                raise RuntimeError(f"fio pod(s) failed during startup: {', '.join(bad)}")
            running = [p for p, ph in phases.items() if ph == "Running"]
            if len(running) == len(self._pods):
                ctx.log.info(f"{self.name}: all {len(running)} fio pod(s) Running")
                return
            time.sleep(5)
        # Name the pods that did not make it and what they are stuck at: "6 of 6 did not
        # start" sends someone to kubectl for the one fact the message could have carried.
        stuck = ", ".join(f"{p}={phases.get(p, 'absent')}" for p in self._pods
                          if phases.get(p) != "Running")
        raise RuntimeError(
            f"only {sum(1 for p in self._pods if phases.get(p) == 'Running')} of "
            f"{len(self._pods)} fio pod(s) reached Running within "
            f"{self.opt('ready_timeout_s')}s: {stuck}")

    def _resolve_pvs(self, ctx: RunContext) -> None:
        ns = self.opt("namespace")
        for pod in self._pods:
            pvc = self._pvc_of[pod]
            cp = kube.run(["-n", ns, "get", "pvc", pvc, "-o", "json"])
            pv = json.loads(cp.stdout).get("spec", {}).get("volumeName", "")
            if not pv:
                raise RuntimeError(f"PVC {pvc} has no bound PV")
            cp = kube.run(["get", "pv", pv, "-o", "json"])
            handle = json.loads(cp.stdout).get("spec", {}).get("csi", {}).get(
                "volumeHandle", "")
            # "<clusterUUID>:<poolUUID>:<volumeUUID>"
            parts = handle.split(":")
            if len(parts) != 3 or not parts[2]:
                raise RuntimeError(f"PV {pv} has an unexpected CSI volume handle {handle!r}")
            self._pv_of[pod] = pv
            self._pod_of[pv] = pod
            self._volume_of[pv] = parts[2]
        ctx.log.info(f"{self.name}: resolved PVC -> PV -> lvol:")
        for pod in self._pods:
            pv = self._pv_of[pod]
            ctx.log.info(f"    {pod}  {self._pvc_of[pod]} -> {pv} "
                         f"(lvol {self._volume_of[pv]}, {self._kind_of[pod]})")

    def _resolve_subsystems(self, ctx: RunContext) -> None:
        """Read each volume's real NVMe subsystem from the backend and group by it.

        Discovered, never assumed: the control plane decides the packing, and everything
        downstream — which volumes a migration moves, which nodes must be sampled — follows
        from the real grouping.
        """
        for pv, lvol in self._volume_of.items():
            nqn, nsid = self._sb.subsystem_of(lvol)
            if not nqn:
                ctx.log.warn(f"{self.name}: cannot resolve the subsystem of {pv} "
                             f"(lvol {lvol}); it will migrate as a group of one")
                continue
            self._nqn_of[pv] = nqn
            self._nsid_of[pv] = nsid
        groups: dict[str, list[str]] = {}
        for pv, nqn in self._nqn_of.items():
            groups.setdefault(nqn, []).append(pv)
        self._groups = {n: sorted(m, key=lambda p: self._nsid_of.get(p, 0))
                        for n, m in groups.items()}

        shared = {n: m for n, m in self._groups.items() if len(m) > 1}
        ctx.log.info(f"{self.name}: {len(self._groups)} subsystem(s), {len(shared)} shared by "
                     "more than one volume")
        for nqn, pvs in sorted(self._groups.items(), key=lambda kv: -len(kv[1])):
            members = ", ".join(f"{self._pod_of.get(p, p)}(ns{self._nsid_of.get(p, '?')})"
                                for p in pvs)
            ctx.log.info(f"    {nqn}  [{len(pvs)}]  {members}")

        # Say this at minute one rather than leaving it for the analysis: if the namespaced
        # volumes each got their own subsystem, no batch migration can be exercised at all,
        # and the run is only testing the single-namespace path whatever its verdict says.
        if int(self.opt("ns_pods")) >= 2 and not shared:
            ctx.log.warn(
                f"{self.name}: the {self.opt('ns_pods')} namespaced volume(s) each got their "
                f"OWN subsystem ({PARAM_MAX_NS}={self.opt('ns_per_subsys')}), so no batch "
                "migration can be exercised. Check that the CSI driver honours "
                f"{PARAM_MAX_NS} and that the volumes landed on the same storage node — a "
                "subsystem is shared per node")

    def _publish(self, ctx: RunContext) -> None:
        """Hand the driver everything it needs to choose targets and attribute observations."""
        pvs = [self._pv_of[p] for p in self._pods if p in self._pv_of]
        node_of: dict[str, str] = {}
        cp = kube.run(["-n", self.opt("namespace"), "get", "pods", "-l",
                       f"sbtest={ctx.run_id}", "-o", "json"], check=False)
        if cp.returncode == 0 and cp.stdout:
            for it in json.loads(cp.stdout).get("items", []):
                node_of[it["metadata"]["name"]] = it.get("spec", {}).get("nodeName", "") or ""

        ctx.shared.update({
            "workload.pvs": pvs,
            "workload.pods": list(self._pods),
            "workload.pod_of": dict(self._pod_of),
            "workload.pv_of": dict(self._pv_of),
            "workload.volume_of": dict(self._volume_of),
            "workload.node_of": node_of,
            "workload.nqn_of": dict(self._nqn_of),
            "workload.subsystem_of": {pv: list(self._groups.get(self._nqn_of.get(pv, ""),
                                                                [pv]))
                                      for pv in pvs},
            "workload.placement": self._placement(),
            "workload.sbctl": self._sb,
        })

    def _placement(self) -> dict[str, str]:
        try:
            return self._sb.nodes_of(dict(self._volume_of))
        except SbctlError:
            return {}

    # ── collection ─────────────────────────────────────────────────────────────────

    def collect(self, ctx: RunContext) -> None:
        """Pull fio's own account out of each pod, into the layout the analyser reads.

        Read with `exec cat` rather than `kubectl cp`, which truncates large files without
        reporting an error — a silently short result.json reads as a clean run.
        """
        ns = self.opt("namespace")
        migs = ctx.shared.get("migrations") or []
        for pod in self._pods:
            d = ctx.dir(pod)
            for remote, local in (("/logs/result.json", "result.json"),
                                  ("/logs/fio.rc", "fio.rc")):
                out = kube.exec_sh(ns, pod, f"cat {remote} 2>/dev/null", timeout=120)
                if out:
                    with open(f"{d}/{local}", "w") as fh:
                        fh.write(out)
            log = kube.run(["-n", ns, "logs", pod, "--tail=-1"], check=False, timeout=300)
            if log.stdout:
                with open(f"{d}/fio.log", "w") as fh:
                    fh.write(log.stdout)
            self._write_timeseries(ctx, ns, pod, d, migs)
        ctx.log.info(f"{self.name}: collected fio output for {len(self._pods)} pod(s)")

    def _write_timeseries(self, ctx: RunContext, ns: str, pod: str, d: str,
                          migs: list) -> None:
        """Per-second IOPS from fio's iops log, with the migration in flight that second.

        The migration column is the point: correlating a throughput dip with the migration
        that caused it is otherwise a manual join across two files, and the detectors need it
        to attribute an outage to a specific migration rather than to the run.

        Column names match the older harness's CSVs (`second`, `wall_clock`) so a single
        reader serves both.
        """
        raw = kube.exec_sh(ns, pod, "cat /logs/iops.*log 2>/dev/null", timeout=180)
        if not raw.strip():
            return
        # fio: <msec since start>, <value>, <rw 0=read 1=write>, <bs>, ...
        per_sec: dict[int, dict[str, float]] = {}
        for line in raw.splitlines():
            f = [x.strip() for x in line.split(",")]
            if len(f) < 3:
                continue
            try:
                sec = int(int(f[0]) / 1000)
                val = float(f[1])
                rw = int(f[2])
            except ValueError:
                continue
            row = per_sec.setdefault(sec, {"read": 0.0, "write": 0.0})
            row["read" if rw == 0 else "write"] += val
        if not per_sec:
            return

        start, _ = ctx.window()
        with open(f"{d}/timeseries.csv", "w") as fh:
            fh.write("second,wall_clock,total_iops,read_iops,write_iops,active_migration\n")
            for sec in sorted(per_sec):
                row = per_sec[sec]
                total = row["read"] + row["write"]
                wall = ""
                active = ""
                if start:
                    from datetime import timedelta
                    ts = start + timedelta(seconds=sec)
                    wall = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
                    active = next((m.name for m in migs if m.covers(ts)), "")
                fh.write(f"{sec},{wall},{total},{row['read']},{row['write']},{active}\n")

    def teardown(self, ctx: RunContext) -> None:
        if ctx.shared.get("keep"):
            ctx.log.info(f"{self.name}: keep set; leaving {len(self._pods)} pod(s), "
                         f"{len(self._pvcs)} PVC(s) and the StorageClasses in place")
            return
        ns = self.opt("namespace")
        kube.run(["-n", ns, "delete", "pod", "-l", "sbtest-run=true",
                  "--ignore-not-found", "--grace-period=5"], check=False, timeout=300)
        kube.run(["-n", ns, "delete", "pvc", "-l", "sbtest-run=true",
                  "--ignore-not-found"], check=False, timeout=300)
        for sc in (self._sc_single, self._sc_ns):
            if sc:
                kube.run(["delete", "sc", sc, "--ignore-not-found"], check=False)
        ctx.log.info(f"{self.name}: removed pods, PVCs and StorageClasses")
