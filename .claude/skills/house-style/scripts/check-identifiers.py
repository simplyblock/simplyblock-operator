#!/usr/bin/env python3
"""Check that declared names are spelled in American English.

A name outlives the sentence around it. A field called "behaviourMode" reaches
the CRD, the Helm values, and every manifest a user writes, and a helper called
"analyseNode" is read by everyone who touches the package afterward. The house
spelling therefore applies to identifiers exactly as it applies to prose.

Only the names a file *declares* are checked, never the names it references: a
call into a dependency has to spell that dependency's name the way its owner
does. In Go that is a function, a method, a type, a var, a const, a struct field,
and the name inside a struct tag; in Python a function, a class, a parameter, and
an assignment; in YAML a mapping key, since those are the field names of a CR or
of a Helm chart.

    Before: func (r *Reconciler) analyseNode(...)   BehaviourMode string
    After:  func (r *Reconciler) analyzeNode(...)   BehaviorMode  string

The brand is checked in a name as well. "simplyblock" is one word, so a name
carries it as "simplyblock" or, at a word position inside a PascalCase or
camelCase name, as "Simplyblock". "NewsimplyBlockClient" is neither, and becomes
"NewSimplyblockClient".

The names are split into words before they are matched, so camelCase,
PascalCase, snake_case, SCREAMING_SNAKE_CASE, and kebab-case are all read the
same way, and an acronym stays whole: "parseHTTPColourCode" is
"parse", "HTTP", "Colour", "Code".

There is no --fix. Renaming an identifier means renaming every reference to it,
which a line-based rewrite cannot do; an exported name is an API change on top.
Use a refactoring tool ("gopls rename", the IDE's rename) and review the result.

The word list is the one of check-american-english.py, so a spelling is added in
one place and both gates pick it up.

Usage:
    python3 .claude/skills/house-style/scripts/check-identifiers.py [PATH ...]
"""

import argparse
import importlib.util
import os
import re
import sys

from markdown_common import (
    SEVERITY_ERROR,
    Violation,
    code_source_lines,
    collect_files,
    drop_generated,
    get_line_excerpt,
    read_lines,
    relative_path,
    report_violations,
    syntax_of,
)

CHECK_NAME = "identifiers"

# This gate reads code, so its defaults are the source trees rather than the
# documentation directory the prose gates default to.
DEFAULT_CODE_DIRS = [
    "operator", "csi-driver", "atlas-lib", "shared", "scripts", "test",
    "helm-charts",
]


