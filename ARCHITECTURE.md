# simplyblock-operator Architecture Overview

## Purpose
`simplyblock-operator` is a Kubernetes operator that maps custom resources (CRs) to Simplyblock control-plane API operations. It reconciles desired state from Kubernetes into actual storage-system state and writes observed results back into CR status.

## High-Level Architecture
1. The Helm chart (external to this repository) installs the operator and creates a singleton `ControlPlane` CR that gates the rest of the system on control-plane readiness.
2. Users apply Simplyblock CRs in Kubernetes (cluster, node set, pool, backup, restore, import, policy, replication, task, volume migration).
3. Controller-runtime watches those CRs in the operator's namespace and triggers reconcilers.
4. Reconcilers resolve cluster auth from Kubernetes Secrets and call the Simplyblock Web API (`http://simplyblock-webappapi:5000` by default), optionally over mTLS.
5. Reconcilers persist results to CR status fields and Kubernetes objects (Secrets, DaemonSets, Services/EndpointSlices, RBAC, StorageClasses, PodDisruptionBudgets, Jobs).

## Core Components
- `cmd/main.go`
  Starts the controller manager, health/ready probes, metrics endpoint, and registers all reconcilers. Determines the operator namespace from the in-cluster service-account namespace file (`/var/run/secrets/kubernetes.io/serviceaccount/namespace`) and restricts most watches/writes to that namespace. Registers 13 reconcilers: `ControlPlane`, `StorageCluster`, `StorageNodeSet`, `Pool`, `Task`, `NodeDrainCoordinator`, `SnapshotReplication`, `StorageBackup`, `BackupRestore`, `StorageBackupSync`, `BackupPolicy`, `BackupImport`, and `VolumeMigration`.
- `cmd/simplyblock-rebalancer/`
  A standalone binary (built from its own `Dockerfile.simplyblock-rebalancer`, based on Red Hat UBI with `fio`, `nvme-cli`) that is decoupled from controller-runtime. It has three modes:
  - `--mode=baseline`: takes a trimmed-mean set of `fio` NVMe/TCP write-latency samples and writes a `{"p50_ns", "p99_ns"}` JSON result to a termination log (used by a per-node measurement Job).
  - `--mode=probe`: long-running Prometheus exporter that watches a ConfigMap-backed node config and exposes `simplyblock_node_fio_write_latency_p50_ns` / `_p99_ns` gauges (labelled by `cluster`/`node`) on `:9199` (default).
  - `--mode=validate-migration`: connects and validates NVMe-oF paths (from `VMIG_CONNECTIONS`) during a volume migration; run as a Job/container by the `VolumeMigration` controller.
- `api/v1alpha1/*`
  CRD Go types (spec + status) for:
  - `ControlPlane` (singleton, named `simplyblock`)
  - `StorageCluster`
  - `StorageNodeSet`
  - `Pool`
  - `Task`
  - `StorageBackup`
  - `BackupRestore`
  - `BackupImport`
  - `BackupPolicy`
  - `SnapshotReplication`
  - `VolumeMigration`
- `internal/controller/*`
  Reconciliation logic for each CRD plus the cluster-wide `NodeDrainCoordinator` and the `StorageBackupSync` reconciler that mirrors backend backups into CRs.
- `internal/webapi/*`
  Thin HTTP client wrapper used by all reconcilers for REST calls, auth-header injection, and JSON payload transport. Supports plain HTTP and mTLS.
  - Runtime base URL can be overridden via `SIMPLYBLOCK_WEBAPI_BASE_URL` (default `http://simplyblock-webappapi:5000`). API calls target the `/api/v2` surface.
  - Auth is bearer-token based; the token is read from the in-cluster service-account token (`/var/run/secrets/kubernetes.io/serviceaccount/token`).
  - mTLS is enabled via `SB_TLS_SERVE`/`SB_TLS_CONNECT=authenticated`, using client certificates issued by cert-manager.
  - `rebalancing.go` is the volume-migration client (`CreateMigration`, `ContinueMigration`, `CancelMigration`, `GetMigration`/`GetMigrations`), including 409-conflict handling that cancels a pre-existing migration before retrying.
  - `internal/webapi/mock` provides an OpenAPI-aware mock server used by unit tests to validate request paths/methods against `openapi.json`.
