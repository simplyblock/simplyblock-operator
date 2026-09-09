// §19.3's nine object-name cases, against the 253-byte limit, plus the three
// that §19.8 re-derives from the cluster once §16.1 retires StorageNodeSet.
//
// These are the roomier half of the problem and they are still reachable: a
// ReplicationSlot joins two names that Kubernetes each allows to be 253
// characters long. The three target-model rows are the interesting ones, and
// what they catch is not length: the DaemonSet, the per-node ConfigMap, and the
// EndpointSlice are named per set precisely so several sets can coexist in one
// cluster, so two sets collapse onto one name the moment the parent becomes the
// cluster.

package derive

import (
	"context"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the object-name rows.
const (
	IDStorageClassName     upgrade.ID = "name-storage-class"
	IDPerNodeConfigMap     upgrade.ID = "name-per-node-config-map"
	IDStorageNodeDaemonSet upgrade.ID = "name-storage-node-daemon-set"
	IDAPIEndpointSlice     upgrade.ID = "name-storage-node-api-endpoint-slice"
	IDClusterSecret        upgrade.ID = "name-cluster-secret"
	IDUpgradeSecret        upgrade.ID = "name-upgrade-secret"
	IDNodeRemoveOps        upgrade.ID = "name-storage-node-remove-ops"
	IDRestoredBackup       upgrade.ID = "name-restored-backup"
	IDImportedBackup       upgrade.ID = "name-imported-backup"

	IDPerNodeConfigMapTarget     upgrade.ID = "name-per-node-config-map-target"
	IDStorageNodeDaemonSetTarget upgrade.ID = "name-storage-node-daemon-set-target"
	IDAPIEndpointSliceTarget     upgrade.ID = "name-storage-node-api-endpoint-slice-target"
)

// Names returns the object-name rows of §19.3, both the current model's and the
// target model's.
func Names() []upgrade.Derivation {
	return []upgrade.Derivation{
		storageClassName(),
		perNodeConfigMapName(),
		storageNodeDaemonSetName(),
		apiEndpointSliceName(),
		clusterSecretName(),
		upgradeSecretName(),
		nodeRemoveOpsName(),
		restoredBackupName(),
		importedBackupName(),

		perNodeConfigMapNameTarget(),
		storageNodeDaemonSetNameTarget(),
		apiEndpointSliceNameTarget(),
	}
}

// storageClassName is simplyblock-<ns>-<cluster>-<pool>, the StorageClass a
// pool generates (internal/controller/storageclass_name.go).
//
// The pool name is what binds it, and at the namespace and cluster names CI
// uses that leaves 210 characters. It is also §19.8's first uniqueness route:
// three names joined with a separator that is legal inside all three, so
// cluster a-b with pool c and cluster a with pool b-c derive one name in one
// namespace.
//
// This is the row whose violation is expensive to resolve. A bound claim pins
// spec.storageClassName immutably, so a pool that has to be renamed means its
// volumes move to claims on a new class, which is a data migration on a running
// cluster. That is why the check runs before anything is deployed.
func storageClassName() Rule {
	return Rule{
		RuleID:  IDStorageClassName,
		Summary: "bounds the StorageClass name a pool generates, and detects two pools deriving one",
		Where:   "a StorageClass name",
		Which:   upgrade.ModelCurrent,
		Build:   atlaskube.Formula{Kind: atlaskube.ObjectName, Prefix: "simplyblock-"},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(pools(s)))
			for _, pool := range pools(s) {
				out = append(out, upgrade.Input{
					Source: s.Ref(pool),
					Parts:  []string{pool.Namespace, pool.Spec.ClusterName, pool.Name},
				})
			}
			return out, nil
		},
	}
}

