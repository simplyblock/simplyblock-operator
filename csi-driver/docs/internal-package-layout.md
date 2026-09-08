<!-- A proposal for the CSI driver's Go package layout: what the internal/ tree
     actually contains, why it does not divide along any concern, and the
     package split that fixes it. It lives in csi-driver/docs/ because it
     describes this component's source tree rather than a product behavior. -->

# CSI driver: an `internal/` package layout

## Status

Steps 1 and 2 of *Sequencing* below have landed: the dead code is gone, `pkg/`
is `internal/`, and the module is `github.com/simplyblock/csi-driver`. Steps 3
to 6 are still proposals. The tables and paths below describe the tree as it now
stands.

## Where the tree stands today

Four packages hold every line of non-test production code, and two of them hold
most of it:

| Package               | Files | Non-test LOC | What is in it                                                                                                                                                             |
|-----------------------|-------|--------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `internal/util`       | 8     | 4,675        | Control-plane REST client, NVMe-oF initiator, ANA path reconciliation, fabric repair, the pod guardian, per-volume locks, the CLI config struct, and a `util.go` grab bag |
| `internal/spdk`       | 6     | 3,538        | The three CSI services, driver bootstrap, topology and cluster selection, mkfs and mount, and control-plane error classification                                          |
| `internal/csi-common` | 6     | 649          | A vendored fork of the upstream `csi-common` helpers                                                                                                                      |
| `internal/kubernetes` | 4     | 342          | An informer-backed PV/PVC reader, plus a `volumehandle` subpackage                                                                                                        |
| `internal/csilink`    | 1     | 124          | The link agent that dials the operator                                                                                                                                    |

Two files carry a quarter of the component between them — `controllerserver.go`
at 1,505 lines and `nodeserver.go` at 1,493 — and a third, `initiator.go`, is
1,524. `internal/util` alone is larger than the whole of `operator/internal/csilink`,
`metrics`, `tlsutil`, and `utils` combined.

The problem is not file size on its own. It is that neither large package has a
subject. `internal/util` is not "utilities" in any sense a reader can use: it is the
control plane, the data path, and a Kubernetes controller in one import path,
which is why `internal/spdk` imports it eleven times and why an initiator change and
a guardian change touch the same package. `internal/spdk` is named after a dependency
the driver no longer talks to directly — the SPDK JSON-RPC path is gone, and
what the package now contains is the CSI service surface.

The consequences are concrete:

- **No enforced direction.** `internal/util` imports `internal/kubernetes`, and
  `internal/spdk` imports both. Nothing prevents the control-plane client from
  reaching for a Kubernetes informer, or the initiator from growing a dependency
  on CSI request types; the compiler has no boundary to defend.
- **A lint exclusion inherited from the operator now bites.** `.golangci.yml`
  exempted `internal/*` from `dupl` and `lll`, a rule that matched nothing
  while there was no `internal/` and would have silenced both linters across
  the whole component the moment there was one. The code passes both without
  it, so step 2 deleted the rule.
- **Tests are named after files, not behaviors.** `controllerserver_volume_test.go`,
  `controllerserver_snapshot_test.go`, `controllerserver_placement_test.go`, and
  `controllerserver_provisioner_test.go` are four test files against one
  1,505-line file, which is the shape a package wants to be.
- **The staging path is untestable in isolation.** `stageVolume`, the mkfs and
  mount logic, the filesystem annotation guard, and the XFS option builders are
  methods on `*nodeServer`, so exercising them means constructing a CSI node
  service.

## The proposed layout

`internal/` replaces `pkg/`, and the two large packages divide along the layers
that already exist in the code but are not expressed in it. Four layers, with
imports pointing only downward:

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
    initiator/                  connect, disconnect, and device resolution
    reconnect/                  subsystem reconnect, ANA path reconciliation,
                                and the monitor loop
    fabric/                     defect-driven NVMe-oF repair
    mount/                      mkfs, mount, filesystem probing, XFS options

    ── layer 1: leaves, no CSI and no node knowledge ─────────────────────
    config/                     the parsed command-line configuration
    clusters/                   the cluster secret, and the client factory
    controlplane/               the simplyblock REST client
    kube/                       PV and PVC reads
    volumeid/                   CSI volume and snapshot handle parsing

  e2e/                          unchanged; `internal/` is importable from here
