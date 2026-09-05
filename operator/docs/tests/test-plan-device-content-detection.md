# Test Plan: Block Device Content Detection

**Related design:** [`../designs/design-device-content-detection.md`](../designs/design-device-content-detection.md)

**Harness:** `atlas-lib` (`make -C atlas-lib test`), `csi-driver`
(`make -C csi-driver unit-test`, `e2e-test`), and the nvmet integration suite
(`make -C test/integration test`, which boots a Talos cluster in QEMU and needs
root).

**Legend:**

| Prefix | Class       | Harness                                                                               |
|--------|-------------|---------------------------------------------------------------------------------------|
| `U-`   | Unit        | Go, byte fixtures through the `Reader` seam, no kernel, no device, no cluster         |
| `I-`   | Integration | Real kernel and real block devices on the nvmet harness, no simplyblock control plane |
| `E-`   | End-to-end  | Live simplyblock cluster, real data path, real fabric faults                          |
| `M-`   | Manual      | Needs a fault this repository cannot inject automatically yet                         |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when the scenario is not covered yet.

---

## 1. Unit Tests — The Reading

Drive `blockdev.Prober.Read` through the `Reader` seam, with no kernel and no
device at test time.

**The format fixtures are captured from real devices, not constructed from design
§5.** A constructed fixture asserts that the decoder agrees with the table, which
is one claim tested twice: a §5 row that names the wrong offset, the wrong byte
order, or the wrong feature word produces a fixture that matches it, and the test
passes while the reading is wrong about every real device. A capture is evidence
the table did not produce. Each one is the head and tail region of a device that
`mkfs.ext4`, `mkfs.xfs`, `mkfs.vfat`, `pvcreate`, `cryptsetup`, `sgdisk`, or
`mkswap` actually wrote, taken with the same region sizes the prober reads.

**Boundary inputs stay constructed**, because the zero-region rule is a property
of the rule rather than of any format, and a captured image cannot place a byte at
exactly the last offset of a region on request.

Fixtures live under `atlas-lib/blockdev/testdata/images/<name>/` as a gzipped
`head.bin` and `tail.bin` beside a `manifest.json` recording the tool, its
version, the exact command line, the device size and logical block size, the
capture date, and a checksum per blob. The regions are almost entirely zeros, so
each fixture compresses to a few kilobytes. `hack/blockdev/capture-image.sh`
regenerates them against loop devices and needs root, so it is run by hand and
never in CI, which is the arrangement `hack/nvmet/capture-sysfs.sh` already uses
for the sysfs snapshots.

**Provenance is load-bearing rather than bookkeeping.** The ext feature words
decide which family a reading names, and they differ across `e2fsprogs`
generations: this repository has already been bitten by an image whose `mkfs`
version decided its on-disk features. A fixture that does not say which tool wrote
it cannot answer why it decodes the way it does.

Files: `atlas-lib/blockdev/content_test.go` for the readings, and
`atlas-lib/blockdev/fixture_test.go` for loading a capture and checking it
against its own manifest.

### Signature Catalog (design §5)

