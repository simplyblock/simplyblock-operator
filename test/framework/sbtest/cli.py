"""sbtest command line.

    sbtest detectors                       what can judge a run, and with which options
    sbtest components                      what can run during a run
    sbtest analyze <rundir> [...]          judge a finished run directory
    sbtest collect  <outdir>  [...]        observe a cluster, then collect the evidence
    sbtest run --suite <name> [...]        drive a full test run, then judge it

`analyze` is the one to reach for first. It runs the whole detector set over an artifact
directory — including every archived fio-mig-* run — so a check can be written or fixed and
tried immediately against the run that motivated it, instead of against the next four-hour
run.
"""

from __future__ import annotations

import argparse
import os
import sys

from . import __version__
from .adapters import ArchiveEvidence
from .core import (
    Config,
    Logger,
    RunContext,
    Runner,
    apply_cli_toggles,
    known_components,
    known_detectors,
    load,
    now_utc,
    suite_path,
)
from .detectors.ana import freeze_summary


def _cfg(args: argparse.Namespace, known_c: list[str], known_d: list[str]) -> Config:
    path = suite_path(args.suite) if args.suite else None
    if args.suite and not path:
        raise SystemExit(f"suite not found: {args.suite}")
    cfg = load(path, known_c, known_d)
    apply_cli_toggles(cfg.components, args.enable_component, args.disable_component)
    apply_cli_toggles(cfg.detectors, args.enable_detector, args.disable_detector)
    return cfg


def _first_doc_line(cls: type) -> str:
    """The first line of a class docstring, or "" — a class may have none at all."""
    doc = (cls.__doc__ or "").strip()
    return doc.splitlines()[0] if doc else ""


def cmd_list(args: argparse.Namespace) -> int:
    if args.what == "detectors":
        for name, dcls in known_detectors().items():
            d = dcls()
            print(name)
            print(f"    {d.summary or _first_doc_line(dcls)}")
            for k, v in sorted(d.defaults().items()):
                print(f"      {k} = {v!r}")
    else:
        for name, ccls in known_components().items():
            c = ccls()
            print(name)
            print(f"    {c.summary or _first_doc_line(ccls)}")
            for k, v in sorted(c.defaults().items()):
                print(f"      {k} = {v!r}")
    return 0


def cmd_analyze(args: argparse.Namespace) -> int:
    ev = ArchiveEvidence(args.rundir)
    outdir = args.outdir or args.rundir
    log = Logger(os.path.join(outdir, "sbtest-analyze.log") if args.write_log else None,
                 verbose=args.verbose)
    ctx = RunContext(run_id=ev.run_id, outdir=outdir, log=log)

    cfg = _cfg(args, list(known_components()), list(known_detectors()))
    # analyze never runs components; make that explicit rather than silently ignoring them.
    if cfg.components.enabled:
        log.info("analyze: components are not run (" +
                 ", ".join(sorted(cfg.components.enabled)) + ")")
    cfg.components.enabled = {}

    log.info(f"analyzing {ev.outdir} (run {ev.run_id})")
    runner = Runner(cfg, ctx).build()
    runner.judge(ev)

    if args.freeze_table:
        rows = freeze_summary(ev)
        if rows:
            log.info("cutover freezes per migration (>1 has predicted write loss exactly):")
            for name, n, worst in sorted(rows, key=lambda r: (-r[1], r[0])):
                mark = "  <== re-froze" if n > 1 else ""
                log.info(f"    {name:28} freezes={n:<3} worst={worst:.0f}s{mark}")

    report = runner.emit(json_name=args.json_name)
    from .core.runner import findings_by_subject_table
    correlated = findings_by_subject_table(report)
    if correlated:
        log.info("subjects with more than one finding (the strongest signal these runs give):")
        for line in correlated:
            log.info(f"    {line}")
    return 1 if report.failed else 0


