"""Turn an archived run directory into a regression fixture for tests/test_corpus.py.

A run directory holds tens of megabytes, most of it container logs no detector in the pNFS
suite reads. This keeps only what the detectors judge (the run window, the pNFS volume
map, the NVMe I/O samples and snapshots, each fio instance's evidence, and dmesg), trimmed
so the corpus stays small enough to live in the repository:

* Each fio summary (`result.json`) keeps only the fields `ArchiveEvidence` reads.
* Each `fio.log` drops fio's progress lines, which carry no timestamp and no verdict.
* Each dmesg keeps the lines within `--margin-s` of the run window, by the time the kernel
  rendered, plus every `sbtest-clock-probe` marker, which the kernel detectors need to
  correct the kernel's clock. The margin is wider than the clock skew measured on the lab
  nodes (up to three minutes), so no line the corrected window would take is dropped.

Standard library only, so it runs from any checkout:

    python3 tests/fixtures/trim_run.py runs/pnfs-1791461034 tests/fixtures/runs/pnfs-1791461034
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
from datetime import UTC, datetime, timedelta

#: Copied as they are when present.
WHOLE = ("run.json", "state.json", "test.log", "pnfs.json", "iostat.csv", "restarts.json",
         "nvme-controllers-pre.json", "nvme-controllers-post.json", "nvme-controllers.json")
#: Copied from each fio instance's directory as they are.
FIO_WHOLE = ("timeseries.csv", "nfs-ops.json", "fio.rc")
#: The per-job fields of fio's JSON summary that the archive and the detectors read.
JOB_FIELDS = ("jobname", "error", "job_start", "job_runtime")
DIR_FIELDS = ("iops", "io_bytes")

_ISO = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})")


def _window(src: str) -> tuple[datetime, datetime]:
    with open(os.path.join(src, "run.json")) as fh:
        run = json.load(fh)

    def parse(v: str) -> datetime:
        return datetime.fromisoformat(v.replace("Z", "+00:00")).astimezone(UTC)

    return parse(run["start"]), parse(run["end"])


def _slim_result(path: str, dest: str) -> None:
    with open(path) as fh:
        res = json.load(fh)
    jobs = []
    for job in res.get("jobs", []):
        slim = {k: job[k] for k in JOB_FIELDS if k in job}
        for d in ("read", "write"):
            slim[d] = {k: job.get(d, {}).get(k, 0) for k in DIR_FIELDS}
        jobs.append(slim)
    with open(dest, "w") as fh:
        json.dump({"jobs": jobs}, fh, indent=1)
        fh.write("\n")


def _trim_fio_log(path: str, dest: str) -> None:
    with open(path, errors="replace") as src, open(dest, "w") as out:
        for line in src:
            if line.lstrip().startswith("Jobs:"):
                continue
            out.write(line)


def _trim_dmesg(path: str, dest: str, start: datetime, end: datetime, margin: timedelta) -> None:
    keep_prev = False
    with open(path, errors="replace") as src, open(dest, "w") as out:
        for line in src:
            m = _ISO.match(line)
            if m:
                ts = datetime.fromisoformat(m.group(1)).replace(tzinfo=UTC)
                keep_prev = start - margin <= ts <= end + margin
            if keep_prev or "sbtest-clock-probe" in line:
                out.write(line)


def trim(src: str, dest: str, margin_s: float) -> None:
    start, end = _window(src)
    margin = timedelta(seconds=margin_s)
    os.makedirs(dest, exist_ok=True)
    for name in WHOLE:
        if os.path.exists(os.path.join(src, name)):
            shutil.copy(os.path.join(src, name), os.path.join(dest, name))
    for name in sorted(os.listdir(src)):
        path = os.path.join(src, name)
        if name.startswith("dmesg-") and name.endswith(".txt"):
            _trim_dmesg(path, os.path.join(dest, name), start, end, margin)
        elif os.path.isdir(path) and "-fio-" in name:
            out = os.path.join(dest, name)
            os.makedirs(out, exist_ok=True)
            for f in FIO_WHOLE:
                if os.path.exists(os.path.join(path, f)):
                    shutil.copy(os.path.join(path, f), os.path.join(out, f))
            if os.path.exists(os.path.join(path, "result.json")):
                _slim_result(os.path.join(path, "result.json"), os.path.join(out, "result.json"))
            if os.path.exists(os.path.join(path, "fio.log")):
                _trim_fio_log(os.path.join(path, "fio.log"), os.path.join(out, "fio.log"))


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("src", help="the archived run directory")
    p.add_argument("dest", help="the fixture directory to write")
    p.add_argument("--margin-s", type=float, default=600.0,
                   help="dmesg kept around the run window, in seconds (default 600)")
    args = p.parse_args()
    trim(args.src, args.dest, args.margin_s)


if __name__ == "__main__":
    main()
