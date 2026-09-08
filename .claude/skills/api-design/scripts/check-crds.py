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
# --design runs the same audit over the Go in a design document's appendices,
# which is where a per-kind design states the type it is specifying. A convention
# broken in a document becomes a convention broken in the API a release later, so
# the cheapest place to catch it is before anything is implemented.
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
#   check-crds.py --design d.md         audit the types a design document's appendix states
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


TOGGLE_OK_RE = re.compile(r"^(enable|disable)[A-Z0-9]")

# The spellings this repository does not accept: a skip/with/no prefix, an
# Enabled/Disabled suffix, or a bare enabled/disabled with nothing named.
# Each alternative carries its own anchor, because the suffix forms are matched
# with search() and an unanchored one would fire on any name containing them.
TOGGLE_BAD_SHAPE_RE = re.compile(
    r"^(?:skip|with|no)[A-Z0-9]|(?:Enabled|Disabled)$|^(?:enabled|disabled)$"
)

# The value list of a +kubebuilder:validation:Enum marker.
ENUM_MARKER_RE = re.compile(r"validation:Enum=(\S+)")

# A value this API group invents is PascalCase. The exception is a value that
# names something outside the group, whose own spelling wins: a filesystem
# (ext4, xfs), a wire protocol, or a vocabulary an external API already defines.
# Those are listed rather than pattern-matched, because "looks like a foreign
# word" is not a property a regex has.
ENUM_PASCAL_RE = re.compile(r"^[A-Z][A-Za-z0-9]*$")
ENUM_FOREIGN = {
    # Filesystems, named by the kernel.
    "ext2", "ext3", "ext4", "xfs", "btrfs",
    # Wire protocols and fabrics, named by their specifications.
    "tcp", "rdma", "fc", "udp", "sctp",
}

# A doc comment claiming a validating webhook guards the field. It is the one
# enforcement that does not appear as a marker, and it is what a field with a
# single legitimate writer uses instead of +k8s:immutable, which would lock the
# operator out along with everyone else.
WEBHOOK_GUARD_RE = re.compile(r"\bwebhook\b[^.]*\b(reject|denie|denies|deny|guard)", re.IGNORECASE)

# A doc comment that describes a toggle, whatever the field ended up called.
TOGGLE_DOC_RE = re.compile(
    r"\b(enables?|enabling|disables?|disabling|skips?|skipping|turns? (on|off)|"
    r"activates?|deactivates?|opts? in|opts? out)\b",
    re.IGNORECASE,
)


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


# A fenced Go block in a design document, and the appendix heading that marks
# where the authoritative types start. Everything before the first appendix is
# excerpts, which are deliberately partial and would parse as truncated types.
DESIGN_FENCE_RE = re.compile(r"^```go\s*$")
DESIGN_FENCE_END_RE = re.compile(r"^```\s*$")
DESIGN_APPENDIX_RE = re.compile(r"^##+\s+Appendix\b")


def design_source(path):
    """The Go in a design document's appendices, at its Markdown line numbers.

    Every line outside a fenced Go block becomes blank, so a finding's line
    number points at the design document rather than at an offset into some
    concatenation of its code blocks.
    """
    lines = path.read_text().splitlines()
    out, in_appendix, in_go = [], False, False
    for raw in lines:
        if DESIGN_APPENDIX_RE.match(raw):
            in_appendix = True
        if in_appendix and not in_go and DESIGN_FENCE_RE.match(raw):
            in_go = True
            out.append("")
            continue
        if in_go and DESIGN_FENCE_END_RE.match(raw):
            in_go = False
            out.append("")
            continue
        out.append(raw if in_go else "")
    return "\n".join(out)


def parse(path):
    """Structs, named types, and their attached comment and marker blocks."""
    return parse_source(path.read_text())


def parse_source(text):
    """parse(), over source already in hand rather than a file."""
    structs, aliases, block, current = {}, {}, [], None
    lines = text.splitlines()

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


def owns(kind, name, kinds):
    """Whether `name` belongs to `kind` rather than to a longer-named sibling.

    Kind names prefix one another: StorageDeviceOpsSpec starts with
    StorageDevice, so a plain startswith would let the entity's audit claim the
    Ops kind's structs and, with them, lose the Ops exemptions. The longest
    matching root kind wins.
    """
    if not name.startswith(kind):
        return False
    return not any(
        other != kind and len(other) > len(kind) and name.startswith(other)
        for other in kinds
    )


