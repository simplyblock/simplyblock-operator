"""Host NVMe fabric detectors — leaked and non-contributing controllers.

These judge the state a run *leaves behind*, which turned out to matter as much as what it
does: a leaked controller is invisible to the run that created it and breaks the next one.
"""

from __future__ import annotations

from collections.abc import Iterable

from ..core import Detector, Evidence, Finding, SkipDetector, critical, detector, warning


@detector
class StaleControllers(Detector):
    """Controllers that cannot carry I/O and will not recover on their own.

    Two shapes, both from the migration path-leak investigation:

    * **live, serving no namespace** — the admin queue is up and enumeration finished, and
      the controller was told about nothing. It looks connected from every angle a connect
      checks, so `nvme connect` returns "already connected" and a reconciler that counts
      paths sees nothing wrong. This is the state that made a single abandoned migration
      block every later migration of the same subsystem, because the pre-cutover check
      reports it forever.
    * **stuck connecting** — retrying an endpoint that has stopped answering for this host.
      Bounded only by ctrl_loss_tmo, and the kernel resets its reconnect counter whenever a
      reconnect gets far enough, so "bounded" can mean "until the node reboots".

    Run this at the *end* of a run, and on the next run's setup: it is the check that turns
    "the last run left a mess" from a guess into a finding.
    """

    name = "nvme.stale-controllers"
    summary = "controllers that are live with no namespace, or stuck connecting"

    def defaults(self) -> dict:
        return {"max_zero_namespace": 0, "max_connecting": 0,
                # Expected paths per subsystem per host: primary plus HA replicas. Above
                # this is leak territory, but the healthy number is topology-dependent, so
                # it warns rather than fails.
                "expected_paths_per_subsystem": 3}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        ctrls = ev.nvme_controllers()
        if not ctrls:
            raise SkipDetector("no host NVMe controller snapshot in this run")

        zero = [c for c in ctrls if c.state == "live" and c.serves_nothing]
        conn = [c for c in ctrls if c.state == "connecting"]

        if len(zero) > int(self.opt("max_zero_namespace")):
            by_node: dict[str, list[str]] = {}
            for c in zero:
                by_node.setdefault(c.node, []).append(f"{c.name}@{c.address}")
            yield critical(
                self.name,
                title=f"{len(zero)} live controller(s) serving no namespace",
                subject="fabric",
                detail="; ".join(f"{n}: {', '.join(sorted(v))}" for n, v in sorted(by_node.items())),
                evidence={"count": len(zero),
                          "per_node": {n: sorted(v) for n, v in by_node.items()}},
                note="Each of these makes the pre-cutover path check fail for its "
                     "subsystem, so they block later migrations until something tears them "
                     "down. Nothing routes I/O over them, so removing them is safe.",
            )

        if len(conn) > int(self.opt("max_connecting")):
            by_node = {}
            for c in conn:
                by_node.setdefault(c.node, []).append(f"{c.name}@{c.address}")
            yield warning(
                self.name,
                title=f"{len(conn)} controller(s) stuck connecting",
                subject="fabric",
                detail="; ".join(f"{n}: {', '.join(sorted(v))}" for n, v in sorted(by_node.items())),
                evidence={"count": len(conn),
                          "per_node": {n: sorted(v) for n, v in by_node.items()},
                          "ctrl_loss_tmo": sorted({c.ctrl_loss_tmo for c in conn
                                                   if c.ctrl_loss_tmo is not None})},
                note="A snapshot cannot tell one of these from a normal HA reconnect, hence "
                     "a warning. What bounds them is ctrl_loss_tmo — a large value here "
                     "means they can outlive the run.",
            )

        # Path count per (host, subsystem): the leak seen from a different angle, and the
        # one that catches an accumulation whose members all still look individually fine.
        limit = int(self.opt("expected_paths_per_subsystem"))
        per: dict[tuple[str, str], list[str]] = {}
        for c in ctrls:
            per.setdefault((c.node, c.nqn), []).append(c.address)
        for (node, nqn), addrs in sorted(per.items()):
            if len(addrs) > limit:
                yield warning(
                    self.name,
                    title=f"{len(addrs)} paths to one subsystem on {node} (expected <= {limit})",
                    subject=f"{node}/{nqn.rsplit(':', 1)[-1]}",
                    detail=", ".join(sorted(addrs)),
                    evidence={"node": node, "nqn": nqn, "addresses": sorted(addrs),
                              "limit": limit},
                )


@detector
class LossTimeout(Detector):
    """A controller whose ctrl_loss_tmo lets it outlive the run that created it.

    Worth checking because it is the difference between a leak that expires on its own and
    one that has to be cleaned up: the control plane answered migration connects with an
    hour, while the CSI driver connects every other path with a minute. A probe path that
    may be abandoned should expire quickly.
    """

    name = "nvme.loss-timeout"
    summary = "a controller connected with a ctrl_loss_tmo long enough to outlive the run"

    def defaults(self) -> dict:
        return {"max_ctrl_loss_tmo_s": 60}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        ctrls = [c for c in ev.nvme_controllers() if c.ctrl_loss_tmo is not None]
        if not ctrls:
            raise SkipDetector("no controller snapshot with ctrl_loss_tmo")
        limit = int(self.opt("max_ctrl_loss_tmo_s"))
        bad = [c for c in ctrls
               if c.ctrl_loss_tmo is not None and c.ctrl_loss_tmo > limit]
        if not bad:
            return
        vals = sorted({c.ctrl_loss_tmo for c in bad if c.ctrl_loss_tmo is not None})
        yield warning(
            self.name,
            title=f"{len(bad)} controller(s) with ctrl_loss_tmo above {limit}s",
            subject="fabric",
            detail=f"values seen: {vals}",
            evidence={"count": len(bad), "values": vals, "limit": limit,
                      "controllers": sorted(f"{c.node}/{c.name}@{c.address}" for c in bad)[:32]},
            note="A path that may be abandoned should expire on its own. The kernel also "
                 "resets its reconnect counter on a partial reconnect, so a large value is "
                 "a floor on how long a leak survives, not a bound.",
        )