- `internal/utils/*`
  Shared helpers for:
  - cluster/pool UUID resolution
  - auth/Secret and cluster lookup
  - API payload structs
  - resource builders (StorageNodeSet DaemonSet, headless Service + EndpointSlice, SPDK proxy Service/EndpointSlice, per-namespace RBAC, namespaced StorageClasses)
  - mTLS / cert-manager `ClusterIssuer` integration (`simplyblock-certificate-authority-issuer`)
  - status/action constants and formatting helpers (e.g. `CSIProvisioner = "csi.simplyblock.io"`)
- `internal/rebalancer/*`
  Shared JSON schema types (`NodeConfig`, `LatencyResult`) exchanged between the operator and the `simplyblock-rebalancer` binary.

## Reconciliation Pattern
Most reconcilers follow the same pattern:
1. Fetch CR.
2. Handle deletion and finalizers.
3. Resolve upstream identifiers and credentials (cluster UUID, pool UUID, secret token).
4. Execute API operation(s) against Simplyblock control plane.
5. Update CR status (and sometimes related K8s resources).
6. Requeue on transient dependency errors or eventually consistent states.

Most controllers project an explicit `running/success/failed` action workflow into status to support long-running operations. The operator watches CRs only within its own namespace.

## Resource Responsibilities
- `ControlPlane` (singleton)
  Reflects readiness of the simplyblock control plane (FoundationDB + management API) via `/api/v2/_meta/ready`. Reconciler ignores any CR whose name is not `simplyblock`; the CR is created automatically by the Helm chart and surfaces a `Phase` (`Initializing` / `Ready`) plus the resolved cluster image (`spec.image`) used for downstream provisioning.
- `StorageCluster`
  Creates/activates/expands clusters, stores cluster UUID and NQN in status, writes cluster secrets (including CSI credentials), and provisions one StorageClass per pool with a unique, namespace-scoped name. Supports `activate`, `expand`, `shutdown`, `start`, `restart`, and `node-recycle` action workflows via `simplyblockstoragecluster_actions.go`. Spec also carries erasure-coding (`stripe`), backup/S3 (`backup`), HashiCorp Vault (`hashicorpVaultSettings`), and volume-migration (`volumeMigrationSettings`, including the rebalancer image) configuration.
- `StorageNodeSet`
  Labels worker nodes, reconciles the storage-node DaemonSet (privileged SPDK pods), the headless Service and EndpointSlice used for NVMe target discovery, and namespaced RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding). Manages TLS certificates via cert-manager, creates and manages storage nodes, exposes node-drain coordination state, and tracks node/action status (`shutdown`, `restart`, `suspend`, `resume`, `remove`). `maxSize` denotes the maximum huge-page allocation for the node; `maxParallelNodeAdds` throttles concurrent (non-FDB) worker-node additions.
- `Pool`
  Creates/deletes storage pools, syncs pool UUID / QoS / `logicalVolumeMaxSize` status fields, configures dhchap security and the host allow-list, and resolves Kubernetes-node-based host affinity for pool placement. Enforces same-namespace cluster references: if the referenced `StorageCluster` is absent from the Pool's namespace, it sets `status.status = "InvalidClusterReference"` and emits an `InvalidClusterReference` Event instead of calling the backend.
- `Task`
  Polls cluster tasks and exposes filtered task state in CR status.
- `StorageBackup`
  Snapshots a referenced PVC's backing volume and creates a backup on the cluster. Imported backups (set via `spec.sourceClusterUUID`) are tracked but not re-created.
- `BackupRestore`
  Restores a `StorageBackup` into the specified cluster/pool/node and creates the resulting PV/PVC, with auto-detection of cross-cluster restore against the source cluster recorded on the backup.
- `BackupImport`
  Imports a backup ID from a source cluster's backend into a target cluster, creating a corresponding `StorageBackup` CR on the target.
- `BackupPolicy`
  Defines retention (`maxVersions`, `maxAge`) and an optional tiered `schedule` for a cluster; the controller merges aged/excess backups according to the policy and attaches it to PVCs via annotation. `clusterName`, `maxVersions`, `maxAge`, and `schedule` are immutable once set.
