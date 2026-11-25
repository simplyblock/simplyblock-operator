# Red Hat certification runs

The test runs behind simplyblock's Red Hat certifications, kept here so they can
be repeated for every supported OpenShift version and every simplyblock release
rather than rebuilt by hand each time:

| Level | Certification                          | Tool                                                  | Here      |
|-------|----------------------------------------|-------------------------------------------------------|-----------|
| 1     | OpenShift CSI Certified                | `openshift-tests run openshift/csi`                   | `level1/` |
| 2     | OpenShift Virtualization CSI Certified | OpenShift Virtualization storage checkup (to confirm) | not yet   |
| 3     | Performance and Scale Tested           | `storage-cert-harness`                                | not yet   |

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
namespace exists.

```sh
make level1 KUBECONFIG=/abs/path/kubeconfig                  # every test pattern
make level1-block-multinode KUBECONFIG=/abs/path/kubeconfig  # ReadWriteMany and ReadOnlyMany
make level1-dry-run KUBECONFIG=/abs/path/kubeconfig          # list the tests, run none
```

Both profiles are needed for a complete result. The suite cannot scope a
capability to one volume mode, and simplyblock serves ReadWriteMany and
ReadOnlyMany for raw block only, so those two run separately on the block
patterns (`level1/manifest.yaml` has the detail).

The manifests name the classes under test as `${STORAGE_CLASS}` and
`${SNAPSHOT_CLASS}`; set them when the cluster's differ from the defaults
(`simplyblock-csi-sc`, `simplyblock-csi-snapshotclass`).

Each run writes `results/level1-<profile>-<UTC time>/` with the rendered
manifests, `run-info.txt` (OpenShift version, image, classes), the full log, the
JUnit files under `junit/`, and the monitor output. `results/` is not committed.

Run one suite at a time on a cluster: the LUN stress test in `ocp-manifest.yaml`
expects nothing else to run beside it.

A filesystem ReadWriteMany run over a pNFS class is not part of this yet.
