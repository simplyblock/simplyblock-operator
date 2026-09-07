"""The volume-migration driver: the component that makes migrations happen.

This is the first component that *is* the run rather than an observer of it, and that changes
two things about it.

It is `required`, so a failure in its setup aborts instead of being recorded — a run whose
driver never started would otherwise report a clean pass for a test that never happened.

And it drives the ANA sampler. Sampling only means something when the samples are attributed
to a migration, so the driver tells the sampler when each one begins, what phase it is in, and
when it ends. That is the one genuine dependency between components here, and it goes through
`ctx.shared` so the sampler stays optional: with sampling disabled the driver still migrates,
and the ANA detectors report themselves skipped rather than clean.

Three things it reads from the backend rather than assuming:

**Where the volume is now.** The cluster's own rebalancer moves volumes without any Kubernetes
object changing, so a source cached at setup is wrong by the tenth migration — and a wrong
source means picking a target the volume already lives on, which the operator rejects and
which reads as a product bug.

**Which volumes move together.** A migration moves the whole NVMe subsystem, so what it
affects is the volume's *group*, not the volume. The control plane decides the packing; the
driver re-reads it before every migration, because a previous migration may have changed it.

**What the migration actually did.** `status.sourceNodeUUID` is the operator's own resolved
answer and beats the driver's guess, so it is written back over it.

What it deliberately does not do is judge anything. Whether a migration was acceptable is the
detectors' business; this records what happened and moves on. That separation is why the same
run can be re-judged later against a check that did not exist when it ran.
"""

from __future__ import annotations

import contextlib
import json
import random
import threading
import time
from datetime import datetime
from typing import Any

from ..core import Component, Migration, RunContext, component, iso, now_utc
from . import kube
from .sbctl import Sbctl, SbctlError

#: Phases the operator will not move on from.
TERMINAL = ("Completed", "Failed", "Aborted")


