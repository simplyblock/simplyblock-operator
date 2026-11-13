# Design Document: Storage Device Health

**Status:** Implemented  
**Author:** Michael  
**Date:** 2026-10-28  
**Test Plan:** [`tests/test-plan-device-health.md`](../tests/test-plan-device-health.md)

---

## Phase 0: External Prerequisites

| #    | Prerequisite                                                                                                                             | Kind                    | Blocks                                                 | Status                   |
|------|------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|--------------------------------------------------------|--------------------------|
| P0-1 | `PUT .../devices/{id}/health-assessment` stores the verdict, and the alerts endpoint raises `device_slow` and `device_unhealthy` from it | Control plane (`sbcli`) | Alerts on the control plane's endpoint                 | Built, branch not merged |
| P0-2 | `GET .../devices/{id}/health-info` returns a JSON object, with `supported` and the SATA fields                                           | Control plane (`sbcli`) | SMART reads (the endpoint failed on every call before) | Built, branch not merged |
| P0-3 | The storage node agent reads the health log of a kernel block device (`GET /device_smart`)                                               | Control plane (`sbcli`) | SMART on lblk (AIO) devices                            | Built, branch not merged |
| P0-4 | `simplyblock_device_tick_rate_hertz` exported per device                                                                                 | Control plane (`sbcli`) | Latency in seconds and the absolute floor (§4.2)       | Built, branch not merged |
| P0-5 | `smartctl` installed in the storage node agent image                                                                                     | Node OS                 | SMART on SATA and SAS lblk devices                     | Not shipped              |

