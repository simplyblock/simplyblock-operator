#!/usr/bin/env python3
#
# Checks the reconcilers against the invariants this skill states, and follows
# the call graph out of Reconcile to do it.
#
# It exists because the first invariant, never block in Reconcile, is the one a
# grep cannot verify. `time.Sleep` appears nowhere in the controllers, and a
# reconcile still blocks for two minutes through a helper that waits on
# `<-time.After` three calls down. Only following the calls finds that, and it is
# the failure mode behind "the operator was up but nothing progressed."
#
# Usage:
#   check-reconcilers.py                 every reconciler
#   check-reconcilers.py --changed       only controllers this change touched
#   check-reconcilers.py --paths a.go    named files
#   check-reconcilers.py --graph Reconcile   print what a reconcile can reach
#   check-reconcilers.py --strict        exit non-zero on warnings too
#
# Exit status is 1 when an error was reported. The call graph is built over
# operator/internal, matching callees by function name, so a helper in another
# package under internal/ is followed and one in atlas-lib is not.

import argparse
import re
import subprocess
import sys
from pathlib import Path

SCAN_DIR = "operator/internal"
CONTROLLER_DIR = "operator/internal/controller"

FUNC_RE = re.compile(r"^func\s+(?:\((\w+)\s+\*?(\w+)\)\s+)?(\w+)\s*\(")
# A helper assigned as a func literal inside a var block, which is how the
# longest blocking wait in this repository is spelled.
FUNC_VAR_RE = re.compile(r"^\t(\w+)\s*=\s*func\s*\(")
CALL_RE = re.compile(r"(?:\b(\w+)\s*\()|(?:\.(\w+)\s*\()")

# Waiting expressed as blocking, which is the thing the invariant forbids. Each
# entry is (rule, regex, what it does).
BLOCKING = (
    ("time.Sleep", re.compile(r"\btime\.Sleep\s*\("), "sleeps the worker"),
    ("time.After", re.compile(r"<-\s*time\.After\s*\("), "waits on a timer channel"),
    ("time.Tick", re.compile(r"<-\s*time\.Tick\s*\("), "waits on a ticker"),
    ("wait.Poll", re.compile(r"\bwait\.(Poll|For|Until)\w*\s*\("), "polls in-process"),
    ("WaitGroup", re.compile(r"\.Wait\s*\(\s*\)"), "joins a goroutine"),
)

# A non-zero Result returned beside a non-nil error: controller-runtime drops the
# Result and requeues with backoff, so the RequeueAfter silently does nothing.
RESULT_AND_ERROR_RE = re.compile(
    r"return\s+(?:&)?ctrl\.Result\{[^}]*(?:RequeueAfter|Requeue)\s*:[^}]*\}\s*,\s*"
    r"(err\b|error\b|fmt\.Errorf|errors\.New|apierrors\.)"
)

FATAL_RE = re.compile(r"\b(os\.Exit|log\.Fatal\w*|klog\.Fatal\w*)\s*\(")
PANIC_RE = re.compile(r"^\s*panic\s*\(")
PHASE_SWITCH_RE = re.compile(r"\bswitch\s+[^{]*\b(Phase|SubPhase|Step)\b")
STATUS_ASSIGN_RE = re.compile(r"\b(\w+)\.Status\.\w+\s*=(?!=)")
PLAIN_UPDATE_RE = re.compile(r"\.(?:Client\.)?Update\s*\(\s*ctx\s*,\s*&?(\w+)")
STATUS_UPDATE_RE = re.compile(r"\.Status\(\)\.(Update|Patch)\s*\(")
JOB_CREATE_RE = re.compile(r"&batchv1\.Job\{")
OWNS_JOB_RE = re.compile(r"\.Owns\(\s*&batchv1\.Job\{")


class Func:
    def __init__(self, name, receiver, path, line, literal=False):
        self.literal = literal
        self.name = name
        self.receiver = receiver
        self.path = path
        self.line = line
        self.body = []
        self.callees = set()

    @property
    def label(self):
        return f"{self.receiver}.{self.name}" if self.receiver else self.name

    def text(self):
        return "\n".join(self.body)


