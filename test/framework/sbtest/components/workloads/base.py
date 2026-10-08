"""The base of every fio workload: what it does around the part that makes it specific.

A workload decides what to provision and how to lay pods out, and that is all a subclass
writes: `documents` returns the claims and pods, with every fio instance it plans recorded in
`self._instances`. Everything around it is the same for every workload and lives here:
applying the documents, waiting for the pods, collecting each instance's evidence in the
layout the fio detectors read, and removing what the run created. A new workload is
therefore a new layout, never a new way of driving fio or of leaving evidence behind.
"""

from __future__ import annotations

import json
import time
from typing import Any

from ...core import Component, RunContext
from .. import kube
from . import fio


class FioWorkload(Component):
    """Provision volumes, run fio instances in pods, collect what each instance saw.

    `required`, because a run whose workload never came up is not a passing run: it is no
    run at all, and the detectors have nothing to judge.
    """

    required = True

    #: How long an instance interrupted after its runtime and grace gets to write its
    #: summary and exit.
    INTERRUPT_GRACE_S = 30

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
            "ready_timeout_s": 420,
            # How long setup() waits for every fio instance to finish laying out its file
            # and enter the timed run. 420s, operator/test/fio_migration_test.py's wait.
            "io_timeout_s": 420,
            # How long past fio's runtime stop() waits for every instance to exit on its own
            # before interrupting it. 180s, the wait operator/test/fio_migration_test.py gave
            # its pods.
            "stop_timeout_s": 180,
            **fio.FIO_DEFAULTS,
            **self.workload_defaults(),
        }

    def workload_defaults(self) -> dict[str, Any]:
        """The subclass's own options, and its overrides of the shared ones."""
        return {}

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._pods: list[str] = []
        self._instances: list[fio.FioInstance] = []
        self._created_scs: list[str] = []
        self._io_started: float | None = None   # when every instance was in the timed run

    # ── what a subclass writes ──────────────────────────────────────────────────────

    def documents(self, ctx: RunContext) -> list[dict]:
        """The claims and pods to apply, with `_pods` and `_instances` filled in."""
        raise NotImplementedError

    def after_running(self, ctx: RunContext) -> None:
        """Read back what the cluster decided, once every pod runs. Optional."""

    def after_collect(self, ctx: RunContext) -> None:
        """Collect what is specific to the workload, after every instance's evidence."""

    def selector(self, ctx: RunContext) -> str:
        """The label selector teardown deletes pods and claims by."""
        return f"sbtest={ctx.run_id}"

    # ── the lifecycle ───────────────────────────────────────────────────────────────

    def setup(self, ctx: RunContext) -> None:
        docs = self.documents(ctx)
        kube.run(["-n", self.opt("namespace"), "apply", "-f", "-"],
                 stdin="\n---\n".join(json.dumps(d) for d in docs))
        fio.wait_running(ctx, self.name, self.opt("namespace"), self._pods,
                         float(self.opt("ready_timeout_s")))
        # The runtime clock starts with the timed run, not with the pods: fio's runtime
        # does not count the layout before it.
        fio.wait_io_flowing(ctx, self.name, self.opt("namespace"), self._instances,
                            float(self.opt("io_timeout_s")))
        self._io_started = time.time()
        self.after_running(ctx)

    def stop(self, ctx: RunContext) -> None:
        """Wait for every fio instance to finish its runtime, and interrupt only stragglers.

        fio writes its JSON summary, its time series and, through the container script,
        its exit code when it exits, and verifies what is outstanding when its runtime ends.
        Suites set fio's runtime past the run's duration so fio never goes idle first, so a
        run collected when its duration ends would have none of that: no checksum or
        job-error verdict, and a result that judged no I/O. So the run waits out fio's
        remaining runtime, counted from when every instance entered the timed run, as
        operator/test/fio_migration_test.py did, plus stop_timeout_s of grace. An instance still running after that is interrupted, so a stuck fio
        cannot hang the run, and said to be, since its verification ended early.
        """
        ns = self.opt("namespace")
        started = self._io_started or time.time()
        deadline = started + float(self.opt("runtime_s")) + float(self.opt("stop_timeout_s"))
        remaining = deadline - time.time()
        if remaining > 0:
            ctx.log.info(f"{self.name}: waiting up to {remaining:.0f}s for fio to finish its "
                         "runtime in every instance")
        pending = self._wait_exited(ns, list(self._instances), deadline)
        if pending:
            ctx.log.warn(f"{self.name}: {len(pending)} fio instance(s) still running past "
                         "their runtime and grace; interrupting them, so their verification "
                         "ends early: " + ", ".join(f"{i.pod}/{i.container}" for i in pending))
            for inst in pending:
                kube.exec_sh(ns, inst.pod, "pkill -INT -x fio 2>/dev/null; true",
                             container=inst.container, timeout=30)
            pending = self._wait_exited(ns, pending, time.time() + self.INTERRUPT_GRACE_S)
        if pending:
            ctx.log.warn(f"{self.name}: {len(pending)} fio instance(s) did not exit even when "
                         "interrupted, so their summaries are missing: "
                         + ", ".join(f"{i.pod}/{i.container}" for i in pending))
        else:
            ctx.log.info(f"{self.name}: every fio instance has exited")

    def _wait_exited(self, ns: str, instances: list[fio.FioInstance],
                     deadline: float) -> list[fio.FioInstance]:
        """The instances that had not written their exit code by deadline. Checked at
        least once, so a deadline already past still reports what has exited."""
        pending = list(instances)
        while True:
            pending = [i for i in pending if not kube.exec_sh(
                ns, i.pod, f"cat {i.logdir}/fio.rc 2>/dev/null",
                container=i.container, timeout=30).strip()]
            if not pending or time.time() >= deadline:
                return pending
            time.sleep(2)

    def collect(self, ctx: RunContext) -> None:
        ns = self.opt("namespace")
        migs = ctx.shared.get("migrations") or []
        for inst in self._instances:
            fio.collect_instance(ctx, ns, inst, migs)
        self.after_collect(ctx)
        ctx.log.info(f"{self.name}: collected {len(self._instances)} fio instance(s) from "
                     f"{len(self._pods)} pod(s)")

    def teardown(self, ctx: RunContext) -> None:
        if ctx.shared.get("keep"):
            ctx.log.info(f"{self.name}: keep set; leaving {len(self._pods)} pod(s), their "
                         "claims and the StorageClasses in place")
            return
        ns, selector = self.opt("namespace"), self.selector(ctx)
        kube.run(["-n", ns, "delete", "pod", "-l", selector, "--ignore-not-found",
                  "--grace-period=5"], check=False, timeout=300)
        kube.run(["-n", ns, "delete", "pvc", "-l", selector, "--ignore-not-found"],
                 check=False, timeout=300)
        for sc in self._created_scs:
            kube.run(["delete", "sc", sc, "--ignore-not-found"], check=False)
        ctx.log.info(f"{self.name}: removed pods, claims and StorageClasses")

    # ── helpers for subclasses ──────────────────────────────────────────────────────

    def fio_container(self, inst: fio.FioInstance, script: str, mount: str) -> dict:
        """A container running one fio instance's script (see fio.container_script), with
        the data volume at mount and the log directory."""
        return {
            "name": inst.container, "image": str(self.opt("image")),
            "imagePullPolicy": "IfNotPresent",
            "command": ["sh", "-c", script],
            "volumeMounts": [{"name": "data", "mountPath": mount},
                             {"name": "logs", "mountPath": "/logs"}],
            "resources": {"requests": {"cpu": "250m", "memory": "256Mi"}},
        }

    @staticmethod
    def pod_volumes(claim: str) -> list[dict]:
        return [
            {"name": "data", "persistentVolumeClaim": {"claimName": claim}},
            # fio's own logs live on an emptyDir, never on the volume under test:
            # collecting the evidence must not depend on the health of the thing the
            # evidence is about.
            {"name": "logs", "emptyDir": {}},
        ]

    @staticmethod
    def get_storageclass(name: str) -> dict | None:
        cp = kube.run(["get", "sc", name, "-o", "json"], check=False)
        if cp.returncode != 0 or not cp.stdout:
            return None
        return dict(json.loads(cp.stdout))

    def apply_storageclass(self, src: dict, name: str, params: dict,
                           binding_mode: str | None = None) -> None:
        """(Re)create a run StorageClass from a source class, and remove it at teardown.

        Always delete-then-create, so a class left behind by a previous run can never be
        silently reused with different parameters.
        """
        kube.run(["delete", "sc", name, "--ignore-not-found"], check=False)
        kube.run(["apply", "-f", "-"], stdin=json.dumps({
            "apiVersion": "storage.k8s.io/v1",
            "kind": "StorageClass",
            "metadata": {"name": name, "labels": {"sbtest-run": "true"}},
            "provisioner": src.get("provisioner", "csi.simplyblock.io"),
            "parameters": params,
            "reclaimPolicy": src.get("reclaimPolicy", "Delete"),
            "volumeBindingMode": binding_mode or src.get("volumeBindingMode",
                                                         "WaitForFirstConsumer"),
            "allowVolumeExpansion": src.get("allowVolumeExpansion", True),
        }))
        self._created_scs.append(name)
