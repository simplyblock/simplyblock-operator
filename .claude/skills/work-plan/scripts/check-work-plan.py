#!/usr/bin/env python3
#
# Validates a work plan under operator/docs/tasks/, derives its execution order,
# and emits the GitHub issue script for it.
#
# It exists because the two things a work plan is read for are the two things a
# writer cannot keep correct by hand: whether the dependencies are consistent,
# and which items can run at the same time. Dependencies are declared per item
# here and the waves are computed, so the plan cannot drift from its own graph.
#
# Usage:
#   check-work-plan.py <file>              validate and report the execution order
#   check-work-plan.py <file> --waves      the waves only, no validation report
#   check-work-plan.py <file> --write      rewrite section 1 in place from the graph
#   check-work-plan.py <file> --issues     write a `gh issue create` script to stdout
#
# Exit status is 1 when validation failed, so it can gate a hand-off. --issues
# never creates anything: it prints a script for a person to read and run.

import argparse
import re
import sys
from pathlib import Path

# The repository's label set, from `gh label list`, which stays the authority.
COMPONENT_LABELS = {"operator", "csi", "atlas-lib"}
TYPE_LABELS = {"enhancement", "bug", "documentation"}
PRIORITY_LABELS = {"critical", "high", "medium", "low"}
OPTIONAL_LABELS = {"blocked", "security", "good first issue", "help wanted"}
KNOWN_LABELS = COMPONENT_LABELS | TYPE_LABELS | PRIORITY_LABELS | OPTIONAL_LABELS

SIZE_WEIGHT = {"S": 1, "M": 2, "L": 4}

ITEM_RE = re.compile(r"^###\s+(W-\d+)\s+—\s+(.+?)\s*$")
META_RE = re.compile(r"^\*\*([A-Za-z][A-Za-z ]*):\*\*\s*(.*)$")
CRITERIA_RE = re.compile(r"^\*\*Acceptance criteria\*\*\s*$")
CHECKBOX_RE = re.compile(r"^-\s+\[[ xX]\]\s+(.+)$")
SECTION_RE = re.compile(r"^##\s+")


class Item:
    """One work item: one issue, one branch, one reviewable pull request."""

    def __init__(self, ident, title, line):
        self.id = ident
        self.title = title
        self.line = line
        self.meta = {}
        self.description = []
        self.criteria = []

    def ids(self, field):
        raw = self.meta.get(field, "").strip()
        if raw in ("", "—", "-", "none", "None"):
            return []
        return [t.strip() for t in raw.replace(";", ",").split(",") if t.strip()]

    def labels(self):
        return self.ids("Labels")

    @property
    def size(self):
        return self.meta.get("Size", "").strip().upper()

    def body(self, plan_path, design_link):
        """The issue body: description, criteria, and where it came from."""
        lines = ["\n".join(self.description).strip(), ""]
        if self.criteria:
            lines.append("### Acceptance criteria")
            lines.append("")
            lines.extend(f"- [ ] {c}" for c in self.criteria)
            lines.append("")
        lines.append("---")
        lines.append("")
        if design_link:
            lines.append(f"Design: {design_link} {self.meta.get('Design', '').strip()}")
        lines.append(f"Work plan: `{plan_path}` {self.id}")
        scenarios = self.meta.get("Scenarios", "").strip()
        if scenarios and scenarios != "—":
            lines.append(f"Test scenarios: {scenarios}")
        return "\n".join(lines).rstrip() + "\n"


def parse(path):
    """Reads the work items and the design back-link out of a work plan."""
    items, current, in_criteria = [], None, False
    design_link = ""
    for number, raw in enumerate(path.read_text().splitlines(), start=1):
        line = raw.rstrip()

        if not design_link:
            match = re.search(r"Related design:\s*\[`([^`]+)`\]", line)
            if match:
                design_link = match.group(1)

        item_match = ITEM_RE.match(line)
        if item_match:
            current = Item(item_match.group(1), item_match.group(2), number)
            items.append(current)
            in_criteria = False
            continue

        if current is None:
            continue

        # A new top-level section closes the item list.
        if SECTION_RE.match(line):
            current = None
            continue

        if CRITERIA_RE.match(line):
            in_criteria = True
            continue

        meta_match = META_RE.match(line)
        if meta_match and not in_criteria:
            current.meta[meta_match.group(1)] = meta_match.group(2)
            continue

        if in_criteria:
            box = CHECKBOX_RE.match(line)
            if box:
                current.criteria.append(box.group(1).strip())
            continue

        if line.strip() and not line.startswith("<!--"):
            current.description.append(line)

    return items, design_link


