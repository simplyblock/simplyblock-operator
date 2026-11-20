# Installation with Helm 3

Follow this guide to install the SPDK-CSI Driver for Kubernetes.

## Prerequisites

### [Install Helm](https://helm.sh/docs/intro/quickstart/#install-helm)

### Build image

```console
make image
cd deploy/spdk
sudo docker build -t spdkdev .
```
 **_NOTE:_**
Kubernetes nodes must pre-allocate hugepages in order for the node to report its hugepage capacity.
A node can pre-allocate huge pages for multiple sizes.

## Install latest CSI Driver via `helm install`

```console

helm repo add spdk-csi https://raw.githubusercontent.com/simplyblock-io/spdk-csi/master/charts/spdk-csi

helm repo update

helm install -n simplyblk --create-namespace spdk-csi spdk-csi/spdk-csi \
  --set csiConfig.simplybk.uuid=ace14718-81eb-441f-9d4c-d71ce6904196 \
  --set csiConfig.simplybk.ip=https://96xdzb9ne7.execute-api.us-east-1.amazonaws.com \
  --set csiSecret.simplybk.secret=k6U5moyrY5vCVtSiCcKo \
  --set logicalVolume.pool_name=testing1
```

## After installation succeeds, you can get a status of Chart

```console
helm status "spdk-csi" --namespace "simplyblk"
```

## Delete Chart

If you want to delete your Chart, use this command

```bash
helm uninstall "spdk-csi" --namespace "simplyblk"
```

If you want to delete the namespace, use this command

```bash
kubectl delete namespace simplyblk
```

## driver parameters

The following table lists the configurable parameters of the latest Simplyblock CSI Driver chart and default values.

