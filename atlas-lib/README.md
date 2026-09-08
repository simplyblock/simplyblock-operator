# Atlas

Shared Go library for the simplyblock **Kubernetes operator** and **CSI
driver**. It holds the node-level storage primitives both consumers need
(NVMe discovery, NVMe-oF fabric management, NQN handling, and the
logical-volume ↔ NVMe-device mapping) so neither re-implements them.

![](../assets/simplyblock-logo.svg)

> Part of the [simplyblock-operator](../README.md) monorepo. For the repository overview, license,
> and contribution guidelines, see the [root README](../README.md).

The library lives in this monorepo and is consumed by the operator and CSI driver via a Go
`replace` directive (module path `github.com/simplyblock/atlas` → `../atlas-lib`). It is not
published or installed independently.

## Layout

```
atlas/
├── doc.go                  Library overview + package index
├── go.mod
│
├── nvme/                   Read-only NVMe subsystem/controller/namespace lookups
│   ├── device.go           Subsystem, Controller, Address, ANAState, Path, Namespace, Device
│   ├── resolver.go         SubsystemResolver (List/ByNQN) + DeviceResolver (List/ListWithSelector/ByUUID/ByDevicePath/ByNamespace)
│   ├── selector.go         DeviceSelector: NQN/NSID/UUID/device filter (Matches/Filter)
│   ├── reachability.go     Device.Accessible: can this device serve I/O
│   ├── sysfs_resolver.go   local impl: NewSysfsSubsystemResolver / NewSysfsDeviceResolver
│   ├── sysfs_scan.go       sysfs tree walk + attribute parsing
│   ├── siblings.go         Siblings (same volume, other paths) + CoTenants (other volumes, same subsystem)
│   ├── multinamespace.go   IsMultiNamespace / Controller.MaxNamespaces (MNAN)
│   └── identify*.go        Identify Controller ioctl (Linux) + field decoding
├── nvmeof/                 NVMe-oF fabric connect/disconnect (TCP)
│   ├── connector.go        Connector iface; Target, Targets + TargetOptions
│   ├── fabrics.go          local impl: NewFabricsConnector (/dev/nvme-fabrics)
│   ├── wait.go             ConnectMultipathDevice: attach all paths -> nvme.Device (start here)
│   ├── reconcile.go        ReconcilePaths: make attached paths match the control plane + PathState
│   ├── detach.go           DetachDevice: disconnect unless the subsystem is shared
│   └── multipath.go        the halves: ConnectPaths (ordered per-path connect) + PathResult
├── nqn/                    Build & parse simplyblock lvol NQNs
├── blockdev/               What a Linux block device is, and what it carries
│   ├── device.go           Device: path, kernel name, device numbers, block sizes, size, read-only
│   ├── content.go          Content, Reading, Reader/Opener seam, Prober.Read
│   ├── signatures.go       The signature catalog and the offsets each format writes to
│   ├── local_linux.go      OpenLocal (O_DIRECT) and ResolveDevice, plus a non-Linux stub
│   └── blkid.go            BlkidProber: the shadow the reading is migrating off
├── lvm/                    Linux LVM commands + content-based identity
│   ├── doc.go              Why identity is read from content, and how scoping is decided
│   ├── lvm.go              Manager, Run (the escape hatch)
│   ├── identity.go         VolumeGroup, HasLogicalVolume, ListLogicalVolumes, Rescan
│   ├── volume.go           Create/Activate/Deactivate/Remove a PV, VG, or LV
│   ├── clone.go            ResolveClonedVolumeGroup (rescan + import + rename)
│   ├── grow.go             Expand a PV, VG, or LV, read an LV's current size
│   ├── dm.go               RemoveOrphanedDMNodes
│   └── vdo/                VDO provisioning handler + the whole per-volume stack lifecycle
│       ├── volume.go       Registers itself with lvm, UpdateVolume
│       └── stack.go        CreateOrAttach, ResolveClone, Deactivate, Remove, Grow, SetFeatures
├── volstack/               A volume's node-side stack, as ordered layers
│   ├── layer.go            Layer, State, Artifact, Geometry + the optional interfaces (Composite, Healer, Grower, NodeRequirements, Recorder), Plan
│   ├── runner.go           Runner: Up / Down / Heal / Grow, and the order they walk the plan in
│   ├── record.go           Store: what was planned for a volume and how far bring-up got
│   ├── layers/             The layer implementations
│   │   ├── fabric.go       fabric: the volume's namespace, attached
│   │   ├── members.go      members: several namespaces presented upward as one
│   │   ├── lvmpv.go        lvmPhysicalVolume: the device labeled into this volume's group
│   │   ├── lvmvolumegroup.go  lvmVolumeGroup: the group between the physical volumes and the logical one
│   │   ├── lvmvolume.go    lvmLogicalVolume: linear, striped, or VDO, by definition
│   │   ├── filesystem.go   filesystem: format if blank, mount, refuse anything else
│   │   └── filesystem_strategy.go  FilesystemLayerStrategy: what each fs type formats, mounts, and resizes with
│   └── plans/              The plan shapes: one constructor per row of the design's plan table
│       ├── plans.go        RawBlock / Plain / LVM / Striped + the LVM naming rule
│       └── node.go         NodeConfig: the seams every plan on this host is built over
├── lvol/                   Logical-volume identity, control-plane + device resolution
│   ├── volume.go           VolumeHandle, Volume
│   ├── resolver.go         Resolver: control-plane lookup (info + Connection)
│   └── mapping.go          Mapper: attached lvol → local nvme.Device
├── kube/                   lvol ↔ PV / PVC / VolumeAttachment mapping
│   ├── names.go            driver name, param/context/label/annotation/finalizer keys
│   ├── identity.go         VolumeHandle↔PV, VolumeContext, pin annotations
│   ├── binding.go          Binding: resolved PV+PVC+Node view of an lvol
│   ├── resolver.go         Resolver iface + ResolveBinding aggregation
│   ├── storageclass.go     Properties: typed StorageClass provisioning params
│   ├── params.go           String/Int/Float/BoolParam map helpers
│   ├── index.go            shared index names + pure key funcs
│   ├── informer.go         InformerResolver: client-go informer-backed Resolver
│   ├── live.go             LiveResolver: uncached clientset-backed Resolver
│   └── id.go               DNS-label-safe short ids / object names
├── controlplane/           Client for the simplyblock control-plane v2 API
│   ├── client.go           Config + New (bearer auth, timeout)
│   ├── volumes.go          create / clone / list / resize / delete / connection
│   ├── pools.go            storage pools (incl. by-name lookup)
│   ├── storagenodes.go     storage nodes + their data NICs
│   └── migrations.go       subsystem migrations: create / get / list / continue / cancel, of a volume or a whole subsystem
├── link/                   gRPC operator ↔ CSI, over connections the CSI driver opens
│   ├── doc.go              why the connection runs backwards, and how gRPC still works on it
│   ├── session.go          Session: yamux-multiplexed link, gRPC server + client on both ends
│   ├── hub.go              Hub: the operator side; accepts links, serves the handshake
│   ├── agent.go            Agent: the CSI side; dials, identifies, serves, reconnects
│   ├── registry.go         Registry: who is linked right now (ErrNoSession when not)
│   ├── peer.go             PeerKind/PeerID/Claim/Identity/Peer
│   ├── auth.go             Authenticator iface, bearer-token plumbing, TokenSource
│   ├── kubeauth.go         KubeAuthenticator: TokenReview → the pod's node, not the peer's word
│   ├── dial.go             TLSDialer / InsecureDialer
│   └── linkv1/             the Hello handshake protocol (buf-generated, committed)
├── storage/                one node's storage as one value, with no gRPC in here
│   ├── accessor.go         Accessor: every lookup flat, plus the questions that re-scan
│   │                       Local(sysfs) fills one in; storagerpc.Remote fills the same struct
│   └── storagerpc/         the same Accessor, served over a link and reached over one
│       ├── server.go       NewServer(storage.Accessor); Register is the link agent's hook
│       ├── client.go       Remote(conn) + the nvme resolver interfaces, remoted
│       ├── convert.go      nvme snapshot types ↔ wire form (total, both directions)
│       └── storagev1/      SubsystemService + DeviceService (buf-generated, committed)
├── statemachine/           Deterministic state machine declared as data
│   ├── statemachine.go     Config, StateDef, Machine, Snapshot, deadlines
│   ├── multiconfig.go      MultiConfig: one graph per action over one state type
│   └── kubernetes.go       KubeSnapshot + ToKube/FromKube: the CRD form of a Snapshot
├── net/                    Outbound URL validation (SSRF guard)
├── ptr/                    Pointer/optional-field helpers for generated + K8s types
├── errs/                   Sentinel errors (errors.Is across packages)
│   └── deferrers/          defer-friendly Close/Run that log instead of dropping errors
│
├── internal/               Private — not importable by consumers
│   ├── cpapi/              oapi-codegen client for the control-plane v2 API (generated)
│   ├── sysfs/              low-level sysfs primitives (paths, attr reads)
│   └── version/            build metadata (stamped via -ldflags)
│
└── .github/workflows/ci.yml
```

