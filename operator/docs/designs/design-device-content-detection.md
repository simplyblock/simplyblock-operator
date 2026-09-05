# Design Document: Block Device Content Detection

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-09-04  
**Related PR:** [#481](https://github.com/simplyblock/simplyblock-operator/pull/481)  
**Amends:** [`design-node-volume-stack.md`](design-node-volume-stack.md) §5.3 and §5.5  
**Test Plan:** [`tests/test-plan-device-content-detection.md`](../tests/test-plan-device-content-detection.md)

---

## Phasing Overview

| Phase               | Status  | Evidence a format rests on                                           | Behavior change                                                       |
|---------------------|---------|----------------------------------------------------------------------|-----------------------------------------------------------------------|
| **Phase 1** (§3–§6) | Planned | The device's own bytes, read directly                                | A device that cannot be read fails staging instead of being formatted |
| **Phase 2** (§7.3)  | Planned | The above, plus the control plane's allocation figure for the volume | A volume the control plane knows holds data is never formatted        |

Phase 1 is shippable on its own and is what closes the failure this design exists
for. Phase 2 covers the one reading Phase 1 cannot distinguish (§7.2), and it
depends on a control-plane field that is not mapped today (P0-1).

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                       | Kind                    | Blocks  | Status                                                                     |
|------|------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|----------------------------------------------------------------------------|
| P0-1 | An allocation figure per volume on the control plane's volume endpoint, such as `lvol_size_used`, mapped into the generated client | Control plane (`sbcli`) | Phase 2 | Present in the API's responses, absent from `atlas-lib/internal/cpapi/gen` |
| P0-2 | `O_DIRECT` on an NVMe-oF namespace and on a device-mapper node                                                                     | Node OS                 | Phase 1 | Available                                                                  |

Without P0-1 the Phase 2 gate cannot be built, and the volume in §7.2 stays
exposed to the one reading the device's own bytes cannot answer. Without P0-2 the
reads in §4.3 fall back to buffered reads preceded by a cache drop, which is
weaker against a stale page cache and is a degradation rather than a failure.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [The Reading](#3-the-reading)
4. [Establishing Blank](#4-establishing-blank)
5. [Signature Catalog](#5-signature-catalog)
6. [Mapping a Reading onto `State`](#6-mapping-a-reading-onto-state)
7. [Evidence Beyond the Device](#7-evidence-beyond-the-device)
8. [Consumers](#8-consumers)
9. [Failure Modes and Fallback](#9-failure-modes-and-fallback)
10. [Configuration](#10-configuration)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration Strategy](#13-migration-strategy)
14. [Open Questions](#14-open-questions)

Appendices:

- [Appendix A: `blockdev` content API](#appendix-a-blockdev-content-api)

---

## Overview

[`design-node-volume-stack.md`](design-node-volume-stack.md) §4.2 states the rule
that governs every irreversible write the node plugin makes: `StateAbsent` is
"the only circumstance under which a layer may format anything," and telling
`StateAbsent` from `StateInactive` is "the one that loses data when it is wrong."
The rule is right. The evidence the layers use to establish it is not.

Two layers answer that question today by asking a tool whether it recognizes
something. The `filesystem` layer formats "when `blkid` shows it is unformatted"
(§5.5), and the `lvmPV` layer reads the on-disk LVM signature (§5.3). Both tools
report *what they recognized*, and neither can report *whether they could read the
device at all*: `blkid` answers "no filesystem here" and "this device could not be
read" with the same exit code, no output, and nothing on stderr, and `pvs` says
`Failed to find physical volume` for both. An absence of recognition is being read
as an absence of data.

This design replaces that evidence. Instead of asking a tool what it recognized,
the node reads the device's own bytes and answers one of four readings, with a
read failure reported as a failure rather than folded into any of them. `Blank`
becomes a positive finding, established by reading the regions every on-disk
format writes to and finding them zero, so it means "this device was read and it
holds nothing" rather than "nothing familiar turned up." That is the reading a
`mkfs` or a `pvcreate` may rest on.

The detection lives in `atlas-lib/blockdev`, beside the `Device` value
[`design-node-volume-stack.md`](design-node-volume-stack.md) Appendix A specifies,
whose size and block size it consumes.

---

## 1. Background

On 2026-09-03 a node plugin ran `mkfs.ext4 -F` over a 2 TiB volume holding 1.1 TiB
of production data. Both storage nodes serving the volume's replicas were
restarted seven minutes apart, the kernel remounted the filesystem read-only, and
kubelet retried `NodeStageVolume`. The plugin reconnected the fabric, probed the
device 174 milliseconds later while no controller was yet live, and formatted it.
The kernel logged the first live controller for that subsystem 18 seconds after
the format began.

The probe did not misbehave. `blkid` read a device whose every read failed,
gathered nothing, and exited 2, which is the same exit code it returns for a
genuinely blank device. The driver's own comment recorded the ambiguity honestly
and resolved it the unsafe way: exit 2 became `("", nil)`, and an empty answer
authorized a format.

[#481](https://github.com/simplyblock/simplyblock-operator/pull/481) narrowed the
window from the other side. A probe that fails outright and a device carrying a
partition table now refuse staging, and a claim annotation recording the on-disk
filesystem settles the blank reading when it is present. That guard holds for
volumes the annotation covers, and it is compensating for the probe rather than
fixing it: the annotation is read from the Kubernetes API, which is unreachable in
exactly the cluster-wide incidents that also break the data path.

**The conflation is not only about devices that cannot be read.** A device
carrying a Linux software-RAID member with metadata version 1.1 puts its
superblock at offset 0, and `blkid -p -s TYPE -s PTTYPE -o export` reports
nothing for it and exits 2, which is the reading that authorizes a format. The
device is healthy, the host is healthy, and nothing about the fabric is involved:
`mdadm --examine` reads the magic off the same device in the same second.
Reproduced on Ubuntu 24.04 with `mdadm` 4.3 and `util-linux` 2.39.3, and captured
as the `mdraid-11` fixture. So the evidence a format rests on today fails in two
independent ways, and only one of them needs a broken data path: what a tool
recognizes is narrower than what a device holds, and the gap between them is
where a volume gets reformatted.

The same shape sits one layer down. `lvm.Manager.VolumeGroup` distinguishes "no PV
signature" from a probe failure by matching `pvs`'s output text for `failed to
find` or `not found`, and its doc comment names the hazard precisely: a caller
reading a probe failure as blank "would proceed straight to `pvcreate`/`vgcreate`
over what might be live, merely unreadable data." The text match catches the
failure case. It cannot catch a device that reads successfully and returns
nothing, which is the case §7.2 describes.

A third instance is already specified and not yet built.
[`design-pnfs-striped.md`](design-pnfs-striped.md) §2.1 assembles a stripe with
`pvcreate` on each member and then `mkfs.xfs` on the logical volume "only when
`blkid` shows it is unformatted."

---

## 2. Goals and Non-Goals

### Goals

- Answer what a block device carries with a reading that distinguishes "nothing is
  here" from "this could not be read" and from "something is here that this driver
  did not put there."
- Make `Blank` a positive finding: established by reading, never inferred from a
  tool's silence.
- Report a read failure as an error, so a device whose fabric is down fails an
  operation rather than authorizing one.
- Give the `filesystem` and `lvmPV` layers of
  [`design-node-volume-stack.md`](design-node-volume-stack.md) an `Observe` that
  establishes `StateAbsent` under the contract its §4.2 already states.
- Carry no dependency on `util-linux`, `lvm2`, or any other userspace tool for the
  reading itself, so the answer does not change with the node image's package
  versions.
- Keep the seam testable without a kernel, a device, or a cluster, and usable
  against a device on another host, which is what the integration harness needs.

### Non-Goals

- **General-purpose filesystem identification.** `libblkid` recognizes upward of a
  hundred formats. This recognizes the two this driver creates, the one stack
  layer it assembles, and enough of the rest to name a refusal usefully (§5).
  Everything unrecognized is refused, so the catalog's completeness is a matter of
  message quality rather than of safety.
- **Replacing `blkid` for diagnostics.** It stays in the node image and stays
  useful for an operator on a host. What it stops doing is deciding.
- **Distinguishing a storage layer that serves zeros from a blank device.** No
  reading of the device's own bytes can (§7.2). That case is Phase 2's, and it is
  answered from the control plane rather than from the device.
- **Deciding whether a device *should* be formatted.** The detector reports what is
  there. Whether a volume that is genuinely blank is one this driver may format is
  the provenance question (§7.3) and the claim annotation's (§7.1).
- **Repairing a device.** No verb here writes.

---

## 3. The Reading

```go
// Content is what a device was found to carry. The zero value permits nothing,
// so a Reading that was never populated cannot authorize a format.
type Content int

const (
	// ContentUnknown is the zero value and is never returned by Read.
	ContentUnknown Content = iota

	// ContentBlank means every byte of the probed regions was read successfully
	// and was zero. It is the only reading that permits a format.
	ContentBlank

	// ContentFilesystem means the device carries a filesystem this driver
	// mounts. Reading.Type names it.
	ContentFilesystem

	// ContentStackLayer means the device carries a layer that owns it without
	// being mountable, which today is an LVM physical-volume label.
	ContentStackLayer

	// ContentForeign means the device carries something else: a recognized
	// format this driver does not create, or bytes that match nothing known.
	ContentForeign
)
```

**`ContentUnknown` is the zero value deliberately.** A `Reading` that a caller
constructed, defaulted, or failed to populate is the one case where a wrong answer
is silent, so the zero value is the reading that authorizes nothing. The
permission to `mkfs` belongs to a value a caller has to set on purpose.

**Four readings rather than a boolean.** "Formatted or not" is the shape that
produced the incident, because it has no room for the answer "the device could not
be read." The four separate the three questions a caller actually has: may this be
formatted (`ContentBlank` alone), is this a filesystem this driver mounts
(`ContentFilesystem`), is this a layer this driver activates
(`ContentStackLayer`), and does it belong to something else (`ContentForeign`).

```go
// Reading is what one probe of a device found.
type Reading struct {
	Content Content

	// Type names the format found, in the spelling the consuming tool uses:
	// "ext4" and "xfs" are mount types, "LVM2_member" is the physical-volume
	// label. It is empty for ContentBlank.
	Type string

	// Detail is the human-readable finding for an event, a log line, and the
	// error text of a refusal. It says where the signature was found, so a
	// refusal can be checked by hand.
	Detail string
}
```

A read failure is not a `Content`. `Read` returns `(Reading{}, error)`, and every
caller treats the error as a refusal rather than mapping it onto a reading, which
is the property the whole design turns on.

---

## 4. Establishing Blank

### 4.1 The regions

A device is `ContentBlank` when the first mebibyte and the last mebibyte both read
successfully and contain only zero bytes.

Every on-disk format this driver can encounter writes an identifying structure
into one of those two regions. The head holds the XFS superblock at offset 0, the
LVM label in one of the first four sectors, the ext superblock at 1024, the LUKS
header at 0, a GPT header at 512, an MBR signature at 510, a swap signature near
the end of the first page, the Btrfs superblock at 65536, an md-raid 1.1 or 1.2
superblock at 0 or 4096, and the first two ZFS vdev labels at 0 and 262144. The
tail holds a backup GPT header in the last sector, an md-raid 1.0 superblock 8 KiB
from the end, and the last two ZFS vdev labels 512 KiB and 256 KiB from the end.
One mebibyte at each end covers all of them with room to spare.

**The rule is what makes the catalog non-safety-critical.** A format nobody
listed in §5 still writes bytes into one of these regions, so it fails the zero
test and is refused as `ContentForeign` rather than mistaken for blank. The
catalog decides how well a refusal is worded, not whether it happens.

A device smaller than two mebibytes is read whole, and the two regions overlap
rather than being clamped.

### 4.2 Why a tail read

The tail region is the reason md-raid 1.0 and a backup GPT are covered, and it is
also the only part of this design that observes the device across its span. A
device serving its first blocks from a cache while its backing path is gone
answers the head read and fails the tail read, and a failed read is a refusal.
The tail read is not a liveness check for the whole device and is not offered as
one: it is one more place a failure can be caught, at the cost of one seek.

### 4.3 Bounded, cache-bypassing reads

Reads are issued with `O_DIRECT`, aligned to the device's logical block size,
which `blockdev.Device` reports. The page cache is the hazard the flag removes: a
device that was read before its fabric broke can serve its old contents, or zeros
for a region never faulted in, from cache after the fact, and a probe answering
from cache is answering about the past. When `O_DIRECT` is refused, the read falls
back to a buffered read preceded by a `BLKFLSBUF` cache drop, and the reading is
annotated as having taken the fallback so the difference is visible in the metric
(§11).

Every read is bounded by the caller's context. A read that has not returned by the
deadline is abandoned and reported as a failure, which is a refusal. This does not
cancel the kernel's outstanding I/O, which no userspace caller can, and the
distinction that matters is not whether the read is stopped: it is that a probe
which never answered is reported as a probe that never answered, rather than as an
empty result. That is the whole of the difference between this and a `blkid` that
hangs and then exits 2.

---

## 5. Signature Catalog

| Format           | `Content`           | Where                                         | Match                                                                        |
|------------------|---------------------|-----------------------------------------------|------------------------------------------------------------------------------|
| ext2, ext3, ext4 | `ContentFilesystem` | 1080, little-endian `uint16`                  | `0xEF53`, with the feature words at 1116, 1120, and 1124 deciding the family |
| XFS              | `ContentFilesystem` | 0, big-endian `uint32`                        | `XFSB`                                                                       |
| LVM2             | `ContentStackLayer` | start of one of the first four 512-byte units | `LABELONE`, with `LVM2 001` at offset 24 of the label                        |
| LUKS             | `ContentForeign`    | 0                                             | `LUKS\xba\xbe`                                                               |
| GPT              | `ContentForeign`    | LBA 1, and the last LBA for the backup header | `EFI PART`, the GPT header signature                                         |
| MBR              | `ContentForeign`    | 510                                           | `0x55AA` with a non-empty partition entry                                    |
| FAT12, FAT16     | `ContentForeign`    | 0x36, with the BPB at 0x0B validated          | `FAT12   `, `FAT16   `, or `FAT     `                                        |
| FAT32            | `ContentForeign`    | 0x52, with the BPB at 0x0B validated          | `FAT32   `                                                                   |
| exFAT            | `ContentForeign`    | 3                                             | `EXFAT   `                                                                   |
| Btrfs            | `ContentForeign`    | 65600                                         | `_BHRfS_M`                                                                   |
| swap             | `ContentForeign`    | page size minus 10                            | `SWAPSPACE2` or `SWAP-SPACE`                                                 |
| md-raid          | `ContentForeign`    | 0, 4096, or 8 KiB from the end                | `0xa92b4efc`                                                                 |
| ZFS              | `ContentForeign`    | vdev label offsets                            | `0x00bab10c`                                                                 |
| anything else    | `ContentForeign`    | anywhere in the probed regions                | a non-zero byte                                                              |

**An offset counted in logical blocks is resolved against the device, not against
512.** The GPT header is at LBA 1 and its backup is at the last LBA, which is
offset 512 on a 512-byte-sector device and offset 4096 on a 4Kn one. NVMe
namespaces are formatted with either, so an offset hardcoded to 512 misses GPT
entirely on a 4Kn namespace. `Device.LogicalBlockSize` is what these rows are
resolved against, which is one of the two fields this design consumes from
[`design-node-volume-stack.md`](design-node-volume-stack.md) Appendix A. The LVM
label is the exception that proves it: LVM scans the first four 512-byte units
regardless of the device's logical block size, so that row is counted in 512-byte
units by the format's own definition rather than by the device's geometry.

**The ext family is decoded to its exact member and mounted with one driver.** The
feature words distinguish ext2, ext3, and ext4, and the reading names what is
actually there because a reading that rounded every ext filesystem up to `ext4`
would disagree with the claim annotation for a volume this driver did not create.
All three mount through the kernel's ext4 driver, so the distinction costs the
`filesystem` layer nothing.

**A partition table is `ContentForeign` and not a stack layer.** This driver never
partitions a volume, so a partition table is something another system wrote, and
the layer that would consume it does not exist. That matches the refusal
[#481](https://github.com/simplyblock/simplyblock-operator/pull/481) already
ships.

**GPT is evaluated before MBR, because a GPT disk carries both.** A protective
MBR at LBA 0 with a partition entry of type `0xEE` is part of a well-formed GPT,
so a device matching both is reported as GPT, which is the name an operator needs
in order to find what is on the device.

**The FAT family is recognized because a disk arrives carrying one.** A volume
restored from an image, a raw-block PV that previously held an EFI system
partition, and a physical disk handed to a storage cluster at deployment are all
routes by which a FAT filesystem reaches a device this driver is asked to
initialize. FAT has no single magic number, so the match validates the BIOS
parameter block alongside the type string: a bytes-per-sector that is a power of
two between 512 and 4096, a FAT count of one or two, and a media descriptor of
`0xF0` or at least `0xF8`. A FAT boot sector also carries `0x55AA` at 510 and can
therefore match the MBR row, and that ambiguity is cosmetic rather than
consequential: both readings are `ContentForeign`, both refuse, and only the
wording of the refusal differs.

**The order of evaluation is head signatures, then tail signatures, then the zero
test.** A device carrying both an LVM label and a stale filesystem signature is
reported as the LVM label, because the label is at the lower offset and is what
the stack below is looking for. A refusal names every signature found, not the
first, so an operator sees the whole picture in one message.

---

## 6. Mapping a Reading onto `State`

The two layers of [`design-node-volume-stack.md`](design-node-volume-stack.md)
that can create a durable object dispatch their `Observe` on the reading. A
reading that has no legitimate meaning for a layer is an `Observe` error, which is
the contract's refusal path (§4.1 of that document).

**`filesystem` (§5.5):**

| Reading                                  | `State`                                   |
|------------------------------------------|-------------------------------------------|
| `ContentBlank`                           | `StateAbsent`, and `Ensure` may `mkfs`    |
| `ContentFilesystem`, mounted at the path | `StateReady`                              |
| `ContentFilesystem`, not mounted         | `StateInactive`, and `Ensure` mounts only |
| `ContentStackLayer`                      | `Observe` error                           |
| `ContentForeign`                         | `Observe` error                           |
| read failure                             | `Observe` error                           |

The `ContentFilesystem` rows are the ones that close the remaining window in the
shipped guard. A device carrying a filesystem is mounted as the filesystem it
carries and is never handed to `SafeFormatAndMount`, whose own internal probe
would otherwise re-decide the question on evidence this design has just replaced.
That also removes the `fsck -a` that helper runs against every existing filesystem
it mounts read-write, which writes to a device whose path state the layer is in no
position to judge.

**`lvmPV` (§5.3):**

| Reading                                               | `State`                                    |
|-------------------------------------------------------|--------------------------------------------|
| `ContentBlank`                                        | `StateAbsent`, and `Ensure` may `pvcreate` |
| `ContentStackLayer`, label naming this volume's group | `StateReady`                               |
| `ContentStackLayer`, label naming another group       | `StateForeign`                             |
| `ContentFilesystem`                                   | `Observe` error                            |
| `ContentForeign`                                      | `Observe` error                            |
| read failure                                          | `Observe` error                            |

Which volume group a label names is a question about the label's content rather
than about the device, so `lvmPV` reads it with `lvm.Manager.VolumeGroup` as it
does today. What changes is that the call is made only after the reading has
established that a label is there, so `pvs` is never the thing that decides
whether the device is empty.

---

## 7. Evidence Beyond the Device

Three independent kinds of evidence bear on whether a format is safe. They cover
different failures, and the design names all three because two of them are already
built and the boundary between them is where the remaining exposure sits.

### 7.1 The claim annotation

`storage.simplyblock.io/on-disk-filesystem` records the filesystem a volume was
staged with, and
[#481](https://github.com/simplyblock/simplyblock-operator/pull/481) reads it back
to settle a blank probe. It covers a device that reads blank when the volume is
known to have been formatted, and it is read from the Kubernetes API.

Its limitation is that availability: the API server is unreachable in exactly the
cluster-wide incidents that also break a data path, and a node plugin restarted
during such an incident has no informer cache to fall back on. The shipped
behavior on an unreadable claim is to refuse staging, which is the right direction
and is why the annotation remains useful after this design lands. It stops being
the only thing between a degraded device and `mkfs`.

### 7.2 What the device's own bytes cannot answer

A storage layer that returns zeros for successful reads is indistinguishable from
a blank device by any amount of reading. The bytes arrive, they are zero, and the
device reports no error. Phase 1 reports `ContentBlank` for such a device, and a
caller with no annotation formats it.

This is a real state rather than a theoretical one. The incident's own measurements
found the volume serving zeros at 83 kB/s during the retry storm, and while those
reads are consistent with blocks the first `mkfs` had already discarded rather than
with the trigger, a storage path that answers reads with zeros while the volume
holds data is a state this stack can enter. **No reading of the device settles it,
and this design does not claim otherwise.**

### 7.3 Provenance (Phase 2)

The control plane knows how much of a volume is allocated, and a volume with a
non-zero allocation has been written to. That figure is evidence about the volume
rather than about the path to it, so it survives the case §7.2 cannot answer.

The Phase 2 gate is: before any layer formats, read the volume's allocation figure
from the control plane, and refuse when it is non-zero. A volume that has never
been written to reports zero and is formatted, which is the provisioning case. A
volume with data reports non-zero and is never formatted, whatever the device
appears to say and whether or not the Kubernetes API is reachable.

The gate degrades in one direction only: a control plane that cannot be reached
leaves Phase 1's reading as the answer, which is where Phase 1 already stands. It
degrades that way because the alternative couples the provisioning of every new
volume to a second service's availability, and the prerequisite that decides
whether that trade is available at all is P0-1.

---

## 8. Consumers

**Before the stack lands**, the reading has one call site: `stageVolume` in
`csi-driver/pkg/spdk/nodeserver.go`, which replaces its `blkid` preflight with
`blockdev.Read` and dispatches on the reading exactly as §6's `filesystem` table
specifies. This is the whole of Phase 1's behavior change, and it is what makes
Phase 1 shippable without waiting for
[`design-node-volume-stack.md`](design-node-volume-stack.md) Phase 1.

**After the stack lands**, the call sites are the `Observe` implementations of the
`filesystem` and `lvmPV` layers, and `stageVolume`'s copy disappears with the rest
of the inline node-service logic.

**The pNFS striped assembly** ([`design-pnfs-striped.md`](design-pnfs-striped.md)
§2.1) reaches the same gate through those layers, and its steps 2 and 5, the
`pvcreate` per member and the `mkfs.xfs` on the logical volume, stop naming
`blkid` as what decides them.

**The integration harness** reads a device on another host through the same API,
with a `Reader` backed by a node shell rather than by a local file descriptor.
That is the second consumer that keeps the seam honest, and it is what lets §12's
integration tests assert the reading against a real kernel.

---

## 9. Failure Modes and Fallback

| Failure                                            | Detection                       | Behavior                                                                                                      |
|----------------------------------------------------|---------------------------------|---------------------------------------------------------------------------------------------------------------|
| A read fails with an I/O error                     | The read returns an error       | `Read` returns an error. Every consumer refuses. Kubelet retries, and the next attempt reads the device again |
| A read does not return before the deadline         | The context expires             | Reported as a read failure, with the region and the elapsed time in the message                               |
| `O_DIRECT` is refused                              | `EINVAL` on open                | Buffered read after `BLKFLSBUF`, and the reading is annotated as degraded (§11)                               |
| The device is smaller than the two regions         | The size from `blockdev.Device` | The whole device is read once and evaluated as one region                                                     |
| The device size cannot be determined               | `blockdev.Device` reports zero  | Read failure. A tail region cannot be located, and a head-only reading is not `ContentBlank`                  |
| A signature is found in both regions               | Both matched                    | Both are named in `Detail`, and the head signature decides `Type`                                             |
| A recognized format sits beyond the probed regions | Not detectable                  | Refused anyway, because such a device is not all zeros in the regions that were read                          |
| The control plane is unreachable (Phase 2)         | The client call fails           | The Phase 1 reading stands, and the refusal is logged as ungated by provenance (§7.3)                         |
| The claim cannot be read                           | The API call fails              | Unchanged from [#481](https://github.com/simplyblock/simplyblock-operator/pull/481): staging is refused       |

Every row that is not a refusal is a reading. There is no path on which a failure
becomes an empty result, which is the single property this design is built to
hold.

---

## 10. Configuration

| Field                   | Type      | Default | Description                                                                                      |
|-------------------------|-----------|---------|--------------------------------------------------------------------------------------------------|
| `SPDKCSI_PROBE_REGION`  | byte size | `1Mi`   | The size of each probed region. Lowering it below the largest offset in §5 is refused at startup |
| `SPDKCSI_PROBE_TIMEOUT` | duration  | `30s`   | The bound on one region's read. A degraded device is expected to exhaust it                      |
| `SPDKCSI_PROBE_SHADOW`  | boolean   | `true`  | Run `blkid` beside the reading and count disagreements (§13). Never decides anything             |

All three are node plugin environment variables rather than `StorageClass`
parameters or CRD fields, because they configure how a host reads its own devices
and no per-volume answer differs. They are read once at startup, and a change
takes effect when the DaemonSet rolls.

---

## 11. Observability

The CSI node plugin emits no Kubernetes events and exposes no Prometheus metrics
today, as [`design-node-volume-stack.md`](design-node-volume-stack.md) §14 records.
Both tables below are new infrastructure, and both are Phase 1 work: a refusal
that an operator cannot see is a volume that will not stage for a reason nobody
can name.

### Kubernetes Events

The PVC is the target, for the reason that document gives: it is the object a user
owns and looks at, and it outlives the pod. The node plugin already resolves the
PVC for a volume handle through the shared `sbkube.Manager` in order to read the
on-disk-filesystem annotation.

| Event                                                                        | Type    | Reason                    |
|------------------------------------------------------------------------------|---------|---------------------------|
| A device's contents could not be read, so the operation was refused          | Warning | `DeviceUnreadable`        |
| A device carries content this driver did not create, so it was not formatted | Warning | `DeviceContentForeign`    |
| A device was read as blank and formatted                                     | Normal  | `DeviceFormatted`         |
| A format was refused because the volume's allocation is non-zero (Phase 2)   | Warning | `DeviceProvenanceRefused` |
| The reading and the tool it shadows disagreed (§13)                          | Warning | `DeviceReadDisagreement`  |

`DeviceUnreadable` is the event for the condition the 2026-09-03 incident hit. It
lands on the PVC at the moment the plugin finds it cannot tell. `DeviceFormatted` is
`Normal` and is emitted once per volume in its life, which makes it the record of
when a volume's data began.

### Prometheus Metrics

| Metric                                             | Labels             | Description                                                                                     |
|----------------------------------------------------|--------------------|-------------------------------------------------------------------------------------------------|
| `simplyblock_csi_node_device_readings_total`       | `content`          | Readings by outcome, with `error` as one value, so refusals are countable rather than anecdotal |
| `simplyblock_csi_node_device_read_seconds`         | `region`, `result` | Histogram of one region's read, which is where a degraded path shows as a long tail             |
| `simplyblock_csi_node_device_reads_degraded_total` | —                  | Reads that fell back from `O_DIRECT` to a buffered read                                         |
| `simplyblock_csi_node_format_refused_total`        | `reason`           | Formats refused, by the condition that refused them                                             |
| `simplyblock_csi_node_probe_disagreements_total`   | `tool`             | Disagreements between the reading and the shadowed tool (§13)                                   |

`simplyblock_csi_node_format_refused_total` is the alert. A non-zero rate means
volumes are not staging and an operator has to look, and its `reason` label
distinguishes a fabric problem from a device somebody else wrote to.
`simplyblock_csi_node_probe_disagreements_total` is the gate for §13: it has to
reach zero and stay there before the shadow is removed, and a non-zero value on a
device that stages successfully is a bug in this design rather than in the tool.

---

## 12. Testing Strategy

The risk concentrates in §4 and §6. A `ContentBlank` returned for a device that
holds data is the failure this design exists to prevent, and a reading misdispatched
in §6 reintroduces it one layer up.

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-device-content-detection.md`](../tests/test-plan-device-content-detection.md)

- **Unit:** every row of §5 against a region captured from a device a real tool
  formatted, and every reading in §3 through the `Reader` seam, with no kernel and
  no device at test time. **The fixtures are captures rather than constructions.**
  A fixture built from the offsets in §5 tests that the decoder agrees with the
  table, which is the same claim twice and passes even when both are wrong about
  what `mkfs` writes. A captured region is evidence independent of the table, so a
  §5 row that misstates an offset, a byte order, or a feature word fails the test
  instead of being confirmed by it. The zero-region rule is tested at its
  boundaries with constructed inputs, because a boundary is a property of the rule
  rather than of any format. The §6 dispatch tables in full, including every row
  that must be an `Observe` error.
- **Integration:** the readings against a real kernel and live devices, on the
  nvmet harness in `test/integration`. What this adds beyond the captures is the
  device rather than the format: a namespace whose target has withdrawn its port
  must read as a failure rather than as blank, a 4Kn namespace must resolve the
  LBA-counted offsets against its own block size, and the `O_DIRECT` and
  stale-cache behavior needs a page cache to defeat. It is also where a capture is
  re-taken, so a fixture and the tool that produced it can be compared on a
  current image.
- **E2E:** the incident, on a live cluster: a volume with data, a fabric that goes
  away, a restage, and the filesystem UUID unchanged afterward. This exists
  already as `SPDKCSI-BLKID-UNREADABLE` and is retargeted rather than rewritten.
- **Load:** none. Nothing here is on a hot path, and the read happens once per
  stage.

Phase 2's gate is unit-testable against a mock control plane and is not testable
end to end until P0-1 lands.

---

## 13. Migration Strategy

**Phase 1 ships with `blkid` running beside the reading and deciding nothing.**
Both run on every stage, the reading decides, and a disagreement is logged with
both answers and counted in
`simplyblock_csi_node_probe_disagreements_total`. The purpose is not to validate
the reading against `blkid`, whose answer is the one being replaced. It is to
find the devices where the new rule refuses something the old one accepted:
a device carrying stray non-zero bytes that no format wrote, which `blkid` called
blank and this design calls `ContentForeign`. That case is a fail-safe refusal and
a legitimate provisioning failure at the same time, and it is better found by a
counter than by a stuck PVC.

**The shadow is removed when the counter has been zero across a release.** Until
then it is on by default and can be turned off per node with
`SPDKCSI_PROBE_SHADOW` (§10).

**No on-disk state changes and no volume is rewritten.** A volume staged by an
older driver stages identically under this one, with the single exception that a
device which cannot be read is now refused rather than formatted. The claim
annotation (§7.1) keeps its meaning and its writer.

**`blkid` stays in the node image.** It is the diagnostic an operator on a host
reaches for, and it is how a refusal this design produces is checked by hand.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                   | Owner                   |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| 1   | **The allocation figure's exact name and semantics.** §7.3 needs a per-volume number that is non-zero once a volume has been written to. Whether `lvol_size_used` is that number, whether it is zero for a freshly provisioned thin volume, and whether a clone reports its source's usage are unconfirmed | Control plane (`sbcli`) |
| 2   | **Whether a non-zero allocation should refuse a format or only demand a second opinion.** A volume whose allocation is non-zero because the control plane counts metadata would make every first stage fail. The answer depends on Q1 and decides whether Phase 2 is a gate or an input                    | Control plane (`sbcli`) |
| 3   | **Whether the tail read belongs in the reading or in the layer.** It is specified here because `ContentBlank` needs it (§4.1). A layer that wants a liveness check across the device's span wants something stronger, and building that separately would read the tail twice                               | —                       |
| 4   | **Whether `ContentStackLayer` needs to name the layer.** Today it is always LVM, and §6 has `lvmPV` re-read the label to find the volume group. A second stack layer would make `Type` load-bearing rather than informational                                                                              | —                       |

---

## Appendix A: `blockdev` content API

The content half of `atlas-lib/blockdev`, beside the `Device` value
[`design-node-volume-stack.md`](design-node-volume-stack.md) Appendix A specifies.
That appendix notes a resolver "can be added beside it when a consumer needs one."
This is that consumer, and it reads `Device.SizeBytes` for the tail region and
`Device.LogicalBlockSize` for `O_DIRECT` alignment.

```go
// Reader is bounded access to a block device's bytes. It carries the context on
// the method rather than at construction because a bounded read is the whole
// point: a probe that cannot be abandoned is a probe that reports nothing when a
// path is gone, which is the failure this package exists to prevent.
type Reader interface {
	// ReadAt fills p from off. A short read is an error, not a partial answer.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Close() error
}

// Opener opens a device for probing. The default opens O_DIRECT on the local
// host; the integration harness supplies one that reads a device on another
// machine through a node shell.
type Opener func(ctx context.Context, dev Device) (Reader, error)

// Prober reads what a block device carries.
type Prober struct {
	// contains filtered or unexported fields
}

// NewProber returns a Prober reading local devices with O_DIRECT.
func NewProber(opts ...Option) *Prober

// NewProberWithOpener returns a Prober reading through open.
func NewProberWithOpener(open Opener, opts ...Option) *Prober

// Read reports what dev carries. It never writes to the device.
//
// A returned error means the device could not be read, and it is never a
// reading: a caller that treats it as ContentBlank reintroduces the defect this
// package was written to remove.
func (p *Prober) Read(ctx context.Context, dev Device) (Reading, error)

// Option configures a Prober.
type Option func(*Prober)

// WithRegionSize sets the size of each probed region. It is refused below the
// largest offset in the signature catalog.
func WithRegionSize(bytes int64) Option

// WithTimeout bounds one region's read.
func WithTimeout(d time.Duration) Option
```

**`Read` takes a `Device` rather than a path.** The size and the block size it
needs are two more inspections of a path at the call site, which is the shape
Appendix A of the volume stack design removes by carrying them on the value. A
consumer holding only a path resolves the `Device` first, and that resolution is
one place rather than three.

**There is no `Format` and no `IsBlank`.** Both are the boolean shape §3 rejects:
a caller asking "is this blank" has already decided that the answer has two
values, and the reading it needs has four.
