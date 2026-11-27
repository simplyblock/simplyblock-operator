# Red Hat certification runs

The test runs behind simplyblock's Red Hat certifications, kept here so they can
be repeated for every supported OpenShift version and every simplyblock release
rather than rebuilt by hand each time:

| Level | Certification                          | Tool                                | Here      |
|-------|----------------------------------------|-------------------------------------|-----------|
| 1     | OpenShift CSI Certified                | `openshift-tests run openshift/csi` | `level1/` |
| 2     | OpenShift Virtualization CSI Certified | KubeVirt storage checkup            | `level2/` |
| 3     | Performance and Scale Tested           | `storage-cert-harness`              | `level3/` |

## Level 1

`level1/run.sh` runs the suite as a Pod inside the cluster, in the `tests` image
of the cluster's own release payload, so the test binary matches the OpenShift
version under test. It needs only `oc` and a cluster-admin kubeconfig. The Pod
is not a local container because `openshift-tests` runs only on the cluster's
architecture: it extracts further test binaries from the release payload and
refuses those of another architecture.

For the length of a run, the namespace `simplyblock-cert-level1` holds a
ServiceAccount bound to cluster-admin, which `openshift-tests` needs. The script
removes both when it exits, also on an interrupt, and refuses to start while the
namespace exists. When the results cannot be copied, the script keeps the
namespace and the test pod and says where the archive is.

```sh
make level1 KUBECONFIG=/abs/path/kubeconfig                  # every test pattern
make level1-block-multinode KUBECONFIG=/abs/path/kubeconfig  # ReadWriteMany and ReadOnlyMany
make level1-dry-run KUBECONFIG=/abs/path/kubeconfig          # list the tests, run none
```

Both profiles are needed for a complete result. The suite cannot scope a
capability to one volume mode, and simplyblock serves ReadWriteMany and
ReadOnlyMany for raw block only, so those two run separately on the block
patterns (`level1/manifest.yaml` has the detail). The block-multinode run leaves
out the multiVolume tests that pair a block volume with a filesystem one: every
claim in that profile is ReadWriteMany, and simplyblock serves a ReadWriteMany
filesystem only from a pNFS class. The main profile runs them. The run writes the
list it ran to `tests.txt` and names what it left out in `run-info.txt`.

Volume group snapshots are tested where the cluster serves
`groupsnapshot.storage.k8s.io/v1`, the generally available API (OpenShift 4.23
and 5.0, where Red Hat lists them as an optional test). Before that the suite's
manifest has no way to name a VolumeGroupSnapshotClass, which arrived with
Kubernetes 1.35, so the capability stays off. The script names a class of the
driver's own when there is one and otherwise has the suite generate one;
`GROUP_SNAPSHOT=on|off` overrides the choice, and `run-info.txt` records it.

The manifests name the classes under test as `${STORAGE_CLASS}` and
`${SNAPSHOT_CLASS}`. The script fills them in with the StorageClass and the
VolumeSnapshotClass whose driver is `csi.simplyblock.io`, because the operator
names its StorageClass after the storage cluster and the name changes with every
deployment. When more than one class of either kind uses the driver, the script
stops and lists them. Set `STORAGE_CLASS` or `SNAPSHOT_CLASS` to pick one.

Each run writes `results/level1-<profile>-<UTC time>/` with the rendered
manifests, `run-info.txt` (OpenShift version, image, classes), the full log, the
JUnit files under `junit/`, and the monitor output. `results/` is not committed.

Run one suite at a time on a cluster: the LUN stress test in `ocp-manifest.yaml`
expects nothing else to run beside it.

The LUN stress test provisions 260 volumes at once, which Red Hat recommends
for a submission. On a cluster that cannot hold that many, a rehearsal can lower
the count with `LUN_STRESS_PODS=<n>`; `run-info.txt` then marks the run as a
rehearsal.

A filesystem ReadWriteMany run over a pNFS class is not part of this yet.

## Level 2

Red Hat's virtualization readiness tier is the Level 1 run plus OpenShift
Virtualization's KubeVirt storage checkup, and the result to submit is the
checkup's ConfigMap. `level2/run.sh` runs the checkup as a Job in the namespace
`simplyblock-cert-level2`, in the checkup image the installed OpenShift
Virtualization operator ships, and removes the namespace and its cluster-reader
binding when it exits.

```sh
make level2 KUBECONFIG=/abs/path/kubeconfig
```

The checkup judges the cluster's whole storage setup, so the script prepares
two things the driver does not decide. It makes the class under test the default
virtualization class, where the golden images (the OS images VMs are cloned
from) are imported, and refuses when another class already is. It then waits
until every golden image is up to date, because the checkup fails on one that is
not and skips its VM tests when there is none.

A StorageProfile CDI cannot fill, such as one for a provisioner it does not know,
fails the checkup whatever class is under test. On the lab cluster that is
`local-path`, which the control plane's own volumes use; the submission explains
it rather than the run hiding it.

Each run writes `results/level2-<UTC time>/` with `run-info.txt`, the result
ConfigMap (`storage-checkup-config.yaml`), the checkup's log, the cluster
version, the OpenShift Virtualization version, and the StorageProfiles.

Level 1 copies the class under test for its own test classes, annotations
included. On a cluster where that class is the default virtualization class, a
Level 1 run briefly adds a second one, and CDI may point the golden images at the
suite's short-lived copy (it records the choice in the DataImportCrons'
`cdi.kubevirt.io/storage.import.storageClass` annotation). The next scheduled
import chooses again.

## Level 3

