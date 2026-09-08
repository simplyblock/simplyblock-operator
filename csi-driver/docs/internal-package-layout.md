<!-- A proposal for the CSI driver's Go package layout: what the internal/ tree
     actually contains, why it does not divide along any concern, and the
     package split that fixes it. It lives in csi-driver/docs/ because it
     describes this component's source tree rather than a product behavior. -->

# CSI driver: an `internal/` package layout

## Status

Steps 1 to 5 of *Sequencing* below have landed. The restructure is done: the
dead code is gone, `pkg/` is `internal/`, the module is
`github.com/simplyblock/csi-driver`, and both `util` and `spdk` have been
dissolved into the layered packages proposed here. Step 6, adopting the
atlas-lib primitives, is partly done and partly still a proposal.

## Where the tree stands today

Neither `internal/util` nor `internal/spdk` exists any more. No file is over
1,325 lines, and the largest package is the one that holds the CSI controller
service, in twelve files:

| Package                   | Files | Non-test LOC | What is in it                                                                                                                                                     |
|---------------------------|-------|--------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `internal/csi/controller` | 12    | 2,016        | The controller service, by RPC group: placement, volume, snapshot, clone, expand, inspect, plus its parameters, PVC annotations, sizing, and error classification |
| `internal/csi/node`       | 9     | 1,415        | The node service, by RPC group: stage, publish, expand, stats, topology, capabilities, plus the filesystem guard and the staged volume context                    |
| `internal/guardian`       | 1     | 1,325        | The coordinated pod-restart guardian                                                                                                                              |
| `internal/controlplane`   | 3     | 1,232        | The simplyblock v2 REST client, its response types, and the node-side queries                                                                                     |
| `internal/initiator`      | 2     | 843          | Connect, disconnect, device resolution, the nvme-cli primitives, and the device-presence record                                                                   |
| `internal/csi/common`     | 8     | 751          | The vendored upstream helpers, `VolumeLocks`, and the keys and handle both services share                                                                         |
| `internal/reconnect`      | 1     | 571          | The monitor loop, ANA path reconciliation, and subsystem reconnect                                                                                                |
| `internal/mount`          | 1     | 469          | Reading a device, mkfs and mount, and the lifecycle of the path it mounts on                                                                                      |
| `internal/fabric`         | 1     | 460          | Defect-driven NVMe-oF repair                                                                                                                                      |
| `internal/kubernetes`     | 3     | 284          | The informer-backed PV and PVC reader                                                                                                                             |
| `internal/driver`         | 1     | 219          | `Run`: the services, the background loops, the operator link, and the gRPC server                                                                                 |
| `internal/clusters`       | 1     | 189          | The cluster secret, and the control-plane client factory                                                                                                          |
| `internal/csilink`        | 1     | 124          | The link agent that dials the operator                                                                                                                            |
| `internal/csi/identity`   | 1     | 73           | The identity service                                                                                                                                              |
| `internal/config`         | 1     | 54           | The parsed command-line configuration                                                                                                                             |

The layering is the compiler's to enforce, and it holds. `config`, `mount`,
`kubernetes`, `controlplane`, `csilink`, and `csi/common` import nothing of this
module's. `clusters` and `fabric` import only `controlplane`. `initiator` and
`reconnect` import only layers beneath them, `reconnect` above `initiator`.
`guardian` sits beside them. The three CSI services import the layers they need
and never each other, sharing only `csi/common`. `driver` is the one package
reaching across everything, which is what an assembly package is for, and
nothing imports it but `main`.

The name `spdk` is gone with the package. It named a dependency the driver
stopped talking to directly long before this work started — the SPDK JSON-RPC
path no longer exists — and what it actually held was the CSI service surface,
which is now called that.

What the split has already bought:

- **Direction the compiler defends.** The control-plane client can no longer
  reach for a Kubernetes informer, and the initiator cannot grow a dependency on
  CSI request types, because neither import would compile.
- **Per-package coverage.** Two blurred numbers became thirteen: `volumehandle`
  100%, `kubernetes` 90%, `fabric` 76%, `clusters` 66%, `spdk` 43%, `initiator`
  32%, `controlplane` 32%, `guardian` 29%, `reconnect` 13%. The last of those is
  the honest measure of how little of the repair loop is tested, which the old
  `util` package's 34% hid.