def parse(path):
    """Top-level functions with their bodies and the names they call."""
    funcs, current = [], None
    for number, raw in enumerate(path.read_text().splitlines(), start=1):
        match = FUNC_RE.match(raw)
        if match:
            current = Func(match.group(3), match.group(2), path, number)
            funcs.append(current)
            continue
        literal = FUNC_VAR_RE.match(raw)
        if literal:
            current = Func(literal.group(1), None, path, number, literal=True)
            funcs.append(current)
            continue
        # A declaration ends at a column-0 brace. A func literal in a var block
        # ends one tab in, and a column-0 brace inside one would be a syntax
        # error, so the two terminators never overlap.
        if raw.startswith("}") or (current is not None and current.literal
                                   and raw.rstrip() == "\t}"):
            current = None
            continue
        if current is None:
            continue
        current.body.append(raw)
        stripped = raw.split("//", 1)[0]
        for bare, method in CALL_RE.findall(stripped):
            name = bare or method
            if name and name not in ("if", "for", "switch", "return", "func", "go", "defer"):
                current.callees.add(name)
    return funcs


def blocking_paths(entry, by_name, seen=None, path=None):
    """Every route from a reconcile to a blocking construct, shortest first."""
    seen = seen or set()
    path = path or [entry]
    if entry.label in seen or len(path) > 8:
        return []
    seen = seen | {entry.label}

    found = []
    for rule, pattern, what in BLOCKING:
        for offset, line in enumerate(entry.body):
            if pattern.search(line.split("//", 1)[0]):
                found.append((path, rule, what, entry.line + offset + 1))
                break

    for callee in sorted(entry.callees):
        for candidate in by_name.get(callee, []):
            found.extend(blocking_paths(candidate, by_name, seen, path + [candidate]))
    return found