Level 3 is Red Hat's storage certification harness
(`github.com/openshift-storage-cert/storage-cert-harness`), whose
`PARTNER-CERTIFICATION-GUIDE.md` is the procedure. The harness ships as an
amd64-only image driven by Podman on a Linux host. `level3/run.sh` runs that
image as a Pod in the cluster instead, with the arguments the harness's own
partner scripts pass, so any workstation can drive it. It pins a harness commit
and image digest, writes `backends.yaml` from the discovered classes, and fills
the plan's nodes: three distinct Ready workers, which the harness Pod is kept
off.

```sh
make level3-preflight KUBECONFIG=/abs/path/kubeconfig
make level3-load-80 KUBECONFIG=/abs/path/kubeconfig EXTERNAL_LOAD="<what holds 80%>"
make level3-no-load-80 KUBECONFIG=/abs/path/kubeconfig FAR_CONFIG=/abs/path/far-config.json
make level3-package
```

Run 1 measures 4K random I/O latency while something else holds the storage at
about 80% of its capacity. The harness does not generate that load, and
`EXTERNAL_LOAD` records what did. Run 2 continues from run 1's report.
TR-VIRT-007 fences a worker through its BMC with Fence Agents Remediation and
needs `FAR_CONFIG`, a FenceAgentsRemediation manifest for that worker (the
harness's `docs/cluster-setup-guide.md`, step 4). `SKIP=TR-VIRT-007` leaves it
out, and `ONLY=`/`SKIP=` with any IDs mark a run as a rehearsal.

Results land in `results/level3/` in the harness's layout:
`certification-results/<profile>/{work,report}`, `logs/`, and
`certification-results-level3.tar.gz` from the package step. A step's earlier
output is kept beside the new one with a timestamp.

## A cluster on AWS

`aws/cluster.sh` creates a cluster the three levels can run on: OpenShift
installed by `openshift-install` into one availability zone, three
`m6i.2xlarge` control-plane nodes, and three `c6id.metal` workers. Metal
workers are needed because OpenShift Virtualization runs VMs only on bare metal
on AWS. The instance-store NVMe drives hold simplyblock's storage. The instance
sizes meet the Level 3 minimum, 64 physical cores and 256 GB per worker. Every
size, the version, and the zone can be changed through the environment, as the
script's header lists.

```sh
make aws-create PULL_SECRET_FILE=~/.secrets/redhat-pull-secret.json  # OCP_VERSION=stable-4.22 by default
make aws-install-virt
make aws-install-simplyblock
make level1 KUBECONFIG="$(aws/cluster.sh kubeconfig)"
make aws-destroy
```

The pull secret comes from console.redhat.com and has to carry
`registry.redhat.io` and `quay.io`. It is written only into the installer's
configuration, which `openshift-install` deletes once it has read it. AWS
credentials come from the environment or `~/.aws`. The public Route 53 zone of
`BASE_DOMAIN` (`api.ocp.cert.simplyblock.ai` by default) has to exist in the
account, and the parent domain has to delegate to it with NS records. The
installer reaches the new cluster's API by its public name, so without the
delegation the install stops at `no such host`.

The installer's state, including the kubeconfig and the metadata `destroy` reads,
lives in `aws/clusters/<CLUSTER_NAME>/`. That directory is the only record of
what to remove: deleting it before `destroy` strands the cluster's AWS
resources. Every resource carries the tags `simplyblock.io/owner` and
`simplyblock.io/expires`. Nothing acts on the expiry tag: a cluster past it
stays until somebody runs `destroy`.

Every step except `status` logs to `aws/logs/<CLUSTER_NAME>/<step>-<UTC time>.log`.
`create`, `wait`, and `destroy` also keep the installer's own debug log there as
`openshift-install-<UTC time>.log`, which holds far more than the installer
prints. The logs outlive `destroy`. The kubeadmin password is redacted in them
and stays only in the installer's `auth/kubeadmin-password`.

`install-simplyblock` first opens TCP between the cluster's nodes, in the
installer's node security group: the group allows only OpenShift's own ports,
and simplyblock's control plane and NVMe-oF listeners use others. The rule
admits the group itself and nothing outside it, and needs the AWS CLI. The step
then installs this checkout's chart with the chart's snapshot controller turned
off, since OpenShift runs its own, and with the control plane's volumes on the
cluster's default StorageClass (`gp3-csi`). Left unnamed, the operator would
install `local-path` for them, which fails CDI's StorageProfile check and with it
the Level 2 checkup. The operator turns the snapshotter's group
snapshot support off where the cluster serves no group snapshot API, which is
OpenShift 4.x without the Tech Preview. The step then approves the discovery draft, and waits until
the storage cluster is active. On OpenShift every worker reboots into the
storage MachineConfig on the way, one at a time, so the step takes most of an
hour. Before approving, it sets the draft's `maxSubsystemCount` to
`MAX_SUBSYSTEM_COUNT` (default 128): every volume takes one NVMe-oF subsystem on
its storage node, the Level 1 LUN stress test creates 260 volumes, and the
draft's 30 per node stopped it at 90 on three nodes. The count is fixed when the
cluster is created. `APPROVE_DRAFT=false` stops at the draft for a review
instead.

`uninstall-simplyblock` removes simplyblock, its storage cluster, and its
volumes with the chart's cleanup script, so that `install-simplyblock` can build
the storage cluster again with settings fixed at creation, such as
`MAX_SUBSYSTEM_COUNT`.

Two operator defects are worked around in the step, so that it runs without a
person: a discovery run that has not appeared within five minutes is applied
from the chart's bootstrap ConfigMap, and when the draft fails because its
activation deadline ran out while the workers rebooted, the step asks for the
activation itself once every node is online. Where cert-manager is
missing, it installs Red Hat's cert-manager Operator first: the chart's default,
mutual TLS, needs it.
