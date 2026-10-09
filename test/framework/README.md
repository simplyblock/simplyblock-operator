# sbtest — a component/detector framework for large-scale test runs

`sbtest` splits a test run into two kinds of pluggable pieces:

* **Components** do the side-effecting work — drive a workload, sample host paths, follow
  logs, snapshot the fabric. Each can be enabled or disabled independently.
* **Detectors** judge the evidence a run produced. Each is a pure function from `Evidence`
  to findings: no cluster access, no ordering, no shared state.

The split is the point. Every check worth having came out of an incident, and adding one
should not mean touching collection code; conversely, turning streaming log collection on or
off should not disturb a single check.

It drives `kubectl` as a subprocess and depends on nothing but PyYAML, so it runs wherever
the existing harness runs.

> `operator/test/fio_migration_test.py` is untouched and still the way to run the migration
> soak today. This framework is the reusable substrate underneath it — its detector set is
> already a strict superset of that harness's checks, and it can judge every run that
> harness has ever produced.

---

## Quick start

Everything goes through `make`, which creates and populates `.venv` on first use — there is
no separate setup step to remember.

```bash
cd test/framework

make                                    # help, with the parameters listed
make gate                               # quality gate: ruff + mypy + the unit tests

make detectors                          # what can judge a run, and with which knobs
make components                         # what can run during a run
make suites                             # the bundled suites

# judge any existing run directory — no cluster needed
make analyze RUN=../../operator/fio-mig-1787171993
make analyze RUN=<rundir> SUITE=corruption-hunt

# judge every archived run at once: the cheapest check on a new detector
make analyze-all SUITE=corruption-hunt

# collect from a live cluster for 10 minutes, then judge
make collect OUT=./runs/my-run SUITE=migration-soak DURATION=600

# drive a full migration run — volumes, fio, VolumeMigration CRs — then judge it
make run SUITE=migration-full DURATION=7200 KEEP=1

# pNFS under concurrent load: shared and private RWX volumes, fio in every container
make run SUITE=pnfs-fio DURATION=1800 KEEP=1
```

`collect` observes a cluster someone else is loading; `run` creates the load itself. The half
after the components stop is identical in both, which is the point: a live run and an
archived one are judged by the same detectors, so a verdict from either means the same thing.

`SUITE` takes a bundled name or a path to your own file, and is optional everywhere: with no
suite every detector runs on its defaults and no component does, which is exactly what
judging an archive wants.

The venv also installs an `sbtest` entry point, if you would rather not go through make:

```bash
.venv/bin/sbtest analyze <rundir> --suite corruption-hunt --freeze-table
```

### Quality gate

`make gate` runs ruff, mypy and the unit tests, and — deliberately — **runs all three even
when an earlier one fails**, then reports once and exits non-zero. Finding out you had three
problems should not take three round trips; a framework whose own checks are tedious to run
is one nobody runs. `make lint`, `make types` and `make test` are the parts individually.

The only runtime dependency is `pyyaml`, for the suites; `ruff` and `mypy` are dev extras.
Everything runs out of the venv that `make` builds, so the dependency costs nothing — and a
suite is a document to be read and argued with, where every threshold wants a sentence saying
why it is that number. JSON cannot carry that sentence.

`analyze` is the one to reach for first. It runs the whole detector set over an artifact
directory — including every archived `fio-mig-*` run — which means a check can be written or
fixed and tried **immediately against the run that motivated it**, instead of against the
next four-hour run. Not being able to do that was the single biggest gap in the harness this
grew from.

### Regression corpus

`tests/fixtures/runs` holds archived runs that found or confirmed a bug, trimmed to the
evidence the detectors read, and `tests/test_corpus.py` judges each one with the pnfs-fio
suite's settings on every `make gate`. Each test asserts the verdict the run reached and its
key findings by detector, severity, and count: an MDS restart whose clients never returned
to their namespaces, one that lost a pod's open files, and two that recovered. A detector
change that flips a corpus verdict therefore fails the gate, and has to be deliberate: either
the detector now sees something the run hid, which the change should say, or it stopped
seeing something it saw.

To add a run, trim it, judge both the full run and the fixture, and write the fixture's
expectations from what they agree on. The `--outdir` keeps the fixture free of a
`findings.json`:

    python3 tests/fixtures/trim_run.py runs/<run-id> tests/fixtures/runs/<run-id>
    make analyze RUN=runs/<run-id> SUITE=pnfs-fio
    .venv/bin/python -m sbtest analyze tests/fixtures/runs/<run-id> --suite pnfs-fio \
        --outdir /tmp/<run-id>

The trim keeps the run window, the pNFS volume map, the NVMe samples and snapshots, each fio
instance's summary, time series, and NFS counters, and the dmesg within ten minutes of the
run, so a trimmed run should reach the full run's critical and warning findings exactly.
Compare the two before committing the fixture. Only informational findings that count
collected logs may differ.

## It reproduces the findings it was built from

Run against `operator/fio-mig-1787171993` (20 pods, 46 migrations, 4h08m), the detectors
independently derive the result that took a manual investigation to find:

```
ana.freeze-count   mig-20 (2 freezes)  mig-29 (4)  mig-38 (5)  mig-42 (5)
fio.checksum       mig-20 (5 blocks)   mig-29 (4)  mig-38 (2)  mig-42 (2)

subjects with more than one finding:
  mig-20: ana.cutover-pause(CRITICAL); ana.freeze-count(CRITICAL); fio.checksum(CRITICAL)
  mig-29: ana.cutover-pause(CRITICAL); ana.freeze-count(CRITICAL); fio.checksum(CRITICAL)
  mig-38: ana.cutover-pause(CRITICAL); ana.freeze-count(CRITICAL); fio.checksum(CRITICAL)
  mig-42: ana.cutover-pause(CRITICAL); ana.freeze-count(CRITICAL); fio.checksum(CRITICAL)
```

