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
# The second mode captures a device that is already in service, for the formats
# no local tool writes: a storage node's own pages, and anything a fleet turned
# out to be carrying. It only ever reads, it never attaches a loop device, and it
# fills the manifest's provenance with placeholders, because a device found in
# the field cannot say which tool wrote it. Those fields are edited by hand
# afterward; the checksums cover the captured bytes and not the manifest, so
# editing it is safe and leaving it is not.
#
# The third mode runs wipefs -a over each device after the tool wrote it, and
# captures what a released disk looks like. wipefs erases the signatures libblkid
# knows and nothing else, which is a few bytes: the point of these images is the
# residue, the structure the format left behind once the word naming it is gone.
# They are what the excision checks are read against, and they are named
# wiped-<format>.
#
# --fill puts data on the device before any of that, because an empty filesystem
# is not what a disk being released looks like. A mountable format is mounted and
# written to, so the data lands where that filesystem puts it rather than where a
# script guessed; the others are written past their metadata. Without it a
# capture says only what mkfs writes, and a head that is mostly zero is then an
# artifact of the fixture rather than a fact about the device.
#
# Usage: capture-image.sh [--region <bytes>] [--wipe] [--fill] <output-dir> [name ...]
#        capture-image.sh [--region <bytes>] --device <path> <output-dir> <name> [note]

set -euo pipefail

REGION=$((1024 * 1024)) # one mebibyte, the prober's default region size
DEVICE=
WIPE=
FILL=

while [ $# -gt 0 ]; do
    case $1 in
    --region)
        REGION=${2:?--region needs a size in bytes}
        shift 2
        ;;
    --device)
        DEVICE=${2:?--device needs a path}
        shift 2
        ;;
    --wipe)
        WIPE=1
        shift
        ;;
    --fill)
        FILL=1
        shift
        ;;
    *) break ;;
    esac
done

OUT=${1:?usage: capture-image.sh [--region <bytes>] [--device <path>] <output-dir> [name ...]}
shift || true
WANTED=("$@")

WORK=$(mktemp -d /var/tmp/blockdev-capture.XXXXXX)

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

# fill puts real data on a device that has just been formatted, when --fill is
# set. A mountable filesystem is mounted and filled to about a third of its size
# in files of random bytes, which is what puts data where that filesystem
# actually allocates it. Anything else takes the data at an offset the caller
# names, past whatever metadata that format keeps at the front.
#
# It is deliberately quiet about failing. A format this cannot fill is still
# worth capturing, and the manifest says whether it was filled.
fill() {
    local dev=$1 kind=$2 at=${3:-}
    [ -n "$FILL" ] || return 0

    if [ "$kind" = offset ]; then
        # at is a byte offset, so a format whose data area begins inside the
        # head region is filled there rather than past where anything is read.
        dd if=/dev/urandom of="$dev" bs=4096 seek=$((at / 4096)) count=64 \
            conv=notrunc status=none 2>/dev/null
        return 0
    fi

    local mnt="$WORK/mnt"
    mkdir -p "$mnt"
    mount "$dev" "$mnt" 2>/dev/null || return 0
    local free_kb count i
    free_kb=$(df -k --output=avail "$mnt" | tail -1)
    count=$((free_kb / 3 / 256)) # 256 KiB files, a third of what is free
    [ "$count" -lt 1 ] && count=1
    [ "$count" -gt 64 ] && count=64
    for i in $(seq 1 "$count"); do
        dd if=/dev/urandom of="$mnt/fill-$i" bs=256K count=1 status=none 2>/dev/null || break
    done
    sync
    umount "$mnt" 2>/dev/null
}

# capture reads the two regions off dev and writes the fixture directory.
# tool_version and command are recorded because the ext feature words, and so the
# family a reading names, depend on which generation of the tool wrote the image.
capture() {
    local name=$1 dev=$2 tool=$3 tool_version=$4 command=$5 note=${6:-}
    local dir size block probe
    if [ -n "$WIPE" ]; then
        # What wipefs erased goes into the note, because the whole value of one
        # of these images is which bytes went and which stayed.
        local erased
        erased=$(wipefs -a "$dev" 2>&1 | sed 's#^[^:]*: ##' | tr '\n' ';' | sed 's/;$//')
        note="wipefs -a reported: ${erased:-nothing to erase}. ${note}"
        command="${command}, then wipefs -a"
        name="wiped-${name}"
    fi

    dir="$OUT/$name"
    mkdir -p "$dir"

    blockdev --flushbufs "$dev" 2>/dev/null || true
    size=$(blockdev --getsize64 "$dev")
    block=$(blockdev --getss "$dev")

    # The tail is skipped to in bytes rather than in regions: a device whose
    # size is not a whole number of regions would otherwise be captured short of
    # its end, and the signatures that live in a tail are placed against the end.
    dd if="$dev" of="$dir/head.bin" bs="$REGION" count=1 iflag=direct status=none
    dd if="$dev" of="$dir/tail.bin" bs="$REGION" count=1 status=none \
        iflag=direct,skip_bytes skip=$((size - REGION))

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
  "filled": $([ -n "$FILL" ] && echo true || echo false),
  "note": "$note"
}
EOF

    gzip -9 -f "$dir/head.bin" "$dir/tail.bin"
    printf '  %-16s %s\n' "$name" "$probe"
}

