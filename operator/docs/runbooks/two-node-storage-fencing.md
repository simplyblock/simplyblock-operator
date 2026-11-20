<!--
Runbook: Kubernetes follows the arbiter's fencing on a two-node cluster
(sbcli docs/design/two-node-kubernetes-fencing.md, mode witness). Written for the
administrator setting it up and for support when a node stays fenced. Keep it in
step with operator/internal/controllers/twonode and the CSI node plugin's host
NQN annotation.
-->

# Two-node clusters: Kubernetes follows the storage fence

On a two-node cluster the central control plane is the witness. When the two
nodes lose each other, it decides which one continues. With host fencing on,
Kubernetes follows that decision: the losing node's pods and VMs are restarted on
the winner within seconds, without waiting for power fencing.

## How it works

1. The arbiter grants the winner and fences the loser's storage.
2. The control plane revokes every NVMe host identity of the loser's Kubernetes
   node on the winner's volumes. From then on nothing on the loser can write to
   the cluster, through any path.
3. The operator sets the node condition `StorageFenced=True` on the loser.
4. NodeHealthCheck sees the condition and creates a `StorageFenceRemediation`
   for the node.
5. The remediation confirms the fence with the control plane and applies
   `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` (non-graceful node
   shutdown). Pods are force-deleted, VolumeAttachments force-detached, and the
   workloads restart on the winner.
6. When the node is back and the storage has healed, the operator waits until
   no pod from before the fence is left on the node, then asks the control plane
   to give the host its access back. The condition turns `False`, NodeHealthCheck
   deletes the remediation, and the taint comes off.

The node's host is **not** fenced, and nothing is restarted, when only the
storage service is affected: an SPDK crash, a planned restart of the storage
node, or the node losing its connection to the control plane while it still
reaches its peer. Its workloads keep running through the peer's path.

## Prerequisites

- The NodeHealthCheck operator (medik8s; on OpenShift the "Node Health Check
  Operator" from OperatorHub).
- All stateful workloads on the two nodes use simplyblock volumes. The
  out-of-service taint force-detaches every volume of the node, and only
  simplyblock's are fenced, so a node with another driver's volume attached is
  not remediated (phase `Blocked`) unless the template sets
  `allowForeignVolumes: true`.

## Enabling it

On the StorageCluster:

```yaml
spec:
  clusterMode: Dual
  enableArbitration: true
  arbitration:
    hostFencing: true
```

The operator pushes the setting to the control plane and reports each node's
host NQNs. Check them on the StorageNodes:

```bash
kubectl -n simplyblock get storagenode -o jsonpath='{range .items[*]}{.spec.workerNode}{"\t"}{.status.remediation.reportedHostNQNs}{"\n"}{end}'
```

Each node should list two NQNs: `nqn.2014-08.org.nvmexpress:uuid:...` (published
by the CSI node plugin as the annotation `storage.simplyblock.io/nvme-host-nqn`)
and `nqn.2014-08.io.simplyblock:uuid:...`.

Then create the remediation template and the NodeHealthCheck:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageFenceRemediationTemplate
metadata:
  name: storage-fence
  namespace: simplyblock
spec:
  template:
    spec:
      allowForeignVolumes: false
---
apiVersion: remediation.medik8s.io/v1alpha1
kind: NodeHealthCheck
metadata:
  name: simplyblock-storage-fenced
spec:
  selector:
    matchExpressions:
      - key: node-role.kubernetes.io/worker
        operator: Exists
  # Required: with the default minHealthy of 51%, one node out of two is never
  # remediated.
  maxUnhealthy: 1
  unhealthyConditions:
    - type: StorageFenced
      status: "True"
      duration: 2s
  remediationTemplate:
    apiVersion: storage.simplyblock.io/v1alpha2
    kind: StorageFenceRemediationTemplate
    namespace: simplyblock
    name: storage-fence
```

On a two-node OpenShift cluster with an arbiter node, the selector must match
the two workers and not the arbiter.

## Watching a fence

```bash
kubectl -n simplyblock get storagecluster <name> -o jsonpath='{.status.arbitration.hostFences}'
kubectl get node <node> -o jsonpath='{.status.conditions[?(@.type=="StorageFenced")]}'
kubectl -n simplyblock get storagefenceremediation
```

| Host fence state | Meaning                                                                                   |
|------------------|-------------------------------------------------------------------------------------------|
| `revoking`       | The control plane is revoking the node's hosts on the winner. No condition yet.           |
| `revoked`        | Revoked; `StorageFenced=True`.                                                            |
| `releasable`     | The storage has healed. The operator releases the fence once the node is Ready and clean. |
| `restoring`      | The release is running.                                                                   |
| `unavailable`    | No host NQN known for the node: nothing revoked, no condition, no remediation.            |

| Remediation phase | Meaning                                                                        |
|-------------------|--------------------------------------------------------------------------------|
| `Pending`         | The fence is not confirmed by the control plane (yet), or host fencing is off. |
| `Blocked`         | A volume of another CSI driver is attached; see `status.message`.              |
| `Tainted`         | The out-of-service taint is on the node.                                       |

## When a node stays fenced

- **State `releasable` for a long time.** The operator logs `host fence
  releasable; waiting for the node to be clean` with the reason. Either the node
  is not Ready, or pods from before the fence are still bound to it (the
  out-of-service taint was never applied, for example, because NodeHealthCheck is
  not installed). Once those pods are gone the release runs by itself.
- **State `unavailable`.** No host NQN was reported. Check that the CSI node
  plugin runs on the node (it publishes the annotation) and that the StorageNode
  has `status.remediation.reportedHostNQNs`.
- **Phase `Blocked`.** Detach the other driver's volume, or set
  `allowForeignVolumes: true` on the template if losing it is acceptable.

The out-of-service taint is removed only by the remediation that set it (the node
carries the annotation `storage.simplyblock.io/out-of-service-by:
storage-fence-remediation`). A taint an administrator set stays until they remove
it.
