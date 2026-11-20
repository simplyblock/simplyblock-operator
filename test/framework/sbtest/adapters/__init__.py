"""Evidence adapters — where a detector's input comes from.

`ArchiveEvidence` reads a finished run directory, which is what lets the detector set be
re-run against any past run. A live run uses `LiveEvidence`, assembled by the components
that collected it.
"""

from .archive import ArchiveEvidence
from .live import LiveEvidence

__all__ = ["ArchiveEvidence", "LiveEvidence"]
