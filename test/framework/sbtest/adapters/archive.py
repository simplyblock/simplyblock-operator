"""ArchiveEvidence — read a finished run directory as Evidence.

This is what makes a check testable against the run that motivated it. The layout it reads
is the one `operator/test/fio_migration_test.py` writes, so every archived fio-mig-* run
becomes a fixture for the whole detector set:

    <rundir>/
      state.json                     run bookkeeping (migrations, pods, pv/nqn maps)
      test.log                       event log; the fallback when state.json is absent
      ana/<run>-mig-N.csv            host ANA samples per migration
      <run>-fio-N/result.json        fio's own summary
      <run>-fio-N/fio.log            the pod's container log (verify failures live here)
      <run>-fio-N/timeseries.csv     per-second IOPS
      spdk-<port>[-proxy].txt        host-sourced container logs
      operator.txt / webappapi.txt   likewise
      dmesg-<vm>.txt                 kernel ring buffer per storage worker
      nvme-controllers.json          fabric snapshot, when the run collected one

Nothing here is required. A missing file makes the corresponding accessor return nothing,
which makes the detectors that need it report themselves skipped rather than clean — the
distinction the whole findings model is built around.
"""

from __future__ import annotations

import csv
import glob
import json
import os
import re
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta

from ..core import (
    AnaSample,
    ControlEvent,
    FioJob,
    IopsSample,
    LogSpan,
    Migration,
    NvmeController,
)


def _dt(v: object) -> datetime | None:
    if not v:
        return None
    if isinstance(v, datetime):
        return v if v.tzinfo else v.replace(tzinfo=UTC)
    try:
        d = datetime.fromisoformat(str(v).replace("Z", "+00:00"))
    except ValueError:
        return None
    return d if d.tzinfo else d.replace(tzinfo=UTC)


