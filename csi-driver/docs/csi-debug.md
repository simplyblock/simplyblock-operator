### case#1: volume mount/unmount failed

#### simplyblock CSI driver requires nvme-cli to be installed.

If the Kubernetes worker doesn't have nvme-cli installed, you might get error like this during the volume mount stage.
```
  Warning  FailedMount             17s (x7 over 49s)  kubelet                  MountVolume.MountDevice failed for volume "pvc-ac6f4696-18a8-4e98-a0d4-28237eef5ad9" : rpc error: code = Internal desc = exit status 254
```

This because the CSI driver internally nvme-cli to connect to the remote NVMe volume. The CLI can be installed by running
```
sudo yum install -y nvme-cli;
sudo modprobe nvme-tcp
```

#### the worker node needs the kernel NVMe/TCP module

If the worker's kernel has no `nvme-tcp` module, the volume mount fails with an error like this.
```
  Warning  FailedMount  kubelet  MountVolume.MountDevice failed for volume "pvc-..." : rpc error: code = FailedPrecondition desc = this node cannot connect a volume: kernel 6.8.0-1044-aws has no NVMe/TCP initiator loaded (/dev/nvme-fabrics is absent, and modprobe nvme-tcp said: modprobe: FATAL: Module nvme-tcp not found in directory /lib/modules/6.8.0-1044-aws). ...
```

Install the package that carries the module on the worker and load it. The mount is retried and needs no restart.
```
sudo apt-get install -y linux-modules-extra-$(uname -r)   # Ubuntu
sudo dnf install -y kernel-modules-extra                  # RHEL and its derivatives
sudo modprobe nvme-tcp
```

A mount failure is not the only signal. The CSI node plugin probes the module at start and publishes the result as a node label, so a missing module is visible before any volume is requested. One command lists every node's state:
```
kubectl get nodes -L storage.simplyblock.io/nvme-tcp-capable
```
A node showing `false` is missing the module and fails to mount a volume until it is installed. The plugin owns the label. A value pinned by hand (for example, on a golden image) is left alone: set the label and remove its `storage.simplyblock.io/nvme-tcp-capable-managed-by` annotation, and the probe no longer overwrites it.

#### the worker nodes should be able to contact simplyblock storage node

Since SPDK uses remote NVMe connection, make sure that there is network connectivity between the simplyblock storage node and the Kubernetes worker nodes.

If not, we get errors like this during VolumeStage step.

```
  Warning  FailedMount             2s (x8 over 88s)  kubelet                  MountVolume.MountDevice failed for volume "pvc-d191cb36-c542-4d1a-806d-28c1999a50c6" : rpc error: code = Internal desc = exit status 146
```

#### check the events of PVC or pod

For any other types of failures in general, you could check the events of the Pod or PVC.

```console
kubectl describe pod test-pod
kubectl describe pvc test-pvc
```

For a more detailed analysis during the `CreateVolume` stage, you could refer to the controller.

```
kubectl -n spdk-csi logs -f spdkcsi-controller-0 -c spdkcsi-controller
```

Or you could refer to the node driver logs to get more info during the volume mount stage

```
kubectl -n spdk-csi logs -f spdkcsi-node-8vh5n -c spdkcsi-node
```