def cmd_collect(args: argparse.Namespace) -> int:
    os.makedirs(args.outdir, exist_ok=True)
    run_id = args.run_id or f"sbtest-{int(now_utc().timestamp())}"
    log = Logger(os.path.join(args.outdir, "sbtest.log"), verbose=args.verbose)
    ctx = RunContext(run_id=run_id, outdir=args.outdir, log=log)
    cfg = _cfg(args, list(known_components()), list(known_detectors()))
    if not cfg.components.enabled:
        raise SystemExit("collect: no components enabled — pass --enable-component or a suite")

    runner = Runner(cfg, ctx).build()
    log.info(f"collecting into {args.outdir} for {args.duration}s")
    try:
        runner.setup()
        runner.start()
        if args.duration:
            runner.run_for(float(args.duration))
        runner.stop()
        runner.collect()
    finally:
        runner.teardown()

    if args.judge:
        runner.judge(ArchiveEvidence(args.outdir))
        report = runner.emit(json_name=args.json_name)
        return 1 if report.failed else 0
    log.info(f"collected into {args.outdir}; judge it with: sbtest analyze {args.outdir}")
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    """A driven run: components create the load, then the same detectors judge it.

    The difference from `collect` is only what the components do — one of them drives
    migrations instead of watching them. Everything after that is identical, which is the
    point: a live run and an archived one are judged by the same code, so a verdict from
    either means the same thing.
    """
    cfg = _cfg(args, list(known_components()), list(known_detectors()))
    run_id = args.run_id or f"{cfg.run_id_prefix}-{int(now_utc().timestamp())}"
    outdir = args.outdir or os.path.join(cfg.outdir, run_id)
    os.makedirs(outdir, exist_ok=True)
    log = Logger(os.path.join(outdir, "test.log"), verbose=args.verbose)
    ctx = RunContext(run_id=run_id, outdir=outdir, log=log)
    # Read by the components that create cluster objects. A kept run leaves the volumes and
    # the CRs behind for inspection — the reason most post-mortems are possible at all.
    ctx.shared["keep"] = bool(args.keep)

    if not cfg.components.enabled:
        raise SystemExit("run: no components enabled — pass --enable-component or a suite")
    drivers = [n for n in cfg.components.enabled if n.endswith(".driver")]
    if not drivers:
        log.warn("no driver component is enabled, so nothing will create load — this run "
                 "will only observe. Use `sbtest collect` if that is what you meant")

    runner = Runner(cfg, ctx).build()
    log.info(f"run {run_id} -> {outdir} (duration {args.duration or 0:.0f}s, "
             f"keep={bool(args.keep)})")
    try:
        runner.setup()
        runner.start()
        if args.duration:
            runner.run_for(float(args.duration))
        runner.stop()
        runner.collect()
    finally:
        runner.teardown()

    # Judged from the artifact directory rather than from memory, so the verdict comes from
    # exactly the evidence someone else would re-analyse later. A run that reports PASS from
    # in-memory state it never wrote down is not reproducible.
    runner.judge(ArchiveEvidence(outdir))
    report = runner.emit(json_name=args.json_name)
    return 1 if report.failed else 0


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="sbtest", description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--version", action="version", version=f"sbtest {__version__}")
    sub = p.add_subparsers(dest="cmd", required=True)

    def common(sp: argparse.ArgumentParser) -> None:
        sp.add_argument("--suite", help="suite file, or a bare name under sbtest/suites/")
        sp.add_argument("--enable-detector", action="append", default=[], metavar="NAME")
        sp.add_argument("--disable-detector", action="append", default=[], metavar="NAME")
        sp.add_argument("--enable-component", action="append", default=[], metavar="NAME")
        sp.add_argument("--disable-component", action="append", default=[], metavar="NAME")
        sp.add_argument("--json-name", default="findings.json",
                        help="where to write findings inside the run directory")
        sp.add_argument("-v", "--verbose", action="store_true")

    for what in ("detectors", "components"):
        sp = sub.add_parser(what, help=f"list the available {what}")
        sp.set_defaults(func=cmd_list, what=what)

    sp = sub.add_parser("analyze", help="judge a finished run directory")
    sp.add_argument("rundir")
    sp.add_argument("--outdir", help="where to write findings (default: the run directory)")
    sp.add_argument("--write-log", action="store_true",
                    help="also write sbtest-analyze.log into the output directory")
    sp.add_argument("--freeze-table", action="store_true",
                    help="print freezes per migration alongside the findings")
    common(sp)
    sp.set_defaults(func=cmd_analyze)

    sp = sub.add_parser("collect", help="run the collection components against a cluster")
    sp.add_argument("outdir")
    sp.add_argument("--duration", type=float, default=0.0,
                    help="seconds to keep components running before collecting")
    sp.add_argument("--run-id")
    sp.add_argument("--judge", action="store_true", help="judge the result when done")
    common(sp)
    sp.set_defaults(func=cmd_collect)

    sp = sub.add_parser("run", help="drive a full test run against a cluster, then judge it")
    sp.add_argument("--duration", type=float, default=0.0,
                    help="seconds to run before stopping the drivers and collecting")
    sp.add_argument("--outdir", help="where to write artifacts "
                                     "(default: <suite outdir>/<run id>)")
    sp.add_argument("--run-id")
    sp.add_argument("--keep", action="store_true",
                    help="leave the volumes, pods and CRs behind for inspection")
    common(sp)
    sp.set_defaults(func=cmd_run)
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