| Parameter                              | Description                                                                                                              | Default                                                                 |
| -------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------- |
| `driverName`                           | alternative driver name                                                                                                  | `csi.simplyblock.io`                                                           |
| `image.csi.repository`             | simplyblock-csi-driver image                                                                                             | `simplyblock/spdkcsi`                                                   |
| `image.csi.tag`                    | simplyblock-csi-driver image tag                                                                                         | `v0.1.0`                                                                |
| `image.csi.pullPolicy`             | simplyblock-csi-driver image pull policy                                                                                 | `Always`                                                                |
| `image.csiProvisioner.repository`      | csi-provisioner docker image                                                                                             | `registry.k8s.io/sig-storage/csi-provisioner`                           |
| `image.csiProvisioner.tag`             | csi-provisioner docker image tag                                                                                         | `v4.0.1`                                                                |
| `image.csiProvisioner.pullPolicy`      | csi-provisioner image pull policy                                                                                        | `Always`                                                                |
| `image.csiAttacher.repository`         | csi-attacher docker image                                                                                                 | `gcr.io/k8s-staging-sig-storage/csi-attacher`                           |
| `image.csiAttacher.tag`                | csi-attacher docker image tag                                                                                             | `v4.5.1`                                                                |
| `image.csiAttacher.pullPolicy`         | csi-attacher image pull policy                                                                                            | `Always`                                                                |
| `image.nodeDriverRegistrar.repository` | csi-node-driver-registrar docker image                                                                                   | `registry.k8s.io/sig-storage/csi-node-driver-registrar`                 |
| `image.nodeDriverRegistrar.tag`        | csi-node-driver-registrar docker image tag                                                                               | `v2.10.1`                                                               |
| `image.nodeDriverRegistrar.pullPolicy` | csi-node-driver-registrar image pull policy                                                                              | `Always`                                                                |
| `image.csiSnapshotter.repository`      | csi-snapshotter docker image                                                                                             | `registry.k8s.io/sig-storage/csi-snapshotter`                           |
| `image.csiSnapshotter.tag`             | csi-snapshotter docker image tag                                                                                         | `v8.2.0`                                                                |
| `image.csiSnapshotter.pullPolicy`      | csi-snapshotter image pull policy                                                                                        | `Always`                                                                |
| `image.csiSnapshotterController.repository`      | csi-snapshotter-controller docker image                                                                                             | `registry.k8s.io/sig-storage/snapshot-controller`                           |
| `image.csiSnapshotterController.tag`             | csi-snapshotter-controller docker image tag                                                                                         | `v8.2.0`                                                                |
| `image.csiSnapshotterController.pullPolicy`      | csi-snapshotter-controller image pull policy                                                                                        | `Always`                                                                |
| `image.csiResizer.repository`          | csi-resizer  docker image                                                                                                | `gcr.io/k8s-staging-sig-storage/csi-resizer`                            |
| `image.csiResizer.tag`                 | csi-resizer docker image tag                                                                                             | `v1.10.1`                                                               |
| `image.csiResizer.pullPolicy`          | csi-resizer image pull policy                                                                                            | `Always`                                                                |
| `image.csiHealthMonitor.repository`    | csi-external-health-monitor-controller docker image                                                                      | `gcr.io/k8s-staging-sig-storage/csi-external-health-monitor-controller` |
| `image.csiHealthMonitor.tag`           | csi-external-health-monitor-controller docker image tag                                                                  | `v0.11.0`                                                               |
| `image.csiHealthMonitor.pullPolicy`    | csi-external-health-monitor-controller image pull policy                                                                 | `Always`                                                                |
| `serviceAccount.create`                | whether to create service account of spdkcsi-controller                                                                  | `true`                                                                  |
| `rbac.create`                          | whether to create rbac of spdkcsi-controller                                                                                | `true`                                                                  |
| `controller.replicas`                  | replica number of spdkcsi-controller                                                                                     | `1`                                                                     |
| `serviceAccount.create`                | whether to create service account of csi controller                                                                  | `true`                                                                  |
| `rbac.create`                          | whether to create rbac of csi controller                                                                                | `true`                                                                  |
| `controller.replicas`                  | replica number of csi controller                                                                                     | `1`                                                                     |
| `controller.tolerations.create`       | Whether to create tolerations for the csi controller                                                                       | `false`                                                                     |  |
| `controller.tolerations.list[0].effect`       | The effect of tolerations on the csi controller	                                                                          | `<empty>`                                                               |  |
| `controller.tolerations.list[0].key	`        | The key of tolerations for the csi controller	                                                                            | `<empty>`                                                                |  |
| `controller.tolerations.list[0].operator	`    | The operator for the csi controller tolerations	                                                                          |                                            `Exists`                                                                    |  |
| `controller.tolerations.list[0].value	`      | The value of tolerations for the csi controller	                                                                          |                                            `<empty>`                                                        |  |
| `controller.nodeSelector.create`       | Whether to create nodeSelector for the csi controller                                                                       | `false`                                                                     |  |
| `controller.nodeSelector.key	`        | The key of nodeSelector for the csi controller	                                                                            | `<empty>`                                                                |  |
| `controller.nodeSelector.value	`      | The value of nodeSelector for the csi controller	                                                                          |                                            `<empty>`                                                        |  |
| `storageclass.create`                  | create storageclass                                                                                                      | `true`                                                                  |  |
| `storageclass.annotations`             | Annotations attached to the created StorageClass. If simplyblock.io/auto-restart-on-pathloss: "true" is set, pods using PVCs from this StorageClass will be automatically restarted when the storage paths to the volume are lost and later restored.	{}	                                                                                                      | `{}`                                                                  |  |
| `snapshotclass.create`                  | create snapshotclass                                                                                                   | `true`                                                                  |  |
| `snapshotcontroller.create`             | create snapshot controller and CRD for snasphot support it                                                                                                    | `true`                                                                  |  |
| `externallyManagedConfigmap.create`    | Specifies whether a externallyManagedConfigmap should be created                                                         | `true`                                                                  |  |
| `externallyManagedSecret.create`       | Specifies whether a externallyManagedSecret should be created                                                            | `true`                                                                  |  |
| `csiConfig.simplybk.uuid`              | the simplyblock cluster UUID on which the volumes are provisioned                                                                 | ``                                                                      |  |
| `csiConfig.simplybk.ip`                | the HTTPS API Gateway endpoint connected to the management node                                                          | `https://o5ls1ykzbb.execute-api.eu-central-1.amazonaws.com`             |  |
| `csiSecret.simplybk.secret`            | the cluster secret associated with the cluster                                                                           | ``                                                                      |  |
| `logicalVolume.pool_name`              | the name of the pool against which the lvols needs to be provisioned. This Pool needs to be created before its passed here. | `testing1`                                                              |  |
| `logicalVolume.qos_rw_iops`            | the value of lvol parameter qos_rw_iops                                                                                  | `0`                                                                     |  |
| `logicalVolume.qos_rw_mbytes`          | the value of lvol parameter qos_rw_mbytes                                                                                | `0`                                                                     |  |
| `logicalVolume.qos_r_mbytes`           | the value of lvol parameter qos_r_mbytes                                                                                 | `0`                                                                     |  |
| `logicalVolume.qos_w_mbytes`           | the value of lvol parameter qos_w_mbytes                                                                                 | `0`                                                                     |  |
| `logicalVolume.encryption`             | set to `True` if encryption needs be enabled on lvols.                                                                   | `False`                                                                 |  |
| `logicalVolume.numDataChunks`          | The number of Erasure coding schema parameter k (distributed raid)                                                       | `1`                                                                     |  |
| `logicalVolume.numParityChunks`        | The number of Erasure coding schema parameter n (distributed raid)                                                                                                 | `1`                                                                     |  |
| `logicalVolume.max_namespace_per_subsys` | the maximum namespace per subsystem                                                                               | `1`                                                                     |  |
| `podAnnotations`                        | Annotations to apply to all pods in the chart                                                                   | `{}`                                                                     |  |
| `simplyBlockAnnotations`                | Annotations to apply to Simplyblock kubernetes resources like DaemonSets, Deployments, or StatefulSets                                                                                         | `{}`                                                                     |  |
| `benchmarks`                           | the number of benchmarks to run                                                                                          | `0`                                                                     |  |
| `node.tolerations.create`       | Whether to create tolerations for the csi node                                                                       | `false`                                                                     |  |
| `node.tolerations.list[0].effect`       | The effect of tolerations on the csi node	                                                                          | `<empty>`                                                               |  |
| `node.tolerations.list[0].key	`        | The key of tolerations for the csi node	                                                                            | `<empty>`                                                                |  |
| `node.tolerations.list[0].operator	`    | The operator for the csi node tolerations	                                                                          |                                            `Exists`                                                                    |  |
| `node.tolerations.list[0].value	`      | The value of tolerations for the csi node	                                                                          |                                            `<empty>`                                                        |  |
| `node.nodeSelector.create`       | Whether to create nodeSelector for the csi node                                                                       | `false`                                                                     |  |
| `node.nodeSelector.key	`        | The key of nodeSelector for the csi node	                                                                            | `<empty>`                                                                |  |
| `node.nodeSelector.value	`      | The value of nodeSelector for the csi node	                                                                          |                                            `<empty>`                                                        |  |
| `storagenode.create`                   | Whether to create the NUMA resource plugin DaemonSet for storage node CPU topology                                        | `false`                                                        |  |
| `storagenode.enableCpuTopology`        | Whether to enable cpu topology for storage node on kubernetes worker node                                                                           | `false`                                                        |  |
| `storagenode.enableDevicePlugin`        | Whether to enable Simplyblock NUMA resource device plugin (numa-resource-plugin) on Kubernetes cluster                                                                          | `true`                                                        |  |
| `storagenode.daemonsets[0].nodeSelector.key`       | The key used in the nodeSelector to constrain which nodes the numa-resource-plugin should run on		                                                                    | `io.simplyblock.node-type`                                                                     |  |
| `storagenode.daemonsets[0].nodeSelector.value`     | The value for the nodeSelector key to match against specific nodes		                                                                    | `simplyblock-storage-plane`                                                                     |  |
| `storagenode.multiCluster.enable`        | Enable multi-cluster CSI secret support                                                                                            | `false`                                                                |  |
| `storagenode.multiCluster.clusters[].cluster_id`  | UUID of the Simplyblock cluster                                                                                                 | `<empty>`                                                                |  |
| `storagenode.multiCluster.clusters[].secret`    |  Secret of the Simplyblock cluster                                                                                                | `<empty>`                                                                |  |

## troubleshooting
 - Add `--wait -v=5 --debug` in `helm install` command to get detailed error
 - Use `kubectl describe` to acquire more info