## Use cases

The flows below are the ones the operator and CSI driver actually perform. Each
shows the atlas-idiomatic implementation, and a _Today_ note points at the live call
site where one exists, so it is visible which patterns are already wired and
which are available but not yet adopted.

All examples assume the usual preamble:

```go
client, err := controlplane.New(controlplane.Config{
    Endpoint: clusterEndpoint,
    Token:    clusterSecret, // cluster secret, sent as a bearer token
    Timeout:  30 * time.Second,
})
```

### Control plane

#### Provision a volume for a StorageClass

The CSI controller's `CreateVolume`. The returned `lvol.VolumeHandle`
(`clusterID:poolID:volumeID`) is the CSI `volume_id` and the value that ends up
in `PV.Spec.CSI.VolumeHandle`.

```go
// StorageClass parameters arrive as a map; parse them with the shared helpers
// so operator and CSI agree on keys and defaults.
maxNS, err := kube.IntParam(params, kube.ParamMaxNamespacePerSubsys, 1)
if err != nil {
    handleError(err)
}

// Pools are addressed by UUID; a class names one, so resolve it once.
poolName := kube.StringParam(params, kube.ParamPool, "")
pool, err := client.StoragePoolByName(ctx, clusterID, poolName)
if err != nil {
    handleError(err) // errors.Is(err, errs.ErrNotFound) → misconfigured class
}

handle, err := client.CreateVolume(ctx, clusterID, pool.ID, controlplane.CreateVolumeParams{
    Name:                  req.GetName(),
    SizeBytes:             uint64(capacity),
    HAType:                "ha",
    Namespaced:            maxNS > 1,
    MaxNamespacePerSubsys: maxNS,
    HostID:                placementTarget, // from the pin/hint annotations, may be ""
    PVCName:               pvcName,
})
```

`ResizeVolume`, `DeleteVolume`, `CloneVolume` and `ListVolumes` complete the
lifecycle. `DeleteVolume` is idempotent, since an already-absent volume is not
an error, so a retried `DeleteVolume` RPC needs no pre-check.

_Today:_ `csi-driver/internal/csi/controller` uses the `kube` param helpers
but still calls the control plane through its own `internal/controlplane/cluster.go` client.

#### Migrate a subsystem to another storage node

The operator's `VolumeMigration` reconciler. A migration is created, observed by
phase, then either continued past its pre-created checkpoint or canceled.

**A migration is addressed by subsystem, not by volume.** One subsystem exports
several volumes on a namespaced pool, so the volume is not the thing that moves:
a subsystem configured for several namespaces migrates as one coordinated group,
and one configured for a single namespace migrates that volume. The control
plane decides which from the subsystem's own namespace capacity, so a request
names a target node and nothing about the shape, and the answer says which was
made.

```go
migration, err := client.CreateMigration(ctx, clusterID, nqn, targetNodeID)
if err != nil {
    handleError(err)
}

// Poll until the control plane parks the migration at its pre-created
// checkpoint, then validate the new paths before committing to the cutover.
for {
    m, err := client.GetMigration(ctx, clusterID, nqn, migration.ID)
    if err != nil {
        handleError(err)
    }
    switch m.Kind {
    case controlplane.MigrationOfVolume:
        log.Info("migration", "phase", m.Phase,
            "snapshots", fmt.Sprintf("%d/%d", m.SnapsMigrated, m.SnapsTotal))
    case controlplane.MigrationOfSubsystem:
        // A group has no single volume whose snapshots could be counted.
        log.Info("migration", "phase", m.Phase, "members", m.MemberCount)
    }
    if m.Phase == "pre_created" { // atlas keeps Phase a plain control-plane string
        break
    }
    // ... sleep / requeue
}

if err := validateTargetPaths(ctx); err != nil {
    // Roll back rather than cut over to paths the consumer cannot reach.
    _ = client.CancelMigration(ctx, clusterID, nqn, migration.ID)
    handleError(err)
}
if err := client.ContinueMigration(ctx, clusterID, nqn, migration.ID); err != nil {
    handleError(err)
}
```