| #    | Scenario                                                                                                          | Type       | Test                                                    |
|------|-------------------------------------------------------------------------------------------------------------------|------------|---------------------------------------------------------|
| U-01 | ext4 superblock at 1080 reads as `ContentFilesystem`, `Type` `ext4`                                               | Positive   | `TestReadingOfCapturedImages`                           |
| U-02 | ext3 feature words read as `ext3`, not `ext4`                                                                     | Positive   | `TestReadingOfCapturedImages`                           |
| U-03 | ext2 feature words read as `ext2`                                                                                 | Positive   | `TestReadingOfCapturedImages`                           |
| U-04 | XFS magic at offset 0 reads as `ContentFilesystem`, `Type` `xfs`                                                  | Positive   | `TestReadingOfCapturedImages`                           |
| U-05 | LVM `LABELONE` in sector 1 reads as `ContentStackLayer`                                                           | Positive   | `TestReadingOfCapturedImages`                           |
| U-06 | LVM label in sectors 0, 2, and 3 is found as well                                                                 | Boundary   | `TestLVMLabelInAnyOfTheFirstFourSectors`                |
| U-07 | `LABELONE` without `LVM2 001` at offset 24 is not a stack layer                                                   | Negative   | `TestLabelWithoutTheLVMTypeIsNotAStackLayer`            |
| U-08 | LUKS header reads as `ContentForeign`, named in `Detail`                                                          | Negative   | `TestReadingOfCapturedImages`                           |
| U-09 | A captured GPT header at LBA 1 reads as `ContentForeign`                                                          | Negative   | `TestReadingOfCapturedImages`                           |
| U-10 | MBR signature with a non-empty entry reads as `ContentForeign`                                                    | Negative   | `TestReadingOfCapturedImages`                           |
| U-11 | MBR signature with an empty table is not a partition table                                                        | Boundary   | `TestBootSignatureWithAnEmptyTableIsNotAPartitionTable` |
| U-12 | Btrfs, swap, md-raid, and ZFS each read as `ContentForeign`                                                       | Negative   | `TestReadingOfCapturedImages`                           |
| U-13 | An unrecognized non-zero byte reads as `ContentForeign`                                                           | Negative   | `TestOneNonZeroByteDefeatsBlank`                        |
| U-14 | A device carrying both an LVM label and a stale filesystem reports the label as `Type` and names both in `Detail` | Boundary   | `TestLVMLabelBeatsAStaleFilesystemAndBothAreNamed`      |
| U-54 | A captured FAT16 boot sector reads as `ContentForeign` and is named as FAT16                                      | Negative   | `TestReadingOfCapturedImages`                           |
| U-55 | A captured FAT32 boot sector reads as `ContentForeign` and is named as FAT32                                      | Negative   | `TestReadingOfCapturedImages`                           |
| U-56 | A captured exFAT boot sector reads as `ContentForeign` and is named as exFAT                                      | Negative   | `TestReadingOfCapturedImages`                           |
| U-57 | A FAT boot sector is never `ContentBlank`, whatever the BPB validation concludes                                  | Regression | `TestOnlyTheBlankImageIsBlank`                          |
| U-58 | A boot sector whose BPB fails validation is not named as FAT, and is still `ContentForeign`                       | Boundary   | `TestFATTypeStringWithoutAValidBPBIsNotFAT`             |
| U-59 | A captured GPT disk is reported as GPT rather than as the protective MBR it also carries                          | Boundary   | `TestReadingOfCapturedImages`                           |
| U-60 | A GPT header on a 4Kn device is found at offset 4096, not 512                                                     | Regression | `TestReadingOfCapturedImages`                           |
| U-61 | A GPT whose primary header is absent is found by its backup header in the tail region                             | Boundary   | `TestGPTFoundByItsBackupHeaderAlone`                    |
| U-62 | Every fixture's bytes match the checksum its manifest records                                                     | Positive   | `TestFixturesMatchTheirManifests`                       |
| U-63 | ext4 images captured from two `e2fsprogs` generations both decode as ext4                                         | Regression | —                                                       |
| U-64 | An md metadata-1.1 member, which `blkid` reports nothing for and exits 2 on, is not `ContentBlank`                | Regression | `TestMdMetadata11IsNotBlankEvenThoughBlkidSaysNothing`  |

### The Blank Rule (design §4.1)

| #    | Scenario                                                                                                                                                     | Type     | Test                                    |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------|
| U-15 | All-zero head and tail regions read as `ContentBlank`                                                                                                        | Positive | `TestBlankRequiresBothRegionsZero`      |
| U-16 | One non-zero byte in the head region defeats `ContentBlank`                                                                                                  | Negative | `TestOneNonZeroByteDefeatsBlank`        |
| U-17 | One non-zero byte in the tail region defeats `ContentBlank`                                                                                                  | Negative | `TestOneNonZeroByteDefeatsBlank`        |
| U-18 | A non-zero byte at the last byte of the head region defeats it                                                                                               | Boundary | `TestOneNonZeroByteDefeatsBlank`        |
| U-19 | A non-zero byte at the first byte of the tail region defeats it                                                                                              | Boundary | `TestOneNonZeroByteDefeatsBlank`        |
| U-20 | A non-zero byte between the two regions is not read and does not defeat it, and the device is still not `ContentBlank` because the regions are what was read | Boundary | `TestAByteBetweenTheRegionsIsNotRead`   |
| U-21 | A device smaller than two regions is read whole and evaluated once                                                                                           | Boundary | `TestSmallDeviceIsReadWhole`            |
| U-22 | A device exactly two regions long reads both without overlapping                                                                                             | Boundary | `TestJustOverTwoRegionsReadsBothEnds`   |
| U-23 | A zero-length device is a read failure, not `ContentBlank`                                                                                                   | Boundary | `TestZeroSizedDeviceIsAFailureNotBlank` |

