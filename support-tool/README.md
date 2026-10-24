# simplyblock support tool

A local web application for reading a customer's support bundle and building a
ClusterDeploymentConfig from it. It runs on the support agent's machine, serves
only loopback, and holds an uploaded bundle in memory only, because a bundle is
customer data.

The bundle is the archive the bundle script writes
(`helm-charts/scripts/kubectl-simplyblock-bundle`, published as
`curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash`). The
tool prints that command when it starts and shows it on every page, with the
script's steps as individual commands for a customer who cannot run it. Probe reports of versions 9
and 10 are read. A version-9 report predates the kernel facts, and the tool
shows those as not reported rather than as unknown.

## Running it

```bash
make -C support-tool run          # serves http://127.0.0.1:8099/
make -C support-tool build        # writes support-tool/bin/support-tool
```

`-listen` takes another loopback address. A non-loopback address is refused.

## What it shows

The page opens with the bundle's diagnosis: the run's failure, workers the run
selected and has no report for, workers it did not inspect and why, workers that
hand over no device and what refused their disks, NVMe controllers a userspace
driver holds, readings a probe could not take, kernels without native NVMe
multipath, management interfaces with different names, the CDC's errors,
collection errors, and Warning events. Errors come first, and each entry opens
the view that explains it.

| View                    | Content                                                                                                                                                                                                              |
|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Overview                | The discovery run and its settings, what the bundle could not tell (unreadable files, collection errors, warning events), and one line per worker                                                                    |
| Workers                 | Per worker: the Node object, the operating system and kernel facts, CPU, memory, huge pages, block devices with every refusal, network interfaces, NVMe controllers, and the interfaces a discovery run would choose |
| Cluster                 | The ControlPlane, StorageCluster, and StorageNode objects the bundle captured                                                                                                                                        |
| ClusterDeploymentConfig | The bundle's CDC, its layout, and its findings                                                                                                                                                                       |
| Editor                  | Choices per worker (node set, failure domain, interfaces, devices) and the cluster settings, built into a CDC to download and apply                                                                                  |

A CDC's findings come from two places:

- **Operator checks:** the operator's own draft validation, run against a fake API
  server holding the bundle's nodes and simplyblock objects. They are the
  problems the CDC's status reports once it is applied.
- **Bundle checks:** each group read against its workers' probe reports: a
  device the worker does not have, one the probe refused, an NVMe controller a
  userspace driver holds, an interface the worker does not carry, and a worker
  that already runs a storage node of another cluster.

The editor groups workers that share their node set, devices, interfaces, and
failure domain, which is how the API states a fleet. Its stripe choice lists the
supported schemes, and disables those the chosen workers and devices do not carry,
with the operator's reason. The environment is one of the API's values, and only a
Vanilla document offers `enableCpuTopology` and `enableKubeletConfiguration`,
which every other environment decides. In logical block device mode the journal
device offers force as well, which accepts a worker whose smallest devices tie:
the document states `enableJournalDevice` either way, and the editor stops warning. Editing the bundle's own CDC
keeps every field the editor does not show, such as tolerations, images, and KMS.
An approved CDC is immutable, so the editor writes its edit under another name.
A document built here is never approved: approval stays a decision made on the
cluster.

## Sample bundles

`make -C support-tool samples` packs every bundle under `testdata` into
`support-tool/bin/samples/<name>.tar.gz` to open in the tool:

| Bundle                    | What it shows                                                                                                                                                                                |
|---------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `bundle-v10`              | A healthy three-worker NVMe fleet with version-10 reports and its draft                                                                                                                      |
| `bundle-v9`               | The same fleet with version-9 reports, which carry no kernel facts                                                                                                                           |
| `bundle-run-failed`       | A failed run: every disk of the fleet is mounted                                                                                                                                             |
| `bundle-held-controllers` | A failed run: a userspace driver holds every NVMe controller of the worker                                                                                                                   |
| `bundle-probe-trouble`    | A probe that never reported, a report of version 8, a cordoned and a tainted node, readings a probe could not take, a kernel without native multipath, collection errors, and Warning events |
| `bundle-cdc-broken`       | A CDC naming a worker, a device, and interfaces that do not exist, management interfaces with different names, a worker already in use, and a supercluster with a member                     |

## How it is built

The module path is `github.com/simplyblock/simplyblock-operator/support-tool`, so
the tool imports the operator's internal packages directly: the probe report
type, the discovery package's interface choice, and the draft checks
(`deployment.ValidateDocument`). The page is plain HTML and JavaScript embedded in
the binary, with no build step.

| Package                  | Content                                                    |
|--------------------------|------------------------------------------------------------|
| `internal/supportbundle` | Reading the archive into the operator's types              |
| `internal/analysis`      | The view model, and the CDC checks                         |
| `internal/cdcedit`       | The editor's request, the prefill, and building a CDC      |
| `internal/server`        | The HTTP API and the embedded page                         |
| `internal/bundletest`    | Packing the fixture bundles under `testdata` for the tests |

The fixtures under `testdata` are two bundles of the discovery test fleet
`fleet-02`, one with version-10 reports and one with version-9 reports.
