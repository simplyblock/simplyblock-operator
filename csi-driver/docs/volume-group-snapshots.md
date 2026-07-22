## Volume Group Snapshots

Kubernetes CSI [VolumeGroupSnapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/#volume-group-snapshots)
allow multiple PVCs to be snapshotted at a consistent point in time. Without
this, snapshotting several volumes one-at-a-time risks capturing them in
different states and producing inconsistent backups.

Simplyblock implements the CSI `GroupController` service, backed by a
cluster-level group snapshot API that creates member snapshots atomically.

---

### Prerequisites

- Kubernetes 1.27+ with the `VolumeGroupSnapshot` feature gate enabled
- [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter) v8.0+ installed (CRDs + snapshot-controller)
- Simplyblock CSI driver v1.x+

Install the VolumeGroupSnapshot CRDs if not already present:

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/main/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshotclasses.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/main/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshotcontents.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/main/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshots.yaml
```

---

### Create a VolumeGroupSnapshotClass

```yaml
apiVersion: groupsnapshot.storage.k8s.io/v1alpha1
kind: VolumeGroupSnapshotClass
metadata:
  name: simplyblock-group-snapclass
driver: csi.simplyblock.io
deletionPolicy: Delete
```

---

### Snapshot multiple PVCs together

```yaml
apiVersion: groupsnapshot.storage.k8s.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: my-group-snapshot
  namespace: default
spec:
  volumeGroupSnapshotClassName: simplyblock-group-snapclass
  source:
    selector:
      matchLabels:
        app: my-database   # all PVCs with this label are snapshotted together
```

The external-snapshotter sidecar discovers matching PVCs, calls the CSI
`CreateVolumeGroupSnapshot` RPC with all their volume IDs, and the driver
sends a single request to the Simplyblock backend. The backend creates one
snapshot per member volume and records them as a group so they share the same
logical creation time.

Check the result:

```sh
kubectl get volumegroupsnapshot my-group-snapshot
kubectl get volumesnapshot -l groupsnapshot.storage.k8s.io/volume-group-snapshot-name=my-group-snapshot
```

---

### Restore from a group snapshot

Each member of a group snapshot is also a standard `VolumeSnapshot` and can
be used to provision a new PVC independently:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: restored-pvc
spec:
  storageClassName: simplyblock-sc
  dataSource:
    name: <member-volume-snapshot-name>
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

---

### Architecture

```
external-snapshotter (sidecar)
        │  CreateVolumeGroupSnapshot RPC
        ▼
CSI GroupController (groupcontrollerserver.go)
        │  POST /api/v2/clusters/{id}/snapshot-groups/
        ▼
Simplyblock control plane (sbcli)
        │  creates individual snapshots atomically
        ▼
Storage nodes (SPDK)
```

**Group snapshot ID format:** `{clusterID}:{groupSnapshotUUID}`

Each member snapshot has the standard CSI snapshot ID format:
`{clusterID}:{poolID}:{snapshotUUID}`

---

### Implementation notes

| File | Role |
|---|---|
| `pkg/spdk/groupcontrollerserver.go` | `GroupControllerServer` — Create, Delete, Get, GetCapabilities |
| `pkg/util/nvmf.go` | `ClusterClient` wrappers for group snapshot backend calls |
| `pkg/util/jsonrpc.go` | `GroupSnapshotResp` type, `APIClient` HTTP methods, URL helpers |
| `pkg/csi-common/server.go` | Registers `GroupControllerServer` with the gRPC server |
| `pkg/spdk/driver.go` | Instantiates `groupControllerServer` alongside the controller server |

**Backend requirement:** The Simplyblock control plane must expose the
`/api/v2/clusters/{id}/snapshot-groups/` endpoint (POST / GET / DELETE).
Without it the `CreateVolumeGroupSnapshot` call will fail with an HTTP error
from the backend. See issue [#302](https://github.com/simplyblock-io/simplyblock-manager/issues/302)
for the backend implementation plan.

**Locking:** Member volumes are locked in sorted order before the backend call
to prevent deadlocks when two concurrent `CreateVolumeGroupSnapshot` requests
share overlapping volume sets.

**Idempotency:** A `409 Conflict` from the backend triggers a reconciliation
pass — the driver lists existing group snapshots, finds the matching name, and
returns it if the source volumes are the same. If the name conflicts with a
different set of volumes, `AlreadyExists` is returned to the sidecar.