### Read Failures (design §4.3, §9)

| #    | Scenario                                                                          | Type       | Test                                         |
|------|-----------------------------------------------------------------------------------|------------|----------------------------------------------|
| U-24 | An I/O error on the head region returns an error and no reading                   | Negative   | `TestEveryReadFailureIsAnErrorAndNeverBlank` |
| U-25 | An I/O error on the tail region returns an error and no reading                   | Negative   | `TestEveryReadFailureIsAnErrorAndNeverBlank` |
| U-26 | A read that exceeds the deadline returns an error naming the region               | Negative   | `TestEveryReadFailureIsAnErrorAndNeverBlank` |
| U-27 | A short read is an error rather than a partial answer                             | Negative   | `TestEveryReadFailureIsAnErrorAndNeverBlank` |
| U-28 | A read failure is never reported as `ContentBlank`, over every failure mode above | Regression | `TestAZeroHeadWithAFailingTailIsNotBlank`    |
| U-29 | `Reading{}` has `Content` `ContentUnknown` and authorizes nothing                 | Boundary   | `TestZeroReadingAuthorizesNothing`           |
| U-30 | A `Device` reporting zero size is a read failure                                  | Negative   | `TestZeroSizedDeviceIsAFailureNotBlank`      |
| U-31 | `WithRegionSize` below the catalog's largest offset is refused                    | Boundary   | `TestTooSmallARegionIsRefused`               |
| U-32 | `Read` issues no write to the `Reader`                                            | Positive   | `TestReaderCannotWrite`                      |

---

## 2. Unit Tests — Dispatch

Assert that a reading reaches the right decision. The rows mirror design §6's two
tables, and every row that must refuse is asserted to refuse.

File: `csi-driver/pkg/spdk/nodeserver_stage_test.go`, and once the stack lands the
layer tests under `atlas-lib/volstack/layers/`.

### `filesystem` Dispatch (design §6)

| #    | Scenario                                                                                       | Type       | Test                                                  |
|------|------------------------------------------------------------------------------------------------|------------|-------------------------------------------------------|
| U-33 | `ContentBlank` permits a format                                                                | Positive   | —                                                     |
| U-34 | `ContentFilesystem` mounts the filesystem found and never formats                              | Regression | `TestStageNeverFormatsWhenPreflightFoundFilesystem`   |
| U-35 | `ContentFilesystem` disagreeing with the requested type mounts what is on disk                 | Positive   | `TestStageMountsTheFoundFilesystemNotTheRequestedOne` |
| U-36 | `ContentFilesystem` never reaches `SafeFormatAndMount`, so its internal probe cannot re-decide | Regression | `TestStageNeverFormatsWhenPreflightFoundFilesystem`   |
| U-37 | `ContentFilesystem` never reaches `fsck -a`                                                    | Regression | `TestStageMountsTheFoundFilesystemNotTheRequestedOne` |
| U-38 | `ContentStackLayer` refuses staging                                                            | Negative   | —                                                     |
| U-39 | `ContentForeign` refuses staging                                                               | Negative   | `TestProbeDiskFormat`                                 |
| U-40 | A read failure refuses staging                                                                 | Negative   | `TestProbeDiskFormat`                                 |
| U-41 | A blank device with a claim annotation mounts the recorded filesystem                          | Regression | `TestAnnotatedFilesystem`                             |
| U-42 | A blank device whose claim cannot be read refuses staging                                      | Negative   | `TestAnnotatedFilesystem_NoClaim`                     |
| U-43 | A blank device with no annotation and a readable claim formats                                 | Positive   | —                                                     |
| U-44 | The filesystem actually staged is recorded on the claim                                        | Positive   | `TestRecordOnDiskFilesystem`                          |