def audit_kind(kind, structs, aliases, path, kinds=()):
    """Every finding for one root kind, as (level, rule, line, message)."""
    found = []
    kinds = set(kinds) or {kind}
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
            error("no-observed-generation", status.line,
                  "status has no ObservedGeneration, so a stale status cannot be told "
                  "from a current one, and a spec edit cannot be waited on. See "
                  "design-crd-model.md §7.9 and reconciler-patterns")
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
    # Three spellings enforce it in the schema. `+k8s:immutable` is the one to
    # use: controller-gen emits a field-level `self == oldSelf` and, for an
    # optional field, a parent-level rule against removal, which together are
    # immutable-once-set. A field-level or type-level XValidation is the longer
    # hand-written form.
    #
    # A fourth enforces it outside the schema, and it is the one a schema rule
    # cannot express: a field with exactly one legitimate writer. A validating
    # webhook admits the operator's own service account and rejects everyone
    # else, where `+k8s:immutable` would reject the operator too and no marker at
    # all would let a user rewrite the field. StorageNode.spec.workerNode is the
    # case, re-pointed by a migration and by nothing else. A doc comment naming
    # that webhook is therefore an enforcement claim rather than an unbacked one,
    # and the comment has to name it: "the webhook rejects" counts, "immutable"
    # alone does not.
    for struct in structs.values():
        if not owns(kind, struct.name, kinds):
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
                or WEBHOOK_GUARD_RE.search(field.doc)
            )
            if enforced:
                continue
            error("unenforced-immutability", field.line,
                  f"{struct.name}.{field.name} says immutable in prose and enforces "
                  "nothing. Add +k8s:immutable, which is once-set on an optional field "
                  "and from-creation on a required one, or stop claiming it")

    # ---- spec conventions
    for struct in structs.values():
        if not owns(kind, struct.name, kinds) or not struct.name.endswith("Spec"):
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

    # ---- boolean toggles must be enableXyz or disableXyz
    #
    # A property that turns something on or off is named `enableXyz` when the
    # thing is off by default and `disableXyz` when it is on, and nothing else.
    # `skipXyz`, `withXyz`, `xyzEnabled`, and a bare `enabled` are all out.
    # The form follows the default: a non-pointer bool zero-values to false, so
    # only the negative spelling makes an unset field mean the default, which is
    # why both forms exist.
    #
    # Status booleans are observations rather than toggles (`ready`, `configured`,
    # `rebalancing`), so they are skipped, and a signal is required besides: an
    # invalid shape in the name, or a doc comment that says the field enables or
    # disables something. Without that, a bool naming a fact about the world
    # (`ubuntuHost`) or an instruction to an action (`deleteSource`) would be
    # flagged for a rule that is not about it.
    for struct in structs.values():
        if struct.name.endswith("Status"):
            continue
        # An Ops spec's booleans qualify one request rather than switching a
        # capability on, so they are outside the rule -- `force`, `deleteSource`,
        # `refreshSNodeAPI`. That covers the nested per-action parameter blocks too,
        # which also end in Spec.
        #
        # The exemption is decided by the struct's own owning kind rather than by
        # the kind being audited, because kind names prefix one another: without
        # that, auditing StorageDevice would scan StorageDeviceOpsSpec with the
        # exemption switched off, since StorageDevice is not itself an Ops kind.
        # This loop deliberately has no owns() filter, because a struct whose name
        # matches no kind at all -- BackupSpec on the cluster -- still has to be
        # checked by somebody.
        struct_is_ops = is_ops or any(
            k.endswith("Ops") and struct.name.startswith(k) for k in kinds
        )
        if struct_is_ops and struct.name.endswith("Spec"):
            continue
        for field in struct.fields:
            if field.gotype.lstrip("*") != "bool":
                continue
            name = field.json.split(",")[0]
            if not name or TOGGLE_OK_RE.match(name):
                continue
            bad_shape = TOGGLE_BAD_SHAPE_RE.search(name)
            says_toggle = TOGGLE_DOC_RE.search(field.doc)
            if not bad_shape and not says_toggle:
                continue
            error("toggle-not-enable-disable", field.line,
                  f"{struct.name}.{field.name} is a boolean toggle named {name!r}. "
                  "Name it enableXyz when the thing is off by default, or disableXyz "
                  "when it is on, so that the zero value is the default")

    if is_ops and spec is not None:
        action = spec.field("Action")
        if action is None:
            warn("ops-without-action", spec.line, "an Ops kind with no Action field")
        else:
            # Two spellings reach the schema, and the second is the better one:
            # the marker sits on the field, or the field is typed as a named
            # alias that carries it. controller-gen resolves the alias, so the
            # generated CRD is identical, and the named type additionally makes
            # an impossible value a compile error rather than a rejection at
            # admission. Checking only the field would report the stronger
            # spelling as the missing one.
            declared = aliases.get(action.gotype.lstrip("*"))
            if not action.has("validation:Enum") and not (
                declared is not None and declared.has("validation:Enum")
            ):
                error("ops-action-not-enum", action.line,
                      "the action verb has no Enum, on the field or on its type, so an "
                      "unknown action becomes a Failed phase instead of an admission "
                      "rejection")
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
        if not owns(kind, alias.name, kinds) or alias.base != "string":
            continue
        if alias.constants >= 2 and not alias.has("validation:Enum"):
            error("enumless-closed-set", alias.line,
                  f"{alias.name} has {alias.constants} constants and no Enum marker, so "
                  "the API server accepts any string")

    # ---- enum value casing
    #
    # Every Enum marker in the file, whether it sits on a named type or inline on
    # a field, because a lowercase verb is the same defect in either place.
    for holder in list(aliases.values()) + [
        f for st in structs.values() for f in st.fields
    ] + list(structs.values()):
        for marker in holder.markers:
            match = ENUM_MARKER_RE.search(marker)
            if not match:
                continue
            offenders = [
                v for v in match.group(1).split(";")
                if v and not ENUM_PASCAL_RE.match(v) and v.lower() not in ENUM_FOREIGN
            ]
            if not offenders:
                continue
            error("enum-value-not-pascal-case", holder.line,
                  f"{holder.name} has enum value(s) {', '.join(repr(o) for o in offenders)}. "
                  "A value this API group defines is PascalCase, matching every phase in "
                  "the group and every enum in core Kubernetes. A value naming something "
                  "outside the group keeps that thing's spelling")

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


