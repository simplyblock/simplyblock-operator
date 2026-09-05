#!/usr/bin/env bash
# Captures the head and tail regions of block devices that real tools formatted,
# for atlas-lib/blockdev's signature tests.
#
# The fixtures these produce are evidence rather than construction: a fixture
# built from the offsets in the design's signature catalog would assert only that
# the decoder agrees with the catalog, and would pass while both were wrong about
# what mkfs actually writes. So every image here is made by the real tool, on a
# real loop device, and only then read back.
#
# It needs root and loop devices, so it is run by hand on a scratch Linux host and
# never in CI. Nothing it does touches a device it did not create: every format
# targets a loop device backed by a file under the work directory.
#
# Usage: capture-image.sh <output-dir> [name ...]

set -euo pipefail

OUT=${1:?usage: capture-image.sh <output-dir> [name ...]}
shift || true
WANTED=("$@")

WORK=$(mktemp -d /var/tmp/blockdev-capture.XXXXXX)
REGION=$((1024 * 1024)) # one mebibyte, the prober's default region size

# Detaching is driven off what losetup reports for this run's work directory
# rather than off a list the script kept: attach runs inside a command
# substitution, so anything it appended to a variable would be written in a
# subshell and lost to this trap, leaving every loop device of the run attached.
# The files are removed only after the devices holding them are gone, so a
# detach that fails is visible rather than hidden behind a deleted backing file.
cleanup() {
    local d
    for d in $(losetup -a | grep -F "$WORK" | cut -d: -f1); do
        losetup -d "$d" 2>/dev/null || losetup -d "$d" 2>/dev/null || true
    done
    for d in $(losetup -a | grep -F "$WORK" | cut -d: -f1); do
        echo "warning: $d is still attached to $WORK; detach it by hand" >&2
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

want() {
    [ ${#WANTED[@]} -eq 0 ] && return 0
    local n
    for n in "${WANTED[@]}"; do [ "$n" = "$1" ] && return 0; done
    return 1
}

# attach makes a loop device of size_mb over a fresh sparse file. block_size is
# the logical block size to present, which is what the GPT rows are read against:
# a 4Kn device puts LBA 1 at offset 4096 rather than 512.
attach() {
    local name=$1 size_mb=$2 block_size=${3:-512} img dev
    img="$WORK/$name.img"
    truncate -s "${size_mb}M" "$img"
    dev=$(losetup --find --show --sector-size "$block_size" "$img")
    echo "$dev"
}

# capture reads the two regions off dev and writes the fixture directory.
# tool_version and command are recorded because the ext feature words, and so the
# family a reading names, depend on which generation of the tool wrote the image.
capture() {
    local name=$1 dev=$2 tool=$3 tool_version=$4 command=$5 note=${6:-}
    local dir="$OUT/$name" size block probe

    mkdir -p "$dir"
    blockdev --flushbufs "$dev" 2>/dev/null || true
    size=$(blockdev --getsize64 "$dev")
    block=$(blockdev --getss "$dev")

    dd if="$dev" of="$dir/head.bin" bs="$REGION" count=1 iflag=direct status=none
    dd if="$dev" of="$dir/tail.bin" bs="$REGION" count=1 iflag=direct status=none \
        skip=$(((size - REGION) / REGION))

    # blkid escapes spaces in its values ("LVM2\ 001"), so the backslashes and
    # quotes are escaped again on the way into JSON.
    probe=$(blkid -p -o export "$dev" 2>/dev/null | tr '\n' ' ' | sed 's/ *$//' |
        sed 's/\\/\\\\/g; s/"/\\"/g' || true)

    cat >"$dir/manifest.json" <<EOF
{
  "name": "$name",
  "tool": "$tool",
  "tool_version": "$tool_version",
  "command": "$command",
  "device_size": $size,
  "logical_block_size": $block,
  "region_size": $REGION,
  "captured": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "captured_on": "$(. /etc/os-release && echo "$PRETTY_NAME"), kernel $(uname -r)",
  "blkid": "$probe",
  "head_sha256": "$(sha256sum <"$dir/head.bin" | cut -d' ' -f1)",
  "tail_sha256": "$(sha256sum <"$dir/tail.bin" | cut -d' ' -f1)",
  "note": "$note"
}
EOF

    gzip -9 -f "$dir/head.bin" "$dir/tail.bin"
    printf '  %-16s %s\n' "$name" "$probe"
}

ver() { "$@" 2>&1 | head -1 | tr -d '\n'; }

mkdir -p "$OUT"
echo "capturing into $OUT"

if want blank; then
    dev=$(attach blank 64)
    capture blank "$dev" "none" "-" "truncate -s 64M" "never written to, the only reading that permits a format"
fi

for fs in ext2 ext3 ext4; do
    if want "$fs"; then
        dev=$(attach "$fs" 64)
        "mkfs.$fs" -q -F "$dev" >/dev/null
        capture "$fs" "$dev" "mkfs.$fs" "$(ver mke2fs -V)" "mkfs.$fs -q -F"
    fi
done

if want xfs; then
    dev=$(attach xfs 512)
    mkfs.xfs -q -f "$dev" >/dev/null
    capture xfs "$dev" "mkfs.xfs" "$(ver mkfs.xfs -V)" "mkfs.xfs -q -f"
fi

if want lvm2; then
    dev=$(attach lvm2 64)
    pvcreate -q -f -y "$dev" >/dev/null
    capture lvm2 "$dev" "pvcreate" "$(ver pvcreate --version)" "pvcreate -q -f -y" \
        "a physical-volume label, the one reading that is a stack layer rather than a filesystem"
fi

for t in luks1 luks2; do
    if want "$t"; then
        dev=$(attach "$t" 64)
        head -c 32 /dev/urandom >"$WORK/key"
        cryptsetup luksFormat --type "$t" --batch-mode --key-file "$WORK/key" \
            --pbkdf pbkdf2 --pbkdf-force-iterations 1000 "$dev" >/dev/null
        # The keyslot payload is random by construction, so it does not compress
        # and it is key material besides. Zeroing it past the header keeps the
        # fixture to a few kilobytes and keeps key bytes out of the repository.
        # The reading is unaffected: the LUKS magic that decides it is at offset 0.
        dd if=/dev/zero of="$dev" bs=4096 seek=1 count=255 conv=notrunc status=none
        capture "$t" "$dev" "cryptsetup" "$(ver cryptsetup --version)" \
            "cryptsetup luksFormat --type $t" \
            "sanitized: keyslot payload zeroed from offset 4096, which is key material and does not compress"
    fi
done

if want gpt; then
    dev=$(attach gpt 64)
    sgdisk -o -n 1:0:0 -t 1:8300 "$dev" >/dev/null 2>&1
    capture gpt "$dev" "sgdisk" "$(ver sgdisk --version)" "sgdisk -o -n 1:0:0 -t 1:8300" \
        "carries a protective MBR at LBA 0 as well, which is why GPT is evaluated first"
fi

if want gpt-4kn; then
    dev=$(attach gpt-4kn 64 4096)
    sgdisk -o -n 1:0:0 -t 1:8300 "$dev" >/dev/null 2>&1
    capture gpt-4kn "$dev" "sgdisk" "$(ver sgdisk --version)" "sgdisk -o -n 1:0:0 -t 1:8300" \
        "4Kn: the GPT header is at offset 4096, not 512"
fi

if want mbr; then
    dev=$(attach mbr 64)
    printf 'o\nn\np\n1\n\n\nw\n' | fdisk "$dev" >/dev/null 2>&1 || true
    capture mbr "$dev" "fdisk" "$(ver fdisk --version)" "fdisk: o, n, p, 1, defaults, w"
fi

for spec in "fat12 16 12" "fat16 64 16" "fat32 512 32"; do
    read -r name size bits <<<"$spec"
    if want "$name"; then
        dev=$(attach "$name" "$size")
        mkfs.vfat -F "$bits" "$dev" >/dev/null
        capture "$name" "$dev" "mkfs.vfat" "$(ver mkfs.vfat --help)" "mkfs.vfat -F $bits"
    fi
done

if want exfat; then
    dev=$(attach exfat 64)
    mkfs.exfat "$dev" >/dev/null 2>&1
    capture exfat "$dev" "mkfs.exfat" "$(ver mkfs.exfat --version)" "mkfs.exfat"
fi

if want btrfs; then
    dev=$(attach btrfs 256)
    mkfs.btrfs -q -f "$dev" >/dev/null
    capture btrfs "$dev" "mkfs.btrfs" "$(ver mkfs.btrfs --version)" "mkfs.btrfs -q -f"
fi

if want swap; then
    dev=$(attach swap 64)
    mkswap "$dev" >/dev/null
    capture swap "$dev" "mkswap" "$(ver mkswap --version)" "mkswap" \
        "the signature sits at page size minus ten, so it moves with the page size"
fi

for meta in 1.0 1.1 1.2; do
    name="mdraid-${meta//./}"
    if want "$name"; then
        dev=$(attach "$name" 64)
        mdadm --create --run --quiet "/dev/md/capture-$name" --level=1 \
            --raid-devices=2 --metadata="$meta" "$dev" missing >/dev/null 2>&1 || true
        mdadm --stop "/dev/md/capture-$name" >/dev/null 2>&1 || true
        capture "$name" "$dev" "mdadm" "$(ver mdadm --version)" \
            "mdadm --create --level=1 --metadata=$meta <dev> missing" \
            "metadata $meta: 1.0 puts the superblock in the tail region, 1.1 and 1.2 in the head"
    fi
done

if want zfs; then
    dev=$(attach zfs 256)
    zpool create -f "capture-$$" "$dev" >/dev/null 2>&1 &&
        zpool export "capture-$$" >/dev/null 2>&1 || true
    capture zfs "$dev" "zpool" "$(ver zpool version)" "zpool create -f <pool> <dev>" \
        "vdev labels L0 and L1 in the head region, L2 and L3 in the tail"
fi

echo "done"
