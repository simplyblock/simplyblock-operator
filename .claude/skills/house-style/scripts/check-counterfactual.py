#!/usr/bin/env python3
"""Check that a document states its design instead of arguing against alternatives.

A design document is reference material. It says what the system does and why,
and a reader arriving cold has never seen the alternatives that were weighed on
the way there. Prose that compares the design against one of those alternatives
carries the discussion into the page, and the reader has to reconstruct the
missing half to parse the sentence.

    Before: "The refusal is admission, not a step."
    After:  "An operation naming an external control plane is rejected at creation."

    Before: "Folding it into the same phase would report an outage for a partial
             failure."
    After:  "That is a partial failure, and Degraded is the phase for it."

Three shapes are reported.

Counterfactuals describe a system that was not built ("would report", "would be",
"an implementation that", "a design that"). State the constraint that makes the
alternative wrong instead of narrating its behavior.

Rebuttals lead with what the design is not ("X, not Y", "is not a", "rather than
merely"). The corpus voice is "X is Y because Z", and the negation belongs in a
Non-Goal or an Open Question when it has to be recorded at all.

Absence statements lead with what the design does not have ("carries no field",
"takes no parameter"). One earns its place where a reader expects the thing from
the system, as every other Ops kind naming its target makes "OperatorOps carries
no target reference" worth writing. One that answers an expectation created by an
earlier draft says nothing to a reader who never saw it.

Evolution notes describe the document rather than the system ("originally", "used
to", "no longer", "still carries", "survives"). The date line and git carry that.

A rejected alternative earns a place in the body only when a reader would
otherwise propose it again, and then it is written as a decision with its reason.
Since that exception is real, every finding here is a warning rather than an
error: it is a sentence to justify or rewrite, not a rule that cannot be broken.

Only Markdown prose is checked. Code blocks, inline code, and link targets are
skipped, as are the instruction directories, whose files address whoever follows
them.
"""

import argparse
import os
import re
import sys

from markdown_common import (
    DEFAULT_TARGET_DIRS,
    SEVERITY_WARNING,
    Violation,
    collect_files,
    drop_generated,
    get_line_excerpt,
    iter_prose_lines,
    prose_source_lines,
    syntax_of,
    relative_path,
    report_violations,
)

CHECK_NAME = "counterfactual"

INSTRUCTION_DIRS = (".claude", ".github")

# Each pattern names the shape it catches, so the finding says what to do.
PATTERNS = (
    (
        "counterfactual",
        re.compile(
            r"\b(?:would (?:be|have|report|produce|make|mean|leave|let|turn|give|"
            r"send|ask|put|read|say|show|stop|start|keep|lose|need|require|arrive|"
            r"return|accept|reject|fail|break|hide|warn|halt|cost|buy)\b"
            r"|an implementation that\b|a design that\b|a controller that\b"
            r"|a version that would\b|somebody who wanted\b)",
            re.IGNORECASE,
        ),
        "describes a system that was not built; state the constraint instead",
    ),
    (
        "rebuttal",
        re.compile(
            r"(?:\*\*[^*]{0,120}?,\s+not\s+(?:a|an|the|its|his|her|their|merely)\b"
            # A bolded lead-in negating a noun needs no comma to be a rebuttal:
            # "**Recreation is not the answer**" reads the same way.
            r"|\*\*[^*]{0,140}?\bis not (?:a|an|the|its|their)\s+\w+"
            r"|\bis not the (?:answer|fix|point|reason)\b"
            r"|\bis not a reason\b"
            # A concession to the alternative before dismissing it.
            r"|\beven though it (?:works|would)\b"
            # Section titles carry the shape too, and a title survives every
            # paragraph-level read: "Why a Kind and Not a List", "X, Not Y".
            r"|^#{2,6}\s.*(?:\bwhy\b.*\bnot\b|,\s+not\s+)"
            r"|\brather than merely\b"
            r"|\bis not a (?:gap|defect|wart|missing|limitation|tolerance)\b"
            r"|\bnot a flag day\b)",
            re.IGNORECASE | re.MULTILINE,
        ),
        "leads with what the design is not; write it as what it is",
    ),
    (
        "absence",
        re.compile(
            r"\*\*[^*]{0,140}?\b(?:carries no|contains no|takes no|has no field|"
            r"is not a field|needs no field|no longer carries|there is no field)\b",
            re.IGNORECASE,
        ),
        "leads with what is absent; state an absence only where the reader expects "
        "it from the system, never from this document's own history",
    ),
    (
        "evolution",
        re.compile(
            r"\b(?:originally|used to (?:be|say|carry|have)|no longer (?:says|carries|has)"
            r"|we considered|was considered and|previously (?:said|carried)"
            r"|survives on\b|under a new name)\b",
            re.IGNORECASE,
        ),
        "describes the document's history; the date line and git carry that",
    ),
)

REASON = "{shape}: '{found}' — {advice}"


def is_instruction(file_path):
    parts = os.path.normpath(file_path).split(os.sep)
    return any(part in INSTRUCTION_DIRS for part in parts)


def scan_file(file_path):
    lines = prose_source_lines(file_path)
    rel = relative_path(file_path)
    violations = []

    for prose in iter_prose_lines(lines):
        for shape, pattern, advice in PATTERNS:
            for match in pattern.finditer(prose.masked):
                violations.append(
                    Violation(
                        file=rel,
                        line=prose.number,
                        column=match.start() + 1,
                        check=CHECK_NAME,
                        reason=REASON.format(
                            shape=shape, found=match.group(0).strip(), advice=advice
                        ),
                        excerpt=get_line_excerpt(prose.text, match.start()),
                        severity=SEVERITY_WARNING,
                    )
                )

    return violations


def main():
    parser = argparse.ArgumentParser(
        description="Check that a document states its design rather than arguing against alternatives."
    )
    parser.add_argument(
        "paths",
        nargs="*",
        help=(
            "directories or files to scan, recursively "
            f"(default: {', '.join(DEFAULT_TARGET_DIRS)} in the repository root)"
        ),
    )
    args = parser.parse_args()

    files = collect_files(
        args.paths,
        on_missing=lambda target: print(f"Skipping missing path: {target}", file=sys.stderr),
    )
    files = drop_generated(files)
    files = [f for f in files if syntax_of(f) == "markdown" and not is_instruction(f)]

    violations = [v for file in files for v in scan_file(file)]

    # Every finding is a sentence to justify or rewrite rather than a rule that
    # cannot be broken, so the check reports and does not fail the gate.
    report_violations(
        violations,
        "counterfactual check",
        files,
        "Reviewed {files} file(s), no argued-against alternatives.",
        group_warnings=False,
    )
    # Every finding is a sentence to justify or rewrite rather than a rule that
    # cannot be broken, so the check reports and does not fail the gate.
    sys.exit(0)


if __name__ == "__main__":
    main()
