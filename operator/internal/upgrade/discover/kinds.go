// Which kinds the migration reads, and why each one is in the list.
//
// The set is not every CRD the group serves. It is the kinds §7.2 changes, the
// kinds §16 retires or absorbs, and the objects those two sets own or derive a
// name from. A kind nothing in the migration reads is not discovered, because
// listing it costs an API call per namespace and buys nothing.

package discover

import (
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the discoverers, so a kind that reads another's objects
// names it rather than relying on the order of the list below.
const (
	IDStorageClusters   upgrade.ID = "discover-storage-clusters"
	IDStorageClusterOps upgrade.ID = "discover-storage-cluster-ops"
	IDStorageNodeSets   upgrade.ID = "discover-storage-node-sets"
	IDStorageNodes      upgrade.ID = "discover-storage-nodes"
	IDStorageNodeOps    upgrade.ID = "discover-storage-node-ops"
	IDStoragePools      upgrade.ID = "discover-storage-pools"
	IDControlPlanes     upgrade.ID = "discover-control-planes"
	IDStorageBackups    upgrade.ID = "discover-storage-backups"
	IDBackupPolicies    upgrade.ID = "discover-backup-policies"
	IDBackupRestores    upgrade.ID = "discover-backup-restores"
	IDBackupImports     upgrade.ID = "discover-backup-imports"
	IDVolumeMigrations  upgrade.ID = "discover-volume-migrations"
	IDStorageClasses    upgrade.ID = "discover-storage-classes"
	IDNamespaces        upgrade.ID = "discover-namespaces"
	IDPersistentVolumes upgrade.ID = "discover-persistent-volumes"

	// The two read across every namespace rather than inside the installation.
	IDClustersEverywhere   upgrade.ID = "discover-storage-clusters-everywhere"
	IDMigrationsEverywhere upgrade.ID = "discover-volume-migrations-everywhere"
)

// SimplyblockKinds are the custom resources the migration reads.
//
// Seven of them are the converting kinds of §7.2, whose objects the storage
// rewrite touches and whose names feed the derivations of §19. Five are the
// kinds §16.2 renames, absorbs, or retires. The four replication kinds and
// Task are absent: §7.2 leaves them at v1alpha1 untouched, so nothing in this
// migration has a question to ask about them.
func SimplyblockKinds() []upgrade.Discoverer {
	return []upgrade.Discoverer{
		Kind{
			RuleID:     IDStorageClusters,
			Summary:    "reads the StorageCluster objects the installation holds",
			List:       &simplyblockv1alpha1.StorageClusterList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDStorageClusterOps,
			Summary:    "reads the StorageClusterOps objects, so an operation in flight can refuse the migration",
			List:       &simplyblockv1alpha1.StorageClusterOpsList{},
			Namespaced: true,
			Needs:      []upgrade.ID{IDStorageClusters},
		},
		Kind{
			RuleID:     IDStorageNodeSets,
			Summary:    "reads the StorageNodeSet objects §16.1 retires, and the per-node configuration they are the source of truth for",
			List:       &simplyblockv1alpha1.StorageNodeSetList{},
			Namespaced: true,
			Needs:      []upgrade.ID{IDStorageClusters},
		},
		Kind{
			RuleID:     IDStorageNodes,
			Summary:    "reads the StorageNode objects §20 reparents onto their cluster",
			List:       &simplyblockv1alpha1.StorageNodeList{},
			Namespaced: true,
			Needs:      []upgrade.ID{IDStorageNodeSets},
		},
		Kind{
			RuleID:     IDStorageNodeOps,
			Summary:    "reads the StorageNodeOps objects, so an operation in flight can refuse the migration",
			List:       &simplyblockv1alpha1.StorageNodeOpsList{},
			Namespaced: true,
			Needs:      []upgrade.ID{IDStorageNodes},
		},
		Kind{
			RuleID:     IDStoragePools,
			Summary:    "reads the StoragePool objects, whose names derive a StorageClass name and a node label key",
			List:       &simplyblockv1alpha1.StoragePoolList{},
			Namespaced: true,
			Needs:      []upgrade.ID{IDStorageClusters},
		},
		Kind{
			RuleID:     IDControlPlanes,
			Summary:    "reads the ControlPlane objects that adopt the control-plane workload",
			List:       &simplyblockv1alpha1.ControlPlaneList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDStorageBackups,
			Summary:    "reads the StorageBackup objects whose status §7.2 regroups",
			List:       &simplyblockv1alpha1.StorageBackupList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDBackupPolicies,
			Summary:    "reads the BackupPolicy objects §16.2 copies to StorageBackupPolicy",
			List:       &simplyblockv1alpha1.BackupPolicyList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDBackupRestores,
			Summary:    "reads the BackupRestore objects §16.2 absorbs into StorageBackupOps",
			List:       &simplyblockv1alpha1.BackupRestoreList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDBackupImports,
			Summary:    "reads the BackupImport objects §16.2 retires",
			List:       &simplyblockv1alpha1.BackupImportList{},
			Namespaced: true,
		},
		Kind{
			RuleID:     IDVolumeMigrations,
			Summary:    "reads the VolumeMigration objects §16.2 absorbs into the cluster-scoped PersistentVolumeOps",
			List:       &simplyblockv1alpha1.VolumeMigrationList{},
			Namespaced: true,
		},
	}
}

// CoreKinds are the Kubernetes objects the migration reads that belong to no
// simplyblock API version.
//
// Each is here because a check needs it rather than because it is nearby. The
// namespaces are what §19.2's derived names are built from and what §19.10's
// fifth check compares across. The StorageClasses are what a pool derives and
// what a collision would already have produced. The PersistentVolumes carry
// the volume handles of §16.4.
func CoreKinds() []upgrade.Discoverer {
	return []upgrade.Discoverer{
		Kind{
			RuleID:  IDNamespaces,
			Summary: "reads the namespaces a derived name is built from, and that a cluster-scoped kind loses",
			List:    &corev1.NamespaceList{},
		},
		Kind{
			RuleID:  IDStorageClasses,
			Summary: "reads the StorageClass objects a StoragePool derives, so a name collision is visible before it is created",
			List:    &storagev1.StorageClassList{},
		},
		Kind{
			RuleID:  IDPersistentVolumes,
			Summary: "reads the PersistentVolume objects whose volume handles §16.4 normalizes",
			List:    &corev1.PersistentVolumeList{},
		},
	}
}

// EscapingKinds are the kinds read across every namespace rather than inside the
// installation, because the identifier each of them derives has no namespace in
// it and two namespaces therefore reach one value.
//
// The list is deliberately short. Reading a kind this way costs a list against
// the whole cluster and puts another tenant's objects where a check could
// mistake them for this installation's, so a kind is here because a named check
// cannot answer its question otherwise, and for no other reason.
func EscapingKinds() []upgrade.Discoverer {
	return []upgrade.Discoverer{
		Kind{
			RuleID: IDClustersEverywhere,
			Summary: "reads the StorageCluster objects of every namespace, because the node label a " +
				"cluster claims workers with carries its name and nothing else",
			List:       &simplyblockv1alpha1.StorageClusterList{},
			Namespaced: true,
			View:       ViewClusterWide,
		},
		Kind{
			RuleID: IDMigrationsEverywhere,
			Summary: "reads the VolumeMigration objects of every namespace, because the kind that " +
				"absorbs them is cluster-scoped",
			List:       &simplyblockv1alpha1.VolumeMigrationList{},
			Namespaced: true,
			View:       ViewClusterWide,
		},
	}
}