// perNodeConfigMapName is <set>-per-node-config, the ConfigMap carrying one key
// per worker hostname (simplyblockstoragenodeset_pernodeconfig.go).
func perNodeConfigMapName() Rule {
	return Rule{
		RuleID:    IDPerNodeConfigMap,
		Summary:   "bounds the per-node ConfigMap name a StorageNodeSet owns",
		Where:     "a ConfigMap name",
		Which:     upgrade.ModelCurrent,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-per-node-config"},
		Enumerate: fromNodeSetNames,
	}
}

// storageNodeDaemonSetName is simplyblock-storage-node-ds-<set>
// (atlas-lib/kube.StorageNodeSetDaemonSetName).
func storageNodeDaemonSetName() Rule {
	return Rule{
		RuleID:    IDStorageNodeDaemonSet,
		Summary:   "bounds the storage-node DaemonSet name a StorageNodeSet owns",
		Where:     "a DaemonSet name",
		Which:     upgrade.ModelCurrent,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Prefix: "simplyblock-storage-node-ds-"},
		Enumerate: fromNodeSetNames,
	}
}

// apiEndpointSliceName is <set>-storage-node-api-endpoints, the slice that
// publishes a set's storage-node-api pods behind the shared headless Service
// (atlas-lib/kube.StorageNodeSetAPIEndpointSliceName).
func apiEndpointSliceName() Rule {
	return Rule{
		RuleID:    IDAPIEndpointSlice,
		Summary:   "bounds the EndpointSlice name a StorageNodeSet publishes its API pods through",
		Where:     "an EndpointSlice name",
		Which:     upgrade.ModelCurrent,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-storage-node-api-endpoints"},
		Enumerate: fromNodeSetNames,
	}
}

// perNodeConfigMapNameTarget is the same ConfigMap once §16.1 has made the
// cluster its parent. Two sets in one cluster derive one name, which no naming
// scheme fixes and which is why the preflight exists (§19.8).
func perNodeConfigMapNameTarget() Rule {
	return Rule{
		RuleID:    IDPerNodeConfigMapTarget,
		Summary:   "detects two StorageNodeSets whose per-node ConfigMaps collapse onto one name under the cluster",
		Where:     "a ConfigMap name, once the cluster is the parent",
		Which:     upgrade.ModelTarget,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-per-node-config"},
		Enumerate: fromNodeSetClusters,
	}
}

// storageNodeDaemonSetNameTarget is the DaemonSet under the same reparenting.
// This one is the most consequential of the three: two sets collapsing onto one
// DaemonSet name is two storage-node workloads becoming one.
func storageNodeDaemonSetNameTarget() Rule {
	return Rule{
		RuleID:    IDStorageNodeDaemonSetTarget,
		Summary:   "detects two StorageNodeSets whose DaemonSets collapse onto one name under the cluster",
		Where:     "a DaemonSet name, once the cluster is the parent",
		Which:     upgrade.ModelTarget,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Prefix: "simplyblock-storage-node-ds-"},
		Enumerate: fromNodeSetClusters,
	}
}

// apiEndpointSliceNameTarget is the EndpointSlice under the same reparenting.
func apiEndpointSliceNameTarget() Rule {
	return Rule{
		RuleID:    IDAPIEndpointSliceTarget,
		Summary:   "detects two StorageNodeSets whose EndpointSlices collapse onto one name under the cluster",
		Where:     "an EndpointSlice name, once the cluster is the parent",
		Which:     upgrade.ModelTarget,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-storage-node-api-endpoints"},
		Enumerate: fromNodeSetClusters,
	}
}

// clusterSecretName is simplyblock-cluster-<cluster>, holding the cluster's
// control-plane credentials (simplyblockstoragecluster_controller.go).
func clusterSecretName() Rule {
	return Rule{
		RuleID:    IDClusterSecret,
		Summary:   "bounds the Secret name a StorageCluster's credentials are kept under",
		Where:     "a Secret name",
		Which:     upgrade.ModelCurrent,
		Build:     atlaskube.Formula{Kind: atlaskube.ObjectName, Prefix: "simplyblock-cluster-"},
		Enumerate: fromClusterNames,
	}
}