- `SnapshotReplication`
  Replicates snapshots from a source cluster/pool to a target cluster/pool, including failback support to a fresh source (`action: failback`), with per-volume phase tracking and status `Conditions`.
- `VolumeMigration`
  Migrates a single PersistentVolume's backing logical volume to a target storage node. Resolves cluster/pool/volume UUIDs from the PV's CSI volume handle, launches a validation Job (running the rebalancer in `validate-migration` mode) on the target worker to establish NVMe-oF paths, then drives the backend migration (`create`/`continue`/`cancel`). Status advances through `Pending → Validating → Running → Completed` (or `Failed`/`Aborted`) and supports `spec.abort`.
- `StorageBackupSync` (no CRD)
  Watches `StorageCluster` objects and creates missing `StorageBackup` CRs for backups discovered in the backend, labeled with `storage.simplyblock.io/imported`.
- `NodeDrainCoordinator` (no CRD)
  Coordinates Simplyblock storage-node shutdown and restart during Kubernetes node drain events (e.g., rolling OS upgrades), tracking per-node progress in `StorageNodeSet.status.drainCoordination` (`detected` → `shutdown_called` → `draining` → `restart_called` → `complete`, or `failed`). It detects cordoned nodes, manages per-node `PodDisruptionBudget` objects (`simplyblock-drain-<node>`) to throttle drains, and enforces a `MaxFaultTolerance` gate on how many nodes may drain simultaneously. A self-PDB (`simplyblock-operator-self`) protects the operator pod while it sets up storage PDB protection on the same node.

## External Interfaces
- Kubernetes API:
  Read/write CRs and their status subresources, Secrets, Nodes, DaemonSets, Services/EndpointSlices, ServiceAccounts, ClusterRoles/RoleBindings (per namespace), StorageClasses, PersistentVolumes, PersistentVolumeClaims, PodDisruptionBudgets, Jobs, Events, and cert-manager Certificates.
- Simplyblock Web API:
  Cluster, node, pool, volume, snapshot, backup, restore, replication, migration, device, and task endpoints under `/api/v2`, optionally over mTLS.

## Security Model
- API authentication is bearer-token based; the operator uses its own in-cluster service-account token, and cluster credentials live in Kubernetes Secrets resolved per reconcile loop. User identities are not propagated to the backend.
- mTLS between the operator and the Simplyblock control plane uses certificates issued by a cert-manager `ClusterIssuer` (`simplyblock-certificate-authority-issuer`), gated by `SB_TLS_SERVE`/`SB_TLS_CONNECT`.
- Pool-level dhchap security and host allow-listing are configurable via the `Pool` CRD.
- Controller RBAC is generated in `config/rbac/*` and scoped by controller needs; StorageNodeSet RBAC is created per operator namespace.
- User authorization is delegated entirely to standard Kubernetes RBAC. Two aggregation ClusterRoles ship with the operator: `simplyblock-aggregate-to-view` (aggregates into `view`) and `simplyblock-aggregate-to-edit` (aggregates into `edit`/`admin`). See `README.md` for the full access-control model.

## Operational Endpoints
- Health probe: `--health-probe-bind-address` (default `:8081`)
- Metrics endpoint: configurable secure/insecure binding via manager flags; the default kustomize deployment serves secure metrics on `:8443` and enables `--leader-elect`.
- Leader election: `--leader-elect` (enabled in the shipped manager manifest).
- Rebalancer probe: Prometheus latency gauges on `:9199` (from the standalone `simplyblock-rebalancer` binary).