```

`internal/` is importable from anywhere rooted at `csi-driver/`, so `e2e/` keeps
working without a shim.

### Layer 1 — leaves

**`internal/config`** takes `internal/util/config.go` unchanged. The `Config` struct
is command-line state and has nothing to do with the rest of `util`; moving it
also lets `cmd/main.go` bind its flags without importing anything from the data
path. The flag registration itself can move here too, leaving `main.go` as
nothing but `config.Parse()` and `driver.Run()`.

**`internal/controlplane`** takes `internal/util/jsonrpc.go` and `internal/util/nvmf.go`:
`APIClient`, `ClusterClient`, `Connection`, the v2 path builders, `HTTPError`,
and the response DTOs (`LvolResp`, `SnapshotResp`, `StoragePool`,
`LvolConnectResp`, `ReplicationRelationship`, `MasterLvol`). This is the one
package in the current tree that is already internally coherent; it is simply
filed under the wrong name. See *Overlap with atlas-lib* below — this package
should eventually not exist at all.

**`internal/clusters`** takes the secret-shaped half of `initiator.go`:
`ClusterConfig`, `ClustersInfo`, `NewsimplyBlockClient`, `resolvePoolUUID`, and
the `SPDKCSI_SECRET` / `SPDKCSI_API_TOKEN_PATH` resolution. Today, three call
sites parse that secret file independently — `NewsimplyBlockClient`,
`Guardian.loadClusterSecret`, and `controllerserver.ListClusters` — each with
its own `FromEnv("SPDKCSI_SECRET", …)` and its own error handling. One package
gives them one loader and one place to add caching, and `ListClusters` stops
being an exported function on the controller service.

**`internal/kube`** takes `internal/kubernetes` as it stands: `Manager`, the PV and
PVC readers, and the informer indexes.

**`internal/volumeid`** merges `internal/kubernetes/volumehandle` with
`parseVolumeID`, `parseSnapshotID`, `spdkVolume`, and `spdkSnapshot` from
`controllerserver.go`. Handle parsing is used by the controller service, the
node service, and the PV indexes; it is not a Kubernetes concern and does not
belong under a package named for Kubernetes.

### Layer 2 — the node-local data path

**`internal/initiator`** takes the connect and disconnect half of
`initiator.go`: the `SpdkCsiInitiator` interface, `initiatorNVMf`,
`NewSpdkCsiInitiator`, the `/dev/disk/by-id` glob and match helpers
(`namespaceDeviceGlob`, `matchNamespaceDevice`, `waitForDeviceReady`,
`resolveToSameDevice`, `waitForDeviceGone`), `disconnectDevicePath`, and the
`nvme-cli` exec wrappers. Roughly `initiator.go:319–910`.

**`internal/reconnect`** takes the rest: `MonitorConnection`,
`reconnectSubsystems`, `recoverPathsWithANA`, `reconcileOptimizedPath`,
`reconcileNonOptimizedPaths`, `resolveExpectedPathCount`, `filterByANA`,
`missingEndpoints`, and the reachability probes. Roughly
`initiator.go:913–1520`. This is a background reconciliation loop with a circuit
breaker, not a connect primitive, and it is the part of the file that the
migration and failover post-mortems keep coming back to; it deserves its own
package and its own tests.

**`internal/fabric`** takes `nvmerepair.go`. One coupling has to be broken
first: `repairFabric` is currently a method on `initiatorNVMf`, and
`healMonitoredVolume` is called from the monitor loop. Both become functions
taking the NQN, the volume ID, and the connections — which is all they use — so
`fabric` depends on nothing above it and both callers depend on `fabric`.

**`internal/mount`** takes the filesystem half of `nodeserver.go`:
`stageVolume`'s mkfs and mount body, `probeDiskFormat`, `stagedFsType`,
`fsTypeOrDefault`, `stagingMountFlags`, `supportedOnDiskFilesystems`, the XFS
stripe and feature option builders, `checkXFSFormatConfig`, `stagingMountDead`,
`forceUnmountStaging`, `backingBlockDeviceGone`, `createMountPoint`,
`deleteMountPoint`, `MakeFile`, `ensureCleanTargetPath`, `getBlockSizeBytes`,
and `ioctlBlkGetSize64`. That is roughly 550 lines — a third of `nodeserver.go`
— and none of it needs a CSI request. Extracting it is what makes the
format-guard behavior testable without a node service, and it is the largest
single win in this proposal.

The filesystem *annotation guard* (`persistentVolumeClaimForVolume`,
`annotatedFilesystem`, `recordOnDiskFilesystem`) stays in `internal/csi/node`:
it reads and patches PVCs, which is Kubernetes-shaped policy layered on top of
the mount primitives, not a mount primitive itself.

### Layer 3 — Kubernetes-shaped daemons

**`internal/guardian`** takes `guardian.go` whole. It already is a package in
everything but name: 1,319 lines, one type, its own persisted state file, its
own config struct, and its own event vocabulary. It is also the clearest
argument that `internal/util` is not a utility package.

**`internal/csilink`** is `internal/csilink` unchanged, and keeps the name it shares
with `operator/internal/csilink`.

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

### What `internal/util/util.go` becomes

Nothing survives as a shared utility. The file breaks up entirely:

| Symbol                                                                                                                                                | Destination                                                                                              |
|-------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------|
| `StashVolumeContext`, `LookupVolumeContext`, `CleanUpVolumeContext`, `stashContext`, `lookupContext`, `cleanUpContext`, `ConvertInterfaceToMap`       | `internal/csi/node` (only the node service stages a volume context)                                      |
| `ParseJSONFile`, `FromEnv`                                                                                                                            | `internal/clusters` (its only remaining callers read the secret)                                         |
| `ToMiB`, `ToGiB`, `AlignToGiBBytes`, `GIB`                                                                                                            | `internal/csi/controller` (only sizing in `CreateVolume` and `ControllerExpandVolume`)                   |
| `parseDurationFromEnv`                                                                                                                                | wherever its caller lands                                                                                |
| `TryLock`                                                                                                                                             | **delete** — no non-test caller                                                                          |
| `getNvmeDeviceName`, `detectNvmeDeviceName`, `CheckIfNvmeDeviceExists`, `GetNvmeDeviceName`, `GetVirtioBlkDeviceName`, `GetAvailablePhysicalFunction` | **delete** — SPDK vhost and virtio-blk leftovers from the upstream fork, with no caller outside the file |

`VolumeLocks` from `idlocker.go` is used by both CSI services and moves to
`internal/csi/common` alongside the other shared service scaffolding.

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
- **`internal/volumeid` against `atlas/lvol.VolumeHandle`.** `volumehandle.Parse`
  and `lvol.VolumeHandle.Split` parse the same `clusterID:poolID:volumeID`
  string, with two independent UUID regexes.
- **NQN handling against `atlas/nqn`.** `getLvolIDFromNQN`, `hostIDFromHostNQN`,
  and the two `fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", …)` literals in
  `initiator.go` and `nodeserver.go` are `nqn.Parse`, `nqn.HostUUID`, and
  `nqn.Host`.

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
3. **Split `internal/util`:** into `config`, `controlplane`, `clusters`,
   `initiator`, `reconnect`, `fabric`, and `guardian`. This is the largest step
   and the one that ends the grab bag; it is also where the `repairFabric`
   method has to become a function.
4. **Extract `internal/mount`:** from `nodeserver.go`, and `internal/volumeid`
   from `controllerserver.go` and `internal/kubernetes`. Both are extractions of
   pure logic and should come with the tests that were previously impossible.
5. **Rename `internal/spdk` to `internal/csi`:** split it into `common`,
   `identity`, `controller`, `node`, and `driver`, and split the two large
   service files by the tables above. Move the guardian and monitor goroutine
   starts out of `newNodeServer` and into `driver.Run`.
6. **Adopt the atlas-lib primitives:** one package per change, deleting the
   local copy each time.

Step 6 is independent of the restructure and can be deferred indefinitely
without leaving the tree in a half-migrated state.
