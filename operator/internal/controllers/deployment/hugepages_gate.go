// The Preprovisioned huge-page gate: whether CreatingCluster and CreatingNodes
// may run, decided fresh on every pass rather than once (design-preprovisioned-
// hugepages.md S:5, S:6.1).
//
// Unlike the structural findings of validation.go, a failing precheck does not
// fail the document: a worker that is undersized today may be corrected
// tomorrow, so this is a retryable readiness condition (HugePagesReady) on the
// document's own status, read fresh every pass, not a permanent verdict about
// the document's shape.

package deployment

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/hugepages"
)

// hugePagesUnit is one cluster the document will create, or is growing, with
// its own Preprovisioned decision, sizing, and candidate workers. A member
// document's node sets each become a cluster of their own
// (design-superclusters.md S:5.6), so each is a unit of its own too.
type hugePagesUnit struct {
	mode                 simplyblockv1alpha2.HugePagesMode
	maxSubsys, vcpuCount int32
	minHugePagesSizeMiB  int64
	// clusterUUID excludes that cluster's own already-running nodes from
	// contention (U-12 of the test plan): empty for a cluster that does not
	// exist yet, which has nothing to exclude.
	clusterUUID string
	workers     []string
}

// hugePagesUnits is every cluster this document's node sets would place a
// node on, each at its own effective sizing. It returns none for a document
// this precheck has nothing to say about: one naming neither an existing
// cluster nor a template, which validation.go's own findings refuse before
// this is ever reached.
func (r *ClusterDeploymentConfigReconciler) hugePagesUnits(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]hugePagesUnit, error) {
	members, err := AddsMembers(ctx, r.Client, config)
	if err != nil {
		return nil, err
	}
	if members {
		units := make([]hugePagesUnit, 0, len(config.Spec.NodeSets))
		for _, set := range config.Spec.NodeSets {
			view := memberView(config, set)
			units = append(units,
				hugePagesUnitFromTemplate(view.Spec.Cluster, "", workersOf(view.Spec.NodeSets)))
		}
		return units, nil
	}

	if config.Spec.ClusterRef != "" {
		var cluster simplyblockv1alpha2.StorageCluster
		key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
		if err := r.Get(ctx, key, &cluster); err != nil {
			return nil, fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
		}
		return []hugePagesUnit{{
			mode:                cluster.Spec.HugePagesMode,
			maxSubsys:           int32(ptr.IntFromOrZero(cluster.Spec.MaxSubsystemCount)),
			vcpuCount:           int32(ptr.IntFromOrZero(cluster.Spec.VCPUCount)),
			minHugePagesSizeMiB: hugepages.MinHugePagesSizeMiB(cluster.Spec.MinHugePagesSize),
			clusterUUID:         cluster.Status.UUID,
			workers:             workersOf(config.Spec.NodeSets),
		}}, nil
	}

	if config.Spec.Cluster == nil {
		// Refused elsewhere as ClusterNotFound (createCluster); nothing of this
		// document's sizing to check.
		return nil, nil
	}
	return []hugePagesUnit{
		hugePagesUnitFromTemplate(config.Spec.Cluster, "", workersOf(config.Spec.NodeSets)),
	}, nil
}

func hugePagesUnitFromTemplate(
	template *simplyblockv1alpha2.ClusterTemplate, clusterUUID string, workers []string,
) hugePagesUnit {
	if template == nil {
		return hugePagesUnit{workers: workers}
	}
	return hugePagesUnit{
		mode:                template.HugePagesMode,
		maxSubsys:           int32(ptr.IntFromOrZero(template.MaxSubsystemCount)),
		vcpuCount:           int32(ptr.IntFromOrZero(template.VCPUCount)),
		minHugePagesSizeMiB: hugepages.MinHugePagesSizeMiB(template.MinHugePagesSize),
		clusterUUID:         clusterUUID,
		workers:             workers,
	}
}

// workersOf flattens every worker named across nodeSets, deduplicated and in
// the order first named -- the same shape missingWorkers reads them in.
func workersOf(nodeSets []simplyblockv1alpha2.NodeSet) []string {
	seen := map[string]struct{}{}
	var workers []string
	for _, set := range nodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				if _, dup := seen[worker]; dup {
					continue
				}
				seen[worker] = struct{}{}
				workers = append(workers, worker)
			}
		}
	}
	return workers
}

// kubeNodesOf reads every named worker as a KubeNode. Each one is guaranteed
// to exist by the time this runs: validate's own missingWorkers finding holds
// a draft, and fails an approved document, before expand ever reaches a step
// that calls this.
func (r *ClusterDeploymentConfigReconciler) kubeNodesOf(
	ctx context.Context, names []string,
) ([]discovery.KubeNode, error) {
	out := make([]discovery.KubeNode, 0, len(names))
	for _, name := range names {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			return nil, fmt.Errorf("reading worker Node %s: %w", name, err)
		}
		out = append(out, discovery.KubeNodeOf(node))
	}
	return out, nil
}

// hugePagesReady runs the Preprovisioned precheck for every unit of config
// that asks for it, records the outcome as the HugePagesReady condition, and
// reports whether every one of them passed.
//
// A document with no Preprovisioned unit at all calls neither the control
// plane nor writeStatus (U-23, I-03 of the test plan): there is nothing to
// report, and reporting Ready regardless would claim a check that never ran.
func (r *ClusterDeploymentConfigReconciler) hugePagesReady(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	units, err := r.hugePagesUnits(ctx, config)
	if err != nil {
		return false, err
	}

	result := &hugepages.Result{}
	var checked bool
	for _, unit := range units {
		if unit.mode != simplyblockv1alpha2.HugePagesModePreprovisioned {
			continue
		}
		checked = true

		workers, err := r.kubeNodesOf(ctx, unit.workers)
		if err != nil {
			return false, err
		}
		unitResult, err := hugepages.Evaluate(ctx, r.Client, r.ControlPlane,
			workers, unit.clusterUUID, unit.maxSubsys, unit.vcpuCount, unit.minHugePagesSizeMiB)
		if err != nil {
			// Left exactly as it was found (design S:9): the control plane's
			// own unreachability is an ordinary reconcile error, not a reason
			// to believe the precheck passed or guess that it failed.
			return false, err
		}
		result.RequiredMiB = unitResult.RequiredMiB
		result.Workers = append(result.Workers, unitResult.Workers...)
	}
	if !checked {
		return true, nil
	}

	condition := result.Condition()
	condition.ObservedGeneration = config.Generation
	if err := r.writeStatus(ctx, config,
		func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
			apimeta.SetStatusCondition(&status.Conditions, condition)
		}); err != nil {
		return false, err
	}
	return result.AllReady(), nil
}