### `lvmPV` Dispatch (design §6)

| #    | Scenario                                                                 | Type       | Test |
|------|--------------------------------------------------------------------------|------------|------|
| U-45 | `ContentBlank` permits `pvcreate`                                        | Positive   | —    |
| U-46 | `ContentStackLayer` naming this volume's group is `StateReady`           | Positive   | —    |
| U-47 | `ContentStackLayer` naming another group is `StateForeign`               | Positive   | —    |
| U-48 | `ContentFilesystem` refuses, rather than running `pvcreate` over it      | Negative   | —    |
| U-49 | `ContentForeign` refuses                                                 | Negative   | —    |
| U-50 | A read failure refuses, and `pvs` is never consulted to decide emptiness | Regression | —    |

### Provenance (design §7.3, Phase 2)

| #    | Scenario                                                              | Type     | Test |
|------|-----------------------------------------------------------------------|----------|------|
| U-51 | A non-zero allocation refuses a format on an otherwise blank device   | Negative | —    |
| U-52 | A zero allocation permits a format                                    | Positive | —    |
| U-53 | An unreachable control plane leaves the Phase 1 reading as the answer | Boundary | —    |

---

## 3. Integration Tests

Real kernel, real `mkfs`, real nvmet-backed block devices, no simplyblock control
plane. This is the class that pins the unit fixtures to reality: a filesystem
written by a real `mkfs` has to read as design §5 says it does.

File: `test/integration/suites/formatprobe_test.go`

| #    | Scenario                                                                                            | Type       | Test                                   |
|------|-----------------------------------------------------------------------------------------------------|------------|----------------------------------------|
| I-01 | A namespace formatted by a real `mkfs.ext4` reads as `ContentFilesystem` `ext4`                     | Positive   | `TestLocalDeviceReading`               |
| I-02 | A namespace formatted by a real `mkfs.xfs` reads as `ContentFilesystem` `xfs`                       | Positive   | `TestLocalDeviceReading`               |
| I-03 | A never-written namespace reads as `ContentBlank`                                                   | Positive   | `TestLocalDeviceReading`               |
| I-04 | A namespace carrying a real `pvcreate` label reads as `ContentStackLayer`                           | Positive   | `TestLocalDeviceReading`               |
| I-05 | A namespace whose target withdrew its port reads as a failure, not as blank                         | Regression | —                                      |
| I-06 | The same namespace, before the outage, reads as its filesystem                                      | Positive   | `TestLocalDeviceReading`               |
| I-07 | The filesystem is unchanged after the outage and the reading recovers                               | Regression | —                                      |
| I-08 | A device read after its contents changed under a warm page cache reads the new contents             | Regression | —                                      |
| I-09 | `O_DIRECT` is used, and the fallback path is exercised when it is refused                           | Boundary   | —                                      |
| I-10 | A pathless device is not reported as blank, superseding the pre-design behavior this suite recorded | Regression | `TestProbe_PathlessDeviceReadsAsBlank` |
| I-11 | A 4Kn namespace resolves the LBA-counted offsets against its own logical block size                 | Regression | `TestLocalDeviceReading`               |
| I-12 | A region re-captured from a live device matches the committed fixture for the same tool version     | Positive   | —                                      |
| I-13 | A namespace carrying a real `mkfs.vfat` filesystem reads as `ContentForeign`                        | Negative   | —                                      |

I-10 is the inversion. `TestProbe_PathlessDeviceReadsAsBlank` asserts today that a
pathless device and a blank device are byte-identical readings through `blkid`,
which is the conflation design §1 describes. Under this design the same physical
state must produce a read failure, so the test is retargeted and renamed rather
than deleted: what it proves is the same fabric state, and what it asserts
inverts.

---

## 4. End-to-End Tests

Live simplyblock cluster, real fabric faults, and an assertion about the volume's
bytes rather than about a log line.

File: `csi-driver/e2e/blkid_unreadable.go`