- **One reader of the cluster secret.** `clusters` owns the file; the three call
  sites that each parsed it independently now share a loader.

- **Tests that name their subject.** `controllerserver_volume_test.go` and its
  three siblings were four test files against one 1,492-line file — the shape a
  package wants to be, which is what they became: `volume_test.go`,
  `snapshot_test.go`, `placement_test.go`, and `provisioner_test.go` in
  `csi/controller`, beside the files they test.
- **Constructing a service no longer starts a daemon.** `newNodeServer` used to
  build a Kubernetes cache manager, start the guardian, and launch the
  connection monitor as a side effect, so any test wanting a node service got a
  poll loop against a live control plane with it. `driver` starts those now.

## The layout

The two large packages divide along the layers that already existed in the code
but were not expressed in it. Four layers, with imports pointing only downward.
Everything below layer 4 is built; layer 4 is what steps 4 and 5 still owe:

```text
csi-driver/
  cmd/
    main.go                     flags, driver name and version, klog setup

  internal/
    ── layer 4: the CSI surface ──────────────────────────────────────────
    driver/                     Run(): builds the kube client, the servers,
                                the link, and the gRPC server
    csi/
      common/                   the vendored csi-common helpers
      identity/                 the identity service
      controller/               the controller service
      node/                     the node service

    ── layer 3: Kubernetes-shaped daemons ────────────────────────────────
    guardian/                   the coordinated pod-restart guardian
    csilink/                    the link agent that dials the operator

    ── layer 2: the node-local data path ─────────────────────────────────
    initiator/                  connect, disconnect, device resolution, the
                                nvme-cli primitives, device presence
    reconnect/                  subsystem reconnect, ANA path reconciliation,
                                and the monitor loop
    fabric/                     defect-driven NVMe-oF repair
    mount/                      mkfs, mount, filesystem probing, XFS options

    ── layer 1: leaves, no CSI and no node knowledge ─────────────────────
    config/                     the parsed command-line configuration
    clusters/                   the cluster secret, and the client factory
    controlplane/               the simplyblock REST client
    kubernetes/                 PV and PVC reads
    volumeid/                   CSI volume and snapshot handle parsing

  e2e/                          unchanged; `internal/` is importable from here
```

`internal/` is importable from anywhere rooted at `csi-driver/`, so `e2e/` keeps
working without a shim.

### Layer 1 — leaves

**`internal/config`** took `util/config.go` unchanged, minus the RPC timeout
constant, which was never configuration and moved to `controlplane` where its
only reader is. The flag registration can still move here, leaving `main.go` as
nothing but `config.Parse()` and `driver.Run()`.

**NQN parsing has no package here at all.** The split briefly gave it one:
`LvolIDFromNQN` and `HostIDFromHostNQN` are called by three layers on strings
from three sources — the control plane's connect response, the kernel's
`list-subsys` output, and a Kubernetes node UID — so filing them under any one
layer would have made the other two import it upward. But atlas-lib's `nqn`
already exported both, which makes this an adoption rather than a placement
question, and the local package was deleted the same day it was written. See
*Overlap with atlas-lib* below.

**`internal/controlplane`** took `util/jsonrpc.go` and `util/nvmf.go`:
`APIClient`, `ClusterClient`, `Connection`, the v2 path builders, `HTTPError`,
and the response types (`LvolResp`, `SnapshotResp`, `StoragePool`,
`LvolConnectResp`, `ReplicationRelationship`, `MasterLvol`). It was the one
package already internally coherent; it was simply filed under the wrong name.

It also gained a `queries.go`. Three callers used to reach through
`client.API.do` at a hand-built path — the connection monitor for a volume's
node placement, the initiator for its endpoints, the guardian for cluster status
— which compiled only while all of them shared one package file. Each is now a
method that names the question it asks, so the transport stays unexported. See
*Overlap with atlas-lib* below: this package should eventually not exist at all.

**`internal/clusters`** took the secret-shaped half of `initiator.go`:
`ClusterConfig` and `ClustersInfo` (now `Config` and `Info`),
`NewsimplyBlockClient` (now `Client`), `resolvePoolUUID`, and the
`SPDKCSI_SECRET` / `SPDKCSI_API_TOKEN_PATH` resolution. Three call sites parsed
that file independently — the client factory, `Guardian.loadClusterSecret`, and
`controllerserver.ListClusters` — each with its own environment default and its
own error handling. They share `clusters.Load` now, `ListClusters` became
`clusters.List` and stopped being an exported function on the controller
service, and there is one place to add caching.

**`internal/kubernetes`** stays as it is: `Manager`, the PV and PVC readers, and
the informer indexes. Renaming it to `kube` buys nothing on its own and is left
to step 6, which replaces it with `atlas/kube.InformerResolver` outright.

**`internal/volumeid` was not created, and should not be.** The plan was to
merge `internal/kubernetes/volumehandle` with `parseVolumeID`, `parseSnapshotID`,
`spdkVolume`, and `spdkSnapshot`, on the grounds that handle parsing is not a
Kubernetes concern. That was right, and it was answered better: the parsing went
to `atlas/lvol.ParseHandle`, `volumehandle` was deleted, and `spdkVolume` turned
out to be `lvol.Handle` with three fields renamed, so it went too — its
`parseVolumeID` adapter is now a four-line `parseVolumeHandle` that wraps the
atlas call in a CSI-shaped error.

What is left is `spdkSnapshot` and `parseSnapshotID`, and they cannot join it: a
snapshot id has a legacy two-part form, `{clusterID}:{snapshotID}`, that
`ParseHandle` rightly rejects. One type and one function, read by one service,
is not a package; step 5 files them under `internal/csi/controller`.

### Layer 2 — the node-local data path

**`internal/initiator`** took the connect and disconnect half of the old
`initiator.go`: the interface (now `Initiator`), `initiatorNVMf`, the
constructor (now `New`), the `/dev/disk/by-id` glob and match helpers, and the
`nvme-cli` wrappers. The types and primitives the monitor also needs are
exported — `Path`, `Subsystem`, `SubsystemResponse`, `DeviceInfo`,
`NVMeDevices`, `SubsystemsForDevice`, `ConnectViaNVMe`, `ParseAddress` — rather
than copied, which is the whole reason `reconnect` sits above it rather than
beside it.

The device-presence record moved with them, into `presence.go`, behind
`MarkDevicePresent`, `ForgetDevice`, and `PruneMissingDevices`. It was three
package-level maps and a mutex that both halves wrote to directly; that only
worked while they shared a package. It is also the one place that can tell a
volume lost every path, since the kernel removes the device and reports nothing.

**`internal/reconnect`** took the rest: `MonitorConnection`,
`reconnectSubsystems`, `recoverPathsWithANA`, `reconcileOptimizedPath`,
`reconcileNonOptimizedPaths`, `resolveExpectedPathCount`, `filterByANA`,
`missingEndpoints`, `NodeHostNQN`, and the reachability probes. This is a
background reconciliation loop with a circuit breaker, not a connect primitive,
and it is the part of the old file the migration and failover post-mortems keep
coming back to. Its coverage now reads separately, at 13%.

**`internal/fabric`** took `nvmerepair.go`. One coupling had to be broken first:
`repairFabric` was a method on `initiatorNVMf` and `healMonitoredVolume` was
called from the monitor loop. Both are functions now — `RepairAttach` and
`HealMonitoredVolume` — taking the NQN, the volume ID, and the connections:
exactly what
they already used, so `fabric` depends on nothing above it and both callers
depend on `fabric`.

**`internal/mount`** took the filesystem half of `nodeserver.go`, 469 lines of
it: the blkid probe, mkfs and mount, filesystem resize, the XFS stripe and
feature option builders, the supported-filesystem set, the `nouuid` rule, the
ext4 reserved-block adjustment, dead-mount detection, force unmount, and the
whole lifecycle of the directory or file a volume is mounted on. `nodeserver.go`
went from 1,497 lines to 1,110.

The seam is what needs a CSI request to answer. Nothing in `mount` does: whether
a device reads blank, which options mkfs takes for a filesystem, whether a mount
has gone dead, and whether a path is safe to mount over are all questions about
the node. It holds the two injectable seams those need — a mount interface and a
command runner — so its tests script commands and never touch a kernel.

`stageVolume` stays in the node service, because it is the *decision* rather
than the operation: which filesystem the volume is supposed to carry, and what
to do when the device disagrees. So do `fsTypeOrDefault`, `stagedFsType`, and
`stagingMountFlags`, which read a `csi.VolumeCapability`; the last of them calls
`mount.FlagsFor` for the one rule that is the filesystem's rather than the
volume's.

The extraction also removed the node service's `execer` field. It existed to let
a test script the probe and observe which commands staging ran; that is now the
mounter's seam, and the node service holds only the mounter.

The filesystem *annotation guard* (`persistentVolumeClaimForVolume`,
`annotatedFilesystem`, `recordOnDiskFilesystem`) stays in `internal/csi/node`:
it reads and patches PVCs, which is Kubernetes-shaped policy layered on top of
the mount primitives, not a mount primitive itself.

### Layer 3 — Kubernetes-shaped daemons

**`internal/guardian`** took `guardian.go` whole. It already was a package in
everything but name: 1,325 lines, one type, its own persisted state file, its
own config struct, and its own event vocabulary. It was also the clearest
argument that `util` was not a utility package. `GuardianConfig`,
`StartGuardian`, and `NewDefaultGuardianConfig` lost the prefix the package name
now carries.

**`internal/csilink`** is unchanged, and keeps the name it shares with
`operator/internal/csilink`.

### Layer 4 — the CSI surface

**`internal/csi/common`** is `internal/csi-common`, renamed so the directory and the
package name agree (`csi-common` on disk, `csicommon` in Go, today).

**`internal/csi/identity`** is `identityserver.go`.

**`internal/csi/controller`** is `controllerserver.go` split into files by RPC
group and concern, which the tests already anticipate:

| File            | Contents                                                                                            | Current lines      |
|-----------------|-----------------------------------------------------------------------------------------------------|--------------------|
| `server.go`     | the `controllerServer` type and its constructor                                                     | 125–150, 1330–1338 |
| `params.go`     | storage-class parameter names, `parseStringMap`, the DHCHAP node segment                            | 46–123, 236–263    |
| `placement.go`  | `resolveClusterSelection` and every topology, zone, and region helper                               | 152–234, 265–432   |
| `volume.go`     | `CreateVolume`, `DeleteVolume`, `prepareCreateVolumeReq`, `createVolume`, `reconcileExistingVolume` | 434–589, 791–1005  |
| `snapshot.go`   | `CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots`, pagination, `reconcileExistingSnapshot`        | 639–790, 1126–1216 |
| `clone.go`      | `handleVolumeContentSource`, `handleSnapshotSource`, `handleVolumeSource`                           | 1339–1486          |
| `expand.go`     | `ControllerExpandVolume`                                                                            | 1084–1125          |
| `inspect.go`    | `ValidateVolumeCapabilities`, `ControllerGetVolume`                                                 | 590–638, 1277–1329 |
| `pvc.go`        | `fetchPVCAnnotations`, `removePVCAnnotations`, `pvcAnnotation`                                      | 1487–1545          |
| `errorclass.go` | `errorclass.go` and `errorclass_rpc.go`, moved as-is                                                | —                  |

`placement.go` is worth calling out: `resolveClusterSelection` plus the topology
helpers are ~250 lines of pure functions over `csi.Topology` with no I/O at all,
and they already have a dedicated test file.

**`internal/csi/node`** is `nodeserver.go` split the same way, once `mount` has
taken the filesystem work:

| File          | Contents                                                                                                        | Current lines                 |
|---------------|-----------------------------------------------------------------------------------------------------------------|-------------------------------|
| `server.go`   | the `nodeServer` type and its constructor                                                                       | 56–107                        |
| `topology.go` | `NodeGetInfo`, `buildAccessibleTopology`                                                                        | 109–181                       |
| `stage.go`    | `NodeStageVolume`, `NodeUnstageVolume`, `restageVolume`, `isStaged`                                             | 309–452, 1247–1312            |
| `publish.go`  | `NodePublishVolume`, `NodeUnpublishVolume`, `publishVolume`, `healVolumeBeforePublish`, `ensureDeviceConnected` | 454–509, 1188–1246, 1314–1364 |
| `expand.go`   | `NodeExpandVolume`                                                                                              | 548–610                       |
| `stats.go`    | `NodeGetVolumeStats`, `redirectToActiveVolume`                                                                  | 183–307                       |
| `fsguard.go`  | the PVC filesystem annotation guard                                                                             | 967–1090                      |
| `context.go`  | the staged volume-context stash (from `util.go`)                                                                | —                             |

The node service constructor currently starts the guardian and the monitor loop
as a side effect of `newNodeServer`. Those two goroutines should be started by
`internal/driver` instead, next to the link agent, so that constructing a node
service does not launch two background daemons.

**`internal/driver`** is `driver.go`: `Run`, `startLink`, the shared Kubernetes
client, and the capability lists. It is the only package that imports across
layers, which is exactly what an assembly package is for.

### What `util/util.go` became

Nothing survived as a shared utility. The file broke up entirely:

| Symbol                                                                                                                                                | Destination                                                                                                                                                   |
|-------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `StashVolumeContext`, `LookupVolumeContext`, `CleanUpVolumeContext`, `stashContext`, `lookupContext`, `cleanUpContext`, `ConvertInterfaceToMap`       | `internal/spdk/volumecontext.go`, unexported — only the node service stages a volume context, and step 5 carries the file into `internal/csi/node`            |
| `ParseJSONFile`, `FromEnv`                                                                                                                            | `internal/clusters` (its only remaining callers read the secret)                                                                                              |
| `ToGiB`, `AlignToGiBBytes`, `GIB`, `MIB`                                                                                                              | `internal/spdk/volumecontext.go`, unexported — only sizing in `CreateVolume` and `ControllerExpandVolume`; step 5 carries them into `internal/csi/controller` |
| `parseDurationFromEnv`                                                                                                                                | `internal/guardian` — its only caller                                                                                                                         |
| `TryLock`, `ToMiB`                                                                                                                                    | **deleted** — no non-test caller                                                                                                                              |
| `getNvmeDeviceName`, `detectNvmeDeviceName`, `CheckIfNvmeDeviceExists`, `GetNvmeDeviceName`, `GetVirtioBlkDeviceName`, `GetAvailablePhysicalFunction` | **deleted** in step 1 — SPDK vhost and virtio-blk leftovers from the upstream fork, with no caller outside the file                                           |

`VolumeLocks` from `idlocker.go` is used by both CSI services and moved to
`internal/csi-common` alongside the other shared service scaffolding, which
step 5 renames to `internal/csi/common`.

## Overlap with atlas-lib

`AGENTS.md` puts node-level and control-plane primitives in `atlas-lib`, and
several of the packages above are second implementations of ones that already
live there. The restructure should not be blocked on resolving these, but the
new package boundaries are what make the swaps mechanical afterward, and each
one deletes a package rather than moving it:

- **`internal/controlplane` against `atlas/controlplane`.** The atlas client
  already covers volumes, pools, storage nodes, connections, resize, delete, and
  clone against the same v2 API, with generated types. What it does not cover is
  snapshots and publish/unpublish. `operator/internal/webapi` is being retired
  the same way, and this is the same debt in the other consumer.
- **`internal/kube` against `atlas/kube.InformerResolver`.** `Manager` and
  `InformerResolver` are the same object: informer-backed PV and PVC reads with
  a direct-read fallback, indexed by CSI volume handle. Two implementations of
  one cache is exactly the drift the house rule exists to prevent.
- **Volume handle parsing against `atlas/lvol` — done for this component.**
  `atlas/lvol.ParseHandle` now decomposes a handle the way the format actually
  works: cluster and volume are canonical UUIDs, the pool segment is carried as
  written because it may be a name, and surrounding whitespace is trimmed.
  `internal/kubernetes/volumehandle` is deleted and its six call sites, `e2e`
  included, call `lvol.ParseHandle` and `lvol.IsCanonicalUUID`. `Split` remains
  for callers that genuinely want three typed UUIDs.

  **The operator has not adopted it, and should not yet.** It parses the same
  string in three more shapes — `splitVolumeHandle`,
  `parseSimplyblockVolumeHandle`, and eight bare
  `strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)` call sites — none of which
  validate anything beyond segment count and non-emptiness. Adopting
  `ParseHandle` there compiles and is arguably a bug fix, since none of those
  loops filters by CSI driver and a foreign handle carrying two colons is
  currently mistaken for a simplyblock one. But it fails roughly seventeen of
  the operator's unit tests, because 35 of its test fixtures spell a cluster or
  volume id as a readable placeholder (`cluster-uuid-policy-add`,
  `lvol-switch`) rather than as a UUID. That is a suite-wide convention, not a
  handful of sloppy fixtures, so tightening the operator's handle validation is
  a decision with its own blast radius and belongs in its own change — one that
  either reworks those fixtures or concludes the operator should keep parsing
  permissively.

- **NQN handling against `atlas/nqn` — done.** `getLvolIDFromNQN` and
  `hostIDFromHostNQN` are `nqn.Parse` and `nqn.HostUUID`, and the three
  hand-spelled `nqn.2014-08.io.simplyblock:uuid:<uid>` literals in the node
  service, the reconnect loop, and the DHCHAP end-to-end test are `nqn.Host`.
  Adopting `HostUUID` tightened one edge deliberately: it requires the `:uuid:`
  marker and a well-formed UUID, where the local copy took whatever followed the
  last colon. No caller can reach the difference, since every host NQN this
  driver sees comes from `nqn.Host(node.UID)`, and a `--hostid` that is not a
  UUID could only ever have failed the connect. A test pins it.

## Module path

The module is `github.com/simplyblock/csi-driver`. It was renamed in step 2,
because step 2 rewrites every import line in the component anyway and doing it
later would cost that rewrite a second time. The old path,
`github.com/spdk/spdk-csi`, named an upstream the driver forked from and no
longer resembles, and it is the reason the package holding the CSI services is
still called `spdk`. The `replace` directive wiring the module to `../atlas-lib`
was unaffected, and so were the Gerrit project references in
`scripts/ci/spdkcsi-ci.yaml`, which name the upstream project rather than this
module.

## Sequencing

Each step compiles and passes the suite on its own, and each is reviewable
without the next:

1. **Delete the dead code (done):** the virtio-blk and vhost helpers, `TryLock`,
   and the commented-out `ListVolumes` and `GetCapacity` bodies in
   `controllerserver.go`. Nothing that is about to be moved should be moved
   twice, and this is the cheapest reduction in the tree. 191 lines.
2. **`git mv pkg internal` (done):** the import paths and the module path
   rewritten in the same change. Pure mechanics, no package boundaries moved.
   `Makefile`'s `SOURCE_DIRS := cmd pkg` became `cmd internal`, the Dockerfile's
   `COPY csi-driver/pkg/` became `COPY csi-driver/internal/`, the `internal/*`
   lint exclusion was deleted rather than allowed to take effect, and the
   `csi-driver/pkg/...` pointers in `atlas-lib/README.md`, the operator's design
   documents and test plans, and two operator source comments were repointed.
3. **Split `internal/util` (done):** into `config`, `controlplane`, `clusters`,
   `initiator`, `reconnect`, `fabric`, and `guardian`. The package is gone. `repairFabric` and
   `healMonitoredVolume` became functions, the device-presence maps became an
   API, three raw `client.API.do` call sites became named control-plane methods,
   and `volumehandle.IsUUID` replaced two hand-rolled UUID predicates. NQN
   parsing briefly became a local leaf and was then adopted from `atlas/nqn`
   instead, which is where it belonged.
4. **Extract `internal/mount` (done):** 469 lines out of `nodeserver.go`, which
   drops from 1,497 to 1,110, with the tests that were previously impossible —
   the probe cases moved out of the node service, and the mkfs option builders
   gained the first tests they have ever had. `internal/volumeid` was not
   created: `atlas/lvol` absorbed the handle parsing instead, and what remained
   was one type and one function belonging to one service.
5. **Rename `internal/spdk` to `internal/csi` (done):** split into
   `csi/common`, `csi/identity`, `csi/controller`, `csi/node`, and a top-level
   `driver`, with the two large service files split by the tables above. The
   guardian and monitor goroutine starts left `newNodeServer` for `driver`. The
   keys the two services share — the claim keys, the cluster parameter, and the
   four topology keys the node advertises and the controller matches on — moved
   to `csi/common`, where a contract between two services can only be spelled
   once.
6. **Adopt the atlas-lib primitives:** one package per change, deleting the
   local copy each time.

Step 6 is independent of the restructure and can be deferred indefinitely
without leaving the tree in a half-migrated state.