## Deployment Topology
- Single manager deployment (`config/manager/manager.yaml`) runs all reconcilers in one process and is scoped to its own namespace.
- The Helm chart (external to this repo) installs the operator, creates the singleton `ControlPlane` CR, and configures the cert-manager `ClusterIssuer` used for mTLS. This repository does not contain the chart or the cert-manager `ClusterIssuer`/`Certificate` manifests.
- The kustomize default overlay (`config/default`) enables the CRDs, RBAC, manager, and secure metrics service. The webhook (a `MutatingWebhookConfiguration` for the rebalancer sidecar injector, `simplyblock-rebalancer-injector.simplyblock.io`), the metrics `NetworkPolicy`, and the Prometheus `ServiceMonitor` are defined under `config/` but are disabled by default (and therefore absent from `dist/install.yaml`).
- `make build-installer` renders the full manifest set into `dist/install.yaml`.
- Storage-node data-plane preparation is performed through a reconciled DaemonSet generated from the `StorageNodeSet` CR intent.
- Multi-arch (amd64/arm64) images for the operator and the `simplyblock-rebalancer` are built via `docker-buildx` and pushed to DockerHub, ECR Public, and quay.io. The OLM bundle is generated via `make bundle`, published to quay.io, and attached to GitHub releases. Grype and Trivy vulnerability scanning (plus CycloneDX SBOM generation) run daily in CI (`security.yml`).

## Current Architectural Characteristics
- API-first orchestration: Kubernetes CRs are declarative frontends; actual storage operations are delegated to the external Simplyblock API.
- Status-centric feedback: each controller projects upstream state into CR status for observability.
- Action workflows: most controllers include explicit action state machines in status (`running/success/failed`) to support long-running operations.
- Backup/restore/replication are first-class concerns with cross-cluster import and policy-driven retention.
- Drain awareness: cluster-node lifecycle is coordinated with Kubernetes drains via PDB-based throttling.
- Volume mobility: per-volume migration between storage nodes is coordinated in-cluster, with NVMe/TCP latency measured and validated by the standalone rebalancer.

## Implementation Cycle Findings (2026-04)
These findings are based on the original implementation and hardening cycle captured in Postbrain for this repository. Some specifics have since shifted (notably the addition of the backup/restore/replication and volume-migration surfaces, the rename of the node CRD to `StorageNodeSet`, and the removal of `Device` and `Lvol` CRDs) — treat this section as historical context for the controller patterns in use.

### Action-State Machine Conventions
- Explicit action state machines are implemented in:
  - `StorageCluster` (`activate`, `expand`, `shutdown`, `start`, `restart`, `node-recycle`)
  - `StorageNodeSet` (`shutdown`, `restart`, `suspend`, `resume`, `remove`)
- `Pool` and `Task` reconcile state directly without explicit `running/success/failed` action workflows.

### Reliability and Idempotency Learnings
- Cluster action idempotency is not fully consistent across `activate` and `expand`: success short-circuit logic checks `observedGeneration`, but activate flow handling has been less consistent and was identified as a risk during analysis.
- Finalizer behavior is uneven across controllers: some resources remove finalizers without full upstream cleanup, which can leave backend artifacts orphaned.

### CRD Contract vs Runtime Behavior
- `StorageCluster` previously exposed several update/policy spec fields whose reconcile update flow was inactive; nonexistent fields have since been removed from the CRD, but users should still verify that any new spec field has a corresponding active reconcile path.
- `Task.spec.subtasks` exists but is not consumed by task reconciliation.
- The historical `SimplyBlockDevice` and `SimplyBlockLvol` CRDs were removed; device and lvol state is now observed indirectly via cluster/node/pool reconcilers and the backup/restore surface.

### Ownership and Resource Lifecycle Policy
- Lifecycle ownership is currently partial:
  - DaemonSets created by `StorageNodeSet` set controller references.
  - Secrets and RBAC resources created by controllers do not consistently set owner references.
- Architectural policy direction for this repo is that CR-created children should be owner-linked where Kubernetes scope rules allow it; cluster-scoped RBAC resources need explicit handling because cross-scope ownership constraints apply.

### Testing and Verification Evolution
- Controller-layer tests were expanded from scaffold-level checks to branch-driven reconcile and state-transition tests across StorageCluster, StorageNodeSet, Pool, Task, BackupPolicy, BackupRestore, StorageBackupSync, SnapshotReplication, VolumeMigration, and NodeDrain paths.
- Negative transition-injection coverage was added (stale generation success, success identity mismatch) to verify illegal terminal transitions are rejected.
- `StorageNodeSet` added injectable retry/sleep timing seams (default-preserving) to make long-running action and polling paths testable without changing runtime behavior.
- The mock Web API server validates request paths/methods against `openapi.json` so controller tests catch contract drift.