ver() { "$@" 2>&1 | head -1 | tr -d '\n'; }

mkdir -p "$OUT"
echo "capturing into $OUT"

# Device mode runs alone: the catalog below formats things, and a run that was
# pointed at a device in service must not reach it.
if [ -n "$DEVICE" ]; then
    [ -b "$DEVICE" ] || {
        echo "$DEVICE is not a block device" >&2
        exit 1
    }
    name=${WANTED[0]:?usage: capture-image.sh --device <path> <output-dir> <name> [note]}
    capture "$name" "$DEVICE" "(unknown: captured from a device in service)" "unknown" \
        "(not reproducible locally: this device was read, not written)" "${WANTED[1]:-}"
    echo "done"
    exit 0
fi

if want blank; then
    dev=$(attach blank 64)
    capture blank "$dev" "none" "-" "truncate -s 64M" "never written to, the only reading that permits a format"
fi

for fs in ext2 ext3 ext4; do
    if want "$fs"; then
        dev=$(attach "$fs" 64)
        "mkfs.$fs" -q -F "$dev" >/dev/null
        fill "$dev" fs
        capture "$fs" "$dev" "mkfs.$fs" "$(ver mke2fs -V)" "mkfs.$fs -q -F"
    fi
done

if want xfs; then
    dev=$(attach xfs 512)
    mkfs.xfs -q -f "$dev" >/dev/null
    fill "$dev" fs
    capture xfs "$dev" "mkfs.xfs" "$(ver mkfs.xfs -V)" "mkfs.xfs -q -f"
fi

if want lvm2; then
    dev=$(attach lvm2 64)
    pvcreate -q -f -y "$dev" >/dev/null
    fill "$dev" offset 2097152
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
    fill "$dev" offset 1048576
    capture gpt "$dev" "sgdisk" "$(ver sgdisk --version)" "sgdisk -o -n 1:0:0 -t 1:8300" \
        "carries a protective MBR at LBA 0 as well, which is why GPT is evaluated first"
fi

if want gpt-4kn; then
    dev=$(attach gpt-4kn 64 4096)
    sgdisk -o -n 1:0:0 -t 1:8300 "$dev" >/dev/null 2>&1
    fill "$dev" offset 1048576
    capture gpt-4kn "$dev" "sgdisk" "$(ver sgdisk --version)" "sgdisk -o -n 1:0:0 -t 1:8300" \
        "4Kn: the GPT header is at offset 4096, not 512"
fi

if want mbr; then
    dev=$(attach mbr 64)
    printf 'o\nn\np\n1\n\n\nw\n' | fdisk "$dev" >/dev/null 2>&1 || true
    fill "$dev" offset 1048576
    capture mbr "$dev" "fdisk" "$(ver fdisk --version)" "fdisk: o, n, p, 1, defaults, w"
fi

for spec in "fat12 16 12" "fat16 64 16" "fat32 512 32"; do
    read -r name size bits <<<"$spec"
    if want "$name"; then
        dev=$(attach "$name" "$size")
        mkfs.vfat -F "$bits" "$dev" >/dev/null
        fill "$dev" fs
        capture "$name" "$dev" "mkfs.vfat" "$(ver mkfs.vfat --help)" "mkfs.vfat -F $bits"
    fi
done

if want exfat; then
    dev=$(attach exfat 64)
    mkfs.exfat "$dev" >/dev/null 2>&1
    fill "$dev" fs
    capture exfat "$dev" "mkfs.exfat" "$(ver mkfs.exfat --version)" "mkfs.exfat"
fi

if want btrfs; then
    dev=$(attach btrfs 256)
    mkfs.btrfs -q -f "$dev" >/dev/null
    fill "$dev" fs
    capture btrfs "$dev" "mkfs.btrfs" "$(ver mkfs.btrfs --version)" "mkfs.btrfs -q -f"
fi

if want swap; then
    dev=$(attach swap 64)
    mkswap "$dev" >/dev/null
    fill "$dev" offset 2097152
    capture swap "$dev" "mkswap" "$(ver mkswap --version)" "mkswap" \
        "the signature sits at page size minus ten, so it moves with the page size"
