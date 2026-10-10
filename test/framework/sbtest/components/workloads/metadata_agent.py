"""The program workload.pnfs-metadata runs inside its pods: namespace operations, and a listing.

It is shipped into an alpine container as source and run with the python3 that apk installs
there, so it uses the standard library only and imports nothing from sbtest. Two commands:

* `worker` makes namespace operations in one directory of a pNFS volume at a steady rate:
  create, rename (within and across directories), chmod, truncate, stat, readdir, unlink,
  mkdir, and rmdir. Each one is a line in `<logdir>/ops.log` with its start time, path,
  result (the errno name of a failure), and latency. The worker keeps its own account of
  what the directory holds, with every file's size and md5, and writes it to
  `<logdir>/manifest.json`. A path whose last operation failed may or may not have changed,
  so it is listed as uncertain rather than guessed. A `pause` file in the log directory
  makes the worker hold still with a fresh manifest written and `paused` present, so another
  client can compare the directory with it. A `stop` file, the duration, or `--max-ops`
  ends the run with a final manifest and `stopped`.
* `list` prints the directory as the client it runs on sees it, in the manifest's shape.

The operation sequence comes from the seed alone, so a run replays with the same seed.
"""

from __future__ import annotations

import argparse
import errno
import hashlib
import json
import os
import random
import sys
import time
from collections.abc import Callable

# Relative weights of the operations a worker draws from.
DEFAULT_MIX: dict[str, float] = {"create": 4, "rename": 3, "chmod": 1, "truncate": 1, "stat": 3, "readdir": 2,
               "unlink": 2, "mkdir": 1, "rmdir": 1}
MAX_FILE = 16 * 1024
MAX_DEPTH = 2


def _md5(data: bytes) -> str:
    return hashlib.md5(data).hexdigest()  # noqa: S324 (a content check, not security)


def _errno_name(e: OSError) -> str:
    return errno.errorcode.get(e.errno or 0, str(e.errno)) if e.errno else type(e).__name__


