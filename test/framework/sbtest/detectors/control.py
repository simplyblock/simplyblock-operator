"""Control-plane detectors, from the cluster's own event log.

The other half of every host-side symptom. "Every path went away" and "the control plane
decided the node was down" are the same incident told from two ends, and only one of those
ends explains *why*.

Nothing here is migration-specific — these are the failure shapes a distributed control plane
has regardless of what operation is running, which is why they belong in a framework rather
than in a migration test.
"""

from __future__ import annotations

import fnmatch
import re
from collections.abc import Iterable
from datetime import UTC, datetime, timedelta

from ..core import Detector, Evidence, Finding, SkipDetector, critical, detector, info, warning


@detector
class NodeFlap(Detector):
    """A node that went offline and came back inside one run.

    The shape behind a real 9.5-hour outage: a three-second kube-apiserver blip made the
    liveness probe conclude SPDK was dead — the probe listed pods through the Kubernetes API —
    so three storage nodes were marked offline at once, and nothing re-probed them afterwards.

    A flap is therefore worth reporting *even when it recovered*, because the recovery is
    luck: the same trigger with a slower re-probe is an outage. A short flap is the signal, not
    the noise — a node that is genuinely gone stays gone.
    """

    name = "control.node-flap"
    summary = "a storage node marked offline and back inside one run — liveness, not liveliness"

    RE_STATUS = re.compile(
        r"(?:Storage node|Management node) status changed from:?\s*(\S+)\s*to:?\s*(\S+)")
    #: States that mean the control plane stopped trusting the node.
    DOWN = {"down", "offline", "unreachable", "suspended"}
    UP = {"online", "active"}

    def defaults(self) -> dict:
        return {"max_flaps": 0,
                # A transition back inside this window is a flap rather than a real outage
                # followed by a real recovery.
                "flap_within_s": 300.0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        events = ev.control_events()
        if not events:
            raise SkipDetector("no control-plane event log collected")
        window = timedelta(seconds=float(self.opt("flap_within_s")))

        # subject -> [(ts, went_down)]
        transitions: dict[str, list] = {}
        for e in events:
            m = self.RE_STATUS.search(e.message)
            if not m:
                continue
            frm, to = m.group(1).strip(". "), m.group(2).strip(". ")
            if to in self.DOWN:
                transitions.setdefault(e.subject or "?", []).append((e.ts, True, frm, to))
            elif to in self.UP and frm in self.DOWN:
                transitions.setdefault(e.subject or "?", []).append((e.ts, False, frm, to))

        flaps = []
        for subject, seq in transitions.items():
            seq.sort()
            for i, (ts, down, _f, to) in enumerate(seq):
                if not down:
                    continue
                back = next(((t2, f2) for t2, d2, f2, _t2 in seq[i + 1:] if not d2), None)
                if back and back[0] - ts <= window:
                    flaps.append((subject, ts, (back[0] - ts).total_seconds(), to))

        if len(flaps) <= int(self.opt("max_flaps")):
            return
        yield critical(
            self.name,
            title=f"{len(flaps)} node flap(s): marked down and back within "
                  f"{float(self.opt('flap_within_s')):.0f}s",
            subject="control-plane",
            detail="; ".join(f"{s[:8]} {t:%H:%M:%S} down->up in {d:.0f}s (as {to})"
                             for s, t, d, to in sorted(flaps, key=lambda x: x[1])[:8]),
            evidence={"flaps": [{"subject": s, "at": str(t), "seconds": round(d), "state": to}
                                for s, t, d, to in flaps]},
            note="A node that recovers in seconds was probably never gone: the usual cause is "
                 "a liveness check that depends on something other than the node — an API "
                 "call, a lock, a lease. Every volume whose paths were on it took a hit for "
                 "the duration, and the recovery here was luck rather than design.",
        )


@detector
class TaskStuck(Detector):
    """A control-plane task that never reached a terminal state.

    Generic on purpose. Whatever the task is — migration, rebalance, backup, node add — one
    that is created and never resolved holds locks, blocks the next operation of its kind, and
    makes the cluster's state un-diagnosable. This is the check that says "the control plane
    stopped finishing things", which no per-operation test asks.
    """

    name = "control.task-stuck"
    summary = "a task created during the run that never reached a terminal state"

    RE_CREATED = re.compile(r"[Tt]ask created")
    RE_UPDATED = re.compile(r"[Tt]ask updated")
    TERMINAL = ("done", "completed", "failed", "cancelled", "canceled", "suspended")

    def defaults(self) -> dict:
        return {"max_stuck": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        events = ev.control_events()
        if not events:
            raise SkipDetector("no control-plane event log collected")

        created = sum(1 for e in events if self.RE_CREATED.search(e.message))
        if not created:
            raise SkipDetector("the event log records no task creations to follow")
        terminal = sum(1 for e in events
                       if self.RE_UPDATED.search(e.message)
                       and any(t in e.message.lower() for t in self.TERMINAL))
        outstanding = created - terminal
        if outstanding <= int(self.opt("max_stuck")):
            yield info(self.name, title=f"{created} task(s) created, {terminal} resolved",
                       subject="control-plane",
                       evidence={"created": created, "terminal": terminal})
            return
        yield warning(
            self.name,
            title=f"{outstanding} task(s) created but never seen to finish",
            subject="control-plane",
            detail=f"{created} created, {terminal} reached a terminal state",
            evidence={"created": created, "terminal": terminal, "outstanding": outstanding},
            note="The event log may simply end before they finished — check it against the run "
                 "window before treating this as a leak. A task that is genuinely stuck holds "
                 "its lock and blocks the next operation of its kind.",
        )


@detector
class VolumeHealth(Detector):
    """A volume whose health went false and never came back.

    Independent of what the run was doing: whatever the operation, a volume that ends the run
    unhealthy is a volume someone has to go and look at. On one archived run five volumes went
    unhealthy mid-migration and only three recovered — the other two stayed down for the rest
    of the run, which is a much more useful statement than the migration's own phase.
    """

    name = "control.volume-health"
    summary = "a volume or node whose health went false during the run and never recovered"

    RE_HEALTH = re.compile(r"(\S+) health check changed from:?\s*(\S+)\s*to:?\s*(\S+)")

    def defaults(self) -> dict:
        return {"max_unrecovered": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        events = ev.control_events()
        if not events:
            raise SkipDetector("no control-plane event log collected")
        state: dict[str, bool] = {}
        seen = False
        for e in events:
            m = self.RE_HEALTH.search(e.message)
            if not m:
                continue
            seen = True
            to = m.group(3).strip(". ").lower()
            state[e.subject or m.group(1)] = to in ("true", "healthy", "online")
        if not seen:
            raise SkipDetector("the event log records no health transitions")

        bad = sorted(k for k, ok in state.items() if not ok)
        if len(bad) <= int(self.opt("max_unrecovered")):
            return
        yield critical(
            self.name,
            title=f"{len(bad)} object(s) ended the run unhealthy",
            subject="control-plane",
            detail=", ".join(b[:12] for b in bad[:16]) + (" ..." if len(bad) > 16 else ""),
            evidence={"unhealthy": bad},
            note="Health that goes false and does not return outlives the run: the next run "
                 "starts from here. Check it against nvme.dirty-start on the following run.",
        )


@detector
class RetryStorm(Detector):
    """One operation attempted far more often than it should need to be.

    A retry loop is invisible in a pass/fail result and obvious in a count. It matters beyond
    the operation retried: each attempt usually re-does whatever the previous one half-did,
    which is how a retry turns a failure into damage — the batch-migration corruption is
    exactly that shape.
    """

    name = "control.retry-storm"
    summary = "the same control-plane operation attempted far more often than it should be"

    def defaults(self) -> dict:
        return {"max_repeats": 5, "kinds": ["STATUS_CHANGE"]}

    @staticmethod
    def _shape(msg: str) -> str:
        s = re.sub(r"[0-9a-f]{8}-[0-9a-f-]{27}", "<uuid>", msg)
        s = re.sub(r"\d+", "N", s)
        return s[:120]

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        events = ev.control_events()
        if not events:
            raise SkipDetector("no control-plane event log collected")
        kinds = set(self.opt("kinds") or [])
        counts: dict[tuple[str, str], int] = {}
        for e in events:
            if kinds and e.kind not in kinds:
                continue
            counts[(e.subject or "?", self._shape(e.message))] = \
                counts.get((e.subject or "?", self._shape(e.message)), 0) + 1

        hot = {k: v for k, v in counts.items() if v > int(self.opt("max_repeats"))}
        if not hot:
            return
        worst = sorted(hot.items(), key=lambda kv: -kv[1])[:8]
        yield warning(
            self.name,
            title=f"{len(hot)} operation(s) repeated more than {self.opt('max_repeats')} times",
            subject="control-plane",
            detail="; ".join(f"{n}x {msg}" for (_subj, msg), n in worst),
            evidence={"repeats": [{"subject": s, "shape": m, "count": n}
                                  for (s, m), n in worst]},
            note="Each attempt usually re-does what the last one half-did, which is how a "
                 "retry turns a failure into damage rather than merely delaying it.",
        )


@detector
class NodeAgent(Detector):
    """The node-side agent's own account: failed calls, and gaps in the liveness polling.

    The storage-node DaemonSet is what starts and probes the SPDK process, so it sits on the
    causal path of every "the node went offline" decision. Its access log is mostly liveness
    polling — `/snode/check`, `/snode/spdk_process_is_up`, `/snode/ping_ip`, thousands of each
    per run — and two things in it are worth watching:

    * **a non-2xx response.** The control plane asked whether SPDK was alive and got an error
      rather than an answer. Whatever it decided next, it decided on no information; this is
      the upstream half of `control.node-flap`.
    * **a gap in the polling.** Either the control plane stopped asking or the agent stopped
      answering, and both precede a node being declared down. A poll that never happened
      leaves no other trace, so this is the only place the stall is visible.

    Grounded rather than speculative: the outage this is built for was a liveness check that
    concluded SPDK was dead because a Kubernetes API call it depended on blipped for three
    seconds, and three nodes went offline at once.
    """

    name = "control.node-agent"
    summary = "the node agent returning errors, or gaps in the liveness polling it answers"

    RE_ACCESS = re.compile(r'"(?:GET|POST|PUT|DELETE) (/snode/[a-z_]+)[^"]*" (\d{3})')
    RE_TS = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})")

    def defaults(self) -> dict:
        return {"logs": ["snode-api-*"],
                # A liveness poll is expected every couple of seconds; this is where a gap
                # stops being scheduling jitter and starts being a stall.
                "max_poll_gap_s": 60.0,
                "liveness_endpoints": ["/snode/check", "/snode/spdk_process_is_up"],
                "max_errors": 0}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        names = [n for n in ev.container_logs()
                 if any(fnmatch.fnmatch(n, g) for g in self.opt("logs"))]
        if not names:
            raise SkipDetector("no node-agent logs collected (enable the storage-node "
                               "DaemonSet target in logs.collect)")

        live = set(self.opt("liveness_endpoints") or [])
        gap_limit = float(self.opt("max_poll_gap_s"))
        errors: dict[str, dict[str, int]] = {}
        gaps: list[tuple[str, str, float]] = []

        for name in names:
            last_poll: datetime | None = None
            for raw in ev.container_log(name):
                m = self.RE_ACCESS.search(raw)
                if not m:
                    continue
                endpoint, status = m.group(1), m.group(2)
                if not status.startswith("2"):
                    key = f"{endpoint} {status}"
                    per = errors.setdefault(name, {})
                    per[key] = per.get(key, 0) + 1
                if endpoint not in live:
                    continue
                mt = self.RE_TS.match(raw)
                if not mt:
                    continue
                try:
                    ts = datetime.strptime(mt.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)
                except ValueError:
                    continue
                if last_poll is not None and (ts - last_poll).total_seconds() > gap_limit:
                    gaps.append((name, f"{last_poll:%H:%M:%S}", (ts - last_poll).total_seconds()))
                last_poll = ts

        total_errors = sum(sum(v.values()) for v in errors.values())
        if total_errors > int(self.opt("max_errors")):
            yield critical(
                self.name,
                title=f"{total_errors} node-agent call(s) returned an error",
                subject="control-plane",
                detail="; ".join(f"{n}: " + ", ".join(f"{k} x{c}" for k, c in sorted(v.items()))
                                 for n, v in sorted(errors.items())),
                evidence={"errors": errors},
                note="The control plane asked and got an error rather than an answer, so "
                     "whatever it decided next it decided on no information. Read with "
                     "control.node-flap: this is the upstream half of a false offline.",
            )

        if gaps:
            worst = sorted(gaps, key=lambda g: -g[2])[:6]
            yield warning(
                self.name,
                title=f"{len(gaps)} gap(s) in the liveness polling, worst {worst[0][2]:.0f}s",
                subject="control-plane",
                detail="; ".join(f"{n} after {t} ({d:.0f}s)" for n, t, d in worst),
                evidence={"gaps": [{"log": n, "after": t, "seconds": round(d)}
                                   for n, t, d in gaps]},
                note="Either the control plane stopped asking or the agent stopped answering. "
                     "A poll that never happened leaves no other trace, so this is the only "
                     "place the stall is visible — and it is what precedes a node being "
                     "declared down.",
            )
