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

If the worker's kernel has no `nvme-tcp` module, the CSI node plugin does not start on that worker. Its pod is in `CrashLoopBackOff`, the `SimplyblockDriver` reports `Degraded`, and a pod that mounts a simplyblock volume on that worker stays in `ContainerCreating`.
```
kubectl get pods -n <namespace> -o wide | grep csi-node
kubectl describe pod -n <namespace> <csi-node-pod>
```
The reason is a `Warning` event with the reason `NVMeTCPInitiatorMissing` at the end of the pod description. It is also the termination message of the `csi-node` container, shown under `Last State`, and the last line of its log.
```
this node cannot connect a volume: kernel 6.8.0-1040-aws has no NVMe/TCP initiator (the nvme-tcp module is not loaded, and modprobe nvme-tcp said: modprobe: FATAL: Module nvme-tcp not found in directory /lib/modules/6.8.0-1040-aws). On Ubuntu the module is in the package linux-modules-extra-6.8.0-1040-aws: install it on the node and run `modprobe nvme-tcp`
```
The last sentence names a package only on an Ubuntu kernel. On any other distribution it says to make the `nvme-tcp` kernel module available.

Make the module available on the worker and load it. On Ubuntu, it is in the `linux-modules-extra` package of the running kernel.
```
sudo apt-get install -y linux-modules-extra-$(uname -r)
sudo modprobe nvme-tcp
```
The plugin starts on its next restart, which kubelet delays by up to five minutes. Deleting the plugin's pod on that worker starts it at once.

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