_TS_PATTERNS = (
    # CRI container log: 2026-08-19T22:23:18.994807954Z stderr F <msg>
    (re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"), "%Y-%m-%dT%H:%M:%S"),
    # dmesg -T: [Thu Aug 20 05:46:57 2026]
    (re.compile(r"^\[(\w{3} \w{3}\s+\d+ \d{2}:\d{2}:\d{2} \d{4})\]"),
     "%a %b %d %H:%M:%S %Y"),
)


def _log_line_ts(raw: str) -> datetime | None:
    """The timestamp of a collected log line, whichever of the known formats it uses."""
    for rx, fmt in _TS_PATTERNS:
        m = rx.match(raw)
        if m:
            try:
                return datetime.strptime(m.group(1), fmt).replace(tzinfo=UTC)
            except ValueError:
                return None
    return None


class ArchiveEvidence:
    """Evidence backed by a finished run directory."""

    def __init__(self, outdir: str) -> None:
        self.outdir = os.path.abspath(outdir)
        if not os.path.isdir(self.outdir):
            raise FileNotFoundError(self.outdir)
        self._state = self._load_state()
        self.run_id = self._state.get("run_id") or os.path.basename(self.outdir).replace(
            "fio-mig-", "fiomig-")
        self._ana_cache: dict[str, list[AnaSample]] = {}
        self._fio_start_cache: dict[str, datetime | None] = {}

    # ── bookkeeping ────────────────────────────────────────────────────────────────

    def _load_state(self) -> dict:
        p = os.path.join(self.outdir, "state.json")
        if os.path.exists(p):
            try:
                with open(p) as fh:
                    loaded = json.load(fh)
                if isinstance(loaded, dict):
                    return loaded
            except (OSError, json.JSONDecodeError):
                pass
        return self._state_from_test_log()

    def _state_from_test_log(self) -> dict:
        """Rebuild just enough from test.log when state.json is missing.

        Only the migration timeline is recovered, because that is what attribution needs and
        it is the one thing the event log records unambiguously.
        """
        p = os.path.join(self.outdir, "test.log")
        if not os.path.exists(p):
            return {}
        migs: dict[str, dict] = {}
        start_re = re.compile(
            r"^(\S+) .*MIGRATION START\s+(\S+)\s+.*?(?:pod=(\S+))?\s*(?:pv=(\S+))?")
        stop_re = re.compile(r"^(\S+) .*MIGRATION STOP\s+(\S+)\s+phase=(\S+)")
        with open(p, errors="ignore") as fh:
            for line in fh:
                m = start_re.match(line)
                if m and "MIGRATION START" in line:
                    name = m.group(2)
                    rec = migs.setdefault(name, {"name": name})
                    rec["start"] = m.group(1)
                    for key, pat in (("pod", r"pod=(\S+)"), ("pv", r"pv=(\S+)"),
                                     ("source", r"source=(\S+)"), ("target", r"target=(\S+)")):
                        mm = re.search(pat, line)
                        if mm:
                            rec[key] = mm.group(1)
                    mm = re.search(r"moves along: ([^)]*)\)", line)
                    rec["group_pvs"] = ([x.strip() for x in mm.group(1).split(",")] + [rec.get("pv", "")]
                                        if mm else [rec.get("pv", "")])
                    continue
                m = stop_re.match(line)
                if m:
                    rec = migs.setdefault(m.group(2), {"name": m.group(2)})
                    rec["end"] = m.group(1)
                    rec["phase"] = m.group(3)
                    mm = re.search(r"error='([^']*)'", line)
                    if mm:
                        rec["error"] = mm.group(1)
        return {"migrations": list(migs.values())}

    # ── Evidence protocol ──────────────────────────────────────────────────────────

    def migrations(self) -> list[Migration]:
        # The framework's own driver writes migrations.json; the older harness writes them
        # inside state.json. Prefer whichever is present, so one reader serves both.
        p = os.path.join(self.outdir, "migrations.json")
        if os.path.exists(p) and not self._state.get("migrations"):
            from ..components.migration import migrations_from_file
            try:
                return migrations_from_file(p)
            except (OSError, json.JSONDecodeError, ValueError):
                pass
        out = []
        for m in self._state.get("migrations", []):
            start = _dt(m.get("start"))
            if not start:
                continue
            cut = {}
            for node, ts in (m.get("ana_cutover") or {}).items():
                d = _dt(ts)
                if d:
                    cut[node] = d
            out.append(Migration(
                name=m.get("name", ""), start=start, end=_dt(m.get("end")),
                phase=m.get("phase", ""), source=m.get("source", ""),
                target=m.get("target", ""), pv=m.get("pv", ""), pod=m.get("pod", ""),
                members=[x for x in (m.get("group_pvs") or []) if x],
                error=m.get("error", "") or "", cutover=cut))
        out.sort(key=lambda x: x.start)
        return out

    def ana_samples(self, migration: str) -> list[AnaSample]:
        if migration in self._ana_cache:
            return self._ana_cache[migration]
        # state.json records an absolute path from the original host, which may not exist
        # here; resolve by name inside this directory instead.
        cands = [os.path.join(self.outdir, "ana", f"{migration}.csv")]
        cands += glob.glob(os.path.join(self.outdir, "ana", f"*{migration}.csv"))
        path = next((c for c in cands if os.path.exists(c)), None)
        samples: list[AnaSample] = []
        if path:
            grouped: dict[tuple, AnaSample] = {}
            try:
                with open(path, newline="") as fh:
                    for r in csv.DictReader(fh):
                        ts = _dt(r.get("ts"))
                        if not ts:
                            continue
                        key = (ts, r.get("node", ""), r.get("address", ""))
                        s = grouped.get(key)
                        if s is None:
                            s = AnaSample(ts=ts, node=r.get("node", ""),
                                          address=r.get("address", ""),
                                          state=r.get("ctrl_state", ""),
                                          ana={}, phase=r.get("phase", ""),
                                          role=r.get("role", ""))
                            grouped[key] = s
                        if r.get("nsid"):
                            s.ana[int(r["nsid"])] = r.get("ana_state", "")
            except OSError:
                pass
            samples = [grouped[k] for k in sorted(grouped)]
        self._ana_cache[migration] = samples
        return samples

    def pods(self) -> list[str]:
        pods = self._state.get("pods")
        if pods:
            return list(pods)
        return sorted(
            d for d in os.listdir(self.outdir)
            if os.path.isdir(os.path.join(self.outdir, d)) and "-fio-" in d)

    def _result_json(self, pod: str) -> dict:
        p = os.path.join(self.outdir, pod, "result.json")
        if not os.path.exists(p):
            return {}
        try:
            with open(p) as fh:
                loaded = json.load(fh)
        except (OSError, json.JSONDecodeError):
            return {}
        return loaded if isinstance(loaded, dict) else {}

    def _fio_start(self, pod: str) -> datetime | None:
        """The wall clock of second 0 of this pod's fio time series.

        Taken from fio's own `job_start` (epoch milliseconds) rather than from the
        wall_clock column, because the column is only as right as whatever wrote it: runs
        archived before the base was fixed derived it from a shared run-start stamp taken
        minutes before each pod's fio actually began. fio's offsets are milliseconds since
        *that job* started, so the job's own start is the only base that is right by
        construction, and it is right for old archives too — which is the point, since a
        detector that cannot be re-run against the run that motivated it is a detector
        nobody trusts.

        Falls back to the CSV's own column when result.json is missing or carries no
        job_start.
        """
        if pod not in self._fio_start_cache:
            jobs = self._result_json(pod).get("jobs") or []
            start_ms = jobs[0].get("job_start") if jobs else None
            self._fio_start_cache[pod] = (
                datetime.fromtimestamp(start_ms / 1000.0, tz=UTC)
                if isinstance(start_ms, int | float) and start_ms > 0 else None)
        return self._fio_start_cache[pod]

    def fio_jobs(self) -> list[FioJob]:
        out = []
        for pod in self.pods():
            res = self._result_json(pod)
            if not res:
                continue
            for job in res.get("jobs", []):
                rd, wr = job.get("read", {}), job.get("write", {})
                out.append(FioJob(
                    pod=pod, error=int(job.get("error", 0) or 0),
                    read_iops=float(rd.get("iops", 0) or 0),
                    write_iops=float(wr.get("iops", 0) or 0),
                    total_iops=float(rd.get("iops", 0) or 0) + float(wr.get("iops", 0) or 0)))
        return out

    def fio_timeseries(self, pod: str) -> list[IopsSample]:
        p = os.path.join(self.outdir, pod, "timeseries.csv")
        if not os.path.exists(p):
            return []
        out = []
        # The offset column is `second` in the harness's own CSVs and `t`/`offset_s` in
        # fio's raw logs. Reading only the latter silently placed every sample at offset 0,
        # which does not look like a parse failure — the IOPS column still parsed, so the
        # series was the right length with the whole run collapsed onto one instant. Anything
        # that locates an outage in time was reading a flatline.
        def _first(r: dict[str, str], *keys: str) -> str:
            for k in keys:
                v = r.get(k)
                if v not in (None, ""):
                    return v
            return ""

        base = self._fio_start(pod)
        try:
            with open(p, newline="") as fh:
                for r in csv.DictReader(fh):
                    try:
                        off = int(float(_first(r, "second", "t", "offset_s") or 0))
                        tot = float(_first(r, "total_iops") or 0)
                    except (TypeError, ValueError):
                        continue
                    wall = (base + timedelta(seconds=off) if base
                            else _dt(_first(r, "wall", "wall_clock")))
                    out.append(IopsSample(offset_s=off, wall=wall, total_iops=tot))
        except OSError:
            return []
        out.sort(key=lambda s: s.offset_s)
        return out

    def fio_log(self, pod: str) -> Iterator[str]:
        p = os.path.join(self.outdir, pod, "fio.log")
        if not os.path.exists(p):
            return iter(())
        return self._lines(p)

    def container_logs(self) -> list[str]:
        names = []
        for p in sorted(glob.glob(os.path.join(self.outdir, "*.txt"))):
            base = os.path.basename(p)[:-4]
            if base in ("test", "REVIEW"):
                continue
            names.append(base)
        return names

    def container_log(self, name: str) -> Iterator[str]:
        p = os.path.join(self.outdir, f"{name}.txt")
        if not os.path.exists(p):
            return iter(())
        return self._lines(p)

    def run_window(self) -> tuple[datetime | None, datetime | None]:
        """From the event log's first and last stamps, falling back to the migrations.

        test.log is preferred because it brackets the whole run including setup and
        collection, while the migrations only cover the part that was migrating.
        """
        # run.json is authoritative when present: the run wrote it, rather than it being
        # inferred from whatever happened to get logged.
        p = os.path.join(self.outdir, "run.json")
        if os.path.exists(p):
            try:
                with open(p) as fh:
                    rec = json.load(fh)
                start, end = _dt(rec.get("start")), _dt(rec.get("end"))
                if start:
                    return start, end
            except (OSError, json.JSONDecodeError):
                pass

        p = os.path.join(self.outdir, "test.log")
        if os.path.exists(p):
            first = last = None
            try:
                with open(p, errors="ignore") as fh:
                    for line in fh:
                        ts = _dt(line[:20].strip())
                        if ts:
                            first = first or ts
                            last = ts
            except OSError:
                pass
            if first:
                return first, last
        migs = self.migrations()
        if migs:
            ends = [m.end for m in migs if m.end]
            return migs[0].start, (max(ends) if ends else None)
        return None, None

    def control_events(self) -> list[ControlEvent]:
        p = os.path.join(self.outdir, "cluster-events.json")
        if not os.path.exists(p):
            return []
        try:
            with open(p) as fh:
                raw = json.load(fh)
        except (OSError, json.JSONDecodeError):
            return []
        out = []
        for e in raw if isinstance(raw, list) else []:
            ts = _dt(str(e.get("Date", "")).replace(" ", "T"))
            if not ts:
                continue
            out.append(ControlEvent(
                ts=ts, level=str(e.get("Level", "")), kind=str(e.get("Event", "")),
                message=str(e.get("Message", "")),
                subject=str(e.get("NodeId") or e.get("Storage_ID") or "")))
        out.sort(key=lambda x: x.ts)
        return out

    def log_spans(self) -> list[LogSpan]:
        """First and last timestamp of every collected log, however it stamps its lines.

        Three formats appear in one artifact directory — CRI (`2026-...Z stderr F`), dmesg
        ctime (`[Thu Aug 20 ...]`) and dmesg ISO — so each is tried per line until one sticks.

        The bounds are the **minimum and maximum** timestamp, not the first and last line's.
        That distinction is not pedantry: an artifact holding several containers is written one
        container after another, so it is not in time order, and reading the last line gives
        whatever the last *container* happened to say. Doing that made a 52 MiB log covering
        two hours look like thirteen seconds.
        """
        spans = []
        for name in self.container_logs():
            lo = hi = None
            lines = 0
            for raw in self.container_log(name):
                lines += 1
                ts = _log_line_ts(raw)
                if ts is None:
                    continue
                if lo is None or ts < lo:
                    lo = ts
                if hi is None or ts > hi:
                    hi = ts
            spans.append(LogSpan(name=name, first=lo, last=hi, lines=lines))
        return spans

    def cluster_uuid(self) -> str:
        """The run's cluster, from the NQNs it recorded, else from the event log."""
        for nqn in (self._state.get("nqn_of") or {}).values():
            m = re.search(r"simplyblock:([0-9a-f-]{36}):", str(nqn))
            if m:
                return m.group(1)
        p = os.path.join(self.outdir, "test.log")
        if os.path.exists(p):
            try:
                with open(p, errors="ignore") as fh:
                    for line in fh:
                        m = re.search(r"live cluster.*=\s*([0-9a-f-]{36})", line)
                        if m:
                            return m.group(1)
            except OSError:
                pass
        return ""

    def nvme_controllers_pre(self) -> list[NvmeController]:
        """The fabric as it was *before* the run — the state the run inherited.

        Optional, and not part of the Evidence protocol: only nvme.dirty-start wants it, and
        a detector that needs it can ask with getattr and skip when it is absent.
        """
        return self._controllers_from("nvme-controllers-pre.json")

    def nvme_controllers(self) -> list[NvmeController]:
        return self._controllers_from("nvme-controllers-post.json") or \
            self._controllers_from("nvme-controllers.json")

    def _controllers_from(self, filename: str) -> list[NvmeController]:
        p = os.path.join(self.outdir, filename)
        if not os.path.exists(p):
            return []
        try:
            with open(p) as fh:
                raw = json.load(fh)
        except (OSError, json.JSONDecodeError):
            return []
        out = []
        for c in raw if isinstance(raw, list) else raw.get("controllers", []):
            out.append(NvmeController(
                node=c.get("node", ""), name=c.get("name", ""), nqn=c.get("nqn", ""),
                address=c.get("address", ""), state=c.get("state", ""),
                namespaces={int(k): v for k, v in (c.get("namespaces") or {}).items()},
                ctrl_loss_tmo=c.get("ctrl_loss_tmo")))
        return out

    # ── helpers ────────────────────────────────────────────────────────────────────

    @staticmethod
    def _lines(path: str) -> Iterator[str]:
        """Stream a log line by line. These files reach tens of megabytes."""
        with open(path, errors="ignore") as fh:
            yield from fh
