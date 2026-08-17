#!/usr/bin/env bash
#
# capture-sysfs.sh — copy a node's NVMe sysfs state into a replayable tree.
#
# The point is to run the real resolver over real kernel output. The expensive
# failure mode of the defect detection is not missing a defect, it is inventing
# one — every repair tears down a live data path — and hand-built fixtures cannot
# rule that out, because they encode what we believe sysfs looks like, which is
# the belief under test.
#
# sysfs cannot be copied with tar: it is full of symlinks that point back up the
# tree and files whose size is a lie. So this walks the two class directories,
# follows symlinks, and writes one "path<TAB>value" line per attribute; the
# reconstruct step turns that back into a directory tree that
# nvme.SysfsConfig{SysRoot: …} can be pointed at.
#
# The snapshot is the artifact, not the tree: one text file, diffable and small
# enough to commit. Snapshots checked in under nvmeof/testdata/sysfs
# are what lets the defect tests run with no cluster and no /sys at all, so a
# state that took two nvmet targets on two nodes to produce becomes an ordinary
# `go test` away forever after.
#
# USAGE
#   # on a node, or through a privileged pod that mounts the host /sys:
#   capture-sysfs.sh dump > snapshot.tsv
#
#   # in Kubernetes, against the node running a workload:
#   kubectl exec -n simplyblock <csi-node-pod> -c csi-node -- \
#       sh -c "$(sed -n '/^dump()/,/^}/p' capture-sysfs.sh); dump" > snapshot.tsv
#
#   # strip real cluster identity before committing one as a fixture:
#   capture-sysfs.sh sanitize snapshot.tsv > testdata/sysfs/healthy.tsv
#
#   # rebuild a tree from a snapshot (the Go tests do this themselves):
#   capture-sysfs.sh reconstruct snapshot.tsv ./sysroot
#   ATLAS_SYSROOT=$PWD/sysroot go test ./nvmeof/ -run TestLiveSysfs -v
#
# A raw snapshot holds device identity — NQNs, cluster and lvol UUIDs, target
# addresses, the host's own NQN — so treat it like any other cluster dump, and
# run it through `sanitize` before it becomes a committed fixture.

set -euo pipefail

# dump is written as a self-contained POSIX-sh function so it can be piped into
# a container that has no copy of this script.
dump() {
	find -L /sys/class/nvme-subsystem /sys/class/nvme -maxdepth 3 \
		\( -name subsystem -o -name power -o -name device -o -name firmware_node \) -prune \
		-o -type f -print 2>/dev/null | while read -r f; do
		v=$(cat "$f" 2>/dev/null | head -1 | tr -d '\n')
		printf '%s\t%s\n' "$f" "$v"
	done
}

reconstruct() {
	local src="${1:?usage: reconstruct <snapshot.tsv> <sysroot>}"
	local root="${2:?usage: reconstruct <snapshot.tsv> <sysroot>}"
	python3 - "$src" "$root" <<'PY'
import os, sys
src, root = sys.argv[1], sys.argv[2]
n = 0
for line in open(src, encoding="utf-8", errors="replace"):
    line = line.rstrip("\n")
    path, _, val = line.partition("\t")
    if not path.startswith("/sys/"):
        continue
    dest = os.path.join(root, path[len("/sys/"):])
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w", encoding="utf-8") as fh:
        fh.write(val + "\n")
    n += 1
print(f"reconstructed {n} attributes under {root}", file=sys.stderr)
PY
}

# sanitize replaces real cluster identity with stable stand-ins so a snapshot can
# be committed as a fixture.
#
# The substitution is consistent across the whole file: each distinct UUID, IP
# and host NQN gets one replacement, reused everywhere it appears. That matters
# more than the anonymity does — the fixtures are only meaningful because the
# model equals the master lvol UUID which also appears in the NQN and in
# namespace 1's uuid, and a per-occurrence scramble would dissolve exactly the
# relationships under test.
sanitize() {
	python3 - "${1:?usage: sanitize <snapshot.tsv>}" <<'PY'
import re, sys

src = sys.argv[1]
text = open(src, encoding="utf-8", errors="replace").read()

UUID = re.compile(r'\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b')
IPV4 = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')

uuids, ips = {}, {}

def sub_uuid(m):
    v = m.group(0).lower()
    if v not in uuids:
        n = len(uuids) + 1
        # Keep UUID shape exactly: length and hyphen positions are what the
        # by-id link names and the driver's globs are built from.
        uuids[v] = "%08x-0000-4000-8000-%012x" % (n, n)
    return uuids[v]

def sub_ip(m):
    v = m.group(0)
    if v in ("127.0.0.1", "0.0.0.0"):
        return v
    if v not in ips:
        ips[v] = "10.0.0.%d" % (len(ips) + 1)
    return ips[v]

text = UUID.sub(sub_uuid, text)
text = IPV4.sub(sub_ip, text)
sys.stdout.write(text)
print(f"sanitized {len(uuids)} uuid(s) and {len(ips)} address(es)", file=sys.stderr)
PY
}

case "${1:-}" in
dump)        dump ;;
reconstruct) reconstruct "${2:-}" "${3:-}" ;;
sanitize)    sanitize "${2:-}" ;;
*) sed -n '/^# USAGE/,/^# run it through/p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac