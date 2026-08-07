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
│   └── migrations.go       volume migrations: create / get / continue / cancel
├── link/                   gRPC operator ↔ CSI, over connections the CSI driver opens
│   ├── doc.go              why the connection runs backwards, and how gRPC still works on it
│   ├── session.go          Session: yamux-multiplexed link, gRPC server + client on both ends
│   ├── hub.go              Hub: operator side — accepts links, serves the handshake
│   ├── agent.go            Agent: CSI side — dials, identifies, serves, reconnects
│   ├── registry.go         Registry: who is linked right now (ErrNoSession when not)
│   ├── peer.go             PeerKind/PeerID/Claim/Identity/Peer
│   ├── auth.go             Authenticator iface, bearer-token plumbing, TokenSource
│   ├── kubeauth.go         KubeAuthenticator: TokenReview → the pod's node, not the peer's word
│   ├── dial.go             TLSDialer / InsecureDialer
│   └── linkv1/             the Hello handshake protocol (buf-generated, committed)
├── storage/                one node's storage as one value — no gRPC in here
│   ├── accessor.go         Accessor: every lookup flat, plus the questions that re-scan
│   │                       Local(sysfs) fills one in; storagerpc.Remote fills the same struct
│   └── storagerpc/         the same Accessor, served over a link and reached over one
│       ├── server.go       NewServer(storage.Accessor); Register is the link agent's hook
│       ├── client.go       Remote(conn) + the nvme resolver interfaces, remoted
│       ├── convert.go      nvme snapshot types ↔ wire form (total, both directions)
│       └── storagev1/      SubsystemService + DeviceService (buf-generated, committed)
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
shows the atlas-idiomatic implementation; a _Today_ note points at the live call
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
lifecycle. `DeleteVolume` is idempotent — an already-absent volume is not an
error — so a retried `DeleteVolume` RPC needs no pre-check.

_Today:_ `csi-driver/pkg/spdk/controllerserver.go` uses the `kube` param helpers
but still calls the control plane through its own `pkg/util/nvmf.go` client.

#### Migrate a volume to another storage node

The operator's `VolumeMigration` reconciler. A migration is created, observed by
phase, then either continued past its pre-created checkpoint or cancelled.

```go
migration, err := client.CreateVolumeMigration(ctx, handle, targetNodeID)
if err != nil {
    handleError(err)
}

// Poll until the control plane parks the migration at its pre-created
// checkpoint, then validate the new paths before committing to the cutover.
for {
    m, err := client.GetVolumeMigration(ctx, handle, migration.ID)
    if err != nil {
        handleError(err)
    }
    log.Info("migration", "phase", m.Phase,
        "snapshots", fmt.Sprintf("%d/%d", m.SnapsMigrated, m.SnapsTotal))
    if m.Phase == "pre_created" { // atlas keeps Phase a plain control-plane string
        break
    }
    // ... sleep / requeue
}

if err := validateTargetPaths(ctx); err != nil {
    // Roll back rather than cut over to paths the consumer cannot reach.
    _ = client.CancelVolumeMigration(ctx, handle, migration.ID)
    handleError(err)
}
if err := client.ContinueVolumeMigration(ctx, handle, migration.ID); err != nil {
    handleError(err)
}
```

_Today:_ `operator/internal/controller/volumemigration_controller.go` runs
exactly this sequence against the operator's own `internal/webapi` client.

#### Choose or validate a placement target

The volume-placement webhook picks the least-loaded node at creation time; the
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
lookup; pick the implementation that matches the consumer's caching, and keep the
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
(`logical_volume_selector.go`, `persistentvolumeclaim_controller.go`);
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
consumers — one unreadable class must not stall a migration.

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

Everything in this section goes through `storage.Accessor` — one node's storage
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
returns — this is the usual way:

```go
dev, err := store.DeviceByUUID(ctx, lvolUUID)
sub, err := store.SubsystemByNQN(ctx, nqn)
all, err := store.ListDevices(ctx)
```

Or reach a resolver through its field, which is what you do when something else
wants the interface — `nvmeof.ConnectMultipathDevice(ctx, c, store.DeviceResolver, …)`
below is the example:

```go
store.DeviceResolver     // nvme.DeviceResolver
store.SubsystemResolver  // nvme.SubsystemResolver
```

#### Attach a volume

The CSI node service's `NodeStageVolume`. Outside a single-node installation the
control plane answers with several endpoints in descending priority (primary,
secondary, tertiary), and attaching means
establishing *all* of them, in that order: a single-path attach leaves the
volume one node failure away from losing I/O, and connecting out of order hands
I/O to the wrong node until the kernel has the full ANA picture. So the flow is
always "ask the control plane where the volume lives → build a target per path →
connect them in order → wait for the block device".

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

Connecting is idempotent per path — a controller already fronting an endpoint is
left alone rather than duplicated — so a retried NodeStage re-establishes only
what is missing. `nvmeof.ConnectDevice` is the single-path form for volumes that
genuinely have one; `ConnectPaths` and `WaitForDevice` are the two halves, for a
caller that has to interleave something between them.

Once attached, `nvme.DeviceSelector` addresses the volume in any later lookup,
with `ListDevicesBySelector` when a caller wants to see *every* match rather
than the first:

```go
sel := nvme.DeviceSelector{NQN: conn.NQN, UUID: volumeID.String()}
matched, err := store.ListDevicesBySelector(ctx, sel)
```

_Today:_ the CSI node service still connects, repairs, and ANA-reconciles paths
with nvme-cli in `csi-driver/pkg/util/initiator.go`; `FabricsConnector` is the
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

The gate is `nvme.Device.IsMultiNamespace` — *can* this subsystem hold other
volumes — not whether it currently does. Enumerating the neighbours describes
only the moment it was looked at: a namespace can join a shared subsystem
between the check and the disconnect, and then a correct "none right now" answer
is still destructive. So a subsystem provisioned to be shared is never
disconnected on one volume's behalf, even while it happens to hold only that one.

That answer sometimes needs an Identify Controller command, which wants a live
controller and Linux. `DetachDevice` returns the error rather than assuming, so
reaping a subsystem whose controllers are all dead is an explicit
`connector.Disconnect` — never a default.

`Disconnect` takes down every path of the subsystem, releasing them in ANA order
— unusable and non-optimized legs first, the optimized one last — so I/O still in
flight keeps the best path it has until the end.

Every one of these questions also has a pure form taking a snapshot the caller
owns, which is the cheap way to sweep many devices — one `List` answers all four
for all of them:

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
attached, e.g. before deciding to clean up.

#### Find the other devices of one volume

When native NVMe multipath is off, a volume can surface as several block devices
sharing its namespace UUID, and a teardown has to release every one of them:

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

Where no StorageClass is in reach. Conclusive sysfs cases cost nothing; only a
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

Generated request bodies and Kubernetes types are full of them; read them without
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

Before the operator forwards one (e.g. a Prometheus endpoint from a CR):

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
  execution). It can change freely; consumers depend on behavior, not
  mechanism.
- **Dependency direction flows one way**: `kube`/`controlplane`/`nvmeof` →
  `lvol` → `nvme`, with `errs`/`nqn`/`ptr`/`net` as leaf utilities. No import
  cycles. `nvmeof` depends on `lvol` only to turn a control-plane `Connection`
  into fabric targets; the fabric mechanics know nothing about volumes.
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
