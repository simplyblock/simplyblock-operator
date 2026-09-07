"""Security detectors over collected evidence.

One check for now, and it is the one that keeps being true: artifacts get attached to tickets,
copied into chat and committed to repositories, so a credential that reaches a log reaches all
of those. This is cheap to check and expensive to miss, and it is entirely independent of what
the run was testing.
"""

from __future__ import annotations

import fnmatch
import re
from collections.abc import Iterable

from ..core import Detector, Evidence, Finding, SkipDetector, critical, detector

#: Shapes worth refusing to ship. Each is deliberately narrow: a pattern that fires on ordinary
#: text gets disabled, and a disabled check finds nothing.
SECRET_PATTERNS: tuple[tuple[str, str], ...] = (
    ("private key block", r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    ("nvme dhchap secret", r"DHHC-1:[0-9]{2}:[A-Za-z0-9+/=]{20,}"),
    ("bearer token", r"[Bb]earer\s+[A-Za-z0-9._-]{24,}"),
    ("kubernetes sa token", r"eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\."),
    ("aws access key", r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b"),
    ("url with credentials", r"[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s:@]+@"),
    ("password assignment", r"(?i)\b(?:password|passwd|secret[_-]?key)\s*[=:]\s*['\"]?[^\s'\"]{8,}"),
)


@detector
class SecretExposure(Detector):
    """A credential-shaped string in a collected log.

    Reports the pattern, the log and the line number — never the match itself. A finding that
    quotes the secret has copied it into a second file, and findings.json travels at least as
    widely as the log did.
    """

    name = "security.secret-exposure"
    summary = "credential-shaped strings in collected logs (reports location, never the value)"

    def defaults(self) -> dict:
        return {"logs": ["*"], "extra_patterns": [], "max_reported": 12}

    def detect(self, ev: Evidence) -> Iterable[Finding]:
        names = [n for n in ev.container_logs()
                 if any(fnmatch.fnmatch(n, g) for g in self.opt("logs"))]
        if not names:
            raise SkipDetector("no collected logs to scan")

        patterns = [(label, re.compile(rx)) for label, rx in SECRET_PATTERNS]
        patterns += [(str(p.get("id", "custom")), re.compile(str(p["regex"])))
                     for p in (self.opt("extra_patterns") or [])]

        # label -> log -> [line numbers]
        hits: dict[str, dict[str, list[int]]] = {}
        for name in names:
            for lineno, line in enumerate(ev.container_log(name), start=1):
                for label, rx in patterns:
                    if rx.search(line):
                        hits.setdefault(label, {}).setdefault(name, []).append(lineno)
        if not hits:
            return

        cap = int(self.opt("max_reported"))
        for label, per_log in sorted(hits.items()):
            total = sum(len(v) for v in per_log.values())
            where = "; ".join(
                f"{log}:{','.join(str(n) for n in lines[:cap])}"
                f"{f' (+{len(lines) - cap} more)' if len(lines) > cap else ''}"
                for log, lines in sorted(per_log.items()))
            yield critical(
                self.name,
                title=f"{total} line(s) matching {label}",
                subject=label,
                detail=where,
                evidence={"pattern": label, "total": total,
                          "per_log": {k: len(v) for k, v in per_log.items()}},
                # The file, not the log's name: the run directory holds <name>.txt, and
                # a pointer a reader has to complete is not a pointer.
                artifacts=[f"{log}.txt" for log in sorted(per_log)],
                note="The value is deliberately not quoted here — findings.json travels at "
                     "least as widely as the log. Rotate whatever this is, then stop the "
                     "component logging it; artifacts get attached to tickets and committed.",
            )