// upgradeSecretName is simplyblock-<cluster>-upgrade, which marks a cluster as
// being adopted by an upgrade rather than created
// (simplyblockstoragecluster_controller.go, storagenode_controller.go).
func upgradeSecretName() Rule {
	return Rule{
		RuleID:  IDUpgradeSecret,
		Summary: "bounds the Secret name that marks a StorageCluster as upgrade-adopted",
		Where:   "a Secret name",
		Which:   upgrade.ModelCurrent,
		Build: atlaskube.Formula{
			Kind:   atlaskube.ObjectName,
			Prefix: "simplyblock-",
			Suffix: "-upgrade",
		},
		Enumerate: fromClusterNames,
	}
}

// nodeRemoveOpsName is <node>-remove, the StorageNodeOps a StorageNode's
// removal is driven through (storagenode_controller.go).
func nodeRemoveOpsName() Rule {
	return Rule{
		RuleID:  IDNodeRemoveOps,
		Summary: "bounds the StorageNodeOps name a node's removal is driven through",
		Where:   "a StorageNodeOps name",
		Which:   upgrade.ModelCurrent,
		Build:   atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-remove"},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(nodes(s)))
			for _, node := range nodes(s) {
				out = append(out, upgrade.Input{Source: s.Ref(node), Parts: []string{node.Name}})
			}
			return out, nil
		},
	}
}

// restoredBackupName is <name>-restored, the object a BackupRestore produces
// (backuprestore_controller.go).
func restoredBackupName() Rule {
	return Rule{
		RuleID:  IDRestoredBackup,
		Summary: "bounds the name a restore gives what it produces",
		Where:   "a restored resource's name",
		Which:   upgrade.ModelCurrent,
		Build:   atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-restored"},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(restores(s)))
			for _, restore := range restores(s) {
				out = append(out, upgrade.Input{Source: s.Ref(restore), Parts: []string{restore.Name}})
			}
			return out, nil
		},
	}
}

// importedBackupName is <name>-imported, the counterpart a BackupImport
// produces. §16.2 retires the kind, so the row constrains the cluster this
// migration starts from rather than the one it produces.
func importedBackupName() Rule {
	return Rule{
		RuleID:  IDImportedBackup,
		Summary: "bounds the name an import gives what it produces",
		Where:   "an imported resource's name",
		Which:   upgrade.ModelCurrent,
		Build:   atlaskube.Formula{Kind: atlaskube.ObjectName, Suffix: "-imported"},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(imports(s)))
			for _, imported := range imports(s) {
				out = append(out, upgrade.Input{Source: s.Ref(imported), Parts: []string{imported.Name}})
			}
			return out, nil
		},
	}
}

// fromNodeSetNames is the input for a name derived from a StorageNodeSet.
func fromNodeSetNames(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
	out := make([]upgrade.Input, 0, len(nodeSets(s)))
	for _, set := range nodeSets(s) {
		out = append(out, upgrade.Input{Source: s.Ref(set), Parts: []string{set.Name}})
	}
	return out, nil
}

// fromNodeSetClusters is the input for the same name once the cluster is the
// parent. The source stays the StorageNodeSet, because that is the object a
// user would have to change, while the part is the cluster it belongs to: two
// sets of one cluster therefore appear as two sources deriving one value, which
// is exactly the collision §19.8 describes.
func fromNodeSetClusters(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
	var out []upgrade.Input
	for _, set := range nodeSets(s) {
		if set.Spec.ClusterName == "" {
			continue
		}
		out = append(out, upgrade.Input{Source: s.Ref(set), Parts: []string{set.Spec.ClusterName}})
	}
	return out, nil
}

// fromClusterNames is the input for a name derived from a StorageCluster.
func fromClusterNames(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
	out := make([]upgrade.Input, 0, len(clusters(s)))
	for _, cluster := range clusters(s) {
		out = append(out, upgrade.Input{Source: s.Ref(cluster), Parts: []string{cluster.Name}})
	}
	return out, nil
}