Two unrelated detectors — one reading host ANA samples, one reading fio's own output — land
on the same four subjects out of 46. That correlation is the strongest signal these runs
produce, and surfacing it automatically is why findings carry a `subject`.

The same detector set on the earlier `fio-mig-1787159565` reports a *different* signature
(8 × EREMOTEIO, 3 overlong pauses, 3481 `does-not-allow-host` matches from the path leak, no
freeze-count findings) and correctly reports `nvme.stale-controllers` as **skipped** rather
than clean, because that run has no fabric snapshot.

## The path-loss ladder

dmesg carries something no other source does: when every path to a namespace goes away, the
kernel does not simply fail I/O. It queues, waits, and only fails once a timeout expires —
and each rung is a different amount of damage.

```
all paths inaccessible
  -> block nvmeXnY: no usable path - requeuing I/O     queued; the application waits
  -> nvme nvmeN: failfast expired                      fast_io_fail_tmo elapsed
  -> block nvmeXnY: no available path - failing I/O     errors reach the application
  -> XFS (nvmeXnY): log I/O error -5
  -> XFS (nvmeXnY): Filesystem has been shut down       the volume needs repair
```

**`fast_io_fail_tmo` is the knob that decides which rung a cutover pause reaches.** A pause
shorter than it is absorbed by queueing; a pause longer becomes application-visible errors and
then filesystem damage. That is why `ana.cutover-pause` and `kernel.path-loss` belong in the
same report: one measures the pause, the other says whether the host survived it.

Measured across three archived runs, counting only what happened **inside each run's window**:

| run           | requeued         | `failfast expired` | failing I/O    | filesystems shut down |
|---------------|------------------|--------------------|----------------|-----------------------|
| `-1787159565` | 2                | 18                 | 0              | 0                     |
| `-1787171993` | 163              | 84                 | 0              | 0                     |
| `-1787205545` | 35 (+186 before) | 20 (+118 before)   | 0 (+30 before) | 0 (+20 before)        |

The parenthesised numbers are the reason attribution exists. Read without a window, that last
run looks catastrophic — 30 failed I/Os and 19 filesystems shut down. All of it happened
between 05:46 and 05:48, eleven minutes *before* the run started at 05:59: it is the previous
run's teardown and the cluster reinstall, not this run's doing. (Worth chasing separately —
tearing a cluster down should not kill mounted filesystems — but it is not a migration defect.)

## Attribution: old damage must not fail a new run

dmesg is a ring buffer covering hours and a cluster outlives its runs, so evidence routinely
contains the previous runs' mess. Counting it twice over is a trap: a clean run inherits its
predecessor's failure, and — worse — a genuinely broken run hides inside inherited noise.

Every finding therefore carries an `Attribution`:

| attribution    | meaning                                 | counts against the run?                                          |
|----------------|-----------------------------------------|------------------------------------------------------------------|
| `run`          | happened inside the run's window        | yes                                                              |
| `unknown`      | no usable timestamp, or no known window | **yes** — "I cannot date this" must not become "not our problem" |
| `pre-existing` | positively dated before the run began   | no                                                               |

Severity says *does this matter*; attribution says *whose fault*. The two are orthogonal, so
the same observation is CRITICAL when the run caused it and a hygiene WARNING when it did not.

### What actually forfeits a run

Almost nothing, and the distinction is worth stating because the temptation is to fail on any
inherited mess:

| pre-existing condition                                                                      | affects this run?                                                                                                            | verdict          |
|---------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------|------------------|
| Dead-cluster controllers, old reconnect storms, old fabric errors                           | No — they cannot make a *different* cluster's migration fail                                                                 | hygiene WARNING  |
| A filesystem killed before the run, on a volume the run does not use                        | No                                                                                                                           | hygiene WARNING  |
| **Live-cluster** controllers already `live`-with-no-namespace at setup (`nvme.dirty-start`) | **Yes** — `VerifyMigrationPaths` will reject migrations that should pass, so the completion rate measures the inherited mess | **INCONCLUSIVE** |

That last row is the only thing that produces `INCONCLUSIVE`, and it is a third verdict rather
than a softer FAIL because it calls for a different action: clean the fabric and run again,
rather than go looking through the code. It needs the `nvme.snapshot` component's pre-run
snapshot; without one the detector reports itself **skipped** instead of guessing.

`dmesg --time-format=iso` is collected in preference to `dmesg -T` for exactly this reason: the
ISO form carries a UTC offset, so an event can be placed against the run window soundly rather
than by assuming the host's clock agrees with the harness's.

## Concepts

### Evidence

The only thing a detector may read. Everything is lazy — a run's SPDK logs are tens of MiB
per node and most detectors never open them.

```python
ev.migrations()              -> list[Migration]      # the timeline
ev.ana_samples("mig-20")     -> list[AnaSample]      # host path state, per migration
ev.fio_jobs()                -> list[FioJob]         # per-pod outcome and errno
ev.fio_timeseries(pod)       -> list[IopsSample]     # per-second IOPS
ev.fio_log(pod)              -> Iterator[str]        # where verify failures live
ev.container_logs()          -> list[str]            # "spdk-4420", "operator", ...
ev.container_log(name)       -> Iterator[str]
ev.nvme_controllers()        -> list[NvmeController] # fabric snapshot
ev.nfs_ops(pod)              -> dict[str, int]       # the NFS mount's per-op counts (pNFS)
ev.block_samples()           -> list[BlockSample]    # NVMe namespace I/O counters per node
ev.pnfs_volumes()            -> list[PnfsVolume]     # pNFS volumes and their client nodes
```

