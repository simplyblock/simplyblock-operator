"""Suite configuration — which components run, which detectors judge, and with what.

The shape is deliberately flat and explicit:

    run:
      id_prefix: fiomig
      outdir: ./runs
    components:
      logs.stream:   {enabled: true, containers: [spdk-container, spdk-proxy-container]}
      logs.collect:  {enabled: true}
      ana.sample:    {enabled: true, interval_s: 2.0}
    detectors:
      ana.freeze-count: {enabled: true, max_freezes: 1}
      fio.checksum:     {enabled: true, verify_lag_s: 45}

Two rules make this predictable. A name absent from the config is **off** for components
and **on** for detectors — collecting is a cost you opt into, judging is not something you
should have to remember to switch on. And an unknown option is an error rather than a
silently ignored key, because a threshold that looks set but is not is worse than one that
is obviously missing.

Suites are YAML. A suite is a document to be read and argued with — every threshold in one
wants a sentence saying why it is that number, and the numbers here were each paid for by a
run that went wrong. JSON cannot carry that sentence, so the format was the wrong one. A
suite written as `.json` still loads, for anything generating them programmatically.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from typing import Any

import yaml

DEFAULT_DETECTORS_ON = True
DEFAULT_COMPONENTS_ON = False


@dataclass
class Selection:
    """A resolved name -> options mapping for one extension point."""

    enabled: dict[str, dict[str, Any]] = field(default_factory=dict)
    disabled: list[str] = field(default_factory=list)

    def __bool__(self) -> bool:
        return bool(self.enabled)


@dataclass
class Config:
    run: dict[str, Any] = field(default_factory=dict)
    components: Selection = field(default_factory=Selection)
    detectors: Selection = field(default_factory=Selection)
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def run_id_prefix(self) -> str:
        return str(self.run.get("id_prefix", "sbtest"))

    @property
    def outdir(self) -> str:
        return str(self.run.get("outdir", "."))


def _resolve(section: dict[str, Any], known: list[str], default_on: bool) -> Selection:
    sel = Selection()
    section = section or {}

    unknown = set(section) - set(known)
    if unknown:
        raise ValueError(f"unknown name(s) in config: {sorted(unknown)}; known: {sorted(known)}")

    for name in known:
        spec = section.get(name)
        if spec is None:
            spec = default_on
        if isinstance(spec, bool):
            if spec:
                sel.enabled.setdefault(name, {})
            else:
                sel.disabled.append(name)
            continue
        if not isinstance(spec, dict):
            raise ValueError(f"config for {name!r} must be a bool or a mapping, got {type(spec).__name__}")
        opts = dict(spec)
        if not opts.pop("enabled", True):
            sel.disabled.append(name)
            continue
        sel.enabled[name] = opts
    return sel


def load(path: str | None, known_components: list[str], known_detectors: list[str],
         overrides: dict[str, Any] | None = None) -> Config:
    """Read a suite file (or take defaults) and resolve it against what is registered.

    Resolving against the registry here rather than at use time means a typo in a name
    fails at startup, with the list of valid names, instead of quietly running a suite that
    is missing the check it was written for.
    """
    raw: dict[str, Any] = {}
    if path:
        with open(path) as fh:
            text = fh.read()
        try:
            # JSON keeps its own parser rather than riding YAML's superset handling, which
            # differs on duplicate keys and tabs — a generated suite should fail on those
            # rather than be quietly reinterpreted.
            if path.endswith(".json"):
                raw = json.loads(text) if text.strip() else {}
            else:
                raw = yaml.safe_load(text) or {}
        except (yaml.YAMLError, json.JSONDecodeError) as e:
            raise ValueError(f"cannot parse suite {path}: {e}") from e
        if not isinstance(raw, dict):
            raise ValueError(f"suite {path} must be a mapping at the top level, got "
                             f"{type(raw).__name__}")
    for k, v in (overrides or {}).items():
        raw.setdefault(k, v) if not isinstance(v, dict) else raw.setdefault(k, {}).update(v)

    unknown_top = set(raw) - {"run", "components", "detectors", "description"}
    if unknown_top:
        raise ValueError(f"unknown top-level key(s) in {path}: {sorted(unknown_top)}")

    return Config(
        run=raw.get("run", {}) or {},
        components=_resolve(raw.get("components", {}), known_components, DEFAULT_COMPONENTS_ON),
        detectors=_resolve(raw.get("detectors", {}), known_detectors, DEFAULT_DETECTORS_ON),
        raw=raw,
    )


def apply_cli_toggles(sel: Selection, enable: list[str], disable: list[str]) -> Selection:
    """Apply `--enable x --disable y` on top of a resolved selection.

    Disable wins over enable when both name the same thing: a run someone explicitly narrowed
    should not be widened by a default in a suite file they did not write.
    """
    for name in enable:
        if name in sel.disabled:
            sel.disabled.remove(name)
        sel.enabled.setdefault(name, {})
    for name in disable:
        sel.enabled.pop(name, None)
        if name not in sel.disabled:
            sel.disabled.append(name)
    return sel


def suite_path(name: str) -> str | None:
    """Resolve a bare suite name against the bundled suites directory."""
    if os.path.sep in name or name.endswith((".yaml", ".yml", ".json")):
        return name if os.path.exists(name) else None
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    for ext in (".yaml", ".yml", ".json"):
        p = os.path.join(here, "suites", name + ext)
        if os.path.exists(p):
            return p
    return None
