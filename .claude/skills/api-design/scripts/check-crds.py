#!/usr/bin/env python3
#
# Audits the CRD types in operator/api/v1alpha1 for the conventions the
# api-design skill states, and reports the marker adoption those conventions are
# measured against.
#
# It exists because the conventions are checkable and were not being checked. The
# skill's own adoption numbers went stale within weeks of being written: short
# names were documented as absent while ten types carried one. The numbers are
# read from the code here instead of maintained by hand, and --adoption prints
# the table that used to live in references/markers.md.
#
# The checks are the ones a reviewer cannot reliably do by eye across 17 types:
# whether a doc comment claiming immutability is backed by one of the three
# markers that actually reach the schema, whether a phase is a named type,
# whether a conditions list carries the markers that make server-side apply merge
# it, and whether the type-level marker set is complete.
#
# Usage:
#   check-crds.py                       audit every type: the repository's backlog
#   check-crds.py --changed             only the type files this change touched
#   check-crds.py --kind StorageNodeOps audit one kind
#   check-crds.py --adoption            the marker adoption table
#   check-crds.py --strict              treat warnings as errors too
#
# Exit status is 1 when an error was reported, so it can gate an API change.

import argparse
import re
import sys
from collections import Counter
from pathlib import Path

API_DIR = "operator/api/v1alpha1"

MARKER_RE = re.compile(r"^\s*//\s*(\+.*)$")
COMMENT_RE = re.compile(r"^\s*//\s?(.*)$")
TYPE_STRUCT_RE = re.compile(r"^type\s+(\w+)\s+struct\s*\{")
TYPE_ALIAS_RE = re.compile(r"^type\s+(\w+)\s+(\w[\w./\[\]]*)\s*$")
FIELD_RE = re.compile(r"^\t(\w+)\s+([^\s`]+(?:\s+[^\s`]+)*?)\s*`([^`]*)`")
EMBEDDED_RE = re.compile(r"^\t([\w.]+)\s*`([^`]*)`")
CONST_RE = re.compile(r"^\s*(\w+)\s+(\w+)\s*=\s*\"")
JSON_TAG_RE = re.compile(r'json:"([^",]*)')

# Markers that make a conditions list merge by type under server-side apply
# rather than being replaced wholesale.
CONDITION_MARKERS = ("listType=map", "listMapKey=type")


class Field:
    def __init__(self, name, gotype, tag, line, block):
        self.name = name
        self.gotype = gotype
        self.raw_tag = tag
        self.json = (JSON_TAG_RE.search(tag).group(1) if JSON_TAG_RE.search(tag) else "")
        self.line = line
        self.markers = [b for b in block if b.startswith("+")]
        self.doc = " ".join(b for b in block if not b.startswith("+"))

    def has(self, needle):
        return any(needle in m for m in self.markers)


class Struct:
    def __init__(self, name, line, block):
        self.name = name
        self.line = line
        self.markers = [b for b in block if b.startswith("+")]
        self.doc = " ".join(b for b in block if not b.startswith("+"))
        self.fields = []

    def has(self, needle):
        return any(needle in m for m in self.markers)

    def field(self, name):
        for f in self.fields:
            if f.name == name:
                return f
        return None


class Alias:
    """A named type, which for a closed set is a string type with constants."""

    def __init__(self, name, base, line, block):
        self.name = name
        self.base = base
        self.line = line
        self.markers = [b for b in block if b.startswith("+")]
        self.constants = 0

    def has(self, needle):
        return any(needle in m for m in self.markers)


def parse(path):
    """Structs, named types, and their attached comment and marker blocks."""
    structs, aliases, block, current = {}, {}, [], None
    lines = path.read_text().splitlines()

    for number, raw in enumerate(lines, start=1):
        marker = MARKER_RE.match(raw)
        if marker:
            block.append(marker.group(1).strip())
            continue
        comment = COMMENT_RE.match(raw)
        if comment:
            block.append(comment.group(1).strip())
            continue

        struct = TYPE_STRUCT_RE.match(raw)
        if struct:
            current = Struct(struct.group(1), number, block)
            structs[current.name] = current
            block = []
            continue

        alias = TYPE_ALIAS_RE.match(raw)
        if alias:
            aliases[alias.group(1)] = Alias(alias.group(1), alias.group(2), number, block)
            block = []
            current = None
            continue

        if raw.startswith("}"):
            current = None
            block = []
            continue

        if current is not None:
            field = FIELD_RE.match(raw)
            if field:
                current.fields.append(
                    Field(field.group(1), field.group(2), field.group(3), number, block)
                )
                block = []
                continue
            embedded = EMBEDDED_RE.match(raw)
            if embedded:
                current.fields.append(
                    Field(embedded.group(1), embedded.group(1), embedded.group(2), number, block)
                )
                block = []
                continue

        if raw.strip():
            const = CONST_RE.match(raw)
            if const and const.group(2) in aliases:
                aliases[const.group(2)].constants += 1
            block = []

    return structs, aliases