Two implementations, kept deliberately close so a check that passes live cannot fail on
replay: `ArchiveEvidence` reads a finished run directory, and `LiveEvidence` is the same
reader pointed at the directory being written, overlaid with in-memory state.

### Findings, and why "skipped" is not "clean"

A detector returns `Finding`s carrying `severity`, `subject`, `detail`, structured
`evidence`, and a `note` explaining what it means. Any CRITICAL fails the run.

A detector whose evidence is **absent** must raise `SkipDetector` rather than return nothing.
Silence makes "could not check" and "checked, all clean" the same output, which is how a
broken check passes a broken run for weeks. Skips are reported separately, by name.

### Component lifecycle

Every hook is optional; override only what applies.

```
setup()      resolve targets, create helpers
start()      begin sampling / following / driving
tick()       periodic, cheap, must not block
stop()       stop doing the thing
collect()    gather artifacts into the run directory
teardown()   release cluster resources — runs even when the run failed
```

`collect` is separate from `stop` for exactly the case that motivated the framework: a
streaming collector has nothing to collect at the end because it has been writing all along,
while a post-run collector does all its work there — and the runner must be able to run
either, or both, without either knowing.

Set `required = True` on a component that *is* the run (a workload, a migration driver): a
failure in its `setup` aborts, because continuing would produce a green result for a test
that never happened. Collectors leave it False, so losing one evidence stream degrades the
run instead of ending it — and the failure is recorded as a WARNING finding.

## Configuration

```yaml
components:
  logs.stream:
    containers: [spdk-container]
    ttl_s: 21600
  logs.collect: true
  ana.sample:
    interval_s: 2.0

detectors:
  ana.freeze-count:
    # More than one freeze in a migration meant lost writes on 5 of 5 archived runs.
    max_freezes: 1
  fio.checksum:
    verify_lag_s: 45
  fio.throughput-outlier: false
```

Two rules make this predictable:

* A name absent from the config is **off for components** and **on for detectors**.
  Collecting is a cost you opt into; judging is not something you should have to remember to
  switch on.
* An unknown name or option is an **error at startup**, with the valid list. A threshold that
  looks set but is not is worse than one that is obviously missing.

Bundled suites live in `sbtest/suites/` (`migration-full`, `migration-soak`,
`corruption-hunt`, `analyze-only`, `pnfs-fio`; `make suites` lists them) and are selected with
`--suite <name>`, which also takes a path to your own file. A `.json` suite still loads, for
anything generating them programmatically. CLI `--enable-detector` / `--disable-component`
layer on top, and disable wins over enable.

### Namespaces

Where things run is a property of the deployment, so a run names three namespaces once, in
the suite's `run:` block or with the matching flag, and every component takes its own from
them:

| Run setting          | Flag                   | What lives there                                                   | Default       |
|----------------------|------------------------|--------------------------------------------------------------------|---------------|
| `operator_namespace` | `--operator-namespace` | the operator, the control plane, the CSI driver, the pNFS MDS      | `simplyblock` |
| `cluster_namespace`  | `--cluster-namespace`  | the storage cluster's own pods: SPDK and the node agents           | `simplyblock` |
| `test_namespace`     | `--test-namespace`     | what the run creates: client pods, migration CRs, log grabber pods | `default`     |

A flag beats the suite, which beats the default. Today, the operator and the cluster share
`simplyblock`. The planned layout moves the operator and control plane to
`simplyblock-system` and keeps the initial cluster in `simplyblock`, which is one setting:
`operator_namespace: simplyblock-system`. Clusters can each be deployed into a namespace of
their own, and a run tests one. A component option naming a namespace still
wins when set, and a `logs.collect` target, `logs.stream`, and `host.dmesg` pick theirs by
`plane` (`cluster` or `operator`).

## Detector catalog

Every one of these came from a real defect. Defaults encode what the runs measured.

