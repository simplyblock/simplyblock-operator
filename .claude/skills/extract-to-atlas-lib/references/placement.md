# Where a primitive goes

What each `atlas-lib` package owns, so that *extend before adding* is a decision
with evidence behind it rather than a preference. Read the package itself before
committing to a row:

```bash
go doc github.com/simplyblock/atlas/<package>
```

## The public packages

| Package          | Owns                                                                                                                                                       | Extend it when the primitive is about                                   |
|------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| `nvme`           | Read-only NVMe lookups: subsystems, controllers, namespaces, devices, ANA state, reachability, multi-namespace                                             | reading what the kernel knows about an NVMe device                      |
| `nvmeof`         | Fabric connect and disconnect over TCP, ordered multipath connects, wait-for-device                                                                        | attaching or detaching a target, or inspecting a connection attempt     |
| `nqn`            | Building and parsing NVMe Qualified Names, subsystem and host                                                                                              | any string both components must produce identically                     |
| `lvm`            | Linux LVM commands (deciding their own device scoping), content-based PV/VG/LV identity checks, and (in `lvm/vdo`) VDO's provisioning and lifecycle        | assembling or inspecting an LVM stack on top of a simplyblock device    |
| `lvol`           | Logical-volume identity: `VolumeHandle`, `Volume`, control-plane resolution, device mapping                                                                | naming, addressing, or locating a logical volume                        |
| `kube`           | lvol-to-PV, PVC, and VolumeAttachment correlation, the driver, parameter, label, annotation, and finalizer keys, StorageClass properties, and DNS-safe ids | correlating a volume with a Kubernetes object, or a key both sides read |
| `controlplane`   | The client for the control-plane v2 API, and its request and response types                                                                                | a new endpoint, or a typed body for one                                 |
| `net`            | Outbound URL validation, the SSRF guard                                                                                                                    | validating a URL that arrives from a user or a CR                       |
| `ptr`            | Pointer and optional-field helpers for generated and Kubernetes types                                                                                      | reading or writing a set-or-omit field                                  |
| `errs`           | Sentinel errors that `errors.Is` crosses package boundaries with                                                                                           | a failure mode a caller has to distinguish                              |
| `errs/class`     | Classifying an error into a gRPC status code and whether a retry can help                                                                                  | deciding whether to requeue, retry, or fail permanently                 |
| `errs/deferrers` | `defer`-friendly `Close` and `Run` that log instead of dropping errors                                                                                     | a cleanup path whose error would otherwise vanish                       |
| `locks`          | Helpers that scope a mutex to one function call, unlocking with `defer` so an early return cannot leave it held                                            | a critical section that should read as one expression                   |
| `statemachine`   | A declared state graph over a consumer's own phase type, with per-state deadlines and `Snapshot`/`Restore`                                                 | a multi-step operation that has to survive a restart                    |

## The internal packages

`atlas-lib/internal/` is not importable by a consumer, which makes it the right
home for anything the library uses to implement a public API but that no consumer
should reach for.

| Package            | Owns                                                                                                |
|--------------------|-----------------------------------------------------------------------------------------------------|
| `internal/sysfs`   | Low-level sysfs paths and attribute reads                                                           |
| `internal/cpapi`   | The generated oapi-codegen client for the control-plane v2 API. Generated output, never hand-edited |
| `internal/version` | Build metadata, stamped through `-ldflags`                                                          |

## Adoption, as of 2026-08-25

Which packages the consumers actually import. A zero is not evidence that a
package is unwanted, since the library is deliberately ahead of its consumers.
It does mean that an extraction near that package should first check whether the
thing being extracted is already sitting there unused.

| Package          | operator | csi-driver | Note                                                                         |
|------------------|----------|------------|------------------------------------------------------------------------------|
| `ptr`            | 24       | 1          |                                                                              |
| `kube`           | 16       | 2          |                                                                              |
| `nvme`           | 4        | 4          |                                                                              |
| `nvmeof`         | 4        | 2          | two connect implementations still exist, and the CSI driver uses `nvme-cli`  |
| `lvm`            | 0        | 0          | adopted by `csi-driver/pkg/util/vdo.go` on PR #402, not yet merged to `main` |
| `errs`           | 1        | 3          |                                                                              |
| `errs/deferrers` | 0        | 3          |                                                                              |
| `net`            | 1        | 0          |                                                                              |
| `locks`          | 0        | 1          |                                                                              |
| `nqn`            | 0        | 0          | both consumers spell the NQN out as a format string instead                  |
| `lvol`           | 0        | 0          | both split the volume handle with `strings.Split(handle, ":")`               |
| `errs/class`     | 0        | 0          | the operator has its own `internal/webapi/errorclass.go`                     |
| `statemachine`   | 0        | 0          | 12 hand-rolled phase switches across 7 controller files                      |
| `controlplane`   | 0        | 0          | see below                                                                    |

## The two legacy control-plane clients

Both consumers still reach the control plane through their own client, and
`atlas-lib/README.md` records the CSI half of this in a `_Today:_` note.

| Client                        | Size        | Status                                                                                                                                |
|-------------------------------|-------------|---------------------------------------------------------------------------------------------------------------------------------------|
| `operator/internal/webapi`    | 2,065 lines | **Being retired** in favor of `atlas-lib/controlplane`. Takes no investment: do not tidy it, do not extract from it, do not add to it |
| `csi-driver/pkg/util/nvmf.go` | 341 lines   | The CSI half of the same duplication                                                                                                  |

`atlas-lib/controlplane` already covers the operator client's surface
(`CreateVolume`, `CreatePool`, `GetStorageNodes`, `GetStorageNodeNICs`,
`GetStoragePools`, and the whole migration set appear in both under
`List*`/`Get*` names), and the operator client carries a second error classifier
in `errorclass.go` beside `atlas-lib/errs/class`.

None of that makes the retirement a cleanup pass. It is a migration with its own
sequencing, and until its last call site moves it stays a transition. What this
skill takes from it is a rule: a primitive extracted anywhere near the control
plane goes to `controlplane`, and never into either legacy client.