| #    | Scenario                                                                                                | Type       | Test |
|------|---------------------------------------------------------------------------------------------------------|------------|------|
| E-01 | An ext4 volume whose paths are all down is not reformatted on restage, and its filesystem UUID survives | Regression | —    |
| E-02 | The same volume with an XFS filesystem                                                                  | Regression | —    |
| E-03 | The volume stages normally once its paths return                                                        | Positive   | —    |
| E-04 | A genuinely new volume is formatted and mounted on first stage                                          | Positive   | —    |
| E-05 | A volume whose claim carries no annotation and whose paths are down is not reformatted                  | Regression | —    |
| E-06 | The refusal is visible on the PVC as a `DeviceUnreadable` event                                         | Positive   | —    |
| E-07 | An LVM-backed volume whose paths are down is not re-`pvcreate`d                                         | Regression | —    |

E-01 is the incident. The existing `SPDKCSI-BLKID-UNREADABLE` spec already
implements this scenario against the shipped annotation guard, and it is retargeted
to assert the same outcome through the reading rather than through `blkid`'s exit
code. Its `It` is currently named for the tool, which is what changes.

---

## 5. Manual Scenarios

### M-01: A storage layer that serves zeros

**Design reference:** §7.2, §7.3.

**What to verify:** that a volume whose data path returns zeros for successful
reads is not formatted, and that Phase 1 alone does not prevent it.

**Current behavior:** Phase 1 reads such a device as `ContentBlank`, because the
reads succeed and the bytes are zero. With a claim annotation present the volume is
mounted rather than formatted, and the mount fails, which is the correct outcome.
With no annotation and no Phase 2 gate, the volume is formatted.

**Test concept:**

1. Provision a volume, write a filesystem and a known payload, and record the
   filesystem UUID.
2. Make the data path return zeros for successful reads rather than errors, which
   is what needs the manual step: no fault this repository can inject produces it,
   and reproducing it means intervening in the storage node.
3. Remove the claim's `storage.simplyblock.io/on-disk-filesystem` annotation.
4. Force a restage.
5. **Phase 1 expectation:** the volume is formatted, and this scenario documents
   the exposure rather than passing.
6. **Phase 2 expectation:** the format is refused, a `DeviceProvenanceRefused`
   event lands on the PVC, and the UUID is unchanged.

**Open question:** this scenario is the acceptance test for Q1 and Q2 of design
§14, and it cannot be written as a passing test until they are answered.

### M-02: A device with stray non-zero bytes

**Design reference:** §13.

**What to verify:** that the shadow counter finds devices the new rule refuses and
the old one accepted, before the shadow is removed.

**Test concept:**

1. Take a volume that has never been formatted by this driver and write a single
   non-zero byte into the head region, which is what a previous tenant, a
   partially completed `mkfs`, or a hand-run tool leaves behind.
2. Stage it.
3. **Expectation:** staging is refused as `ContentForeign`,
   `simplyblock_csi_node_probe_disagreements_total` increments, and the log names
   both answers.
4. Confirm the refusal is actionable: the `Detail` says where the byte was, and
   an operator can clear the device deliberately.

---

## 6. Axis Coverage

Which topologies the matrix exercises. An axis with no bearing on the reading is
argued rather than listed as a gap.

| Axis            | Values covered                                                                                                   | IDs                                   | Not covered                                                           |
|-----------------|------------------------------------------------------------------------------------------------------------------|---------------------------------------|-----------------------------------------------------------------------|
| Device content  | blank, ext2, ext3, ext4, XFS, LVM label, LUKS, GPT, MBR, FAT16, FAT32, exFAT, Btrfs, swap, md-raid, ZFS, garbage | U-01…U-15, U-54…U-61, I-01…I-04, I-13 | A second stack layer, which does not exist yet (design §14 Q4)        |
| Device state    | healthy, all paths down, warm cache, slow                                                                        | U-24…U-28, I-05…I-08                  | **Serving zeros successfully: M-01, and it is design §7.2**           |
| Device size     | smaller than two regions, exactly two regions, normal, zero-length                                               | U-21, U-22, U-23                      | Multi-terabyte, where only the tail seek's cost differs               |
| Stack layer     | raw namespace, LVM physical volume                                                                               | U-45…U-50, I-04, E-07                 | VDO logical volume and a striped logical volume, both Phase 2         |
| Block size      | 512-byte logical blocks, 4Kn                                                                                     | U-60, I-11                            | A 512e device, whose logical size is what these offsets follow anyway |
| Cache state     | cold, stale after a content change                                                                               | I-08, I-09                            | —                                                                     |
| Namespace scope | single namespace                                                                                                 | every `E-` row                        | Multi-namespace, argued below                                         |
| Cluster size    | single node                                                                                                      | every `I-` row                        | Three-node and larger, argued below                                   |
| Cluster count   | single cluster                                                                                                   | every `E-` row                        | Multi-cluster, argued below                                           |

