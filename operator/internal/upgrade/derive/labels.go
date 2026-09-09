// §19.2's seven label cases. Every row is live today, and the longest input each
// admits is the design's measured number rather than a reasoned one.
//
// The tightest row is the pool key, and it is the only one that binds three
// names at once: 63 bytes less a five-character prefix and two separators leaves
// the namespace, the cluster, and the pool 56 characters between them.

package derive

import (
	"context"
	"strconv"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the label rows.
const (
	IDPoolNodeLabelKey    upgrade.ID = "label-pool-node-key"
	IDStorageClassCluster upgrade.ID = "label-storage-class-cluster"
	IDStorageClassPool    upgrade.ID = "label-storage-class-pool"
	IDNodeSetLabel        upgrade.ID = "label-storage-node-set"
	IDWorkerLabel         upgrade.ID = "label-worker"
	IDDrainNodeLabel      upgrade.ID = "label-drain-node"
	IDStorageNodeUUIDKey  upgrade.ID = "label-storage-node-uuid-key"
)

// Labels returns the label rows of §19.2.
func Labels() []upgrade.Derivation {
	return []upgrade.Derivation{
		poolNodeLabelKey(),
		storageClassClusterLabel(),
		storageClassPoolLabel(),
		nodeSetLabel(),
		workerLabel(),
		drainNodeLabel(),
		storageNodeUUIDLabelKey(),
	}
}

// poolNodeLabelKey is simplyblock.io/pool.<ns>.<cluster>.<pool>, patched onto
// every worker a pool allows (simplyblockstoragepool_controller.go).
//
// It is a key rather than a value, so the 63 bytes bound the part after the
// slash. Five of them go to the prefix and two to the separators, which leaves
// the namespace, the cluster, and the pool 56 characters between them, and a
// 27-character pool name at the namespace and cluster names CI uses.
//
// The separator is a dot, which is legal inside all three names, so this row is
// also one of §19.8's ambiguous concatenations: cluster a-b with pool c and
// cluster a with pool b-c produce one key in one namespace.
func poolNodeLabelKey() Rule {
	return Rule{
		RuleID:     IDPoolNodeLabelKey,
		Summary:    "bounds the worker label key a pool patches onto the nodes it allows",
		Where:      "Node label key simplyblock.io/pool.<namespace>.<cluster>.<pool>",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixTruncateAndHash,
		Unique:     upgrade.SpaceCluster,
		Build: atlaskube.Formula{
			Kind:      atlaskube.LabelKeyName,
			Prefix:    "pool.",
			Separator: ".",
		},
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

// storageClassClusterLabel is storage.simplyblock.io/cluster on a generated
// StorageClass, carrying the cluster name verbatim
// (simplyblockstoragepool_controller.go).
//
// A bare name against the bare limit, so a 63-character cluster name works and
// a 64-character one does not. §19.5 resolves this row by using a UUID rather
// than by truncating, since nothing reads the value and the label exists to be
// selected on.
func storageClassClusterLabel() Rule {
	return Rule{
		RuleID:     IDStorageClassCluster,
		Summary:    "bounds the cluster label a generated StorageClass carries",
		Where:      "StorageClass label storage.simplyblock.io/cluster",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixUseUUID,
		Unique:     upgrade.SpaceShared,
		Build:      atlaskube.Formula{Kind: atlaskube.LabelValue},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(pools(s)))
			for _, pool := range pools(s) {
				out = append(out, upgrade.Input{
					Source: s.Ref(pool),
					Parts:  []string{pool.Spec.ClusterName},
				})
			}
			return out, nil
		},
	}
}

// storageClassPoolLabel is storage.simplyblock.io/pool on the same object,
// carrying the pool's own name.
func storageClassPoolLabel() Rule {
	return Rule{
		RuleID:     IDStorageClassPool,
		Summary:    "bounds the pool label a generated StorageClass carries",
		Where:      "StorageClass label storage.simplyblock.io/pool",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixUseUUID,
		Unique:     upgrade.SpaceShared,
		Build:      atlaskube.Formula{Kind: atlaskube.LabelValue},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(pools(s)))
			for _, pool := range pools(s) {
				out = append(out, upgrade.Input{
					Source: s.Ref(pool),
					Parts:  []string{pool.Name},
				})
			}
			return out, nil
		},
	}
}

// nodeSetLabel is io.simplyblock.storagenodeset, which scopes a worker, a pod,
// and a DaemonSet to one StorageNodeSet and lets several sets coexist in one
// cluster (atlas-lib/kube.LabelStorageNodeSet).
//
// §19.5 resolves it by bounding the input, since a name somebody types has no
// business being 200 characters. The row stays ModelCurrent because §16.1
// retires the kind: what it constrains is the cluster this migration starts
// from, not the one it produces.
func nodeSetLabel() Rule {
	return Rule{
		RuleID:     IDNodeSetLabel,
		Summary:    "bounds the node-set label that lets several sets coexist in one cluster",
		Where:      "Node label io.simplyblock.storagenodeset",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixBoundInput,
		Unique:     upgrade.SpaceCluster,
		Build:      atlaskube.Formula{Kind: atlaskube.LabelValue},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			out := make([]upgrade.Input, 0, len(nodeSets(s)))
			for _, set := range nodeSets(s) {
				out = append(out, upgrade.Input{
					Source: s.Ref(set),
					Parts:  []string{set.Name},
				})
			}
			return out, nil
		},
	}
}

