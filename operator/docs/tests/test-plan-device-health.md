# Test Plan: Storage Device Health

Related design: [`designs/design-device-health.md`](../designs/design-device-health.md)
Harness: [`internal/controllers/node`](../../internal/controllers/node), [`atlas-lib/prometheus`](../../../atlas-lib/prometheus), [`atlas-lib/controlplane`](../../../atlas-lib/controlplane)

Scope: the operator and atlas-lib. The control plane's storage of the assessment
and its alert rules are a dependency, faked at the boundary, and are covered by
sbcli's own `tests/unit/test_alerts_controller.py`, `tests/unit/test_device_health_info.py`,
and `tests/unit/web/api/v2/test_device_endpoints.py`.

Scenario IDs are permanent: `U-` unit, `I-` integration, `E-` end-to-end, `M-`
manual. Types are `Positive`, `Negative`, `Boundary`, `Regression`.

---

## 1. Unit Tests

### Fleet comparison (design §4.2)

File: `internal/controllers/node/storagedevice_health_assess_test.go`

| #    | Scenario                                                                                   | Type     | Test                                                  |
|------|--------------------------------------------------------------------------------------------|----------|-------------------------------------------------------|
| U-01 | Four devices at the same load and latency are all `Ok`                                     | Positive | `TestAHealthyFleetIsOk`                               |
| U-02 | One device at 375 times the fleet's latency at its share is `Slow`; siblings stay `Ok`     | Positive | `TestADeviceFarSlowerThanTheFleetAtTheSameLoadIsSlow` |
| U-03 | A device at 2.5 times its share and 10 times the latency is `Ok` (load exemption)          | Negative | `TestAHighLatencyExplainedByLoadIsNotSlow`            |
| U-04 | A device at 31 % of its share and twice the latency is `Slow` (not keeping up)             | Positive | `TestADeviceServingLessThanItsShareWhileSlowerIsSlow` |
| U-05 | A device at 10 % of its share at the fleet's latency is `Ok`                               | Negative | `TestADeviceUnderItsShareAtNormalLatencyIsOk`         |
| U-06 | A device of twice the weight carrying twice the I/O is at 100 % of its share               | Positive | `TestTheShareFollowsTheWeight`                        |
| U-07 | Five times the median but 48 µs above it stays under the 2 ms floor and is `Ok`            | Boundary | `TestARatioBelowTheAbsoluteFloorIsOk`                 |
| U-08 | A fleet of two devices is `Unknown`                                                        | Negative | `TestAFleetTooSmallToCompareIsUnknown`                |
| U-09 | A device under 5 IOPS is `Unknown` and leaves the fleet comparable                         | Negative | `TestAnIdleDeviceIsUnknown`                           |
| U-10 | Without a tick rate, a device 1000 times slower is `Slow` and claims no latency in seconds | Boundary | `TestWithoutATickRateTheRatioAloneDecides`            |

### Hysteresis (design §4.3)

File: `internal/controllers/node/storagedevice_health_assess_test.go`

| #    | Scenario                                                                                        | Type       | Test                                          |
|------|-------------------------------------------------------------------------------------------------|------------|-----------------------------------------------|
| U-11 | `Slow` after three slow assessments, `Unknown` neither helps nor resets, clears after `okAfter` | Boundary   | `TestAVerdictChangesOnlyAfterItHasHeld`       |
| U-12 | A tracker with no history starts from the published verdict                                     | Regression | `TestTheTrackerStartsFromThePublishedVerdict` |

### Health-log judgment (design §5)

File: `internal/controllers/node/storagedevice_health_smart_test.go`

| #    | Scenario                                                                      | Type     | Test                                                 |
|------|-------------------------------------------------------------------------------|----------|------------------------------------------------------|
| U-13 | A healthy NVMe log is `Ok`                                                    | Positive | `TestAHealthyNVMeLogIsOk`                            |
| U-14 | Each NVMe critical-warning bit maps to its verdict and is named in the reason | Positive | `TestTheNVMeCriticalWarningBitsDecide`               |
| U-15 | 93 % of rated life used and media errors are `Warning`                        | Positive | `TestWearAndMediaErrorsAreWarnings`                  |
| U-16 | Spare exactly at the threshold is `Failing` without the warning bit           | Boundary | `TestSpareAtTheThresholdIsFailing`                   |
| U-17 | A failed SATA self-assessment is `Failing`                                    | Positive | `TestASATADeviceThatFailsItsSelfAssessmentIsFailing` |
| U-18 | Reallocated sectors are a `Warning`                                           | Positive | `TestRemappedSectorsAreAWarning`                     |
| U-19 | A device with no health log is `Unsupported`                                  | Negative | `TestADeviceWithNoHealthLogIsUnsupported`            |
| U-20 | A supported log with no judgeable field is `Unknown`, not `Ok`                | Negative | `TestALogWithNoMetricsIsUnknown`                     |
| U-21 | The status carries the readings, and no SATA field on NVMe                    | Positive | `TestTheStatusCarriesTheReadings`                    |

### The monitor (design §3, §4.4, §5, §7)

File: `internal/controllers/node/storagedevice_health_monitor_test.go`