`ListMigrations` returns everything one subsystem has in flight, of both kinds,
because the control plane returns them in one list. `Kind` is decided on a field
the other shape does not carry rather than on whether a decode succeeds: both
shapes decode as each other, and a group read as a volume's migration is a
migration of volume "" with no snapshots, which reads as a finished one.

_Today:_ `operator/internal/controller/volumemigration_controller.go` runs
exactly this sequence against the operator's own `internal/webapi` client, and
already addresses migrations by cluster and subsystem NQN, which is what these
calls take.

#### Choose or validate a placement target

The volume-placement webhook picks the least-loaded node at creation time. The
PVC pin controller only needs to confirm that a user-supplied node exists.

```go
nodes, err := client.ListStorageNodes(ctx, clusterID)
if err != nil {
    handleError(err)
}

var best controlplane.StorageNode
for _, n := range nodes {
    if n.Status != "online" || (n.MaxLvols > 0 && n.Lvols >= n.MaxLvols) {
        continue // full or unavailable
    }
    if best.ID == "" || n.Lvols < best.Lvols {
        best = n
    }
}

// The data-plane addresses a node exports (traddr candidates for a connect).
nics, err := client.ListStorageNodeNICs(ctx, clusterID, best.ID)
```

### Kubernetes correlation

#### Wire the resolver once per consumer

`kube.Resolver` is the single seam for every PV/PVC/VolumeAttachment/StorageClass
lookup. Pick the implementation that matches the consumer's caching, and keep the
rest of the code on the interface.

```go
// CSI driver: a standalone client-go informer factory.
resolver, err := kube.NewResolverFromFactory(factory)

// Operator: the controller-runtime manager cache — same implementation, so
// PV/PVC caching is not reimplemented per consumer.
resolver, err := kube.NewResolver(kube.ResolverConfig{
    PersistentVolumes:      pvInformer,
    PersistentVolumeClaims: pvcInformer,
    VolumeAttachments:      vaInformer, // optional; nil ⇒ Node/Attached stay zero
    StorageClasses:         scInformer, // optional; nil ⇒ ErrUnsupported
})

// One-shot paths, or where a stale cache is unacceptable: uncached reads.
resolver := kube.NewLiveResolver(clientset)
```

Indexers must be registered before the informers start. A controller-runtime
operator that indexes on its own manager cache should reuse the exported key
funcs so both consumers index identically:

```go
mgr.GetFieldIndexer().IndexField(ctx,
    &corev1.PersistentVolume{}, kube.IndexPVByVolumeHandle,
    func(o client.Object) []string {
        return kube.VolumeHandleKeys(o.(*corev1.PersistentVolume))
    })
```

_Today:_ `operator/internal/controller/volumemigration_controller.go` and
`operator/internal/autoplacement/logical_volume_selector.go` hold an
`atlaskube.Resolver` backed by `NewLiveResolver`.

#### Answer "where is this volume attached?"

One call assembles the cross-resource view a drain, migration, or rebalancing
decision needs.

```go
binding, err := kube.ResolveBinding(ctx, resolver, handle)
if err != nil {
    handleError(err) // errs.ErrNotFound: no PV carries this handle
}
// binding.PersistentVolumeName / .PersistentVolumeClaim / .Node / .Attached
```

#### Split a volume handle instead of parsing it by hand

Every control-plane call keys off the three UUIDs the handle encodes.

```go
handle, err := kube.VolumeHandleFromPV(pv) // errs.ErrUnsupported for foreign PVs
if err != nil {
    handleError(err)
}
clusterID, poolID, volumeID, err := handle.Split()
```

_Today:_ several operator call sites still do `strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)`
(`logical_volume_selector.go`, `persistentvolumeclaim_controller.go`).
`VolumeHandle.Split` is the typed replacement and rejects malformed handles.

#### Read how a volume was provisioned

When you hold the `StorageClass` object (operator), parse the whole parameter set
at once instead of key by key.

```go
props, err := kube.ResolvePropertiesForPV(ctx, resolver, pv)
if err != nil {
    // errs.ErrUnsupported: not our driver; errs.ErrNotFound: PV names no class
    handleError(err)
}
_ = props.Pool
_ = props.QoS.RWIOPS
```

#### Exclude volumes that must not be moved alone

A namespaced volume shares its NVMe subsystem with siblings, so migrating or
rebalancing it disturbs every co-tenant. The StorageClass answers this centrally,
without host access:

```go
if props.IsMultiNamespace() { // max_namespace_per_subsys > 1
    // migration is subsystem-wide: skip rebalancing, or flag the migration
}
```

An unresolvable StorageClass is treated as single-namespace and logged in both
consumers, because one unreadable class must not stall a migration.

_Today:_ `volumemigration_controller.go:isMultiNamespaceMigration` and the
rebalancer's namespaced-set collection in `logical_volume_selector.go`.

#### Respect a pinned volume

Pins block drains and rebalancing and drive
pin-change migrations. Use the helpers rather than reading annotations directly:
they encode the precedence (canonical annotation, then the two legacy host-id
forms) and deliberately exclude the one-shot `AnnoPlacementHint`, which is a
creation hint and not a pin.

```go
if kube.IsPinnedVolume(pvc.Annotations) {
    target := kube.PinnedNode(pvc.Annotations)
    // Migrate only when the pin actually changed, so the controller's own
    // writes do not re-trigger one.
    if target != pvc.Annotations[kube.AnnoSelectedStorageNodeApplied] {
        requestMigration(target)
    }
}
```

_Today:_ `operator/internal/controller/persistentvolumeclaim_controller.go`,
`simplyblockstoragenodeset_drain.go`, the rebalancer, and the placement webhook.

#### Name generated objects

Migration Jobs, per-volume CRs and the like, named so the result is a valid DNS
label even for long prefixes:

```go
name := kube.NameWithID("mig-" + pv.Name) // "<prefix>-<6 char id>", ≤63 chars
```

Ids are random, not derived: retry with a fresh call on a name collision.

### Node & fabric

Everything in this section goes through `storage.Accessor`, one node's storage
as one value. There are two ways to get one, and the code after that point is
identical either way:

```go
// On the node itself:
store := storage.Local(nvme.SysfsConfig{})

// In the operator, for a node at the other end of a link:
store := storagerpc.Remote(peer.Conn())
```