// workerLabel is storage.simplyblock.io/worker on a StorageNode, carrying the
// worker's own name through sanitiseDNSLabel
// (simplyblockstoragenodeset_storagenode.go).
//
// This is the row whose input this repository does not own. The sanitizer
// replaces every character a label may not carry and trims the ends, and does
// not bound the length, so a cloud that names a worker after its fully
// qualified domain name produces the overflow. §19.5 resolves it by truncating
// and hashing, because bounding an input a cloud owns breaks enrollment on a
// legal node name.
func workerLabel() Rule {
	return Rule{
		RuleID:     IDWorkerLabel,
		Summary:    "bounds the worker label, whose input is a node name a cloud chose",
		Where:      "StorageNode label storage.simplyblock.io/worker",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixTruncateAndHash,
		Unique:     upgrade.SpaceShared,
		Build:      atlaskube.Formula{Kind: atlaskube.LabelValue},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			var out []upgrade.Input
			for _, node := range nodes(s) {
				worker := node.Labels["storage.simplyblock.io/worker"]
				if worker == "" {
					// A StorageNode with no worker label is one the set's
					// controller has not reached yet, and there is no input to
					// check rather than an empty one to report.
					continue
				}
				out = append(out, upgrade.Input{Source: s.Ref(node), Parts: []string{worker}})
			}
			return out, nil
		},
	}
}

// drainNodeLabel is simplyblock.io/drain-node, patched onto the storage pod on a
// draining node so the per-node PodDisruptionBudget can select it precisely
// (nodedrain_controller.go).
//
// The value is a worker name, so it breaks where the worker label does, and one
// character earlier: character 63 landing on a dash or a dot is a value a label
// may not end on, which caps a node name at 62 rather than 63.
//
// The two call sites do not agree today. The writer patches the node name
// verbatim and the reader compares against sanitizeLabelValue of it, so a
// worker whose name needs sanitizing is written in a form the API server
// refuses and, were it accepted, in a form the reader would never match. The
// row models the sanitized form, which is what the reader expects and what the
// fix has to produce.
func drainNodeLabel() Rule {
	return Rule{
		RuleID:     IDDrainNodeLabel,
		Summary:    "bounds the drain label the per-node PodDisruptionBudget selects on",
		Where:      "Pod label simplyblock.io/drain-node",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixTruncateAndHash,
		Unique:     upgrade.SpaceShared,
		Build:      atlaskube.Formula{Kind: atlaskube.LabelValue},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			var out []upgrade.Input
			for _, node := range nodes(s) {
				worker := node.Labels["storage.simplyblock.io/worker"]
				if worker == "" {
					continue
				}
				out = append(out, upgrade.Input{Source: s.Ref(node), Parts: []string{worker}})
			}
			return out, nil
		},
	}
}

// storageNodeUUIDLabelKey is simplyblock.io/storage-node-uuid.<clusterUUID>.<n>,
// the per-slot topology key the CSI node plugin advertises and the CSI
// controller co-locates on (simplyblockstoragenodeset_controller.go).
//
// It is the one row of §19.2 that needs no fix. The rest of the key is 55 bytes
// once the cluster UUID is in it, so the socket index would need nine digits to
// overflow, and a worker with a hundred million sockets is not the failure this
// audit is about. The row is declared anyway, so the audit is complete and so
// that a later release widening the prefix is caught rather than assumed safe.
//
// It shares its space, because the key is written per worker Node
// (slotsByWorker in the set's controller) rather than once per cluster. Three
// storage nodes on three workers, each at socket 0, each write this key onto
// their own Node, which is the normal shape of a three-node cluster. The
// invariant that would be worth checking is two storage nodes claiming one
// slot on one worker, and that is a placement question rather than a derived
// name, since this row does not carry the worker the key lands on.
func storageNodeUUIDLabelKey() Rule {
	return Rule{
		RuleID:     IDStorageNodeUUIDKey,
		Summary:    "bounds the per-slot topology key, which the socket index cannot realistically overflow",
		Where:      "Node label key simplyblock.io/storage-node-uuid.<clusterUUID>.<slot>",
		Which:      upgrade.ModelCurrent,
		Resolution: upgrade.FixNone,
		Unique:     upgrade.SpaceShared,
		Build: atlaskube.Formula{
			Kind:      atlaskube.LabelKeyName,
			Prefix:    "storage-node-uuid.",
			Separator: ".",
		},
		Enumerate: func(_ context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
			// The key is built per cluster and per slot, and the two halves come
			// from different objects: the reconcile owns one cluster's UUID and
			// walks its nodes' socket indexes.
			var out []upgrade.Input
			for _, cluster := range clusters(s) {
				if cluster.Status.UUID == "" {
					// A cluster the control plane has not created yet has no
					// UUID, so the key its nodes will carry is not derivable.
					continue
				}
				for _, node := range nodes(s) {
					if node.Spec.SocketIndex == nil {
						continue
					}
					out = append(out, upgrade.Input{
						Source: s.Ref(node),
						Parts: []string{
							cluster.Status.UUID,
							strconv.Itoa(int(*node.Spec.SocketIndex)),
						},
					})
				}
			}
			return out, nil
		},
	}
}
