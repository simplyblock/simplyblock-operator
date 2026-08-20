"""RunContext — the shared state a component is handed, and the run's event timeline.

Components get exactly one object, so adding a component never changes a signature
elsewhere. The timeline is the part worth explaining: components record events on it, and
detectors read those events back as evidence. That is how a sampler's observation reaches a
check without the two knowing about each other.
"""

from __future__ import annotations

import json
import os
import sys
import threading
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any


def now_utc() -> datetime:
    return datetime.now(UTC)


def iso(ts: datetime) -> str:
    return ts.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


class Logger:
    """Timestamped console log, mirrored to a file in the artifact directory.

    A file mirror rather than console-only because the whole framework exists to make runs
    re-readable afterwards, and a run whose log lives only in someone's terminal scrollback
    is a run that cannot be reviewed.
    """

    LEVELS = ("DEBUG", "INFO", "EVENT", "WARN", "ERROR", "CRITICAL")

    def __init__(self, path: str | None = None, verbose: bool = False) -> None:
        self._fh = open(path, "a", buffering=1) if path else None  # noqa: SIM115
        self._verbose = verbose
        self._lock = threading.Lock()

    def _emit(self, level: str, msg: str) -> None:
        if level == "DEBUG" and not self._verbose:
            return
        line = f"{iso(now_utc())} [{level:8}] {msg}"
        with self._lock:
            stream = sys.stderr if level in ("ERROR", "CRITICAL") else sys.stdout
            print(line, file=stream, flush=True)
            if self._fh:
                self._fh.write(line + "\n")

    def debug(self, m: str) -> None: self._emit("DEBUG", m)
    def info(self, m: str) -> None: self._emit("INFO", m)
    def event(self, m: str) -> None: self._emit("EVENT", m)
    def warn(self, m: str) -> None: self._emit("WARN", m)
    def error(self, m: str) -> None: self._emit("ERROR", m)
    def crit(self, m: str) -> None: self._emit("CRITICAL", m)

    def close(self) -> None:
        if self._fh:
            self._fh.close()
            self._fh = None


@dataclass
class Event:
    """Something a component observed, timestamped and typed.

    `kind` is what detectors filter on and should be a stable dotted name
    ("migration.start", "node.offline"), because a detector keying off a message string
    breaks the first time someone improves the wording.
    """

    ts: datetime
    kind: str
    subject: str = ""
    data: dict[str, Any] = field(default_factory=dict)


class Timeline:
    """Thread-safe ordered record of what happened during a run."""

    def __init__(self) -> None:
        self._events: list[Event] = []
        self._lock = threading.Lock()

    def record(self, kind: str, subject: str = "", **data: Any) -> Event:
        ev = Event(ts=now_utc(), kind=kind, subject=subject, data=data)
        with self._lock:
            self._events.append(ev)
        return ev

    def of_kind(self, *kinds: str) -> list[Event]:
        with self._lock:
            return [e for e in self._events if e.kind in kinds]

    def all(self) -> list[Event]:
        with self._lock:
            return sorted(self._events, key=lambda e: e.ts)

    def to_list(self) -> list[dict]:
        return [{"ts": iso(e.ts), "kind": e.kind, "subject": e.subject, "data": e.data}
                for e in self.all()]


@dataclass
class RunContext:
    """Everything a component may need, and the only thing it is given."""

    run_id: str
    outdir: str
    log: Logger
    timeline: Timeline = field(default_factory=Timeline)
    #: Free-form scratch space shared between components, keyed by component name. Used for
    #: the handful of genuine dependencies between them (a workload publishing the pods it
    #: created, so a sampler knows which nodes to watch) — always read defensively, since
    #: the producing component may be disabled.
    shared: dict[str, Any] = field(default_factory=dict)
    #: Set when the run is being torn down, so a component's background loop can exit.
    stopping: threading.Event = field(default_factory=threading.Event)

    def path(self, *parts: str) -> str:
        """A path inside the artifact directory, with parent directories created."""
        p = os.path.join(self.outdir, *parts)
        os.makedirs(os.path.dirname(p) or self.outdir, exist_ok=True)
        return p

    def dir(self, *parts: str) -> str:
        """A *directory* inside the artifact directory, created. Unlike `path`, which creates
        the parent of the thing you name, this creates the thing you name."""
        p = os.path.join(self.outdir, *parts)
        os.makedirs(p, exist_ok=True)
        return p

    def window(self) -> tuple[datetime | None, datetime | None]:
        """The run's start and end as currently known — end is None while it is still running.

        Components need the start to place a relative offset (fio counts seconds from its own
        launch) on the wall clock, which is the only way an observation from one component can
        be lined up against another's.
        """
        return getattr(self, "_window_start", None), getattr(self, "_window_end", None)

    def mark_window(self, start: datetime | None = None, end: datetime | None = None) -> None:
        """Record when the run began and ended, into run.json.

        Without this nothing in the artifact directory says when the run was, so every
        detector that reads a ring buffer has to mark what it finds Attribution.UNKNOWN — and
        UNKNOWN counts against the run. A live collect therefore failed on twenty-nine
        filesystem shutdowns that had happened hours before it started.
        """
        self._window_start = getattr(self, "_window_start", None) or start or now_utc()
        if end:
            self._window_end = end
        self.save_json("run.json", {
            "run_id": self.run_id,
            "start": iso(self._window_start),
            "end": iso(end) if end else None,
        })

    def save_json(self, name: str, obj: Any) -> str:
        p = self.path(name)
        with open(p, "w") as fh:
            json.dump(obj, fh, indent=2, default=str)
        return p