Both give a `storage.Accessor`, which is a plain struct holding the two
resolvers:

```go
type Accessor struct {
    SubsystemResolver nvme.SubsystemResolver
    DeviceResolver    nvme.DeviceResolver
}
```

Call it two ways. The lookups are flat on the accessor and each says what it
returns, which is the usual way:

```go
dev, err := store.DeviceByUUID(ctx, lvolUUID)
sub, err := store.SubsystemByNQN(ctx, nqn)
all, err := store.ListDevices(ctx)
```

Or reach a resolver through its field, which is what you do when something else
wants the interface. `nvmeof.ConnectMultipathDevice(ctx, c, store.DeviceResolver, …)`
below is the example:

```go
store.DeviceResolver     // nvme.DeviceResolver
store.SubsystemResolver  // nvme.SubsystemResolver
```

#### Decide whether a device may be formatted

The reading an irreversible write rests on: a `mkfs` or a `pvcreate` may only run
against a device positively read as holding nothing. Asking a tool what it
recognized cannot answer that. `blkid` reports "no filesystem here" and "this
device could not be read" with the same exit code, and it reports nothing at all
for a signature it does not know: a Linux software-RAID member with metadata 1.1
makes it exit 2 on a device that is neither degraded nor unreadable. So the
reading is taken from the device's own bytes, and a read failure is an error
rather than any reading at all.

```go
dev, err := blockdev.ResolveDevice("/dev/disk/by-id/nvme-...")  // size and block size
if err != nil {
    // The device could not be inspected. Refuse, and let the caller retry.
}

reading, err := blockdev.NewProber().Read(ctx, dev)
switch {
case err != nil:
    // The device could not be read. Never treat this as an empty device.
case reading.Content == blockdev.ContentBlank:
    // Positively all zeros in the first and last mebibyte. The only reading
    // that permits a format.
case reading.Content == blockdev.ContentFilesystem:
    // reading.Type names it. Mount it; never re-probe and never hand it to a
    // helper that formats on its own probe.
case reading.Content == blockdev.ContentStackLayer:
    // An LVM physical-volume label. Activate the stack; do not pvcreate.
default:
    // ContentForeign: somebody else's data. Refuse, and say what was found
    // through reading.Detail.
}
```

`nvme.Namespace.BlockDevice()` produces the same `Device` for an attached volume,
which is the path a node service takes rather than resolving one from `/dev`.

_Today:_ the reading is built and covered by images captured from devices real
tools formatted (`atlas-lib/blockdev/testdata/images`, regenerated by
`hack/blockdev/capture-image.sh`). Its consumers are still on the blkid probe:
the CSI driver's `NodeStageVolume` calls `BlkidProber` through `probeDiskFormat`
in `csi-driver/internal/csi/node`, and moving it onto `Read` is Phase 1 of
[`design-device-content-detection.md`](../operator/docs/designs/design-device-content-detection.md).

#### Attach a volume

The CSI node service's `NodeStageVolume`. Outside a single-node installation the
control plane answers with several endpoints in descending priority (primary,
secondary, tertiary), and attaching means
establishing *all* of them, in that order: a single-path attach leaves the
volume one node failure away from losing I/O, and connecting out of order hands
I/O to the wrong node until the kernel has the full ANA picture. So the flow is
always "ask the control plane where the volume lives → build a target per path →
connect them in order → wait for the block device."

```go
// The wait for a path to go live is bounded per path (10s by default), not per
// connect, so one unreachable node cannot eat the whole NodeStage deadline.
connector := nvmeof.NewFabricsConnector(nil, // nil ⇒ local sysfs resolver
    nvmeof.WithPathTimeout(15*time.Second))
store := storage.Local(nvme.SysfsConfig{})

// controlplane.Client implements lvol.Resolver, so the node service can depend
// on the interface and be tested without a control plane.
conn, err := client.Connection(ctx, handle)
if err != nil {
    handleError(err) // errs.ErrNotConnected: the volume is not published
}

// One target per endpoint, in the control plane's priority order. The connect
// tunables come with each endpoint; options override them and add what only
// the node knows (its host identity, a local timeout policy).
targets := nvmeof.Targets(conn,
    nvmeof.WithCtrlLossTMOSec(60), // explicit: 0 and -1 are both meaningful
    nvmeof.WithHostNQN(hostNQN),
)

// The whole of NodeStage in one call: attach every path in the given order,
// then wait for the block device that comes up behind them. NSID picks the
// namespace on a multi-namespace subsystem; 0 means the subsystem's only one.
//
// It takes an nvme.DeviceResolver rather than the accessor, hence the field —
// and it must be a local one, because the wait resolves device symlinks against
// whatever filesystem it runs on. Never hand it a storagerpc.Remote.
dev, results, err := nvmeof.ConnectMultipathDevice(ctx, connector,
    store.DeviceResolver, targets, nvme.NamespaceID(conn.NSID))

// results comes back even on error, so per-path reporting happens either way.
for _, r := range results {
    switch {
    case !r.Live:
        log.Info("path unavailable", "address", r.Target.Address, "error", r.Err)
    case r.AlreadyPresent:
        log.V(1).Info("path already attached", "address", r.Target.Address)
    }
}
if err != nil {
    handleError(err) // no path came up at all, or none produced a device
}
stage(dev.Namespace.DevicePath) // /dev/nvme0n1 — the multipath head, not a leg
```

Prefer the composed call over assembling it by hand. Both halves have a failure
mode that is easy to get wrong and expensive when you do: the paths must be
attached in the control plane's order, because the first one up carries I/O
until the kernel has the full ANA picture; and the device wait must not guess
between a freshly connected namespace and a stale one the kernel has yet to
reap, since handing back the wrong block device is a data-corruption-grade
mistake. A path whose node is down is skipped, not reordered, so a partially
reachable volume still stages.

Connecting is idempotent per path, since a controller already fronting an
endpoint is left alone rather than duplicated, so a retried NodeStage
re-establishes only
what is missing. `Connector.Connect` is the single-path form (`ConnectPaths` with
one target), and `nvmeof.ConnectDevice` pairs it with the device wait for the
cases that genuinely have one path.

Once attached, `nvme.DeviceSelector` addresses the volume in any later lookup,
with `ListDevicesBySelector` when a caller wants to see *every* match rather
than the first:

```go
sel := nvme.DeviceSelector{NQN: conn.NQN, UUID: volumeID.String()}
matched, err := store.ListDevicesBySelector(ctx, sel)
```