def audit_designs(paths, args):
    """Audit the types a design document states in its appendices.

    A design's appendix is the type as it will be written, so the conventions
    that gate a shipped type gate it here too, one release earlier and before
    anything has been implemented against it.
    """
    errors, warnings, audited, seen = 0, 0, 0, 0
    for path in paths:
        if not path.is_file():
            print(f"check-crds.py: {path} not found", file=sys.stderr)
            return 2
        structs, aliases = parse_source(design_source(path))
        seen += len(structs)
        roots = {n for n, st in structs.items()
                 if st.has("object:root=true") and not n.endswith("List")}
        for name, struct in sorted(structs.items()):
            if not struct.has("object:root=true") or name.endswith("List"):
                continue
            if args.kind and name != args.kind:
                continue
            audited += 1
            found = audit_kind(name, structs, aliases, path, roots)
            if not found:
                continue
            print(f"\n{name}  ({path.name}, appendix)")
            for level, rule, line, message in sorted(found, key=lambda f: (f[0], f[2])):
                print(f"  {level:<5} {path.name}:{line}  [{rule}] {message}")
                if level == "ERROR":
                    errors += 1
                else:
                    warnings += 1
        if not structs:
            print(f"check-crds.py: {path.name} has no Go in an appendix", file=sys.stderr)
            return 2
        if not any(st.has("object:root=true") for st in structs.values()):
            print(f"check-crds.py: {path.name}'s appendix declares no root kind. A design "
                  "that specifies a CRD states its root type and its markers", file=sys.stderr)
            return 2

    docs = "document" if len(paths) == 1 else "documents"
    kinds = "kind" if audited == 1 else "kinds"
    print(f"\n{len(paths)} design {docs}, {seen} type(s) in appendices, {audited} root {kinds} "
          f"audited, {errors} error(s), {warnings} warning(s)")
    if errors or (args.strict and warnings):
        return 1
    return 0


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--kind", help="audit one kind by name")
    parser.add_argument("--adoption", action="store_true", help="print the marker adoption table")
    parser.add_argument("--strict", action="store_true", help="exit non-zero on warnings too")
    parser.add_argument("--paths", nargs="*", help="type files to read instead of the whole API")
    parser.add_argument("--changed", action="store_true",
                        help="only type files changed against HEAD, which is the scope an "
                             "API change should be gated on")
    parser.add_argument("--design", nargs="+", metavar="DOC",
                        help="audit the Go in a design document's appendices, so a type is "
                             "checked while it is still a document")
    args = parser.parse_args()

    if args.design:
        return audit_designs([Path(p) for p in args.design], args)

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
        roots = {n for n, st in structs.items()
                 if st.has("object:root=true") and not n.endswith("List")}
        for name, struct in sorted(structs.items()):
            if not struct.has("object:root=true") or name.endswith("List"):
                continue
            if args.kind and name != args.kind:
                continue
            audited += 1
            found = audit_kind(name, structs, aliases, path, roots)
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
