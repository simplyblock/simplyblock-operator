// Conversion of StorageClusterOps between this version and the v1alpha2 hub.
//
// Three properties move (design-property-renames.md §2.1, §2.2, and §2.5): the
// rolling-restart spec and status blocks lose their Node prefix, and the action
// enum is recased. See controlplane_conversion.go for why an unmapped enum value
// is passed through rather than rejected.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// storageClusterOpsActionToHub maps this version's lowercase, hyphenated actions
// onto the hub's PascalCase ones. Every value is listed, because unlike a phase
// this enum is user-authored and appears in scripts and runbooks.
var storageClusterOpsActionToHub = map[string]string{
	"activate":             string(v1alpha2.StorageClusterOpsActionActivate),
	"expand":               string(v1alpha2.StorageClusterOpsActionExpand),
	"shutdown":             string(v1alpha2.StorageClusterOpsActionShutdown),
	"start":                string(v1alpha2.StorageClusterOpsActionStart),
	"restart":              string(v1alpha2.StorageClusterOpsActionRestart),
	"node-rolling-restart": string(v1alpha2.StorageClusterOpsActionRollingRestart),
}

// storageClusterOpsActionFromHub is the inverse, derived so the two cannot
// disagree about a value.
var storageClusterOpsActionFromHub = invertStringMap(storageClusterOpsActionToHub)

// ConvertTo converts this StorageClusterOps to the v1alpha2 hub.
func (src *StorageClusterOps) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageClusterOps)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = v1alpha2.StorageClusterOpsSpec{
		ClusterRef: src.Spec.ClusterRef,
		Action: v1alpha2.StorageClusterOpsAction(
			mapOrPassThrough(storageClusterOpsActionToHub, src.Spec.Action),
		),
	}
	// An absent block stays absent: an operation of another action must not gain
	// a rolling-restart block it never had.
	if src.Spec.NodeRollingRestart != nil {
		dst.Spec.RollingRestart = &v1alpha2.RollingRestartSpec{
			RefreshSNodeAPI: src.Spec.NodeRollingRestart.RefreshSNodeAPI,
		}
	}

	dst.Status = v1alpha2.StorageClusterOpsStatus{
		Phase:       v1alpha2.StorageClusterOpsPhase(src.Status.Phase),
		Triggered:   src.Status.Triggered,
		Message:     src.Status.Message,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	if s := src.Status.NodeRollingRestartStatus; s != nil {
		dst.Status.RollingRestart = &v1alpha2.RollingRestartStatus{
			PendingNodes:   s.PendingNodes,
			ProcessedNodes: s.ProcessedNodes,
			NodePhase:      s.NodePhase,
			PhaseTriggered: s.PhaseTriggered,
		}
	}

	return nil
}

// ConvertFrom converts the v1alpha2 hub into this StorageClusterOps.
func (dst *StorageClusterOps) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageClusterOps)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = StorageClusterOpsSpec{
		ClusterRef: src.Spec.ClusterRef,
		Action:     mapOrPassThrough(storageClusterOpsActionFromHub, string(src.Spec.Action)),
	}
	if src.Spec.RollingRestart != nil {
		dst.Spec.NodeRollingRestart = &NodeRollingRestartSpec{
			RefreshSNodeAPI: src.Spec.RollingRestart.RefreshSNodeAPI,
		}
	}

	dst.Status = StorageClusterOpsStatus{
		Phase:       StorageClusterOpsPhase(src.Status.Phase),
		Triggered:   src.Status.Triggered,
		Message:     src.Status.Message,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	if s := src.Status.RollingRestart; s != nil {
		dst.Status.NodeRollingRestartStatus = &NodeRollingRestartStatus{
			PendingNodes:   s.PendingNodes,
			ProcessedNodes: s.ProcessedNodes,
			NodePhase:      s.NodePhase,
			PhaseTriggered: s.PhaseTriggered,
		}
	}

	return nil
}
