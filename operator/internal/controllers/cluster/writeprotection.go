// The SwitchWriteProtection action: an upgraded cluster's distribs keep v1
// write protection until the control plane is asked to move them to v2, which
// is the scheme every cluster it creates starts with. The action waits for
// every node to be online and makes the switch once (design-release-upgrade.md
// §4.4). It lives in its own file because it is neither a single call and a
// wait nor a walk over nodes, and shares its peer check with neither.

package cluster

import (
	"context"
	"fmt"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// switchWriteProtection performs one step of the action. Both steps finish at
// once on a cluster that already reports v2, so an operation against such a
// cluster succeeds without a call.
func (r *StorageClusterOpsReconciler) switchWriteProtection(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	current step,
) (bool, error) {
	reading, err := r.clusterReading(ctx, clusterID)
	if err != nil {
		return false, err
	}
	if reading.WriteProtectionV2 == nil {
		return false, fatalf("the control plane does not report the cluster's write protection, " +
			"so it predates the switch")
	}
	if *reading.WriteProtectionV2 {
		return true, nil
	}

	switch current {
	case stepCheckingPeers:
		// The control plane switches only the nodes that are online and records
		// v2 for the rest, so a switch made while a node is down still finishes.
		// Waiting for every node keeps the runtime change from landing on a
		// cluster that is already absorbing a failure.
		nodes, err := r.storageNodes(ctx, clusterID)
		if err != nil {
			return false, err
		}
		for _, node := range nodes {
			if !utils.NodeIsLeaving(lower(node.Status)) && lower(node.Status) != utils.NodeStatusOnline {
				return false, nil
			}
		}
		return true, nil

	case stepSwitching:
		// The switch is idempotent, and one the control plane refused (409:
		// a node rejected it, and the cluster stays on v1) is asked again when
		// the claim's lease runs out, until the step's deadline.
		claimed, err := r.once(ctx, ops, func() error {
			if err := r.API.SwitchWriteProtection(ctx, clusterID); err != nil {
				return fmt.Errorf("switch the write protection of cluster %s: %w", ops.Spec.ClusterRef, err)
			}
			return nil
		})
		if claimed && err == nil {
			logf.FromContext(ctx).Info("the write-protection switch was made",
				"operation", ops.Name, "cluster", ops.Spec.ClusterRef)
		}
		return false, err

	default:
		return false, fatalf("step %s is not part of the write-protection switch", current)
	}
}