def audit(funcs, controller_funcs, by_name):
    """Findings as (level, rule, path, line, message)."""
    found = []

    def error(rule, path, line, message):
        found.append(("ERROR", rule, path, line, message))

    def warn(rule, path, line, message):
        found.append(("WARN", rule, path, line, message))

    reconciles = [f for f in controller_funcs if f.name == "Reconcile"]

    # ---- invariant 1: never block in Reconcile
    reported = set()
    for entry in reconciles:
        for route, rule, what, line in blocking_paths(entry, by_name):
            target = route[-1]
            key = (target.label, rule)
            if key in reported:
                continue
            reported.add(key)
            chain = " → ".join(f.label for f in route)
            error("blocking-in-reconcile", target.path, line,
                  f"{rule} {what} on a reconcile path: {chain}. A worker holds its key "
                  "while it waits, and the default concurrency is one. Express waiting "
                  "as state plus a RequeueAfter")

    # ---- per-function checks over the controllers
    for func in controller_funcs:
        text = func.text()

        for offset, line in enumerate(func.body):
            code = line.split("//", 1)[0]
            if RESULT_AND_ERROR_RE.search(code):
                error("result-and-error", func.path, func.line + offset + 1,
                      f"{func.label} returns a non-zero Result beside a non-nil error. "
                      "controller-runtime drops the Result and requeues with backoff, so "
                      "the RequeueAfter does nothing")
            if FATAL_RE.search(code):
                error("fatal-in-controller", func.path, func.line + offset + 1,
                      f"{func.label} exits the process. Return an error so the work is "
                      "requeued instead of killing every other controller")
            if PANIC_RE.search(code):
                warn("panic-in-controller", func.path, func.line + offset + 1,
                     f"{func.label} panics. Return an error so the reconcile requeues")

        # Only when the object whose status was assigned is the object passed to
        # Update. Labeling some other object with Update is ordinary work.
        assigned = {m.group(1) for m in STATUS_ASSIGN_RE.finditer(text)}
        updated = {m.group(1) for m in PLAIN_UPDATE_RE.finditer(text)}
        overlap = assigned & updated
        if overlap and not STATUS_UPDATE_RE.search(text):
            error("status-via-update", func.path, func.line,
                  f"{func.label} assigns to {sorted(overlap)[0]}.Status and writes it "
                  "with Update rather than Status().Update, which needs spec RBAC and "
                  "bumps generation")

        if PHASE_SWITCH_RE.search(text):
            warn("hand-rolled-phase-switch", func.path, func.line,
                 f"{func.label} switches on a phase string. atlas-lib/statemachine "
                 "validates transitions against a declared graph and survives a restart")

    # ---- finalizers, per file
    by_file = {}
    for func in controller_funcs:
        by_file.setdefault(func.path, []).append(func)
    for path, group in sorted(by_file.items()):
        text = "\n".join(f.text() for f in group)
        removes = (
            "RemoveFinalizer" in text
            or re.search(r"RemoveString\(\s*\w+\.Finalizers", text)
            or re.search(r"\.Finalizers\s*=(?!=)", text)
        )
        if "AddFinalizer" in text and not removes:
            error("finalizer-never-removed", path, group[0].line,
                  f"{path.name} adds a finalizer and never removes one, so an object of "
                  "this kind cannot finish deleting")

    # ---- a Job waited on without watching it
    for path, group in sorted(by_file.items()):
        text = "\n".join(f.text() for f in group)
        if JOB_CREATE_RE.search(text) and not OWNS_JOB_RE.search(text):
            warn("job-without-owns", path, group[0].line,
                 f"{path.name} creates a Job but its SetupWithManager does not "
                 ".Owns(&batchv1.Job{}), so the Job's terminal event wakes no reconcile")

    return found, len(reconciles)


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--changed", action="store_true", help="only controllers changed vs HEAD")
    parser.add_argument("--paths", nargs="*", help="named controller files")
    parser.add_argument("--graph", metavar="FUNC", help="print what this function can reach")
    parser.add_argument("--strict", action="store_true", help="exit non-zero on warnings too")
    args = parser.parse_args()

    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / SCAN_DIR).is_dir():
            repo = parent
            break
    else:
        print(f"check-reconcilers.py: {SCAN_DIR} not found above {here}", file=sys.stderr)
        return 2

    # The call graph spans internal/, so a helper in another package is followed.
    scan = [
        p for p in (repo / SCAN_DIR).rglob("*.go")
        if not p.name.endswith("_test.go") and "zz_generated" not in p.name
    ]
    # Named paths join the graph rather than only filtering it, so the checker
    # works on a file that does not live under internal/ yet.
    named = [Path(p).resolve() for p in (args.paths or [])]
    for path in named:
        if path not in {p.resolve() for p in scan}:
            scan.append(path)

    funcs = [f for path in scan for f in parse(path)]
    by_name = {}
    for func in funcs:
        by_name.setdefault(func.name, []).append(func)

    if args.paths:
        targets = set(named)
    elif args.changed:
        listed = set()
        for command in (
            ["git", "diff", "--name-only", "HEAD", "--", CONTROLLER_DIR],
            ["git", "diff", "--name-only", "--cached", "--", CONTROLLER_DIR],
            ["git", "ls-files", "--others", "--exclude-standard", "--", CONTROLLER_DIR],
        ):
            result = subprocess.run(command, cwd=repo, capture_output=True, text=True)
            listed.update(
                line for line in result.stdout.split("\n")
                if line.endswith(".go") and not line.endswith("_test.go")
            )
        if not listed:
            print("check-reconcilers.py: no controller files changed against HEAD")
            return 0
        targets = {(repo / name).resolve() for name in listed}
    else:
        targets = {p.resolve() for p in (repo / CONTROLLER_DIR).glob("*.go")}

    controller_funcs = [f for f in funcs if f.path.resolve() in targets]
    if not controller_funcs:
        print("check-reconcilers.py: no reconciler code in scope", file=sys.stderr)
        return 2

    if args.graph:
        roots = by_name.get(args.graph, [])
        if not roots:
            print(f"check-reconcilers.py: no function named {args.graph}", file=sys.stderr)
            return 2
        for root in roots:
            print(f"\n{root.label}  ({root.path.name}:{root.line})")
            walk(root, by_name, set(), 1)
        return 0

    found, reconcilers = audit(funcs, controller_funcs, by_name)

    errors = sum(1 for f in found if f[0] == "ERROR")
    warnings = len(found) - errors
    for level, rule, path, line, message in sorted(found, key=lambda f: (f[0], str(f[2]), f[3])):
        print(f"{level:<5} {path.name}:{line}  [{rule}] {message}")

    print(f"\n{reconcilers} reconcilers in scope, {errors} error(s), {warnings} warning(s)")
    if errors or (args.strict and warnings):
        return 1
    return 0


def walk(func, by_name, seen, depth):
    """Prints the reachable call tree, marking anything that blocks."""
    if func.label in seen or depth > 5:
        return
    seen = seen | {func.label}
    for callee in sorted(func.callees):
        for candidate in by_name.get(callee, []):
            marks = [
                rule for rule, pattern, _ in BLOCKING
                if any(pattern.search(line.split("//", 1)[0]) for line in candidate.body)
            ]
            flag = f"  <-- {', '.join(marks)}" if marks else ""
            print(f"{'  ' * depth}{candidate.label}  ({candidate.path.name}:{candidate.line}){flag}")
            walk(candidate, by_name, seen, depth + 1)


if __name__ == "__main__":
    sys.exit(main())