def audit_kind(kind, structs, aliases, path):
    """Every finding for one root kind, as (level, rule, line, message)."""
    found = []
    root = structs[kind]
    spec = structs.get(f"{kind}Spec")
    status = structs.get(f"{kind}Status")
    is_ops = kind.endswith("Ops")

    def error(rule, line, message):
        found.append(("ERROR", rule, line, message))

    def warn(rule, line, message):
        found.append(("WARN", rule, line, message))

    # ---- type-level markers
    if not root.has("subresource:status"):
        error("no-status-subresource", root.line,
              "no +kubebuilder:subresource:status, so a status write needs spec RBAC "
              "and bumps generation")
    if f"{kind}List" not in structs:
        error("no-list-type", root.line, f"no {kind}List companion type")
    if not root.has("shortName"):
        warn("no-shortname", root.line,
             "no shortName. 10 of 17 types carry one; a design that names one and a "
             "type that does not declare it is a test scenario that cannot pass")

    columns = [m for m in root.markers if "printcolumn" in m]
    if len(columns) < 3:
        warn("thin-printcolumns", root.line,
             f"{len(columns)} printcolumns; two or three answer \"what is happening\" "
             "without a describe")
    if columns and not any("creationTimestamp" in m for m in columns):
        warn("no-age-column", root.line, "no Age printcolumn")

    # ---- status conventions
    if status is not None:
        phase = status.field("Phase")
        conditions = status.field("Conditions")

        if status.field("ObservedGeneration") is None:
            warn("no-observed-generation", status.line,
                 "status has no ObservedGeneration, so a stale status cannot be told "
                 "from a current one. See reconciler-patterns")
        if phase is None and conditions is None:
            warn("no-phase-no-conditions", status.line,
                 "status reports neither a phase nor conditions")
        if phase is not None and phase.gotype == "string":
            error("untyped-phase", phase.line,
                  f"Phase is a plain string. Declare a {kind}Phase string type with "
                  "its constants beside it, so an impossible value is a compile error")
        if conditions is not None:
            missing = [m for m in CONDITION_MARKERS if not conditions.has(m)]
            if missing:
                error("conditions-without-listtype", conditions.line,
                      f"Conditions is missing {', '.join(missing)}, so server-side "
                      "apply replaces the whole list instead of merging by type")

    # ---- immutability claimed in prose but not enforced anywhere
    #
    # Three spellings enforce it, and all three reach the schema: `+k8s:immutable`
    # (controller-gen emits `self == oldSelf`), a field-level XValidation, and a
    # type-level XValidation naming the field. A doc comment is the fourth
    # spelling and it is the only one that enforces nothing.
    for struct in structs.values():
        if not struct.name.startswith(kind):
            continue
        for field in struct.fields:
            if "immutab" not in field.doc.lower():
                continue
            enforced = (
                field.has("k8s:immutable")
                or any("XValidation" in m for m in field.markers)
                or any(
                    "XValidation" in m and field.json and field.json in m
                    for m in struct.markers
                )
            )
            if enforced:
                continue
            error("unenforced-immutability", field.line,
                  f"{struct.name}.{field.name} says immutable in prose and enforces "
                  "nothing. Add +k8s:immutable for always-immutable, or a CEL rule for "
                  "immutable-once-set, or stop claiming it")

    # ---- spec conventions
    for struct in structs.values():
        if not struct.name.startswith(kind) or not struct.name.endswith("Spec"):
            continue
        for field in struct.fields:
            if field.name in ("TypeMeta", "ObjectMeta") or "inline" in field.json:
                continue
            explicit = field.has("+optional") or field.has("validation:Required")
            if not explicit and "omitempty" not in field.raw_tag:
                warn("unspecified-spec-field", field.line,
                     f"{struct.name}.{field.name} has no +optional, no "
                     "validation:Required, and no omitempty, so whether it is required "
                     "is decided by accident")

    if is_ops and spec is not None:
        action = spec.field("Action")
        if action is None:
            warn("ops-without-action", spec.line, "an Ops kind with no Action field")
        elif not action.has("validation:Enum"):
            error("ops-action-not-enum", action.line,
                  "the action verb has no Enum, so an unknown action becomes a Failed "
                  "phase instead of an admission rejection")
        immutable = any("XValidation" in m for m in spec.markers) or any(
            f.has("k8s:immutable") or any("XValidation" in m for m in f.markers)
            for f in spec.fields
        )
        if not immutable:
            warn("ops-spec-mutable", spec.line,
                 "an Ops spec enforces no immutability. A request that can be rewritten "
                 "while it runs is a different request")

    # ---- closed sets
    for alias in aliases.values():
        if not alias.name.startswith(kind) or alias.base != "string":
            continue
        if alias.constants >= 2 and not alias.has("validation:Enum"):
            error("enumless-closed-set", alias.line,
                  f"{alias.name} has {alias.constants} constants and no Enum marker, so "
                  "the API server accepts any string")

    return found