def validate(items):
    """Every way a plan can be inconsistent with itself, in one pass."""
    errors, warnings = [], []
    by_id = {}
    for item in items:
        if item.id in by_id:
            errors.append(f"{item.id}: duplicate ID, first seen at line {by_id[item.id].line}")
        by_id[item.id] = item

    for item in items:
        where = f"{item.id} (line {item.line})"

        for field in ("Depends on", "Labels", "Design", "Scenarios", "Size"):
            if field not in item.meta:
                errors.append(f"{where}: missing **{field}:**")

        for field in ("Depends on", "Conflicts with"):
            for dep in item.ids(field):
                if dep == item.id:
                    errors.append(f"{where}: {field.lower()} itself")
                elif dep not in by_id:
                    errors.append(f"{where}: {field.lower()} unknown item {dep}")

        labels = item.labels()
        for label in labels:
            if label not in KNOWN_LABELS:
                errors.append(f"{where}: unknown label '{label}'")
        for group, name in (
            (COMPONENT_LABELS, "component"),
            (TYPE_LABELS, "type"),
            (PRIORITY_LABELS, "priority"),
        ):
            hits = [lab for lab in labels if lab in group]
            if len(hits) != 1:
                errors.append(f"{where}: needs exactly one {name} label, found {hits or 'none'}")

        if not item.criteria:
            errors.append(f"{where}: no acceptance criteria")
        if not item.description:
            errors.append(f"{where}: no description")
        if item.size not in SIZE_WEIGHT:
            errors.append(f"{where}: Size must be S, M, or L")
        elif item.size == "L":
            warnings.append(f"{where}: size L — split it before filing, or say why it cannot be split")

        if len(item.title) > 70:
            warnings.append(f"{where}: title is {len(item.title)} characters, over the 70 an issue title wants")

    cycle = find_cycle(items, by_id)
    if cycle:
        errors.append("dependency cycle: " + " → ".join(cycle))

    return errors, warnings, by_id


def find_cycle(items, by_id):
    """Depth-first search over `Depends on`, returning the first cycle found."""
    WHITE, GRAY, BLACK = 0, 1, 2
    color = {item.id: WHITE for item in items}
    stack = []

    def visit(ident):
        color[ident] = GRAY
        stack.append(ident)
        for dep in by_id[ident].ids("Depends on"):
            if dep not in by_id:
                continue
            if color[dep] == GRAY:
                return stack[stack.index(dep):] + [dep]
            if color[dep] == WHITE:
                found = visit(dep)
                if found:
                    return found
        stack.pop()
        color[ident] = BLACK
        return None

    for item in items:
        if color[item.id] == WHITE:
            found = visit(item.id)
            if found:
                return found
    return None


def waves(items, by_id):
    """Dependency levels, then pushed apart so conflicting items never share one.

    Both constraints are settled in one fixpoint, because they interact: pushing
    an item down for a conflict pushes everything that depends on it down too,
    which can create a new conflict a wave later.
    """
    level = {}
    remaining = {item.id for item in items}
    current = 1
    while remaining:
        ready = [
            ident
            for ident in remaining
            if all(dep in level for dep in by_id[ident].ids("Depends on") if dep in by_id)
        ]
        if not ready:
            break  # a cycle, already reported by validate()
        for ident in ready:
            level[ident] = current
            remaining.discard(ident)
        current += 1

    # A conflict is ordering the mechanics require rather than correctness: the
    # later ID moves down. Its dependents follow, so both rules run to a fixpoint.
    ordered = sorted(items, key=lambda i: i.id)
    for _ in range(len(items) * len(items) + 1):
        changed = False

        for item in ordered:
            for dep in item.ids("Depends on"):
                if dep in level and level[item.id] <= level[dep]:
                    level[item.id] = level[dep] + 1
                    changed = True

        for item in ordered:
            for other in item.ids("Conflicts with"):
                if other not in level or other not in by_id:
                    continue
                if level[item.id] == level[other]:
                    level[max(item.id, other)] += 1
                    changed = True

        if not changed:
            break

    grouped = {}
    for ident, lvl in level.items():
        grouped.setdefault(lvl, []).append(ident)

    # Levels can end up sparse after the shifts, so waves are renumbered here.
    return {
        wave: sorted(grouped[lvl])
        for wave, lvl in enumerate(sorted(grouped), start=1)
    }