Without P0-1 the verdicts are still published on the StorageDevice, as events,
and as gauges; only the control plane's alert feed lacks them, and each report
fails with a 404 that is logged and not retried until the next refresh. Without
P0-2 every SMART read fails and each device's SMART verdict stays `Unknown`.
Without P0-4 devices are compared in SPDK ticks, which is correct for a fleet of
one CPU type, and no latency in seconds is published. Without P0-5 a SATA or SAS
lblk device reports `Unsupported`, as a virtual disk does.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture](#3-architecture)
4. [Performance Against the Fleet](#4-performance-against-the-fleet)
5. [The Health Log](#5-the-health-log)
6. [API](#6-api)
7. [Reporting to the Control Plane](#7-reporting-to-the-control-plane)
8. [Observability](#8-observability)
9. [Configuration](#9-configuration)
10. [Testing Strategy](#10-testing-strategy)
11. [Open Questions](#11-open-questions)

---

## Overview

The operator assesses every storage device for two things: how it performs
against the other devices of its cluster, and what its own health log says. Both
verdicts are published on the StorageDevice, emitted as events and gauges, and
reported to the control plane, whose alerts endpoint raises them as
`device_slow` and `device_unhealthy`. The health log is read on a schedule, and a
StorageDeviceOps with action `CheckHealth` reads it on demand.

---

## 1. Background

A slow device is the failure the control plane notices last. It keeps answering,
so nothing marks it unavailable, yet every write that lands on it waits for it:
the cluster mirrors or erasure-codes each write across devices, and a write
completes when its slowest leg does. One device whose I/O takes seconds therefore
sets the latency of every volume striped across it.

The incident that motivated this design was a virtual disk whose host storage had
degraded. Its sibling on the same virtual machine, at the same weight and the same
load, served I/O in milliseconds, while it served the same share in one to six
seconds, with several hundred I/Os queued in front of it. Client latency followed
it to the second. Nothing in the control plane flagged the device: its status was
online throughout.

The control plane's statistics collector already exports each device's I/O
counters, and it logs a cluster event when a device's latency is twice the
cluster average. That event is a log line nobody is paged by, and its threshold
is per cluster rather than per role, so journal devices and storage devices are
compared with each other.

---

## 2. Goals and Non-Goals

### Goals

- Detect a device that performs far worse than its fleet, relative to the load
  its weight entitles it to, and tell it apart from one that is slow because it
  carries more than its share.
- Read every device's health log on a schedule and on demand, and record the
  readings and a verdict on the StorageDevice.
- Make both verdicts alerts on the control plane's alerts endpoint, with the same
  firing and resolution lifecycle every other alert there has.
- Make both verdicts available to a Kubernetes-side alerting stack as events and
  as gauges.

### Non-Goals

- **Not acting on a verdict.** A slow or failing device is reported, never
  restarted or failed. A test result is not a decision
  ([`design-storagedevice.md`](crd-redesign/design-storagedevice.md) §6 says the
  same of a self-test), and `Fail` remains a person's action.
- **Not a device self-test.** Reading the health log is an observation; running
  the device's self-test exercises it, takes it out of service, and remains the
  blocked `SelfTest` action.
- **Not tunable per cluster.** Thresholds are the operator's, set by its flags
  (§9), so the policy is stated in one place rather than also on each StorageCluster.

---

## 3. Architecture

```
 Prometheus ──DeviceIO (one query per cluster)──┐
                                                ▼
                               StorageDeviceHealthMonitor (leader only, every minute)
                                 │  fleet comparison (§4) + hysteresis
                                 │  health log read when due (§5)
 control plane ◄─health-info────┤
                                 ├─► StorageDevice status.health      (on change)
                                 ├─► events: DeviceSlow, DeviceSMARTFailing, ...
                                 ├─► gauges: simplyblock_storagedevice_*_state
 control plane ◄─health-assessment (on change, refreshed every 5 minutes)
       │
       └─► GET /clusters/{id}/alerts: device_slow, device_unhealthy

 StorageDeviceOps action=CheckHealth ──► monitor.CheckHealth(device)
```

The monitor is a Runnable rather than a reconciler, for the reason the device
collector is one: the fleet is the unit of comparison, so a pass has to see every
device of a cluster at once, and no single device changing is what should start
it. It runs on the leader only, because two replicas would disagree about when a
verdict changed.

The StorageDevice mirror rebuilds a device's status from the control-plane
stream on every device event. It carries `status.health` over unchanged, the way
it carries the operation lock, because the stream says nothing about health and
rebuilding the block would clear a verdict on every unrelated event.

---

## 4. Performance Against the Fleet

### 4.1 The fleet

A device is compared with the serving devices (phase `Online` or `Degraded`) of
the same cluster and the same role. Journal devices are their own fleet: they
carry the journal's small sequential writes rather than data stripes, so against
storage devices every one of them reads as an outlier.

### 4.2 The comparison

For each device, from one Prometheus query per cluster over a ten-minute window:

- **IOPS** is the rate of read plus write operations.
- **Mean latency** is the rate of the latency-tick counters over the rate of
  operations, divided by the device's tick rate. Ticks are per CPU, so seconds
  are what compare across nodes; without a tick rate the fleet is compared in
  ticks.
- **Weight** is the device's capacity, because capacity is what the cluster map
  places data by. A device's **share** of the fleet's IOPS is
  `weight × (Σ IOPS / Σ weight)`, and its IOPS ratio is its IOPS over its share.
- **Fleet latency** is the IOPS-weighted median of the fleet's mean latencies:
  the latency of a median I/O rather than of a median device, and a median so that
  the device being judged cannot drag the reference toward itself.

| Condition                                                                                  | Verdict                         |
|--------------------------------------------------------------------------------------------|---------------------------------|
| Fewer than 5 IOPS                                                                          | `Unknown`                       |
| Fewer than 3 devices in the fleet with at least 5 IOPS                                     | `Unknown`                       |
| IOPS ratio at least 2                                                                      | `Ok`, the latency is the load's |
| Latency at least 3 times the fleet's, and at least 2 ms above it                           | `Slow`                          |
| IOPS ratio at most 0.5, latency at least 1.5 times the fleet's, and at least 2 ms above it | `Slow`, not keeping up          |
| Otherwise                                                                                  | `Ok`                            |

The load exemption is what keeps a rebuild from paging anybody: a node that
returns after an outage receives the writes it missed, its devices queue, and the
queue is the load's rather than the hardware's. The second `Slow` row is the
saturated device: what it cannot serve waits in front of it, so it serves less
than its share while it is slower. A device that serves less than its share at a
normal latency is a placement fact, typically a device whose data has not been
rebalanced onto it yet, and is `Ok`.

The absolute floor keeps a fast fleet's small differences quiet. It is skipped
when the fleet is compared in ticks, because ticks have no duration.

### 4.3 Hysteresis

A verdict changes after three consecutive assessments disagree with it in the
`Slow` direction, and after five in the `Ok` direction. An `Unknown` assessment
says nothing about the device and moves neither the verdict nor the count. The
first reading of a device with no verdict is believed at once when it is `Ok`.

The count lives in memory. After a restart the monitor starts from the verdict
the object already carries, so a slow device does not read as fine for the first
minutes after the operator restarts.

### 4.4 Writes

The device is written when its verdict changes, or when its published readings
are fifteen minutes old. A write per assessment would wake every watcher of the
kind every minute for a number nothing reconciles toward. The gauges in §8 carry
the readings at full resolution.

---

## 5. The Health Log

The health log is read through the control plane's `health-info` endpoint, which
reads an SPDK-attached NVMe controller through SPDK and asks the storage node
agent to read a kernel block device: `nvme smart-log` for an NVMe namespace,
`smartctl` otherwise. A device with no health log, such as a virtual disk, is
`Unsupported`.

Each device's log is read every six hours, oldest first and at most ten per pass,
so a fleet whose schedule comes due at once is read over minutes rather than in
one burst. A failed read keeps the verdict the device already has, records the
failure in the reason, emits `DeviceHealthCheckFailed`, and is retried after
fifteen minutes: a control plane that cannot reach a node says nothing about the
drive, and clearing a `Failing` verdict for that reason would resolve its alert.

| Health log                                                                                                                            | Verdict   |
|---------------------------------------------------------------------------------------------------------------------------------------|-----------|
| NVMe critical warning: spare below threshold, reliability degraded, read-only, volatile backup failed, or persistent memory read-only | `Failing` |
| Available spare at or below the device's threshold                                                                                    | `Failing` |
| SATA or SAS self-assessment failed                                                                                                    | `Failing` |
| NVMe critical warning: temperature                                                                                                    | `Warning` |
| At least 90 % of rated life used, media errors, reallocated or pending sectors                                                        | `Warning` |
| Nothing above, and at least one of these fields reported                                                                              | `Ok`      |
| No field that can be judged                                                                                                           | `Unknown` |

`Failing` raises a critical alert that reads as an instruction to replace the drive, so only what
the device itself declares about its reliability reaches it. Wear, temperature,
and error counts that a drive keeps serving through are warnings.

---

## 6. API

`StorageDeviceStatus` gains `health`, written by the monitor alone:

```go
type DeviceHealth struct {
	Performance *DevicePerformance `json:"performance,omitempty"`
	SMART       *DeviceSMART       `json:"smart,omitempty"`
}
```

`DevicePerformance` carries `verdict` (`Ok`, `Slow`, `Unknown`), `reason`,
`since`, `assessedAt`, `latencyMicroseconds`, `fleetLatencyMicroseconds`,
`latencyRatioPercent`, and `iopsRatioPercent`. `DeviceSMART` carries `verdict`
(`Ok`, `Warning`, `Failing`, `Unsupported`, `Unknown`), `reason`, `checkedAt`,
`source`, and the readings: `criticalWarning`, `temperatureCelsius`,
`availableSparePercent`, `availableSpareThresholdPercent`, `percentageUsed`,
`mediaErrors`, `powerOnHours`, `unsafeShutdowns`, `smartPassed`,
`reallocatedSectors`, and `pendingSectors`. Two print columns, `Performance` and
`SMART`, show the verdicts in `kubectl get sd`.

`StorageDeviceOps` gains the action `CheckHealth`, whose graph is the single step
`Requesting`. It reads the health log through the monitor, so a read on demand is
judged, published, and reported exactly as a scheduled one is, and its message
carries the verdict. It changes nothing on the device and is abortable for its
whole life. When the read fails, the operation fails with the read's error.

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageDeviceOps
metadata:
  name: check-worker-3-nvme0
spec:
  deviceRef: production-7f3a9c-5e0000
  action: CheckHealth
```

The design of the device kind listed SMART as a non-goal, as the host's business
and a node exporter's. That held while no part of the system acted on a health
log. The alert feed is the place an operator is paged from, and a failing drive
that pages nobody until it is gone is the failure the feed exists for.

---

## 7. Reporting to the Control Plane

Each serving device's health block is sent with
`PUT /api/v2/clusters/{c}/storage-nodes/{n}/devices/{d}/health-assessment`, in
the control plane's lowercase verdict spelling, with `reported_at` and a validity
of fifteen minutes. A report is sent when its verdicts or reasons change, and
again every five minutes while they do not.

The control plane stores the report on the device and raises:

| Kind               | Severity                                      | Raised while                              |
|--------------------|-----------------------------------------------|-------------------------------------------|
| `device_slow`      | warning                                       | `performance.verdict` is `slow`           |
| `device_unhealthy` | critical for `failing`, warning for `warning` | `smart.verdict` is `failing` or `warning` |

Both stay quiet on a node that is not online, for a device that is not serving
(it is already `device_unavailable`), for a device the operator removed, and once
the report is older than its validity. The last is what keeps an operator that
stopped reporting from leaving an alert firing forever. Raising and resolving go
through the endpoint's existing lifecycle, so each transition is one
`ALERT_RAISED` or `ALERT_RESOLVED` cluster event.

---

## 8. Observability

| Event                        | Type      | On              |
|------------------------------|-----------|-----------------|
| `DeviceSlow`                 | `Warning` | `StorageDevice` |
| `DevicePerformanceRecovered` | `Normal`  | `StorageDevice` |
| `DeviceSMARTFailing`         | `Warning` | `StorageDevice` |
| `DeviceSMARTWarning`         | `Warning` | `StorageDevice` |
| `DeviceSMARTRecovered`       | `Normal`  | `StorageDevice` |
| `DeviceHealthCheckFailed`    | `Warning` | `StorageDevice` |

| Gauge                                                 | Labels                                 |
|-------------------------------------------------------|----------------------------------------|
| `simplyblock_storagedevice_performance_state`         | `cluster`, `node`, `device`, `verdict` |
| `simplyblock_storagedevice_smart_state`               | `cluster`, `node`, `device`, `verdict` |
| `simplyblock_storagedevice_relative_latency_count`    | `cluster`, `node`, `device`            |
| `simplyblock_storagedevice_relative_iops_share_count` | `cluster`, `node`, `device`            |

The two relative gauges are dimensionless ratios: 1 is the fleet median latency,
and 1 is the device's full weighted IOPS share. They end in `count` because
§7.12 of design-crd-model.md allows no `ratio` suffix, and `count` is its suffix
for a gauge.

An Alertmanager rule on `simplyblock_storagedevice_performance_state{verdict="Slow"} == 1`
or `simplyblock_storagedevice_smart_state{verdict="Failing"} == 1` is how a
Kubernetes-side stack receives the same alerts.

---

## 9. Configuration

| Flag                       | Default | Meaning                                  |
|----------------------------|---------|------------------------------------------|
| `--enable-device-health`   | `true`  | Run the monitor and accept `CheckHealth` |
| `--device-health-interval` | `1m`    | How often the fleet is assessed          |
| `--device-health-window`   | `10m`   | The window each assessment averages over |
| `--device-smart-interval`  | `6h`    | How often each health log is read        |

The performance assessment reads the Prometheus at `--prometheus-url`. Without
one, only the health log is assessed.

---

## 10. Testing Strategy

The scenarios are in
[`tests/test-plan-device-health.md`](../tests/test-plan-device-health.md). The
fleet comparison and the health-log judgment are pure functions and are covered
exhaustively at that level; the monitor is driven pass by pass against a fake
cluster, a fake Prometheus, and a fake control plane.

---

## 11. Open Questions

- The thresholds of §4.2 are set from one incident and from what reads as slow.
  They want a review against a quiet production fleet's distribution of latency
  ratios before the alert is relied on.
- Per-operation latency is compared, not latency per byte. A fleet striped by the
  cluster sees one I/O-size mix on every device of a role, so the two agree; a
  device serving a different mix would not, and the control plane's own outlier
  log normalizes by bytes for that reason.
- The control plane's `device_latency_outlier` event now overlaps `device_slow`.
  Whether it stays as a log line for clusters without the operator is a
  control-plane decision.