def adoption(files):
    """The marker counts, read from the code rather than maintained by hand."""
    counts = Counter()
    kinds, with_short, with_phase, typed_phase, with_conditions, with_obsgen = 0, 0, 0, 0, 0, 0
    immutable_claims, cel_rules = 0, 0

    for path in files:
        structs, aliases = parse(path)
        text = path.read_text()
        for marker, needle in (
            ("object:root", "+kubebuilder:object:root=true"),
            ("subresource:status", "+kubebuilder:subresource:status"),
            ("printcolumn", "+kubebuilder:printcolumn"),
            ("XValidation", "XValidation"),
            ("+optional", "+optional"),
            ("validation:Required", "+kubebuilder:validation:Required"),
            ("validation:Enum", "+kubebuilder:validation:Enum"),
            ("default", "+kubebuilder:default"),
            ("Minimum/Maximum", "+kubebuilder:validation:M"),
        ):
            counts[marker] += text.count(needle)
        cel_rules += text.count("XValidation")

        for name, struct in structs.items():
            if not struct.has("object:root=true") or name.endswith("List"):
                continue
            kinds += 1
            if struct.has("shortName"):
                with_short += 1
            status = structs.get(f"{name}Status")
            if status is None:
                continue
            phase = status.field("Phase")
            if phase is not None:
                with_phase += 1
                if phase.gotype != "string":
                    typed_phase += 1
            if status.field("Conditions") is not None:
                with_conditions += 1
            if status.field("ObservedGeneration") is not None:
                with_obsgen += 1

        for struct in structs.values():
            for field in struct.fields:
                if "immutab" in field.doc.lower():
                    immutable_claims += 1

    print(f"{kinds} root kinds in {API_DIR}\n")
    print("marker                       count")
    print("-----------------------------------")
    for marker, count in counts.most_common():
        print(f"{marker:<28} {count:>5}")
    print("\nconvention                   adopted")
    print("-----------------------------------")
    for label, value in (
        ("shortName", with_short),
        ("status.phase", with_phase),
        ("  of those, typed", typed_phase),
        ("status.conditions", with_conditions),
        ("status.observedGeneration", with_obsgen),
    ):
        print(f"{label:<28} {value:>3} / {kinds}")
    print(f"\n{immutable_claims} fields claim immutability in prose, {cel_rules} CEL rules exist")


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--kind", help="audit one kind by name")
    parser.add_argument("--adoption", action="store_true", help="print the marker adoption table")
    parser.add_argument("--strict", action="store_true", help="exit non-zero on warnings too")
    parser.add_argument("--paths", nargs="*", help="type files to read instead of the whole API")
    parser.add_argument("--changed", action="store_true",
                        help="only type files changed against HEAD, which is the scope an "
                             "API change should be gated on")
    args = parser.parse_args()

    root = Path(__file__).resolve()
    for parent in root.parents:
        if (parent / API_DIR).is_dir():
            repo = parent
            break
    else:
        print(f"check-crds.py: {API_DIR} not found above {root}", file=sys.stderr)
        return 2

    if args.paths:
        files = [Path(p) for p in args.paths]
    elif args.changed:
        import subprocess

        listed = set()
        for command in (
            ["git", "diff", "--name-only", "HEAD", "--", API_DIR],
            ["git", "diff", "--name-only", "--cached", "--", API_DIR],
            ["git", "ls-files", "--others", "--exclude-standard", "--", API_DIR],
        ):
            result = subprocess.run(command, cwd=repo, capture_output=True, text=True)
            listed.update(line for line in result.stdout.split("\n") if line.endswith("_types.go"))
        files = sorted(repo / name for name in listed)
        if not files:
            print("check-crds.py: no API type files changed against HEAD")
            return 0
    else:
        files = sorted((repo / API_DIR).glob("*_types.go"))
    files = [f for f in files if f.is_file() and "zz_generated" not in f.name]
    if not files:
        print("check-crds.py: no type files in scope", file=sys.stderr)
        return 2

    if args.adoption:
        adoption(files)
        return 0

    errors, warnings, audited = 0, 0, 0
    for path in files:
        structs, aliases = parse(path)
        for name, struct in sorted(structs.items()):
            if not struct.has("object:root=true") or name.endswith("List"):
                continue
            if args.kind and name != args.kind:
                continue
            audited += 1
            found = audit_kind(name, structs, aliases, path)
            if not found:
                continue
            print(f"\n{name}  ({path.name})")
            for level, rule, line, message in sorted(found, key=lambda f: (f[0], f[2])):
                print(f"  {level:<5} {path.name}:{line}  [{rule}] {message}")
                if level == "ERROR":
                    errors += 1
                else:
                    warnings += 1

    if args.kind and audited == 0:
        print(f"check-crds.py: no root kind named {args.kind}", file=sys.stderr)
        return 2

    kinds = "kind" if audited == 1 else "kinds"
    print(f"\n{audited} {kinds} audited, {errors} error(s), {warnings} warning(s)")
    if errors or (args.strict and warnings):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