def critical_path(items, by_id):
    """The longest dependency chain, by item count and by size weight."""
    memo = {}

    def longest(ident):
        if ident in memo:
            return memo[ident]
        deps = [d for d in by_id[ident].ids("Depends on") if d in by_id]
        best = []
        for dep in deps:
            chain = longest(dep)
            if len(chain) > len(best):
                best = chain
        memo[ident] = best + [ident]
        return memo[ident]

    paths = [longest(item.id) for item in items]
    if not paths:
        return [], 0
    best = max(paths, key=lambda p: (len(p), sum(SIZE_WEIGHT.get(by_id[i].size, 1) for i in p)))
    return best, sum(SIZE_WEIGHT.get(by_id[i].size, 1) for i in best)


PLACEHOLDER_REASON = "<why these two cannot be in flight together>"


def read_reasons(text):
    """The Reason cells a writer already filled in, keyed by the item pair."""
    found = {}
    for match in re.finditer(r"^\|\s*(W-\d+),\s*(W-\d+)\s*\|\s*(.+?)\s*\|$", text, re.M):
        reason = match.group(3).strip()
        if reason and reason != PLACEHOLDER_REASON and not reason.startswith("---"):
            found[tuple(sorted((match.group(1), match.group(2))))] = reason
    return found


def render_section(items, by_id, reasons=None):
    """Section 1, exactly as it belongs in the document."""
    reasons = reasons or {}
    computed = waves(items, by_id)
    out = ["## 1. Execution Order", ""]
    out.append("<!-- Generated. Run `.claude/skills/work-plan/scripts/check-work-plan.py")
    out.append("     <this file> --write` to refresh this section, and never edit the")
    out.append("     three tables by hand. -->")
    out += ["", "### Waves", "", "| Wave | Items | Runs after |", "|---|---|---|"]
    for level, ids in computed.items():
        after = "—" if level == 1 else f"wave {level - 1}"
        out.append(f"| {level} | {', '.join(ids)} | {after} |")

    path, weight = critical_path(items, by_id)
    out += ["", "### Critical path", ""]
    out.append(
        f"{' → '.join(path)} ({len(path)} items, weight {weight})" if path else "—"
    )

    conflicts = []
    for item in sorted(items, key=lambda i: i.id):
        for other in item.ids("Conflicts with"):
            pair = tuple(sorted((item.id, other)))
            if pair not in conflicts:
                conflicts.append(pair)
    out += ["", "### Serialization notes", ""]
    if conflicts:
        out += ["| Items | Reason |", "|---|---|"]
        for left, right in conflicts:
            reason = reasons.get((left, right), PLACEHOLDER_REASON)
            out.append(f"| {left}, {right} | {reason} |")
    else:
        out.append("No items conflict. Every wave above can run fully in parallel.")
    return "\n".join(out) + "\n"


def write_section(path, items, by_id):
    """Replaces section 1 in place, preserving any hand-written Reason cells."""
    text = path.read_text()
    match = re.search(r"^## 1\. Execution Order.*?(?=^## |\Z)", text, re.M | re.S)
    if not match:
        print("check-work-plan.py: no '## 1. Execution Order' section to write", file=sys.stderr)
        return False

    section = render_section(items, by_id, read_reasons(match.group(0)))

    trailing = "\n---\n\n" if match.group(0).rstrip().endswith("---") else "\n"
    path.write_text(text[: match.start()] + section + trailing + text[match.end():])
    return True


