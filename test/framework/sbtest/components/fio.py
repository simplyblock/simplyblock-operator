"""fio as a building block: the command line, the container script, and the evidence.

Every workload component drives fio the same way and leaves the same evidence behind, so the
fio detectors judge any of them without knowing which one ran. What differs between workloads
is what they provision and how they lay pods out; that stays in the components. What is here
is one fio *instance* — one fio process in one container, with one data file — from its
command line to its artifacts.

Three rules hold for every instance, and each is here rather than in a component because a
workload that broke one would produce a run whose verdict means less than it says:

* **Verification only when it can be trusted.** fio's md5 verify races itself when two
  in-flight I/Os touch the same block, and reports corruption that never happened. With one
  job that is solvable (`--serialize_overlap`); across processes it is not, without
  `io_submit_mode=offload`. So more than one job turns verification *off*, loudly.
* **One writer per data file.** Verification also assumes nothing else writes the file's
  blocks. Two instances on one file report each other's writes as corruption, so an instance
  owns its file outright, even when the filesystem under it is shared.
* **Logs off the volume under test.** Collecting the evidence must not depend on the health of
  the thing the evidence is about, so fio's own output goes to an emptyDir.
"""

from __future__ import annotations

import json
import time
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

from ..core import RunContext
from . import kube

FIO_IMAGE = "alpine:3.20"

# The options every fio-driving workload exposes, with the defaults they share. A workload
# merges these into its own `defaults()` so the knobs are spelled the same everywhere.
FIO_DEFAULTS: dict[str, Any] = {
    "runtime_s": 3600,
    "iodepth": 8,
    "bs": "4k",
    "rwmixread": 70,
    "ioengine": "libaio",
    "verify_fatal": False,
    "image": FIO_IMAGE,
}


@dataclass(frozen=True)
class FioInstance:
    """One fio process: where it runs, what it writes, and where its evidence lands."""

    pod: str
    container: str
    filename: str   # the data file, on the volume under test
    logdir: str     # fio's own output, on an emptyDir in the pod
    evidence: str   # its directory in the run's artifacts; must contain "-fio-"


def fio_args(opts: Mapping[str, Any], *, filename: str, size_gb: int, logdir: str,
             numjobs: int = 1, direct: bool = True) -> list[str]:
    """The fio command line for one instance, from a workload's options."""
    args = [
        "fio", "--name=fiotest", f"--filename={filename}", f"--size={size_gb}G",
        f"--ioengine={opts['ioengine']}", f"--direct={1 if direct else 0}", "--rw=randrw",
        f"--rwmixread={opts['rwmixread']}", f"--bs={opts['bs']}",
        f"--iodepth={opts['iodepth']}", f"--numjobs={numjobs}",
        "--group_reporting", "--time_based", f"--runtime={opts['runtime_s']}",
        "--continue_on_error=all",   # record EIO, do not die on it
        "--percentile_list=50:95:99:99.9",
        f"--write_iops_log={logdir}/iops", f"--write_lat_log={logdir}/lat",
        f"--write_bw_log={logdir}/bw", "--log_avg_msec=1000",
        "--eta=always", "--eta-newline=30",
        f"--output={logdir}/result.json", "--output-format=json",
    ]
    if verifies(numjobs):
        args += [
            # An md5 header per block, re-verified continuously during the run rather than
            # only at the end, so a lost write surfaces close enough in time to attribute.
            "--verify=md5", "--verify_backlog=4096", "--verify_backlog_batch=4096",
            # Dump the mismatching block: a block holding its pre-write content is a lost
            # write, which is a different defect from a block holding garbage.
            "--verify_dump=1",
            # Not fatal by default, so EVERY corrupted block is reported instead of only the
            # first. With a known write rate the count bounds how wide the window of lost
            # writes was; fatal stops at the first and says nothing about size.
            f"--verify_fatal={1 if opts['verify_fatal'] else 0}",
        ]
        if int(opts["iodepth"]) > 1:
            args.append("--serialize_overlap=1")
    return args


def verifies(numjobs: int) -> bool:
    """Whether an instance with this many jobs can verify what it wrote."""
    return numjobs == 1


def unverified_warning(who: str, numjobs: int) -> str:
    return (f"{who}: data-integrity verification DISABLED: numjobs={numjobs} (>1 cannot "
            "serialize overlapping writes without io_submit_mode=offload, so verify would "
            "report corruption that never happened). This run measures I/O only, not "
            "integrity; use iodepth for concurrency instead")


def container_script(args: list[str], logdir: str) -> str:
    """The shell a fio container runs: install fio, run it, keep the logs readable."""
    return (
        "set -u\n"
        'echo "[pod] $(date -u +%FT%TZ) installing fio"\n'
        "apk add --no-cache fio >/dev/null 2>&1 || "
        '{ echo "[pod] apk add fio FAILED"; exit 90; }\n'
        f"mkdir -p {logdir}\n"
        'echo "[pod] $(date -u +%FT%TZ) starting fio"\n'
        + " ".join(args) + "\n"
        "rc=$?\n"
        f'echo "$rc" > {logdir}/fio.rc\n'
        'echo "[pod] $(date -u +%FT%TZ) fio exited rc=$rc"\n'
        # Stay alive after fio exits so the logs on the emptyDir can still be collected.
        "sleep 100000\n"
    )


