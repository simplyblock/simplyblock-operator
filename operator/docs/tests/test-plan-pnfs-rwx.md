# Test Plan: pNFS (RWX) Support

Related designs: [`designs/design-pnfs-rwx.md`](../designs/design-pnfs-rwx.md) and
[`designs/design-pnfs-mds-vm.md`](../designs/design-pnfs-mds-vm.md) (the metadata
server's QEMU guest).
Striped volumes and group snapshots: [`test-plan-pnfs-striped.md`](test-plan-pnfs-striped.md).

Harness: `csi-driver` (`make -C csi-driver unit-test`, `e2e-test`), `operator`
(`make -C operator test`), `atlas-lib` (`make -C atlas-lib test`), and the sbtest
framework (`make -C test/framework gate` for its own tests, and
`make -C test/framework run SUITE=pnfs-fio` against a live cluster).

Scope: the CSI driver, the operator, the metadata server's runner and guest agent,
and the Kubernetes surface of this repository. The control plane (`sbcli`) and
SPDK are dependencies, faked at the boundary. The guest kernel's nfsd patches are
built in the `pnfs-os` repository, and what this plan asserts of them is the
behavior a client sees across a metadata server restart (§6).

This plan covers **single-volume** pNFS exports. Striping, consistency-group
snapshots, and the user-facing `VolumeGroupSnapshot` are in
[`test-plan-pnfs-striped.md`](test-plan-pnfs-striped.md).

Scenario IDs are permanent. A scenario the design no longer describes keeps its
row, struck through, with what supersedes it. New scenarios continue each block's
numbering.

| Prefix | Class                 | Harness                                                                                                     |
|--------|-----------------------|-------------------------------------------------------------------------------------------------------------|
| `U-`   | Unit                  | no cluster: pure functions, mock control plane, stubbed host commands, sbtest detectors on literal evidence |
| `O-`   | Operator unit         | fake client, mock assembler                                                                                 |
| `SAN-` | CSI sanity            | `csi-sanity` against the driver                                                                             |
| `I-`   | Integration           | two components over a real gRPC connection or a real child process, nothing else live                       |
| `E-`   | End-to-end            | live cluster: the CSI e2e suite (`csi-driver/e2e/pnfs.go`) or the `pnfs-fio` sbtest suite                   |
| `F-`   | Failure injection     | live cluster plus a fault: the metadata server pod deleted, a node rebooted, a network cut                  |
| `SEC-` | Security              | live cluster, allow-listing and fencing                                                                     |
| `L-`   | Load, scale, and soak | live cluster, multi-day for `L-03`                                                                          |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, e2e spec, or sbtest suite and detector, or `—`
when the scenario is not covered yet. A scenario proven by a recorded live run
rather than by an automated test names the run, and the gap table says so.

---

## Table of Contents

1. [Unit Tests](#1-unit-tests)
2. [Operator Unit Tests](#2-operator-unit-tests)
3. [Sanity Tests](#3-sanity-tests)
4. [Integration Tests](#4-integration-tests)
5. [End-to-End Tests](#5-end-to-end-tests)
6. [Failure-Injection and Resilience Tests](#6-failure-injection-and-resilience-tests)
7. [Security Tests](#7-security-tests)
8. [Long-Term, Load, and Soak Tests](#8-long-term-load-and-soak-tests)
9. [Test Environment Requirements](#9-test-environment-requirements)
10. [Axis Coverage](#10-axis-coverage)
11. [Coverage Summary](#11-coverage-summary)
12. [What Is Not Yet Covered](#12-what-is-not-yet-covered)

---

## 1. Unit Tests

#### Superseded scenarios

The first design planned stripes, a four-part `nfs:` volume handle, and an
export registry with its own migration phases. A pNFS volume keeps its backing
lvol's handle (design §11), striping is the striped plan's, and an export is
restarted rather than migrated (design §13).

| #        | Scenario                                                                                                 |
|----------|----------------------------------------------------------------------------------------------------------|
| ~~U-01~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-02~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-03~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-04~~ | Superseded: U-29, the `pnfs` fstype selects the export path                                              |
| ~~U-05~~ | Superseded: U-35, the export record is named after the backing volume                                    |
| ~~U-06~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-07~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-08~~ | Superseded: U-30 and U-32                                                                                |
| ~~U-09~~ | Superseded: U-29, the filesystem is the `pnfs` fstype's, always XFS (U-63)                               |
| ~~U-10~~ | Superseded: U-32                                                                                         |
| ~~U-11~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-12~~ | Superseded: a pNFS volume keeps the backing lvol's three-part handle (U-45)                              |
| ~~U-13~~ | Superseded: a pNFS volume keeps the backing lvol's three-part handle (U-45)                              |
| ~~U-14~~ | Superseded: a pNFS volume keeps the backing lvol's three-part handle (U-45)                              |
| ~~U-15~~ | Superseded: a pNFS volume keeps the backing lvol's three-part handle (U-45)                              |
| ~~U-16~~ | Superseded: persistent reservations are a Phase 0 dependency of the storage plane                        |
| ~~U-17~~ | Superseded: group snapshots, `test-plan-pnfs-striped.md`                                                 |
| ~~U-18~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                        |
| ~~U-19~~ | Superseded: group snapshots, `test-plan-pnfs-striped.md`                                                 |
| ~~U-20~~ | Superseded: U-37, the volume context carries the Service address and export path                         |
| ~~U-21~~ | Superseded: U-43                                                                                         |
| ~~U-22~~ | Superseded: U-40                                                                                         |
| ~~U-23~~ | Superseded: U-44                                                                                         |
| ~~U-24~~ | Superseded: U-37 and U-44                                                                                |
| ~~U-25~~ | Superseded: U-42                                                                                         |
| ~~U-26~~ | Superseded: an export is restarted, not migrated (O-24, F-11)                                            |
| ~~U-27~~ | Superseded: O-15 and O-21, one metadata server per storage cluster and a binding that survives a restart |
| ~~U-28~~ | Superseded: an export is restarted, not migrated                                                         |

#### CSI controller: CreateVolume, DeleteVolume, and expansion (design §9), in `csi-driver/internal/csi/controller/pnfs_test.go`

| #    | Scenario                                                                                                | Type                | Test                                                                                                  |
|------|---------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------------------------------------------------|
| U-29 | The `pnfs` fstype takes the export path, and every other fstype the block path                          | Positive            | `TestPNFSIsTakenOnlyForThePNFSFSType`                                                                 |
| U-30 | ReadWriteMany without the `pnfs` fstype → `InvalidArgument`                                             | Negative            | `TestReadWriteManyWithoutPNFSIsRefused`                                                               |
| U-31 | ReadWriteOnce over a `pnfs` class is accepted                                                           | Boundary            | `TestReadWriteOnceOverPNFSIsAccepted`                                                                 |
| U-32 | A raw block claim on a `pnfs` class → refused                                                           | Negative            | `TestPNFSRefusesARawBlockClaim`                                                                       |
| U-33 | A clone or a restore into a pNFS volume → refused before any lvol is made                               | Negative            | `TestCloningIntoAPNFSVolumeIsRefused`                                                                 |
| U-34 | Claims of the same name in two namespaces get different export paths                                    | Boundary            | `TestExportPathSeparatesNamespaces`                                                                   |
| U-35 | The export record is named after the backing volume, DNS-safe and stable across retries                 | Positive            | `TestTheRecordIsNamedAfterTheBackingVolume`, `TestRecordNameIsDNSSafeAndStable`                       |
| U-36 | An export not yet Ready → `Unavailable`, which is retried, and a Degraded one → not retried             | Positive + Negative | `TestNotReadyIsUnavailableAndDegradedIsNot`                                                           |
| U-37 | The volume context carries the Service address, the export path, and the access protocol                | Positive            | `TestVolumeContextCarriesWhatTheNodeNeeds`                                                            |
| U-38 | Delete takes the export down before the lvol, and a volume with no export deletes cleanly               | Positive + Negative | `TestDeleteTakesTheExportDownFirst`, `TestDeletingAVolumeWithNoExportIsNotAnError`                    |
| U-39 | Expanding records the new size on the export, and without an export the node grows it as a block volume | Positive + Negative | `TestExpandingAnExportedVolumeRecordsTheNewSize`, `TestExpandingAVolumeWithNoExportLeavesTheNodeToIt` |

#### CSI node: staging a pNFS client (design §10.1–§10.3), in `csi-driver/internal/csi/node/pnfs_test.go`

| #    | Scenario                                                                                     | Type     | Test                                                                                |
|------|----------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------------------------|
| U-40 | The alias is `nvme-eui.<nguid>`, one spelling whatever form the NGUID is read in             | Positive | `TestNormalizeNGUIDAgreesAcrossSpellings`, `TestAliasUsesThePrefixTheKernelTries`   |
| U-41 | An alias pointing at an old device node is replaced                                          | Positive | `TestEnsureAliasReplacesAStaleLink`                                                 |
| U-42 | A namespace with no usable NGUID refuses the stage instead of mounting without a direct path | Negative | `TestStageRefusesAnUnusableNGUID`                                                   |
| U-43 | Mount options always carry `vers=4.1`, `soft`, and bounded retries                           | Positive | `TestMountOptionsAlwaysCarryV41`, `TestMountOptionsAreSoftWithBoundedRetries`       |
| U-44 | A mount failure is reported, and unstaging an absent mount succeeds                          | Negative | `TestStageReportsAMountFailure`, `TestUnstageToleratesAnAbsentMount`                |
| U-45 | A pNFS volume is recognized from its stashed context, and its backing volume from its handle | Positive | `TestPNFSIsRecognizedFromTheStashedContext`, `TestBackingVolumeIsReadFromTheHandle` |

#### CSI node: priming the layout (design §10.1, §10.4), in `csi-driver/internal/csi/node/pnfs_prime_test.go`

| #    | Scenario                                                                                                  | Type                | Test                                                                               |
|------|-----------------------------------------------------------------------------------------------------------|---------------------|------------------------------------------------------------------------------------|
| U-46 | The probe writes one synced block and leaves nothing behind on the export                                 | Positive            | `TestPrimeLayoutTouchesTheMountAndLeavesNothingBehind`                             |
| U-47 | An unwritable mount is reported, and a canceled stage starts no I/O                                       | Negative            | `TestPrimeLayoutReportsAnUnwritableMount`, `TestPrimeLayoutRespectsACanceledStage` |
| U-48 | A staging mount is probed when first seen                                                                 | Positive            | `TestAStagingMountIsPrimedWhenFirstSeen`                                           |
| U-49 | A staging mount is probed again after its transport reconnects, and not without a reconnect               | Positive + Negative | `TestAStagingMountIsPrimedAgainAfterAReconnect`                                    |
| U-50 | A probe still running is not started a second time                                                        | Boundary            | `TestAProbeStillRunningIsNotStartedTwice`                                          |
| U-51 | A failed probe is retried on the next pass                                                                | Negative            | `TestAFailedProbeIsRetried`                                                        |
| U-52 | A pod's bind of a staging mount, another driver's mount, and a mount without a SCSI layout are not probed | Negative            | `TestOnlyThisDriversPNFSStagingMountsArePrimed`                                    |

#### NFS client mount statistics, in `atlas-lib/nfsclient/mountstats_test.go`, `csi-driver/e2e/mountstats_test.go`

| #    | Scenario                                                                            | Type     | Test                                                                                                   |
|------|-------------------------------------------------------------------------------------|----------|--------------------------------------------------------------------------------------------------------|
| U-53 | Every NFS mount is read, and no other filesystem                                    | Positive | `TestParseMountstatsReadsEveryNFSMountAndOnlyThose`                                                    |
| U-54 | A staging mount yields its device, layout type, connect count, and operation counts | Positive | `TestParseMountstatsReadsAStagingMount`                                                                |
| U-55 | A mount without pNFS reports no layout type and no connects                         | Negative | `TestParseMountstatsReadsNoLayoutTypeFromAPlainMount`, `TestMountUsesLayout`                           |
| U-56 | The e2e reader refuses to guess between two NFS mounts, and fails a file with none  | Negative | `TestParseNFSOpCountsRefusesToGuessBetweenTwoMounts`, `TestParseNFSOpCountsReportsAFileWithNoNFSMount` |

#### Export assembly in the guest (design §8, MDS design §6.3), in `atlas-lib/nfsexport/export_test.go`, `csi-driver/internal/nfsexport/*_test.go`, `atlas-lib/nvmeof/detach_test.go`

| #    | Scenario                                                                                                                                                                                                           | Type                | Test                                                                                                                                                                                                                                               |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-57 | Create brings the volume stack up before publishing, and is idempotent                                                                                                                                             | Positive            | `TestCreateBringsTheStackUpBeforePublishing`, `TestCreateIsIdempotent`                                                                                                                                                                             |
| U-58 | A stack that does not come up, or a grow that fails, is not published                                                                                                                                              | Negative            | `TestCreateDoesNotPublishAStackThatWouldNotComeUp`, `TestCreateDoesNotPublishWhatItCouldNotGrow`                                                                                                                                                   |
| U-59 | An empty client set is refused rather than widened                                                                                                                                                                 | Negative            | `TestCreateRefusesAnEmptyClientSet`                                                                                                                                                                                                                |
| U-60 | Delete unpublishes before taking the stack down, and tolerates an export already gone                                                                                                                              | Positive + Negative | `TestDeleteUnpublishesBeforeTakingTheStackDown`, `TestDeleteToleratesAnAlreadyGoneExport`                                                                                                                                                          |
| U-61 | Check passes only when the filesystem is mounted and nfsd's table carries the export                                                                                                                               | Positive + Negative | `TestCheckPassesWhenMountedAndPublished`, `TestCheckFailsWhenTheFilesystemIsNotMounted`, `TestCheckFailsWhenNFSDsTableDoesNotCarryTheExport`                                                                                                       |
| U-62 | The exports entry carries `pnfs`                                                                                                                                                                                   | Positive            | `TestExportLineCarriesPNFS`                                                                                                                                                                                                                        |
| U-63 | The plan is fabric then XFS, formatted with the block path's pinned options                                                                                                                                        | Positive            | `TestPlanIsFabricThenFilesystem`, `TestPlanFormatsXFSWithThePinnedOptions`, `TestExportFormatOptionsArePinnedTheSameWayTheBlockPathPinsThem`                                                                                                       |
| U-64 | A volume nothing identifies, or a spec with no cluster, is refused                                                                                                                                                 | Negative            | `TestPlanRefusesAVolumeNothingIdentifies`, `TestPlanRefusesASpecWithNoCluster`                                                                                                                                                                     |
| U-65 | An export's stack handle cannot collide with a staged block volume's                                                                                                                                               | Boundary            | `TestStackHandleCannotCollideWithAStagedVolume`                                                                                                                                                                                                    |
| U-66 | The nfsd self-check fails when nfsd never answers, and cleans up when it does                                                                                                                                      | Positive + Negative | `TestSelfCheckNFSDFailsWhenNFSDNeverAnswers`, `TestSelfCheckNFSDPassesAndCleansUpWhenNFSDAnswers`                                                                                                                                                  |
| U-80 | Regression (2026-10-09-pnfs-mds-restart-eacces): the root export survives a guest restart because its update is written through the state-disk link and keeps it. A root file that cannot be inspected is an error | Regression          | `TestCreateWritesTheRootThroughALinkAndKeepsTheLink`, `TestRootFileReportsARootItCannotInspect`                                                                                                                                                    |
| U-81 | Regression (2026-10-09-pnfs-unstage-deleted-volume): unstaging a volume the control plane already deleted releases its subsystem locally, keeps one another volume uses, and refuses when sharing cannot be asked  | Regression          | `TestDetachReleasesAVolumeTheControlPlaneNoLongerKnows`, `TestReleaseDeletedVolume_ReapsItsOwnSubsystemWithNoLivePath`, `TestReleaseDeletedVolume_KeepsASubsystemServingAnotherVolume`, `TestReleaseDeletedVolume_RefusesWhenSharingCannotBeAsked` |
| U-82 | Regression (2026-10-09-pnfs-empty-root-file-panic): the first export written into the empty root file the guest image creates gets the root line instead of panicking                                              | Regression          | `TestCreateWritesTheRootIntoAnEmptyRootFile`                                                                                                                                                                                                       |

#### Metadata server runner and guest (MDS design §5, §6, §8.2), in `csi-driver/internal/mds/*`

| #    | Scenario                                                                                                                                                                             | Type                           | Test                                                                                                                                                                       |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-67 | QEMU always runs under KVM, and an architecture and firmware mismatch is refused                                                                                                     | Positive + Negative            | `TestGuestAlwaysRunsUnderKVM`, `TestARM64WithoutFirmwareIsRefused`, `TestAMD64WithFirmwareIsRefused`, `TestUnknownArchitectureIsRefused`                                   |
| U-68 | The kernel command line carries the static address and the nfsd layout hold parameters                                                                                               | Positive                       | `TestKernelCmdlineCarriesTheStaticAddress`                                                                                                                                 |
| U-69 | The root disk is first and read-only, and the state disk carries its serial                                                                                                          | Positive                       | `TestRootDiskIsFirstAndReadOnlyStateDiskCarriesItsSerial`                                                                                                                  |
| U-70 | A guest panic, a missed boot deadline, and an ignored power button all end QEMU                                                                                                      | Negative                       | `TestGuestPanicEndsQEMU`, `TestMissedBootDeadlineKillsQEMU`, `TestGuestIgnoringThePowerButtonIsKilledAfterTheGrace`                                                        |
| U-71 | Guest memory leaves QEMU its allowance, and limits too small or unset are refused                                                                                                    | Boundary                       | `TestGuestResourcesLeaveTheAllowanceToQEMU`, `TestGuestResourcesRefuseLimitsTooSmallOrUnset`                                                                               |
| U-72 | A `/dev/kvm` that cannot be opened read-write is refused                                                                                                                             | Negative                       | `TestCheckKVMNeedsAReadWriteDevice`                                                                                                                                        |
| U-73 | Readiness follows the guest agent's probe after boot                                                                                                                                 | Positive                       | `TestReadinessFollowsTheProbeAfterBoot`, `TestGRPCHealthProbeFollowsTheAgent`                                                                                              |
| U-74 | The agent is healthy only with its state disk mounted and nfsd running                                                                                                               | Positive + Negative            | `TestAGuestWithItsStateDiskAndNFSDIsHealthy`, `TestAGuestWithoutItsStateDiskIsUnhealthy`, `TestAGuestWithoutNFSDIsUnhealthy`                                               |
| U-75 | Only the NFS port is forwarded to the guest, client source addresses are kept, and the rules survive a restart                                                                       | Positive + Negative            | `TestNFSIsTheOnlyPortForwardedToTheGuest`, `TestInboundClientAddressesAreNotRewritten`, `TestApplyIsIdempotentAcrossRestarts`                                              |
| U-76 | The relay resolves connections as the operator's identity, and delete proceeds without the control plane                                                                             | Positive                       | `TestCreateHandsTheGuestAConnectionResolvedForTheOperatorsIdentity`, `TestDeleteGoesAheadWithoutTheControlPlane`                                                           |
| U-77 | A create without a resolvable connection reaches no guest, and a connection arriving with the call is dropped                                                                        | Negative                       | `TestCreateWithoutAConnectionReachesNoGuest`, `TestAConnectionArrivingWithTheCallIsNotPassedOn`                                                                            |
| U-83 | The nfsd watchdog reads pool statistics by column name, a missing file as nfsd not running, and only nfsd threads with their state and wait                                          | Positive + Negative            | `TestPoolStatsAreSummedByColumnName`, `TestMissingPoolStatsIsNFSDNotRunning`, `TestThreadStateIsReadAfterTheLastParenthesis`, `TestOnlyNFSDThreadsAreRead`                 |
| U-84 | A thread on one non-idle wait for three ticks is stuck. Idle waits and a changing wait are not                                                                                       | Positive + Negative            | `TestAThreadWaitingOnTheSameThingForThreeTicksIsStuck`, `TestIdleWaitsAreNeverStuck`, `TestAThreadWhoseWaitChangesIsNotStuck`                                              |
| U-85 | Work arriving while no request is processed is a stall. Requests completing without threads woken are progress, and without RPC statistics nothing is judged                         | Positive + Negative + Boundary | `TestWorkArrivingWithNoRequestProcessedIsNotDraining`, `TestRequestsCompletingWithNoThreadWokenIsDraining`, `TestWithoutRPCStatisticsTheBacklogIsNotJudged`                |
| U-86 | A stall dumps every nfsd thread's stack once, again only after five minutes, and logs its end. A summary is logged every fourth tick, and a guest without nfsd logs nothing alarming | Positive + Negative            | `TestStacksAreDumpedAtMostEveryFiveMinutesInAnEpisode`, `TestTheEndOfAnEpisodeIsLogged`, `TestASummaryIsLoggedEveryFourthTick`, `TestAGuestWithoutNFSDLogsNothingAlarming` |

#### Forgetting a replaced pod's flows (MDS design §8.1), in `atlas-lib/conntrack/*_test.go`, `atlas-lib/conntrack/conntrackrpc/conntrackrpc_test.go`

| #    | Scenario                                                                                                                                                                                                       | Type                | Test                                                                                                                                                                                            |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-87 | A selector matches the flows of its protocol, original destination, and destination port translated to its address, and nothing else, also when another Service's flows reach a pod that inherited the address | Positive + Negative | `TestAFlowTranslatedToTheOldAddressMatches`, `TestAnythingElseDoesNotMatch`                                                                                                                     |
| U-88 | A selector missing its protocol, original destination, port, or address is refused, by Forget as well                                                                                                          | Negative            | `TestASelectorMissingAPartIsRefused`, `TestForgetRefusesAnInvalidSelector`                                                                                                                      |
| U-89 | The netlink adapter takes the reply direction's source as the translated backend (Linux)                                                                                                                       | Positive + Negative | `TestTheAdapterReadsTheReplyDirectionsSource`                                                                                                                                                   |
| U-90 | The flow service carries a selector intact and returns the count. An invalid selector, one without an original destination among them, is refused on both ends, and a node failure comes back as an error      | Positive + Negative | `TestASelectorReachesTheNodeIntactAndTheCountComesBack`, `TestAnInvalidSelectorIsRefusedOnBothEnds`, `TestANodeFailureComesBackAsAnError`, `TestARequestWithoutTheOriginalDestinationIsRefused` |

#### Run judgment: the pNFS detectors (sbtest), in `test/framework/tests/test_detectors.py`

| #    | Scenario                                                                                                                                                                                      | Type                           | Test           |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------|----------------|
| U-78 | A mount that fetched no layout fails the run, data through the server beside layouts warns, and a run without NFS mounts is skipped                                                           | Positive + Negative            | `PnfsLayout`   |
| U-79 | A client whose namespace stayed flat, was never attached, or stopped taking writes for good fails the run. A stall that ends warns, and a device quiet after its fio instances ended is clean | Positive + Negative + Boundary | `PnfsDeviceIO` |

---

## 2. Operator Unit Tests

#### Superseded scenarios

No node serves an export, so the node-hosted reconcilers' rows have nothing to
test: exports are assembled in the metadata server's guest over csi-link, not
through the control plane's API.

| #        | Scenario                                                                                      |
|----------|-----------------------------------------------------------------------------------------------|
| ~~O-01~~ | Superseded: no node runs an NFS server. The guest's health is U-74 and O-22                   |
| ~~O-02~~ | Superseded: client kernel requirements are design §5.3, and the guest's kernel is the image's |
| ~~O-03~~ | Superseded: no node holds an export to quiesce before a drain                                 |
| ~~O-04~~ | Superseded: an export is restarted, not migrated (O-24)                                       |
| ~~O-05~~ | Superseded: an export is restarted, not migrated                                              |
| ~~O-06~~ | Superseded: export calls go to the guest over csi-link (I-09), not to the control plane       |
| ~~O-07~~ | Superseded: export calls go to the guest over csi-link (I-09)                                 |
| ~~O-08~~ | Superseded: no node is selected as a metadata server                                          |
| ~~O-09~~ | Superseded: export calls go to the guest over csi-link (I-09)                                 |

#### NFSExport binding (design §7.2, MDS design §7.3), in `operator/internal/controller/nfsexport_mds_unit_test.go`, `nfsexport_clients_test.go`, `nfsexport_service_test.go`

| #    | Scenario                                                                                                                                                                                                                                                           | Type                | Test                                                                                                                                                                           |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| O-10 | No driver sets `spec.pnfs.mds` → the export waits in Pending with a message naming it, and no StatefulSet is made                                                                                                                                                  | Negative            | `TestPendingWithoutTheMDSSpecWaitsForIt`                                                                                                                                       |
| O-11 | A storage cluster's first export creates the metadata server's ServiceAccount and StatefulSet                                                                                                                                                                      | Positive            | `TestPendingPodHostedCreatesTheMDSStatefulSet`                                                                                                                                 |
| O-12 | An unschedulable pod is named: `NoKVMCapableNode` or `MDSStateUnavailable`                                                                                                                                                                                         | Negative            | `TestPendingPodHostedNamesWhyThePodCannotSchedule`                                                                                                                             |
| O-13 | A pod whose guest is not Ready yet is waited for                                                                                                                                                                                                                   | Negative            | `TestPendingPodHostedWaitsForTheGuest`                                                                                                                                         |
| O-41 | Regression (2026-10-09-mds-statefulset-never-updated): an existing MDS StatefulSet built by another release gets the current pod template and keeps its claim templates, an up-to-date one is not written, and a new one records its template hash                 | Regression          | `TestPendingPodHostedUpdatesAnOutdatedMDSStatefulSet`, `TestPendingPodHostedLeavesAnUpToDateMDSStatefulSet`, `TestPendingPodHostedCreatesTheMDSStatefulSetWithItsTemplateHash` |
| O-42 | Regression (2026-10-09-mds-statefulset-never-updated): a Ready export updates an outdated MDS StatefulSet and stays Ready, writes nothing to a current one, never creates an absent one, and an update that lost a conflict to the current template counts as done | Regression          | `TestReadyExportUpdatesAnOutdatedMDSStatefulSet`, `TestReadyExportLeavesTheMDSStatefulSetAsItIs`, `TestAnMDSUpdateThatLostTheRaceToTheCurrentTemplateSucceeds`                 |
| O-14 | A Ready pod is bound by name and addressed by its IP                                                                                                                                                                                                               | Positive            | `TestPendingPodHostedBindsTheReadyPod`                                                                                                                                         |
| O-15 | A StatefulSet of the same name belonging to another storage cluster is not used                                                                                                                                                                                    | Negative            | `TestPendingPodHostedRefusesAnotherClustersStatefulSet`                                                                                                                        |
| O-16 | A metadata server memory limit under 512Mi is refused with `MDSResourcesInvalid`                                                                                                                                                                                   | Boundary            | `TestPendingPodHostedRefusesAnMDSMemoryLimitUnder512Mi`                                                                                                                        |
| O-17 | The client set is every node's InternalIP and pod CIDR, never a wildcard                                                                                                                                                                                           | Positive + Negative | `TestClientSetIsClusterNodesAndNothingElse`, `TestPendingPodHostedAllowsTheNodesPodCIDRs`, `TestBindingRecordsTheResolvedClientSet`                                            |
| O-18 | Binding creates the Service and EndpointSlice, and keeps a ClusterIP already allocated                                                                                                                                                                             | Positive            | `TestBindingCreatesAStableAddress`, `TestBindingKeepsAnAlreadyAllocatedClusterIP`                                                                                              |

#### NFSExport phases (design §7.1, MDS design §7.4–§7.7), in `operator/internal/controller/nfsexport_controller_unit_test.go`, `nfsexport_assembler_test.go`

| #    | Scenario                                                                                                                                                                          | Type                | Test                                                                                                                                                                                                                                                                   |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| O-19 | Assembling reaches Ready through the metadata server's peer, with the StatefulSet's host NQN, and records the assembling instance                                                 | Positive            | `TestAssemblingReachesReady`, `TestPodHostedCallsCarryTheStatefulSetsHostNQN`, `TestAssemblyRecordsTheAssemblingPod`                                                                                                                                                   |
| O-20 | No session is a wait, an assembly error is retried, and the deadline ends in Degraded                                                                                             | Negative            | `TestAssemblingWaitsForAnUnreachableMDS`, `TestAssemblyErrorIsRetried`, `TestAssemblingGivesUpAtTheDeadline`                                                                                                                                                           |
| O-21 | The phase survives an operator restart, an undeclared jump is refused, and Degraded is terminal                                                                                   | Positive + Negative | `TestPhaseSurvivesARestart`, `TestGraphRefusesAnUndeclaredJump`, `TestTheKindDeclaresOnlyThePhasesTheControllerDrives`, `TestDegradedIsTerminal`                                                                                                                       |
| O-22 | A Ready export's health is polled, an unhealthy one is surfaced without degrading it, and an unreachable one is not checked                                                       | Positive + Negative | `TestReadyExportPollsAHealthyHostAtTheSteadyStateInterval`, `TestReadyExportSurfacesAnUnhealthyHostWithoutDegradingIt`, `TestReadyExportDoesNotCheckAnUnreachableHost`                                                                                                 |
| O-23 | Regression (2026-09-23-pnfs-grow-disconnected-mds): a grow is not marked applied while the metadata server is disconnected                                                        | Regression          | `TestReadyExportDoesNotObserveAGrowWhileTheMDSIsDisconnected`                                                                                                                                                                                                          |
| O-24 | After a restart, in a new pod or in place, the export is reassembled and its EndpointSlice repointed. An unchanged instance is only checked                                       | Positive + Negative | `TestAReadyExportIsReassembledAfterItsPodRestarted`, `TestAReadyExportIsReassembledAfterItsGuestRestartedInPlace`, `TestAResyncRepointsTheEndpointSliceAtTheNewPodIP`, `TestAResyncWaitsForTheRestartedPodsSession`, `TestAReadyExportOnItsAssemblingPodIsOnlyChecked` |
| O-25 | A metadata server pod event wakes only the exports bound to it                                                                                                                    | Positive + Negative | `TestAnMDSPodEventEnqueuesTheExportsBoundToIt`                                                                                                                                                                                                                         |
| O-26 | Delete tears down and releases. With the pod gone it releases at once, with the pod present and no session it waits, a never-assembled export converges, and a refusal is retried | Positive + Negative | `TestDeleteTearsDownThenReleases`, `TestPodHostedDeleteWithThePodGoneReleasesTheFinalizer`, `TestPodHostedDeleteWaitsForAPodThatStillExists`, `TestDeletingAnExportThatNeverAssembledConverges`, `TestDeletingRetriesWhenTheHostRefuses`                               |
| O-27 | The export spec is derived from the volume handle, and an unreadable `volumeRef` is refused                                                                                       | Positive + Negative | `TestSpecForDerivesIdentityFromTheHandle`, `TestSpecCarriesTheVolumesClusterAndPool`, `TestSpecRefusesAVolumeRefItCannotRead`                                                                                                                                          |

#### Metadata server state disk (MDS design §5.3), in `operator/internal/controller/nfsexport_mds_unit_test.go`

| #    | Scenario                                                                                           | Type                | Test                                                                                                           |
|------|----------------------------------------------------------------------------------------------------|---------------------|----------------------------------------------------------------------------------------------------------------|
| O-28 | The state disk gets a class of its own, derived from the cluster's block class, and reuses it      | Positive            | `TestPodHostedStateDiskGetsAClassOfItsOwn`, `TestPodHostedStateDiskReusesItsClass`                             |
| O-29 | The state class is reserved by a ValidatingAdmissionPolicy before it exists                        | Positive            | `TestPodHostedStateClassIsReservedByAnAdmissionPolicy`                                                         |
| O-30 | A named class is used when it is a simplyblock block class, and refused otherwise                  | Positive + Negative | `TestPodHostedStateDiskTakesANamedSimplyblockClass`, `TestPodHostedStateDiskRefusesAClassThatIsNotSimplyblock` |
| O-31 | A claim outside the operator's namespace naming a reserved state class is denied by the API server | Negative            | —                                                                                                              |

#### Metadata server workload (MDS design §5), in `operator/internal/controllers/driver/mds_test.go`, `operator/internal/webhook/simplyblockdriver_validator_test.go`

| #    | Scenario                                                                                                                    | Type                | Test                                                                                                                                             |
|------|-----------------------------------------------------------------------------------------------------------------------------|---------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|
| O-32 | The StatefulSet name fits and identifies the cluster, and keeps clusters sharing a prefix apart                             | Boundary            | `TestMDSStatefulSetNameFitsAndIdentifiesTheCluster`, `TestMDSNamesKeepClustersSharingAPrefixApart`                                               |
| O-33 | The pod runs its privileged runner on a KVM node, under its own ServiceAccount, mounting only `/dev/kvm` and `/dev/net/tun` | Positive + Negative | `TestMDSPodRunsTheRunnerPrivilegedOnAKVMNode`, `TestMDSPodRunsUnderItsOwnServiceAccount`, `TestMDSPodMountsOnlyTheTwoHostDevices`                |
| O-34 | The state disk is a block claim at the runner's path, with a default size                                                   | Positive            | `TestMDSStateDiskIsABlockClaimAtTheRunnersPath`, `TestMDSStateDiskDefaultsItsSize`                                                               |
| O-35 | Limits are defaulted or kept, and admission refuses a memory limit under 512Mi                                              | Boundary            | `TestMDSPodWithoutLimitsGetsDefaultOnes`, `TestMDSPodKeepsTheLimitsItIsGiven`, `TestSimplyblockDriverValidatorRefusesAnMDSMemoryLimitUnder512Mi` |
| O-36 | The image defaults to the operator's build, and an unresolvable one is an error                                             | Positive + Negative | `TestMDSImageDefaultsToTheOperatorsBuildInTheDriversRepository`, `TestMDSImageWithoutAnAnswerIsAnError`                                          |
| O-37 | No workload is rendered without `spec.pnfs.mds`                                                                             | Negative            | `TestMDSObjectsNeedTheMDSSpec`                                                                                                                   |

#### Volume operations on a pNFS volume (design §12), in `operator/internal/webhook/persistentvolumeops_validator_test.go`, `operator/internal/controllers/volume/pnfs_test.go`

| #    | Scenario                                                                                                                    | Type                | Test                                                                                      |
|------|-----------------------------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------------------------------------|
| O-38 | A PersistentVolumeOps migration of a pNFS volume is refused at admission                                                    | Negative            | `TestPersistentVolumeOpsRefusesWhatNoReconcileCouldFix` (`a pNFS volume`)                 |
| O-39 | One admitted without the webhook fails without creating a migration, and a filesystem volume still migrates                 | Positive + Negative | `TestAPNFSVolumeFailsTheOperationWithoutAMigration`, `TestAFilesystemVolumeStillMigrates` |
| O-40 | The rebalancer, pinning, latency, and node-removal movers skip pNFS volumes instead of raising operations admission refuses | Negative            | —                                                                                         |

#### Address withdrawal and flow flush (MDS design §8.1), in `operator/internal/controller/nfsexport_withdraw_unit_test.go`, `operator/internal/csilink/flows_test.go`

| #    | Scenario                                                                                                                                                                                                                                                                                                                                   | Type                           | Test                                                                                                                                                                                                                                                          |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| O-43 | Regression (2026-10-09-pnfs-mds-conntrack-pinning): a terminating or gone metadata server pod's address leaves every bound export's EndpointSlice at once, the export stays Ready, and the nodes are asked to forget the address                                                                                                           | Regression                     | `TestATerminatingMDSPodsAddressIsWithdrawn`, `TestAGoneMDSPodsAddressIsWithdrawn`                                                                                                                                                                             |
| O-44 | An address is withdrawn once. The replacement's address follows, and the nodes forget the old address again at the move, also when no withdrawal was seen. A replacement that receives the old address forgets nothing more and cancels the waiting request, and the Service's ClusterIP is read from the Service when the status lacks it | Positive + Boundary            | `TestAWithdrawnAddressIsWithdrawnOnce`, `TestTheReplacementsAddressFollowsAWithdrawal`, `TestAMoveWithoutAWithdrawalForgetsTheOldAddress`, `TestAReplacementWithTheSameAddressForgetsNothingMore`, `TestTheServiceIPIsReadFromTheServiceWhenTheStatusLacksIt` |
| O-45 | Every node advertising the flow service is asked, for TCP port 2049 to the export's ClusterIP and the old address only. One request per Service and address, duplicates collapse, a canceled request is never sent, one node failing does not stop the others, and an invalid address asks nobody                                          | Positive + Negative + Boundary | `TestEveryCapableNodeForgetsTheOldAddress`, `TestRequestsForOneAddressCollapse`, `TestANodeFailingDoesNotStopTheOthers`, `TestAnInvalidAddressAsksNobody`, `TestOneRequestPerServiceAndAddress`, `TestACanceledRequestIsNotSent`                              |

---

## 3. Sanity Tests

`internal/csi/sanity_test.go`.

| #      | Scenario                                                                                                         | Type     | Test |
|--------|------------------------------------------------------------------------------------------------------------------|----------|------|
| SAN-01 | With `MULTI_NODE_MULTI_WRITER` added, csi-sanity confirms identity/controller capability reporting is consistent | Positive | —    |
| SAN-02 | Existing RWO sanity suite still passes (no regression)                                                           | Positive | —    |

---

## 4. Integration Tests

#### Superseded scenarios

Export assembly runs in the guest agent, whose steps are unit-tested in U-57 to
U-66. These rows described it on a node, with stripes.

| #        | Scenario                                                                 |
|----------|--------------------------------------------------------------------------|
| ~~I-01~~ | Superseded: U-57 and U-63                                                |
| ~~I-02~~ | Superseded: U-60, with one lvol per volume                               |
| ~~I-03~~ | Superseded: U-57                                                         |
| ~~I-04~~ | Superseded: U-39 and E-08. The grow runs in the guest through reassembly |
| ~~I-05~~ | Superseded: O-20 and O-21                                                |
| ~~I-06~~ | Superseded: U-58                                                         |
| ~~I-07~~ | Superseded: striping, `test-plan-pnfs-striped.md`                        |
| ~~I-08~~ | Superseded: group snapshots, `test-plan-pnfs-striped.md`                 |

#### Components over a real connection

| #    | Scenario                                                                                                                                 | Type                | Test                                                                                                                                                                                                                               |
|------|------------------------------------------------------------------------------------------------------------------------------------------|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| I-09 | The export service round-trips create, delete, and check over gRPC. An invalid spec arrives as invalid and a missing device as not found | Positive + Negative | `TestCreateRoundTripsTheWholeSpec`, `TestDeleteRoundTripsTheWholeSpec`, `TestCheckRoundTripsAHealthyExport`, `TestCheckReportsAnUnhealthyExportAsAnError`, `TestInvalidSpecArrivesAsInvalid`, `TestMissingDeviceArrivesAsNotFound` |
| I-10 | The runner starts a child process, turns Ready, and shuts down cleanly on cancel, and a guest exiting on its own is an error             | Positive + Negative | `TestGuestTurnsReadyAndShutsDownCleanlyOnCancel`, `TestCancelDuringBootShutsDown`, `TestGuestExitingOnItsOwnIsAnError`                                                                                                             |
| I-11 | The csi-link hub names a metadata server by its pod and admits the kind only for its ServiceAccount                                      | Positive + Negative | `TestKubeAuthenticatorNamesAnMDSByItsPod`, `TestKubeAuthenticatorKeepsTheMDSKindToItsServiceAccount`                                                                                                                               |

---

## 5. End-to-End Tests

The CSI e2e suite (`csi-driver/e2e/pnfs.go`) proves the data path one claim at a
time. The `pnfs-fio` sbtest suite (`test/framework/sbtest/suites/pnfs-fio.yaml`)
runs shared and private volumes under verified concurrent fio and judges the run
from both ends of the data path: the NFS client's own counters (`pnfs.layout`)
and each client node's NVMe namespace counters (`pnfs.device-io`).

| #        | Scenario                                                                                                                                                           | Type     | Test                                                                         |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------------------------------------|
| E-01     | Provision RWX PVC; 3 pods across 3 nodes mount and read/write a shared file; data visible across pods                                                              | Positive | `two pods on different nodes write to one volume` (two nodes); E-15 (three)  |
| E-02     | Direct block path used: `n` namespaces attached on client; layout stats show block (not MDS) I/O for large sequential writes                                       | Positive | `writes reach the storage nodes without passing through the metadata server` |
| E-03     | Concurrent writers with byte-range locks: two pods append to the same file, checksum verified                                                                      | Positive | —                                                                            |
| ~~E-04~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                                                                                  |          |                                                                              |
| ~~E-05~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                                                                                                  |          |                                                                              |
| E-06     | Snapshot RWX (consistency group) → restore into new RWX PVC → data matches; new PVC has its own MDS binding                                                        | Positive | — (E-21 covers the snapshot; restore into pNFS is refused, U-33)             |
| ~~E-07~~ | Superseded: a clone into a pNFS volume is refused (U-33, E-20)                                                                                                     |          |                                                                              |
| E-08     | Online resize under active I/O: `xfs_growfs` grows the mount; pods see new capacity; no I/O interruption                                                           | Positive | `an expanded volume keeps writing straight to the storage nodes`             |
| E-09     | Delete RWX PVC → unexported, LV/VG removed, namespaces disconnected (MDS + clients), lvols deleted                                                                 | Positive | —                                                                            |
| E-10     | Distro matrix: core provision/mount/rw on RHEL/Rocky/Alma **and** Ubuntu; `eui64`+direct path work or graceful fallback (FM-10)                                    | Positive | —                                                                            |
| E-11     | RWX pod on kernel < 6.11 node → scheduling avoided / stage fails with clear event; no partial mount                                                                | Negative | —                                                                            |
| ~~E-12~~ | Superseded: the `pnfs` fstype decides the path (U-29, U-30)                                                                                                        |          |                                                                              |
| E-13     | Two RWX PVCs never share an `fsid` on the same MDS host (provision many, assert uniqueness)                                                                        | Negative | —                                                                            |
| ~~E-14~~ | Superseded: SCSI layout devices are resolved in the kernel, without `blkmapd` (design §10.1)                                                                       |          |                                                                              |
| E-15     | Two shared volumes with three pods each on at least two nodes, and two private volumes, two fio containers per pod on files of their own: no checksum or job error | Positive | `pnfs-fio` (`fio.checksum`, `fio.job-error`)                                 |
| E-16     | Every client mount took layouts and moved no data through the metadata server                                                                                      | Positive | `pnfs-fio` (`pnfs.layout`)                                                   |
| E-17     | Every client node's namespace kept taking reads and writes for the length of its fio instances                                                                     | Positive | `pnfs-fio` (`pnfs.device-io`)                                                |
| E-18     | A volume whose pods all landed on one node is reported as exercising one client, not several                                                                       | Boundary | `pnfs-fio` (spread warning)                                                  |
| E-19     | The metadata server pod on a node that is also a client, and on one that is not                                                                                    | Positive | `pnfs-fio`, placement not pinned                                             |
| E-20     | A pNFS claim naming a data source is refused rather than half-made                                                                                                 | Negative | `a pNFS claim naming a data source is refused rather than half-made`         |
| E-21     | A snapshot of a pNFS volume becomes ready                                                                                                                          | Positive | `a snapshot of a pNFS volume becomes ready`                                  |
| E-22     | A pNFS volume mounts ReadWriteMany over NFSv4.1                                                                                                                    | Positive | `a pNFS volume mounts ReadWriteMany over NFSv4.1`                            |
| E-23     | With no `spec.pnfs.mds`, a pNFS claim stays Pending and its export carries `NoMetadataServer`                                                                      | Negative | —                                                                            |

---

## 6. Failure-Injection and Resilience Tests

A metadata server restart is the failure this design recovers from in place
(design §13.4). Its recovery depends on the guest kernel's nfsd patches (layout
hold, fencing reservation keys a previous boot left, and a retry instead of
`STALE` for an export not back yet), and the proof of those is a client staying
on the direct path across a restart, not a unit test.

#### Superseded scenarios

| #        | Scenario                                                                                 |
|----------|------------------------------------------------------------------------------------------|
| ~~F-01~~ | Superseded: no export migrates, and a restart is F-11                                    |
| ~~F-02~~ | Superseded: F-11 and F-13, fencing a previous boot's keys                                |
| ~~F-03~~ | Superseded: striping, `test-plan-pnfs-striped.md`, and one namespace's path loss is F-18 |
| ~~F-06~~ | Superseded: clients mount a Service address that does not move (O-18, O-24)              |
| ~~F-07~~ | Superseded: clients mount a Service address that does not move (O-18, O-24)              |
| ~~F-09~~ | Superseded: striping, `test-plan-pnfs-striped.md`                                        |

| #    | Scenario                                                                                                                                                                                                              | Type       | Test                                                       |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------------------------------------------------------------|
| F-04 | Client node reboot with active RWX mount → restage reconnects namespaces + remounts NFS; pods recover                                                                                                                 | Positive   | —                                                          |
| F-05 | CSI node/controller pod killed mid-operation → at-most-one export/lvol set; recovery on restart                                                                                                                       | Positive   | —                                                          |
| F-08 | Rolling storage-node restart under active RWX I/O → sustained I/O (per-node migration)                                                                                                                                | Positive   | —                                                          |
| F-10 | Network partition: clients↔MDS cut but namespaces reachable → metadata blocks, direct I/O verified; heal reconnects                                                                                                   | Negative   | —                                                          |
| F-11 | The metadata server pod is deleted during `pnfs-fio`'s timed run: every client is back on its namespace after grace, no fio error, no critical finding                                                                | Positive   | `pnfs-fio` with a manual pod delete, run `pnfs-1791484322` |
| F-12 | Regression (run `pnfs-1791461034`): after a restart a pod's device lookup failed in its own mount namespace and its I/O stayed on the metadata server for the rest of the run                                         | Regression | F-11's run, with the shape judged by U-79                  |
| F-13 | Regression (run `pnfs-1791468760`): a client could not register its new reservation key over its previous boot's, and SPDK refused Preempt and Abort. After a restart every registrant carries the current boot's key | Regression | F-11's run, checked with `nvme resv-report`                |
| F-14 | Regression (run `pnfs-1791468760`): a reclaim before its export was exported again lost the open, and fio failed with `EBADF`                                                                                         | Regression | F-11's run (`fio.job-error` clean)                         |
| F-15 | The guest crashes and its runner container restarts in the same pod: exports reassembled, clients recover as in F-11 (O-24 covers the operator side)                                                                  | Positive   | —                                                          |
| F-16 | A restart while one client node is down: grace runs its full length, and the other clients still recover                                                                                                              | Boundary   | —                                                          |
| F-17 | Two restarts in quick succession, the second before grace ends                                                                                                                                                        | Boundary   | —                                                          |
| F-18 | One client loses its paths to its namespace and heals: its I/O resumes direct, without a restage                                                                                                                      | Positive   | —                                                          |
| F-19 | Regression (run `pnfs-1791575321`): once the replacement metadata server pod is Ready, no client's NFS flow is still translated to the deleted pod's address                                                          | Regression | —                                                          |

---

## 7. Security Tests

| #      | Scenario                                                                                                                         | Type     | Test |
|--------|----------------------------------------------------------------------------------------------------------------------------------|----------|------|
| SEC-01 | With host allow-listing + PR fencing, a fenced client can no longer write to the shared namespaces                               | Positive | —    |
| SEC-02 | (If in scope) `sec=krb5` mount succeeds with valid credentials                                                                   | Positive | —    |
| SEC-03 | `root_squash`/tenancy: in-pod root cannot write as root when squashed                                                            | Positive | —    |
| SEC-04 | Unauthorized node (NQN not allow-listed) attempts `nvme connect` a member namespace → refused                                    | Negative | —    |
| SEC-05 | Host outside the export client set attempts `mount` → denied (once `*` replaced by allow-list, design §15)                       | Negative | —    |
| SEC-06 | (If in scope) `sec=krb5` mount without valid credentials → denied                                                                | Negative | —    |
| SEC-07 | A pod on a client node mounts an export directly over NFS: the pod CIDR admits it, and its data goes through the metadata server | Negative | —    |
| SEC-08 | The guest holds no Kubernetes token and no control-plane secret (U-76 and U-77 cover the relay side)                             | Positive | —    |

---

## 8. Long-Term, Load, and Soak Tests

| #        | Scenario                                                                                                                                                                                    | Type     | Test |
|----------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| L-01     | Load: 20–50 clients driving `fio` (mixed random/sequential, varying block sizes) on one RWX volume; measure aggregate throughput, per-client latency, direct-vs-MDS I/O ratio, MDS CPU      | Positive | —    |
| ~~L-02~~ | Superseded: L-07, exports per metadata server pod                                                                                                                                           |          |      |
| L-03     | Soak (multi-day): sustained mixed I/O + periodic snapshot/clone/resize; assert no leaks (lvols/exports/mounts/symlinks), no `fsid` exhaustion, stable CSI+SNodeAPI memory, XFS `fsck` clean | Positive | —    |
| ~~L-04~~ | Superseded: L-08, repeated metadata server restarts                                                                                                                                         |          |      |
| L-05     | Provisioning churn: rapid create/delete of RWX PVCs → detect record/registry leaks and reconciler correctness under load                                                                    | Negative | —    |
| L-06     | Regression guard: existing RWO load/reconnect suites pass alongside (no regression from shared initiator/monitor code, NFR-4)                                                               | Positive | —    |
| L-07     | Exports per metadata server pod up to nfsd's and the guest memory's limits, then one more                                                                                                   | Boundary | —    |
| L-08     | Repeated metadata server restarts and client node reboots under active writers, ending with clean checksums and a clean `fsck`                                                              | Positive | —    |

---

## 9. Test Environment Requirements

- At least one node exposing `/dev/kvm` for the metadata server pod, and a block StorageClass on the storage cluster for its state disk.
- Client kernels with `CONFIG_PNFS_BLOCK` and NVMe multipath.
- The guest image built from `pnfs-os` (`pnfs-minimal`) with its nfsd patches, and the MDS image built from it.
- A `SimplyblockDriver` with `spec.pnfs.mds` set, and csi-link enabled.
- For `pnfs-fio`: the suite's `source_storageclass` set to the cluster's block class, `nvme.iostat` and `nvme.snapshot` enabled, and `host.dmesg` pointed at the CSI node pods.
- Distro matrix: at least one RHEL-family and one Debian-family node pool (E-10).
- Fault injection as in the existing reconnect e2e suites (network partition, process kill, node reboot), plus deleting the metadata server pod (F-11).

---

## 10. Axis Coverage

Which topologies the matrix exercises. An axis value with no IDs is a gap, see §12.

| Axis                      | Values covered                                                            | IDs                                      | Not covered                                                                         |
|---------------------------|---------------------------------------------------------------------------|------------------------------------------|-------------------------------------------------------------------------------------|
| Cluster topology          | five nodes, one of them hosting the metadata server pod and also a client | E-15 … E-19, F-11                        | single-node cluster, and a cluster without a KVM node (only O-12's event)           |
| Metadata server placement | on a client node, and elsewhere                                           | E-19                                     | pinned placement in either position                                                 |
| Volume sharing            | shared by three pods on two or more nodes, and private to one pod         | E-15, E-18                               | one volume shared by every node                                                     |
| Namespace scope           | export paths separate same-named claims in two namespaces                 | U-34                                     | the same in a live run                                                              |
| Cluster count             | one storage cluster, with names and StatefulSets kept per cluster         | O-15, O-32                               | two storage clusters with exports, each with its metadata server, in one run        |
| Kernel and distro         | Talos clients on 6.18, and the guest on 6.18                              | E-15, F-11                               | RHEL family, Debian family (E-10), and kernels below 6.11 (E-11)                    |
| Lifecycle                 | create, resize, snapshot, clone refused, delete, metadata server restart  | U-38, U-39, E-08, E-20, E-21, O-26, F-11 | restore (E-06), delete in a live run (E-09), and a guest crash in place live (F-15) |
| Scale                     | eight pods and four volumes on one metadata server                        | E-15                                     | L-01, L-07                                                                          |

---

## 11. Coverage Summary

Struck rows are not counted.

| Class                    | Scenarios | Covered | Not covered |
|--------------------------|-----------|---------|-------------|
| Unit (`U-`)              | 62        | 62      | 0           |
| Operator (`O-`)          | 36        | 34      | 2           |
| Sanity (`SAN-`)          | 2         | 0       | 2           |
| Integration (`I-`)       | 3         | 3       | 0           |
| End-to-end (`E-`)        | 18        | 12      | 6           |
| Failure injection (`F-`) | 13        | 4       | 9           |
| Security (`SEC-`)        | 8         | 0       | 8           |
| Load and soak (`L-`)     | 6         | 0       | 6           |
| **Total**                | **148**   | **115** | **33**      |

Unit and operator coverage follows the code. The live classes are the gaps: four
`F-` rows rest on a recorded run with a manual pod delete rather than on a suite
that deletes the pod itself.

---

## 12. What Is Not Yet Covered

| #               | Gap                                                                                          | Reason                                                                                                                                                                                            |
|-----------------|----------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| F-11 … F-14     | The metadata server restart is proven by run `pnfs-1791484322`, with the pod deleted by hand | sbtest has no component that deletes the metadata server pod at a point in the timed run. Until it does, a regression in the nfsd patches or the re-probe shows only when someone repeats the run |
| O-31            | The state class reservation enforced by the API server                                       | The unit test asserts the policy and binding are created, and enforcement needs a real API server (envtest with the admission plugin, or a live cluster)                                          |
| O-40            | Movers raising operations for pNFS volumes                                                   | The rebalancer, pinning, latency, and node-removal movers choose PVs themselves, and with O-38 they are refused per cycle instead of skipping the volume                                          |
| E-10, E-11      | Distros and kernels beyond Talos 6.18                                                        | The lab cluster is Talos, and RHEL- and Debian-family node pools are not part of any run                                                                                                          |
| E-23            | `NoMetadataServer` in a live cluster                                                         | O-10 covers the decision, and no e2e spec provisions without `spec.pnfs.mds`                                                                                                                      |
| F-15 … F-18     | Guest crash in place, restart with a client down, back-to-back restarts, client path loss    | Not run. F-16 and F-17 are the boundaries of the grace period the nfsd patches rely on                                                                                                            |
| F-19            | Clients reaching the deleted pod's address after its replacement is Ready                    | The fix is unit-tested (O-43 … O-45). A `pnfs-fio` run with an MDS restart, judged clean by `pnfs.conntrack-pinned`, has not run against it yet                                                   |
| SEC-01 … SEC-08 | Every security scenario                                                                      | No security suite exists for pNFS. SEC-07 is the known exposure of admitting pod CIDRs (MDS design §8.3)                                                                                          |
| L-01 … L-08     | Load, scale, and soak                                                                        | No long-running pNFS suite exists                                                                                                                                                                 |
| —               | Two storage clusters with exports in one run                                                 | Per-cluster names are unit-tested (O-15, O-32), but no live run has had two metadata servers                                                                                                      |
| —               | Backend behavior (persistent reservations, `ptpl_file`, SPDK's reservation actions)          | Out of scope for this repository. F-13 is what a client sees of SPDK refusing Preempt and Abort                                                                                                   |