The last three axes do not discriminate here. The reading is a node-local read of
one device's bytes, taken with no reference to a namespace, a peer node, or a
cluster identity, and there is no state shared between two readings that a second
namespace or a second cluster could perturb. They are listed so that the omission
is a decision on the page rather than an oversight, and the axis that genuinely
matters and is genuinely uncovered is device state, whose gap is M-01.

---

## 7. Coverage Summary

| Class              | Scenarios | Covered | Not covered |
|--------------------|-----------|---------|-------------|
| Unit (`U-`)        | 64        | 51      | 13          |
| Integration (`I-`) | 13        | 7       | 6           |
| End-to-end (`E-`)  | 7         | 0       | 7           |
| Manual (`M-`)      | 2         | 0       | 2           |
| **Total**          | **86**    | **58**  | **28**      |

The reading itself is built and its scenarios are covered. What remains
uncovered falls into three groups, and none of them is the reading: the
`filesystem` and `lvmPV` dispatch that consumes it (U-33 aside, the rows waiting
on the volume stack's layers), the provenance gate that is blocked on P0-1, and
the end-to-end rows that need a live cluster.

Two qualifications on what "covered" means here. The `I-` rows cite
`TestLocalDeviceReading`, which reads real devices on a real kernel and is opt-in
through `SB_BLOCKDEV_DEVICE`: it is not part of an unattended run, and it was
exercised against loop devices carrying ext4, XFS, an LVM label, FAT16, a 4Kn
GPT whose header the reading found at 4096 rather than 512, and a blank device. And I-10 exists with the opposite assertion, counted as
covered because the scenario is exercised rather than because the design's
expectation is met.

---

## 8. What Is Not Yet Covered

| #               | Gap                                                       | Reason                                                                                                          |
|-----------------|-----------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| U-33            | `ContentBlank` permits a format                           | The dispatch is not wired into a consumer yet                                                                   |
| U-38, U-43      | `filesystem` dispatch rows with no consumer wired yet     | Land with the staging call site                                                                                 |
| U-45…U-50       | `lvmPV` dispatch                                          | The layer does not exist. It is Phase 2 of the volume stack design, and the rows land with it                   |
| U-51…U-53       | Provenance                                                | Blocked on P0-1, and their expectation is blocked on design §14 Q1 and Q2                                       |
| U-63            | A second `e2fsprogs` generation                           | The capture host carries one generation. A second needs a host or container with a different `e2fsprogs`        |
| I-05, I-07…I-09 | Fabric faults, a warm cache, and the direct-read fallback | Need the nvmet harness rather than a loop device, so they land with a suite there                               |
| I-10            | Asserts the pre-design behavior                           | Inverted by this design, retargeted rather than deleted                                                         |
| I-12, I-13      | Re-capture, and a real `mkfs.vfat` namespace              | Land with the capture script and with the FAT rows it feeds                                                     |
| E-01…E-07       | The incident, end to end                                  | `SPDKCSI-BLKID-UNREADABLE` covers E-01's shape against the annotation guard and is retargeted to the reading    |
| M-01            | A storage layer serving zeros                             | No injectable fault produces it, and its Phase 2 expectation is unsettled. This is the design's stated exposure |
| M-02            | Stray non-zero bytes                                      | Needs a deliberately dirtied device, and its value is in the transition window rather than in the steady state  |

The gap that matters is M-01. Every other row is work that lands with the code,
and M-01 is a case the design says outright it does not close in Phase 1.