def _load_spellings():
    """Import the word list from check-american-english.py.

    The file name carries hyphens, so it cannot be imported by name.
    """
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-american-english.py")
    spec = importlib.util.spec_from_file_location("american_english_check", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.SPELLINGS, module.matching_case


SPELLINGS, matching_case = _load_spellings()

# Words that keep a British spelling because they are part of a proper name.
EXEMPT_WORDS = {"fibre"}

# The brand is one word. Inside a name it is written all lowercase, or with a
# leading capital where it sits at a word position of a camelCase or PascalCase
# name. An all-caps name (a SCREAMING_SNAKE_CASE constant) carries it all-caps.
BRAND = "simplyblock"
BRAND_PATTERN = re.compile(BRAND, re.IGNORECASE)
BRAND_REASON = (
    "Brand casing '{found}' in the declared name '{name}', the brand is one word: "
    "'{expected}'{caveat}"
)
ALL_CAPS_NAME_PATTERN = re.compile(r"^[A-Z0-9_]+$")


def expected_brand(name, start):
    """How the brand has to be cased at this position of this name.

    All-caps in a SCREAMING_SNAKE_CASE name. A capital where the brand opens a
    word inside a camelCase or PascalCase name, since that is where a word
    starts. All lowercase where it opens the name itself, or follows a "_", a
    "-", or a ".".
    """
    if ALL_CAPS_NAME_PATTERN.match(name):
        return BRAND.upper()
    if start == 0:
        return BRAND.capitalize() if name[:1].isupper() else BRAND
    previous = name[start - 1]
    if previous in "_-.":
        return BRAND
    # Preceded by a letter or a digit: this is a word boundary in a camel name.
    return BRAND.capitalize()


def brand_findings(name, name_column, line, line_number, rel, syntax):
    """Report every brand occurrence in a name that is cased another way."""
    violations = []
    for match in BRAND_PATTERN.finditer(name):
        found = match.group(0)
        expected = expected_brand(name, match.start())
        if found == expected:
            continue
        violations.append(
            Violation(
                file=rel,
                line=line_number,
                column=name_column + match.start() + 1,
                check=CHECK_NAME,
                reason=BRAND_REASON.format(
                    found=found,
                    name=name,
                    expected=expected,
                    caveat=caveat_for(name, syntax),
                ),
                excerpt=get_line_excerpt(line, name_column),
                severity=SEVERITY_ERROR,
            )
        )
    return violations

# ─── Where a name is declared ──────────────────────────────────────────────
#
# Every pattern captures the declared name in group "name". A pattern that would
# also match a reference is not listed: a struct literal key, a call, and a
# selector all name something that was declared elsewhere.

GO_PATTERNS = (
    # func Name(, func (r *T) Name(
    re.compile(r"^\s*func\s+(?:\([^)]*\)\s*)?(?P<name>[A-Za-z_]\w*)\s*[\(\[]"),
    # type Name struct|interface|=|...
    re.compile(r"^\s*type\s+(?P<name>[A-Za-z_]\w*)\s"),
    # var Name Type, const Name = ..., outside a declaration block
    re.compile(r"^\s*(?:var|const)\s+(?P<name>[A-Za-z_]\w*)\s*[\w\[\]\*\.=]"),
    # Name := ..., Name, err := ...
    re.compile(r"^\s*(?P<name>[a-zA-Z_]\w*)\s*(?:,\s*[a-zA-Z_]\w*\s*)*:="),
)

# Inside a "type X struct { ... }" body: "Name Type" and "Name Type `tag`".
GO_FIELD_PATTERN = re.compile(
    r"^\s*(?P<name>[A-Za-z_]\w*)\s+(?:\*|\[\]|map\[|chan\s|func\s*\(|<-)?[\w\.\[\]\*]+"
)
# Inside a "var (" or "const (" block: "Name Type = value" and "Name = value".
GO_BLOCK_MEMBER_PATTERN = re.compile(r"^\s*(?P<name>[A-Za-z_]\w*)\s*(?:[\w\.\[\]\*]+\s*)?=")
GO_STRUCT_OPEN_PATTERN = re.compile(r"^\s*type\s+\w+\s+(?:struct|interface)\s*\{")
GO_DECL_BLOCK_OPEN_PATTERN = re.compile(r"^\s*(?:var|const)\s*\(\s*$")
# The name a struct tag publishes: json:"behaviorMode,omitempty".
GO_TAG_PATTERN = re.compile(r"(?:json|yaml):\"(?P<name>[A-Za-z_][\w-]*)")

PYTHON_PATTERNS = (
    re.compile(r"^\s*(?:async\s+)?def\s+(?P<name>[A-Za-z_]\w*)\s*\("),
    re.compile(r"^\s*class\s+(?P<name>[A-Za-z_]\w*)\s*[\(:]"),
    re.compile(r"^\s*self\.(?P<name>[A-Za-z_]\w*)\s*(?::[^=]+)?="),
    re.compile(r"^\s*(?P<name>[A-Za-z_]\w*)\s*(?::[^=]+)?=(?!=)"),
)
PYTHON_PARAMETER_PATTERN = re.compile(r"[\(,]\s*(?P<name>[A-Za-z_]\w*)\s*(?=[,:=\)])")
PYTHON_DEF_PATTERN = re.compile(r"^\s*(?:async\s+)?def\s")

# A mapping key: the field name of a CR, of a Helm value, or of a workflow step.
YAML_KEY_PATTERN = re.compile(r"^\s*(?:-\s+)?(?P<name>[A-Za-z_][\w.-]*)\s*:(?:\s|$)")

WORD_PATTERN = re.compile(r"[A-Z]+(?=[A-Z][a-z])|[A-Z]?[a-z]+|[A-Z]+|\d+")
SEPARATOR_PATTERN = re.compile(r"[_\-.]+")

REASON = (
    "British spelling '{found}' in the declared name '{name}', names are American "
    "English too: '{expected}'{caveat}"
)
# Renaming a name that something outside the file depends on is more than a
# rename, so the finding says so.
CAVEAT_OF = {
    "go": " (exported, so the rename is an API change)",
    "yaml": " (a manifest key, so the rename changes the API)",
}


def words_of(identifier):
    """Yield every word of an identifier with its offset inside the name."""
    offset = 0
    for chunk in SEPARATOR_PATTERN.split(identifier):
        for match in WORD_PATTERN.finditer(chunk):
            yield match.group(0), offset + match.start()
        offset += len(chunk) + 1


def caveat_for(name, syntax):
    """The note appended when a rename reaches beyond the file."""
    if syntax == "go" and name[:1].isupper():
        return CAVEAT_OF["go"]
    if syntax == "yaml":
        return CAVEAT_OF["yaml"]
    return ""


def findings_for(name, name_column, line, line_number, rel, syntax):
    """Report every British word inside one declared name."""
    violations = []
    for word, offset in words_of(name):
        lowered = word.lower()
        if lowered in EXEMPT_WORDS or lowered not in SPELLINGS:
            continue
        expected = matching_case(word, SPELLINGS[lowered])
        caveat = caveat_for(name, syntax)
        violations.append(
            Violation(
                file=rel,
                line=line_number,
                column=name_column + offset + 1,
                check=CHECK_NAME,
                reason=REASON.format(
                    found=word, name=name, expected=expected, caveat=caveat
                ),
                excerpt=get_line_excerpt(line, name_column),
                severity=SEVERITY_ERROR,
            )
        )
    return violations


def declared_names_go(lines):
    """Yield (line_index, column, name) for every name a Go file declares."""
    struct_depth = 0
    in_decl_block = False

    for index, line in enumerate(lines):
        if in_decl_block:
            if re.match(r"^\s*\)\s*$", line):
                in_decl_block = False
            else:
                match = GO_BLOCK_MEMBER_PATTERN.match(line)
                if match:
                    yield index, match.start("name"), match.group("name")
                continue

        if struct_depth > 0:
            struct_depth += line.count("{") - line.count("}")
            match = GO_FIELD_PATTERN.match(line)
            if match and match.group("name") not in ("struct", "interface", "func", "return"):
                yield index, match.start("name"), match.group("name")
            for tag in GO_TAG_PATTERN.finditer(line):
                yield index, tag.start("name"), tag.group("name")
            continue

        if GO_STRUCT_OPEN_PATTERN.match(line):
            struct_depth = 1 + line.count("{") - 1 - line.count("}")
            type_match = re.match(r"^\s*type\s+(?P<name>\w+)\s", line)
            if type_match:
                yield index, type_match.start("name"), type_match.group("name")
            continue

        if GO_DECL_BLOCK_OPEN_PATTERN.match(line):
            in_decl_block = True
            continue

        for pattern in GO_PATTERNS:
            match = pattern.match(line)
            if match:
                yield index, match.start("name"), match.group("name")
                break
        for tag in GO_TAG_PATTERN.finditer(line):
            yield index, tag.start("name"), tag.group("name")


def declared_names_python(lines):
    for index, line in enumerate(lines):
        for pattern in PYTHON_PATTERNS:
            match = pattern.match(line)
            if match:
                yield index, match.start("name"), match.group("name")
                break
        if PYTHON_DEF_PATTERN.match(line):
            for match in PYTHON_PARAMETER_PATTERN.finditer(line):
                if match.group("name") not in ("self", "cls"):
                    yield index, match.start("name"), match.group("name")


def declared_names_yaml(lines):
    for index, line in enumerate(lines):
        match = YAML_KEY_PATTERN.match(line)
        if match:
            yield index, match.start("name"), match.group("name")


DECLARATION_READERS = {
    "go": declared_names_go,
    "python": declared_names_python,
    "yaml": declared_names_yaml,
}


def scan_file(file_path):
    syntax = syntax_of(file_path)
    reader = DECLARATION_READERS.get(syntax)
    if reader is None:
        return []

    source = read_lines(file_path)
    code = code_source_lines(file_path, source)
    rel = relative_path(file_path)

    violations = []
    seen = set()
    for index, column, name in reader(code):
        key = (index, column, name)
        if key in seen:
            continue
        seen.add(key)
        violations.extend(
            findings_for(name, column, source[index], index + 1, rel, syntax)
        )
        violations.extend(
            brand_findings(name, column, source[index], index + 1, rel, syntax)
        )
    return violations


def report_generated(generated):
    print(f"Skipped {len(generated)} generated file(s), fix those at their source:")
    for file in generated:
        print(f"  • {relative_path(file)}")


def main():
    parser = argparse.ArgumentParser(
        description="Check that declared names are spelled in American English."
    )
    parser.add_argument(
        "paths",
        nargs="*",
        help=(
            "directories or files to scan, recursively "
            f"(default: {', '.join(DEFAULT_CODE_DIRS)} in the repository root)"
        ),
    )
    args = parser.parse_args()

    files = collect_files(
        args.paths,
        default_dirs=DEFAULT_CODE_DIRS,
        on_missing=lambda target: print(f"Skipping missing path: {target}", file=sys.stderr),
    )
    files = [file for file in files if syntax_of(file) != "markdown"]
    files = drop_generated(files, report=report_generated)

    violations = [v for file in files for v in scan_file(file)]

    sys.exit(
        report_violations(
            violations,
            "identifier check",
            files,
            "No British spellings or brand casing errors in the declared names "
            "of {files} file(s).",
        )
    )


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as error:  # noqa: BLE001
        print("Failed to run identifier check.", file=sys.stderr)
        print(error, file=sys.stderr)
        sys.exit(1)