def emit_issues(items, by_id, plan_path, design_link):
    """A gh script in wave order, wiring real issue numbers into Blocked by."""
    computed = waves(items, by_id)
    order = [ident for ids in computed.values() for ident in ids]

    print("#!/usr/bin/env bash")
    print("#")
    print(f"# Generated by check-work-plan.py from {plan_path}.")
    print("# Read it before running it: it creates issues in the simplyblock repository,")
    print("# in wave order, substituting each created number into the items that")
    print("# depend on it.")
    print("")
    print("set -euo pipefail")
    print("declare -A ISSUE")
    print("")
    for ident in order:
        item = by_id[ident]
        deps = [d for d in item.ids("Depends on") if d in by_id]
        marker = f"{ident.replace('-', '')}_BODY"
        body = item.body(plan_path, design_link)
        if deps:
            body += "\nBlocked by: __BLOCKED_BY__\n"
        print(f"# ---------- {ident}: {item.title}")
        print(f"body=\"$(cat <<'{marker}'")
        print(body.rstrip())
        print(marker)
        print(')"')
        if deps:
            refs = " ".join(f'"#${{ISSUE[{d}]}}"' for d in deps)
            print(f"blocked=\"$(printf '%s, ' {refs} | sed 's/, $//')\"")
            print('body="${body/__BLOCKED_BY__/$blocked}"')
        print(
            f"url=\"$(gh issue create --title {shell_quote(item.title)}"
            f" --label {shell_quote(','.join(item.labels()))} --body \"$body\")\""
        )
        print(f'ISSUE[{ident}]="${{url##*/}}"')
        print(f'echo "{ident} -> $url"')
        print("")


def shell_quote(value):
    return "'" + value.replace("'", "'\\''") + "'"


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("file", type=Path)
    parser.add_argument("--waves", action="store_true", help="print the derived waves only")
    parser.add_argument("--write", action="store_true", help="rewrite section 1 from the graph")
    parser.add_argument("--issues", action="store_true", help="emit a gh issue create script")
    args = parser.parse_args()

    if not args.file.is_file():
        print(f"check-work-plan.py: no such file: {args.file}", file=sys.stderr)
        return 2

    items, design_link = parse(args.file)
    if not items:
        print("check-work-plan.py: no '### W-nn — title' items found", file=sys.stderr)
        return 2

    errors, warnings, by_id = validate(items)

    if args.issues:
        if errors:
            print("check-work-plan.py: refusing to emit issues, the plan has errors:", file=sys.stderr)
            for error in errors:
                print(f"  ERROR {error}", file=sys.stderr)
            return 1
        emit_issues(items, by_id, str(args.file), design_link)
        return 0

    if args.write:
        if errors:
            print("check-work-plan.py: refusing to write, the plan has errors:", file=sys.stderr)
            for error in errors:
                print(f"  ERROR {error}", file=sys.stderr)
            return 1
        if not write_section(args.file, items, by_id):
            return 2
        print(f"section 1 rewritten from {len(items)} items")
        return 0

    if args.waves:
        print(render_section(items, by_id, read_reasons(args.file.read_text())))
        return 1 if errors else 0

    text = args.file.read_text()
    section = render_section(items, by_id, read_reasons(text))
    print(f"{args.file}: {len(items)} work items")
    print("")
    print(section)

    declared = re.search(r"^### Waves\s*\n\s*\n((?:\|.*\n)+)", text, re.M)
    if declared:
        current = re.sub(r"\s+", " ", declared.group(1)).strip()
        expected = re.sub(
            r"\s+", " ", section.split("### Waves", 1)[1].split("###", 1)[0]
        ).strip()
        if current != expected:
            warnings.append(
                "section 1 disagrees with the declared dependencies — run --write"
            )

    for warning in warnings:
        print(f"WARN  {warning}")
    for error in errors:
        print(f"ERROR {error}")
    print("")
    print(f"{len(errors)} error(s), {len(warnings)} warning(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
