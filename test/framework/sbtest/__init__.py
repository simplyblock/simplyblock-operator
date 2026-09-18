"""sbtest — a component-and-detector framework for large-scale simplyblock test runs.

Two extension points: **components** do the side-effecting work of a run (drive a workload,
sample paths, follow logs) and can each be enabled or disabled independently; **detectors**
judge the evidence a run produced and can be added without touching collection.

Importing this package registers the bundled components and detectors, so
`known_detectors()` and `known_components()` are populated after `import sbtest`.
"""

from . import components as _components  # noqa: F401  (registers via decorators)
from . import detectors as _detectors  # noqa: F401
from .core import *  # noqa: F401,F403
from .core import __all__ as _core_all

__all__ = list(_core_all)
__version__ = "0.1.0"
