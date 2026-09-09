// The one implementation of upgrade.Derivation, plus the typed reads of the
// graph the rows are built from. A row is data: a formula, a sentence saying
// where the value lands, and a function that enumerates what this cluster would
// hand it.

package derive

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"

	corev1 "k8s.io/api/core/v1"

	atlaskube "github.com/simplyblock/atlas/kube"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// Rule is one derived identifier: the formula, and what a cluster feeds it.
type Rule struct {
	// RuleID names the row in a report and on the command line.
	RuleID upgrade.ID

	// Summary says what the row is for, in one sentence.
	Summary string

	// Where says where the derived value ends up, in the form a report prints.
	Where string

	// Which model the formula belongs to.
	Which upgrade.Model

	// Resolution is what a report tells the user to do about a violation. It
	// is the Fix column of §19.2 and §19.3, which follows from who owns the
	// name rather than from how long it is.
	Resolution upgrade.Fix

	// Build is the formula.
	Build atlaskube.Formula

	// Enumerate lists what this cluster would hand the formula. It reads the
	// graph rather than the cluster, so a row costs no API call of its own.
	Enumerate func(ctx context.Context, s *upgrade.Scope) ([]upgrade.Input, error)
}

func (r Rule) ID() upgrade.ID             { return r.RuleID }
func (r Rule) Description() string        { return r.Summary }
func (r Rule) Written() string            { return r.Where }
func (r Rule) Model() upgrade.Model       { return r.Which }
func (r Rule) Formula() atlaskube.Formula { return r.Build }
func (r Rule) Fix() upgrade.Fix           { return r.Resolution }

func (r Rule) Inputs(ctx context.Context, s *upgrade.Scope) ([]upgrade.Input, error) {
	if r.Enumerate == nil {
		return nil, nil
	}
	return r.Enumerate(ctx, s)
}

// The kinds the rows read. They are named here so a row says which objects it
// is about rather than restating a group, a version, and a kind.
var (
	clusterGVK = gvk("StorageCluster")
	poolGVK    = gvk("StoragePool")
	nodeSetGVK = gvk("StorageNodeSet")
	nodeGVK    = gvk("StorageNode")
	restoreGVK = gvk("BackupRestore")
	claimGVK   = schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}
	importGVK  = gvk("BackupImport")
)

func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: kind}
}

// The typed reads. Each returns what discovery adopted, and an empty slice on a
// cluster that holds none, which is what a row on an empty cluster wants.
func clusters(s *upgrade.Scope) []*simplyblockv1alpha1.StorageCluster {
	return upgrade.Typed[*simplyblockv1alpha1.StorageCluster](s.Graph, clusterGVK)
}

func pools(s *upgrade.Scope) []*simplyblockv1alpha1.StoragePool {
	return upgrade.Typed[*simplyblockv1alpha1.StoragePool](s.Graph, poolGVK)
}

func nodeSets(s *upgrade.Scope) []*simplyblockv1alpha1.StorageNodeSet {
	return upgrade.Typed[*simplyblockv1alpha1.StorageNodeSet](s.Graph, nodeSetGVK)
}

func nodes(s *upgrade.Scope) []*simplyblockv1alpha1.StorageNode {
	return upgrade.Typed[*simplyblockv1alpha1.StorageNode](s.Graph, nodeGVK)
}

func restores(s *upgrade.Scope) []*simplyblockv1alpha1.BackupRestore {
	return upgrade.Typed[*simplyblockv1alpha1.BackupRestore](s.Graph, restoreGVK)
}

func imports(s *upgrade.Scope) []*simplyblockv1alpha1.BackupImport {
	return upgrade.Typed[*simplyblockv1alpha1.BackupImport](s.Graph, importGVK)
}

// claims reads the cluster-wide graph rather than the installation's, because a
// claim lives in the namespace of the workload that mounts it. Unlike another
// tenant's StorageCluster, a claim in a workload namespace is this
// installation's data plane, so its derived names are this upgrade's problem.
func claims(s *upgrade.Scope) []*corev1.PersistentVolumeClaim {
	return upgrade.Typed[*corev1.PersistentVolumeClaim](s.ClusterWide, claimGVK)
}