fi

for meta in 0.90 1.0 1.1 1.2; do
    name="mdraid-${meta//./}"
    if want "$name"; then
        dev=$(attach "$name" 64)
        mdadm --create --run --quiet "/dev/md/capture-$name" --level=1 \
            --raid-devices=2 --metadata="$meta" "$dev" missing >/dev/null 2>&1 || true
        mdadm --stop "/dev/md/capture-$name" >/dev/null 2>&1 || true
        fill "$dev" offset 2097152
        capture "$name" "$dev" "mdadm" "$(ver mdadm --version)" \
            "mdadm --create --level=1 --metadata=$meta <dev> missing" \
            "metadata $meta: 0.90 and 1.0 put the superblock in the tail region, at different offsets, and 1.1 and 1.2 put it in the head"
    fi
done

if want zfs; then
    dev=$(attach zfs 256)
    zpool create -f "capture-$$" "$dev" >/dev/null 2>&1 &&
        zpool export "capture-$$" >/dev/null 2>&1 || true
    capture zfs "$dev" "zpool" "$(ver zpool version)" "zpool create -f <pool> <dev>" \
        "vdev labels L0 and L1 in the head region, L2 and L3 in the tail"
fi

# The block-layer caches. Only one of the three writes a signature a whole disk
# carries, and the other two are captured to show that rather than to assert it:
# lvmcache is LVM, and dm-cache lives on devices the candidate rules refuse
# before anything reads them.
if want bcache; then
    dev=$(attach bcache 64)
    make-bcache -B "$dev" >/dev/null 2>&1
    fill "$dev" offset 8192
    capture bcache "$dev" "make-bcache" "$(ver make-bcache --version)" "make-bcache -B" \
        "a backing device: the superblock is at 4096 and the first block is zero, which is what makes a rule that reads only the first block unsafe"
fi

if want lvmcache; then
    dev=$(attach lvmcache 128)
    pvcreate -q -f -y "$dev" >/dev/null
    vgcreate -q -f -y "capture$$" "$dev" >/dev/null
    lvcreate -q -y -L 32M -n origin "capture$$" >/dev/null
    lvcreate -q -y -L 16M -n fast "capture$$" >/dev/null
    lvconvert -q -y --type cache --cachevol fast "capture$$/origin" >/dev/null 2>&1 || true
    capture lvmcache "$dev" "lvconvert --type cache" "$(ver lvconvert --version)" \
        "pvcreate, vgcreate, lvcreate origin and fast, lvconvert --type cache --cachevol" \
        "a whole disk holding an lvmcache: the disk is an LVM physical volume and reads as one, because the cache is metadata inside the group rather than anything on the disk"
    vgchange -q -an "capture$$" >/dev/null 2>&1 || true
    vgremove -q -f "capture$$" >/dev/null 2>&1 || true
fi

if want dm-cache-metadata; then
    dev=$(attach dm-cache-metadata 16)
    origin=$(attach dm-cache-origin 64)
    fast=$(attach dm-cache-fast 32)
    dd if=/dev/zero of="$dev" bs=4096 count=1 status=none
    blocks=$(blockdev --getsz "$origin")
    dmsetup create "capture$$" --table \
        "0 $blocks cache $dev $fast $origin 128 1 writeback default 0" >/dev/null 2>&1 || true
    dmsetup remove "capture$$" >/dev/null 2>&1 || true
    capture dm-cache-metadata "$dev" "dmsetup create ... cache" "$(ver dmsetup --version)" \
        "dmsetup create <name> --table '0 <sz> cache <meta> <fast> <origin> 128 1 writeback default 0'" \
        "the metadata device of a raw dm-cache, whose superblock is at offset 0: the origin and the fast device carry no dm-cache signature at all, and all three are logical volumes or mapper nodes in ordinary use"
fi

# A disk somebody wiped that wipefs had nothing to erase on. Every format the
# catalog knows is captured wiped by --wipe; this is the other case, where the
# tool reported success and removed no bytes at all, so nothing on the device
# says it was given up.
if want wiped-random; then
    dev=$(attach wiped-random 8)
    dd if=/dev/urandom of="$dev" bs=1M count=8 status=none
    wipefs -a "$dev" >/dev/null 2>&1 || true
    capture wiped-random "$dev" "dd and wipefs" "$(ver wipefs --version)" \
        "dd if=/dev/urandom, then wipefs -a" \
        "eight megabytes of random bytes that wipefs reported clean: it had no signature to remove and removed none, and every byte of the data is still there"
fi

echo "done"
