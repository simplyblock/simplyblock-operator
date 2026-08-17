# Design Document: DHCHAP Allowed-Node Scheduling

**Status:** Implemented

**Author:** Manohar Reddy &nbsp;·&nbsp; **Date:** 2026-08-13

**Related PR:** [#417](https://github.com/simplyblock/simplyblock-operator/pull/417)

---

## Overview

A DHCHAP-enabled `StoragePool` restricts a pool's volumes to a specific set
of Kubernetes worker nodes (`Spec.AllowedNodes`) — only hosts on that list
are registered in the pool's `allowed_hosts` on the control plane, so only
those nodes' NVMe-oF connect attempts are authenticated successfully.

Two independent mechanisms enforce this at the Kubernetes level, one for the
volume's *first* scheduling decision and one for every decision after that:

1. **`StorageClass.AllowedTopologies`** (native Kubernetes field) — read
   directly by the scheduler's `VolumeBinding` plugin against live `Node`
   labels. Governs which node an unbound PVC's first consuming Pod can land
   on.
2. **`PersistentVolume.spec.nodeAffinity`** — set once by `CreateVolume`,
   built directly from the pool's `dhchap_node_label` StorageClass parameter.
   Governs every later Pod scheduling decision that reuses the already-bound
   PV (recreate, restart, drain).

Neither depends on the CSI driver's `NodeGetInfo`/`CSINode` topology-key
registration — that dependency (in an earlier version of this fix) required
restarting the csi-node DaemonSet pod on every `AllowedNodes` change, which
was node-wide disruptive. The current design needs no such restart.

---

## Example: `StoragePool`

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StoragePool
metadata:
  name: pool-a
  namespace: simplyblock
spec:
  clusterName: cluster-a
  dhchap: true
  allowedNodes:
    - worker-1
    - worker-2
```

Reconciling this:

- `syncNodeLabels` patches `worker-1` and `worker-2` with
  `simplyblock.io/pool.<namespace/>.cluster-a.pool-a: allowed`.
- `syncStoragePoolHosts` registers each allowed node's NVMe-oF host NQN
  (`nqn.2014-08.io.<namespace/>:uuid:<Node UID>`) into the pool's
  `allowed_hosts` on the control plane.
- `createStorageClassIfNotExists` creates the StorageClass below.

## Example: generated `StorageClass`

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simplyblock-simplyblock-cluster-a-pool-a
  labels:
    storage.simplyblock.io/namespace: simplyblock
    storage.simplyblock.io/cluster: cluster-a
    storage.simplyblock.io/pool: pool-a
provisioner: csi.simplyblock.io
parameters:
  cluster_id: 2403cae5-b9df-4e46-a761-4283d81d8535
  pool_name: pool-a
  dhchap_node_label: simplyblock.io/pool.simplyblock.cluster-a.pool-a
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
allowedTopologies:
- matchLabelExpressions:
  - key: simplyblock.io/pool.simplyblock.cluster-a.pool-a
    values:
    - allowed
```

`dhchap_node_label` and `allowedTopologies` are only set when
`spec.dhchap: true` **and** `allowedNodes` is non-empty. Both `parameters`
and `allowedTopologies` are immutable in the Kubernetes API once created, so
this object is create-only — editing `AllowedNodes` later only ever changes
Node labels, never this StorageClass.

## hostNQN / hostid

- New: `--hostnqn=nqn.2014-08.io.simplyblock:uuid:<Node UID>`, `--hostid=<same UUID>` — derived, one per node.
- Legacy default (no longer used for lvol connects): `nqn.2014-08.org.nvmexpress:uuid:<random>`, from `/etc/nvme/hostnqn`/`hostid`.

## Example: `nvme connect` with DHCHAP

```
nvme connect ... --hostnqn=nqn.2014-08.io.simplyblock:uuid:416db8c3-... \
  --dhchap-secret=DHHC-1:00:...: --dhchap-ctrl-secret=DHHC-1:00:...: \
  --hostid=416db8c3-...
```

`--hostid` is always the same UUID as `--hostnqn` (`hostIDFromHostNQN`), never
the node's file-seeded default.

## Failure mode: Pod scheduled to a disallowed node

If a future bug let a Pod using this PVC/PV land on a node outside
`AllowedNodes` anyway, `nvme connect` itself is never reached. `NodeStageVolume`
on that node computes *its own* host NQN and calls the control plane's
`/connect?host_nqn=...`; since that node's NQN was never added to the pool's
`allowed_hosts`, `HostConnectAuth.resolve` (sbcli) raises before any connect
command is even built:

```
Host NQN nqn.2014-08.io.simplyblock:uuid:<wrong-node-UID> not found in allowed hosts for volume <lvol-id>
```

`/connect` returns this as an HTTP `404`. The CSI driver surfaces it wrapped
as an `HTTPError`, visible as a `FailedMount`/`MountVolume.MountDevice failed`
Pod event:

```
rpc error: code = Internal desc = ... failed to fetch connection: GET 404: Host NQN nqn.2014-08.io.simplyblock:uuid:<wrong-node-UID> not found in allowed hosts for volume <lvol-id>
```
