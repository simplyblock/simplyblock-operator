#!/usr/bin/env python3
"""Export the control plane's OpenAPI spec, and summarize what changed.

openapi.json is the contract three things in this repository are generated or
checked against — atlas-lib's client, the integration suite's control-plane
simulator, and the operator's spec-backed mock — so it has to come from the
control plane itself rather than be edited by hand.

The export mirrors what simplyblock-documentation does (its
scripts/openapi-json-gen.py): import the FastAPI app, ask it for its spec, keep
the /api/v2 paths. The v1 surface is a WSGI-mounted Flask app plus a set of
catch-all redirect routes, and those redirects are what the filter drops; they
are not part of the typed v2 surface and their {full_path} parameter is not
declared in a way a generator can use.

Importing the app needs sbcli's requirements installed, but no database and no
FoundationDB client library: nothing connects at import time.

    export-openapi.py export --sbcli ../sbcli --out shared/openapi.json
    export-openapi.py summarize --old openapi.old.json --new shared/openapi.json
"""

import argparse
import json
import os
import sys


V2_PREFIX = "/api/v2"


def export(sbcli: str, out: str) -> None:
    # Ahead of the rest of sys.path: a stray installed copy of simplyblock_web
    # would otherwise export the spec of whatever version that is.
    sys.path.insert(0, os.path.abspath(sbcli))

    from simplyblock_web.app import app  # noqa: PLC0415  (needs the path above)

    spec = app.openapi()
    spec["paths"] = {
        path: item for path, item in spec["paths"].items()
        if path.startswith(V2_PREFIX)
    }
    if not spec["paths"]:
        sys.exit(f"no {V2_PREFIX} paths in the exported spec — did the API move?")

    # indent=2 and no trailing newline, matching what is committed, so a
    # formatting choice never shows up as a diff.
    with open(out, "w", encoding="utf-8") as f:
        json.dump(spec, f, indent=2)

    print(f"{out}: {len(spec['paths'])} paths, "
          f"{len(spec.get('components', {}).get('schemas', {}))} schemas")


def summarize(old: str, new: str) -> None:
    """Print a Markdown summary of the change, for a pull-request body.

    A regenerated spec is a large diff of mostly mechanical JSON. What a reviewer
    needs first is which endpoints appeared, which went away, and which changed
    shape in place — the last of those being the ones a generated client compiles
    against without noticing.
    """
    with open(old, encoding="utf-8") as f:
        a = json.load(f)
    with open(new, encoding="utf-8") as f:
        b = json.load(f)

    a_paths, b_paths = set(a["paths"]), set(b["paths"])
    removed, added = sorted(a_paths - b_paths), sorted(b_paths - a_paths)
    changed = sorted(p for p in a_paths & b_paths if a["paths"][p] != b["paths"][p])

    a_schemas = a.get("components", {}).get("schemas", {})
    b_schemas = b.get("components", {}).get("schemas", {})
    schemas_added = sorted(set(b_schemas) - set(a_schemas))
    schemas_removed = sorted(set(a_schemas) - set(b_schemas))
    schemas_changed = sorted(
        s for s in set(a_schemas) & set(b_schemas) if a_schemas[s] != b_schemas[s]
    )

    def section(title: str, items: list, warn: str = "") -> None:
        if not items:
            return
        print(f"### {title} ({len(items)})")
        if warn:
            print(f"\n{warn}")
        print()
        for i in items:
            print(f"- `{i}`")
        print()

    print(f"`{len(b_paths)}` paths, `{len(b_schemas)}` schemas "
          f"(was `{len(a_paths)}` and `{len(a_schemas)}`).\n")

    section("Endpoints removed", removed,
            "A generated client keeps compiling against an endpoint that is gone; "
            "so does an overlay that patches one.")
    section("Endpoints added", added)
    section("Endpoints changed in place", changed,
            "Parameters, request bodies or responses moved. These are the changes "
            "a client compiles against without noticing.")
    section("Schemas added", schemas_added)
    section("Schemas removed", schemas_removed)
    section("Schemas changed", schemas_changed)

    if not (removed or added or changed or schemas_added or schemas_removed or schemas_changed):
        print("No structural change; the diff is formatting only.")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    e = sub.add_parser("export", help="write the spec")
    e.add_argument("--sbcli", required=True, help="path to a checkout of the sbcli repository")
    e.add_argument("--out", required=True, help="file to write")

    s = sub.add_parser("summarize", help="describe the change between two specs")
    s.add_argument("--old", required=True)
    s.add_argument("--new", required=True)

    args = parser.parse_args()
    if args.command == "export":
        export(args.sbcli, args.out)
    else:
        summarize(args.old, args.new)


if __name__ == "__main__":
    main()
