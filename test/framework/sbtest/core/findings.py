"""Findings — what a detector produces, and how a run is judged from them.

A finding is deliberately more than a log line. The thing that made the migration
post-mortems expensive was never noticing that something was wrong; it was reconstructing,
hours later, *which* migration a symptom belonged to and *what evidence* said so. So a
finding carries its subject, the evidence that supports it, and where to look next — and
the report is assembled from findings rather than printed as it goes.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import Enum, IntEnum


class Attribution(Enum):
    """Whether a finding is something *this run* did.

    dmesg is a ring buffer and a cluster outlives its runs, so evidence routinely contains
    damage and debris from before the run began. Counting that against the run is how a
    green build gets blamed for its predecessor's mess — and, worse, how a genuinely broken
    run hides inside inherited noise.

    The distinction changes the verdict rather than merely annotating it. A CRITICAL the run
    caused is a FAIL. A CRITICAL it *inherited* means the environment was already damaged, so
    the run cannot be trusted either way: that is INCONCLUSIVE, which is a different
    conversation (fix the environment, run again) from a failure (fix the code).
    """

    RUN = "run"                    # happened inside the run's window
    PRE_EXISTING = "pre-existing"  # happened before the run started
    UNKNOWN = "unknown"            # no usable timestamp, or no known run window

    def __str__(self) -> str:  # noqa: D105
        return self.value


class Severity(IntEnum):
    """How much a finding matters. Ordered, so `max()` over findings is the verdict.

    The line that matters is CRITICAL vs the rest: CRITICAL fails the run. WARNING is for
    something a human should read but that does not condemn the build — a threshold
    approached, a cleanup that did not complete. INFO carries context that is only
    interesting next to another finding.
    """

    INFO = 10
    WARNING = 20
    CRITICAL = 30

    def __str__(self) -> str:  # noqa: D105
        return self.name


@dataclass
class Finding:
    """One defect, with everything needed to act on it without re-deriving it.

    `subject` is what the finding is *about* — a migration name, a pod, a node. It is what
    makes findings joinable across detectors, which is where most of the diagnostic value
    turned out to be: "the four migrations that re-froze" and "the four migrations that lost
    writes" are only obviously the same set if both name their subjects the same way.
    """

    detector: str
    severity: Severity
    title: str
    subject: str = ""
    detail: str = ""
    #: Whether the run caused this. Defaults to RUN: most detectors read evidence that only
    #: exists because the run produced it, so anything they find is the run's. Detectors
    #: reading a ring buffer or a long-lived cluster's state must say otherwise.
    attribution: Attribution = Attribution.RUN
    # Structured backing for the claim: counts, timestamps, thresholds. Goes to JSON
    # verbatim, so keep it to primitives.
    evidence: dict = field(default_factory=dict)
    # Where a human should look — artifact paths, log offsets.
    artifacts: list[str] = field(default_factory=list)
    # Optional remediation or interpretation note. Used for findings whose meaning is not
    # obvious from the title, which is most of the interesting ones.
    note: str = ""

    def to_dict(self) -> dict:
        d = asdict(self)
        d["severity"] = str(self.severity)
        d["attribution"] = str(self.attribution)
        return d

    def one_line(self) -> str:
        head = f"[{self.severity}] {self.detector}"
        if self.subject:
            head += f" {self.subject}"
        if self.attribution is not Attribution.RUN:
            head += f" ({self.attribution})"
        return f"{head}: {self.title}"

    @property
    def counts_against_the_run(self) -> bool:
        """Whether this finding may fail the run.

        UNKNOWN counts: a detector that cannot place an event in time has found something
        real, and treating "I am not sure when" as "not this run" is how a defect gets
        excused. Only evidence positively dated before the run is excluded.
        """
        return self.attribution is not Attribution.PRE_EXISTING


def _make(detector: str, severity: Severity, title: str, subject: str, detail: str,
          evidence: dict | None, artifacts: list[str] | None, note: str,
          attribution: Attribution) -> Finding:
    return Finding(detector=detector, severity=severity, title=title, subject=subject,
                   detail=detail, evidence=evidence or {}, artifacts=artifacts or [],
                   note=note, attribution=attribution)


def critical(detector: str, title: str, subject: str = "", detail: str = "",
             evidence: dict | None = None, artifacts: list[str] | None = None,
             note: str = "",
             attribution: Attribution = Attribution.RUN) -> Finding:
    """A defect that fails the run."""
    return _make(detector, Severity.CRITICAL, title, subject, detail, evidence, artifacts, note,
                 attribution)


def warning(detector: str, title: str, subject: str = "", detail: str = "",
            evidence: dict | None = None, artifacts: list[str] | None = None,
            note: str = "",
            attribution: Attribution = Attribution.RUN) -> Finding:
    """Something a human should read that does not condemn the run."""
    return _make(detector, Severity.WARNING, title, subject, detail, evidence, artifacts, note,
                 attribution)


def info(detector: str, title: str, subject: str = "", detail: str = "",
         evidence: dict | None = None, artifacts: list[str] | None = None,
         note: str = "",
         attribution: Attribution = Attribution.RUN) -> Finding:
    """Context that is interesting next to another finding."""
    return _make(detector, Severity.INFO, title, subject, detail, evidence, artifacts, note,
                 attribution)


@dataclass
class Report:
    """Every finding of a run, plus the verdict they add up to."""

    run_id: str = ""
    findings: list[Finding] = field(default_factory=list)
    # Detectors that ran but could not decide, and why. A detector that silently returns
    # nothing because its evidence was missing is indistinguishable from one that returned
    # nothing because the run was clean — which is the failure mode that lets a broken
    # check pass a broken run for weeks.
    skipped: dict[str, str] = field(default_factory=dict)

    def add(self, *findings: Finding) -> None:
        self.findings.extend(findings)

    def skip(self, detector: str, reason: str) -> None:
        self.skipped[detector] = reason

    @property
    def verdict(self) -> str:
        """PASS, FAIL, or INCONCLUSIVE.

        INCONCLUSIVE is not a softer FAIL, it is a different problem. It means the run began
        in a state that was already broken — a filesystem shut down before it started, a host
        retrying controllers for a cluster that no longer exists — so neither a pass nor a
        failure can be believed. The action is to clean the environment and run again, not to
        go looking through the code.
        """
        if self.failed:
            return "FAIL"
        if self.inherited_damage:
            return "INCONCLUSIVE"
        return "PASS"

    @property
    def failed(self) -> bool:
        """A CRITICAL this run is answerable for."""
        return any(f.severity >= Severity.CRITICAL and f.counts_against_the_run
                   for f in self.findings)

    @property
    def inherited_damage(self) -> bool:
        """A CRITICAL that pre-dates the run — the environment was already broken."""
        return any(f.severity >= Severity.CRITICAL and not f.counts_against_the_run
                   for f in self.findings)

    def attributed(self, attribution: Attribution) -> list[Finding]:
        return [f for f in self.findings if f.attribution is attribution]

    def of_severity(self, sev: Severity, counting_only: bool = False) -> list[Finding]:
        return [f for f in self.findings if f.severity == sev
                and (f.counts_against_the_run or not counting_only)]

    def by_subject(self) -> dict[str, list[Finding]]:
        """Findings grouped by what they are about, worst first within each subject.

        This is the join that turns separate detectors into a diagnosis: a subject carrying
        both a freeze-count finding and a checksum finding is a much stronger statement than
        either alone.
        """
        out: dict[str, list[Finding]] = {}
        for f in self.findings:
            out.setdefault(f.subject or "-", []).append(f)
        for v in out.values():
            v.sort(key=lambda f: -int(f.severity))
        return out

    def to_dict(self) -> dict:
        return {
            "run_id": self.run_id,
            "verdict": self.verdict,
            "counts": {str(s): len(self.of_severity(s)) for s in Severity},
            "counts_by_attribution": {
                str(a): len(self.attributed(a)) for a in Attribution},
            "findings": [f.to_dict() for f in self.findings],
            "skipped": self.skipped,
        }

    def to_json(self, indent: int = 2) -> str:
        return json.dumps(self.to_dict(), indent=indent, sort_keys=False)