| detector                     | what it catches                                                                                                                                                                                                                                                                                                                                                                                    |
|------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `ana.freeze-count`           | **A migration that froze the volume more than once.** Exact predictor of silent write loss so far: 4/4 corrupting vs 0/42 clean. A migration takes the cutover pause once; the rest are retries, and each retry replays a non-idempotent transfer against a source that has been serving writes.                                                                                                   |
| `ana.cutover-pause`          | An all-paths-inaccessible window longer than the design pause (~2s). Complements the count: catches one window that overran, which the count cannot see.                                                                                                                                                                                                                                           |
| `ana.split-brain`            | Source and target both `optimized` at the same instant — two writers, silent corruption by construction.                                                                                                                                                                                                                                                                                           |
| `ana.unserved-after-cutover` | A Completed migration whose live target controller serves only some of the subsystem's namespaces — the half-moved case.                                                                                                                                                                                                                                                                           |
| `ana.path-churn`             | More distinct path addresses per host than the topology should produce. Informational: healthy counts are topology-dependent.                                                                                                                                                                                                                                                                      |
| `fio.checksum`               | **fio read back data it never wrote.** Reads succeeded, so nothing else notices. Attributes to a migration through a verify lag (see below).                                                                                                                                                                                                                                                       |
| `fio.job-error`              | An fio job ended with a non-zero errno, with the errno's meaning — 121/EREMOTEIO points straight at the ANA detectors.                                                                                                                                                                                                                                                                             |
| `fio.outage`                 | A pod's I/O stopped for longer than a cutover should cost — reported as a **freeze** when it came back and a **loss** when it never did. Both fail; only one means writes went missing.                                                                                                                                                                                                            |
| `fio.throughput-outlier`     | A pod far below the run's median IOPS. Weak alone; strong next to an ANA finding on the same subject.                                                                                                                                                                                                                                                                                              |
| `logs.pattern`               | **User-definable regex checks over any collected log.** Ships a catalog: undrained transfer, migration sub-task failure, host-not-allowed reconnect storm, write-to-RO-range, path-validation failure, stuck migration group, kernel reconnect loop.                                                                                                                                               |
| `migration.outcomes`         | Completion rate and phase breakdown.                                                                                                                                                                                                                                                                                                                                                               |
| `migration.errors`           | Distinct migration errors, grouped by shape — 16 identical failures are one defect.                                                                                                                                                                                                                                                                                                                |
| `nvme.stale-controllers`     | Controllers that are live with no namespace (blocks every later migration of that subsystem) or stuck connecting.                                                                                                                                                                                                                                                                                  |
| `nvme.loss-timeout`          | A `ctrl_loss_tmo` long enough that a leaked path outlives the run that made it.                                                                                                                                                                                                                                                                                                                    |
| `kernel.path-loss`           | **How far the kernel got up the path-loss ladder** (see below). Stronger than ANA sampling for the same event: it is what the kernel did, not what a sampler caught, so it cannot miss a window shorter than the interval.                                                                                                                                                                         |
| `kernel.filesystem-shutdown` | XFS/ext4 shut down or went read-only after failed log I/O — the volume needs unmount and repair.                                                                                                                                                                                                                                                                                                   |
| `nvme.foreign-cluster`       | **A controller retrying a subsystem whose cluster no longer exists.** No threshold, no topology: an NQN names its cluster. Hygiene only — it cannot affect the live cluster's migrations.                                                                                                                                                                                                          |
| `nvme.dirty-start`           | The fabric already held blocking debris **for the live cluster** at setup, so the run's results cannot be trusted. The one pre-existing CRITICAL.                                                                                                                                                                                                                                                  |
| `nvme.controller-churn`      | Controllers created vs removed — "they never disappear," counted — plus controllers retrying without ever succeeding.                                                                                                                                                                                                                                                                              |
| `kernel.fabric-errors`       | Connect/reset/timeout errors grouped by kind. Texture around a failure rather than a verdict.                                                                                                                                                                                                                                                                                                      |
| `control.node-flap`          | A node marked down and back within seconds — a liveness check that depended on something other than the node. The shape behind a 9.5h outage.                                                                                                                                                                                                                                                      |
| `control.volume-health`      | A volume or node whose health went false during the run and never returned.                                                                                                                                                                                                                                                                                                                        |
| `control.task-stuck`         | Tasks created and never resolved — "the control plane stopped finishing things."                                                                                                                                                                                                                                                                                                                   |
| `control.retry-storm`        | One operation attempted far more often than it should be; each retry re-does what the last half-did.                                                                                                                                                                                                                                                                                               |
| `control.node-agent`         | The node-side agent returning errors, or **gaps in the liveness polling** — the upstream half of a false offline, visible nowhere else.                                                                                                                                                                                                                                                            |
| `evidence.log-coverage`      | **A collected log that does not span the run**, bounding what every other log-based finding may claim.                                                                                                                                                                                                                                                                                             |
| `evidence.blind-spot`        | A migration no log covers, so it cannot be post-mortemed whatever it did.                                                                                                                                                                                                                                                                                                                          |
| `evidence.inventory`         | What evidence the run produced (INFO).                                                                                                                                                                                                                                                                                                                                                             |
| `evidence.versions`          | What the run started on (INFO): the operator, CSI, MDS, and SPDK images with the digest each resolved to, the nodes' kernels, and the server version, from `run.versions`. Makes two runs comparable without reconstructing what either ran.                                                                                                                                                       |
| `security.secret-exposure`   | Credential-shaped strings in collected logs. Reports the location, never the value.                                                                                                                                                                                                                                                                                                                |
| `pnfs.layout`                | **A pNFS mount that got no layouts**: it ran as plain NFS, every byte through the metadata server, and fio's verification still passed. Warns when data went through the server beside layouts. Reads the NFS client's own per-operation counters.                                                                                                                                                 |
| `pnfs.device-io`             | **A pNFS client node whose NVMe-oF namespace did not see the data.** The other end of the same question: per volume and consuming node, the namespace must be attached and its read and write counters must grow, without standing still longer than `max_stall_s`. A pause that begins at a metadata server restart and stays within `restart_pause_s` is that restart working, reported as INFO. |
| `pnfs.recovery`              | **The time from a metadata server restart back to each client's direct path**: the first sample after the restart's pause that saw a write reach the client's namespace. INFO within `budget_s`, a warning above it or when writes never came back.                                                                                                                                                |
| `chaos.recovery`             | **A pod the run restarted that never came back.** Every restart `chaos.restart` made must end with a Ready replacement. The ones that did are listed as INFO with how long they took.                                                                                                                                                                                                              |
| `pnfs.conntrack-pinned`      | **An NFS flow still sent to the replaced MDS pod after its replacement was Ready**: the client reconnected from its old source port, matched the node's old connection tracking entry, and kept going to the deleted pod's address. INFO when the samples cover a restart and nothing was pinned, which rules the cause out. Reads `nfs.conntrack` against `chaos.restart`'s pod addresses.        |
| `pnfs.fence`                 | **A write that landed after nfsd fenced its client.** From `chaos.fence`: a write by the partitioned node after the recaller's truncate returned and before the heal is critical, and so is a truncate that never returned. A warning when no write failed during the partition, since the run then proved nothing about fencing.                                                                  |
| `pnfs.churn`                 | **A pod that joined a pNFS volume and never did I/O, or an own volume left behind.** Every churn pod must reach fio's timed run. A pod that brought its own volume must leave no PersistentVolume and no NFSExport behind, and a cleanup slower than `delete_budget_s` is a warning.                                                                                                               |
| `pnfs.volume-ops`            | **An expansion or snapshot of a live pNFS volume that did not complete or reach the clients.** An expansion the claim never showed, or that no client's `df` saw, is critical, as are a snapshot that never became ready and a restored marker that differs from the one written before it. A completed operation across which the client's writes stalled is a warning.                           |
| `nvme.stale-reservations`    | **A pNFS namespace still carrying a registration from an earlier MDS boot** at the end of the run, which blocks that client's new key and sends its I/O through the metadata server. Critical, or PRE_EXISTING when the key was stale before the run. Also warns about clients registered on a namespace nobody reserves.                                                                          |

