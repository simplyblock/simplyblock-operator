# Atlas

Shared Go library for the simplyblock **Kubernetes operator** and **CSI
driver**. It holds the node-level storage primitives both consumers need —
NVMe discovery, NVMe-oF fabric management, NQN handling, and the
logical-volume ↔ NVMe-device mapping — so neither re-implements them.

![](../assets/simplyblock-logo.svg)

> Part of the [simplyblock-operator](../README.md) monorepo. For the repository overview, license,
> and contribution guidelines, see the [root README](../README.md).

The library lives in this monorepo and is consumed by the operator and CSI driver via a Go
`replace` directive (module path `github.com/simplyblock/atlas` → `../atlas-lib`); it is not
published or installed independently.

## Layout

```
atlas/
├── doc.go                  Library overview + package index
├── go.mod
│
├── nvme/                   Read-only NVMe subsystem/controller/namespace lookups
│   ├── device.go           Subsystem, Controller, Address, Namespace, Device
│   ├── resolver.go         SubsystemResolver (List/ByNQN) + DeviceResolver (List/ByUUID/ByDevicePath/ByNamespace)
│   ├── sysfs_resolver.go   local impl: NewSysfsSubsystemResolver / NewSysfsDeviceResolver
│   └── sysfs_scan.go       sysfs tree walk + attribute parsing
├── nvmeof/                 NVMe-oF fabric connect/disconnect (TCP)
│   └── connector.go        Connector: Connect / Disconnect / IsConnected
├── nqn/                    Build & parse simplyblock lvol NQNs
├── lvol/                   Logical-volume identity, control-plane + device resolution
│   ├── volume.go           VolumeHandle, Volume
│   ├── resolver.go         Resolver: control-plane lookup (info + Connection)
│   └── mapping.go          Mapper: attached lvol → local nvme.Device
├── kube/                   lvol ↔ PV / PVC / VolumeAttachment mapping
│   ├── names.go            driver name, param/context/label/finalizer keys
│   ├── identity.go         VolumeHandle↔PV, VolumeContext, StorageClass params
│   ├── binding.go          Binding: resolved PV+PVC+Node view of an lvol
│   ├── resolver.go         Resolver iface + ResolveBinding aggregation
│   ├── index.go            shared index names + pure key funcs
│   └── informer.go         InformerResolver: client-go informer-backed Resolver
├── controlplane/           Client for the simplyblock control-plane API
├── log/                    Shared slog-based structured logger
├── errs/                   Sentinel errors (errors.Is across packages)
│
├── internal/               Private — not importable by consumers
│   ├── sysfs/              low-level sysfs primitives (paths, attr reads)
│   ├── exec/               context-aware nvme-cli / command runner
│   └── version/            build metadata (stamped via -ldflags)
│
├── examples/
│   └── attach/             NodeStage-style flow, dependency-free
│
└── .github/workflows/ci.yml
```

## Design

- **Domain packages are public and read like the problem** (`nvme`,
  `nvmeof`, `lvol`), one cohesive concern each. No `pkg/` prefix.
- **Public APIs are interfaces** (`nvme.SubsystemResolver`/`nvme.DeviceResolver`, `nvmeof.Connector`,
  `lvol.Mapper`) so the operator and CSI driver can unit-test against
  fakes without a kernel, `/sys`, or `nvme-cli` present.
- **The Linux grunt work hides in `internal/`** (sysfs parsing, command
  execution). It can change freely; consumers depend on behavior, not
  mechanism.
- **Dependency direction flows one way**: `kube`/`controlplane` → `lvol`
  → `nvme`, with `errs`/`log`/`nqn` as leaf utilities. No import cycles.
- **Kubernetes deps are confined to `kube`.** Only that package imports
  `k8s.io/api` and `k8s.io/client-go`; importing `nvme`/`nvmeof`/etc.
  pulls no Kubernetes deps. (client-go is already in both the operator and
  CSI driver, so this adds nothing to either consumer's graph.)
- **One shared resolution implementation.** `kube.NewResolver(ResolverConfig)`
  returns an `InformerResolver` that works off any `cache.SharedIndexInformer`
  — a standalone client-go `SharedInformerFactory` (CSI driver) or a
  controller-runtime manager cache (operator) — so PV/PVC/VolumeAttachment
  caching lives here once instead of being reimplemented per consumer. The
  pure index key funcs in `index.go` are reused by both the client-go
  indexer and a controller-runtime `FieldIndexer`.
- **Errors are sentinels in `errs`** so CSI can map them to gRPC status
  codes (`ErrNotFound` → `codes.NotFound`) at its boundary.

## Development

```bash
go test -race ./...
go vet ./...
```
