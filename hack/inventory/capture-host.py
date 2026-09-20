#!/usr/bin/env python3
# Captures a machine's sysfs into the transcript atlas-lib/inventory's tests
# replay, so a reading can be checked against what a host actually exports.
#
# It is run by hand on a machine worth keeping a transcript of, not in CI:
#
#     oc debug node/<node> --quiet -- chroot /host python3 - \
#         < hack/inventory/capture-host.py \
#         | gzip -9 > atlas-lib/inventory/testdata/hosts/<name>.json.gz
#
# Only the trees the readers walk are taken, and a symlink into /sys/devices is
# followed a little way so the class directories have the attributes behind them.
# The output is one JSON document of three maps -- directories, file contents,
# and symlink targets -- with every path relative to /sys.

import json
import os

SYS = "/sys"

# The seeds are the readers' own entry points: interfaces, NVMe controllers,
# block devices, the PCI bus, CPU topology, NUMA, and huge pages.
SEEDS = [
    "class/net",
    "class/nvme",
    "block",
    "bus/pci/devices",
    "devices/system/cpu",
    "devices/system/node",
    "kernel/mm/hugepages",
]

MAX_FILE_BYTES = 64 * 1024
MAX_DEPTH = 8
# A device a class entry points at is walked, but only a little way: the whole
# device tree reached from one link is most of the machine.
MAX_DEVICE_DEPTH = 4

dirs = set()
files = {}
links = {}
seen = set()


def relative(path):
    return os.path.relpath(path, SYS)


def walk(start, depth=0):
    if depth > MAX_DEPTH or start in seen:
        return
    seen.add(start)
    try:
        entries = sorted(os.listdir(start))
    except OSError:
        return
    dirs.add(relative(start))
    for name in entries:
        path = os.path.join(start, name)
        if os.path.islink(path):
            try:
                links[relative(path)] = os.readlink(path)
            except OSError:
                continue
            target = os.path.realpath(path)
            if target.startswith(SYS + "/devices") and depth < MAX_DEVICE_DEPTH:
                walk(target, depth + 1)
        elif os.path.isdir(path):
            walk(path, depth + 1)
        elif os.path.isfile(path):
            # Most of sysfs refuses a read for a reason that is not an error: an
            # attribute that does not apply to this device, a link with no
            # carrier, a file the kernel answers only for root. What was
            # readable is what the transcript carries.
            try:
                if os.stat(path).st_size > MAX_FILE_BYTES:
                    continue
                with open(path, "rb") as handle:
                    files[relative(path)] = handle.read(MAX_FILE_BYTES).decode("utf-8", "replace")
            except OSError:
                continue


for seed in SEEDS:
    walk(os.path.join(SYS, seed))

# The readers take one root for both trees, so the few procfs files they read
# are carried in the same transcript: the memory reading and the affinity mask
# come from there rather than from sysfs.
for rel, path in (("meminfo", "/proc/meminfo"), ("self/status", "/proc/self/status")):
    try:
        with open(path) as handle:
            files[rel] = handle.read()
    except OSError:
        continue
dirs.add("self")

print(json.dumps(
    {"dirs": sorted(dirs), "files": dict(sorted(files.items())), "links": dict(sorted(links.items()))},
    indent=1, sort_keys=True))