@component
class MigrationDriver(Component):
    """Drive VolumeMigration CRs in a loop, one at a time, recording each outcome.

    One at a time on purpose. Concurrent migrations of the same subsystem are rejected by the
    control plane, and concurrent migrations of *different* subsystems make every host-side
    observation ambiguous about which one caused it. A test that cannot attribute what it saw
    produces anecdotes.
    """

    name = "migration.driver"
    summary = "create VolumeMigration CRs in a loop and record what each one did"
    required = True

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
            "api_group": "storage.simplyblock.io/v1alpha1",
            # alternate | consumer | no-consumer | random. The policy is about whether the
            # *target* also hosts a pod consuming this subsystem — the materially harder case,
            # so which one a migration exercised is chosen rather than left to chance.
            "target_policy": "alternate",
            "gap_s": 30.0,          # settle time between migrations
            "timeout_s": 600.0,     # per migration, before it is called a TIMEOUT
            "poll_s": 3.0,
            "max_migrations": 0,    # 0 = for as long as the run lasts
            # Volumes to migrate. Normally whatever the workload published; set explicitly to
            # drive migrations against volumes this framework did not create.
            "pvs": None,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._records: list[Migration] = []
        self._sb: Sbctl | None = None
        self._nodes: list[str] = []
        self._node_host: dict[str, str] = {}
        self._placement: dict[str, str] = {}   # pv -> storage node uuid
        self._volume_of: dict[str, str] = {}   # pv -> lvol uuid
        self._nqn_of: dict[str, str] = {}      # pv -> subsystem nqn
        self._groups: dict[str, list[str]] = {}
        self._pvs: list[str] = []
        self._idx = 0

    # ── setup ──────────────────────────────────────────────────────────────────────

    def setup(self, ctx: RunContext) -> None:
        self._nodes, self._node_host = self._storage_nodes()
        if len(self._nodes) < 2:
            raise RuntimeError("need at least two online storage nodes to migrate between, "
                               f"found {len(self._nodes)}")

        self._pvs = list(self.opt("pvs") or ctx.shared.get("workload.pvs") or [])
        if not self._pvs:
            raise RuntimeError(
                "no volumes to migrate: enable a workload component, or set this component's "
                "`pvs` option to migrate volumes the run did not create")

        # Reuse the workload's sbctl handle when there is one, so the webappapi pod is
        # resolved once per run rather than once per component.
        self._sb = ctx.shared.get("workload.sbctl") or Sbctl()
        self._volume_of = dict(ctx.shared.get("workload.volume_of", {}))
        self._nqn_of = dict(ctx.shared.get("workload.nqn_of", {}))
        self._groups = self._regroup()
        self._placement = dict(ctx.shared.get("workload.placement", {}))
        ctx.shared["migrations"] = self._records
        ctx.log.info(f"{self.name}: {len(self._pvs)} volume(s) across {len(self._nodes)} "
                     f"storage node(s), policy={self.opt('target_policy')}")

    def _storage_nodes(self) -> tuple[list[str], dict[str, str]]:
        """Online storage nodes as (uuids, uuid -> k8s hostname).

        Only online ones: migrating *to* an offline node is a rejected request, not a test.
        """
        cp = kube.run(["get", "storagenodes", "-A", "-o", "json"], check=False)
        try:
            items = json.loads(cp.stdout).get("items", []) if cp.stdout else []
        except json.JSONDecodeError:
            items = []
        uuids, hosts = [], {}
        for it in items:
            st = it.get("status", {})
            uuid = st.get("uuid") or ""
            if not uuid or st.get("status") != "online":
                continue
            uuids.append(uuid)
            hosts[uuid] = it.get("spec", {}).get("workerNode", "") or st.get("workerNode", "")
        return uuids, hosts

    # ── the loop ───────────────────────────────────────────────────────────────────

    def start(self, ctx: RunContext) -> None:
        def loop() -> None:
            gap = float(self.opt("gap_s"))
            cap = int(self.opt("max_migrations"))
            while not self._stop.is_set() and not ctx.stopping.is_set():
                if cap and self._idx >= cap:
                    ctx.log.info(f"{self.name}: reached max_migrations={cap}")
                    return
                self._idx += 1
                try:
                    self._one(ctx, self._idx)
                except Exception as e:  # noqa: BLE001
                    # A migration that blows up is a data point, not the end of the run: the
                    # next one may behave differently, and the detectors want the whole set.
                    ctx.log.error(f"{self.name}: migration {self._idx} errored: {e}")
                if self._stop.wait(gap):
                    return

        self._thread = threading.Thread(target=loop, name="migration-driver", daemon=True)
        self._thread.start()

    def _one(self, ctx: RunContext, idx: int) -> None:
        pv = self._pvs[(idx - 1) % len(self._pvs)]
        self._reread_subsystem(ctx, pv)
        group = self._group_of(pv)
        self._refresh_placement(group)
        source = self._placement.get(pv, "")
        target, policy, consumers = self._pick_target(ctx, group, idx, source)

        name = f"{ctx.run_id}-mig-{idx}"
        rec = Migration(name=name, start=now_utc(), source=source, target=target, pv=pv,
                        pod=ctx.shared.get("workload.pod_of", {}).get(pv, ""),
                        members=list(group))
        self._records.append(rec)

        sampler = ctx.shared.get("ana.sampler")
        if sampler is not None:
            sampler.begin(name)

        ctx.timeline.record("migration.start", subject=name, pv=pv, source=source,
                            target=target, policy=policy, members=len(group),
                            nqn=self._nqn_of.get(pv, ""))
        ctx.log.event(
            f"MIGRATION START  {name}  pv={pv}  members={len(group)}  "
            f"source={source[:8] or '?'}({self._node_host.get(source, '?')})  "
            f"target={target[:8]}({self._node_host.get(target, '?')})  policy={policy}  "
            + (f"target hosts consumer(s): {', '.join(consumers)}" if consumers
               else "target hosts no consumer"))
        # A group whose members are not all on the source is inconsistent *before* the
        # migration starts, and every later placement check would be judging that instead.
        off = [p for p in group if self._placement.get(p, source) != source]
        if off:
            ctx.log.warn(f"{self.name}: {name}: subsystem members are not all on the source "
                         f"node: {', '.join(sorted(off))}")

        try:
            kube.run(["apply", "-f", "-"], stdin=self._manifest(name, pv, target))
        except kube.KubectlError as e:
            rec.phase, rec.error, rec.end = "Failed", f"create: {e}", now_utc()
            ctx.log.error(f"{self.name}: cannot create {name}: {e}")
            if sampler is not None:
                sampler.end(ctx, name)
            return

        self._await_terminal(ctx, rec, name, sampler)

        if sampler is not None:
            sampler.end(ctx, name)
        # Where the group ended up — for the next target pick, and for the placement checks.
        self._refresh_placement(group)
        ctx.timeline.record("migration.stop", subject=name, phase=rec.phase, error=rec.error,
                            landed=self._placement.get(pv, ""))
        secs = f"{(rec.end - rec.start).total_seconds():.0f}s" if rec.end else "?"
        ctx.log.event(f"MIGRATION STOP   {name}  phase={rec.phase}  duration={secs}"
                      + (f"  error={rec.error!r}" if rec.error else ""))

    def _await_terminal(self, ctx: RunContext, rec: Migration, name: str,
                        sampler: Any) -> None:
        deadline = time.time() + float(self.opt("timeout_s"))
        poll = float(self.opt("poll_s"))
        seen = ""
        while time.time() < deadline and not self._stop.is_set():
            cp = kube.run(["-n", self.opt("namespace"), "get", "volumemigration", name,
                           "-o", "json"], check=False, timeout=30)
            if cp.returncode == 0 and cp.stdout:
                try:
                    st = json.loads(cp.stdout).get("status", {})
                except json.JSONDecodeError:
                    st = {}
                # The operator resolved the source from the control plane; ours came from a
                # listing that may already be stale, so its answer wins.
                if st.get("sourceNodeUUID"):
                    rec.source = str(st["sourceNodeUUID"])
                    self._placement[rec.pv] = rec.source
                phase = str(st.get("phase", ""))
                if phase and phase != seen:
                    seen = phase
                    # The sampler stamps this on every sample it takes, which is what makes an
                    # ANA transition readable as "during cutover" rather than "at 09:31:02".
                    if sampler is not None and hasattr(sampler, "set_phase"):
                        sampler.set_phase(phase)
                    ctx.timeline.record("migration.phase", subject=name, phase=phase)
                    ctx.log.info(f"{self.name}: {name} -> {phase}")
                if phase in TERMINAL:
                    rec.phase = phase
                    rec.error = str(st.get("errorMessage", "") or "")
                    rec.end = now_utc()
                    return
            self._stop.wait(poll)
        # Distinct from Failed on purpose: a migration the control plane rejected and one that
        # never finished are different defects, and only one of them has an error to read.
        rec.phase = "TIMEOUT"
        rec.end = now_utc()

    # ── target selection ───────────────────────────────────────────────────────────

    def _pick_target(self, ctx: RunContext, group: list[str], idx: int,
                     source: str) -> tuple[str, str, list[str]]:
        """Pick the node to migrate to, honouring the policy.

        `alternate` starts with `consumer` on the first migration, so if a run is cut short the
        harder case is the one that got exercised.

        When the policy cannot be honoured — no candidate matches — it falls back to any other
        node and records that it did, rather than skipping the migration. A migration that ran
        under the other condition is still evidence; one that did not run is not.
        """
        candidates = [n for n in self._nodes if n != source] or list(self._nodes)
        policy = str(self.opt("target_policy"))
        if policy == "alternate":
            policy = "consumer" if idx % 2 == 1 else "no-consumer"

        consuming = self._consumer_nodes(ctx, group)   # k8s host -> consuming pods
        want = None if policy == "random" else (policy == "consumer")

        pool = candidates
        if want is not None:
            matching = [n for n in candidates
                        if (self._node_host.get(n, "") in consuming) == want]
            if matching:
                pool = matching
            else:
                ctx.log.warn(
                    f"{self.name}: target policy {policy!r} cannot be honoured: no candidate "
                    f"node {'hosts' if want else 'is free of'} a consumer of this subsystem "
                    f"(consumers on {', '.join(sorted(consuming)) or 'none'}); picking any "
                    "other node")
                policy = f"{policy}(unmet)"
        target = random.choice(pool)  # noqa: S311  (test placement, not cryptography)
        return target, policy, sorted(consuming.get(self._node_host.get(target, ""), []))

    def _consumer_nodes(self, ctx: RunContext, pvs: list[str]) -> dict[str, list[str]]:
        """k8s node -> pods on it consuming any volume in this subsystem.

        The whole group, not just the volume being named: every pod holding *any* namespace of
        the subsystem has its paths moved, so each one is a consumer for this purpose.
        """
        pod_of = ctx.shared.get("workload.pod_of", {})
        node_of = ctx.shared.get("workload.node_of", {})
        out: dict[str, list[str]] = {}
        for pv in pvs:
            pod = pod_of.get(pv)
            node = node_of.get(pod) if pod else None
            if pod and node:
                out.setdefault(node, []).append(pod)
        return out

    # ── backend state ──────────────────────────────────────────────────────────────

    def _refresh_placement(self, pvs: list[str]) -> None:
        """Re-read where these volumes are, from one `sbctl volume list`.

        One listing for the whole group: its members are compared against each other, and
        per-volume lookups taken seconds apart could straddle a move and make a consistent
        subsystem look split.
        """
        if not self._sb:
            return
        lvols = {pv: self._volume_of[pv] for pv in pvs if pv in self._volume_of}
        if not lvols:
            return
        # A failure here leaves the last known placement in place: a stale source beats no
        # source, because "unknown" would make every target pick arbitrary.
        with contextlib.suppress(SbctlError):
            self._placement.update(self._sb.nodes_of(lvols))

    def _reread_subsystem(self, ctx: RunContext, pv: str) -> None:
        """Re-read this volume's subsystem before migrating it.

        Not cached from setup, because a previous migration can have changed the packing. The
        group is what a migration moves, so a stale group means sampling the wrong nodes and
        verifying the wrong volumes.
        """
        if not self._sb:
            return
        lvol = self._volume_of.get(pv, "")
        if not lvol:
            return
        nqn, _ = self._sb.subsystem_of(lvol)
        if nqn and nqn != self._nqn_of.get(pv):
            ctx.log.info(f"{self.name}: {pv} changed subsystem: "
                         f"{self._nqn_of.get(pv, '?')} -> {nqn}; regrouping")
            self._nqn_of[pv] = nqn
            self._groups = self._regroup()

    def _regroup(self) -> dict[str, list[str]]:
        groups: dict[str, list[str]] = {}
        for pv, nqn in self._nqn_of.items():
            groups.setdefault(nqn, []).append(pv)
        return {n: sorted(m) for n, m in groups.items()}

    def _group_of(self, pv: str) -> list[str]:
        """Everything a migration of `pv` moves: the volumes sharing its subsystem."""
        nqn = self._nqn_of.get(pv, "")
        return list(self._groups.get(nqn, [pv])) if nqn else [pv]

    def _manifest(self, name: str, pv: str, target: str) -> str:
        return json.dumps({
            "apiVersion": self.opt("api_group"),
            "kind": "VolumeMigration",
            "metadata": {"name": name, "namespace": self.opt("namespace"),
                         "labels": {"sbtest-run": "true", "sbtest": name}},
            "spec": {"pvName": pv, "targetNodeUUID": target},
        })

    # ── lifecycle tail ─────────────────────────────────────────────────────────────

    def stop(self, ctx: RunContext) -> None:
        self._stop.set()
        if self._thread:
            # Long enough for a migration already in flight to reach its own timeout: cutting
            # it short would record a TIMEOUT the product never caused.
            self._thread.join(timeout=float(self.opt("timeout_s")) + 30)
            self._thread = None
        done = [r for r in self._records if r.end]
        ctx.log.info(f"{self.name}: {len(done)}/{len(self._records)} migration(s) reached a "
                     "terminal state")

    def collect(self, ctx: RunContext) -> None:
        """Write the migration timeline where the analyser can read it back."""
        ctx.save_json("migrations.json", [{
            "name": r.name,
            "start": iso(r.start),
            "end": iso(r.end) if r.end else None,
            "phase": r.phase,
            "source": r.source,
            "target": r.target,
            "pv": r.pv,
            "pod": r.pod,
            "group_pvs": r.members,
            "nqn": self._nqn_of.get(r.pv, ""),
            "landed": self._placement.get(r.pv, ""),
            "error": r.error,
        } for r in self._records])
        by_phase: dict[str, int] = {}
        for r in self._records:
            by_phase[r.phase or "?"] = by_phase.get(r.phase or "?", 0) + 1
        ctx.log.info(f"{self.name}: "
                     + ", ".join(f"{k}={v}" for k, v in sorted(by_phase.items(),
                                                               key=lambda kv: -kv[1])))

    def teardown(self, ctx: RunContext) -> None:
        """Remove the CRs this run created, unless asked to keep them.

        By label rather than by name, so a CR whose creation succeeded but whose name never
        made it into the records still goes.
        """
        if ctx.shared.get("keep"):
            ctx.log.info(f"{self.name}: keep set; leaving {len(self._records)} CR(s) in place")
            return
        kube.run(["-n", self.opt("namespace"), "delete", "volumemigration",
                  "-l", "sbtest-run=true", "--ignore-not-found", "--wait=false"],
                 check=False)


def migrations_from_file(path: str) -> list[Migration]:
    """Read back what the driver wrote. Used by the archive adapter."""
    with open(path) as fh:
        raw = json.load(fh)

    def _dt(v: object) -> datetime | None:
        if not v:
            return None
        try:
            return datetime.fromisoformat(str(v).replace("Z", "+00:00"))
        except ValueError:
            return None

    out = []
    for m in raw:
        start = _dt(m.get("start"))
        if not start:
            continue
        out.append(Migration(
            name=m.get("name", ""), start=start, end=_dt(m.get("end")),
            phase=m.get("phase", ""), source=m.get("source", ""),
            target=m.get("target", ""), pv=m.get("pv", ""), pod=m.get("pod", ""),
            members=[x for x in (m.get("group_pvs") or []) if x],
            error=m.get("error", "") or ""))
    out.sort(key=lambda x: x.start)
    return out
