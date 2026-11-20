#!/usr/bin/env python3
# Checks that every field path the console's headline tables name
# (cr-headlines.jsx) exists in the operator's CRDs (operator/config/crd/bases),
# so a page can only show a field the operator has. Run from the repository
# root or from control-center; exits non-zero on an unknown path.
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("check-crd-paths: PyYAML is not installed; skipping", file=sys.stderr)
    sys.exit(0)

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
CRD_DIR = os.path.join(ROOT, "operator", "config", "crd", "bases")
HEADLINES = os.path.join(ROOT, "control-center", "cr-headlines.jsx")
# kinds whose schema is not the operator's
CORE_KINDS = {"PersistentVolume", "PersistentVolumeClaim", "StorageClass", "VolumeSnapshot", "VolumeSnapshotContent", "Node", "Pod"}


def walk(schema, path, acc):
    for key, node in (schema.get("properties") or {}).items():
        p = (path + "." + key) if path else key
        if node.get("type") == "array":
            acc.add(p + "[]")
            acc.add(p)
            items = node.get("items") or {}
            if items.get("properties"):
                walk(items, p + "[]", acc)
                walk(items, p, acc)
        else:
            acc.add(p)
            walk(node, p, acc)
            extra = node.get("additionalProperties")
            if isinstance(extra, dict) and extra.get("properties"):
                walk(extra, p + "{}", acc)


def inventory():
    kinds = {}
    for f in sorted(glob.glob(os.path.join(CRD_DIR, "*.yaml"))):
        crd = yaml.safe_load(open(f, encoding="utf-8"))
        newest = sorted(crd["spec"]["versions"], key=lambda v: v["name"])[-1]
        acc = set()
        walk(newest["schema"]["openAPIV3Schema"], "", acc)
        kinds[crd["spec"]["names"]["kind"]] = acc
    return kinds


def main():
    kinds = inventory()
    src = open(HEADLINES, encoding="utf-8").read()
    bad = []
    for m in re.finditer(r"^  (\w+): \{(.*?)^  \},?$", src, re.S | re.M):
        kind, body = m.group(1), m.group(2)
        if kind in CORE_KINDS:
            continue
        fields = kinds.get(kind)
        if fields is None:
            bad.append((kind, "<no CRD of this kind>"))
            continue
        paths = re.findall(r'\b(?:row|ro)\("([^"]+)"', body) + re.findall(r'path: "([^"]+)"', body)
        for p in paths:
            if p not in fields:
                bad.append((kind, p))
    if bad:
        for kind, p in bad:
            print("check-crd-paths: %s: %s is not a field of the CRD" % (kind, p))
        sys.exit(1)
    print("check-crd-paths: every headline path exists in the operator's CRDs")


if __name__ == "__main__":
    main()