Three things are load-bearing and worth knowing:

* **`fio.checksum` verify lag (45s).** fio detects a lost write when it next *reads* that
  block, 3–34s later in practice. Without the lag a migration's own losses are filed under
  "no migration was running" — which is how a `Completed`-but-corrupting migration hid.
* **`ana.freeze-count` over `ana.cutover-pause`.** The count is more sensitive (two 3s
  freezes look like one healthy pause on any longest-window measure), more specific (one
  migration's single 5–6s pause lost nothing), and robust to sampling granularity. Keep both;
  they catch different shapes, and `tests/test_detectors.py` pins exactly that.
* **The fio clock is fio's own.** Every offset in `timeseries.csv` is milliseconds since
  *that pod's* fio started, so the wall clock hangs off `job_start` from its `result.json`,
  never off the run's start — the run begins minutes before any fio does, by a different
  amount per pod. The base decides which migration an outage overlaps, so getting it wrong
  does not merely shift a chart: it names the wrong migration. `ArchiveEvidence` re-derives
  it on replay, which corrects archives written before this was fixed.

## Component catalog

| component             | what it does                                                                                                                                                                                                                                                                                                                             |
|-----------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `logs.stream`         | Follows chosen container logs for the whole run, surviving kubelet rotation, container restarts, and pod recreation.                                                                                                                                                                                                                     |
| `logs.collect`        | Grabs container logs from each host's `/var/log/pods` at the end. Skips whatever `logs.stream` followed.                                                                                                                                                                                                                                 |
| `host.dmesg`          | `dmesg -T` from each storage worker.                                                                                                                                                                                                                                                                                                     |
| `cluster.events`      | `sbctl cluster get-logs` → `cluster-events.json`.                                                                                                                                                                                                                                                                                        |
| `nvme.snapshot`       | Fabric snapshot before *and* after the run — "did the last run leave a mess?" is a real question, because a leaked controller breaks the *next* run.                                                                                                                                                                                     |
| `ana.sample`          | Per-namespace ANA state on every consuming node, on an interval, written per migration in the layout `ArchiveEvidence` reads. Needs a driver to tell it which migration is in flight.                                                                                                                                                    |
| `workload.fio`        | Provisions volumes from two StorageClasses (single-namespace and packed) and drives continuous md5-verified fio against them. `required`.                                                                                                                                                                                                |
| `migration.driver`    | Creates `VolumeMigration` CRs in a loop, one at a time, and records what each one did. `required`.                                                                                                                                                                                                                                       |
| `workload.pnfs`       | Provisions pNFS volumes some pods share and some own, and runs verified fio in every container, each instance with a data file of its own. Pods sharing a volume are spread across nodes. `required`.                                                                                                                                    |
| `nvme.iostat`         | Samples every node's NVMe namespace I/O counters (sysfs `stat`, head devices only) on an interval, for `pnfs.device-io`. Finds a replaced node plugin and keeps reading through it.                                                                                                                                                      |
| `chaos.restart`       | Restarts the metadata server, a pNFS client's node plugin, or the CSI controller during the timed run: `guaranteed` times at seeded random moments, and otherwise with a low `chance` per tick. Records each restart and its recovery in `restarts.json`, and the victim's log through its shutdown in `restart-<n>-<target>-<pod>.txt`. |
| `chaos.volume-ops`    | Expands a shared pNFS claim and snapshots one during the timed run, at seeded times: the expansion is followed to the claim's status and a client's `df`, and the snapshot is restored into a block XFS volume whose marker file is read back. Records each operation in `volume-ops.json`.                                              |
| `chaos.fence`         | Once per run, at a seeded time, cuts one node off from the metadata server (port 2049 only) while a probe there writes with a layout, and has a probe on another node truncate the file, which makes nfsd recall the layout and fence the node. Records the rules, the truncate, and every probe write in `fence.json`.                  |
| `workload.pnfs-churn` | Keeps short-lived pods joining and leaving pNFS volumes during the run, on a seeded schedule: some join a volume `workload.pnfs` shares, some bring a volume of their own and delete it when they leave. Each runs a verified fio for its lifetime and is recorded in `churn.json`.                                                      |
| `nvme.reservations`   | Records every namespace's NVMe reservation (holder, registrants, keys) through `nvme resv-report` on each node, before and after the run, for `nvme.stale-reservations`. Only the pNFS volumes' namespaces once the workload has mapped them.                                                                                            |
| `nfs.mountstats`      | Samples each pNFS fio pod's NFS client counters (LAYOUTGET, READ, WRITE, transport connects) on an interval, per instance, into `nfs-timeline.csv`, so `pnfs.layout` can say when data went through the server.                                                                                                                          |
| `nfs.conntrack`       | Samples each node's NFS flows in its connection tracking table (the client's tuple and the pod it was translated to) on an interval, through a host-network helper per node, into `conntrack.csv`, for `pnfs.conntrack-pinned`.                                                                                                          |
| `run.versions`        | Records at setup every container image and resolved digest in the operator and cluster namespaces, every node's kernel, OS image, runtime, and kubelet, and the server version, in `versions.json`.                                                                                                                                      |

### What gets collected

