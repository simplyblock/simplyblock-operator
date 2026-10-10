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
import os
import re
import time
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

from ...core import RunContext
from .. import kube

FIO_IMAGE = "alpine:3.20"

# The options every fio-driving workload exposes, with the defaults they share. A workload
# merges these into its own `defaults()` so the knobs are spelled the same everywhere.
FIO_DEFAULTS: dict[str, Any] = {
    # fio's --runtime. 0 takes the run's --duration (see FioWorkload._resolve_runtime): the
    # run waits for fio to finish, so the two are one setting unless a suite says otherwise.
    "runtime_s": 0,
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


def _install(logdir: str) -> str:
    return (
        "set -u\n"
        'echo "[pod] $(date -u +%FT%TZ) installing fio"\n'
        "apk add --no-cache fio >/dev/null 2>&1 || "
        '{ echo "[pod] apk add fio FAILED"; exit 90; }\n'
        f"mkdir -p {logdir}\n"
    )


def container_script(args: list[str], logdir: str) -> str:
    """The shell a fio container runs: install fio, run it, keep the logs readable."""
    return (
        _install(logdir)
        + 'echo "[pod] $(date -u +%FT%TZ) starting fio"\n'
        + " ".join(args) + "\n"
        "rc=$?\n"
        f'echo "$rc" > {logdir}/fio.rc\n'
        'echo "[pod] $(date -u +%FT%TZ) fio exited rc=$rc"\n'
        # Stay alive after fio exits so the logs on the emptyDir can still be collected.
        "sleep 100000\n"
    )


# ── rounds: one writer, readers on other clients ─────────────────────────────────────
#
# A randrw instance verifies its own writes through its own client, so it cannot show that
# another client sees them. Rounds can: a writer writes a whole file, closes it, and only
# then publishes a marker, and readers on other nodes open the file after the marker and
# verify it. NFS promises close-to-open consistency, so a reader that opens after the
# writer's close must see every block, and fio's verify_only checks each one against the
# header the writer put in it.

#: Rounds a writer keeps on the volume: the one being written, and the two before it, which
#: readers may still be verifying. Older rounds are removed.
ROUND_KEEP = 3


@dataclass(frozen=True)
class Rounds:
    """Where and how one writer publishes its rounds, which its readers must know too."""

    base: str        # round N is <base>.r<N>, and the marker round_marker(base)
    size_mb: int     # the size of each round's file
    round_s: int     # a writer starts a round at most this often
    wait_s: int      # how long a reader waits for the next round before recording a timeout
    runtime_s: int   # how long the writer starts new rounds
    #: How long a round pod waits for <logdir>/start, which the workload writes once the
    #: timed run starts, before it starts without it. 0 starts at once.
    start_wait_s: int = 0


def round_marker(base: str) -> str:
    """The file a writer replaces after each round: `<round> <rc> <more|last>`."""
    return f"{base}.marker"


def round_args(opts: Mapping[str, Any], rounds: Rounds, *, direct: bool) -> list[str]:
    """The fio arguments a round's writer and its readers share.

    A sequential write of the whole file, with fio's default seeds, lays out the same blocks
    and headers every time, which is what lets a reader's verify_only check a file another
    process wrote. The file and output are shell variables the round loop sets.
    """
    return [
        "fio", "--name=xround", '--filename="$f"', f"--size={rounds.size_mb}M",
        f"--ioengine={opts['ioengine']}", f"--direct={1 if direct else 0}", "--rw=write",
        f"--bs={opts['bs']}", f"--iodepth={opts['iodepth']}", "--numjobs=1",
        "--continue_on_error=all", "--verify=md5",
        f"--verify_fatal={1 if opts['verify_fatal'] else 0}",
        # No dump of a failing block, and no verify state file: either would land in the
        # working directory, which nothing collects.
        "--verify_dump=0", "--verify_state_save=0",
        *(["--serialize_overlap=1"] if int(opts["iodepth"]) > 1 else []),
        '--output="$out"', "--output-format=json",
    ]


# fio exits 0 after errors it continued past, so its JSON summary is what says a round
# failed. Shared by both scripts: sets r to fio's exit code, or to 1 for such errors.
_ROUND_R = (
    "r=$?\n"
    'if [ "$r" -eq 0 ] && grep -q \'"error" : [1-9]\' "$out" 2>/dev/null; then r=1; fi\n'
)
# Counts round r toward the script's rc. result.json is the first failed round's summary,
# else the latest round's.
_ROUND_RC = 'if [ "$rc" -eq 0 ]; then cp -f "$out" "$logs/result.json" 2>/dev/null; rc=$r; fi\n'


def _indent(snippet: str, depth: int) -> str:
    return "".join(" " * depth + ln + "\n" for ln in snippet.splitlines())


def _await_start(tag: str, rounds: Rounds, logdir: str) -> str:
    """Wait, bounded by start_wait_s, for the workload to say the timed run started.

    A round pod starts with the other pods, and the randrw instances lay out their files
    before their timed run. Without the wait, the rounds would start, and a reader's
    deadline run out, that much earlier than the run they belong to.
    """
    if not rounds.start_wait_s:
        return ""
    return (
        f"i=0; while [ ! -e {logdir}/start ] && [ \"$i\" -lt {rounds.start_wait_s} ]; do "
        "sleep 1; i=$((i + 1)); done\n"
        f'if [ -e {logdir}/start ]; then echo "[{tag}] $(date -u +%FT%TZ) the timed run '
        'started"\n'
        f'else echo "[{tag}] $(date -u +%FT%TZ) no start after {rounds.start_wait_s}s, '
        'starting without it"; fi\n'
    )


def round_ledger(base: str) -> str:
    """The file a writer appends `<round> <rc>` to for every round it publishes."""
    return f"{base}.ledger"


#: fio's line for a block that read back wrong: its file and its offset.
_BAD_BLOCK_RE = re.compile(r"^verify: .* at file (\S+) offset (\d+), length (\d+)")


def bad_ranges(lines: Iterable[str]) -> dict[str, list[tuple[int, int]]]:
    """A reader's bad blocks as (offset, length) ranges per file name, contiguous blocks
    merged. fio may name one block more than once, and those count once."""
    blocks: dict[str, set[tuple[int, int]]] = {}
    for line in lines:
        m = _BAD_BLOCK_RE.search(line)
        if m:
            blocks.setdefault(os.path.basename(m.group(1)), set()).add(
                (int(m.group(2)), int(m.group(3))))
    out: dict[str, list[tuple[int, int]]] = {}
    for name, found in blocks.items():
        ranges: list[tuple[int, int]] = []
        for off, length in sorted(found):
            if ranges and ranges[-1][0] + ranges[-1][1] >= off:
                start, have = ranges[-1]
                ranges[-1] = (start, max(have, off + length - start))
            else:
                ranges.append((off, length))
        out[name] = ranges
    return out


def round_keep_marker(path: str) -> str:
    """The file a reader leaves beside a round it failed, so its writer keeps the round."""
    return f"{path}.keep"


def round_writer_script(opts: Mapping[str, Any], rounds: Rounds, logdir: str,
                        direct: bool = True) -> str:
    """Write a new file each round, fsync it, and close it, then publish a marker.

    The marker is replaced with `mv`, which is atomic in one directory, so a reader never
    reads a half-written marker. Before the marker, the round and its rc go into the ledger,
    so a reader that falls behind still knows how each round it missed ended. A round whose
    fio failed is published with its rc, and readers skip it. Writes no IOPS log: rounds
    idle between files, which fio.outage reads as an outage.
    """
    args = [*round_args(opts, rounds, direct=direct), "--do_verify=0", "--end_fsync=1",
            "--fsync_on_close=1"]
    marker = round_marker(rounds.base)
    return (
        _install(logdir)
        + _await_start("xwrite", rounds, logdir)
        + f"logs={logdir}; base={rounds.base}; out=$logs/round.json\n"
        f"end=$(( $(date +%s) + {rounds.runtime_s} )); n=0; r=0; rc=0\n"
        f'echo "[xwrite] $(date -u +%FT%TZ) writing {rounds.size_mb}M rounds to $base.r<N>"\n'
        'while [ "$(date +%s)" -lt "$end" ]; do\n'
        '  n=$((n + 1)); f="$base.r$n"; t0=$(date +%s)\n'
        "  " + " ".join(args) + "\n"
        + _indent(_ROUND_R + _ROUND_RC, 2)
        + f'  echo "$n $r" >> "{round_ledger(rounds.base)}"\n'
        f'  echo "$n $r more" > "{marker}.tmp" && mv -f "{marker}.tmp" "{marker}"\n'
        '  echo "[xwrite] $(date -u +%FT%TZ) round $n written rc=$r"\n'
        # A round a reader failed is kept as evidence (round_keep_marker), however old.
        f'  old="$base.r$((n - {ROUND_KEEP}))"\n'
        '  if [ ! -e "$old.keep" ]; then rm -f "$old"; fi\n'
        f"  pause=$(( t0 + {rounds.round_s} - $(date +%s) ))\n"
        '  if [ "$pause" -gt 0 ]; then sleep "$pause"; fi\n'
        "done\n"
        f'echo "$n $r last" > "{marker}.tmp" && mv -f "{marker}.tmp" "{marker}"\n'
        # The summary before fio.rc: stop() and collection go ahead once fio.rc exists.
        'echo "[xwrite] $(date -u +%FT%TZ) done: rounds=$n rc=$rc"\n'
        f'echo "$rc" > {logdir}/fio.rc\n'
        "sleep 100000\n"
    )


def round_reader_script(opts: Mapping[str, Any], rounds: Rounds, logdir: str,
                        direct: bool = True) -> str:
    """Wait for each new round's marker, then verify every round up to it, in order.

    A reader slower than its writer works through the rounds it has not read yet, taking
    each one's rc from the ledger. A round the writer has already removed, ROUND_KEEP
    rounds after it, is logged as lapsed: this reader fell behind. A round the writer still
    keeps whose file is gone is logged as missing, and fails the reader. Every wait is
    bounded: a wait longer than wait_s is logged as a timeout and the reader waits again,
    and the reader stops at runtime_s + wait_s whatever happened, so stop() always finds its
    exit code. One line per round names the outcome, and the last line counts them, for
    fio.cross-read.
    """
    args = [*round_args(opts, rounds, direct=direct), "--verify_only"]
    marker = round_marker(rounds.base)
    return (
        _install(logdir)
        + _await_start("xread", rounds, logdir)
        + f"logs={logdir}; base={rounds.base}; out=$logs/round.json\n"
        # Whether the writer has removed round i: it does so once it publishes round
        # i + ROUND_KEEP.
        f'removed() {{ set -- $(cat "{marker}" 2>/dev/null); '
        f'[ "${{1:-0}}" -ge $(( i + {ROUND_KEEP} )) ]; }}\n'
        f"deadline=$(( $(date +%s) + {rounds.runtime_s} + {rounds.wait_s} ))\n"
        "last=0; rc=0; final=0; verified=0; failed=0; skipped=0; missing=0; lapsed=0\n"
        "timeouts=0\n"
        'while [ "$final" -eq 0 ] && [ "$(date +%s)" -lt "$deadline" ]; do\n'
        "  since=$(date +%s); n=0; w=0; st=more\n"
        "  while :; do\n"
        f'    set -- $(cat "{marker}" 2>/dev/null)\n'
        '    n=${1:-0}; w=${2:-0}; st=${3:-more}\n'
        '    if [ "$n" -gt "$last" ] || [ "$st" = last ]; then break; fi\n'
        f'    if [ $(( $(date +%s) - since )) -ge {rounds.wait_s} ] || '
        '[ "$(date +%s)" -ge "$deadline" ]; then\n'
        "      timeouts=$((timeouts + 1))\n"
        '      echo "[xread] $(date -u +%FT%TZ) waiting for a round after $last timed out '
        f'after {rounds.wait_s}s"\n'
        "      break\n"
        "    fi\n"
        "    sleep 1\n"
        "  done\n"
        '  if [ "$st" = last ]; then final=1; fi\n'
        '  i=$last\n'
        '  while [ "$i" -lt "$n" ]; do\n'
        '    i=$((i + 1)); f="$base.r$i"; wi=$w\n'
        '    if [ "$i" -lt "$n" ]; then\n'
        f"      wi=$(awk -v i=\"$i\" '$1 == i {{ print $2 }}' \"{round_ledger(rounds.base)}\" "
        "2>/dev/null); wi=${wi:-0}\n"
        "    fi\n"
        '    if [ "$wi" -ne 0 ]; then\n'
        "      skipped=$((skipped + 1))\n"
        '      echo "[xread] $(date -u +%FT%TZ) round $i skipped: its writer ended rc=$wi"\n'
        # Never let fio lay out a file that is not there: that would write the volume, and
        # verify the zeros it wrote.
        '    elif [ ! -f "$f" ] && removed; then\n'
        "      lapsed=$((lapsed + 1))\n"
        '      echo "[xread] $(date -u +%FT%TZ) round $i lapsed: removed before this reader '
        'reached it"\n'
        '    elif [ ! -f "$f" ]; then\n'
        "      missing=$((missing + 1))\n"
        '      echo "[xread] $(date -u +%FT%TZ) round $i missing: $f is gone"\n'
        "    else\n"
        "      " + " ".join(args) + "\n"
        + _indent(_ROUND_R, 6)
        + '      if [ "$r" -eq 0 ]; then\n'
        + _indent(_ROUND_RC, 8)
        + "        verified=$((verified + 1))\n"
        '        echo "[xread] $(date -u +%FT%TZ) round $i verified"\n'
        # The writer removed the file during the read: this reader was too slow, and the
        # read proves nothing either way.
        '      elif [ ! -f "$f" ] && removed; then\n'
        "        lapsed=$((lapsed + 1))\n"
        '        echo "[xread] $(date -u +%FT%TZ) round $i lapsed: removed while this reader '
        'read it"\n'
        "      else\n"
        + _indent(_ROUND_RC, 8)
        + "        failed=$((failed + 1))\n"
        # Marked before the writer can remove it, so the run can ask the server about it.
        '        touch "$f.keep" 2>/dev/null\n'
        '        echo "[xread] $(date -u +%FT%TZ) round $i failed rc=$r"\n'
        "      fi\n"
        "    fi\n"
        "  done\n"
        '  if [ "$n" -gt "$last" ]; then last=$n; fi\n'
        "done\n"
        # A published round that was gone fails the reader, though no fio failed.
        'if [ "$missing" -gt 0 ] && [ "$rc" -eq 0 ]; then rc=1; fi\n'
        # The count before fio.rc: stop() and collection go ahead once fio.rc exists.
        'echo "[xread] $(date -u +%FT%TZ) done: verified=$verified failed=$failed '
        'skipped=$skipped missing=$missing lapsed=$lapsed timeouts=$timeouts"\n'
        f'echo "$rc" > {logdir}/fio.rc\n'
        "sleep 100000\n"
    )


def wait_running(ctx: RunContext, who: str, namespace: str, pods: list[str],
                 timeout_s: float) -> None:
    """Wait until every pod labeled with the run is Running, or say which are not."""
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


def in_timed_run(log: str) -> bool:
    """Whether fio is past laying out its file and in the timed run.

    During layout fio prints its file-layout message and an [f(N)] status. Only the timed
    run prints a status line with an eta and a running job state, such as
    `Jobs: 1 (f=1): [m(1)][0.2%][r=508KiB/s,w=196KiB/s][r=127,w=49 IOPS][eta 34m:57s]`.
    The two together tell real I/O from layout. The test operator/test/
    fio_migration_test.py applies, ported as it is.
    """
    return any("[eta " in line and ("[m(" in line or "[r(" in line or "[w(" in line)
               for line in log.splitlines())


def wait_io_flowing(ctx: RunContext, who: str, namespace: str,
                    instances: list[FioInstance], timeout_s: float) -> list[FioInstance]:
    """Wait until every instance's fio is in the timed run, and return those that are not.

    fio's runtime counts the timed run only, not the layout before it, so this is where a
    run's I/O begins. A slow layout would otherwise eat the window the run measures, and
    the run would judge empty logs. Instances still laying out at timeout_s are named and
    left to run, as operator/test/fio_migration_test.py did: the run continues, and its
    evidence may be incomplete.
    """
    ctx.log.info(f"{who}: waiting for fio to finish layout and enter the timed run in "
                 f"{len(instances)} instance(s)")
    deadline = time.time() + timeout_s
    pending = list(instances)
    while True:
        pending = [i for i in pending if not in_timed_run(kube.run(
            ["-n", namespace, "logs", i.pod, "-c", i.container, "--tail=8"],
            check=False, timeout=30).stdout or "")]
        if not pending or time.time() >= deadline or ctx.stopping.is_set():
            break
        time.sleep(5)
    if pending:
        ctx.log.warn(f"{who}: timed run not confirmed in "
                     + ", ".join(f"{i.pod}/{i.container}" for i in pending)
                     + " (continuing; their evidence may be incomplete)")
    else:
        ctx.log.info(f"{who}: fio timed run active in every instance")
    return pending


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
    """Per-second IOPS from fio's IOPS log, with the migration in flight that second.

    The migration column is the point: correlating a throughput dip with the migration that
    caused it is otherwise a manual join across two files, and the detectors need it to
    attribute an outage to a specific migration rather than to the run.

    Column names match the older harness's CSVs (`second`, `wall_clock`) so a single reader
    serves both.
    """
    # fio names the log <prefix>_iops.<job>.log, and the prefix here is fio_args' `iops`.
    raw = kube.exec_sh(namespace, pod, f"cat {logdir}/iops_iops.*.log 2>/dev/null",
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