class Worker:
    """One directory's namespace operations and the worker's account of the result."""

    def __init__(self, root: str, logdir: str, seed: int, rate: float,
                 mix: dict[str, float]) -> None:
        self.root, self.logdir = root, logdir
        self.rng = random.Random(seed)
        self.rate = max(rate, 0.01)
        self.mix = {k: float(v) for k, v in mix.items() if k in DEFAULT_MIX and float(v) > 0}
        self.files: dict[str, bytes] = {}
        self.dirs: set[str] = set()
        self.uncertain: set[str] = set()
        self.counter = 0
        self.ops = 0
        os.makedirs(logdir, exist_ok=True)
        self.log = open(os.path.join(logdir, "ops.log"), "a", buffering=1)  # noqa: SIM115

    # ── the account ─────────────────────────────────────────────────────────────────

    def _abs(self, rel: str) -> str:
        return os.path.join(self.root, rel) if rel else self.root

    def _name(self, prefix: str) -> str:
        self.counter += 1
        return f"{prefix}{self.counter}"

    def _forget(self, rel: str) -> None:
        """A path whose operation failed: it and everything under it are unknown now."""
        self.uncertain.add(rel)
        self.files.pop(rel, None)
        self.dirs.discard(rel)
        for p in [p for p in self.files if p.startswith(rel + "/")]:
            del self.files[p]
        self.dirs -= {d for d in self.dirs if d.startswith(rel + "/")}

    def manifest(self) -> None:
        doc = {"t": time.time(), "seq": self.ops,
               "files": {p: {"size": len(b), "md5": _md5(b)} for p, b in sorted(self.files.items())},
               "dirs": sorted(self.dirs), "uncertain": sorted(self.uncertain)}
        tmp = os.path.join(self.logdir, "manifest.json.tmp")
        with open(tmp, "w") as fh:
            json.dump(doc, fh)
        os.replace(tmp, os.path.join(self.logdir, "manifest.json"))

    # ── the operations ──────────────────────────────────────────────────────────────

    def _parent(self) -> str:
        return self.rng.choice([""] + sorted(d for d in self.dirs if d.count("/") < MAX_DEPTH - 1))

    def _file(self) -> str:
        return self.rng.choice(sorted(self.files))

    def _join(self, parent: str, name: str) -> str:
        return f"{parent}/{name}" if parent else name

    def step(self) -> None:
        """One operation, drawn from the mix among those the directory allows now."""
        allowed = dict(self.mix)
        if not self.files:
            for k in ("rename", "chmod", "truncate", "unlink"):
                allowed.pop(k, None)
        empty = [d for d in self.dirs
                 if not any(p.startswith(d + "/") for p in list(self.files) + list(self.dirs))]
        if not empty:
            allowed.pop("rmdir", None)
        if not allowed:
            allowed = {"create": 1.0}
        op = self.rng.choices(sorted(allowed), weights=[allowed[k] for k in sorted(allowed)])[0]
        getattr(self, f"_op_{op}")(empty)

    def _run(self, op: str, rel: str, fn: Callable[[], object],
             on_error: Callable[[], None] | None = None) -> bool:
        t, t0 = time.time(), time.monotonic()
        err = ""
        try:
            fn()
        except OSError as e:
            err = _errno_name(e)
            if on_error:
                on_error()
        ms = (time.monotonic() - t0) * 1000.0
        self.log.write(json.dumps({"t": round(t, 6), "op": op, "path": rel, "ok": not err,
                                   "err": err, "ms": round(ms, 3)}) + "\n")
        self.ops += 1
        return not err

    def _op_create(self, _empty: list[str]) -> None:
        rel = self._join(self._parent(), self._name("f"))
        data = self.rng.randbytes(self.rng.randint(0, MAX_FILE))

        def do() -> None:
            with open(self._abs(rel), "wb") as fh:
                fh.write(data)

        if self._run("create", rel, do, lambda: self._forget(rel)):
            self.files[rel] = data

    def _op_rename(self, _empty: list[str]) -> None:
        src = self._file()
        dst = self._join(self._parent(), self._name("f"))

        def failed() -> None:
            self._forget(src)
            self._forget(dst)

        if self._run("rename", src, lambda: os.rename(self._abs(src), self._abs(dst)), failed):
            self.files[dst] = self.files.pop(src)

    def _op_chmod(self, _empty: list[str]) -> None:
        rel = self._file()
        mode = self.rng.choice([0o600, 0o640, 0o644])
        self._run("chmod", rel, lambda: os.chmod(self._abs(rel), mode))

    def _op_truncate(self, _empty: list[str]) -> None:
        rel = self._file()
        old = self.files[rel]
        size = self.rng.randint(0, len(old) + 4096)
        if self._run("truncate", rel, lambda: os.truncate(self._abs(rel), size),
                     lambda: self._forget(rel)):
            self.files[rel] = old[:size] + b"\0" * max(0, size - len(old))

    def _op_stat(self, _empty: list[str]) -> None:
        rel = self.rng.choice(sorted(self.files) + sorted(self.dirs) + [""])
        self._run("stat", rel, lambda: os.stat(self._abs(rel)))

    def _op_readdir(self, _empty: list[str]) -> None:
        rel = self.rng.choice([""] + sorted(self.dirs))
        self._run("readdir", rel, lambda: os.listdir(self._abs(rel)))

    def _op_unlink(self, _empty: list[str]) -> None:
        rel = self._file()
        if self._run("unlink", rel, lambda: os.remove(self._abs(rel)), lambda: self._forget(rel)):
            del self.files[rel]

    def _op_mkdir(self, _empty: list[str]) -> None:
        rel = self._join(self._parent(), self._name("d"))
        if self._run("mkdir", rel, lambda: os.mkdir(self._abs(rel)), lambda: self._forget(rel)):
            self.dirs.add(rel)

    def _op_rmdir(self, empty: list[str]) -> None:
        rel = self.rng.choice(sorted(empty))
        if self._run("rmdir", rel, lambda: os.rmdir(self._abs(rel)), lambda: self._forget(rel)):
            self.dirs.discard(rel)

    # ── the loop ────────────────────────────────────────────────────────────────────

    def _flag(self, name: str) -> str:
        return os.path.join(self.logdir, name)

    def run(self, duration_s: float, manifest_interval_s: float, max_ops: int) -> None:
        end = time.monotonic() + duration_s
        while not self._ready(end):
            time.sleep(1.0)
        last_manifest, nxt = 0.0, time.monotonic()
        while time.monotonic() < end and not os.path.exists(self._flag("stop")):
            if max_ops and self.ops >= max_ops:
                break
            if os.path.exists(self._flag("pause")):
                self._hold()
                nxt = time.monotonic()
                continue
            self.step()
            now = time.monotonic()
            if now - last_manifest >= manifest_interval_s:
                self.manifest()
                last_manifest = now
            nxt = max(nxt + 1.0 / self.rate, now)
            time.sleep(max(0.0, nxt - time.monotonic()))
        self.manifest()
        open(self._flag("stopped"), "w").close()

    def _ready(self, end: float) -> bool:
        """The worker's directory exists, created if need be. Retried rather than fatal: the
        metadata server may be restarting when the pod starts."""
        try:
            os.makedirs(self.root, exist_ok=True)
            return True
        except OSError:
            return time.monotonic() >= end

    def _hold(self) -> None:
        self.manifest()
        open(self._flag("paused"), "w").close()
        while os.path.exists(self._flag("pause")) and not os.path.exists(self._flag("stop")):
            time.sleep(0.05)
        os.remove(self._flag("paused"))