| artifact                        | source                      | why per-what                                                                                                                                                                                                                 |
|---------------------------------|-----------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spdk-<port>.txt`               | storage-node SPDK container | **must be streamed.** Measured on vm04: a rotation every ~2 min, so the whole 50 MiB budget bought about **10 minutes** of retention. A migration was unrecoverable 6 seconds after it ended                                 |
| `spdk-<port>-proxy.txt`         | the SPDK JSON-RPC proxy     | streamed too, but far lower volume and survives hours — so it is the fallback when the SPDK log is gone. It carries the RPC-level narrative (which calls, in what order, with what arguments) without SPDK's internal errors |
| `snode-api-<node>.txt`          | storage-node DaemonSet      | per node — it starts and probes SPDK, so it is on the causal path of every node-offline decision                                                                                                                             |
| `csi-node-<node>.txt`           | CSI node plugin             | per node — the plugin reconciles per host, so "which node" is the first question about anything it did                                                                                                                       |
| `<container>.txt`               | tasks pod, csi-controller   | **per container.** The tasks pod runs seventeen independent runners; merging them gives a 50 MiB file that is not in time order, so its time span is meaningless and a pattern cannot be scoped to one runner                |
| `operator.txt`, `webappapi.txt` | control plane               | single-container, so per pod is fine                                                                                                                                                                                         |
| `dmesg-<node>.txt`              | each storage worker         | ISO-timestamped, see the attribution section                                                                                                                                                                                 |
| `cluster-events.json`           | `sbctl cluster get-logs`    | the control plane's own account                                                                                                                                                                                              |

### Why both log components exist

The kubelet keeps only `containerLogMaxSize × containerLogMaxFiles` per container — 10Mi × 5
= **50 MiB** on the clusters this runs against. Measured SPDK write rates were 0.28–1.23
MiB/min, so 50 MiB buys 41–176 minutes. A post-run grab of a four-hour run therefore returns
its tail and silently drops the rest; that is how several runs' worth of early evidence was
lost. So the high-volume logs are followed live and everything else is grabbed at the end,
where a one-shot grab is still complete.

Worth knowing which of the two to reach for: when the SPDK log has aged out, the proxy log on
the *same node* usually still has the RPC sequence — on one run it recovered the full
freeze/revert/retry pattern of a corrupting migration whose SPDK log was already gone, and its
count of the retries was more accurate than the ANA sampler's.

`logs.stream` handles three separate ways a log moves out from under a follower, because any
one of them leaves the stream *silent rather than failed*: rotation (same filename reopened —
`tail -F` handles it), a container restart (`<n+1>.log` beside the old one — the target is
re-resolved on a timer), and a pod recreation (a whole new UID directory — same re-resolve).

## Adding a detector

```python
from sbtest.core import Detector, SkipDetector, critical, detector

@detector
class MyCheck(Detector):
    name = "mine.my-check"                       # dotted, stable; config keys off it
    summary = "one line for `sbtest detectors`"

    def defaults(self) -> dict:
        return {"threshold": 3}                  # also the option allow-list

    def detect(self, ev):
        migs = ev.migrations()
        if not migs:
            raise SkipDetector("no migrations in this run")   # not the same as clean
        for m in migs:
            if len(m.members) > self.opt("threshold"):
                yield critical(self.name, title="subsystem too wide", subject=m.name,
                               evidence={"members": len(m.members)})
```

Import it from `sbtest/detectors/__init__.py` and it appears in `sbtest detectors`, is
configurable by name, and runs against every archived run. Unit-test it against
`FakeEvidence` in `tests/test_detectors.py` — no cluster.

## Adding a component

```python
from sbtest.core import Component, component

@component
class MyCollector(Component):
    name = "mine.collector"
    summary = "one line for `sbtest components`"
    required = False                             # True only if this component *is* the run

    def defaults(self) -> dict:
        return {"namespace": "default"}

    def setup(self, ctx): ...                    # resolve targets
    def collect(self, ctx):
        with open(ctx.path("my-artifact.txt"), "w") as fh:
            fh.write("...")
    def teardown(self, ctx): ...                 # always runs
```

Write artifacts through `ctx.path(...)` so they land in the run directory in the layout
`ArchiveEvidence` expects — that is what makes them replayable. Record observations with
`ctx.timeline.record("kind", subject=..., **data)` so detectors can read them back without
knowing which component produced them.

## Adding a workload

A workload is a subclass of `workloads/base.py::FioWorkload` in its own module under
`components/workloads/`, imported from that package's `__init__.py`. It writes only what makes
it specific: `documents()` returns the claims and pods, recording every fio instance it plans
in `self._instances` (one per container, each with its own data file and evidence directory),
and `after_running()` / `after_collect()` read back what the cluster decided and anything
beyond fio's own evidence. Applying, waiting, collecting each instance and cleaning up are the
base's, so every workload leaves evidence the fio detectors already read.

```python
@component
class MyWorkload(FioWorkload):
    name = "workload.mine"
    summary = "one line for `sbtest components`"

    def workload_defaults(self) -> dict:
        return {"pods": 3}

    def documents(self, ctx):
        ...   # claims and pods; append a FioInstance per container to self._instances
```

## Layout

```
test/framework/
  sbtest/
    core/        evidence, findings, plugin registry, config, context, runner
    components/  logs (stream + collect), nvme (ANA and I/O samplers + snapshot), events,
                 nfs (mountstats), kube
      workloads/ base (the FioWorkload every workload extends), fio (command line,
                 script, per-instance evidence), volumemigration, pnfs_rwx
    detectors/   ana, control, fio, kernel, logs, meta, migration, nvme, pnfs, security
    adapters/    archive (finished run dir), live (run in progress)
    suites/      migration-full, migration-soak, corruption-hunt, analyze-only, pnfs-fio
                 (YAML)
    cli.py
  tests/         detector and core tests — 108, no cluster required
  Makefile       bootstraps .venv; every target depends on it
  pyproject.toml packaging, ruff and mypy configuration
