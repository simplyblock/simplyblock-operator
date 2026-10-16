# Design Document: Consistency-Group Backup and Restore

**Status:** Draft  
**Author:** Geoffrey (geoffrey1330)  
**Date:** 2026-10-13  
**Test Plan:** [`tests/test-plan-consistency-group-backup.md`](../tests/test-plan-consistency-group-backup.md)

---

## Phasing Overview

| Phase       | Status  | Scope                                                                                                                                                                                  | Sections           |
|-------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| **Phase 1** | Planned | Control plane: back up the member snapshots of one group generation as one all-or-nothing unit, a `GroupBackup` record, a group manifest in S3, group restore, and the `sbctl` surface | §4, §5, §6, §8     |
| **Phase 2** | Planned | Kubernetes surface: `StorageBackupPolicy` gains a consistency-group source, and `StorageBackupOps{action: Restore}` restores a whole group backup into a new group of claims           | §7, Appendix A/B/C |
| **Phase 3** | Planned | Disaster recovery: export, import, and discover a group backup across clusters, carrying the generation as a unit                                                                      | §6.5               |
| **Phase 4** | Planned | Scheduling and retention: a group-backup schedule and retention that prunes whole generations, not individual members                                                                  | §9                 |

Phase 1 is the control-plane feature and stands alone: it is usable through `sbctl` with no Kubernetes surface. Phase 2 puts the operator's existing data-protection kinds on top of it, extending the single-volume `StorageBackup` family rather than adding a new kind (§7). Phase 3 makes a group backup portable across clusters for disaster recovery, the same way single-volume export/import already is. Phase 4 adds the scheduled, pruned lifecycle.

This design builds on two shipped features and fills the gap between them: single-volume backup to S3 ([`design-storagebackup.md`](crd-redesign/design-storagebackup.md)) and the crash-consistent group snapshot generation ([`design-consistency-groups.md`](design-consistency-groups.md)). Backup of a consistency group is named as future work in [`design-consistency-groups.md`](design-consistency-groups.md) §2, and this document specifies it.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                | Kind                    | Blocks  | Status                                      |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|---------------------------------------------|
| P0-1 | Single-volume S3 backup and recovery: `bdev_lvol_s3_backup` and `bdev_lvol_s3_recovery`, the `Backup` record, and the self-describing `BackupManifest`      | Control plane (`sbcli`) | Phase 1 | Shipped                                     |
| P0-2 | The crash-consistent group generation: `bdev_lvol_snapshot_group` and `create_group_snapshot_for_group`, N member snapshots sharing `(group_id, group_seq)` | Control plane (`sbcli`) | Phase 1 | Shipped (`design-consistency-groups.md` §5) |
| P0-3 | A group-backup control-plane surface: a `GroupBackup` record, a group manifest, the create and restore flows, and the REST and `sbctl` endpoints            | Control plane (`sbcli`) | Phase 1 | **This design**                             |
| P0-4 | The single-volume `StorageBackup` family: `StorageBackupPolicy`, `StorageBackup`, `StorageBackupOps`                                                        | Operator                | Phase 2 | Shipped (`design-storagebackup.md`)         |
| P0-5 | The chain coherence gate: a chain cannot span buckets or encodings, enforced by `BackupChain.assemble`                                                      | Control plane (`sbcli`) | Phase 1 | Shipped (`controllers/backup/chain.py`)     |

