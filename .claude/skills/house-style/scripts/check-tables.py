#!/usr/bin/env python3
"""Checks the Markdown tables of the design documents and the test plans.

Ported from the simplyblock documentation repository, where these two rules live
in scripts/check-mkdocs-syntax.py as "table-format" and "table-separator". Only
the table rules are ported here: the rest of that checker is about mkdocs
constructs this repository's documents do not use. The check names, the messages,
and the alignment the fix produces are kept identical, so a finding reads the same
in both repositories and a document can move between them.

A table is written with its pipes lined up under each other. The rendered page
looks the same either way; the source does not. A column that shifts by a
character on every row cannot be read, and a diff that touches one cell rewrites
the whole block. A table whose header carries no separator row is worse than
crooked: every row runs together into one paragraph of pipes.

"--fix" re-aligns the tables, which is the finding with only one possible answer.
"""

import argparse
import sys

from markdown_common import (
    CODE_FENCE_PATTERN,
    FileFix,
    TABLE_ROW_PATTERN,
    TABLE_SEPARATOR_PATTERN,
    Violation,
    apply_fixes_to_file,
    collect_files,
    drop_generated,
    read_lines,
    relative_path,
    report_violations,
)


def split_table_row(line):
    """Split a table row into its cells.

    A pipe inside an inline code span or behind a backslash belongs to the cell,
    not to the table, so the row is walked instead of split with a regex.
    """
    text = line.strip()
    if text.startswith("|"):
        text = text[1:]
    if text.endswith("|") and not text.endswith("\\|"):
        text = text[:-1]

    cells, current, in_code = [], [], False
    index = 0
    while index < len(text):
        char = text[index]
        if char == "\\" and index + 1 < len(text):
            current.append(text[index:index + 2])
            index += 2
            continue
        if char == "`":
            in_code = not in_code
        if char == "|" and not in_code:
            cells.append("".join(current).strip())
            current = []
        else:
            current.append(char)
        index += 1
    cells.append("".join(current).strip())
    return cells


def format_table(rows):
    """Render a table with one space of padding and its pipes lined up."""
    indent = rows[0][: len(rows[0]) - len(rows[0].lstrip())]
    grid = [split_table_row(row) for row in rows]
    columns = max(len(cells) for cells in grid)
    grid = [cells + [""] * (columns - len(cells)) for cells in grid]

    # The separator row carries the alignment and not the content, so its own
    # dashes are left out of the width.
    widths = [
        max(len(cells[column]) for index, cells in enumerate(grid) if index != 1)
        for column in range(columns)
    ]
    widths = [max(width, 3) for width in widths]

    formatted = []
    for index, cells in enumerate(grid):
        if index == 1:
            rendered = []
            for column, cell in enumerate(cells):
                left, right = cell.startswith(":"), cell.endswith(":")
                dashes = "-" * (widths[column] + 2)
                if left:
                    dashes = ":" + dashes[1:]
                if right:
                    dashes = dashes[:-1] + ":"
                rendered.append(dashes)
            formatted.append(indent + "|" + "|".join(rendered) + "|")
        else:
            rendered = [
                f" {cell.ljust(widths[column])} " for column, cell in enumerate(cells)
            ]
            formatted.append(indent + "|" + "|".join(rendered) + "|")
    return formatted


def check_table_formatting(lines, rel):
    """A table is written with its pipes lined up under each other.

    The rendered page looks the same either way. The source does not: a column
    that shifts by a character on every row cannot be read, and a diff that
    touches one cell rewrites the whole block.
    """
    violations = []
    fixes = []
    in_code_fence = False
    block, start = [], 0

    def flush():
        if len(block) < 2 or not TABLE_SEPARATOR_PATTERN.match(block[1]):
            return
        for offset, (original, wanted) in enumerate(zip(block, format_table(block))):
            if original.rstrip() == wanted:
                continue
            violations.append(
                Violation(
                    file=rel,
                    line=start + offset,
                    column=1,
                    check="table-format",
                    reason="Table row is not aligned with the rest of its table",
                    excerpt=original.strip()[:80],
                )
            )
            fixes.append(
                FileFix(
                    line=start + offset,
                    column=1,
                    length=len(original),
                    replacement=wanted,
                )
            )

    for index, line in enumerate(lines):
        if CODE_FENCE_PATTERN.match(line):
            in_code_fence = not in_code_fence
            flush()
            block = []
            continue
        if in_code_fence:
            continue
        if line.strip().startswith("|"):
            if not block:
                start = index + 1
            block.append(line)
        else:
            flush()
            block = []
    flush()

    return violations, fixes


def check_table_separator(lines, rel):
    """A table needs its separator row under the header.

    Without it nothing renders as a table: every row runs together into one
    paragraph of pipes, which is why this is not a formatting finding.
    """
    violations = []
    in_code_fence = False

    for index, line in enumerate(lines):
        if CODE_FENCE_PATTERN.match(line):
            in_code_fence = not in_code_fence
            continue
        if in_code_fence:
            continue
        if TABLE_ROW_PATTERN.match(line) and index + 1 < len(lines):
            previous = lines[index - 1] if index else ""
            if not TABLE_ROW_PATTERN.match(previous) and not TABLE_SEPARATOR_PATTERN.match(
                lines[index + 1]
            ):
                violations.append(
                    Violation(
                        file=rel,
                        line=index + 1,
                        column=1,
                        check="table-separator",
                        reason="Table without a separator row below its header",
                        excerpt=line.strip()[:80],
                    )
                )

    return violations


def scan_file(file_path):
    """Return the violations of a file and the fixes that would repair them."""
    lines = read_lines(file_path)
    rel = relative_path(file_path)
    violations, fixes = check_table_formatting(lines, rel)
    violations += check_table_separator(lines, rel)
    return violations, fixes


def main():
    parser = argparse.ArgumentParser(
        description="Check that Markdown tables are aligned and carry a separator row."
    )
    parser.add_argument("paths", nargs="*", help="Files or directories to check.")
    parser.add_argument(
        "--fix",
        action="store_true",
        help="re-align the tables, the only finding with one possible answer",
    )
    args = parser.parse_args()

    files = collect_files(
        args.paths,
        on_missing=lambda target: print(
            f"Skipping missing path: {target}", file=sys.stderr
        ),
    )
    # A table is a Markdown construct. A pipe in a Go or YAML comment is a pipe.
    files = [file for file in files if file.endswith(".md")]
    files = drop_generated(files)

    scans = {file: scan_file(file) for file in files}

    if args.fix:
        files_changed = 0
        applied = 0
        for file, (_, fixes) in scans.items():
            written = apply_fixes_to_file(file, fixes)
            if written:
                files_changed += 1
                applied += written
        if applied:
            print(
                f"Auto-fix mode: re-aligned {applied} table row(s) "
                f"across {files_changed} file(s)."
            )
        scans = {file: scan_file(file) for file in files}

    violations = [v for violations, _ in scans.values() for v in violations]

    sys.exit(
        report_violations(
            violations,
            "Table check",
            files,
            "Every table is aligned and carries its separator row "
            "in {files} file(s).",
        )
    )


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as error:  # noqa: BLE001
        print("Failed to run table check.", file=sys.stderr)
        print(error, file=sys.stderr)
        sys.exit(1)
