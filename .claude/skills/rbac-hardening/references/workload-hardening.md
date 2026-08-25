# Workload hardening

RBAC decides what a workload may ask the API server for. This page is the other
half: what a workload may do to the node it runs on, and what an escape from its
container reaches.

Both halves compound, and that is the thing to keep in view. A privileged
container holding no API permissions is a host problem. A privileged container
holding cluster-wide `pods/exec` is a cluster problem. See
`references/escalation-primitives.md` for the second half.

Counts measured 2026-08-25 with `scripts/check-rbac.py --source workload`.

## What the data path genuinely needs

NVMe-oF attachment is a host-kernel operation, so parts of this product cannot
run unprivileged, and an audit that pretends otherwise gets ignored.

| Component                    | Needs                                                     | Because                                                                  |
|------------------------------|-----------------------------------------------------------|--------------------------------------------------------------------------|
| CSI node plugin (`csi-node`) | `/dev`, `/sys`, `/lib/modules`, `SYS_ADMIN`, `SYS_MODULE` | connects fabrics, walks the NVMe sysfs tree, loads the nvme-tcp module   |
|                              | `/var/lib/kubelet/pods` with mount propagation            | a mount made in the container has to appear on the host for the workload |
|                              | `/var/lib/kubelet/plugins`, `plugins_registry`            | the CSI socket and its registration                                      |
| Storage node                 | `/dev`, `/sys`, `/lib/modules`, `hostNetwork`             | SPDK drives the device directly and binds host ports                     |
| NUMA resource plugin         | `/sys`, `/var/lib/kubelet/device-plugins`                 | reads topology, registers as a device plugin                             |

`numa-resource-plugin.yaml` is the model for how to write one of these: it drops
`ALL` capabilities and mounts only the two paths it reads. Nothing else in the
charts drops capabilities.

## What does not need it

Four privileged containers do no host-device work. Each is a sidecar or an agent
whose upstream examples run unprivileged.

| Container              | Where                                           | What it actually does                                                                                       |
|------------------------|-------------------------------------------------|-------------------------------------------------------------------------------------------------------------|
| `csi-snapshotter`      | `controller.yaml:79`                            | watches VolumeSnapshot objects and calls the CSI socket. No host access at all                              |
| `csi-registrar`        | `node.yaml:53`                                  | writes one socket into the registration directory                                                           |
| `fluent-bit`           | `fluentbit-daemonset.yaml:85`                   | reads `/var/log` and `/var/lib/docker/containers`, which needs read access, not privilege                   |
| the `csi-hostpath` set | `controlplane_csi-hostpath.yaml` (4 containers) | the upstream hostpath example driver. Whether it belongs in a production chart at all is the prior question |

For the first three the narrowing is the same shape: drop `privileged: true`, drop
`ALL` capabilities, add back only what fails, and make the host mounts
`readOnly: true` where nothing is written. For a log shipper reading host paths,
read-only mounts plus `runAsNonRoot` is usually the whole answer.

## The ladder, narrowest first

1. **No host access.** Most containers, including every sidecar that only talks
   to an API or a socket.
2. **A named host path, read-only.** `/sys` for topology, `/var/log` for logs.
3. **A named host path, writable.** `/dev` for block devices, the kubelet pod
   directory for mount propagation.
4. **Specific capabilities**, with `drop: ["ALL"]` first. `SYS_ADMIN` and
   `SYS_MODULE` for the NVMe path, and nothing else without a reason.
5. **`privileged: true`.** The whole capability set plus device access. It makes
   any `capabilities.add` beside it redundant, which is a reliable sign that
   someone reached for it rather than deriving what was needed. `csi-node`
   carries both.

`hostPID` and `hostIPC` appear nowhere and should stay that way: they break the
container boundary for process inspection, and nothing here needs them.

## Host paths worth a second look

Thirty-eight host mounts exist across the charts. Most name a specific directory
under `/dev`, `/sys`, or the kubelet tree, which is what rung 2 and 3 want. Two
are broader than the rung they sit on:

- **`/mnt` in `storage-node.yaml`** is a parent directory rather than a named
  path. Whatever lives beneath it is reachable, including anything another
  workload mounted there.
- **`/var/lib/docker/containers` in `fluentbit-daemonset.yaml`** is every
  container's logs on the node, which is inherent to log shipping and is a reason
  for the mount to be read-only and the container unprivileged.

A writable mount of `/`, `/etc`, `/var/lib/kubelet` as a whole, or the container
runtime socket is a host takeover and none exists today. That is worth keeping
true, and the checker reports every `hostPath` so a new one is visible in review.

## Service-account tokens

**No pod spec in either chart sets `automountServiceAccountToken`.** Every pod
therefore receives a projected token for its service account, including the ones
that never call the API server, and a token on disk in a privileged container is
the first thing an escape looks for.

Where a pod makes no API calls, `automountServiceAccountToken: false` on the pod
spec removes the credential entirely. That is the cheapest hardening available
here, because it needs no permission analysis: either the workload calls the API
or it does not.

Where a pod does call the API, the token stays, and the RBAC half of the audit
decides what it is worth stealing.

## Writing a new workload

- Start from `numa-resource-plugin.yaml`, not from the storage node.
- `automountServiceAccountToken: false` unless the pod calls the API server.
- Its own ServiceAccount, named in the pod spec, never `default`.
- `drop: ["ALL"]`, then add back what fails, with a `rbac-justified:` note per
  addition saying what needed it.
- `readOnly: true` on every host mount that is not written.
- A privileged container names, in its justification, what an escape from it
  reaches, including the API permissions its service account holds.
