"""Log-pattern detectors — user-definable checks over any collected log.

This is the extension point for "whatever the user wants". A pattern entry is data, so a
new check costs a config block rather than code:

    detectors:
      logs.pattern:
        patterns:
          - id: spdk.undrained-transfer
            regex: "still have outstanding io"
            logs: ["spdk-*"]
            severity: critical
            min_count: 1
            note: "the batch transfer started before in-flight I/O was drained"

The bundled catalogue below is what past runs turned out to care about, and it doubles as
worked examples. Everything in it is overridable: `patterns` replaces the catalogue,
`extra_patterns` appends to it.
"""

from __future__ import annotations

import fnmatch
import re
from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Any

from ..core import Detector, Evidence, Finding, Severity, SkipDetector, detector


@dataclass
class Pattern:
    """One log check. `id` is the finding subject, so keep it stable and dotted."""

    id: str
    regex: str
    #: Glob(s) over the available log names. "spdk-*" covers every storage node.
    logs: list[str] = field(default_factory=lambda: ["*"])
    severity: str = "warning"
    #: Fire only at or above this many matches. The default of 1 suits a line that should
    #: never appear; raise it for a line that is normal in small numbers.
    min_count: int = 1
    note: str = ""
    #: Cap on example lines kept in the finding, to keep reports readable.
    examples: int = 3

    def compiled(self) -> re.Pattern:
        return re.compile(self.regex)

    def matches_log(self, name: str) -> bool:
        return any(fnmatch.fnmatch(name, g) for g in self.logs)


#: Patterns worth having by default. Each one was the visible symptom of a real defect.
CATALOGUE: list[Pattern] = [
    Pattern(
        id="spdk.undrained-transfer",
        regex=r"still have outstanding io|Task transfer timeout with outstanding",
        logs=["spdk-4*", "spdk-*"],
        severity="critical",
        note="The batch delta copy started while I/O was still in flight, so the transfer "
             "failed and the control plane retried it. Each retry replays a non-idempotent "
             "step against a source that has been serving writes in between — this is the "
             "line that accompanies silent write loss. Correlate with ana.freeze-count.",
    ),
    Pattern(
        id="spdk.migration-subtask-failed",
        regex=r"Sub task failed for migration of lvol",
        logs=["spdk-*"],
        severity="critical",
        note="One member of a batch migration failed its transfer; the group is retried or "
             "abandoned as a whole.",
    ),
    Pattern(
        id="nvme.host-not-allowed",
        regex=r"does not allow host .* to connect at this address",
        logs=["spdk-*"],
        severity="warning",
        min_count=50,
        note="A host is retrying an endpoint whose allow-list no longer includes it — the "
             "signature of a leaked controller nobody disconnected. Volume, not presence, "
             "is the signal: a steady rate means a reconnect storm.",
    ),
    Pattern(
        id="nvme.write-to-readonly",
        regex=r"WRITE TO RO RANGE",
        logs=["spdk-*"],
        severity="warning",
        min_count=1,
        note="A write reached a range the target considers read-only, which during a "
             "migration means it landed on a copy that was already frozen.",
    ),
    Pattern(
        id="operator.path-validation-failed",
        regex=r"NVMe path validation failed",
        logs=["operator*"],
        severity="warning",
        note="The pre-cutover check refused a migration. Persistent failures for one "
             "subsystem usually mean a leaked controller the check keeps reporting — see "
             "nvme.stale-controllers.",
    ),
    Pattern(
        id="operator.migration-group-stuck",
        regex=r"is already past pre-create|is not active \(status=cancelled\)",
        logs=["operator*"],
        severity="warning",
        note="The operator and the control plane disagree about a migration group's state; "
             "the group needs /continue or an explicit cancel.",
    ),
    Pattern(
        id="kernel.controller-reconnect-loop",
        regex=r"Failed reconnect attempt (\d{3,})",
        logs=["dmesg-*"],
        severity="warning",
        note="A controller has retried hundreds of times. The counter resets on a partial "
             "reconnect, so a high number means it is being refused rather than merely "
             "waiting.",
    ),
]


@detector
class LogPattern(Detector):
    """Count regex hits across collected logs and report the ones over threshold.

    One pass per log, all patterns evaluated per line, because these logs are tens of
    megabytes and reading them once per pattern is what makes the difference between a check
    that runs and one that gets disabled.
    """

    name = "logs.pattern"
    summary = "configurable regex checks over any collected log (ships a catalogue)"

    def defaults(self) -> dict:
        return {"patterns": None, "extra_patterns": [], "logs": None}

    def _patterns(self) -> list[Pattern]:
        given = self.opt("patterns")
        pats = [self._as_pattern(p) for p in given] if given is not None else list(CATALOGUE)
        pats += [self._as_pattern(p) for p in (self.opt("extra_patterns") or [])]
        return pats

    @staticmethod
    def _as_pattern(p: Any) -> Pattern:
        if isinstance(p, Pattern):
            return p
        if not isinstance(p, dict):
            raise ValueError(f"pattern must be a mapping, got {type(p).__name__}")
        unknown = set(p) - set(Pattern.__dataclass_fields__)
        if unknown:
            raise ValueError(f"pattern {p.get('id', '?')!r}: unknown key(s) {sorted(unknown)}")
        return Pattern(**p)

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        available = ev.container_logs()
        if not available:
            raise SkipDetector("no container logs collected in this run")
        pats = self._patterns()
        only = self.opt("logs")
        if only:
            available = [n for n in available if any(fnmatch.fnmatch(n, g) for g in only)]

        # (pattern id, log name) -> [count, examples]
        hits: dict[tuple[str, str], list] = {}
        for log_name in available:
            active = [(p, p.compiled()) for p in pats if p.matches_log(log_name)]
            if not active:
                continue
            for line in ev.container_log(log_name):
                for p, rx in active:
                    if rx.search(line):
                        slot = hits.setdefault((p.id, log_name), [0, []])
                        slot[0] += 1
                        if len(slot[1]) < p.examples:
                            slot[1].append(line.strip()[:220])

        by_id = {p.id: p for p in pats}
        # Aggregate across logs per pattern, but keep the per-log breakdown: "which node"
        # is usually the first question, and for the undrained-transfer line it was the
        # answer (the node stuck in the retry loop).
        per_pattern: dict[str, dict[str, int]] = {}
        examples: dict[str, list[str]] = {}
        for (pid, log_name), (count, ex) in hits.items():
            per_pattern.setdefault(pid, {})[log_name] = count
            examples.setdefault(pid, []).extend(ex)

        for pid, counts in sorted(per_pattern.items()):
            p = by_id[pid]
            total = sum(counts.values())
            if total < p.min_count:
                continue
            sev = {"critical": Severity.CRITICAL, "warning": Severity.WARNING}.get(
                p.severity.lower(), Severity.INFO)
            yield Finding(
                detector=self.name,
                severity=sev,
                title=f"{total} match(es) for {pid}",
                subject=pid,
                detail="; ".join(f"{k}={v}" for k, v in sorted(counts.items(), key=lambda x: -x[1]))
                       + ("\n" + "\n".join(examples.get(pid, [])[:p.examples]) if examples.get(pid) else ""),
                evidence={"pattern": p.regex, "total": total, "per_log": counts,
                          "min_count": p.min_count},
                artifacts=[f"{k}.txt" for k in sorted(counts)],
                note=p.note,
            )