| #    | Scenario                                                                                                                                                             | Type       | Test                                                  |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|-------------------------------------------------------|
| U-22 | A device goes `Slow` on the third slow pass with its latencies, `DeviceSlow`, and a `slow` report; recovers after five good passes with `DevicePerformanceRecovered` | Positive   | `TestASlowDeviceIsPublishedAfterItHasHeldAndRecovers` |
| U-23 | An unreadable Prometheus leaves the published verdict                                                                                                                | Negative   | `TestAnUnreadablePrometheusLeavesTheVerdict`          |
| U-24 | The health log is read on the first pass, judged, announced, and reported, then not again until the interval                                                         | Positive   | `TestTheHealthLogIsReadOnItsScheduleAndJudged`        |
| U-25 | A failed read keeps a `Failing` verdict and emits `DeviceHealthCheckFailed`                                                                                          | Regression | `TestAFailedReadKeepsTheVerdict`                      |
| U-26 | An unchanged report is not resent within the refresh interval and is resent after it                                                                                 | Boundary   | `TestAnUnchangedReportIsResentOnlyOnRefresh`          |
| U-27 | A `Failed` device is neither assessed, read, nor reported                                                                                                            | Negative   | `TestADeviceThatIsNotServingIsNotAssessed`            |
| U-28 | A restarted monitor keeps a published `Slow` through a good pass                                                                                                     | Regression | `TestARestartedMonitorKeepsThePublishedVerdict`       |

### The mirror (design §3)

File: `internal/controllers/node/storagedevice_controller_reporting_test.go`

| #    | Scenario                                      | Type       | Test                                        |
|------|-----------------------------------------------|------------|---------------------------------------------|
| U-29 | A device event does not clear `status.health` | Regression | `TestTheMirrorDoesNotClearTheHealthVerdict` |

### CheckHealth (design §6)

File: `internal/controllers/node/storagedeviceops_checkhealth_test.go`, `storagedeviceops_test.go`

| #    | Scenario                                                                                               | Type     | Test                                            |
|------|--------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------|
| U-30 | The log is read once, the operation succeeds with the verdict, nothing is issued, the lock is released | Positive | `TestCheckHealthReadsTheLogOnceAndSucceeds`     |
| U-31 | A failed read fails the operation with the read's error and releases the lock                          | Negative | `TestCheckHealthFailsWhenTheLogCannotBeRead`    |
| U-32 | Without a monitor the action is refused                                                                | Negative | `TestCheckHealthWithNoMonitorIsRefused`         |
| U-33 | The action enum and the declared graphs agree                                                          | Positive | `TestTheActionEnumAdmitsOnlyWhatAGraphDeclares` |

### atlas-lib (design §4.2, §5, §7)

Files: `atlas-lib/prometheus/deviceio_test.go`, `atlas-lib/controlplane/devicehealth_test.go`

| #    | Scenario                                                                       | Type     | Test                                                                |
|------|--------------------------------------------------------------------------------|----------|---------------------------------------------------------------------|
| U-34 | Every counter and the tick rate fold into one reading; mean latency in seconds | Positive | `TestDeviceIOFoldsEveryKindIntoOneReading`                          |
| U-35 | Without a tick rate there is a latency in ticks and none in seconds            | Boundary | `TestDeviceIOWithoutATickRateHasNoLatencyInSeconds`                 |
| U-36 | An idle device has no mean latency                                             | Negative | `TestAnIdleDeviceHasNoMeanLatency`                                  |
| U-37 | One query rates the v2 counters over the window and pins the cluster           | Positive | `TestDeviceIOQueryRatesTheV2CountersOverTheWindowAndPinsTheCluster` |
| U-38 | A series naming no device is dropped                                           | Negative | `TestADeviceIOSeriesWithNoDeviceLabelIsDropped`                     |
| U-39 | The health log is read from `health-info` and absent fields stay nil           | Positive | `TestDeviceHealthInfoCarriesWhatTheDeviceReported`                  |
| U-40 | A response without `supported` is supported                                    | Boundary | `TestAHealthInfoWithoutSupportedIsSupported`                        |
| U-41 | A 404 wraps `ErrNotFound`                                                      | Negative | `TestDeviceHealthInfoReportsNotFound`                               |
| U-42 | The assessment is a `PUT` with validity, reporter, and both halves             | Positive | `TestReportDeviceHealthAssessmentPutsTheWholeAssessment`            |
| U-43 | A 422 is an error                                                              | Negative | `TestReportDeviceHealthAssessmentReportsARefusal`                   |

---

## 2. End-to-End Tests

| #    | Scenario                                                                                                                                                                                                                              | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | On a lblk cluster in virtual machines, throttling one virtual disk on the host (`blkiotune`) turns its device `Slow` within five minutes, raises `device_slow` on the alerts endpoint, and resolves both after the throttle is lifted | Positive | —    |
| E-02 | A rebuild after a node outage does not raise `device_slow` on the returning node's devices                                                                                                                                            | Negative | —    |
| E-03 | `CheckHealth` on an NVMe device publishes its log; on a virtio disk it publishes `Unsupported`                                                                                                                                        | Positive | —    |

---

## 3. What Is Not Yet Covered

- E-01 to E-03: no bed run yet. E-02 is the scenario the load exemption exists
  for, and the one most likely to need the thresholds tuned.
- The monitor's `Start` loop (ticker and leader election) is not tested apart
  from `Pass`.
- Reporting against a control plane without the `health-assessment` endpoint is
  exercised only as an error path in U-43, not as the logged 404 the monitor
  tolerates.
