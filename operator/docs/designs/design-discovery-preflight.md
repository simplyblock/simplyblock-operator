# Design Document: Discovery Preflight

**Status:** Draft  
**Author:** Manohar Reddy  
**Date:** 2026-11-12

---

## 1. Background

A failed deployment is diagnosed today from probe ConfigMaps, Warning events on the
OperatorOps run, operator logs, SPDK logs, sbcli, and Graylog. Events expire after
an hour, and `discovery.Finding` reaches the reviewer only as an event. Deployments
at two customers needed the CLI to find causes the operator had already seen.

## 2. Goals and Non-Goals

Goals: every node problem that can be predicted is checked before anything is
deployed, all failures are reported at once, and the result lives in the
OperatorOps status, readable with `kubectl` alone.

Non-goals: diagnosing failures after deployment starts (a later section), and
replacing the support bundle (`scripts/collect-discovery-logs.sh`).

## 3. Checks

| Check                                   | Source                         | Severity |
|-----------------------------------------|--------------------------------|----------|
| Free huge pages against the requirement | `Report.HugePages` (exists)    | Fail     |
| Allocatable CPUs against `vcpuCount`    | `Report.CPU` (exists)          | Fail     |
| `isolcpus` overlapping the SPDK mask    | probe, new                     | Fail     |
| `nvme_tcp` and `nvme_fabrics` loadable  | probe, new                     | Fail     |
| NVMe host auth and TLS when DHCHAP is on| probe, new                     | Fail     |
| Native NVMe multipath                   | `noteMultipath` (exists)       | Warn     |
| Time synchronization active             | probe, new                     | Warn     |
| Equal MTU on the storage network        | `Report.Interfaces` (exists)   | Fail     |
| Data ports reachable between workers    | probe, second pass, new        | Fail     |
| Probe readings unreadable               | `Report.Unreadable` (exists)   | Warn     |
| Filesystem, RAID, or LVM signature, mounted or swap disk | `AvailableRule` (exists) | Fail |
| Old simplyblock metadata on a disk      | probe, new                     | Fail     |
| Leftover SPDK or vfio binding           | probe, new                     | Warn     |
| Disk throughput far below its peers     | fio baseline Job               | Warn     |
| IOMMU off or vfio-pci not loadable      | probe, new                     | Fail     |
| Memory beyond huge pages, swap enabled  | `Report.Memory` (exists)       | Warn     |
| CPU manager or reserved CPUs against the SPDK mask | kubelet config, new | Fail     |
| Huge pages and NICs on different NUMA nodes | `Report` (exists)          | Warn     |
| Control plane API reachable from each node | probe, new                  | Fail     |
| Required ports free on each node        | probe, new                     | Fail     |
| NVMe/TCP ports open between nodes       | probe, second pass, new        | Fail     |
| Bandwidth and NIC link speed on the storage network | probe, second pass, new | Warn |
| DNS resolution and clock skew between nodes | probe, second pass, new    | Warn     |
| API server reaches webhook pod IPs      | operator, new                  | Fail     |
| Every required image pullable, mirrors included | operator, new          | Fail     |
| cert-manager and the issuer chain present | operator, new                | Fail     |
| Another CSI driver or default StorageClass present | operator, new       | Warn     |
| Pod Security admission allows privileged pods | operator, new            | Fail     |
| SELinux and stale MachineConfig pools (OKD) | operator, new              | Warn     |
| Kubelet configuration writable (k3s, Talos) | operator, new              | Warn     |
| Kernel config suitable (Talos)          | probe, new                     | Fail     |
| Taints or cordons keeping storage pods off a node | operator, new        | Fail     |
| Supported Kubernetes and kernel versions | operator, probe, new          | Fail     |
Cross-node checks run in a second probe pass, after every worker has reported its
addresses, so each worker can test the others.

## 4. Status

`discovery.Finding` gains a severity and a node, and the run copies every finding
into `status.findings[]` as `{node, severity, reason, message, remedy}`, ordered
Fail before Warn. A `Preflight` condition summarizes them, and a printcolumn
shows the Fail count. Events remain, but the status is the record.

## 5. Gating

A ClusterDeploymentConfig written by a run with a Fail finding stays in `Draft`,
and approval is refused with the findings listed. An administrator who disagrees
sets `spec.discover.ignoreFindings` to the reasons to waive. A waived finding
stays in the status, marked as waived.

## 6. Open Questions

**Q1:** Should each remedy be a fixed string per reason or generated from the reading?

**Q2:** Further improvements under discussion are collected here before they get sections.