P0-1 and P0-2 are the two halves this design joins: the generation gives a crash-consistent set of N member snapshots, and single-volume backup turns each into an S3 copy. P0-3 is the new control-plane work that ties N member backups into one unit with a group manifest and an all-or-nothing commit. P0-5 is why the first version requires homogeneous members (§3.4): the existing chain gate already refuses a chain that mixes buckets or encryption, and a group inherits that constraint per member.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [The Model](#3-the-model)
4. [The Group Backup Record and Manifest](#4-the-group-backup-record-and-manifest)
5. [Creating a Group Backup](#5-creating-a-group-backup)
6. [Restoring a Group Backup](#6-restoring-a-group-backup)
7. [Extending the StorageBackup Family](#7-extending-the-storagebackup-family)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Retention and Scheduling](#9-retention-and-scheduling)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)
13. [Appendix A: `storagebackuppolicy_types.go` additions](#appendix-a-storagebackuppolicy_typesgo-additions)
14. [Appendix B: `storagebackup_types.go` additions](#appendix-b-storagebackup_typesgo-additions)
15. [Appendix C: `storagebackupops_types.go` additions](#appendix-c-storagebackupops_typesgo-additions)
16. [Appendix D: the group manifest schema](#appendix-d-the-group-manifest-schema)

---

## Overview

A consistency-group backup is the member snapshots of one group generation, each backed up to S3, committed as a single unit. The group snapshot already gives the crash-consistent cut: `bdev_lvol_snapshot_group` freezes I/O across every member and takes one snapshot each, and `create_group_snapshot_for_group` records them as N `SnapShot` records sharing a `(group_id, group_seq)` (`design-consistency-groups.md` §5). This design backs up that set as a unit.

The backup of each member reuses the single-volume path unchanged: the same `bdev_lvol_s3_backup`, the same per-volume `Backup` record and `BackupManifest`, the same chain walk. What is new is the unit around them: a `GroupBackup` record that names the N member backups, a group manifest in S3 that lists them as one generation, and a commit barrier that makes the group backup visible only when every member has completed. Restore is the inverse: recreate N logical volumes from their member backups and re-form the consistency group, the shape `clone_generation` already has for snapshots (`design-consistency-groups.md` §7), but reading from S3.

On top of the control-plane feature, the operator extends its single-volume data-protection kinds rather than adding new ones. `StorageBackupPolicy` gains a consistency-group source so a whole group is backed up on a schedule, `StorageBackup` gains a group dimension so the N member copies of one generation are legible as a unit, and `StorageBackupOps{action: Restore}` restores a group backup into a new consistency group of claims, which is the group analog of the restore that produces one claim today (`design-storagebackup.md` §8).

---

## 1. Background

Single-volume backup is shipped and specified. A `Backup` record carries a unique `s3_id`, the source lvol and snapshot ids, a `prev_backup_id` chain link, the S3 `location`, and an encryption `KeyDescriptor` (`simplyblock_core/models/backup.py`). A self-describing `BackupManifest` is written to `manifests/<backup_id>.json` with `schema_version: 1`, naming only the immediate predecessor; the chain is derived by walking `prev_backup_id`, never stored (`controllers/backup/manifest.py`, `controllers/backup/chain.py`). The create task issues `bdev_lvol_s3_backup` and polls, and writes the manifest **before** marking the record `completed`, so a completed backup is always identifiable from the bucket alone (`services/tasks_runner_backup.py`). Restore builds a chain, creates a fresh logical volume with `add_lvol_ha`, and drives `bdev_lvol_s3_recovery`. Export, import, and discover move manifests between clusters, preserving both `uuid` and `s3_id`.

The consistency group is shipped and specified. A `ConsistencyGroup` record pins every member to one logical volume store (`node_id`, `lvs_name`), tracks membership as join and removed sequence epochs, and counts generations in `last_group_seq` (`models/replication.py`). A group snapshot takes one crash-consistent generation across all members in a single frozen SPDK call, records N member `SnapShot` records tagged with `group_id` and `group_seq`, and advances the counter only on full success; `list_generations` reports `expected` membership versus `present` member snapshots so an incomplete generation is visible, not hidden (`controllers/consistency_group_controller.py`, `design-consistency-groups.md` §5, §6, §8).

What is missing is the join: nothing today backs up a group as a unit, and there is no group manifest, no group record, and no all-or-nothing commit across member backups. `design-consistency-groups.md` §2 lists S3 or object backup of a group's generations as future work. This design is that work.

---

## 2. Goals and Non-Goals

### Goals

- Specify a group backup as the member snapshots of one generation backed up as an all-or-nothing unit (§3, §5).
- Specify the `GroupBackup` record and the group manifest that make a generation portable and restorable from S3 alone (§4).
- Specify group restore: recreate N logical volumes and re-form the consistency group (§6).
- Specify how the operator extends `StorageBackupPolicy`, `StorageBackup`, and `StorageBackupOps` to cover a group without a new kind (§7).
- Specify the backend endpoints the operator calls, and flag the ones that do not exist yet (§8).

### Non-Goals

- **Not the per-volume backup mechanism.** How a single member is written to S3, chained, and recovered is the control plane's and is specified in `design-storagebackup.md` §2 and in `controllers/backup/`. This design wraps N of them and does not change one.
- **Not the group snapshot.** `bdev_lvol_snapshot_group` and the generation it produces are `design-consistency-groups.md` §5. A group backup consumes a generation and does not take one differently.
- **Not heterogeneous members in the first version.** A group whose members live in different buckets or have different encryption dispositions is deferred (§3.4, §12 Q1). The first version requires homogeneity, which is what the existing chain gate already enforces per chain.
- **Not retention or scheduling in Phase 1.** Pruning whole generations and running a schedule is Phase 4 (§9), and until then a group backup is taken on demand and kept until deleted.
- **Not the API group's conventions.** The entity and action split, the enum casing, the lock, and the observed generation belong to [`design-crd-model.md`](crd-redesign/design-crd-model.md) and are cited, not restated.

---

## 3. The Model

### 3.1 A group backup is one generation's member backups

A group backup is taken against one group generation. The generation is the crash-consistent cut: N member `SnapShot` records sharing a `(group_id, group_seq)`, taken by `bdev_lvol_snapshot_group` in one frozen call. The group backup backs up each of those member snapshots with the existing single-volume path, and ties the N resulting `Backup` records together under one `GroupBackup`.

Taking the generation and backing it up are two steps, and the design keeps them separate. A caller either names an existing generation to back up, or asks for a fresh generation to be taken first. This mirrors the single-volume split between `backup_snapshot` (an existing snapshot) and `backup_lvol` (take a snapshot, then back it up) in `controllers/backup/controller.py`. Taking a generation is the group snapshot that already exists; this design adds only the backup of it.

### 3.2 The crash-consistency guarantee comes from the generation, not the upload

The member snapshots are a single point in time because `bdev_lvol_snapshot_group` froze I/O across all members to take them. The subsequent uploads to S3 are independent and can finish in any order and over any span of time: each member backup copies its own snapshot, which is already immutable. The group is crash-consistent because the snapshots are, not because the uploads were coordinated. This is why a group backup does not need a second freeze and why member uploads can retry independently.

### 3.3 All-or-nothing commit

A group backup is visible only when every member backup has completed. The barrier is the group manifest (§4.2): it is written to S3 only after all N member manifests exist, and discovery and restore key on the group manifest. Until it is published, a partially uploaded generation is not a restorable group backup, in the same way that a single backup is not `completed` until its own manifest is written (`services/tasks_runner_backup.py`). A member upload that fails is retried; the group backup does not commit until all N have succeeded, and a group backup that cannot complete within its deadline fails as a whole.

This lifts the single-volume invariant "completed implies manifest exists" to the group: "group backup available implies group manifest exists, and the group manifest implies every member manifest exists." The `expected` versus `present` accounting that `list_generations` already does for snapshots (`design-consistency-groups.md` §8.3) is the integrity check the group manifest encodes at commit.

### 3.4 Members are homogeneous in the first version

Every member of a group backup shares one S3 location and one encryption disposition. This is not a new rule; it is the existing chain gate (P0-5) applied across the group. `BackupChain.assemble` already refuses a chain that spans buckets or encodings or mixes encrypted and plain (`controllers/backup/chain.py`), and a group backup is N chains that a restore treats as one unit, so the group inherits the constraint. A group whose members would need different buckets or keys is rejected at creation with the offending members named, rather than silently split.

Heterogeneous members (N independent coherent chains under one group manifest) are a later extension (§12 Q1). The group manifest schema (Appendix D) records a single location at the group level in the first version, which is what homogeneity buys: one place to read, one key disposition to resolve.

### 3.5 Placement follows the group

Every member of a consistency group is pinned to one logical volume store on one node (`design-consistency-groups.md` §4.2). A group backup does not change that, and a group restore re-establishes it: the restored volumes are created on one store and joined to one new group, which is the `clone_generation(into_name=...)` shape (§6). The pinning is why a group restore can re-form a group at all, and why a restore that cannot place all members on one store fails rather than scattering them.

---

## 4. The Group Backup Record and Manifest

### 4.1 The `GroupBackup` record

A new control-plane record, parallel to `Backup`, ties the N member backups of one generation together. It lives beside `Backup` in `simplyblock_core/models/`.

```python
class GroupBackup(BaseModel):
    # Identity: "<cluster_id>/<uuid>", like Backup.get_id().
    uuid: str
    cluster_id: str

    # The source generation this group backup was taken from.
    consistency_group_id: str          # ConsistencyGroup.get_id()
    consistency_group_name: str
    group_seq: int                     # the generation (last_group_seq at take)

    # The member backups, one per member snapshot of the generation. Each entry
    # is a member lvol id and the Backup.get_id() that copied its snapshot.
    members: list                      # [{"lvol_id": ..., "backup_id": ...}, ...]

    # Homogeneous location and encryption disposition (v1): every member shares
    # these. A serialized BackupLocation (never credentials) and a bool.
    location: dict
    encrypted: bool

    size: int                          # sum of member sizes, for reporting
    created_at: str
    completed_at: str
    status: str                        # pending / in_progress / completed / failed / deleting
```

Each member `Backup` gains a back-reference so a member copy can be attributed to its group backup and so the mirror and discovery can group them:

```python
# On the existing Backup model (models/backup.py), additive:
group_backup_id: str = ""              # "" for a standalone single-volume backup
group_seq: int = 0                     # 0 when not part of a group backup
```

`GroupBackup` carries secondary indexes on `uuid`, `consistency_group_id`, and `group_seq`, and `Backup` gains an index on `group_backup_id`, so "the member backups of this group backup" and "the group backups of this group" are point reads, not scans. The member `Backup` records are otherwise ordinary: they chain, retain, and restore per volume exactly as a standalone backup does. The group record adds the unit, not a new per-volume mechanism.

### 4.2 The group manifest

The group manifest is the S3 object that makes a group backup self-describing and is the commit barrier (§3.3). It is written under a new prefix, disjoint from the per-backup `manifests/` keyspace:

```
groups/<group_backup_id>.json
```

Its schema is Appendix D. It records the generation's identity, the N member `backup_id`s, the shared location and encryption disposition, and the membership of the generation (which lvols were expected, so a reader can verify completeness). It names the member backups by id; it does not copy their manifests, which already exist under `manifests/`. A reader resolves a group backup by reading `groups/<id>.json`, then reads each member's `manifests/<backup_id>.json` to assemble that member's chain.

Like the per-backup manifest, the group manifest records encoding and identity, never the bucket, region, endpoint, or credentials: the reader supplies the connection (`controllers/backup/manifest.py` module contract). `manifest.list_all` over the `groups/` prefix is the disaster-recovery entry point for "what group backups are in this bucket," parallel to the per-backup `list_all` over `manifests/`.

### 4.3 Why a group manifest rather than only records

Records live in the control plane's database; manifests live in the bucket. A group backup must be restorable from the bucket alone, for the same reason a single backup is: the cluster that took it may be gone. The per-backup manifests already make each member restorable in isolation, but nothing in the bucket says "these N backups are one generation." The group manifest is that statement, and publishing it last is what makes the group atomic to a reader who only has the bucket.

---

## 5. Creating a Group Backup

The flow, orchestrated by a new `create_group_backup` controller path and a group-aware task runner:

1. **Resolve the generation.** Either an existing `group_seq` named by the caller, or take a fresh one via `create_group_snapshot_for_group` (`design-consistency-groups.md` §5). The result is N member snapshots sharing `(group_id, group_seq)`.
2. **Resolve one location and check homogeneity** (§3.4). Read `cluster.get_backup_config().location()` once; reject if any member would require a different bucket or encryption disposition, naming the offenders.
3. **Create the `GroupBackup` record** with `status=pending` and the member list resolved from the generation. Allocate the group backup UUID.
4. **Back up each member snapshot** with the existing single-volume path (`create_single_backup` over each member's snapshot chain), stamping each member `Backup` with `group_backup_id` and `group_seq`. Each member upload is an independent task that retries on its own, exactly as a standalone backup does; the member's own manifest is written before the member is marked `completed`.
5. **Commit.** When every member backup is `completed`, write `groups/<group_backup_id>.json`, then set the `GroupBackup` to `completed`. If any member cannot complete within the group deadline, the group backup is `failed`; the member backups that did complete remain as ordinary single-volume backups and are not deleted (retention is separate, §9).

The commit step is the group analog of the per-backup "write manifest before completed" invariant: the group manifest is the last write, and a group backup is `completed` only after it exists. A retry of the group task re-checks member completion and re-attempts the commit; writing the group manifest is idempotent onto the same key.

**Incremental across generations.** Nothing special is required. Each member's backup is incremental against that member's previous snapshot through the existing blob-ancestry walk (`ensure_snapshot_chain_backed_up`, `controllers/backup/controller.py`), which already uploads only the delta since the member snapshot's parent. A group backup of generation `N+1` therefore ships only each member's delta since generation `N`, with no group-level diffing: the generation boundary is where the members were frozen together, and the per-member chains carry the deltas.

---

## 6. Restoring a Group Backup

Group restore recreates the generation as a new consistency group, reading from S3. It is the `clone_generation(into_name=...)` shape (`design-consistency-groups.md` §7), but the source is a group backup rather than live snapshots.

1. **Read the group manifest** `groups/<group_backup_id>.json` (or the `GroupBackup` record, when restoring on the cluster that took it). Resolve the shared location and the N member `backup_id`s. Resolve the encryption key disposition before creating anything, as single-volume restore does (`_resolve_crypto_key`).
2. **Require the group restorable.** Every member's chain must be restorable (`BackupChain.require_restorable`), and the generation's membership must be complete (every expected member present). An incomplete group backup is refused at restore with the missing members named, rather than restoring a torn group.
3. **Create N logical volumes** on one store, each from its member backup, reusing single-volume restore (`restore_backup` per member: a fresh `add_lvol_ha` volume, then `bdev_lvol_s3_recovery`). Placement is pinned so the restored volumes can form one group (§3.5).
4. **Re-form the consistency group.** `ensure_group(into_name)` and `add_member_to_group` for each restored volume, the same group-forming path `clone_generation` uses, which enforces single-store placement and fails loudly on an off-store member.
5. **Commit as a unit.** The restore is a group operation: it succeeds when all N volumes are online and joined, and on failure it unwinds the volumes it created so a failed group restore leaves no half-group. This mirrors `VolumeGroupSnapshotOps`, which composes every member or none (`design-consistency-groups.md` §7.4).

The result is a new consistency group of restored volumes, independent of the source group. Restoring into existing claims is out of scope; a group restore always produces a new group, which is the single-volume rule that a restore produces a fresh volume (`design-storagebackup.md` §8) lifted to the group.

### 6.5 Export, import, and discover (Phase 3)

A group backup is portable the way a single backup is. Export emits the group manifest together with the member manifests it names, as one unit. Import preserves ids (`uuid`, `s3_id`, and `group_backup_id`), rehydrates each member chain, writes the member `Backup` records and the `GroupBackup` record, and is all-or-nothing across the group: a group backup imports completely or not at all. Discover over the `groups/` prefix lists the group backups in a bucket for a cluster that has only the bucket. These reuse the single-volume export/import/discover machinery (`controllers/backup/controller.py`) with the group manifest as the unit of transfer.

---

## 7. Extending the StorageBackup Family

The operator covers a group backup by extending its three single-volume data-protection kinds, not by adding a kind. The entity and action categories are unchanged (`design-crd-model.md` §3): a policy and a backup are things that are, and a restore is a thing that happens.

### 7.1 `StorageBackupPolicy` gains a consistency-group source

Today, a policy selects claims with a `claimSelector` and backs each up independently (`design-storagebackup.md` §4.1). A policy gains an optional consistency-group source: when set, the policy backs up the named group as a unit on its schedule, taking one generation per interval and backing it up.

```go
// ConsistencyGroupName names the consistency group this policy backs up as a
// unit, resolved against ClusterRef. The group is named rather than selected by
// label: the membership label's key is constant and only its value, the group
// name, varies, so a label selector would be indirection for a single string.
// Mutually exclusive with ClaimSelector: a policy backs up either independent
// claims or one whole group, not both.
// +optional
// +kubebuilder:validation:MaxLength=63
ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
```

The group is named directly rather than selected by label. The membership label `storage.simplyblock.io/consistency-group` (`design-consistency-groups.md` §3) has a constant key and carries the group name as its value, so the only varying part is the name, and `clusterRef` plus `consistencyGroupName` identifies the group, which is unique per cluster. The operator reconciles the policy against that named group, and `status.phase` reports whether it is active. A policy covers one group, and covering several means several policies. The schedule and retention fields are unchanged and apply per generation (§9). A CEL rule admits at most one of `claimSelector` and `consistencyGroupName`.

### 7.2 `StorageBackup` gains a group dimension

A `StorageBackup` is an observation of one copy in the store (`design-storagebackup.md` §5.1), and that is unchanged: the mirror still creates one `StorageBackup` per member backup copy the control plane reports. What is added is the group identity that ties the N member observations of one generation together, so a group backup is legible as a unit without a new object.

```go
// BackupGroup is the consistency-group generation a backup belongs to, when it
// is a member of a group backup. Empty for a standalone single-volume backup.
// Written once from the store, never updated, like the rest of status.source.
type BackupGroup struct {
	// GroupBackupID is the control-plane GroupBackup this member belongs to.
	// Every member backup of one generation shares it.
	// +optional
	GroupBackupID string `json:"groupBackupID,omitempty"`
	// ConsistencyGroupName is the group the generation was taken from.
	// +optional
	ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
	// Generation is the group sequence (group_seq) of this backup's generation.
	// +optional
	Generation int64 `json:"generation,omitempty"`
	// MemberCount is how many members the generation has, so a reader can tell a
	// complete group backup from a partial one without listing.
	// +optional
	MemberCount int32 `json:"memberCount,omitempty"`
}
```

`status.source.group` carries it, in the source group that is written once and never updated (`design-storagebackup.md` §5.2). A label `storage.simplyblock.io/group-backup-id` on each member `StorageBackup` makes "the members of this group backup" a selector, which is what a group restore addresses (§7.3). No new object represents the group; the group is the set of member `StorageBackup`s sharing the id, the same way a generation is the set of member snapshots sharing `(group_id, group_seq)` in the control plane.

### 7.3 `StorageBackupOps{Restore}` restores a group

A restore operation gains the ability to restore a whole group backup, producing a new consistency group of claims. The action and the kind are unchanged; the target widens.

```go
// GroupBackupID, when set, restores a whole group backup instead of the single
// backup named by BackupRef, producing one PersistentVolumeClaim per member,
// labeled into one restored consistency group. Exactly one of BackupRef and
// GroupBackupID is set (a CEL rule).
// +optional
// +k8s:immutable
GroupBackupID string `json:"groupBackupID,omitempty"`
```

A group restore validates that every member `StorageBackup` of the id is present and restorable, then drives the control-plane group restore (§6), and binds one claim per restored member volume, labeling them with `storage.simplyblock.io/consistency-group` so the restored claims form a group. The steps widen from the single-volume restore (`Validating → Restoring → AwaitingVolume → Binding`) to the group: `Validating → Restoring → AwaitingVolumes → Binding`, where `Restoring` drives N member recoveries and `AwaitingVolumes` waits for all N. The lock is `StorageBackup.status.activeOpsRef` taken on every member of the group, so two group restores of one generation serialize, which is the single-volume lock rule (`design-storagebackup.md` §6) applied across the members.

A group restore cannot be aborted once it has created a logical volume, the same edge the single-volume restore declares (`design-storagebackup.md` §6), and for the same reason: a partially created group must be unwound by the finalizer, not left by an abort.

### 7.4 Reference validation at creation

Every added reference is validated at admission, the group rule (`design-storagebackup.md` §7), with one exception stated below. A `StorageBackupOps` with a `groupBackupID` is rejected when no `StorageBackup` carries that id: the id is immutable, so a group backup that is not there at creation will never be, and the restore's whole purpose is gone. A `StorageBackupPolicy` with a `consistencyGroupName` is rejected when it also sets `claimSelector` (a CEL rule), because the two are alternatives.

The exception is the policy's `consistencyGroupName` itself: it is **not** rejected when no group of that name exists yet. A consistency group is born from its first labeled member volume (`design-consistency-groups.md` §4.1), so a policy that declares how a group is backed up is routinely applied before the group exists — a GitOps bundle that creates the policy and the first member together, in any order, is the common case. Requiring the group at admission would reject exactly that bundle. Instead, the policy reconciler resolves the group by name each pass (`ConsistencyGroupByName`) and holds the policy in `Pending` with a message naming the group it waits for, attaching it the moment the group appears. This is the `api-design` rule that existence is admission's only when the referent cannot legitimately arrive later; here it can, so readiness is the controller's.

---

## 8. Backend API Requirements

The operator calls the control plane; it does not open the bucket (`design-storagebackup.md` §5.1). The single-volume endpoints exist; the group endpoints are new and are the Phase 1 deliverable (P0-3).

| Method & Endpoint                                                                  | Purpose                                                                 | Idempotency / Notes                                                                       | Status  |
|------------------------------------------------------------------------------------|-------------------------------------------------------------------------|-------------------------------------------------------------------------------------------|---------|
| `POST /api/v2/clusters/{id}/consistency-groups/{cg}/backups`                       | Create a group backup of a generation (take one, or name a `group_seq`) | Returns a `group_backup_id`; re-issuing with the same generation is a no-op if one exists | **New** |
| `GET /api/v2/clusters/{id}/group-backups`                                          | List group backups, with member ids and status                          | Read-only; the mirror's stream source                                                     | **New** |
| `GET /api/v2/clusters/{id}/group-backups/{group_backup_id}`                        | One group backup's detail and members                                   | Read-only                                                                                 | **New** |
| `POST /api/v2/clusters/{id}/group-backups/{group_backup_id}/restore`               | Restore a group backup into a new consistency group                     | Returns the restored group id and member lvol ids; all-or-nothing                         | **New** |
| `DELETE /api/v2/clusters/{id}/group-backups/{group_backup_id}`                     | Delete a group backup (the group manifest and the record)               | Deletes the unit; member backups follow retention (§9)                                    | **New** |
| `GET /api/v2/clusters/{id}/group-backups/export?group_backup_id=`                  | Export a group backup's manifests as a unit (Phase 3)                   | Carries the group manifest plus member manifests                                          | **New** |
| `POST /api/v2/clusters/{id}/group-backups/import`                                  | Import a group backup, preserving ids (Phase 3)                         | All-or-nothing; rejects id collisions                                                     | **New** |
| `POST /api/v2/clusters/{id}/group-backups/discover`                                | Discover group backups in a bucket (Phase 3)                            | Reads `groups/` prefix with no cluster reference                                          | **New** |
| `POST /api/v2/clusters/{id}/backups`, `.../backups/restore`, `.../backup-policies` | Single-volume create, restore, policy (reused per member)               | Unchanged                                                                                 | Shipped |

The group endpoints are thin: create resolves a generation and fans out to the single-volume backup path per member, restore fans out to the single-volume restore path per member, and both add the group record, the group manifest, and the commit barrier. No new data-plane RPC is required; `bdev_lvol_snapshot_group`, `bdev_lvol_s3_backup`, and `bdev_lvol_s3_recovery` are all shipped.

---

## 9. Retention and Scheduling

Retention is the control plane's, as it is for single-volume backups (`design-storagebackup.md` §9). A group-backup policy carries `maxVersions` and `maxAge` that the control plane applies **per generation**: pruning a group backup deletes its group manifest and, subject to the member chains, the member backups it alone holds. A member backup that a later generation still chains against is not deleted, which is the existing per-volume chain retention applied under the group. Pruning a whole generation is the unit, so a group's retention count is generations kept, not member copies kept.

Scheduling is Phase 4. Until then a group backup is taken on demand through the create endpoint or `sbctl`, and kept until deleted. The schedule, when it lands, takes one generation per interval and backs it up, which is the single-volume schedule applied to the group source (§7.1).

---

## 10. Observability

### Kubernetes Events

| Reason                  | Object                | When                                                                                               |
|-------------------------|-----------------------|----------------------------------------------------------------------------------------------------|
| `GroupBackupCreated`    | `StorageBackupPolicy` | A policy with a `consistencyGroupName` took a generation and started its backup                    |
| `GroupBackupAvailable`  | `StorageBackup`       | Every member of a generation completed and the group manifest was published                        |
| `GroupBackupIncomplete` | `StorageBackupPolicy` | A generation's backup could not complete within its deadline; the members that completed are named |
| `GroupRestoreStarted`   | `StorageBackupOps`    | A group restore began driving member recoveries                                                    |
| `GroupRestored`         | `StorageBackupOps`    | All N member volumes are online and joined into the restored group                                 |
| `GroupRestoreFailed`    | `StorageBackupOps`    | A member could not be restored; the operation unwound the volumes it created                       |

Events are attached to the object whose status the reader checks: a policy for the taking, a backup for the availability, and the ops for the restore. One reason per condition, not one per member, so a ten-member group does not emit ten events for one outcome; the member detail is in the message.

### Prometheus Metrics

| Metric                                       | Type      | Labels                       | Meaning                                             |
|----------------------------------------------|-----------|------------------------------|-----------------------------------------------------|
| `simplyblock_group_backup_members_total`     | Gauge     | `group_backup_id`, `cluster` | Members expected in a group backup                  |
| `simplyblock_group_backup_members_completed` | Gauge     | `group_backup_id`, `cluster` | Members whose upload has completed                  |
| `simplyblock_group_backup_available`         | Gauge     | `group_backup_id`, `cluster` | 1 when the group manifest is published, else 0      |
| `simplyblock_group_restore_duration_seconds` | Histogram | `cluster`                    | Wall-clock of a group restore to all-members-online |

The completed-versus-total gauges are the `expected` versus `present` accounting (`design-consistency-groups.md` §8.3) exposed for alerting: a group backup stuck below its member count is the signal that one member's upload is wedged, which is exactly the kind of silent stall the single-volume design also watches for.

---

## 11. Testing Strategy

The test plan is [`test-plan-consistency-group-backup.md`](../tests/test-plan-consistency-group-backup.md); this section states what each class of test must prove and where the risk concentrates.

- **Control-plane unit tests** prove the commit barrier and the group record: a group backup is not `completed` and its group manifest is not written until every member backup is `completed`; a member failure holds the group in `in_progress` and then fails it as a whole; the group manifest lists exactly the generation's members; and group restore unwinds on a member failure. These ride the existing backup unit harness (`tests/unit/backup/`, `tests/integration/backup/`) and the consistency-group harness, mocking the data plane. The all-or-nothing barrier is where the risk concentrates, and it is where a mocked member failure must leave no group manifest.
- **Control-plane integration tests** (real FoundationDB, mocked nodes) prove the records round-trip: a `GroupBackup` and its member `Backup`s persist and read back, the indexes resolve "members of a group backup" and "group backups of a group," and import preserves the group ids all-or-nothing.
- **Operator envtest** proves the CRD extensions: a policy with a `consistencyGroupName` resolves its named group and is refused for an unknown name or a `claimSelector` set alongside it; the mirror groups member `StorageBackup`s by `group-backup-id`; a `StorageBackupOps{groupBackupID}` serializes against the per-member lock and cannot abort after creating a volume.
- **Live cluster** proves the end-to-end crash-consistency: back up a group under load, restore it on another cluster, and verify every member restores to the same generation boundary, which is the group analog of the single-volume restore verification and is only provable against real SPDK and real S3.

The regression focus is the barrier and the unwind: a group backup that commits while a member is still uploading, or a group restore that leaves a half-group, are the two failures that would make the feature silently lossy, and both get a red-first regression test.

---

## 12. Open Questions

| #   | Question                                                                                                                                                                                                                                 | Owner          |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------|
| Q1  | Heterogeneous members: when a group spans buckets or encryption dispositions, does the group manifest carry a location per member, or is homogeneity a permanent constraint?                                                             | Control plane  |
| Q2  | Does the mirror create one `StorageBackup` per member (this design), or one `StorageBackup` for the group with members in status? The former keeps "one object per copy"; the latter is fewer objects but a different observation shape. | Operator / API |
| Q3  | Group-backup deadline: a fixed timeout like the single-volume `_DEFAULT_BACKUP_TIMEOUT_SEC`, or a function of member count and size? A twenty-member group is twenty uploads.                                                            | Control plane  |
| Q4  | Restore-into-existing-group: the first version always restores into a new group. Is restoring a generation back over a live group ever wanted, and if so how is it reconciled with the members' current chains?                          | Control plane  |
| Q5  | Partial-generation backup: `list_generations` reports incomplete snapshot generations rather than hiding them. Should a group backup ever be allowed over an incomplete generation, flagged incomplete, or always refused (this design)? | Control plane  |

---

## Appendix A: `storagebackuppolicy_types.go` additions

The base type is `design-storagebackup.md` Appendix A and is unchanged; this is the addition. `ConsistencyGroupName` joins `ClaimSelector` in `StorageBackupPolicySpec`, and the two are mutually exclusive.

```go
// StorageBackupPolicySpec addition (full type in design-storagebackup.md Appendix A).

// ConsistencyGroupName names the consistency group this policy backs up as a
// unit, resolved against ClusterRef. The group is named rather than selected by
// label because the membership label's key is constant and only its value, the
// group name, varies. Mutually exclusive with ClaimSelector.
// +optional
// +kubebuilder:validation:MaxLength=63
ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
```

A named group needs no dynamic attach list in status, so `StorageBackupPolicyStatus` gains no `attachedGroups` field: `status.phase` reports whether the policy is active against the named group. Admission (`StorageBackupPolicyValidator`) rejects a policy that sets both `claimSelector` and `consistencyGroupName`, and one that names a consistency group that does not exist.

## Appendix B: `storagebackup_types.go` additions

The base type is `design-storagebackup.md` Appendix B and is unchanged; this is the addition to `BackupSource`.

```go
// BackupGroup is the consistency-group generation a backup belongs to, when it
// is a member of a group backup. Empty for a standalone backup. Part of
// status.source, written once from the store and never updated.
type BackupGroup struct {
	// GroupBackupID is the control-plane GroupBackup this member belongs to.
	// +optional
	GroupBackupID string `json:"groupBackupID,omitempty"`
	// ConsistencyGroupName is the group the generation was taken from.
	// +optional
	ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`
	// Generation is the group_seq of this backup's generation.
	// +optional
	Generation int64 `json:"generation,omitempty"`
	// MemberCount is how many members the generation has.
	// +optional
	MemberCount int32 `json:"memberCount,omitempty"`
}

// In BackupSource (design-storagebackup.md Appendix B):
//   // Group is the group backup this member belongs to, if any.
//   // +optional
//   Group *BackupGroup `json:"group,omitempty"`
```

Each member `StorageBackup` carries the label `storage.simplyblock.io/group-backup-id` so the members of one group backup are a selector. A `+kubebuilder:printcolumn` surfaces the generation: `Group` from `.status.source.group.consistencyGroupName` and `Generation` from `.status.source.group.generation`.

## Appendix C: `storagebackupops_types.go` additions

The base type is `design-storagebackup.md` Appendix C and is unchanged; this is the addition to `StorageBackupOpsSpec`. The action set and the phase enum are unchanged; `Restore` widens to cover a group.

```go
// StorageBackupOpsSpec addition (full type in design-storagebackup.md Appendix C).

// GroupBackupID restores a whole group backup instead of the single backup named
// by BackupRef, producing one PersistentVolumeClaim per member, labeled into one
// restored consistency group. Exactly one of BackupRef and GroupBackupID is set.
// +optional
// +k8s:immutable
GroupBackupID string `json:"groupBackupID,omitempty"`
```

The restore step machine widens for a group restore: `Validating → Restoring → AwaitingVolumes → Binding`. The lock is `StorageBackup.status.activeOpsRef` taken on every member of the group. Admission (`StorageBackupOpsValidator`) rejects an ops that sets neither or both of `backupRef` and `groupBackupID`, and rejects a `groupBackupID` that no `StorageBackup` carries.

> This appendix states additions to kinds declared in full by `design-storagebackup.md`; it is not itself a standalone kind, so the `api-design` `check-crds.py --design` full-kind audit runs against that document, and these deltas are reviewed against it by reading.

## Appendix D: the group manifest schema

The S3 object at `groups/<group_backup_id>.json`, read by discovery and restore. It records identity, members, and the shared location and encoding; never the bucket, region, endpoint, or credentials, which the reader supplies (the `controllers/backup/manifest.py` contract).

```json
{
  "schema_version": 1,
  "group_backup_id": "<uuid>",
  "created_at": "<iso8601>",
  "completed_at": "<iso8601>",
  "source": {
    "cluster_id": "<uuid>",
    "cluster_name": "<name>",
    "consistency_group_id": "<cluster_id>/<uuid>",
    "consistency_group_name": "<name>",
    "generation": 7
  },
  "members": [
    { "lvol_id": "<uuid>", "lvol_name": "<name>", "backup_id": "<uuid>", "s3_id": 123 }
  ],
  "membership": {
    "expected_lvol_ids": ["<uuid>", "..."]
  },
  "encrypted": false,
  "size": 0
}
```

`members` names the N member backups by `backup_id`; a reader then reads each `manifests/<backup_id>.json` to assemble that member's chain. `membership.expected_lvol_ids` is the generation's membership at the take, so a reader verifies completeness (`members` covers `expected_lvol_ids`) before restore, which is the `expected` versus `present` check (`design-consistency-groups.md` §8.3) encoded in the bucket. `encrypted` is the shared disposition, not a key: each member has its own DEK recorded in its own `manifests/<backup_id>.json`, because the per-member backups carry per-volume keys and there is no group-level key (§3.4). A reader resolves per-member keys before a restore. A `schema_version` the reader does not know is refused, as the per-backup manifest does.