_Today:_ the CSI node service still connects, repairs, and ANA-reconciles paths
with nvme-cli in `csi-driver/internal/initiator/initiator.go`. `FabricsConnector` is the
kernel-direct replacement (it needs no nvme-cli binary in the node image).

#### Detach without collateral damage

Disconnecting a subsystem tears down every namespace on it, so a namespaced
volume must not disconnect on unstage:

```go
out, err := nvmeof.DetachDevice(ctx, connector, dev)
if err != nil {
    handleError(err) // the question went unanswered; the fabric was not touched
}
if out.SharedSubsystem {
    // Unmount only, leave the fabric up for the volumes that share it.
    // store.CoTenants(ctx, dev) names the current ones for an event.
    return nil
}
```

The gate is `nvme.Device.IsMultiNamespace`, meaning *can* this subsystem hold
other volumes, not whether it currently does. Enumerating the neighbors describes
only the moment it was looked at: a namespace can join a shared subsystem
between the check and the disconnect, and then a correct "none right now" answer
is still destructive. So a subsystem provisioned to be shared is never
disconnected on one volume's behalf, even while it happens to hold only that one.

That answer sometimes needs an Identify Controller command, which wants a live
controller and Linux. `DetachDevice` returns the error rather than assuming, so
reaping a subsystem whose controllers are all dead is an explicit
`connector.Disconnect`, never a default.

`Disconnect` takes down every path of the subsystem, releasing them in ANA order
(unusable and non-optimized legs first, the optimized one last) so I/O still in
flight keeps the best path it has until the end.

Every one of these questions also has a pure form taking a snapshot the caller
owns, which is the cheap way to sweep many devices, since one `List` answers all
four for all of them:

```go
all, err := store.ListDevices(ctx)
if err != nil {
    handleError(err)
}
for _, d := range all {
    if nvme.HasCoTenants(d, all) || nvme.HasSiblings(d, all) {
        report(d, nvme.CoTenants(d, all), nvme.Siblings(d, all))
    }
}
```

#### Watch and repair path health

The node-side connection guardian. `Device` values are immutable snapshots, so
re-resolve to observe change rather than expecting a value to update.

```go
dev, err := store.DeviceByUUID(ctx, volumeID.String()) // fresh snapshot
if err != nil {
    handleError(err)
}

// One question, whatever the multipath configuration: can this volume serve
// I/O? Accessible weighs the ANA view when there is one and falls back to
// controller liveness when the kernel publishes no per-path legs.
if !dev.Accessible() {
    log.Info("volume attached but unusable", "device", dev.Namespace.DevicePath)
}
for _, p := range dev.Namespace.Paths {
    if !p.ANAState.Accessible() { // inaccessible / persistent-loss / change
        log.Info("path carries no servable I/O", "path", p.Name, "ana", p.ANAState)
    }
}

// How many paths *should* exist is a control-plane question — the set changes
// after a migration or a node replacement — so repair re-asks and reconnects.
conn, err := client.Connection(ctx, handle)
if err != nil {
    handleError(err)
}

// The whole repair in one call: establish the published paths that are not up,
// in priority order, and report what is left over. Safe on every tick —
// connecting is idempotent per path, so a volume already attached over all of
// them costs one controller lookup each and changes nothing.
state, err := nvmeof.ReconcilePaths(ctx, connector, store.SubsystemResolver, conn,
    nvmeof.WithCtrlLossTMOSec(60))
if err != nil {
    handleError(err) // non-nil only when no path could be established at all
}
switch {
case state.Down():
    log.Error(nil, "volume cannot serve I/O", "nqn", state.NQN)
case state.Degraded():
    log.Info("volume short of paths", "live", state.Live, "expected", state.Expected)
}

// Stale paths are reported, never removed: a path missing from the control
// plane's answer right now is not necessarily gone for good — a node in restart
// is the obvious case — and tearing down a controller changes a live data path.
for _, c := range state.Stale {
    log.Info("attached path no longer published", "controller", c.ID, "address", c.Address.TrAddr)
}
```