```

Note on lint config: `PTH` (pathlib-over-`os.path`) is deliberately **not** selected.
`operator/test/fio_migration_test.py` and everything around it use `os.path`, and one module
in a different idiom is worse than a consistent old one.

## Driving a run

`workload.fio` and `migration.driver` are the two components that *are* the run rather than
observers of it, and `migration-full` is the suite that wires them into the configuration the
corruption work used:

```bash
make run SUITE=migration-full DURATION=7200 KEEP=1
```

`DURATION` is also fio's runtime. A workload waits for every fio instance to finish laying
out its file and enter the timed run before the clock starts, since fio's runtime does not
count the layout, and at the end it waits for fio to finish that runtime on its own, plus 180
seconds of grace, before collecting. That is when fio writes its summary and runs its final
verification, and what `operator/test/fio_migration_test.py` did. Only an instance still running
after the grace is interrupted, and named. A suite sets `runtime_s` only to make fio run for
a different time than the run.

Both declare `required = True`, so a failure in their setup aborts the run instead of being
recorded as a warning. That distinction is the whole reason the flag exists: a run whose
workload never came up would otherwise sail through every detector — nothing to find, nothing
found — and report a clean pass for a test that never happened.

Three things the driver reads from the backend instead of assuming, each because assuming it
produced a wrong answer that *looked* right:

* **Where the volume is now.** The cluster's own rebalancer moves volumes with no Kubernetes
  object changing, so a source cached at setup is stale by the tenth migration — and a stale
  source means picking a target the volume already lives on, which the operator rejects and
  which reads as a product bug.
* **Which volumes move together.** A migration moves the whole NVMe subsystem, so its blast
  radius is the volume's *group*. The control plane decides the packing and a previous
  migration can change it, so the group is re-read before each one.
* **What the migration actually did.** `status.sourceNodeUUID` is the operator's own resolved
  answer, so it overwrites the driver's guess in the record.

The driver also feeds `ana.sample`: `begin()` when a migration starts, `set_phase()` on each
phase transition, `end()` when it finishes. Sampling is worthless unattributed — a freeze
window is per migration — and the phase stamp is what makes a transition readable as "all
paths went inaccessible during Cutover" instead of "at 09:31:02". The handshake goes through
`ctx.shared`, so the sampler stays optional: with it disabled the driver still migrates and
the ANA detectors report themselves *skipped* rather than clean.

Target selection is deliberate rather than random. The `target_policy` controls whether the
target node *also* hosts a pod consuming the subsystem being moved — the materially harder
case, since that host has to join a subsystem on the very node becoming its target.
`alternate` puts `consumer` first, so a run cut short still exercised it. When the policy
cannot be honoured the driver falls back to any other node and records the policy as
`consumer(unmet)`: the migration is still evidence, but the run's own record must not claim a
case it never reached.

`workload.fio` enables md5 verification only when it can be trusted. fio's verify races
itself when two in-flight I/Os touch the same block; with one job that is fixable
(`--serialize_overlap`), across processes it is not without `io_submit_mode=offload`. So
`numjobs > 1` turns verification off and says so loudly — a run that measures throughput is
not a run that would have noticed data loss, and the report must not imply otherwise.

## Trying it against a cluster

`sbtest collect <outdir> --suite migration-soak --duration 90 --judge` exercises the
observation half of the lifecycle on its own: grabber pods, live following, pre/post fabric
snapshots, control-plane events, dmesg, then judging. Useful for checking that collection
works before committing a long run to it, and for watching a cluster someone else is loading.

Doing that for the first time found two bugs no amount of archive replay could have:

* **Two components wanting a grabber on one node collided.** A Pod is immutable, so the second
  to apply the same name failed on a field it may not change — and `logs.collect` silently
  produced empty files for every node `logs.stream` had already claimed. The pod name now
  carries the component.
* **A live run recorded no window**, so nothing could be dated, so every ring-buffer finding
  was `unknown` — which counts — and the run failed on twenty-nine filesystem shutdowns from
  hours earlier. The runner now writes `run.json`, which `ArchiveEvidence` prefers over
  inferring a window from whatever happened to get logged.

Both have regression tests. The lesson generalises: the detectors were verifiable offline, the
components were not, and only the components had these bugs.

## pNFS under concurrent load

`workload.pnfs` runs fio against pNFS volumes the way a ReadWriteMany user does: several pods
on several nodes reading and writing one filesystem, beside pods with a volume of their own.

Every fio *instance* (one per container) writes and verifies a data file of its own, named
after the run, pod and container. fio's md5 verification assumes nothing else writes the
blocks it checks, so instances sharing a file would report each other's writes as corruption.
With one file each, the instances on a shared volume meet only in the filesystem: concurrent
allocation, layouts and commits through one metadata server. That is the multi-writer case
pNFS has to get right, and every verify failure still means what it says. Each instance gets
its own evidence directory (`<run>-fio-<pod>-c<k>`), so the fio detectors judge it like any
other.

A passing fio is not enough for pNFS, because plain NFS passes it too: the data is right
whether or not it bypassed the server. Two detectors read the evidence that tells the
difference, one from each end of the data path. `pnfs.layout` reads the client mount's own
counters (were layouts issued, did data go through the server anyway), and `pnfs.device-io`
reads the client node's NVMe counters (did the volume's namespace on that node see the reads
and writes, all run long). A client whose namespace stayed flat while fio ran sent its data
through the metadata server.

`nfs.mountstats` adds the NFS side over time. With its timeline, `pnfs.layout` reports from
when to when data went through the server instead of only that it did, and `pnfs.recovery`
measures, per client and per metadata server restart, how long the client took to write to
its own namespace again.

Shared volumes prefer one pod per node rather than requiring it, so the suite runs on a small
cluster too. The setup log says when every pod sharing a volume landed on one node, since such
a volume exercises one NFS client rather than several.

### Restarts during the run

`chaos.restart` restarts what the data path depends on while fio runs, because the moments
that break volumes are rarely the steady state. The metadata server's guest boots cold and
its clients have to reclaim, a node plugin comes back knowing nothing of the mounts it
staged, and the controller restarts mid-reconcile. The suite asks for at least one restart
per run (`guaranteed`) and a low chance of more on every tick (`chance`), with the metadata
server weighted highest. A second restart while one is in flight happens only with
`overlap_chance`, and nothing restarts in the last `quiet_tail_s`, so each recovery is
observed before fio ends. The log names the seed, and `seed:` replays the schedule.

What each restart should cost differs, and the detectors judge it that way. A metadata
server restart pauses every client until its grace period ends, so a pause within
`restart_pause_s` that begins at one is INFO. A node plugin or controller restart is not on
the data path, and any pause around one is a finding. Writes that never resume are critical
whatever was restarted, and `chaos.recovery` fails a restart whose pod never came back.

### Pods joining and leaving

`workload.pnfs-churn` keeps a flow of short-lived pods coming and going while the long-lived
pods run, which is how a ReadWriteMany volume is used day to day. A churn pod joins one of
the volumes `workload.pnfs` shares (`reuse_ratio`), which is a new NFS client taking layouts
on a volume already in use, or brings a pNFS volume of its own, which is a whole export
created and torn down under load. Arrivals follow a seeded Poisson process
(`mean_interval_s`), each pod lives `min_life_s` to `max_life_s`, an arrival that finds
`max_concurrent` pods running waits for room, and nothing arrives in the last
`quiet_tail_s`. The log names the seed, and `seed:` replays the flow.

Each churn pod runs one md5-verified fio on a file of its own, collected before the pod is
deleted, so `fio.checksum` and `fio.job-error` judge it like any other instance.
`churn.json` records when each pod arrived, reached I/O, finished, and left, and for an own
volume whether its PersistentVolume and NFSExport were gone afterward, which `pnfs.churn`
judges.

### Fencing a client

A pNFS client cut off from the metadata server keeps its NVMe-oF paths, so once nfsd has
given up on recalling its layout, the reservation is the only thing that stops its writes.
`chaos.fence` stages that once per run, at a seeded time that leaves room for the whole
scenario before `quiet_tail_s`. A probe on the victim node writes 4 KiB with O_DIRECT through
one open file descriptor every `write_interval_s` and logs each result. Once those writes go
direct, a privileged hostNetwork helper on the same node drops TCP port 2049 to and from
the metadata server's addresses, and a probe on another node truncates the file to
`grow_mb`. nfsd recalls the layout, cannot reach the victim, fences it after two lease
periods, and only then completes the truncate, which is bounded by `truncate_timeout_s`.
The victim keeps writing for `after_s`, then the partition heals. The victim is a node
where no workload pod mounts a volume, because every pNFS mount on it loses the server too.
`allow_busy_victim` lifts that restriction. Teardown removes exactly the rules it recorded, the helper
removes them when it is terminated or after `max_partition_s`, and a rule that cannot be
removed is logged as an error with the command that removes it. `pnfs.fence` reads
`fence.json`: a write that succeeded between the truncate's return and the heal is critical.

### Reservations

The metadata server holds an NVMe reservation on each export's namespace, and each client
registers the key nfsd hands it, whose upper 32 bits are nfsd's boot time. On NVMe a
client's registration outlives the server that issued it, so after a metadata server restart
the old registration blocks the new key, and the client's I/O goes through the metadata
server without a word anywhere. `nvme.reservations` records every namespace's registrants
through `nvme resv-report`, before the run and after it, and `nvme.stale-reservations`
fails a pNFS namespace that still carries a key from an earlier boot at the end. A key is
from an earlier boot when it is older than the newest client key on the namespace, or older
than an MDS restart the run recorded, since after a restart no client may have registered
anew.

## Not yet here

Deliberate gaps, so nobody looks for them:

* **No snapshots during the run.** `operator/test/fio_migration_test.py` can take a
  `VolumeSnapshot` before a migration and re-check afterwards that the backend snapshot still
  resolves (`--snapshot-chance`). Not ported: it is a second dimension on top of migration, and
  the corruption work ran with it at 0.
* **The driver migrates one volume at a time, in round-robin order.** Deliberate — concurrent
  migrations of different subsystems make every host-side observation ambiguous about which one
  caused it — but it does mean the concurrent case is untested by this framework.
* **No fault injection.** Killing a node mid-cutover, partitioning the fabric, or filling a
  volume are the obvious next scenarios, and nothing here does them yet.
* **No role labelling in `ana.sample` output.** `ana.split-brain` and
  `ana.unserved-after-cutover` need source/target roles per listener and skip without them.
  Left open on purpose rather than half-done: the driver now knows the source and target node
  UUIDs, and `sbctl storage-node list` gives each node's management IP, so labelling by IP is
  easy — and wrong. One host serves both sides of a migration on different ports, so an IP
  names a *node* while only `ip:port` names a *path*. The existing harness labelled by IP and
  read a third-party replica's listener as the source's, which inverted a cutover/revert
  conclusion in one review. What is actually missing is the port → lvstore mapping; until that
  is available, these two detectors correctly skip instead of guessing.
* **The phase stamp is best-effort.** `set_phase` records the last phase the driver *saw*,
  polling every few seconds, so a phase shorter than the poll interval never lands on a sample.
  The ANA transitions themselves are sampled independently and are unaffected; only the label
  is coarse.