def listing(root: str) -> dict[str, object]:
    """The directory as this client sees it, in the manifest's shape."""
    files: dict[str, dict[str, object]] = {}
    dirs: list[str] = []
    for top, subdirs, names in os.walk(root):
        rel_top = os.path.relpath(top, root)
        for d in subdirs:
            dirs.append(d if rel_top == "." else f"{rel_top}/{d}")
        for n in names:
            rel = n if rel_top == "." else f"{rel_top}/{n}"
            with open(os.path.join(top, n), "rb") as fh:
                data = fh.read()
            files[rel] = {"size": len(data), "md5": _md5(data)}
    return {"files": files, "dirs": sorted(dirs), "root_exists": os.path.isdir(root)}


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="metadata-agent")
    sub = ap.add_subparsers(dest="cmd", required=True)
    w = sub.add_parser("worker")
    w.add_argument("--root", required=True)
    w.add_argument("--logdir", required=True)
    w.add_argument("--seed", type=int, default=0)
    w.add_argument("--rate", type=float, default=2.0)
    w.add_argument("--duration", type=float, default=0.0)
    # The run's own stop time, in epoch seconds. A worker that starts late still stops
    # when the run needs it to, which a duration counted from its start cannot promise.
    w.add_argument("--until", type=float, default=0.0)
    w.add_argument("--manifest-interval", type=float, default=30.0)
    w.add_argument("--max-ops", type=int, default=0)
    w.add_argument("--mix", default="")
    ls = sub.add_parser("list")
    ls.add_argument("--root", required=True)
    args = ap.parse_args(argv)
    if args.cmd == "list":
        json.dump(listing(args.root), sys.stdout)
        return 0
    if not args.duration and not args.until:
        ap.error("worker needs --duration or --until")
    duration = args.duration or float("inf")
    if args.until:
        duration = min(duration, max(0.0, args.until - time.time()))
    mix: dict[str, float] = json.loads(args.mix) if args.mix else DEFAULT_MIX
    Worker(args.root, args.logdir, args.seed, args.rate, mix).run(
        duration, args.manifest_interval, args.max_ops)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