def wait_running(ctx: RunContext, who: str, namespace: str, pods: list[str],
                 timeout_s: float) -> None:
    """Wait until every pod labelled with the run is Running, or say which are not."""
    deadline = time.time() + timeout_s
    phases: dict[str, str] = {}
    while time.time() < deadline and not ctx.stopping.is_set():
        cp = kube.run(["-n", namespace, "get", "pods", "-l", f"sbtest={ctx.run_id}",
                       "-o", "json"], check=False)
        phases = {}
        if cp.returncode == 0 and cp.stdout:
            for it in json.loads(cp.stdout).get("items", []):
                phases[it["metadata"]["name"]] = it.get("status", {}).get("phase", "?")
        bad = [f"{p}={ph}" for p, ph in phases.items() if ph in ("Failed", "Unknown")]
        if bad:
            raise RuntimeError(f"fio pod(s) failed during startup: {', '.join(bad)}")
        running = [p for p in pods if phases.get(p) == "Running"]
        if len(running) == len(pods):
            ctx.log.info(f"{who}: all {len(running)} fio pod(s) Running")
            return
        time.sleep(5)
    # Name the pods that did not make it and what they are stuck at: "6 of 6 did not start"
    # sends someone to kubectl for the one fact the message could have carried.
    stuck = ", ".join(f"{p}={phases.get(p, 'absent')}" for p in pods
                      if phases.get(p) != "Running")
    raise RuntimeError(
        f"only {sum(1 for p in pods if phases.get(p) == 'Running')} of {len(pods)} fio "
        f"pod(s) reached Running within {timeout_s:.0f}s: {stuck}")


def collect_instance(ctx: RunContext, namespace: str, inst: FioInstance,
                     migs: list) -> str:
    """Pull one instance's account out of its pod, into the layout the analyser reads.

    Read with `exec cat` rather than `kubectl cp`, which truncates large files without
    reporting an error: a silently short result.json reads as a clean run. Returns the
    instance's evidence directory.
    """
    d = ctx.dir(inst.evidence)
    for remote, local in ((f"{inst.logdir}/result.json", "result.json"),
                          (f"{inst.logdir}/fio.rc", "fio.rc")):
        out = kube.exec_sh(namespace, inst.pod, f"cat {remote} 2>/dev/null",
                           container=inst.container, timeout=120)
        if out:
            with open(f"{d}/{local}", "w") as fh:
                fh.write(out)
    log = kube.run(["-n", namespace, "logs", inst.pod, "-c", inst.container, "--tail=-1"],
                   check=False, timeout=300)
    if log.stdout:
        with open(f"{d}/fio.log", "w") as fh:
            fh.write(log.stdout)
    write_timeseries(ctx, namespace, inst.pod, d, migs, logdir=inst.logdir,
                     container=inst.container)
    return d


def write_timeseries(ctx: RunContext, namespace: str, pod: str, d: str, migs: list,
                     logdir: str = "/logs", container: str | None = None) -> None:
    """Per-second IOPS from fio's iops log, with the migration in flight that second.

    The migration column is the point: correlating a throughput dip with the migration that
    caused it is otherwise a manual join across two files, and the detectors need it to
    attribute an outage to a specific migration rather than to the run.

    Column names match the older harness's CSVs (`second`, `wall_clock`) so a single reader
    serves both.
    """
    raw = kube.exec_sh(namespace, pod, f"cat {logdir}/iops.*log 2>/dev/null",
                       container=container, timeout=180)
    if not raw.strip():
        return
    # fio: <msec since start>, <value>, <rw 0=read 1=write>, <bs>, ...
    per_sec: dict[int, dict[str, float]] = {}
    for line in raw.splitlines():
        f = [x.strip() for x in line.split(",")]
        if len(f) < 3:
            continue
        try:
            sec = int(int(f[0]) / 1000)
            val = float(f[1])
            rw = int(f[2])
        except ValueError:
            continue
        row = per_sec.setdefault(sec, {"read": 0.0, "write": 0.0})
        row["read" if rw == 0 else "write"] += val
    if not per_sec:
        return

    start = time_base(ctx, pod, d)
    with open(f"{d}/timeseries.csv", "w") as fh:
        fh.write("second,wall_clock,total_iops,read_iops,write_iops,active_migration\n")
        for sec in sorted(per_sec):
            row = per_sec[sec]
            total = row["read"] + row["write"]
            wall = ""
            active = ""
            if start:
                ts = start + timedelta(seconds=sec)
                wall = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
                active = next((m.name for m in migs if m.covers(ts)), "")
            fh.write(f"{sec},{wall},{total},{row['read']},{row['write']},{active}\n")


def time_base(ctx: RunContext, pod: str, d: str) -> datetime | None:
    """The wall clock of second 0 of one instance's fio time series.

    fio's log timestamps are milliseconds since *that job* started, so the only correct base
    is the job's own start: `job_start` in its result.json, in epoch milliseconds. The run's
    own window start is not that base. It is stamped when the run began, before the PVCs, the
    pods and their fio processes existed, so it sits well ahead of every pod's fio start
    (162-211s in fiomig-1787685649, and every pod by a different amount). Using it shifts
    every wall clock by that much and, because the same base decides which migration an
    outage overlaps, names the wrong migration for gaps near a real one.

    Falls back to the run's start when result.json is unreadable: a wrong base is still
    better than an empty wall_clock column, and the fallback is reported.
    """
    try:
        with open(f"{d}/result.json") as fh:
            jobs = json.load(fh).get("jobs") or []
        start_ms = jobs[0].get("job_start") if jobs else None
        if isinstance(start_ms, int | float) and start_ms > 0:
            return datetime.fromtimestamp(start_ms / 1000.0, tz=UTC)
    except (OSError, json.JSONDecodeError, AttributeError, IndexError):
        pass
    run_start, _ = ctx.window()
    ctx.log.warn(f"fio: {pod}: result.json carries no job_start; falling back to the run's "
                 "start, which leads each pod's real fio start")
    return run_start
