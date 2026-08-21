# Findings from lblk e2e testing (side issues, not the main feature)

Minor/tangential issues hit while working through lblk-e2e-testing.md. None of these
are lblk-specific bugs — noting them here since they're worth someone's attention
separately.

## 1. sbcli: hugepage sizing stacks additively on top of a pre-existing reservation
`simplyblock_core/utils/__init__.py::set_hugepages_if_needed()` treats whatever
`nr_hugepages` value it finds *before* simplyblock's first configure call as a
"user baseline" that must be preserved, then adds simplyblock's own calculated need
on top (`required = current_user + hugepages_needed`). If the host already has hugepages
reserved via `sysctl`/boot config (e.g. our VMs' `/etc/sysctl.d/hugepages.conf`), that
baseline gets treated as permanently user-owned and simplyblock's own requirement stacks
on top of it, not instead of it. On a 23-24GB VM with a 14GB pre-existing reservation,
this pushed hugepages to ~24GB (nearly all of RAM), starving Kubernetes' "regular"
memory pool down to ~300MB and blocking pod scheduling — even affecting unrelated pods
(`admin-control` started erroring). The persisted state lives at
`/var/run/simplyblock/hugepages_baseline_node{N}` and `hugepages_sb_node{N}`; workaround
was to delete those files and reset `nr_hugepages=0` before the first configure call so
the baseline captured is 0. Reproduced on two separate nodes (vm02, vm03) independently.

## 2. Operator/helm: recreating the storage-node ServiceAccount orphans already-running pods' tokens, taking the cluster down
`spdk_process_start`/`spdk_process_cleanup` (invoked from the storage-node pods themselves,
via `simplyblock_web/api/internal/storage_node/kubernetes.py`, running as ServiceAccount
`simplyblock-storage-node-sa` in namespace `default`) started getting `401 Unauthorized`
from the Kubernetes API on every job/pod-delete call. The underlying SPDK pods and storage
nodes were never actually unhealthy — but because the storage node could no longer manage
its own SPDK pod via the k8s API, it marked itself offline, which cascaded into the whole
cluster going `suspended`, degrading a previously-healthy 3-node cluster with no real
infrastructure fault.

Root cause (confirmed, not the "stale token after ~107 min" guess I originally wrote here):
one of the earlier `helm upgrade`/redeploy cycles deleted and recreated the
`simplyblock-storage-node-sa` ServiceAccount object (new UID). Kubernetes' bound
service-account-token feature embeds the SA's UID as a claim in the JWT at mint time; two
of the three `storage-node-ds` pods had been running since *before* that recreation, so
their already-mounted, kubelet-projected tokens still carried the old (now nonexistent) SA
UID. The API server correctly rejects those as `401` even though the token file's *content*
looks unchanged and `kubectl auth can-i ... --as=system:serviceaccount:default:simplyblock-storage-node-sa`
against the *current* SA object returns "yes" (RBAC itself was never the problem — this is
purely an authentication-layer identity mismatch, one layer below RBAC). Confirmed by
decoding the JWT from inside the older pod and diffing its `serviceaccount.uid` claim
against `kubectl get sa simplyblock-storage-node-sa -o jsonpath='{.metadata.uid}'`.

Workaround: restart/recreate the DaemonSet pods that predate the SA recreation, so kubelet
mounts a fresh token bound to the current SA UID. Worth someone checking whether the helm
chart's SA template has a churn-prone name/annotation that causes `helm upgrade` to
delete+recreate it (vs. patch-in-place) — that churn is the actual trigger, and any
long-running pod using that SA is vulnerable to this on every such redeploy.

## 3. Environment: QEMU NVMe emulation returns invalid namespace identify data
On the israel VMs (before a reboot), rebinding the local NVMe PCI devices from
`uio_pci_generic` to the kernel `nvme` driver succeeded at the controller/queue level but
produced zero usable block devices — dmesg logged `"Ignoring bogus Namespace
Identifiers"` for every namespace. A full VM reboot resolved it cleanly (all namespaces
came up with valid unique serials and real capacity afterward). Root cause not fully
diagnosed (likely something about how the per-namespace Identify Namespace ID Descriptor
List is populated by the emulation, possibly stateful/transient), but not reproducible
after a clean boot. Environment-specific, not a simplyblock code issue.

## 4. sbcli: NVMe-oF export `serial_number` is not per-volume-unique (architectural, not a bug)
`subsystem_create(lvol.nqn, lvol.ha_type, lvol.uuid, ...)` sets the exported NVMe `SN`
field to `lvol.ha_type` ("ha"/"single"/"default" — 2-3 possible values), not something
unique per lvol; the actually-unique identifier (lvol UUID) goes in `model_number`
instead, the reverse of normal NVMe SN/MN convention. This only matters if something
tries to distinguish multiple lvols from the same simplyblock cluster by their exported
NVMe serial (e.g. our abandoned donor-cluster-as-lblk-backing workaround, which hit
`lblk`'s serial-uniqueness check almost immediately). Not a concern for any normal
consumption path (the CSI driver keys off PV/PVC/lvol UUID, never host-visible NVMe SN).

## 5. Test hygiene: stale StorageClass from a previous run caused a false e2e failure
`spdkcsi-e2e-xfs` StorageClass survived from an old test run (28h old) and collided with
a fresh e2e run's attempt to create the same name, producing a `[FAILED]` that had
nothing to do with the code under test. Not a bug — just a reminder to clean up
`kubectl get storageclass` leftovers before a fresh e2e run on a shared/reused cluster.