`connector.IsConnected(ctx, nqn)` is the cheap coarse check ("any live
controller at all") for callers that only need to know whether the subsystem is
attached, e.g., before deciding to clean up.

#### Find the other devices of one volume

A volume can surface as several block devices sharing its namespace UUID when a
stale controller has not been reaped yet, and a teardown has to release every
one of them. This is a state to detect and clear, not one to run in: a live
simplyblock volume is one namespace head, with its paths selected inside it by
ANA state.

```go
siblings, err := store.Siblings(ctx, dev) // re-scans the node
if err != nil {
    handleError(err)
}
for _, s := range siblings {
    release(s.Namespace.DevicePath)
}
```

These live on the accessor, not on `nvme.Device`, because they re-scan and a
rescan needs a resolver — a device that quietly carried one would hide the cost,
which over a link is a round trip each. `nvme.Siblings(dev, all)` is the pure
form over a snapshot you already hold, and the one to use when sweeping many
devices.

Siblings and co-tenants are the two opposite relations, and mixing them up is a
data-loss bug: siblings are the *same* volume reached another way and all have to
go, co-tenants are *other* volumes that must be left alone. `nvme.IsSibling` and
`nvme.IsCoTenant` are the single-pair predicates behind all of them.

#### Detect a namespaced volume host-side

Where no StorageClass is in reach. Conclusive sysfs cases cost nothing, and only a
lone namespace at NSID 1 needs an Identify Controller command (Linux only, live
controller required):

```go
multi, err := dev.IsMultiNamespace()
switch {
case errors.Is(err, errs.ErrUnsupported):   // not Linux
case errors.Is(err, errs.ErrNotConnected):  // no live controller to Identify
}
```

#### Build or parse an lvol NQN

Without string formatting at the call site:

```go
subsysNQN := nqn.Make(clusterID.String(), volumeID.String())

if s, ok := nqn.Parse(dev.Subsystem.NQN); ok {
    _, _ = s.ClusterID, s.LvolID
}
```

A host NQN is the same story from the other end. `nqn.Host(nodeUID)` composes
the identity an access-controlled pool authorizes, and `nqn.HostUUID` reads it
back out — which is what a `--hostid` has to be derived from, since the kernel
pairs hostid with hostnqn by comparison.

_Today:_ the CSI driver builds every host NQN with `nqn.Host` (its node service
and its reconnect loop), derives the DHCHAP `--hostid` with `nqn.HostUUID`
(`csi-driver/internal/initiator`), and reads the cluster and lvol out of a
subsystem NQN with `nqn.Parse` (`internal/controlplane`, `internal/initiator`,
`internal/reconnect`). It kept its own copies of the last two until the
`internal/csi/common` split, when they were adopted and deleted.

#### Assemble an LVM stack on a device

LVM answers "which device does this volume group live on" by scanning devices
and matching the UUIDs and names it finds written in their content. That is
unambiguous while no two visible devices carry the same content, which is the
normal case here: every volume group is named after its own lvol and every
`pvcreate` mints a fresh PV UUID, so a node's other tenants cannot answer a
lookup meant for this volume however many are attached.

Cloning breaks that on purpose. A byte-level clone or snapshot restore copies
its source's PV and VG UUIDs *and its VG name* verbatim, so from the moment a
clone is attached beside its source until `ImportClonedVolumeGroup` has
re-stamped it, the two are the same volume group as far as a name lookup is
concerned. Two consequences, both confirmed live:

- **A scan reports a duplicate PV** and can resolve later commands against
  whichever of the two its cache happened to pick.
- **A name-based existence check isn't tied to a device.** `vgs <name>` answers
  "does a volume group called X exist anywhere LVM is allowed to look?" rather
  than "does it exist *on this device*." That reported a volume group as
  already present when it had never been created on the device being asked
  about, leaving no logical volume behind it and failing `mkfs`.

This package answers identity questions from a device's own content rather than
a name lookup, and scopes the commands that need it (`--devices`) so a scan
cannot reach the other copy. **Which commands need it is the package's decision,
not its caller's**, and no method takes a device list. A command that names a
device scopes itself to it. A command that addresses a volume group or logical
volume by name runs unscoped, because by then the name is unique.

Identity is typed, not a bare string: `PhysicalVolume`, `VolumeGroup`, and
`LogicalVolume` each wrap the one string that identifies them, so passing a
device path where a VG name belongs is a compile error, not an LVM failure
discovered at runtime. `LogicalVolume` carries its `VolumeGroup` rather than a
bare name for the same reason: a caller pairing the wrong VG with an LV by
hand is a mistake the type system catches instead of one that surfaces as
"volume group not found." None of the three references a `Manager`: they are
plain values, comparable with `==`, and the same value works with any
`Manager` instance.

```go
mgr := lvm.NewManager()
pv := lvm.PhysicalVolume{DevicePath: devicePath}

// Content-based, not "vgs <name>": the zero VolumeGroup means genuinely blank,
// or unreadable — both read the same way to a caller deciding whether to
// create fresh.
volumeGroup, err := mgr.VolumeGroup(ctx, pv)
if err != nil {
    handleError(err)
}
switch {
case volumeGroup != expectedVolumeGroup:
    // Genuinely blank device (or a foreign identity to resolve first, e.g. a
    // byte-level clone) — create fresh.
default:
    logicalVolume := lvm.LogicalVolume{VolumeGroup: volumeGroup, Name: logicalVolumeName}
    hasLV, err := mgr.HasLogicalVolume(ctx, logicalVolume)
    if err != nil {
        handleError(err)
    } else if hasLV {
        err = mgr.ActivateVolumeGroup(ctx, volumeGroup) // reactivate, never recreate
    } else {
        // Orphaned: pvcreate/vgcreate completed, the final lvcreate did not.
        err = mgr.RemoveVolumeGroup(ctx, volumeGroup) // fall through to a fresh create
    }
}

if _, err := mgr.CreatePhysicalVolume(ctx, pv); err != nil {
    handleError(err)
}
// Variadic: one device for VDO, several for a striped volume group.
if _, err := mgr.CreateVolumeGroup(ctx, expectedVolumeGroup, pv); err != nil {
    handleError(err)
}
if _, err := mgr.CreateLogicalVolume(ctx, expectedVolumeGroup, poolName, logicalVolumeName, def); err != nil {
    handleError(err)
}
```

Every named method has this shape: build the right LVM/dm-vdo command, scope it
if its operands call for that, and wrap the error with the operation it was
attempting. `Run` stays available as an escape hatch for a command that doesn't
have a named method yet, and is always unscoped: a command that has to be scoped
belongs in the package as a named method, since the scope follows from the
operands. Reaching for `Run` first is exactly the duplication this package
exists to prevent.

A freshly attached device may turn out to be a byte-level clone or snapshot
restore of another volume, and resolving that is one call, safe and cheap to
make on any device before staging it:

```go
// Refresh, probe the device's own identity, and if it is somebody else's
// volume group, re-stamp it and rename the logical volume inside. Returns the
// foreign VolumeGroup it found, or the zero value when there was nothing to
// resolve.
previous, err := mgr.ResolveClonedVolumeGroup(ctx, pv, volumeGroup, logicalVolumeName, poolName)
if err != nil {
    handleError(err)
}
if previous != (lvm.VolumeGroup{}) {
    log.Warnf("device %s carried a foreign VG identity %q, re-stamped as %s",
        devicePath, previous.Name, volumeGroup.Name)
}
```

The order is not the caller's to get right, which is why the sequence lives in
the package: the refresh has to precede the probe or the probe reads a stale
cache, the probe has to be content-based or it cannot see a foreign identity at
all (the volume group on disk is still named after the source), and the rename
has to follow the import because `vgimportclone` renames the volume group but
leaves the logical volume inside named after the source. The trailing arguments
name logical volumes to preserve, for the structural ones a stack names
identically in every volume, such as VDO's pool. `ImportClonedVolumeGroup` and
`RenameLogicalVolume` remain available for a recovery path that needs one step
alone.

VDO lives in the `lvm/vdo` subpackage rather than in `lvm` itself. It registers
a provisioning handler at init, and `CreateLogicalVolume` consults the registry
for the extra `lvcreate` flags a `LogicalVolumeDefinition` implies, so a caller
asks for compression or deduplication instead of knowing how dm-vdo spells it.
Importing the subpackage is what makes those flags reachable, and
`vdo.UpdateVolume` toggles them on a pool that already exists.

`RemoveOrphanedDMNodes` is the fallback when the backing device is already gone
and `RemoveVolumeGroup`/`DeactivateVolumeGroup` can no longer read the metadata
they need: it clears the live dm nodes directly, retrying across a few passes so
removing a dependent unblocks what it was blocking.

`lvm/vdo` also holds the whole per-volume stack lifecycle a caller actually
drives, not only the provisioning handler: `CreateOrAttach` (idempotent
create-or-reactivate), `ResolveClone` (a thin wrapper over
`ResolveClonedVolumeGroup`, naming VDO's own volume group/pool convention),
`Deactivate`/`Remove` (each with its own rule for when an unreachable backing
device falls back to `RemoveOrphanedDMNodes`: `Deactivate` only on that specific
failure, `Remove` unconditionally, since one is trying to preserve the volume
and the other is already destroying it), `Grow`, and `SetFeatures` (a
lvolID-keyed wrapper over `UpdateVolume`). Every one of them is keyed by
lvolID alone. The volume group/pool naming convention stays internal to this
package rather than leaking to a caller. None of it references a Kubernetes
type: it is node-level orchestration that happens to live in a CSI driver
today, not CSI-shaped logic, and `Logger` (a package-level `*slog.Logger`,
nil-safe) is how a caller gets its own log format without this package taking
on a Kubernetes-specific logging dependency.

_Today:_ `lvm/vdo` is the only in-tree consumer. The CSI driver's client-side
VDO support (`csi-driver/internal/mount/vdo.go`) is the code this package was
extracted from, and now just wires `vdo.CreateOrAttach`/`ResolveClone`/
`Deactivate`/`Remove`/`Grow` into `NodeStageVolume`/`NodeUnstageVolume`/
`NodeExpandVolume`. A striped LVM volume group across several members would use
`CreateVolumeGroup`'s variadic device-path list the same way.

#### Bring up a volume's stack

`volstack` is a volume's node side expressed as ordered layers, and
`volstack/plans` is the catalog of the orders that mean something. Build the node
once per process, because the seams are the host's rather than any volume's, then
name the kind of volume and hand the plan to the runner.

```go
node := plans.NewNode(plans.NodeConfig{
	HostNQN:    hostNQN,
	HostID:     hostID,
	Connector:  nvmeof.NewCLIConnector(subsystems),
	Devices:    nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{}),
	Manager:    lvm.NewManager(),
	Content:    blockdev.NewProber(),
	Filesystem: mounter, // the consumer's own mount library
})

plan := node.Plain(connection, plans.Volume{
	UUID:        handle.VolumeID,
	StagingPath: stagingPath,
	FsType:      fsType,
})

runner := volstack.NewRunner(volstack.NewStore("/var/lib/simplyblock/stacks"))
artifact, err := runner.Up(ctx, handle.String(), plan)
```

There are four shapes, and they are the design's plan table: `RawBlock`
(`fabric` alone, which is raw block mode as a shorter plan rather than a flag
inside a stage function), `Plain` (`fabric` → `filesystem`), `LVM` (`fabric` →
`lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem`, for
client-side deduplication or compression), and `Striped` (the same four above a
`members` composite, for a volume assembled over several namespaces). The last
two take a `LogicalVolumeOptions`, which is the only thing separating a linear
volume from a VDO or a striped one: one layer, three definitions.

Deciding which shape a volume gets stays with the consumer, because that answer
comes from a StorageClass, a volume capability, and the node's role for the
volume, none of which this library knows about. What it does own is the layer
list each shape means, so that a node service and the tests that cover it cannot
disagree about what a staged volume looks like.

`VolumeGroupName` and `LogicalVolumeName` are the LVM naming rule on their own,
for a caller that has a volume's identity and no plan: a teardown driven from a
stack record, or a sweep looking for what this driver left behind, has to name
the group the way the plan that created it did, character for character.

Building a plan reaches nothing. It resolves no device, runs no command, and
reads no sysfs, so a consumer can unit-test the selection it makes and only the
runner needs a host.

_Today:_ nothing in the operator or the CSI driver imports `volstack` yet. The
on-node integration suite (`test/integration/onnode`) is the only caller, and it
fills the seams with the implementations that ship: nvme-cli, the sysfs
resolvers, `lvm.Manager`, and `blockdev.Prober`. `NodeStageVolume` still
assembles its own fabric connect, `mkfs`, and mount in
`csi-driver/internal/csi/node`, which is the call site the `Plain` and `LVM`
shapes are meant to replace.

### Operator ↔ CSI link

#### Reach a node's services from the operator

The operator needs to ask nodes questions, but nothing listens on a node. The CSI
pods dial the operator instead and hold the connection open; the operator issues
its RPCs back down it. Each link is a yamux session, so both ends run an ordinary
gRPC server *and* an ordinary client on it — which end dialled stops mattering
once the session exists.

Operator side, as a leader-election `Runnable`:

```go
hub, err := link.NewHub(link.HubConfig{
    Listener: tls.NewListener(lis, servingTLS), // bearer tokens: encrypt the link
    Auth: &link.KubeAuthenticator{
        Client:    clientset,
        Audiences: []string{"atlas-link"},
        ServiceAccounts: map[link.PeerKind][]string{
            link.PeerKindNode:       {"simplyblock/csi-node"},
            link.PeerKindController: {"simplyblock/csi-controller"},
        },
    },
    // Only the replica doing the reconciling may hold peers; the rest turn them
    // away so they redial to the leader.
    Accepting: func() bool {
        select {
        case <-mgr.Elected():
            return true
        default:
            return false
        }
    },
})
go hub.Serve(ctx)
```

CSI side, one call for the whole lifecycle — dial, identify, serve, reconnect:

```go
agent, err := link.NewAgent(link.AgentConfig{
    Dial:        link.TLSDialer("simplyblock-operator-link:9443", clientTLS),
    ID:          link.NodePeer(os.Getenv("NODE_NAME")),   // downward API
    InstanceUID: os.Getenv("POD_UID"),                    // supersedes a stale session
    Token:       link.TokenFile("/var/run/secrets/atlas/link/token"),
    Register:     nodeServer.Register,  // see "Serve a node's NVMe state" below
    Capabilities: storagerpc.Capabilities(),
})
go agent.Run(ctx)
```

A peer's identity is never taken from what it says. Every node plugin pod shares
one ServiceAccount, so a token proves DaemonSet membership, not which node sent
it; `KubeAuthenticator` derives the node from the token's bound-pod claims and
refuses a `Hello` that disagrees.

#### Serve a node's NVMe state, and read it from the operator

This is the other half of `storage.Accessor` (see [Node & fabric](#node--fabric)):
the node serves its own, and the operator fills in the same struct with clients
that reach it. Everything written against the local resolvers runs unchanged
against a node elsewhere in the cluster — which is the point of there being one
type and no mixed form. `storage` itself carries no gRPC; the transport is
`storage/storagerpc`.

On the node:

```go
nodeServer, err := storagerpc.NewServer(storage.Local(nvme.SysfsConfig{}))
// nodeServer.Register is the link agent's Register hook, above.
```

In a reconciler:

```go
conn, err := hub.Registry().Conn(link.NodePeer(nodeName))
if errors.Is(err, link.ErrNoSession) {
    // Not a failure: the node is mid-rollout, or leadership just moved.
    // ErrNoSession classifies as Unavailable/retryable, so requeue.
    return ctrl.Result{RequeueAfter: backoff}, nil
}

store := storagerpc.Remote(conn)                // a storage.Accessor
dev, err := store.DeviceByUUID(ctx, lvolUUID)   // errs.ErrNotFound as usual
if err != nil {
    return ctrl.Result{}, err
}
if !dev.Accessible() {                         // derived from fields that crossed
    // attached, but nothing can serve I/O to it
}

// Needs to be current, not as-of-scan-time — costs a round trip:
shared, err := store.HasCoTenants(ctx, dev)
```

The snapshot crosses whole, so everything derived from it is just as true on the
operator: `Accessible`, and the pure filters `nvme.Siblings` / `nvme.CoTenants`.
The re-scanning questions on `Accessor` work too — by asking the same node again.

Two things to keep in mind. Each call is a round trip, so a caller wanting
several answers should `List` once and use the package-level filters
(`nvme.Siblings`, `nvme.CoTenants`, `DeviceSelector.Filter`) over the snapshot;
the `Accessor` methods of the same name are for when being current is the point.
And the `nvmeof` composition helpers must not be assembled across a link:
`WaitForDevice` resolves device symlinks against the filesystem it runs on, so
in the operator it would consult the operator's `/dev`. Those belong on the node,
behind their own RPC.

### Cross-cutting

#### Map errors at the consumer's boundary

Every package wraps the `errs` sentinels, so a CSI service translates once:

```go
func status(err error) error {
    switch {
    case errors.Is(err, errs.ErrNotFound):
        return grpcstatus.Error(codes.NotFound, err.Error())
    case errors.Is(err, errs.ErrAlreadyExists):
        return grpcstatus.Error(codes.AlreadyExists, err.Error())
    case errors.Is(err, errs.ErrNotConnected):
        return grpcstatus.Error(codes.FailedPrecondition, err.Error())
    case errors.Is(err, errs.ErrUnsupported):
        return grpcstatus.Error(codes.InvalidArgument, err.Error())
    }
    return grpcstatus.Error(codes.Internal, err.Error())
}
```

#### Handle optional fields

Generated request bodies and Kubernetes types are full of them. Read them without
one-off nil checks:

```go
target := ptr.To(60)                              // *int for a set-or-omit field
scName := ptr.From(pvc.Spec.StorageClassName, "")  // deref with default, trimmed
replicas := ptr.IntFromOrZero(spec.Replicas)
size := ptr.ClampToInt(sizeBytes, false)           // saturates, never wraps
```

#### Never drop a cleanup error

`deferrers` logs it with the call site that scheduled the defer:

```go
defer deferrers.Close(resp.Body)
defer deferrers.Run(cancelWatch)
```

#### Validate user-supplied outbound URLs

Before the operator sends a request to one (e.g., a Prometheus endpoint from a
CR):

```go
if err := net.ValidateExternalURL(spec.PrometheusURL); err != nil {
    handleError(err) // non-http(s) scheme, unresolvable host, or blocked IP range
}
```

_Today:_ `operator/internal/controller/simplyblockstoragecluster_controller.go`.

### Testing against the seams

Every public API is an interface or accepts a config, so consumer tests need no
kernel, `/sys`, `nvme-cli`, cluster, or control plane:

```go
// Point the whole accessor at a fixture tree.
store := storage.Local(nvme.SysfsConfig{
    SysRoot: "testdata/sys",
    DevRoot: "testdata/dev",
})

// Or compose one from fakes — the resolvers are the seam worth faking, and a
// partial accessor is fine: what is missing reports errs.ErrUnsupported rather
// than answering as an empty node.
store := storage.Accessor{DeviceResolver: &fakeDevices{...}}

// Uncached resolver over a fake clientset — real index/aggregation logic.
resolver := kube.NewLiveResolver(kfake.NewSimpleClientset(pv, pvc, sc))

// Fake the fabric: implement nvmeof.Connector (4 methods) or lvol.Resolver (2).
```

## Design

- **Domain packages are public and read like the problem** (`nvme`,
  `nvmeof`, `lvol`), one cohesive concern each. No `pkg/` prefix.
- **Public APIs are interfaces** (`nvme.SubsystemResolver`/`nvme.DeviceResolver`, `nvmeof.Connector`,
  `lvol.Mapper`) so the operator and CSI driver can unit-test against
  fakes without a kernel, `/sys`, or `nvme-cli` present.
- **One node's storage is one value** (`storage.Accessor`), reached the same way
  whether it is this machine or one across a link. It is a struct, not an
  interface: there is a single implementation and the variation is in what goes
  *in* it, which is also what lets the questions that re-scan live on it instead
  of becoming an obligation on every implementer.
- **Snapshots carry no handles.** `nvme.Device` and friends are immutable scans
  with nothing pointing back at what produced them, so they compare, copy and
  cross a wire as the values they look like. Anything needing a fresh scan is a
  method on the accessor, where the cost of asking is visible.
- **The Linux grunt work hides in `internal/`** (sysfs parsing, command
  execution). It can change freely, and consumers depend on behavior, not
  mechanism.
- **Dependency direction flows one way:** `kube`/`controlplane`/`nvmeof` →
  `lvol` → `nvme`, with `errs`/`nqn`/`ptr`/`net` as leaf utilities. No import
  cycles. `nvmeof` depends on `lvol` only to turn a control-plane `Connection`
  into fabric targets, and the fabric mechanics know nothing about volumes.
- **Kubernetes deps are confined to `kube`.** Only that package imports
  `k8s.io/api` and `k8s.io/client-go`, and importing `nvme`/`nvmeof`/etc.
  pulls no Kubernetes deps. (client-go is already in both the operator and
  CSI driver, so this adds nothing to either consumer's graph.)
- **One shared resolution implementation.** `kube.NewResolver(ResolverConfig)`
  returns an `InformerResolver` that works off any `cache.SharedIndexInformer`,
  whether a standalone client-go `SharedInformerFactory` (CSI driver) or a
  controller-runtime manager cache (operator), so PV/PVC/VolumeAttachment
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
