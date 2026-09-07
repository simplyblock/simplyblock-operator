"""Detectors, components, and the registry that makes both selectable by name.

Two extension points, deliberately different shapes:

* A **Detector** is a pure judgement: Evidence in, findings out. No cluster access, no
  ordering constraints, no state between runs. That is what makes them cheap to write, to
  unit-test against a handful of synthetic samples, and to re-run against an archive.

* A **Component** does the side-effecting work — starting pods, sampling, following logs,
  driving a workload. It has a lifecycle, it can fail, and it may be enabled or disabled
  independently of everything else.

The split is the point. Every check worth having came from an incident, and none of them
should require touching collection code to add. Conversely, turning streaming log
collection on or off should not disturb a single check.
"""

from __future__ import annotations

from collections.abc import Callable, Iterable
from typing import TYPE_CHECKING, Any

from .findings import Finding

if TYPE_CHECKING:  # pragma: no cover
    from .context import RunContext
    from .evidence import Evidence


class SkipDetector(Exception):
    """Raised by a detector whose evidence is absent, so the report can say so.

    The alternative — returning no findings — makes "could not check" and "checked, all
    clean" the same output, which is how a broken check passes a broken run.
    """


class Detector:
    """Base class for a defect detector.

    Subclasses set `name` and implement `detect`. Options arrive through `configure` from
    the config file, so thresholds are never hard-coded at the call site — a detector's
    default should encode what is known (see the ANA detectors, whose defaults come from
    measured runs) while staying overridable per suite.

    Deliberately not a dataclass: a generated __init__ would assign the base class's empty
    default over every subclass's `name`, so every finding and every skip would be reported
    against an anonymous detector.
    """

    #: Dotted, stable — findings and config both key off it.
    name: str = ""
    #: One line, shown by `sbtest detectors`. Say what it catches, not how.
    summary: str = ""

    def __init__(self) -> None:
        self.options: dict[str, Any] = dict(self.defaults())

    def configure(self, **options: Any) -> Detector:
        unknown = set(options) - set(self.defaults())
        if unknown:
            raise ValueError(
                f"detector {self.name}: unknown option(s) {sorted(unknown)}; "
                f"known: {sorted(self.defaults())}")
        self.options = {**self.defaults(), **options}
        return self

    def defaults(self) -> dict[str, Any]:
        """Option names and their defaults. Also the allow-list for `configure`."""
        return {}

    def opt(self, key: str) -> Any:
        return self.options.get(key, self.defaults().get(key))

    def detect(self, ev: Evidence) -> Iterable[Finding]:  # pragma: no cover
        raise NotImplementedError


class Component:
    """Base class for a lifecycle unit: collection, sampling, workload, driver.

    Every hook is optional; override only what applies. The runner calls them in this
    order, and guarantees `stop`/`teardown` run for any component whose `setup` was
    entered, so a component that starts pods is responsible for removing them and will be
    given the chance to.

        setup()      once, before anything runs. Resolve targets, create helpers.
        start()      begin doing the thing (sampling, following, driving).
        tick()       called periodically by the runner; cheap, must not block long.
        stop()       stop doing the thing. Data already written stays written.
        collect()    gather artifacts into the run directory. After stop, before detect.
        teardown()   release cluster resources. Runs even when the run failed.

    `collect` is separate from `stop` because the two differ for exactly the case that
    motivated this framework: a streaming collector has nothing to collect at the end (it
    has been writing all along) while a post-run collector does all its work there, and the
    runner has to be able to run one, the other, or both without either knowing.
    """

    name: str = ""
    #: One line, shown by `sbtest components`.
    summary: str = ""
    #: Whether a failure in `setup` should abort the run.
    #:
    #: False for collectors: a run that loses one evidence stream is still a run, and the
    #: detectors that needed it will report themselves skipped. True for a component that
    #: *is* the run — a workload or a migration driver — because continuing without it
    #: produces a green result for a test that never happened, which is the worst outcome
    #: available.
    required: bool = False

    def __init__(self, **options: Any) -> None:
        self.options = {**self.defaults(), **options}
        unknown = set(options) - set(self.defaults())
        if unknown:
            raise ValueError(
                f"component {self.name}: unknown option(s) {sorted(unknown)}; "
                f"known: {sorted(self.defaults())}")

    def defaults(self) -> dict[str, Any]:
        return {}

    def opt(self, key: str) -> Any:
        return self.options.get(key)

    # -- lifecycle, all optional ----------------------------------------------------
    def setup(self, ctx: RunContext) -> None: ...
    def start(self, ctx: RunContext) -> None: ...
    def tick(self, ctx: RunContext) -> None: ...
    def stop(self, ctx: RunContext) -> None: ...
    def collect(self, ctx: RunContext) -> None: ...
    def teardown(self, ctx: RunContext) -> None: ...


# ── registry ────────────────────────────────────────────────────────────────────────

_DETECTORS: dict[str, Callable[[], Detector]] = {}
_COMPONENTS: dict[str, Callable[..., Component]] = {}


def detector(cls: type[Detector]) -> type[Detector]:
    """Register a detector class under its `name`."""
    if not cls.name:
        raise ValueError(f"{cls.__qualname__} must set `name`")
    if cls.name in _DETECTORS:
        raise ValueError(f"duplicate detector name {cls.name!r}")
    _DETECTORS[cls.name] = cls
    return cls


def component(cls: type[Component]) -> type[Component]:
    """Register a component class under its `name`."""
    if not cls.name:
        raise ValueError(f"{cls.__qualname__} must set `name`")
    if cls.name in _COMPONENTS:
        raise ValueError(f"duplicate component name {cls.name!r}")
    _COMPONENTS[cls.name] = cls
    return cls


def known_detectors() -> dict[str, type[Detector]]:
    return dict(sorted(_DETECTORS.items()))  # type: ignore[arg-type]


def known_components() -> dict[str, type[Component]]:
    return dict(sorted(_COMPONENTS.items()))  # type: ignore[arg-type]


def build_detector(name: str, **options: Any) -> Detector:
    try:
        cls = _DETECTORS[name]
    except KeyError:
        raise KeyError(f"unknown detector {name!r}; known: {sorted(_DETECTORS)}") from None
    return cls().configure(**options)


def build_component(name: str, **options: Any) -> Component:
    try:
        cls = _COMPONENTS[name]
    except KeyError:
        raise KeyError(f"unknown component {name!r}; known: {sorted(_COMPONENTS)}") from None
    return cls(**options)
